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

// LLMTokens counts tokens reported by the model provider per call
// (mctlhq/mctl-agent#38), for cost attribution across skills. Labels are all
// bounded: the model, the skill that made the call, the ticket type, and
// direction (input or output). The provider's own counts are used, never an
// estimate; a response without usage adds nothing.
var LLMTokens = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "mctl_agent_llm_tokens_total",
		Help: "Tokens reported by the model provider, by model, skill, ticket type and direction.",
	},
	[]string{"model", "skill", "ticket_type", "direction"},
)

// LLMRequests counts model calls by model, skill and outcome (ok, error).
var LLMRequests = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "mctl_agent_llm_requests_total",
		Help: "Model calls, by model, skill and outcome.",
	},
	[]string{"model", "skill", "outcome"},
)

func init() {
	// Pre-initialize all expected label combinations so the metric appears
	// in /metrics output at zero even before any ticket has been resolved.
	// Keep in sync with the thresholds map in monitor.Poller.resolveStale —
	// every status the watchdog can TTL-resolve needs a label here.
	for _, s := range []string{"open", "analyzing", "escalated", "fix_proposed"} {
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

	// Pre-populate LLMRequests so an error-rate rule has a zero series to
	// evaluate on a healthy agent. Keep in sync with the model and skill
	// name in internal/skill/builtin/llm_diagnosis.go. LLMTokens is left
	// lazy: with ticket_type in its labels the cross-product is not worth it.
	for _, outcome := range []string{"ok", "error"} {
		LLMRequests.WithLabelValues("claude-sonnet-5", "llm_diagnosis", outcome)
	}
}
