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

package diagnosis

import (
	"strings"
	"time"

	"github.com/mctlhq/mctl-agent/internal/ticket"
)

// PatternResult is the output of a known pattern match.
type PatternResult struct {
	Matched    bool
	Diagnosis  string
	Confidence string
	Fixable    bool
	FixType    string // "bump_memory", "rollback_image", etc.
}

// MatchKnownPattern checks evidence against known patterns that don't need LLM analysis.
func MatchKnownPattern(t *ticket.Ticket, recentDeploy bool) PatternResult {
	logs := collectLogs(t)

	// OOMKilled in logs → bump memory.
	if containsAny(logs, "OOMKilled", "oom-kill", "Out of memory") {
		return PatternResult{
			Matched:    true,
			Diagnosis:  "Container killed due to OOM (Out of Memory). Memory limit needs to be increased.",
			Confidence: ticket.ConfidenceHigh,
			Fixable:    true,
			FixType:    "bump_memory",
		}
	}

	// ImagePullBackOff → no auto-fix.
	if containsAny(logs, "ImagePullBackOff", "ErrImagePull", "image pull failed") {
		return PatternResult{
			Matched:    true,
			Diagnosis:  "Container image pull failed. Check image tag exists and registry credentials are valid.",
			Confidence: ticket.ConfidenceMedium,
			Fixable:    false,
		}
	}

	// CrashLoop + recent deploy → rollback.
	if t.Type == ticket.TypePodCrashloop && recentDeploy {
		return PatternResult{
			Matched:    true,
			Diagnosis:  "Pod crash-looping after recent deployment. Rollback to previous version recommended.",
			Confidence: ticket.ConfidenceHigh,
			Fixable:    true,
			FixType:    "rollback_image",
		}
	}

	// OutOfSync but Healthy → no action needed.
	for _, ev := range t.Evidence {
		if ev.Type == "argocd_status" && strings.Contains(ev.Content, `"syncStatus":"OutOfSync"`) &&
			strings.Contains(ev.Content, `"health":"Healthy"`) {
			return PatternResult{
				Matched:    true,
				Diagnosis:  "ArgoCD app is OutOfSync but healthy. Sync will happen automatically or may be intentional drift.",
				Confidence: ticket.ConfidenceLow,
				Fixable:    false,
			}
		}
	}

	return PatternResult{Matched: false}
}

// HasRecentDeploy checks audit entries for a deploy within the last 30 minutes.
func HasRecentDeploy(tenant, service string, auditEntries []AuditEntry) bool {
	cutoff := time.Now().UTC().Add(-30 * time.Minute)
	target := tenant + "/" + service
	for _, e := range auditEntries {
		if e.Timestamp.After(cutoff) && strings.Contains(e.Target, target) {
			return true
		}
	}
	return false
}

// AuditEntry mirrors the mctlclient type for convenience.
type AuditEntry struct {
	User      string    `json:"user"`
	Action    string    `json:"action"`
	Target    string    `json:"target"`
	Timestamp time.Time `json:"timestamp"`
}

func collectLogs(t *ticket.Ticket) string {
	var sb strings.Builder
	for _, ev := range t.Evidence {
		if ev.Type == "logs" || ev.Type == "alert" {
			sb.WriteString(ev.Content)
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

func containsAny(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
