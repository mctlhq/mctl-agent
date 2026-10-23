package builtin

import (
	"os"
	"regexp"
	"strconv"
	"testing"

	"github.com/mctlhq/mctl-agent/internal/skill"
)

// CLAUDE.md states the number of compiled skills; it drifted from
// RegisterAll twice already, so pin it the same way server_test.go pins the
// MCP tool count in mctl-api.
func TestClaudeMDSkillCount(t *testing.T) {
	t.Setenv("DISABLE_LLM_DIAGNOSIS", "")
	reg := skill.NewRegistry()
	RegisterAll(reg, "test-key")

	raw, err := os.ReadFile("../../../CLAUDE.md")
	if err != nil {
		t.Fatalf("read CLAUDE.md: %v", err)
	}
	m := regexp.MustCompile(`(\d+) compiled Go skills`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("CLAUDE.md has no \"<N> compiled Go skills\" line")
	}
	documented, _ := strconv.Atoi(string(m[1]))
	if documented != reg.Count() {
		t.Errorf("CLAUDE.md says %d compiled Go skills, RegisterAll registers %d — update the Skill Types line", documented, reg.Count())
	}
}
