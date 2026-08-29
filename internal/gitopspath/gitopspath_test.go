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

package gitopspath

import "testing"

func TestDefaultAllowlistValidate(t *testing.T) {
	a := DefaultAllowlist()

	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"empty path rejected", "", true},
		{"traversal rejected", "../.github/workflows/ci.yml", true},
		{"absolute path rejected", "/etc/passwd", true},
		{"traversal inside allowed prefix rejected", "platform-gitops/services/../../etc/passwd", true},
		{"off-prefix path rejected", "bootstrap/x.yaml", true},
		{"tenant values file accepted", "platform-gitops/services/acme/api/values.yaml", false},
		{"platform service template accepted", "platform-gitops/apps/templates/mctl-agent.yaml", false},
		{"workflow template accepted", "platform-gitops/argo-workflows/workflow-templates/wft-deploy-service.yaml", false},
		{"project apps template accepted", "platform-gitops/apps/templates/projects/project-apps.yaml", false},
		{"prefix without trailing content rejected", "platform-gitops/services", true},
		{"lookalike prefix rejected", "platform-gitops/services-evil/x.yaml", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := a.Validate(tt.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate(%q) error = %v, wantErr %v", tt.path, err, tt.wantErr)
			}
		})
	}
}

func TestNewAllowlistCustomPrefixes(t *testing.T) {
	a := NewAllowlist([]string{"custom/prefix/"})

	if err := a.Validate("custom/prefix/file.yaml"); err != nil {
		t.Errorf("expected custom prefix to be accepted, got error: %v", err)
	}
	if err := a.Validate("platform-gitops/services/acme/api/values.yaml"); err == nil {
		t.Error("expected default prefix to be rejected when not in custom allowlist")
	}
}
