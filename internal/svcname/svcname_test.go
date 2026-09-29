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

package svcname

import (
	"reflect"
	"testing"
)

func TestCandidates(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		derived   string
		labels    map[string]string
		want      []string
	}{
		{
			name:      "base-service chart fullname leads with the app name, raw value last",
			namespace: "labs",
			derived:   "labs-mctl-telegram-base-service",
			want:      []string{"mctl-telegram", "labs-mctl-telegram", "labs-mctl-telegram-base-service"},
		},
		{
			name:      "base-service chart fullname for a platform-style tenant",
			namespace: "admins",
			derived:   "admins-mctl-api-base-service",
			want:      []string{"mctl-api", "admins-mctl-api", "admins-mctl-api-base-service"},
		},
		{
			name:      "backstage.io/kubernetes-id identity label wins, used as-is",
			namespace: "labs",
			derived:   "labs-mctl-telegram-base-service",
			labels:    map[string]string{"backstage.io/kubernetes-id": "mctl-telegram"},
			want:      []string{"mctl-telegram", "labs-mctl-telegram", "labs-mctl-telegram-base-service"},
		},
		{
			name:      "label_app_kubernetes_io_instance identity label strips the namespace prefix",
			namespace: "labs",
			derived:   "labs-mctl-telegram-base-service",
			labels:    map[string]string{"label_app_kubernetes_io_instance": "labs-mctl-telegram"},
			want:      []string{"mctl-telegram", "labs-mctl-telegram", "labs-mctl-telegram-base-service"},
		},
		{
			name:      "label_backstage_io_kubernetes_id identity label used as-is",
			namespace: "labs",
			derived:   "labs-mctl-telegram-base-service",
			labels:    map[string]string{"label_backstage_io_kubernetes_id": "mctl-telegram"},
			want:      []string{"mctl-telegram", "labs-mctl-telegram", "labs-mctl-telegram-base-service"},
		},
		{
			name:      "app.kubernetes.io/instance identity label strips the namespace prefix",
			namespace: "labs",
			derived:   "labs-mctl-telegram-base-service",
			labels:    map[string]string{"app.kubernetes.io/instance": "labs-mctl-telegram"},
			want:      []string{"mctl-telegram", "labs-mctl-telegram", "labs-mctl-telegram-base-service"},
		},
		{
			name:      "no chart-fullname signature leaves a plain name unchanged",
			namespace: "default",
			derived:   "myapp",
			want:      []string{"myapp"},
		},
		{
			name:      "no chart-fullname signature leaves a two-segment name unchanged",
			namespace: "default",
			derived:   "two-parts",
			want:      []string{"two-parts"},
		},
		{
			name:      "no chart-fullname signature: the namespace-prefix gate does not fire on a bare StatefulSet name",
			namespace: "labs",
			derived:   "labs-something",
			want:      []string{"labs-something"},
		},
		{
			name:      "empty derived returns nil",
			namespace: "labs",
			derived:   "",
			want:      nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Candidates(tt.namespace, tt.derived, tt.labels)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Candidates(%q, %q, %v) = %v, want %v", tt.namespace, tt.derived, tt.labels, got, tt.want)
			}
		})
	}
}

func TestResolve(t *testing.T) {
	got := Resolve("labs", "labs-mctl-telegram-base-service", nil)
	if got != "mctl-telegram" {
		t.Errorf("Resolve(...) = %q, want %q", got, "mctl-telegram")
	}

	if got := Resolve("labs", "", nil); got != "" {
		t.Errorf("Resolve with empty derived = %q, want empty", got)
	}

	// A label hit takes priority over the deterministic derivation.
	got = Resolve("labs", "labs-mctl-telegram-base-service", map[string]string{
		"backstage.io/kubernetes-id": "mctl-telegram-canonical",
	})
	if got != "mctl-telegram-canonical" {
		t.Errorf("Resolve with identity label = %q, want the label value", got)
	}
}
