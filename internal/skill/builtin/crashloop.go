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

package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/mctlhq/mctl-agent/internal/skill"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

// CrashLoopSkill reports what a restarting container actually did: which
// container, how many restarts, how it last exited, and the last error lines
// it wrote. It reads pod state, which is where a crash loop is visible; the
// application's own log never contains "CrashLoopBackOff".
//
// It proposes no fix. The cause of a crash on startup is in the error line,
// and the right change (a config value, a dependency, a rollback) is a
// decision, not a patch this skill can derive.
type CrashLoopSkill struct{}

func NewCrashLoopSkill() *CrashLoopSkill { return &CrashLoopSkill{} }

func (s *CrashLoopSkill) Name() string    { return "crashloop" }
func (s *CrashLoopSkill) Version() string { return "1.0.0" }

func (s *CrashLoopSkill) Description() string {
	return "Reports the exit code and last error lines of a container that keeps restarting"
}

func (s *CrashLoopSkill) RequiredCapabilities() []skill.CapabilityID {
	return []skill.CapabilityID{skill.CapReadLogs, skill.CapReadStatus}
}

// crashLoopMinRestarts is how many restarts with a failed last exit count as
// a loop when the kubelet does not say CrashLoopBackOff itself. One or two
// restarts are what a rollout or a node drain leaves behind.
const crashLoopMinRestarts = 3

type crashState struct {
	Kind     string `json:"kind"`
	Reason   string `json:"reason"`
	Message  string `json:"message"`
	ExitCode *int32 `json:"exit_code"`
	Signal   *int32 `json:"signal"`
}

type crashContainer struct {
	Name            string      `json:"name"`
	Ready           bool        `json:"ready"`
	RestartCount    int32       `json:"restart_count"`
	State           *crashState `json:"state"`
	LastTermination *crashState `json:"last_termination"`
}

type crashPod struct {
	Name           string           `json:"name"`
	InitContainers []crashContainer `json:"init_containers"`
	Containers     []crashContainer `json:"containers"`
}

// looping is one container found in a crash loop.
type looping struct {
	pod       string
	container crashContainer
	init      bool
}

// loopingContainers returns the containers in a crash loop, worst first.
// Evidence that is absent or does not parse yields none: the skill then does
// not match, and never reports "no restarts" from a read that failed.
func loopingContainers(ev skill.EvidenceSet) []looping {
	raw := ev.Get("pods")
	if raw == "" {
		return nil
	}
	var resp struct {
		Items []crashPod `json:"items"`
	}
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		return nil
	}
	var out []looping
	add := func(pod string, cs []crashContainer, init bool) {
		for _, c := range cs {
			if inCrashLoop(c) {
				out = append(out, looping{pod: pod, container: c, init: init})
			}
		}
	}
	for _, p := range resp.Items {
		add(p.Name, p.InitContainers, true)
		add(p.Name, p.Containers, false)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].container.RestartCount > out[j].container.RestartCount
	})
	return out
}

func inCrashLoop(c crashContainer) bool {
	if c.State != nil && c.State.Kind == "waiting" && c.State.Reason == "CrashLoopBackOff" {
		return true
	}
	if c.RestartCount < crashLoopMinRestarts || c.LastTermination == nil {
		return false
	}
	// A container that is running and ready again has recovered; its old
	// restart count is history, not a loop.
	// A loop is caught "running" between two crashes, so running alone
	// clears nothing; it has to be ready as well.
	if c.Ready && c.State != nil && c.State.Kind == "running" {
		return false
	}
	lt := c.LastTermination
	return lt.Reason != "Completed" && (lt.ExitCode == nil || *lt.ExitCode != 0)
}

func (s *CrashLoopSkill) Match(_ context.Context, _ *ticket.Ticket, ev skill.EvidenceSet) skill.MatchResult {
	found := loopingContainers(ev)
	if len(found) == 0 {
		return skill.MatchResult{}
	}
	return skill.MatchResult{
		Matched: true,
		// Below oomkilled, rollback, imagepull and probe_fix, which name a
		// cause and a fix; above a bare "ArgoCD app is Degraded".
		Confidence: 0.78,
		Priority:   72,
		Reason:     fmt.Sprintf("container %s of pod %s is in a crash loop", found[0].container.Name, found[0].pod),
	}
}

func (s *CrashLoopSkill) Diagnose(_ context.Context, _ *ticket.Ticket, ev skill.EvidenceSet) (*skill.DiagnosisResult, error) {
	found := loopingContainers(ev)
	if len(found) == 0 {
		return nil, fmt.Errorf("no container in a crash loop in the pod evidence")
	}
	var sb strings.Builder
	for i, l := range found {
		if i == 3 {
			fmt.Fprintf(&sb, "…and %d more container(s).\n", len(found)-3)
			break
		}
		kind := "Container"
		if l.init {
			kind = "Init container"
		}
		fmt.Fprintf(&sb, "%s %s of pod %s keeps restarting: %d restart(s), %s.\n",
			kind, l.container.Name, l.pod, l.container.RestartCount, describeExit(l.container))
	}
	if lines := lastErrorLines(ev.Get("logs"), 3); len(lines) > 0 {
		sb.WriteString("Last error lines in its log:\n")
		for _, line := range lines {
			fmt.Fprintf(&sb, "  %s\n", line)
		}
	} else {
		sb.WriteString("The collected log holds no error line; the container may exit before it logs.\n")
	}
	if w := warningEvents(ev.Get("events"), 3); w != "" {
		fmt.Fprintf(&sb, "Warning events: %s.\n", w)
	}
	sb.WriteString("No fix is proposed: the cause is in the error above.")
	return &skill.DiagnosisResult{
		Diagnosis:  sb.String(),
		Confidence: ticket.ConfidenceMedium,
		Fixable:    false,
		SkillName:  s.Name(),
	}, nil
}

func (s *CrashLoopSkill) Fix(context.Context, *ticket.Ticket, *skill.DiagnosisResult) (*skill.FixResult, error) {
	return nil, fmt.Errorf("crashloop reports a cause and proposes no fix")
}

func describeExit(c crashContainer) string {
	lt := c.LastTermination
	if lt == nil {
		return "no previous termination recorded"
	}
	var parts []string
	if lt.ExitCode != nil {
		parts = append(parts, fmt.Sprintf("last exit code %d", *lt.ExitCode))
	}
	if lt.Signal != nil && *lt.Signal != 0 {
		parts = append(parts, fmt.Sprintf("signal %d", *lt.Signal))
	}
	if lt.Reason != "" {
		parts = append(parts, "reason "+lt.Reason)
	}
	if len(parts) == 0 {
		return "last termination recorded without an exit code"
	}
	return strings.Join(parts, ", ")
}

// maxErrorLineLen bounds one quoted log line. The diagnosis goes to the
// operators' chat and into the ticket; it should name the failure, not carry
// a payload.
const maxErrorLineLen = 240

// lastErrorLines returns up to n of the most recent log lines that look like
// an error, oldest first.
func lastErrorLines(raw string, n int) []string {
	if raw == "" {
		return nil
	}
	var resp struct {
		Lines []struct {
			Line string `json:"line"`
		} `json:"lines"`
	}
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		return nil
	}
	var out []string
	for i := len(resp.Lines) - 1; i >= 0 && len(out) < n; i-- {
		line := strings.TrimSpace(resp.Lines[i].Line)
		lower := strings.ToLower(line)
		if !containsAny(lower, "error", "fatal", "panic") {
			continue
		}
		if r := []rune(line); len(r) > maxErrorLineLen {
			line = string(r[:maxErrorLineLen]) + "…"
		}
		out = append(out, line)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// warningEvents summarises the Warning events by reason, most frequent first.
func warningEvents(raw string, n int) string {
	if raw == "" {
		return ""
	}
	var resp struct {
		Items []struct {
			Reason string `json:"reason"`
			Count  int32  `json:"count"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		return ""
	}
	counts := map[string]int32{}
	for _, e := range resp.Items {
		c := e.Count
		if c < 1 {
			c = 1
		}
		counts[e.Reason] += c
	}
	reasons := make([]string, 0, len(counts))
	for r := range counts {
		reasons = append(reasons, r)
	}
	sort.Slice(reasons, func(i, j int) bool {
		if counts[reasons[i]] != counts[reasons[j]] {
			return counts[reasons[i]] > counts[reasons[j]]
		}
		return reasons[i] < reasons[j]
	})
	if len(reasons) > n {
		reasons = reasons[:n]
	}
	parts := make([]string, len(reasons))
	for i, r := range reasons {
		parts[i] = fmt.Sprintf("%s ×%d", r, counts[r])
	}
	return strings.Join(parts, ", ")
}
