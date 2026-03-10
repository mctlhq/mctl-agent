package builtin

import "github.com/mctlhq/mctl-agent/internal/skill"

// RegisterAll registers all built-in skills with the given registry.
func RegisterAll(reg *skill.Registry, anthropicKey string) {
	reg.Register(NewOOMKilledSkill())
	reg.Register(NewImagePullBackOffSkill())
	reg.Register(NewPostDeployRollbackSkill())
	reg.Register(NewArgoCDDriftSkill())
	reg.Register(NewLLMDiagnosisSkill(anthropicKey))
}
