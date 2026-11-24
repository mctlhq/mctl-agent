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
	"github.com/mctlhq/mctl-agent/internal/gitopspath"
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
		{
			name: "global.tag above image.tag is ignored",
			content: `global:
  tag: "ignored-global"
image:
  repository: foo
  tag: "real-chart"`,
			want:   "real-chart",
			wantOK: true,
		},
		{
			name: "tag outside image block (no image: declared)",
			content: `global:
  tag: "1.0.0"
service:
  name: foo`,
			wantOK: false,
		},
		{
			name: "trailing inline comment",
			content: `image:
  repository: ghcr.io/mctlhq/mctl-openclaw
  tag: "2026.4.29-beta.2"  # managed by gitops`,
			want:   "2026.4.29-beta.2",
			wantOK: true,
		},
		{
			name: "trailing inline comment unquoted",
			content: `image:
  repository: foo
  tag: 1.2.3 # bumped 2026-04-30`,
			want:   "1.2.3",
			wantOK: true,
		},
		{
			name: "nested map under image is skipped, sibling tag wins",
			content: `image:
  pullPolicy: IfNotPresent
  pullSecrets:
    - name: ghcr
      tag: "ignored-nested"
  repository: foo
  tag: "real-chart"`,
			want:   "real-chart",
			wantOK: true,
		},
		{
			name: "sub-image declared before chart image — chart wins on min-indent",
			content: `sidecar:
  image:
    repository: ghcr.io/foo/sidecar
    tag: "ignored-sub"
image:
  repository: ghcr.io/mctlhq/mctl-openclaw
  tag: "real-chart"`,
			want:   "real-chart",
			wantOK: true,
		},
		{
			name: "image: line with trailing inline comment",
			content: `image: # primary chart image
  repository: foo
  tag: "1.2.3"`,
			want:   "1.2.3",
			wantOK: true,
		},
		{
			name: "indented image: block (helm.values inline-string shape)",
			content: `apiVersion: argoproj.io/v1alpha1
kind: Application
spec:
  source:
    helm:
      values: |
        image:
          repository: ghcr.io/mctlhq/mctl-agent
          tag: "1.6.0"
        service:
          port: 8080`,
			want:   "1.6.0",
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
		client:    client,
		owner:     "mctlhq",
		repo:      "mctl-gitops",
		allowlist: gitopspath.DefaultAllowlist(),
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

	t.Run("404 at one revision is skipped, walk continues", func(t *testing.T) {
		f, cleanup := fakeGH(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/commits"):
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode([]map[string]any{
					{"sha": "head"},
					{"sha": "missing"},
					{"sha": "pre-bump"},
				})
			case strings.HasPrefix(r.URL.Path, "/repos/mctlhq/mctl-gitops/contents/"):
				ref := r.URL.Query().Get("ref")
				if ref == "missing" {
					http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
					return
				}
				body := current
				if ref == "pre-bump" {
					body = prev
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"name":     "values.yaml",
					"sha":      ref + "-blob",
					"content":  encodeContent(body),
					"encoding": "base64",
				})
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

	t.Run("non-404 error propagates", func(t *testing.T) {
		f, cleanup := fakeGH(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/commits"):
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode([]map[string]any{
					{"sha": "head"},
				})
			case strings.HasPrefix(r.URL.Path, "/repos/mctlhq/mctl-gitops/contents/"):
				http.Error(w, `{"message":"server overloaded"}`, http.StatusServiceUnavailable)
			}
		})
		defer cleanup()

		_, err := f.LookupPreviousImageTag(context.Background(), path, "anything")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "reading") {
			t.Errorf("error should mention what failed; got %q", err.Error())
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

// The previous image is the tag and the digest of one and the same revision.
func TestLookupPreviousImageReturnsDigestOfThatRevision(t *testing.T) {
	const path = "platform-gitops/services/labs/mctl-telegram/values.yaml"
	file := func(tag, digest string) string {
		return "image:\n  repository: ghcr.io/mctlhq/mctl-telegram\n  tag: \"" + tag + "\"\n  digest: \"" + digest + "\"\n"
	}
	f, cleanup := fakeGH(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/commits") {
			_ = json.NewEncoder(w).Encode([]map[string]any{{"sha": "head"}, {"sha": "pre-bump"}})
			return
		}
		body := file("0.148.0", digestNew)
		if r.URL.Query().Get("ref") == "pre-bump" {
			body = file("0.147.0", digestOld)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "file", "encoding": "base64", "sha": "b", "content": encodeContent(body)})
	})
	defer cleanup()

	got, err := f.LookupPreviousImage(context.Background(), path, "0.148.0")
	if err != nil {
		t.Fatal(err)
	}
	if want := (ImageRef{Tag: "0.147.0", Digest: digestOld}); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// ReadImageRef agrees with the line scanner where the scanner is right and
// refuses where it is not: an image block the scanner cannot see is "not
// understood", never "no tag".
func TestReadImageRef(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    ImageRef
		wantOK  bool
		wantErr string
	}{
		{"block style", "image:\n  repository: r\n  tag: \"1.2.3\"\n", ImageRef{Tag: "1.2.3"}, true, ""},
		{"unquoted tag that looks like a number", "image:\n  tag: 1.20\n", ImageRef{Tag: "1.20"}, true, ""},
		{"tag and digest", "image:\n  tag: 1.2.3\n  digest: \"" + digestOld + "\"\n", ImageRef{Tag: "1.2.3", Digest: digestOld}, true, ""},
		{"no image block", "service:\n  port: 8080\n", ImageRef{}, false, ""},
		{"image block without a tag", "image:\n  repository: r\n", ImageRef{}, false, ""},
		{"empty tag", "image:\n  tag: \"\"\n", ImageRef{}, false, ""},
		{"empty file", "", ImageRef{}, false, ""},
		{"flow style", "image: {repository: r, tag: main-new}\n", ImageRef{}, false, "not understood"},
		{"quoted tag key", "image:\n  \"tag\": main-new\n", ImageRef{}, false, "not understood"},
		{"anchored tag", "image:\n  tag: &t main-new\n", ImageRef{}, false, "not understood"},
		{"folded scalar tag", "image:\n  tag: >-\n    main-new\n", ImageRef{}, false, "not understood"},
		{"tag that is a mapping", "image:\n  tag:\n    value: main-new\n", ImageRef{}, false, "not a scalar"},
		{"only a nested image carries a tag", "sidecar:\n  image:\n    tag: 9.9.9\n", ImageRef{}, false, "not understood"},
		{"not YAML", "image:\n  tag: \"1.2.3\n", ImageRef{}, false, "not valid YAML"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok, err := ReadImageRef(tt.content)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q (ref %+v, ok %v)", err, tt.wantErr, got, ok)
				}
				if ok || got != (ImageRef{}) {
					t.Fatalf("a failed read returned %+v, ok %v", got, ok)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want || ok != tt.wantOK {
				t.Fatalf("got %+v, ok %v; want %+v, ok %v", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

// revisionsAPI serves a commit listing, newest first, and the values file of
// each listed revision. A revision whose content is "!503" answers 503.
func revisionsAPI(t *testing.T, shas []string, files map[string]string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/commits") {
			rows := make([]map[string]any, 0, len(shas))
			for _, sha := range shas {
				row := map[string]any{"commit": map[string]any{"message": "x"}}
				if sha != "" {
					row["sha"] = sha
				}
				rows = append(rows, row)
			}
			_ = json.NewEncoder(w).Encode(rows)
			return
		}
		body, ok := files[r.URL.Query().Get("ref")]
		switch {
		case !ok:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		case body == "!503":
			http.Error(w, `{"message":"server overloaded"}`, http.StatusServiceUnavailable)
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"type": "file", "encoding": "base64", "sha": "b", "content": encodeContent(body)})
		}
	}
}

// LookupImageByTag returns the revision that carries exactly the wanted tag,
// however many other images lie between it and the head, with that
// revision's digest.
func TestLookupImageByTag(t *testing.T) {
	const path = "platform-gitops/services/labs/mctl-telegram/values.yaml"
	const digestMid = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
	file := func(tag, digest string) string {
		return "image:\n  repository: ghcr.io/mctlhq/mctl-telegram\n  tag: \"" + tag + "\"\n  digest: \"" + digest + "\"\n"
	}
	flow := "image: {repository: ghcr.io/mctlhq/mctl-telegram, tag: 0.147.5}\n"

	tests := []struct {
		name    string
		shas    []string
		files   map[string]string
		tag     string
		want    ImageRef
		wantErr string
	}{
		{
			name:  "the wanted tag, past a nearer distinct one",
			shas:  []string{"c3", "c2", "c1"},
			files: map[string]string{"c3": file("0.149.0", digestNew), "c2": file("0.148.0", digestMid), "c1": file("0.147.0", digestOld)},
			tag:   "0.147.0",
			want:  ImageRef{Tag: "0.147.0", Digest: digestOld},
		},
		{
			name:  "a tag no revision carries is not found",
			shas:  []string{"c3", "c2"},
			files: map[string]string{"c3": file("0.149.0", digestNew), "c2": file("0.148.0", digestMid)},
			tag:   "0.147.0",
		},
		{
			name:  "a revision where the file did not exist is passed over",
			shas:  []string{"c3", "gone", "c1"},
			files: map[string]string{"c3": file("0.149.0", digestNew), "c1": file("0.147.0", digestOld)},
			tag:   "0.147.0",
			want:  ImageRef{Tag: "0.147.0", Digest: digestOld},
		},
		{
			name:    "an empty tag is refused",
			shas:    []string{"c3"},
			files:   map[string]string{"c3": file("0.149.0", digestNew)},
			wantErr: "no image tag to look up",
		},
		{
			name:    "an unreadable revision stops the walk",
			shas:    []string{"c3", "c2", "c1"},
			files:   map[string]string{"c3": file("0.149.0", digestNew), "c2": "!503", "c1": file("0.147.0", digestOld)},
			tag:     "0.147.0",
			wantErr: "at c2",
		},
		{
			name:    "a revision that is not understood stops the walk",
			shas:    []string{"c3", "c2", "c1"},
			files:   map[string]string{"c3": file("0.149.0", digestNew), "c2": flow, "c1": file("0.147.0", digestOld)},
			tag:     "0.147.0",
			wantErr: "image.tag not understood",
		},
		{
			name:    "a commit without a SHA stops the walk",
			shas:    []string{"c3", "", "c1"},
			files:   map[string]string{"c3": file("0.149.0", digestNew), "c1": file("0.147.0", digestOld)},
			tag:     "0.147.0",
			wantErr: "without a SHA",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, cleanup := fakeGH(t, revisionsAPI(t, tt.shas, tt.files))
			defer cleanup()

			got, err := f.LookupImageByTag(context.Background(), path, tt.tag)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q (got %+v)", err, tt.wantErr, got)
				}
				if got != (ImageRef{}) {
					t.Fatalf("a failed lookup returned %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

// LookupPreviousImage must not step over a revision it did not understand:
// the image there may be the real previous one, and the next older tag would
// be returned in its place.
func TestLookupPreviousImageStopsAtARevisionNotUnderstood(t *testing.T) {
	const path = "platform-gitops/services/labs/mctl-telegram/values.yaml"
	files := map[string]string{
		"head": "image:\n  tag: \"0.149.0\"\n",
		"mid":  "image: {repository: ghcr.io/mctlhq/mctl-telegram, tag: 0.148.0}\n",
		"old":  "image:\n  tag: \"0.147.0\"\n",
	}
	f, cleanup := fakeGH(t, revisionsAPI(t, []string{"head", "mid", "old"}, files))
	defer cleanup()

	got, err := f.LookupPreviousImage(context.Background(), path, "0.149.0")
	if err == nil || !strings.Contains(err.Error(), "at mid") {
		t.Fatalf("got %+v, error = %v; want the walk to stop at mid", got, err)
	}
}
