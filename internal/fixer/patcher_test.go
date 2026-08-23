package fixer

import (
	"strings"
	"testing"
)

func TestGenerateMemoryBump(t *testing.T) {
	content := `resources:
  limits:
    memory: 512Mi
    cpu: 500m`

	newContent, summary, err := GenerateMemoryBump(content)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "512Mi") || !strings.Contains(summary, "768Mi") {
		t.Errorf("unexpected summary: %s", summary)
	}
	if !strings.Contains(newContent, "memory: 768Mi") {
		t.Errorf("expected 768Mi in output, got:\n%s", newContent)
	}
}

func TestGenerateCPUBump(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		wantCPU  string
		wantSumm string
		wantErr  bool
	}{
		{
			name: "millicore bump",
			content: `resources:
  limits:
    cpu: 500m
    memory: 512Mi`,
			wantCPU:  "cpu: 750m",
			wantSumm: "Bump CPU limit from 500m to 750m",
		},
		{
			name: "whole core bump",
			content: `resources:
  limits:
    cpu: 1
    memory: 512Mi`,
			wantCPU:  "cpu: 1500m",
			wantSumm: "Bump CPU limit from 1 to 1500m",
		},
		{
			name: "no limits section",
			content: `resources:
  requests:
    cpu: 100m`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			newContent, summary, err := GenerateCPUBump(tt.content)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(newContent, tt.wantCPU) {
				t.Errorf("expected %q in output, got:\n%s", tt.wantCPU, newContent)
			}
			if summary != tt.wantSumm {
				t.Errorf("summary = %q, want %q", summary, tt.wantSumm)
			}
		})
	}
}

func TestGenerateProbeFix(t *testing.T) {
	tests := []struct {
		name       string
		content    string
		probeField string
		wantDelay  string
		wantErr    bool
	}{
		{
			name: "liveness probe default",
			content: `livenessProbe:
  httpGet:
    path: /healthz
  initialDelaySeconds: 10
  periodSeconds: 10`,
			probeField: "livenessProbe.initialDelaySeconds",
			wantDelay:  "initialDelaySeconds: 30",
		},
		{
			name: "readiness probe already >= 30",
			content: `readinessProbe:
  httpGet:
    path: /ready
  initialDelaySeconds: 30
  periodSeconds: 5`,
			probeField: "readinessProbe.initialDelaySeconds",
			wantDelay:  "initialDelaySeconds: 45",
		},
		{
			name: "missing probe section",
			content: `containers:
  - name: app
    image: myapp:latest`,
			probeField: "livenessProbe.initialDelaySeconds",
			wantErr:    true,
		},
		{
			// The base-service chart nests probes under "probes:" with bare
			// liveness/readiness keys. Every tenant values.yaml uses this
			// shape, so a manifest-only lookup patched none of them.
			name: "base-service chart shape",
			content: `probes:
  liveness:
    path: /healthz
    initialDelaySeconds: 10
  readiness:
    path: /readyz
    initialDelaySeconds: 5`,
			probeField: "livenessProbe.initialDelaySeconds",
			wantDelay:  "initialDelaySeconds: 30",
		},
		{
			// A probe block that relies on the chart default has no
			// initialDelaySeconds line to bump; insert one instead of failing.
			name: "probe block without initialDelaySeconds",
			content: `probes:
  liveness:
    path: /healthz
    periodSeconds: 15`,
			probeField: "livenessProbe.initialDelaySeconds",
			wantDelay:  "    initialDelaySeconds: 30",
		},
		{
			// A bare "liveness:" outside a probes: mapping is an unrelated key.
			name: "bare liveness key outside probes mapping",
			content: `featureFlags:
  liveness:
    enabled: true`,
			probeField: "livenessProbe.initialDelaySeconds",
			wantErr:    true,
		},
		{
			name:       "unrecognised probe field",
			content:    "livenessProbe:\n  initialDelaySeconds: 10",
			probeField: "bogusProbe.initialDelaySeconds",
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			newContent, _, err := GenerateProbeFix(tt.content, tt.probeField)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(newContent, tt.wantDelay) {
				t.Errorf("expected %q in output, got:\n%s", tt.wantDelay, newContent)
			}
		})
	}
}

// TestGenerateProbeFixAmbiguousSide covers the regression seen live on
// 2026-08-23: when the logs only say the generic "probe failed", probe_fix
// emits "liveness/readinessProbe.initialDelaySeconds". That is not a YAML key
// anywhere, so the patcher used to fail with "could not find
// liveness/readinessProbe.initialDelaySeconds initialDelaySeconds to bump"
// and the ticket stalled in fix_proposed with an empty patch.
func TestGenerateProbeFixAmbiguousSide(t *testing.T) {
	content := `probes:
  liveness:
    path: /healthz
    initialDelaySeconds: 10
  readiness:
    path: /readyz
    initialDelaySeconds: 5`

	newContent, summary, err := GenerateProbeFix(content, "liveness/readinessProbe.initialDelaySeconds")
	if err != nil {
		t.Fatalf("ambiguous probe side must still patch: %v", err)
	}
	if strings.Count(newContent, "initialDelaySeconds: 30") != 2 {
		t.Errorf("expected both probes bumped to 30, got:\n%s", newContent)
	}
	if summary == "" {
		t.Error("expected a non-empty summary")
	}
}

// TestGenerateProbeFixComments covers the three review findings on PR #91:
// a comment indented deeper than its sibling keys must not become the block's
// first child, an inline comment on the initialDelaySeconds line must not
// defeat the number parse, and the comment must survive the rewrite.
func TestGenerateProbeFixComments(t *testing.T) {
	t.Run("deeply indented comment is not the first child", func(t *testing.T) {
		content := `probes:
  liveness:
      # the app needs a while to warm its caches
    path: /healthz
    periodSeconds: 15`

		newContent, _, err := GenerateProbeFix(content, "livenessProbe.initialDelaySeconds")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(newContent, "\n    initialDelaySeconds: 30\n") {
			t.Errorf("insert must take its indent from a real key, got:\n%s", newContent)
		}
		if strings.Count(newContent, "initialDelaySeconds") != 1 {
			t.Errorf("expected exactly one initialDelaySeconds, got:\n%s", newContent)
		}
	})

	t.Run("comment hides an existing initialDelaySeconds", func(t *testing.T) {
		// With the comment treated as the first child, childIndent came from
		// the comment, the real initialDelaySeconds no longer matched it, and
		// a second one was spliced in — corrupting the YAML.
		content := `probes:
  liveness:
      # bumped once already
    initialDelaySeconds: 10
    periodSeconds: 15`

		newContent, _, err := GenerateProbeFix(content, "livenessProbe.initialDelaySeconds")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(newContent, "initialDelaySeconds") != 1 {
			t.Errorf("existing field must be bumped, not duplicated, got:\n%s", newContent)
		}
		if !strings.Contains(newContent, "initialDelaySeconds: 30") {
			t.Errorf("expected the existing field bumped to 30, got:\n%s", newContent)
		}
	})

	t.Run("inline comment on the delay line", func(t *testing.T) {
		content := `probes:
  liveness:
    initialDelaySeconds: 10  # matches the chart default`

		newContent, _, err := GenerateProbeFix(content, "livenessProbe.initialDelaySeconds")
		if err != nil {
			t.Fatalf("an inline comment must not make the block unpatchable: %v", err)
		}
		if !strings.Contains(newContent, "initialDelaySeconds: 30  # matches the chart default") {
			t.Errorf("expected the bump to preserve the comment, got:\n%s", newContent)
		}
	})

	t.Run("comment on the probe key itself", func(t *testing.T) {
		content := `probes:
  liveness: # slow starter
    initialDelaySeconds: 10`

		newContent, _, err := GenerateProbeFix(content, "livenessProbe.initialDelaySeconds")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(newContent, "initialDelaySeconds: 30") {
			t.Errorf("expected the block to be found, got:\n%s", newContent)
		}
	})
}

// TestGenerateProbeFixSummaryPairsKinds pins the summary to the right probe:
// patching runs bottom-up, so a summary built in patch order reported the
// readiness value against liveness and vice versa.
func TestGenerateProbeFixSummaryPairsKinds(t *testing.T) {
	content := `probes:
  liveness:
    initialDelaySeconds: 40
  readiness:
    initialDelaySeconds: 5`

	_, summary, err := GenerateProbeFix(content, "liveness/readinessProbe.initialDelaySeconds")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "liveness 40 -> 55") {
		t.Errorf("liveness must be paired with its own value, got: %s", summary)
	}
	if !strings.Contains(summary, "readiness 5 -> 30") {
		t.Errorf("readiness must be paired with its own value, got: %s", summary)
	}
	if strings.Index(summary, "liveness") > strings.Index(summary, "readiness") {
		t.Errorf("summary must keep file order, got: %s", summary)
	}
}

func TestGenerateImageRollback(t *testing.T) {
	t.Run("rewrites only the chart-level tag", func(t *testing.T) {
		// Mirrors openclaw values.yaml shape: chart-level image at the top,
		// sidecar / init container images deeper with their own `tag:` keys
		// that must NOT be touched by a rollback.
		content := `image:
  repository: ghcr.io/mctlhq/mctl-openclaw
  tag: "2026.4.29-beta.2"
sidecar:
  image:
    repository: ghcr.io/mctlhq/whisper-builder
    tag: "2026.3.24-beta.19"`

		newContent, summary, err := GenerateImageRollback(content, "2026.4.29-beta.1")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(newContent, `tag: "2026.4.29-beta.1"`) {
			t.Errorf("expected new chart tag, got:\n%s", newContent)
		}
		if !strings.Contains(newContent, `tag: "2026.3.24-beta.19"`) {
			t.Errorf("sidecar tag must remain untouched, got:\n%s", newContent)
		}
		if !strings.Contains(summary, "2026.4.29-beta.2") || !strings.Contains(summary, "2026.4.29-beta.1") {
			t.Errorf("summary missing tag transition: %s", summary)
		}
	})

	t.Run("missing tag returns error", func(t *testing.T) {
		_, _, err := GenerateImageRollback("image:\n  repository: foo", "1.0.0")
		if err == nil {
			t.Error("expected error when no tag found")
		}
	})

	t.Run("preserves trailing inline comment", func(t *testing.T) {
		content := `image:
  repository: ghcr.io/mctlhq/mctl-openclaw
  tag: "broken-2.0"  # bumped 2026-04-30 by deploy bot`

		newContent, _, err := GenerateImageRollback(content, "good-1.0")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(newContent, `# bumped 2026-04-30 by deploy bot`) {
			t.Errorf("inline comment must be preserved, got:\n%s", newContent)
		}
		if !strings.Contains(newContent, `tag: "good-1.0"`) {
			t.Errorf("tag must be flipped, got:\n%s", newContent)
		}
	})

	t.Run("sub-image block earlier in file is not the rollback target", func(t *testing.T) {
		content := `sidecar:
  image:
    repository: ghcr.io/foo/sidecar
    tag: "do-not-touch-sub"
image:
  repository: ghcr.io/mctlhq/mctl-openclaw
  tag: "broken-2.0"`

		newContent, summary, err := GenerateImageRollback(content, "good-1.0")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(newContent, `tag: "do-not-touch-sub"`) {
			t.Errorf("sub-image tag must remain untouched, got:\n%s", newContent)
		}
		if !strings.Contains(newContent, `tag: "good-1.0"`) {
			t.Errorf("chart tag must be flipped, got:\n%s", newContent)
		}
		if !strings.Contains(summary, "broken-2.0") {
			t.Errorf("summary should reference chart's old tag: %s", summary)
		}
	})

	t.Run("indented image: block (platform inline-values shape)", func(t *testing.T) {
		content := `spec:
  source:
    helm:
      values: |
        image:
          repository: ghcr.io/mctlhq/mctl-agent
          tag: "broken-2.0"
        service:
          port: 8080`

		newContent, summary, err := GenerateImageRollback(content, "good-1.0")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(newContent, `tag: "good-1.0"`) {
			t.Errorf("expected indented tag to be flipped, got:\n%s", newContent)
		}
		if !strings.Contains(summary, "broken-2.0") {
			t.Errorf("summary should reference old tag: %s", summary)
		}
		// Indent on the rewritten line must match the original 10-space depth.
		if !strings.Contains(newContent, `          tag: "good-1.0"`) {
			t.Errorf("expected 10-space indent preserved, got:\n%s", newContent)
		}
	})

	t.Run("nested map under image with its own tag is not rewritten", func(t *testing.T) {
		content := `image:
  pullPolicy: IfNotPresent
  pullSecrets:
    - name: ghcr
      tag: "do-not-touch-nested"
  repository: foo
  tag: "broken-2.0"`

		newContent, _, err := GenerateImageRollback(content, "good-1.0")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(newContent, `tag: "do-not-touch-nested"`) {
			t.Errorf("nested tag must remain untouched, got:\n%s", newContent)
		}
		if !strings.Contains(newContent, `tag: "good-1.0"`) {
			t.Errorf("chart tag must be flipped, got:\n%s", newContent)
		}
	})

	t.Run("global.tag earlier in file is not the rollback target", func(t *testing.T) {
		content := `global:
  tag: "do-not-touch"
image:
  repository: ghcr.io/mctlhq/mctl-openclaw
  tag: "broken-2.0"`

		newContent, summary, err := GenerateImageRollback(content, "good-1.0")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(newContent, `tag: "do-not-touch"`) {
			t.Errorf("global.tag must remain untouched, got:\n%s", newContent)
		}
		if !strings.Contains(newContent, `tag: "good-1.0"`) {
			t.Errorf("expected chart tag to be flipped to good-1.0, got:\n%s", newContent)
		}
		if !strings.Contains(summary, "broken-2.0") {
			t.Errorf("summary should reference the chart tag, not the global one: %s", summary)
		}
	})
}

func TestBumpCPU(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"500m", "750m"},
		{"1000m", "1500m"},
		{"200m", "300m"},
		{"1", "1500m"},
		{"2", "3000m"},
	}
	for _, tt := range tests {
		got := bumpCPU(tt.input)
		if got != tt.want {
			t.Errorf("bumpCPU(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
