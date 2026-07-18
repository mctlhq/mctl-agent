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
	"testing"
	"time"

	"github.com/mctlhq/mctl-agent/internal/config"
)

const mi = int64(1 << 20)

func testPolicy() config.OptimizerConfig {
	return config.OptimizerConfig{
		MinDays:        7,
		TargetDays:     14,
		CPUBuffer:      0.30,
		MemBuffer:      0.20,
		MinChangePct:   20,
		MinCPUMillis:   10,
		MinMemBytes:    32 * mi,
		DeployCooldown: 168 * time.Hour,
	}
}

func testSpec() ServiceSpec {
	return ServiceSpec{
		Tenant: "labs", Service: "svc", ImageTag: "1.0.0",
		CPURequest: "500m", MemRequest: "512Mi",
		CPULimit: "1000m", MemLimit: "1Gi",
		ReplicaCount: 1,
	}
}

// steadyRollups builds `hours` hourly rollups ending one hour before now,
// with constant per-hour stats.
func steadyRollups(now time.Time, hours int, cpuP95m, memP99Mi float64) []Rollup {
	out := make([]Rollup, 0, hours)
	start := now.Truncate(time.Hour).Add(-time.Duration(hours) * time.Hour)
	for i := 0; i < hours; i++ {
		out = append(out, Rollup{
			Tenant: "labs", Service: "svc",
			HourStart: start.Add(time.Duration(i) * time.Hour),
			CPUP95m:   cpuP95m, CPUP99m: cpuP95m * 1.2, CPUMaxm: cpuP95m * 1.5,
			MemP95: memP99Mi * 0.9 * float64(mi), MemP99: memP99Mi * float64(mi), MemMax: memP99Mi * 1.05 * float64(mi),
			Replicas: 1, Samples: 60, ImageTag: "1.0.0",
		})
	}
	return out
}

func TestRecommendShrink(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	rollups := steadyRollups(now, 336, 142, 301) // 14 days, CPU p95 142m, mem p99 301Mi

	rec, skip := Recommend(testSpec(), rollups, testPolicy(), GuardInputs{}, now)
	if skip != SkipNone || rec == nil {
		t.Fatalf("skip = %q, want none", skip)
	}
	// 142 * 1.3 = 184.6 → 184 → round up to 190m
	if rec.NewCPURequest != "190m" {
		t.Errorf("NewCPURequest = %s, want 190m", rec.NewCPURequest)
	}
	// 301Mi * 1.2 = 361.2Mi → round up to 32Mi step = 384Mi
	if rec.NewMemRequest != "384Mi" {
		t.Errorf("NewMemRequest = %s, want 384Mi", rec.NewMemRequest)
	}
	if rec.Confidence != ConfidenceHigh {
		t.Errorf("Confidence = %s, want HIGH", rec.Confidence)
	}
	if rec.Risk != RiskHigh { // memory shrink
		t.Errorf("Risk = %s, want HIGH", rec.Risk)
	}
	if rec.EvidenceJSON == "" {
		t.Error("EvidenceJSON empty")
	}
}

func TestRecommendOOMBlocksMemShrink(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	rollups := steadyRollups(now, 336, 142, 301)
	rollups[100].OOMSeen = true

	rec, skip := Recommend(testSpec(), rollups, testPolicy(), GuardInputs{}, now)
	if skip != SkipNone || rec == nil {
		t.Fatalf("skip = %q, want none", skip)
	}
	if rec.NewMemRequest != "512Mi" {
		t.Errorf("NewMemRequest = %s, want unchanged 512Mi (OOM in window)", rec.NewMemRequest)
	}
	if rec.NewCPURequest != "190m" {
		t.Errorf("NewCPURequest = %s, want 190m (CPU may still shrink)", rec.NewCPURequest)
	}
	if rec.Risk != RiskMedium { // CPU-only shrink
		t.Errorf("Risk = %s, want MEDIUM", rec.Risk)
	}
}

func TestRecommendMediumConfidenceBlocksMemShrink(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	rollups := steadyRollups(now, 192, 142, 301) // 8 days → MEDIUM

	rec, skip := Recommend(testSpec(), rollups, testPolicy(), GuardInputs{}, now)
	if skip != SkipNone || rec == nil {
		t.Fatalf("skip = %q, want none", skip)
	}
	if rec.Confidence != ConfidenceMedium {
		t.Errorf("Confidence = %s, want MEDIUM", rec.Confidence)
	}
	if rec.NewMemRequest != "512Mi" {
		t.Errorf("NewMemRequest = %s, want unchanged (mem shrink needs HIGH confidence)", rec.NewMemRequest)
	}
}

func TestRecommendBelowMinChange(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	spec := testSpec()
	spec.CPURequest = "200m"
	spec.MemRequest = "256Mi"
	// cpu: 150*1.3 = 195 → 200m (0% change); mem: 220*1.2 = 264 → 288Mi (12.5% < 20%)
	rollups := steadyRollups(now, 336, 150, 220)

	_, skip := Recommend(spec, rollups, testPolicy(), GuardInputs{}, now)
	if skip != SkipBelowMinChange {
		t.Fatalf("skip = %q, want below_min_change", skip)
	}
}

func TestRecommendGrowClampedByLimitAndQuota(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	spec := testSpec()
	spec.CPURequest = "100m"
	spec.CPULimit = "300m"
	spec.MemRequest = "512Mi"
	rollups := steadyRollups(now, 336, 400, 301) // cpu p95 way above request

	// Own limit clamps: 400*1.3 = 520 → clamped to 300m.
	rec, skip := Recommend(spec, rollups, testPolicy(), GuardInputs{}, now)
	if skip != SkipNone || rec == nil {
		t.Fatalf("skip = %q, want none", skip)
	}
	if rec.NewCPURequest != "300m" {
		t.Errorf("NewCPURequest = %s, want 300m (clamped to container limit)", rec.NewCPURequest)
	}

	// Quota headroom too small for the grow: revert; mem shrink still passes,
	// so this yields a recommendation with cpu unchanged.
	g := GuardInputs{QuotaCPUUsedM: 1950, QuotaCPUHardM: 2000}
	rec, skip = Recommend(spec, rollups, testPolicy(), g, now)
	if skip != SkipNone || rec == nil {
		t.Fatalf("skip = %q, want none", skip)
	}
	if rec.NewCPURequest != "100m" {
		t.Errorf("NewCPURequest = %s, want unchanged 100m (quota exceeded)", rec.NewCPURequest)
	}

	// With memory also unchanged the whole thing collapses to quota_exceeded.
	spec.MemRequest = "384Mi" // 301*1.2=361.2→384Mi == old
	_, skip = Recommend(spec, rollups, testPolicy(), g, now)
	if skip != SkipQuotaExceeded {
		t.Fatalf("skip = %q, want quota_exceeded", skip)
	}
}

func TestRecommendLimitRangeClamp(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	spec := testSpec()
	spec.CPURequest = "100m"
	spec.CPULimit = "4000m"
	rollups := steadyRollups(now, 336, 2000, 301)

	g := GuardInputs{LimitRangeMaxCPUM: 1500}
	rec, skip := Recommend(spec, rollups, testPolicy(), g, now)
	if skip != SkipNone || rec == nil {
		t.Fatalf("skip = %q, want none", skip)
	}
	if rec.NewCPURequest != "1500m" {
		t.Errorf("NewCPURequest = %s, want 1500m (LimitRange max)", rec.NewCPURequest)
	}
}

func TestRecommendMinFloors(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	spec := testSpec()
	spec.CPURequest = "100m"
	spec.MemRequest = "64Mi"
	rollups := steadyRollups(now, 336, 1, 4) // nearly idle

	rec, skip := Recommend(spec, rollups, testPolicy(), GuardInputs{}, now)
	if skip != SkipNone || rec == nil {
		t.Fatalf("skip = %q, want none", skip)
	}
	if rec.NewCPURequest != "10m" {
		t.Errorf("NewCPURequest = %s, want floor 10m", rec.NewCPURequest)
	}
	if rec.NewMemRequest != "32Mi" {
		t.Errorf("NewMemRequest = %s, want floor 32Mi", rec.NewMemRequest)
	}
}

func TestRecommendSkips(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	good := steadyRollups(now, 336, 142, 301)

	tests := []struct {
		name    string
		mutSpec func(*ServiceSpec)
		rollups []Rollup
		guards  GuardInputs
		want    SkipReason
	}{
		{
			name:    "blueGreen excluded",
			mutSpec: func(s *ServiceSpec) { s.BlueGreen = true },
			rollups: good,
			want:    SkipNotPlainDeployment,
		},
		{
			name:    "autoscaling excluded",
			mutSpec: func(s *ServiceSpec) { s.Autoscaling = true },
			rollups: good,
			want:    SkipNotPlainDeployment,
		},
		{
			name:    "no requests block",
			mutSpec: func(s *ServiceSpec) { s.CPURequest = "" },
			rollups: good,
			want:    SkipNotPlainDeployment,
		},
		{
			name:    "ignored by allowlist/regex",
			rollups: good,
			guards:  GuardInputs{Ignored: true},
			want:    SkipIgnored,
		},
		{
			name:    "insufficient data",
			rollups: steadyRollups(now, 72, 142, 301), // 3 days
			want:    SkipInsufficientData,
		},
		{
			name:    "active incident",
			rollups: good,
			guards:  GuardInputs{OpenTickets: true},
			want:    SkipActiveIncident,
		},
		{
			name:    "open recommendation",
			rollups: good,
			guards:  GuardInputs{HasOpenRecommendation: true},
			want:    SkipOpenRecommendation,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := testSpec()
			if tt.mutSpec != nil {
				tt.mutSpec(&spec)
			}
			_, skip := Recommend(spec, tt.rollups, testPolicy(), tt.guards, now)
			if skip != tt.want {
				t.Errorf("skip = %q, want %q", skip, tt.want)
			}
		})
	}
}

func TestRecommendRecentRelease(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)

	// Tag flipped 2 days ago — inside the 7d cooldown.
	rollups := steadyRollups(now, 336, 142, 301)
	for i := 336 - 48; i < 336; i++ {
		rollups[i].ImageTag = "1.1.0"
	}
	spec := testSpec()
	spec.ImageTag = "1.1.0"
	_, skip := Recommend(spec, rollups, testPolicy(), GuardInputs{}, now)
	if skip != SkipRecentRelease {
		t.Fatalf("skip = %q, want recent_release (tag change in cooldown)", skip)
	}

	// Declared tag never observed yet — deploy in flight.
	rollups = steadyRollups(now, 336, 142, 301)
	spec.ImageTag = "2.0.0"
	_, skip = Recommend(spec, rollups, testPolicy(), GuardInputs{}, now)
	if skip != SkipRecentRelease {
		t.Fatalf("skip = %q, want recent_release (declared tag unobserved)", skip)
	}
}

func TestQuantityHelpers(t *testing.T) {
	if v, _ := ParseCPUMillis("500m"); v != 500 {
		t.Errorf("ParseCPUMillis(500m) = %d", v)
	}
	if v, _ := ParseCPUMillis("1"); v != 1000 {
		t.Errorf("ParseCPUMillis(1) = %d", v)
	}
	if v, _ := ParseCPUMillis("0.5"); v != 500 {
		t.Errorf("ParseCPUMillis(0.5) = %d", v)
	}
	if v, _ := ParseMemBytes("256Mi"); v != 256*mi {
		t.Errorf("ParseMemBytes(256Mi) = %d", v)
	}
	if v, _ := ParseMemBytes("1Gi"); v != 1024*mi {
		t.Errorf("ParseMemBytes(1Gi) = %d", v)
	}
	if s := FormatMemBytes(384 * mi); s != "384Mi" {
		t.Errorf("FormatMemBytes(384Mi) = %s", s)
	}
	if s := FormatMemBytes(8 * 1024 * mi); s != "8Gi" {
		t.Errorf("FormatMemBytes(8Gi) = %s", s)
	}
	if s := FormatCPUMillis(250); s != "250m" {
		t.Errorf("FormatCPUMillis(250) = %s", s)
	}
}
