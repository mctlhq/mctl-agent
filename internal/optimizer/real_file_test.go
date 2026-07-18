package optimizer

import (
	"os"
	"strings"
	"testing"
)

// TestGenerateRequestsPatchRealKuptsiFile exercises the patcher against the
// real gitops file when a sibling mctl-gitops checkout exists (dev/CI-local
// only; silently skipped elsewhere).
func TestGenerateRequestsPatchRealKuptsiFile(t *testing.T) {
	raw, err := os.ReadFile("../../../mctl-gitops/platform-gitops/services/labs/kuptsi-app/values.yaml")
	if err != nil {
		t.Skip("mctl-gitops checkout not available")
	}
	patched, _, err := GenerateRequestsPatch(string(raw), "80m", "224Mi")
	if err != nil {
		t.Fatalf("GenerateRequestsPatch on real file: %v", err)
	}
	orig := strings.Split(string(raw), "\n")
	got := strings.Split(patched, "\n")
	if len(orig) != len(got) {
		t.Fatalf("line count changed")
	}
	var changed []int
	for i := range orig {
		if orig[i] != got[i] {
			changed = append(changed, i+1)
		}
	}
	if len(changed) != 2 {
		t.Fatalf("changed lines %v, want exactly the two top-level request lines", changed)
	}
	if !strings.Contains(patched, "        cpu: 100m") {
		t.Error("temporal-worker sidecar requests were touched")
	}
}
