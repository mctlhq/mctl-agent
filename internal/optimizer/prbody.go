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
	"fmt"
	"strings"
	"time"
)

// BuildPRBody renders the right-sizing PR description. The savings framing
// is deliberately honest: on fixed nodes a request trim reclaims schedulable
// headroom, not money — claiming euros here would be fiction.
func BuildPRBody(rec *Recommendation, ev Evidence, replicas int, warmup, evalWindow time.Duration) string {
	var b strings.Builder

	fmt.Fprintf(&b, "## mctl Optimizer — resource right-sizing for %s/%s\n\n", rec.Tenant, rec.Service)
	fmt.Fprintf(&b, "**Observation window:** %s → %s (%.1f days) · **Confidence:** %s · **Risk:** %s\n\n",
		rec.WindowStart.Format("2006-01-02"), rec.WindowEnd.Format("2006-01-02"), rec.DaysOfData,
		rec.Confidence, rec.Risk)

	b.WriteString("| Resource | Was | Proposed | Change |\n|---|---|---|---|\n")
	fmt.Fprintf(&b, "| cpu request | %s | %s | %s |\n",
		rec.OldCPURequest, rec.NewCPURequest, changeCell(rec.OldCPURequest, rec.NewCPURequest, ParseCPUMillis))
	fmt.Fprintf(&b, "| memory request | %s | %s | %s |\n\n",
		rec.OldMemRequest, rec.NewMemRequest, changeCell(rec.OldMemRequest, rec.NewMemRequest, ParseMemBytes))

	b.WriteString("### Evidence (per pod, main container only)\n")
	fmt.Fprintf(&b, "- CPU p95: %dm · p99: %dm · max: %dm — proposed = p95 + buffer\n",
		ev.CPUP95m, ev.CPUP99m, ev.CPUMaxm)
	fmt.Fprintf(&b, "- Memory p99 peak: %s · max: %s — proposed = p99 peak + buffer\n",
		FormatMemBytes(ev.MemP99), FormatMemBytes(ev.MemMax))
	fmt.Fprintf(&b, "- OOMKilled events in window: %d · Restarts: %.0f · CPU throttling: %.1f%%\n",
		ev.OOMCount, ev.Restarts, ev.ThrottleRatio*100)
	fmt.Fprintf(&b, "- Replicas (avg): %.1f · Image tag %s stable for %.1f days\n\n",
		ev.AvgReplicas, ev.ImageTag, ev.TagStableDays)

	b.WriteString("### Expected reclaimed capacity\n")
	dCPU := quantityDelta(rec.OldCPURequest, rec.NewCPURequest, ParseCPUMillis) * int64(replicas)
	dMem := quantityDelta(rec.OldMemRequest, rec.NewMemRequest, ParseMemBytes) * int64(replicas)
	fmt.Fprintf(&b, "%s CPU / %s memory of scheduled requests across %d replica(s).\n",
		signedQuantity(dCPU, FormatCPUMillis), signedQuantity(dMem, FormatMemBytes), replicas)
	b.WriteString("Direct saving: €0 — node count is fixed; this frees schedulable headroom.\n")
	b.WriteString("Potential saving only after node consolidation.\n\n")

	b.WriteString("### Post-merge verification\n")
	fmt.Fprintf(&b, "After merge + %s warmup, mctl-agent observes this service for %s and posts\n",
		humanDuration(warmup), humanDuration(evalWindow))
	b.WriteString("SUCCESS or REGRESSION as a comment here. On regression a rollback PR is opened\n")
	b.WriteString("(no auto-rollback). Limits, replicas, and HPA are not touched.\n\n")

	b.WriteString("---\n")
	fmt.Fprintf(&b, "*Recommendation `%s` — generated deterministically from VictoriaMetrics data; no LLM involved. "+
		"Review before merging; this PR is never auto-merged.*\n", rec.ID)

	return b.String()
}

// BuildRollbackPRBody renders the description for a rollback PR opened after
// a REGRESSION verdict.
func BuildRollbackPRBody(rec *Recommendation, verdict *VerdictResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## mctl Optimizer — rollback right-sizing for %s/%s\n\n", rec.Tenant, rec.Service)
	fmt.Fprintf(&b, "Post-merge evaluation of #%d returned **REGRESSION**:\n\n", rec.PRNumber)
	for _, r := range verdict.Reasons {
		fmt.Fprintf(&b, "- %s\n", r)
	}
	b.WriteString("\nThis PR restores the previous resource requests:\n\n")
	b.WriteString("| Resource | Applied | Restored |\n|---|---|---|\n")
	fmt.Fprintf(&b, "| cpu request | %s | %s |\n", rec.NewCPURequest, rec.OldCPURequest)
	fmt.Fprintf(&b, "| memory request | %s | %s |\n\n", rec.NewMemRequest, rec.OldMemRequest)
	fmt.Fprintf(&b, "---\n*Recommendation `%s` — rollback proposed automatically; review and merge to restore. No auto-rollback is performed.*\n", rec.ID)
	return b.String()
}

// BuildVerdictComment renders the evaluation outcome posted on the original PR.
func BuildVerdictComment(rec *Recommendation, v *VerdictResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Optimization result: %s\n\n", v.Verdict)

	if v.Verdict == VerdictSuccess {
		b.WriteString("Resource allocation:\n")
		fmt.Fprintf(&b, "- CPU request: %s (%s)\n", rec.NewCPURequest,
			changeCell(rec.OldCPURequest, rec.NewCPURequest, ParseCPUMillis))
		fmt.Fprintf(&b, "- Memory request: %s (%s)\n\n", rec.NewMemRequest,
			changeCell(rec.OldMemRequest, rec.NewMemRequest, ParseMemBytes))
	} else {
		b.WriteString("Regression criteria fired:\n")
		for _, r := range v.Reasons {
			fmt.Fprintf(&b, "- %s\n", r)
		}
		b.WriteString("\n")
	}

	b.WriteString("Service stability (baseline → evaluation):\n")
	fmt.Fprintf(&b, "- CPU p95: %.0fm → %.0fm\n", v.BaselineCPUP95m, v.EvalCPUP95m)
	fmt.Fprintf(&b, "- Memory p99: %s → %s\n", FormatMemBytes(int64(v.BaselineMemP99)), FormatMemBytes(int64(v.EvalMemP99)))
	fmt.Fprintf(&b, "- CPU throttling: %.1f%% → %.1f%%\n", v.BaselineThrottle*100, v.EvalThrottle*100)
	fmt.Fprintf(&b, "- Restarts/day: %.2f → %.2f\n", v.BaselineRestartsPerDay, v.EvalRestartsPerDay)
	fmt.Fprintf(&b, "- OOMKilled: %d → %d\n\n", v.BaselineOOM, v.EvalOOM)

	if v.Verdict == VerdictSuccess {
		b.WriteString("Conclusion: resource usage reduced without meaningful degradation.\n")
	} else {
		b.WriteString("Recommended action: restore previous requests via the rollback PR linked below.\n")
	}
	b.WriteString("\n*Latency/error-rate comparison is not part of this evaluation (no app metrics exposed); " +
		"resource and stability signals only.*\n")
	return b.String()
}

func changeCell(oldQ, newQ string, parse func(string) (int64, error)) string {
	oldV, err1 := parse(oldQ)
	newV, err2 := parse(newQ)
	if err1 != nil || err2 != nil || oldV == 0 {
		return "—"
	}
	if oldV == newV {
		return "unchanged"
	}
	return fmt.Sprintf("%+.1f%%", float64(newV-oldV)/float64(oldV)*100)
}

func quantityDelta(oldQ, newQ string, parse func(string) (int64, error)) int64 {
	oldV, err1 := parse(oldQ)
	newV, err2 := parse(newQ)
	if err1 != nil || err2 != nil {
		return 0
	}
	return oldV - newV // positive = reclaimed
}

// signedQuantity renders a reclaimed-capacity delta; a negative value means
// the change adds requests rather than reclaiming them.
func signedQuantity(v int64, format func(int64) string) string {
	if v < 0 {
		return "-" + format(-v)
	}
	return format(v)
}

func humanDuration(d time.Duration) string {
	if d%(24*time.Hour) == 0 {
		days := int(d / (24 * time.Hour))
		if days == 1 {
			return "24h"
		}
		return fmt.Sprintf("%d days", days)
	}
	return d.String()
}
