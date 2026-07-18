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
	"encoding/json"
	"math"
	"sort"
	"time"

	"github.com/mctlhq/mctl-agent/internal/config"
)

// SkipReason explains why a workload got no recommendation. Machine-readable;
// surfaced by the candidates API.
type SkipReason string

const (
	SkipNone               SkipReason = ""
	SkipNotPlainDeployment SkipReason = "not_plain_deployment"
	SkipIgnored            SkipReason = "ignored"
	SkipInsufficientData   SkipReason = "insufficient_data"
	SkipRecentRelease      SkipReason = "recent_release"
	SkipActiveIncident     SkipReason = "active_incident"
	SkipOpenRecommendation SkipReason = "open_recommendation"
	SkipBelowMinChange     SkipReason = "below_min_change"
	SkipQuotaExceeded      SkipReason = "quota_exceeded"
	SkipInvalidSpec        SkipReason = "invalid_spec"
)

// Confidence and risk levels.
const (
	ConfidenceHigh   = "HIGH"
	ConfidenceMedium = "MEDIUM"

	RiskLow    = "LOW"
	RiskMedium = "MEDIUM"
	RiskHigh   = "HIGH"
)

// GuardInputs carries the live checks the pure engine cannot do itself.
type GuardInputs struct {
	// Ignored is set by the caller from the tenant allowlist / service
	// ignore regex.
	Ignored bool
	// OpenTickets means an unresolved incident exists for the workload's
	// tenant (or the workload itself).
	OpenTickets bool
	// HasOpenRecommendation enforces one recommendation per workload.
	HasOpenRecommendation bool

	// Namespace ResourceQuota state (kube_resourcequota), 0 = unknown.
	QuotaCPUUsedM int64
	QuotaCPUHardM int64
	QuotaMemUsed  int64
	QuotaMemHard  int64

	// Tenant LimitRange max per container, 0 = unset.
	LimitRangeMaxCPUM int64
	LimitRangeMaxMem  int64
}

// Evidence is the measured basis for a recommendation, serialized into
// the recommendation row and rendered in the PR body.
type Evidence struct {
	WindowStart   time.Time `json:"window_start"`
	WindowEnd     time.Time `json:"window_end"`
	DaysOfData    float64   `json:"days_of_data"`
	Coverage      float64   `json:"coverage"`
	CPUP95m       int64     `json:"cpu_p95_m"`
	CPUP99m       int64     `json:"cpu_p99_m"`
	CPUMaxm       int64     `json:"cpu_max_m"`
	MemP95        int64     `json:"mem_p95"`
	MemP99        int64     `json:"mem_p99"`
	MemMax        int64     `json:"mem_max"`
	ThrottleRatio float64   `json:"throttle_ratio"`
	OOMCount      int       `json:"oom_count"`
	Restarts      float64   `json:"restarts"`
	AvgReplicas   float64   `json:"avg_replicas"`
	ImageTag      string    `json:"image_tag"`
	TagStableDays float64   `json:"tag_stable_days"`
}

// Recommend applies the deterministic right-sizing rules to one workload.
// Pure: no I/O, no clocks other than the supplied now. Returns (nil, reason)
// when no PR should be opened. The returned recommendation has Status unset;
// the caller decides dry_run vs pr_open.
//
// Formulas (per design):
//
//	cpu request    = q95(hourly cpu p95) + CPUBuffer, floor MinCPUMillis
//	memory request = max(hourly mem p99) + MemBuffer, floor MinMemBytes
//
// clamped to the container's own limits, the tenant LimitRange max, and —
// for increases — the namespace ResourceQuota headroom.
func Recommend(spec ServiceSpec, rollups []Rollup, pol config.OptimizerConfig, g GuardInputs, now time.Time) (*Recommendation, SkipReason) {
	// MVP targets plain Deployments with an explicit requests block.
	if spec.BlueGreen || spec.Autoscaling || spec.Persistence || spec.ReplicaCount <= 0 ||
		spec.CPURequest == "" || spec.MemRequest == "" {
		return nil, SkipNotPlainDeployment
	}
	if g.Ignored {
		return nil, SkipIgnored
	}

	oldCPU, err := ParseCPUMillis(spec.CPURequest)
	if err != nil || oldCPU <= 0 {
		return nil, SkipInvalidSpec
	}
	oldMem, err := ParseMemBytes(spec.MemRequest)
	if err != nil || oldMem <= 0 {
		return nil, SkipInvalidSpec
	}

	windowStart := now.Add(-time.Duration(pol.TargetDays * 24 * float64(time.Hour)))
	data := filterWindow(rollups, windowStart, now)
	if len(data) == 0 {
		return nil, SkipInsufficientData
	}

	daysOfData := float64(len(data)) / 24
	span := now.Sub(data[0].HourStart).Hours()
	coverage := 1.0
	if span > 0 {
		coverage = math.Min(1, float64(len(data))/span)
	}
	if daysOfData < pol.MinDays || coverage < 0.6 {
		return nil, SkipInsufficientData
	}

	if recentRelease(spec, data, pol.DeployCooldown, now) {
		return nil, SkipRecentRelease
	}
	if g.OpenTickets {
		return nil, SkipActiveIncident
	}
	if g.HasOpenRecommendation {
		return nil, SkipOpenRecommendation
	}

	ev := buildEvidence(data, daysOfData, coverage, now)

	// Confidence before sizing: memory shrinks are gated on it.
	confidence := ConfidenceMedium
	if daysOfData >= pol.TargetDays && coverage >= 0.9 && cpuP95CV(data) <= 0.5 {
		confidence = ConfidenceHigh
	}

	cpuBasis := quantileFloat(collect(data, func(r Rollup) float64 { return r.CPUP95m }), 0.95)
	memBasis := maxFloat(collect(data, func(r Rollup) float64 { return r.MemP99 }))

	newCPU := roundUpTo(maxInt64(int64(cpuBasis*(1+pol.CPUBuffer)), pol.MinCPUMillis), 10)
	newMem := roundUpTo(maxInt64(int64(memBasis*(1+pol.MemBuffer)), pol.MinMemBytes), 32*1024*1024)

	// Requests may never exceed the container's own limits or the tenant
	// LimitRange max — kubeconform in CI checks neither, admission does.
	if limit, err := ParseCPUMillis(spec.CPULimit); err == nil && limit > 0 && newCPU > limit {
		newCPU = limit
	}
	if limit, err := ParseMemBytes(spec.MemLimit); err == nil && limit > 0 && newMem > limit {
		newMem = limit
	}
	if g.LimitRangeMaxCPUM > 0 && newCPU > g.LimitRangeMaxCPUM {
		newCPU = g.LimitRangeMaxCPUM
	}
	if g.LimitRangeMaxMem > 0 && newMem > g.LimitRangeMaxMem {
		newMem = g.LimitRangeMaxMem
	}

	// Memory shrinks are the riskiest move: only with a clean OOM record
	// and high confidence. Any OOM in the window forbids shrinking.
	if newMem < oldMem && (ev.OOMCount > 0 || confidence != ConfidenceHigh) {
		newMem = oldMem
	}

	// Below-threshold changes are noise — keep the old value per resource.
	if pctChange(oldCPU, newCPU) < pol.MinChangePct {
		newCPU = oldCPU
	}
	if pctChange(oldMem, newMem) < pol.MinChangePct {
		newMem = oldMem
	}

	// Increases must fit the namespace quota across all replicas.
	replicas := int64(spec.ReplicaCount)
	quotaExceeded := false
	if newCPU > oldCPU && g.QuotaCPUHardM > 0 {
		if g.QuotaCPUUsedM+(newCPU-oldCPU)*replicas > g.QuotaCPUHardM {
			newCPU = oldCPU
			quotaExceeded = true
		}
	}
	if newMem > oldMem && g.QuotaMemHard > 0 {
		if g.QuotaMemUsed+(newMem-oldMem)*replicas > g.QuotaMemHard {
			newMem = oldMem
			quotaExceeded = true
		}
	}

	if newCPU == oldCPU && newMem == oldMem {
		if quotaExceeded {
			return nil, SkipQuotaExceeded
		}
		return nil, SkipBelowMinChange
	}

	risk := RiskLow
	if newCPU < oldCPU {
		risk = RiskMedium
	}
	if newMem < oldMem {
		risk = RiskHigh
	}

	evJSON, _ := json.Marshal(ev)
	return &Recommendation{
		Tenant:        spec.Tenant,
		Service:       spec.Service,
		WindowStart:   data[0].HourStart,
		WindowEnd:     now.UTC(),
		DaysOfData:    daysOfData,
		Confidence:    confidence,
		Risk:          risk,
		OldCPURequest: spec.CPURequest,
		OldMemRequest: spec.MemRequest,
		NewCPURequest: FormatCPUMillis(newCPU),
		NewMemRequest: FormatMemBytes(newMem),
		EvidenceJSON:  string(evJSON),
	}, SkipNone
}

func filterWindow(rollups []Rollup, from, to time.Time) []Rollup {
	var out []Rollup
	for _, r := range rollups {
		if r.Samples > 0 && !r.HourStart.Before(from) && r.HourStart.Before(to) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].HourStart.Before(out[j].HourStart) })
	return out
}

// recentRelease reports whether the workload's image tag changed within the
// cooldown, or the currently declared tag has not been observed yet.
func recentRelease(spec ServiceSpec, data []Rollup, cooldown time.Duration, now time.Time) bool {
	cutoff := now.Add(-cooldown)
	var lastTag string
	for _, r := range data {
		if r.ImageTag == "" {
			continue
		}
		if lastTag != "" && r.ImageTag != lastTag && r.HourStart.After(cutoff) {
			return true
		}
		lastTag = r.ImageTag
	}
	// Declared tag differs from the newest observed tag: a deploy is in
	// flight (or just landed) — observations don't describe it yet.
	if spec.ImageTag != "" && lastTag != "" && spec.ImageTag != lastTag {
		return true
	}
	return false
}

func buildEvidence(data []Rollup, daysOfData, coverage float64, now time.Time) Evidence {
	ev := Evidence{
		WindowStart: data[0].HourStart,
		WindowEnd:   now.UTC(),
		DaysOfData:  daysOfData,
		Coverage:    coverage,
		CPUP95m:     int64(quantileFloat(collect(data, func(r Rollup) float64 { return r.CPUP95m }), 0.95)),
		CPUP99m:     int64(quantileFloat(collect(data, func(r Rollup) float64 { return r.CPUP99m }), 0.99)),
		CPUMaxm:     int64(maxFloat(collect(data, func(r Rollup) float64 { return r.CPUMaxm }))),
		MemP95:      int64(quantileFloat(collect(data, func(r Rollup) float64 { return r.MemP95 }), 0.95)),
		MemP99:      int64(maxFloat(collect(data, func(r Rollup) float64 { return r.MemP99 }))),
		MemMax:      int64(maxFloat(collect(data, func(r Rollup) float64 { return r.MemMax }))),
	}

	var throttleSum, restarts, replicasSum float64
	tagSince := now
	for _, r := range data {
		throttleSum += r.ThrottleRatio
		restarts += r.Restarts
		replicasSum += r.Replicas
		if r.OOMSeen {
			ev.OOMCount++
		}
		if r.ImageTag != "" {
			if r.ImageTag != ev.ImageTag {
				ev.ImageTag = r.ImageTag
				tagSince = r.HourStart
			}
		}
	}
	ev.ThrottleRatio = throttleSum / float64(len(data))
	ev.Restarts = restarts
	ev.AvgReplicas = replicasSum / float64(len(data))
	ev.TagStableDays = now.Sub(tagSince).Hours() / 24
	return ev
}

func cpuP95CV(data []Rollup) float64 {
	vals := collect(data, func(r Rollup) float64 { return r.CPUP95m })
	var sum float64
	for _, v := range vals {
		sum += v
	}
	mean := sum / float64(len(vals))
	if mean == 0 {
		return 0
	}
	var sq float64
	for _, v := range vals {
		sq += (v - mean) * (v - mean)
	}
	return math.Sqrt(sq/float64(len(vals))) / mean
}

func collect(data []Rollup, f func(Rollup) float64) []float64 {
	out := make([]float64, len(data))
	for i, r := range data {
		out[i] = f(r)
	}
	return out
}

// quantileFloat returns the q-quantile (nearest-rank) of vals.
func quantileFloat(vals []float64, q float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sorted := append([]float64(nil), vals...)
	sort.Float64s(sorted)
	idx := int(math.Ceil(q*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func maxFloat(vals []float64) float64 {
	var m float64
	for _, v := range vals {
		if v > m {
			m = v
		}
	}
	return m
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func pctChange(old, new int64) float64 {
	if old == 0 {
		return 0
	}
	return math.Abs(float64(new-old)) / float64(old) * 100
}
