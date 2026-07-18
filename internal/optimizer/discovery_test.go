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

package optimizer

import "testing"

func TestParseServiceSpec(t *testing.T) {
	raw := `image:
  repository: ghcr.io/mctlhq/instruments-proxy
  tag: "0.8.0"
service:
  port: 8787
resources:
  requests:
    cpu: 200m
    memory: 256Mi
  limits:
    cpu: 500m
    memory: 512Mi
env:
  APP_ENV: production
`
	spec, err := parseServiceSpec("labs", "instruments-proxy", "p", raw)
	if err != nil {
		t.Fatalf("parseServiceSpec: %v", err)
	}
	if spec.CPURequest != "200m" || spec.MemRequest != "256Mi" {
		t.Errorf("requests = %q/%q, want 200m/256Mi", spec.CPURequest, spec.MemRequest)
	}
	if spec.CPULimit != "500m" || spec.MemLimit != "512Mi" {
		t.Errorf("limits = %q/%q, want 500m/512Mi", spec.CPULimit, spec.MemLimit)
	}
	if spec.ImageTag != "0.8.0" || spec.ReplicaCount != 1 {
		t.Errorf("tag/replicas = %q/%d, want 0.8.0/1", spec.ImageTag, spec.ReplicaCount)
	}
	if spec.BlueGreen || spec.Autoscaling || spec.HasExtraContainers {
		t.Errorf("flags unexpectedly set: %+v", spec)
	}
}

func TestParseServiceSpecFlags(t *testing.T) {
	raw := `image:
  tag: "1.0"
replicaCount: 3
resources:
  requests:
    cpu: 1
    memory: 1Gi
blueGreen:
  enabled: true
extraContainers:
  - name: sidecar
    resources:
      requests:
        cpu: 25m
`
	spec, err := parseServiceSpec("admins", "web", "p", raw)
	if err != nil {
		t.Fatalf("parseServiceSpec: %v", err)
	}
	if !spec.BlueGreen || !spec.HasExtraContainers || spec.ReplicaCount != 3 {
		t.Errorf("flags = %+v", spec)
	}
	// Bare integer CPU quantity must stringify, not vanish.
	if spec.CPURequest != "1" {
		t.Errorf("CPURequest = %q, want \"1\"", spec.CPURequest)
	}
}

func TestImageTag(t *testing.T) {
	tests := []struct{ in, want string }{
		{"ghcr.io/mctlhq/svc:1.2.3", "1.2.3"},
		{"ghcr.io/mctlhq/svc:1.2.3@sha256:abc", "1.2.3"},
		{"registry:5000/team/svc:2.0", "2.0"},
		{"ghcr.io/mctlhq/svc", ""},
	}
	for _, tt := range tests {
		if got := imageTag(tt.in); got != tt.want {
			t.Errorf("imageTag(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestPodSelector(t *testing.T) {
	got := podSelector("labs", "svc")
	want := `namespace="labs",pod=~"labs-svc-base-service-[a-z0-9]+-[a-z0-9]+",container="base-service"`
	if got != want {
		t.Errorf("podSelector = %s, want %s", got, want)
	}
}
