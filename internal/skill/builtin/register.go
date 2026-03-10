package builtin

import "github.com/mctlhq/mctl-agent/internal/skill"

// RegisterAll registers all built-in skills with the given registry.
func RegisterAll(reg *skill.Registry, anthropicKey string) {
	// Pattern-based skills (zero LLM cost).
	reg.Register(NewOOMKilledSkill())
	reg.Register(NewImagePullBackOffSkill())
	reg.Register(NewPostDeployRollbackSkill())
	reg.Register(NewArgoCDDriftSkill())
	reg.Register(NewProbeFixSkill())
	reg.Register(NewCPUThrottleSkill())
	reg.Register(NewQuotaAdjustSkill())
	reg.Register(NewScaleUpSkill())

	// LLM-based fallback skill.
	reg.Register(NewLLMDiagnosisSkill(anthropicKey))
}
