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
		name       string
		content    string
		wantCPU    string
		wantSumm   string
		wantErr    bool
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
