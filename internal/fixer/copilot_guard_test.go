package fixer

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// copilotHosts are Copilot API hosts that must not appear in non-test Go
// source without revisiting docs/spikes/copilot-s2s-auth.md.
var copilotHosts = []string{
	"api.githubcopilot.com",
	"copilot-proxy.githubusercontent.com",
}

// findCopilotHost returns the first Copilot host contained in src, or "".
func findCopilotHost(src string) string {
	for _, h := range copilotHosts {
		if strings.Contains(src, h) {
			return h
		}
	}
	return ""
}

func TestFindCopilotHost(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"clean", `const u = "https://api.github.com"`, ""},
		{"api host", `const u = "https://api.githubcopilot.com/chat"`, "api.githubcopilot.com"},
		{"proxy host", `x := "copilot-proxy.githubusercontent.com"`, "copilot-proxy.githubusercontent.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := findCopilotHost(tt.src); got != tt.want {
				t.Fatalf("findCopilotHost() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNoCopilotHostsInSource(t *testing.T) {
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if h := findCopilotHost(string(b)); h != "" {
			t.Errorf("%s references Copilot host %q; revisit docs/spikes/copilot-s2s-auth.md", path, h)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
