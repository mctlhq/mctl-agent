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

	"github.com/mctlhq/mctl-agent/internal/skill"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

// ImagePullBackOffSkill detects image pull failures.
type ImagePullBackOffSkill struct{}

func NewImagePullBackOffSkill() *ImagePullBackOffSkill { return &ImagePullBackOffSkill{} }

func (s *ImagePullBackOffSkill) Name() string    { return "imagepull" }
func (s *ImagePullBackOffSkill) Version() string { return "1.0.0" }

func (s *ImagePullBackOffSkill) Description() string {
	return "Detects image pull failures (ImagePullBackOff, ErrImagePull)"
}

func (s *ImagePullBackOffSkill) RequiredCapabilities() []skill.CapabilityID {
	return []skill.CapabilityID{skill.CapReadLogs, skill.CapSendNotify}
}

func (s *ImagePullBackOffSkill) Match(_ context.Context, _ *ticket.Ticket, ev skill.EvidenceSet) skill.MatchResult {
	logs := ev.Get("logs") + "\n" + ev.Get("alert")
	if containsAny(logs, "ImagePullBackOff", "ErrImagePull", "image pull failed") {
		return skill.MatchResult{
			Matched:    true,
			Confidence: 0.85,
			Priority:   90,
			Reason:     "Image pull error detected in logs/alert",
		}
	}
	return skill.MatchResult{}
}

func (s *ImagePullBackOffSkill) Diagnose(_ context.Context, _ *ticket.Ticket, _ skill.EvidenceSet) (*skill.DiagnosisResult, error) {
	return &skill.DiagnosisResult{
		Diagnosis:  "Container image pull failed. Check image tag exists and registry credentials are valid.",
		Confidence: ticket.ConfidenceMedium,
		Fixable:    false,
		SkillName:  s.Name(),
	}, nil
}

func (s *ImagePullBackOffSkill) Fix(_ context.Context, _ *ticket.Ticket, _ *skill.DiagnosisResult) (*skill.FixResult, error) {
	return nil, nil // Not fixable automatically.
}
