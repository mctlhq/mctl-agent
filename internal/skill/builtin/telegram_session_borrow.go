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
	"strings"

	"github.com/mctlhq/mctl-agent/internal/skill"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

const sessionBorrowAlert = "MctlTelegramSessionBorrowFastBurn"

// TelegramSessionBorrowSkill triages the mctl-telegram session-borrow SLO
// fast-burn alert. It is advisory only: it inspects the collected logs for
// corroborating failure signals and tells the operator whether the page looks
// real or like the documented low-traffic threshold crossing (see
// vm-rules/mctl-telegram-slo.yaml in mctl-gitops). It never fixes anything and
// never changes the alert rule.
type TelegramSessionBorrowSkill struct{}

func NewTelegramSessionBorrowSkill() *TelegramSessionBorrowSkill {
	return &TelegramSessionBorrowSkill{}
}

func (s *TelegramSessionBorrowSkill) Name() string    { return "telegram_session_borrow" }
func (s *TelegramSessionBorrowSkill) Version() string { return "1.0.0" }

func (s *TelegramSessionBorrowSkill) Description() string {
	return "Triages MctlTelegramSessionBorrowFastBurn by checking logs for corroborating session/Telegram-client errors"
}

func (s *TelegramSessionBorrowSkill) RequiredCapabilities() []skill.CapabilityID {
	return []skill.CapabilityID{skill.CapReadLogs, skill.CapSendNotify}
}

func (s *TelegramSessionBorrowSkill) Match(_ context.Context, t *ticket.Ticket, _ skill.EvidenceSet) skill.MatchResult {
	if t == nil || t.AlertName != sessionBorrowAlert {
		return skill.MatchResult{}
	}
	return skill.MatchResult{
		Matched:    true,
		Confidence: 0.95,
		Priority:   90,
		Reason:     "alert is MctlTelegramSessionBorrowFastBurn",
	}
}

// sessionBorrowErrorLines returns the log lines that look like real session-pool
// or Telegram-client failures.
func sessionBorrowErrorLines(logs string) []string {
	var out []string
	for _, line := range strings.Split(logs, "\n") {
		l := strings.ToLower(line)
		switch {
		case strings.Contains(l, `"level":"error"`),
			strings.Contains(l, `\"level\":\"error\"`),
			strings.Contains(l, "borrow"),
			strings.Contains(l, "mtproto"),
			strings.Contains(l, "session store"),
			strings.Contains(l, `"status":"error"`),
			strings.Contains(l, `\"status\":\"error\"`):
			out = append(out, strings.TrimSpace(line))
		}
		if len(out) >= 5 {
			break
		}
	}
	return out
}

func (s *TelegramSessionBorrowSkill) Diagnose(_ context.Context, _ *ticket.Ticket, ev skill.EvidenceSet) (*skill.DiagnosisResult, error) {
	logs := ev.Get("logs")
	if lines := sessionBorrowErrorLines(logs); len(lines) > 0 {
		return &skill.DiagnosisResult{
			Diagnosis: "Session borrow burn is corroborated by error signals in the mctl-telegram logs, so it looks like a real incident. Sample lines: " +
				strings.Join(lines, " | "),
			Confidence: ticket.ConfidenceMedium,
			Fixable:    false,
			SkillName:  s.Name(),
		}, nil
	}
	return &skill.DiagnosisResult{
		Diagnosis: "No session-pool, Telegram-client, or failed tool-call errors were found in the collected mctl-telegram logs. " +
			"This matches the caveat documented in vm-rules/mctl-telegram-slo.yaml: on a low-traffic service the 1h window holds few invocations, " +
			"so a couple of errors can cross the burn threshold. Verify mctl_telegram:session_borrow_errors:ratio_rate1h before treating it as an outage. " +
			"The alert rule is intentionally not changed.",
		Confidence: ticket.ConfidenceLow,
		Fixable:    false,
		SkillName:  s.Name(),
	}, nil
}

func (s *TelegramSessionBorrowSkill) Fix(_ context.Context, _ *ticket.Ticket, _ *skill.DiagnosisResult) (*skill.FixResult, error) {
	return nil, nil
}
