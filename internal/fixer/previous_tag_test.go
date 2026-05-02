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

package fixer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/go-github/v68/github"
)

func TestExtractImageTag(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
		wantOK  bool
	}{
		{
			name: "quoted tag",
			content: `image:
  repository: ghcr.io/mctlhq/mctl-openclaw
  tag: "2026.4.29-beta.2"`,
			want:   "2026.4.29-beta.2",
			wantOK: true,
		},
		{
			name: "unquoted tag",
			content: `image:
  repository: foo
  tag: 1.2.3`,
			want:   "1.2.3",
			wantOK: true,
		},
		{
			name: "first tag wins (chart-level over sub-image)",
			content: `image:
  repository: foo
  tag: "1.0.0"
extra:
  - name: sidecar
    image:
      tag: "9.9.9"`,
			want:   "1.0.0",
			wantOK: true,
		},
		{
			name:    "no tag",
			content: `image:\n  repository: foo`,
			wantOK:  false,
		},
		{
			name: "single-quoted tag",
			content: `image:
  repository: foo
  tag: '1.2.3'`,
			want:   "1.2.3",
			wantOK: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ExtractImageTag(tt.content)
			if ok != tt.wantOK {
				t.Fatalf("ok=%v, want %v", ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// fakeGH wires a GitHubFixer to an httptest server so tests don't hit GitHub.
func fakeGH(t *testing.T, h http.HandlerFunc) (*GitHubFixer, func()) {
	t.Helper()
	srv := httptest.NewServer(h)
	client := github.NewClient(nil)
	base, err := url.Parse(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	client.BaseURL = base
	client.UploadURL = base
	return &GitHubFixer{
		client: client,
		owner:  "mctlhq",
		repo:   "mctl-gitops",
	}, srv.Close
}

func encodeContent(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func TestLookupPreviousImageTag(t *testing.T) {
	const path = "platform-gitops/services/admins/openclaw/values.yaml"
	current := `image:
  repository: ghcr.io/mctlhq/mctl-openclaw
  tag: "2026.4.29-beta.2"`
	prev := `image:
  repository: ghcr.io/mctlhq/mctl-openclaw
  tag: "2026.4.29-beta.1"`

	t.Run("returns prior distinct tag", func(t *testing.T) {
		f, cleanup := fakeGH(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/commits"):
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode([]map[string]any{
					{"sha": "head"},
					{"sha": "pre-bump"},
				})
			case strings.HasPrefix(r.URL.Path, "/repos/mctlhq/mctl-gitops/contents/"):
				ref := r.URL.Query().Get("ref")
				body := current
				if ref == "pre-bump" {
					body = prev
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"name":     "values.yaml",
					"path":     path,
					"sha":      ref + "-blob",
					"content":  encodeContent(body),
					"encoding": "base64",
				})
			default:
				t.Fatalf("unexpected GET %s", r.URL.Path)
			}
		})
		defer cleanup()

		got, err := f.LookupPreviousImageTag(context.Background(), path, "2026.4.29-beta.2")
		if err != nil {
			t.Fatal(err)
		}
		if got != "2026.4.29-beta.1" {
			t.Errorf("got %q, want %q", got, "2026.4.29-beta.1")
		}
	})

	t.Run("no prior distinct tag returns empty", func(t *testing.T) {
		f, cleanup := fakeGH(t, func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/commits") {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode([]map[string]any{
					{"sha": "head"},
					{"sha": "older"},
				})
				return
			}
			// Both revisions hold the same tag.
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name":     "values.yaml",
				"sha":      "blob",
				"content":  encodeContent(current),
				"encoding": "base64",
			})
		})
		defer cleanup()

		got, err := f.LookupPreviousImageTag(context.Background(), path, "2026.4.29-beta.2")
		if err != nil {
			t.Fatal(err)
		}
		if got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}
