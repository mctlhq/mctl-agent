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

package remote

import (
	"context"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestValidateRegistration covers the accept/reject matrix from the
// proposal's acceptance criteria. Every case uses a literal IP host (never
// a hostname) so the test never performs live DNS resolution.
func TestValidateRegistration(t *testing.T) {
	tests := []struct {
		name    string
		reg     Registration
		wantErr string // substring expected in the error, "" means no error
	}{
		{
			name:    "http scheme rejected",
			reg:     Registration{Endpoint: "http://8.8.8.8/"},
			wantErr: "https",
		},
		{
			name:    "link-local / cloud metadata rejected",
			reg:     Registration{Endpoint: "https://169.254.169.254/"},
			wantErr: "disallowed",
		},
		{
			name:    "private RFC1918 rejected",
			reg:     Registration{Endpoint: "https://10.1.2.3/"},
			wantErr: "disallowed",
		},
		{
			name:    "loopback v6 rejected",
			reg:     Registration{Endpoint: "https://[::1]/"},
			wantErr: "disallowed",
		},
		{
			name:    "CGNAT rejected",
			reg:     Registration{Endpoint: "https://100.64.1.1/"},
			wantErr: "disallowed",
		},
		{
			name: "unknown capability rejected",
			reg: Registration{
				Endpoint:     "https://8.8.8.8/",
				Capabilities: []string{"delete_everything"},
			},
			wantErr: "unknown capability",
		},
		{
			name: "valid public endpoint with known capabilities accepted",
			reg: Registration{
				Endpoint:     "https://8.8.8.8/",
				Capabilities: []string{"read_logs", "modify_gitops"},
			},
			wantErr: "",
		},
		{
			name:    "missing host rejected",
			reg:     Registration{Endpoint: "https://"},
			wantErr: "host",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateRegistration(tt.reg)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestIsDeniedIP(t *testing.T) {
	tests := []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", true},
		{"::1", true},
		{"10.0.0.1", true},
		{"172.16.0.1", true},
		{"192.168.1.1", true},
		{"169.254.1.1", true},
		{"fe80::1", true},
		{"100.64.1.1", true},
		{"100.63.255.255", false}, // just below the CGNAT block
		{"100.128.0.1", false},    // just above the CGNAT block
		{"8.8.8.8", false},
		{"1.1.1.1", false},
	}
	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			ip := net.ParseIP(tt.ip)
			if ip == nil {
				t.Fatalf("failed to parse %q as an IP", tt.ip)
			}
			if got := isDeniedIP(ip); got != tt.want {
				t.Errorf("isDeniedIP(%q) = %v, want %v", tt.ip, got, tt.want)
			}
		})
	}
}

// TestGuardedDialContextRefusesLoopback proves the connection-time guard
// blocks a dial to a loopback-bound httptest server even when
// ValidateRegistration is bypassed entirely (constructing the Skill
// directly), so registration-time and connection-time checks share one
// source of truth (isDeniedIP) rather than drifting apart.
func TestGuardedDialContextRefusesLoopback(t *testing.T) {
	srv := httptest.NewServer(nil)
	defer srv.Close()

	s := New(Registration{Name: "bypasses-validation", Endpoint: srv.URL})

	var resp matchResponse
	err := s.post(context.Background(), "/match", map[string]string{}, &resp)
	if err == nil {
		t.Fatal("expected dial to loopback server to be refused, got nil error")
	}
	if !strings.Contains(err.Error(), "disallowed") {
		t.Errorf("error = %q, want it to mention the dial was disallowed", err.Error())
	}
}
