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
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/mctlhq/mctl-agent/internal/fixer"
)

// stalePRAfter is how long an unreviewed optimizer PR stays open before the
// evaluator closes it (mirrors AUTO_RESOLVE_FIX_PROPOSED_AFTER's spirit).
const stalePRAfter = 14 * 24 * time.Hour

// evalCoverageExtension is how much extra time the evaluator waits for
// missing eval-window data before evaluating with whatever exists.
const evalCoverageExtension = 48 * time.Hour

// VerdictResult is the concrete outcome of a post-merge evaluation,
// serialized into optimizer_runs.verdict_json and rendered in PR comments.
type VerdictResult struct {
	Verdict string   `json:"verdict"`
	Reasons []string `json:"reasons,omitempty"`

	BaselineCPUP95m        float64 `json:"baseline_cpu_p95_m"`
	EvalCPUP95m            float64 `json:"eval_cpu_p95_m"`
	BaselineMemP99         float64 `json:"baseline_mem_p99"`
	EvalMemP99             float64 `json:"eval_mem_p99"`
	BaselineThrottle       float64 `json:"baseline_throttle"`
	EvalThrottle           float64 `json:"eval_throttle"`
	BaselineRestartsPerDay float64 `json:"baseline_restarts_per_day"`
	EvalRestartsPerDay     float64 `json:"eval_restarts_per_day"`
	BaselineOOM            int     `json:"baseline_oom"`
	EvalOOM                int     `json:"eval_oom"`
	EvalHours              int     `json:"eval_hours"`
}

// ComputeVerdict compares eval-window rollups against the baseline.
// REGRESSION criteria (any one fires):
//  1. any OOMKilled in the eval window (post-warmup);
//  2. mean throttle ratio > 5%, or > 10% in at least 10% of eval hours;
//  3. restart rate exceeding baseline by more than 0.5/day;
//  4. memory-shrink only: hourly memory max within 5% of the memory limit
//     (running at the edge of OOM even without one yet).
//
// memLimitBytes <= 0 disables criterion 4.
func ComputeVerdict(baseline, eval []Rollup, memShrunk bool, memLimitBytes int64) *VerdictResult {
	v := &VerdictResult{Verdict: VerdictSuccess, EvalHours: len(eval)}

	v.BaselineCPUP95m = quantileFloat(collect(baseline, func(r Rollup) float64 { return r.CPUP95m }), 0.95)
	v.EvalCPUP95m = quantileFloat(collect(eval, func(r Rollup) float64 { return r.CPUP95m }), 0.95)
	v.BaselineMemP99 = maxFloat(collect(baseline, func(r Rollup) float64 { return r.MemP99 }))
	v.EvalMemP99 = maxFloat(collect(eval, func(r Rollup) float64 { return r.MemP99 }))

	var evalHighThrottleHours int
	var maxEvalMem float64
	for _, r := range baseline {
		v.BaselineThrottle += r.ThrottleRatio
		v.BaselineRestartsPerDay += r.Restarts
		if r.OOMSeen {
			v.BaselineOOM++
		}
	}
	for _, r := range eval {
		v.EvalThrottle += r.ThrottleRatio
		v.EvalRestartsPerDay += r.Restarts
		if r.OOMSeen {
			v.EvalOOM++
		}
		if r.ThrottleRatio > 0.10 {
			evalHighThrottleHours++
		}
		if r.MemMax > maxEvalMem {
			maxEvalMem = r.MemMax
		}
	}
	if n := len(baseline); n > 0 {
		v.BaselineThrottle /= float64(n)
		v.BaselineRestartsPerDay = v.BaselineRestartsPerDay / float64(n) * 24
	}
	if n := len(eval); n > 0 {
		v.EvalThrottle /= float64(n)
		v.EvalRestartsPerDay = v.EvalRestartsPerDay / float64(n) * 24
	}

	if v.EvalOOM > 0 {
		v.Reasons = append(v.Reasons, fmt.Sprintf("%d OOMKilled event(s) during evaluation", v.EvalOOM))
	}
	if v.EvalThrottle > 0.05 {
		v.Reasons = append(v.Reasons, fmt.Sprintf("mean CPU throttling %.1f%% (limit 5%%)", v.EvalThrottle*100))
	} else if len(eval) > 0 && float64(evalHighThrottleHours)/float64(len(eval)) >= 0.10 {
		v.Reasons = append(v.Reasons, fmt.Sprintf("CPU throttling above 10%% in %d/%d eval hours", evalHighThrottleHours, len(eval)))
	}
	if v.EvalRestartsPerDay > v.BaselineRestartsPerDay+0.5 {
		v.Reasons = append(v.Reasons, fmt.Sprintf("restart rate %.2f/day vs baseline %.2f/day",
			v.EvalRestartsPerDay, v.BaselineRestartsPerDay))
	}
	if memShrunk && memLimitBytes > 0 && maxEvalMem > 0.95*float64(memLimitBytes) {
		v.Reasons = append(v.Reasons, fmt.Sprintf("memory peak %s within 5%% of the %s limit",
			FormatMemBytes(int64(maxEvalMem)), FormatMemBytes(memLimitBytes)))
	}

	if len(v.Reasons) > 0 {
		v.Verdict = VerdictRegression
	}
	return v
}

// evaluatorTick advances every non-done run one step: merge detection,
// warmup promotion, and final evaluation. Failures are logged per run and
// retried on the next tick.
func (o *Optimizer) evaluatorTick(ctx context.Context, now time.Time) {
	runs, err := o.store.RunsByStatus(RunStatusWaitingMerge, RunStatusWarmup, RunStatusEvaluating)
	if err != nil {
		slog.Error("optimizer: listing runs failed", "error", err)
		return
	}
	for _, run := range runs {
		rec, err := o.store.GetRecommendation(run.RecommendationID)
		if err != nil || rec == nil {
			slog.Error("optimizer: run has no recommendation", "run", run.ID, "error", err)
			continue
		}
		switch run.Status {
		case RunStatusWaitingMerge:
			o.checkMerge(ctx, run, rec, now)
		case RunStatusWarmup:
			if run.WarmupUntil != nil && !now.Before(*run.WarmupUntil) {
				run.Status = RunStatusEvaluating
				rec.Status = RecStatusEvaluating
				if err := o.store.UpdateRun(run); err != nil {
					slog.Error("optimizer: run update failed", "run", run.ID, "error", err)
					continue
				}
				_ = o.store.UpdateRecommendation(rec)
			}
		case RunStatusEvaluating:
			o.evaluate(ctx, run, rec, now)
		}
	}
}

// checkMerge polls the PR and transitions the run on merge/close/staleness.
func (o *Optimizer) checkMerge(ctx context.Context, run *Run, rec *Recommendation, now time.Time) {
	pr, err := o.gitops.GetPR(ctx, rec.PRNumber)
	if err != nil {
		slog.Warn("optimizer: PR poll failed", "pr", rec.PRNumber, "error", err)
		return
	}

	switch {
	case pr.GetMerged():
		mergedAt := pr.GetMergedAt().Time.UTC()
		warmupUntil := mergedAt.Add(o.cfg.Warmup)
		evalStart := warmupUntil
		evalEnd := evalStart.Add(o.cfg.EvalWindow)

		run.MergedAt = &mergedAt
		run.WarmupUntil = &warmupUntil
		run.EvalStart = &evalStart
		run.EvalEnd = &evalEnd
		run.Status = RunStatusWarmup

		rec.Status = RecStatusMerged
		rec.MergeSHA = pr.GetMergeCommitSHA()
		rec.MergedAt = &mergedAt

		if err := o.store.UpdateRun(run); err != nil {
			slog.Error("optimizer: run update failed", "run", run.ID, "error", err)
			return
		}
		_ = o.store.UpdateRecommendation(rec)
		o.notify(fmt.Sprintf("Optimizer: right-size PR #%d for %s/%s merged — evaluating for %s after %s warmup.",
			rec.PRNumber, rec.Tenant, rec.Service, humanDuration(o.cfg.EvalWindow), humanDuration(o.cfg.Warmup)))

	case pr.GetState() == "closed":
		rec.Status = RecStatusClosed
		run.Status = RunStatusDone
		_ = o.store.UpdateRecommendation(rec)
		_ = o.store.UpdateRun(run)
		o.notify(fmt.Sprintf("Optimizer: right-size PR #%d for %s/%s was closed without merging.",
			rec.PRNumber, rec.Tenant, rec.Service))

	case now.Sub(rec.CreatedAt) > stalePRAfter:
		if err := o.gitops.ClosePR(ctx, rec.PRNumber, "optimizer recommendation expired unreviewed after 14 days"); err != nil {
			slog.Warn("optimizer: closing stale PR failed", "pr", rec.PRNumber, "error", err)
			return
		}
		rec.Status = RecStatusClosed
		run.Status = RunStatusDone
		_ = o.store.UpdateRecommendation(rec)
		_ = o.store.UpdateRun(run)
	}
}

// evaluate finalizes a run whose eval window has passed, posts the verdict
// comment, and opens a rollback PR on regression.
func (o *Optimizer) evaluate(ctx context.Context, run *Run, rec *Recommendation, now time.Time) {
	if run.EvalStart == nil || run.EvalEnd == nil {
		slog.Error("optimizer: evaluating run without eval window", "run", run.ID)
		return
	}
	if now.Before(*run.EvalEnd) {
		return
	}

	evalRollups, err := o.store.RollupsInWindow(rec.Tenant, rec.Service, *run.EvalStart, *run.EvalEnd)
	if err != nil {
		slog.Error("optimizer: eval rollups query failed", "run", run.ID, "error", err)
		return
	}

	// Sparse data: give the collector extra time before judging.
	windowHours := run.EvalEnd.Sub(*run.EvalStart).Hours()
	if float64(len(evalRollups)) < 0.6*windowHours && now.Before(run.EvalEnd.Add(evalCoverageExtension)) {
		return
	}

	baseline, err := o.store.RollupsInWindow(rec.Tenant, rec.Service, run.BaselineStart, run.BaselineEnd)
	if err != nil {
		slog.Error("optimizer: baseline rollups query failed", "run", run.ID, "error", err)
		return
	}

	memShrunk := false
	if oldMem, err1 := ParseMemBytes(rec.OldMemRequest); err1 == nil {
		if newMem, err2 := ParseMemBytes(rec.NewMemRequest); err2 == nil {
			memShrunk = newMem < oldMem
		}
	}
	verdict := ComputeVerdict(baseline, evalRollups, memShrunk, o.memLimitFor(ctx, rec))

	verdictJSON, _ := json.Marshal(verdict)
	run.Verdict = verdict.Verdict
	run.VerdictJSON = string(verdictJSON)
	run.Status = RunStatusDone
	if verdict.Verdict == VerdictSuccess {
		rec.Status = RecStatusSuccess
	} else {
		rec.Status = RecStatusRegression
	}

	comment := BuildVerdictComment(rec, verdict)
	if verdict.Verdict == VerdictRegression {
		if url, num := o.openRollbackPR(ctx, rec, verdict); num > 0 {
			rec.RollbackPRNum = num
			rec.RollbackPRURL = url
			comment += fmt.Sprintf("\nRollback PR: #%d\n", num)
		}
	}
	if err := o.gitops.CreatePRComment(ctx, rec.PRNumber, comment); err != nil {
		slog.Warn("optimizer: verdict comment failed", "pr", rec.PRNumber, "error", err)
	} else {
		run.CommentPosted = true
	}

	if err := o.store.UpdateRun(run); err != nil {
		slog.Error("optimizer: run update failed", "run", run.ID, "error", err)
	}
	if err := o.store.UpdateRecommendation(rec); err != nil {
		slog.Error("optimizer: recommendation update failed", "rec", rec.ID, "error", err)
	}

	o.notify(fmt.Sprintf("Optimizer verdict for %s/%s: %s (PR #%d)%s",
		rec.Tenant, rec.Service, verdict.Verdict, rec.PRNumber,
		rollbackSuffix(rec)))
}

// openRollbackPR restores the pre-recommendation requests. If the file has
// diverged from the recommended values (someone already changed them), no
// rollback PR is opened.
func (o *Optimizer) openRollbackPR(ctx context.Context, rec *Recommendation, verdict *VerdictResult) (string, int) {
	path := fmt.Sprintf("%s/%s/%s/values.yaml", servicesRoot, rec.Tenant, rec.Service)
	content, err := o.gitops.GetFileContent(ctx, path, "main")
	if err != nil {
		slog.Error("optimizer: rollback fetch failed", "path", path, "error", err)
		return "", 0
	}

	spec, err := parseServiceSpec(rec.Tenant, rec.Service, path, content)
	if err != nil {
		slog.Error("optimizer: rollback parse failed", "path", path, "error", err)
		return "", 0
	}
	if spec.CPURequest != rec.NewCPURequest || spec.MemRequest != rec.NewMemRequest {
		slog.Warn("optimizer: values diverged since merge, skipping rollback PR",
			"tenant", rec.Tenant, "service", rec.Service,
			"cpu", spec.CPURequest, "memory", spec.MemRequest)
		return "", 0
	}

	patched, summary, err := GenerateRequestsPatch(content, rec.OldCPURequest, rec.OldMemRequest)
	if err != nil {
		slog.Error("optimizer: rollback patch failed", "path", path, "error", err)
		return "", 0
	}

	branch := fmt.Sprintf("agent/optimize/rollback-%s-%s-%s", rec.Tenant, rec.Service, time.Now().UTC().Format("20060102"))
	url, num, err := o.gitops.CreatePRRaw(ctx, fixer.RawPRRequest{
		Branch: branch,
		Title:  fmt.Sprintf("mctl optimizer: rollback right-size %s/%s", rec.Tenant, rec.Service),
		Body:   BuildRollbackPRBody(rec, verdict),
		CommitMessage: fmt.Sprintf("chore(%s): rollback resource right-sizing\n\n%s\nRecommendation: %s\nAutomated by mctl-agent optimizer",
			rec.Service, summary, rec.ID),
		FilePath:   path,
		NewContent: patched,
	})
	if err != nil {
		slog.Error("optimizer: rollback PR failed", "tenant", rec.Tenant, "service", rec.Service, "error", err)
		return "", 0
	}
	return url, num
}

// memLimitFor resolves the workload's current memory limit for the near-OOM
// criterion; 0 disables it.
func (o *Optimizer) memLimitFor(ctx context.Context, rec *Recommendation) int64 {
	path := fmt.Sprintf("%s/%s/%s/values.yaml", servicesRoot, rec.Tenant, rec.Service)
	content, err := o.gitops.GetFileContent(ctx, path, "main")
	if err != nil {
		return 0
	}
	spec, err := parseServiceSpec(rec.Tenant, rec.Service, path, content)
	if err != nil {
		return 0
	}
	limit, err := ParseMemBytes(spec.MemLimit)
	if err != nil {
		return 0
	}
	return limit
}

func rollbackSuffix(rec *Recommendation) string {
	if rec.RollbackPRNum > 0 {
		return fmt.Sprintf(" — rollback PR #%d opened", rec.RollbackPRNum)
	}
	return ""
}
