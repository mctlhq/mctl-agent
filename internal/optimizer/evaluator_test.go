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
	"strings"
	"testing"
	"time"
)

func evalRollups(hours int, mut func(i int, r *Rollup)) []Rollup {
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	out := make([]Rollup, hours)
	for i := range out {
		out[i] = Rollup{
			Tenant: "labs", Service: "svc",
			HourStart: start.Add(time.Duration(i) * time.Hour),
			CPUP95m:   140, CPUP99m: 170, CPUMaxm: 200,
			MemP95: 250 * float64(mi), MemP99: 280 * float64(mi), MemMax: 300 * float64(mi),
			Replicas: 1, Samples: 60, ImageTag: "1.0.0",
		}
		if mut != nil {
			mut(i, &out[i])
		}
	}
	return out
}

func TestComputeVerdict(t *testing.T) {
	baseline := evalRollups(336, nil)

	tests := []struct {
		name       string
		eval       []Rollup
		memShrunk  bool
		memLimit   int64
		want       string
		wantReason string
	}{
		{
			name: "clean success",
			eval: evalRollups(168, nil),
			want: VerdictSuccess,
		},
		{
			name: "OOM regression",
			eval: evalRollups(168, func(i int, r *Rollup) {
				if i == 50 {
					r.OOMSeen = true
				}
			}),
			want:       VerdictRegression,
			wantReason: "OOMKilled",
		},
		{
			name: "sustained throttle regression",
			eval: evalRollups(168, func(i int, r *Rollup) {
				r.ThrottleRatio = 0.08 // mean 8% > 5%
			}),
			want:       VerdictRegression,
			wantReason: "mean CPU throttling",
		},
		{
			name: "spiky throttle regression",
			eval: evalRollups(168, func(i int, r *Rollup) {
				if i%10 == 0 { // 10% of hours above 10%
					r.ThrottleRatio = 0.30
				}
			}),
			want:       VerdictRegression,
			wantReason: "eval hours",
		},
		{
			name: "restart rate regression",
			eval: evalRollups(168, func(i int, r *Rollup) {
				if i%12 == 0 { // 2/day vs baseline 0
					r.Restarts = 1
				}
			}),
			want:       VerdictRegression,
			wantReason: "restart rate",
		},
		{
			name: "memory near limit after shrink",
			eval: evalRollups(168, func(i int, r *Rollup) {
				r.MemMax = 500 * float64(mi) // > 95% of 512Mi
			}),
			memShrunk:  true,
			memLimit:   512 * mi,
			want:       VerdictRegression,
			wantReason: "within 5% of the",
		},
		{
			name: "memory near limit without shrink is fine",
			eval: evalRollups(168, func(i int, r *Rollup) {
				r.MemMax = 500 * float64(mi)
			}),
			memShrunk: false,
			memLimit:  512 * mi,
			want:      VerdictSuccess,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := ComputeVerdict(baseline, tt.eval, tt.memShrunk, tt.memLimit)
			if v.Verdict != tt.want {
				t.Fatalf("verdict = %s (reasons %v), want %s", v.Verdict, v.Reasons, tt.want)
			}
			if tt.wantReason != "" {
				found := false
				for _, r := range v.Reasons {
					if strings.Contains(r, tt.wantReason) {
						found = true
					}
				}
				if !found {
					t.Errorf("reasons %v missing %q", v.Reasons, tt.wantReason)
				}
			}
		})
	}
}

func TestBuildPRBodyGolden(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	rec := &Recommendation{
		ID:     "11111111-2222-3333-4444-555555555555",
		Tenant: "labs", Service: "svc",
		WindowStart: now.Add(-14 * 24 * time.Hour), WindowEnd: now,
		DaysOfData: 14, Confidence: ConfidenceHigh, Risk: RiskHigh,
		OldCPURequest: "500m", OldMemRequest: "512Mi",
		NewCPURequest: "250m", NewMemRequest: "384Mi",
	}
	ev := Evidence{
		CPUP95m: 142, CPUP99m: 181, CPUMaxm: 220,
		MemP95: 274 * mi, MemP99: 301 * mi, MemMax: 315 * mi,
		OOMCount: 0, Restarts: 0, ThrottleRatio: 0.003,
		AvgReplicas: 3, ImageTag: "1.0.0", TagStableDays: 21.4,
	}
	body := BuildPRBody(rec, ev, 3, 24*time.Hour, 168*time.Hour)

	// The honest-framing lines are the contract of this template.
	for _, want := range []string{
		"## mctl Optimizer — resource right-sizing for labs/svc",
		"**Confidence:** HIGH · **Risk:** HIGH",
		"| cpu request | 500m | 250m | -50.0% |",
		"| memory request | 512Mi | 384Mi | -25.0% |",
		"CPU p95: 142m · p99: 181m · max: 220m",
		"Memory p99 peak: 301Mi · max: 315Mi",
		"OOMKilled events in window: 0 · Restarts: 0 · CPU throttling: 0.3%",
		"750m CPU / 384Mi memory of scheduled requests across 3 replica(s).",
		"Direct saving: €0 — node count is fixed; this frees schedulable headroom.",
		"Potential saving only after node consolidation.",
		"After merge + 24h warmup, mctl-agent observes this service for 7 days",
		"no LLM involved",
		"this PR is never auto-merged",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("PR body missing %q\n---\n%s", want, body)
		}
	}
}

func TestBuildVerdictComment(t *testing.T) {
	rec := &Recommendation{
		Tenant: "labs", Service: "svc", PRNumber: 42,
		OldCPURequest: "500m", NewCPURequest: "250m",
		OldMemRequest: "512Mi", NewMemRequest: "384Mi",
	}
	v := &VerdictResult{
		Verdict:         VerdictRegression,
		Reasons:         []string{"mean CPU throttling 19.0% (limit 5%)"},
		BaselineCPUP95m: 142, EvalCPUP95m: 240,
		BaselineMemP99: 301 * float64(mi), EvalMemP99: 310 * float64(mi),
		BaselineThrottle: 0.003, EvalThrottle: 0.19,
	}
	c := BuildVerdictComment(rec, v)
	if !strings.Contains(c, "Optimization result: REGRESSION") ||
		!strings.Contains(c, "mean CPU throttling") ||
		!strings.Contains(c, "rollback PR") {
		t.Errorf("comment:\n%s", c)
	}

	v.Verdict = VerdictSuccess
	v.Reasons = nil
	c = BuildVerdictComment(rec, v)
	if !strings.Contains(c, "Optimization result: SUCCESS") ||
		!strings.Contains(c, "without meaningful degradation") {
		t.Errorf("comment:\n%s", c)
	}
}
