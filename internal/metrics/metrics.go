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

package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// StaleTTLResolved counts tickets auto-resolved by Phase 1's stale-TTL
// GC, labelled by the ticket's status at the moment of resolution
// (open, analyzing, or fix_proposed).
var StaleTTLResolved = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "mctl_agent_stale_ttl_resolved_total",
		Help: "Tickets auto-resolved by the stale-TTL GC, by previous status.",
	},
	[]string{"status"},
)

// OrphanPruned counts tickets auto-resolved by orphan pruning (service
// no longer in inventory).
var OrphanPruned = promauto.NewCounter(
	prometheus.CounterOpts{
		Name: "mctl_agent_orphan_pruned_total",
		Help: "Tickets auto-resolved by orphan pruning (service no longer in inventory).",
	},
)

// AMReconcileResolved counts tickets auto-resolved by AlertManager
// fingerprint reconciliation.
var AMReconcileResolved = promauto.NewCounter(
	prometheus.CounterOpts{
		Name: "mctl_agent_am_reconcile_resolved_total",
		Help: "Tickets auto-resolved by AlertManager fingerprint reconciliation.",
	},
)

// CleanupSkipped counts cleanup passes short-circuited by safety guards,
// labelled by reason.
var CleanupSkipped = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "mctl_agent_cleanup_skipped_total",
		Help: "Cleanup passes short-circuited by safety guards.",
	},
	[]string{"reason"},
)

// AMRequestDuration observes AlertManager /api/v2/alerts request duration,
// labelled by outcome.
var AMRequestDuration = promauto.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "mctl_agent_am_request_duration_seconds",
		Help:    "AlertManager /api/v2/alerts request duration.",
		Buckets: prometheus.DefBuckets,
	},
	[]string{"outcome"},
)

// OpenTickets is a gauge of non-terminal tickets by status and source.
var OpenTickets = promauto.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "mctl_agent_open_tickets",
		Help: "Non-terminal tickets by status and source.",
	},
	[]string{"status", "source"},
)

func init() {
	// Pre-initialize all expected label combinations so the metric appears
	// in /metrics output at zero even before any ticket has been resolved.
	for _, s := range []string{"open", "analyzing", "fix_proposed"} {
		StaleTTLResolved.WithLabelValues(s)
	}

	// Pre-populate CleanupSkipped label combinations.
	for _, reason := range []string{"empty_inventory", "am_unknown", "am_empty_set", "am_fetch_error"} {
		CleanupSkipped.WithLabelValues(reason)
	}

	// Pre-populate AMRequestDuration with a synthetic zero observation so
	// the histogram series appears at first scrape for all outcome labels.
	for _, outcome := range []string{"success", "http_error", "decode_error", "transport_error"} {
		AMRequestDuration.WithLabelValues(outcome).Observe(0)
	}
}
