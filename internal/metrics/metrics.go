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
// (open, analyzing, or fix_proposed). Phase 4b will add the remaining
// five handles to this package.
var StaleTTLResolved = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "mctl_agent_stale_ttl_resolved_total",
		Help: "Tickets auto-resolved by the stale-TTL GC, by previous status.",
	},
	[]string{"status"},
)

func init() {
	// Pre-initialize all expected label combinations so the metric appears
	// in /metrics output at zero even before any ticket has been resolved.
	for _, s := range []string{"open", "analyzing", "fix_proposed"} {
		StaleTTLResolved.WithLabelValues(s)
	}
}
