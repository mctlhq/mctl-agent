// Copyright 2025 MCTL Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package optimizer

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mctlhq/mctl-agent/internal/vmetrics"
)

// MetricsQuerier is the subset of vmetrics.Client the collector needs.
type MetricsQuerier interface {
	Query(ctx context.Context, promql string, ts time.Time) ([]vmetrics.Sample, error)
	QueryScalar(ctx context.Context, promql string, ts time.Time) (float64, bool, error)
}

// rollupRetention is how long hourly rollups are kept. Must comfortably
// exceed baseline (14d) + warmup (1d) + eval (7d) + slack.
const rollupRetention = 45 * 24 * time.Hour

// backfillLimit bounds how far back a single collection pass will reach for
// missing hours (VictoriaMetrics retention permitting).
const backfillLimit = 14 * 24 * time.Hour

// podSelector pins metrics to the main container of a base-service workload.
// Release names are {tenant}-{service}, the chart appends "-base-service" to
// the fullname, and containers[0] is named after the chart — so this selector
// excludes sidecars and init containers by construction.
func podSelector(tenant, service string) string {
	podRe := fmt.Sprintf("%s-%s-base-service-[a-z0-9]+-[a-z0-9]+", tenant, service)
	return fmt.Sprintf(`namespace=%q,pod=~%q,container="base-service"`, tenant, podRe)
}

// CollectHour builds the rollup for one complete hour [hourStart, hourStart+1h).
// Returns (nil, nil) when the workload produced no samples that hour (scaled
// to zero, or the hour predates metrics retention) — absence is not zero.
func CollectHour(ctx context.Context, vm MetricsQuerier, spec ServiceSpec, hourStart time.Time) (*Rollup, error) {
	sel := podSelector(spec.Tenant, spec.Service)
	ts := hourStart.Add(time.Hour)

	samples, ok, err := vm.QueryScalar(ctx,
		fmt.Sprintf(`sum(count_over_time(max by (pod) (container_memory_working_set_bytes{%s})[1h:1m]))`, sel), ts)
	if err != nil {
		return nil, fmt.Errorf("samples query: %w", err)
	}
	if !ok || samples == 0 {
		return nil, nil
	}

	r := &Rollup{
		Tenant:    spec.Tenant,
		Service:   spec.Service,
		HourStart: hourStart.UTC(),
		Samples:   int(samples),
	}

	// Per-pod CPU rate series, worst pod wins; millicores.
	cpuSeries := fmt.Sprintf(`sum by (pod) (rate(container_cpu_usage_seconds_total{%s}[2m]))`, sel)
	scalarQueries := []struct {
		dst   *float64
		query string
	}{
		{&r.CPUP95m, fmt.Sprintf(`max(quantile_over_time(0.95, %s[1h:1m])) * 1000`, cpuSeries)},
		{&r.CPUP99m, fmt.Sprintf(`max(quantile_over_time(0.99, %s[1h:1m])) * 1000`, cpuSeries)},
		{&r.CPUMaxm, fmt.Sprintf(`max(max_over_time(%s[1h:1m])) * 1000`, cpuSeries)},
		{&r.MemP95, fmt.Sprintf(`max(quantile_over_time(0.95, max by (pod) (container_memory_working_set_bytes{%s})[1h:1m]))`, sel)},
		{&r.MemP99, fmt.Sprintf(`max(quantile_over_time(0.99, max by (pod) (container_memory_working_set_bytes{%s})[1h:1m]))`, sel)},
		{&r.MemMax, fmt.Sprintf(`max(max_over_time(max by (pod) (container_memory_working_set_bytes{%s})[1h:1m]))`, sel)},
		{&r.ThrottleRatio, fmt.Sprintf(`sum(increase(container_cpu_cfs_throttled_periods_total{%s}[1h])) / sum(increase(container_cpu_cfs_periods_total{%s}[1h]))`, sel, sel)},
		{&r.Restarts, fmt.Sprintf(`sum(increase(kube_pod_container_status_restarts_total{%s}[1h]))`, sel)},
		{&r.Replicas, fmt.Sprintf(`avg_over_time(count(container_memory_working_set_bytes{%s})[1h:1m])`, sel)},
	}
	for _, q := range scalarQueries {
		v, ok, err := vm.QueryScalar(ctx, q.query, ts)
		if err != nil {
			return nil, fmt.Errorf("rollup query %q: %w", q.query, err)
		}
		if ok {
			*q.dst = v
		}
	}

	oom, ok, err := vm.QueryScalar(ctx,
		fmt.Sprintf(`max(max_over_time(kube_pod_container_status_last_terminated_reason{%s,reason="OOMKilled"}[1h]))`, sel), ts)
	if err != nil {
		return nil, fmt.Errorf("oom query: %w", err)
	}
	r.OOMSeen = ok && oom >= 1

	tagSamples, err := vm.Query(ctx,
		fmt.Sprintf(`max by (image) (kube_pod_container_info{%s})`, sel), ts)
	if err != nil {
		return nil, fmt.Errorf("image query: %w", err)
	}
	if len(tagSamples) > 0 {
		r.ImageTag = imageTag(tagSamples[len(tagSamples)-1].Labels["image"])
	}

	return r, nil
}

// imageTag extracts the tag from an image reference, ignoring any digest.
func imageTag(image string) string {
	if i := strings.Index(image, "@"); i >= 0 {
		image = image[:i]
	}
	slash := strings.LastIndex(image, "/")
	if i := strings.LastIndex(image, ":"); i > slash {
		return image[i+1:]
	}
	return ""
}

// Collect gathers all missing complete hours for every spec, bounded by
// backfillLimit, then prunes expired rollups. Per-service failures are
// logged and skipped so one broken workload doesn't stall the rest; the
// gap is retried on the next tick.
func (o *Optimizer) Collect(ctx context.Context, specs []ServiceSpec, now time.Time) {
	lastComplete := now.UTC().Truncate(time.Hour).Add(-time.Hour)
	earliest := now.UTC().Add(-backfillLimit).Truncate(time.Hour)

	for _, spec := range specs {
		key := spec.Tenant + "/" + spec.Service
		start := earliest
		if latest, ok, err := o.store.LatestRollupHour(spec.Tenant, spec.Service); err != nil {
			slog.Warn("optimizer: latest rollup lookup failed", "tenant", spec.Tenant, "service", spec.Service, "error", err)
			continue
		} else if ok && latest.Add(time.Hour).After(start) {
			start = latest.Add(time.Hour)
		}
		if through, ok := o.collectedThrough[key]; ok && through.Add(time.Hour).After(start) {
			start = through.Add(time.Hour)
		}

		through := start.Add(-time.Hour)
		for hour := start; !hour.After(lastComplete); hour = hour.Add(time.Hour) {
			if ctx.Err() != nil {
				return
			}
			rollup, err := CollectHour(ctx, o.vm, spec, hour)
			if err != nil {
				slog.Warn("optimizer: collect hour failed", "tenant", spec.Tenant, "service", spec.Service, "hour", hour, "error", err)
				break // leave the gap; retried next tick
			}
			if rollup != nil {
				if err := o.store.UpsertRollup(rollup); err != nil {
					slog.Warn("optimizer: rollup upsert failed", "tenant", spec.Tenant, "service", spec.Service, "hour", hour, "error", err)
					break
				}
			}
			through = hour
		}
		o.collectedThrough[key] = through
	}

	if _, err := o.store.PruneRollups(now.UTC().Add(-rollupRetention)); err != nil {
		slog.Warn("optimizer: rollup prune failed", "error", err)
	}
}
