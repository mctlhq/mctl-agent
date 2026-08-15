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
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestTokenSource(t *testing.T) {
	t.Run("no file configured uses the env token", func(t *testing.T) {
		src := newTokenSource("  env-token  ", "")
		if got := src.token(); got != "env-token" {
			t.Fatalf("token() = %q, want %q", got, "env-token")
		}
	})

	t.Run("file wins over the env token", func(t *testing.T) {
		path := writeToken(t, "file-token\n")
		src := newTokenSource("env-token", path)
		if got := src.token(); got != "file-token" {
			t.Fatalf("token() = %q, want %q", got, "file-token")
		}
	})

	t.Run("picks up a rotated token without reconstruction", func(t *testing.T) {
		// The whole point: the same source must observe a new value written
		// after it was created, which an env-captured token never can.
		path := writeToken(t, "first")
		src := newTokenSource("env-token", path)
		if got := src.token(); got != "first" {
			t.Fatalf("token() = %q, want %q", got, "first")
		}

		if err := os.WriteFile(path, []byte("rotated"), 0o600); err != nil {
			t.Fatalf("rewrite token file: %v", err)
		}
		if got := src.token(); got != "rotated" {
			t.Fatalf("after rotation token() = %q, want %q", got, "rotated")
		}
	})

	t.Run("missing file keeps the last known-good token", func(t *testing.T) {
		path := writeToken(t, "good")
		src := newTokenSource("env-token", path)
		_ = src.token() // prime the cache

		if err := os.Remove(path); err != nil {
			t.Fatalf("remove token file: %v", err)
		}
		// Degrading to "" here would turn a mid-rotation race into a 401.
		if got := src.token(); got != "good" {
			t.Fatalf("token() = %q, want previous value %q", got, "good")
		}
	})

	t.Run("empty file keeps the last known-good token", func(t *testing.T) {
		path := writeToken(t, "good")
		src := newTokenSource("env-token", path)
		_ = src.token()

		if err := os.WriteFile(path, []byte("   \n"), 0o600); err != nil {
			t.Fatalf("truncate token file: %v", err)
		}
		if got := src.token(); got != "good" {
			t.Fatalf("token() = %q, want previous value %q", got, "good")
		}
	})

	t.Run("unreadable file falls back to the env token", func(t *testing.T) {
		src := newTokenSource("env-token", filepath.Join(t.TempDir(), "absent"))
		if got := src.token(); got != "env-token" {
			t.Fatalf("token() = %q, want %q", got, "env-token")
		}
	})

	t.Run("concurrent reads are race-free", func(t *testing.T) {
		path := writeToken(t, "concurrent")
		src := newTokenSource("env-token", path)

		var wg sync.WaitGroup
		for i := 0; i < 32; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if got := src.token(); got != "concurrent" {
					t.Errorf("token() = %q, want %q", got, "concurrent")
				}
			}()
		}
		wg.Wait()
	})
}

func TestAuthTransport(t *testing.T) {
	t.Run("sends the current token on every request", func(t *testing.T) {
		var got []string
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			got = append(got, r.Header.Get("Authorization"))
		}))
		defer srv.Close()

		path := writeToken(t, "first")
		client := newTestClient(t, newTokenSource("", path), srv.URL)

		doGet(t, client, srv.URL)
		if err := os.WriteFile(path, []byte("second"), 0o600); err != nil {
			t.Fatalf("rewrite token file: %v", err)
		}
		doGet(t, client, srv.URL)

		want := []string{"Bearer first", "Bearer second"}
		if len(got) != len(want) {
			t.Fatalf("got %d requests, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("request %d Authorization = %q, want %q", i, got[i], want[i])
			}
		}
	})

	t.Run("does not mutate the caller's request", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
		defer srv.Close()

		client := newTestClient(t, newTokenSource("tok", ""), srv.URL)
		req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if h := req.Header.Get("Authorization"); h != "" {
			t.Errorf("caller's request was mutated: Authorization = %q", h)
		}
	})

	t.Run("survives a request with a nil Header", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
		defer srv.Close()

		tr := &authTransport{
			base:  http.DefaultTransport,
			src:   newTokenSource("tok", ""),
			hosts: hostsOf(t, srv.URL),
		}
		req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header = nil // http.NewRequest always sets one; a hand-built request need not

		resp, err := tr.RoundTrip(req) // must not panic
		if err != nil {
			t.Fatalf("round trip: %v", err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Fatalf("close body: %v", err)
		}
	})

	t.Run("omits the header when no token is available", func(t *testing.T) {
		var seen string
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			seen = r.Header.Get("Authorization")
		}))
		defer srv.Close()

		client := newTestClient(t, newTokenSource("", ""), srv.URL)
		doGet(t, client, srv.URL)

		if seen != "" {
			t.Errorf("Authorization = %q, want empty", seen)
		}
	})

	t.Run("never sends the token to a non-GitHub host", func(t *testing.T) {
		var seen string
		other := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			seen = r.Header.Get("Authorization")
		}))
		defer other.Close()

		// Only the "GitHub" server is allow-listed; the other host is not.
		gh := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
		defer gh.Close()

		client := newTestClient(t, newTokenSource("super-secret", ""), gh.URL)
		doGet(t, client, other.URL)

		if seen != "" {
			t.Errorf("token leaked to a non-GitHub host: Authorization = %q", seen)
		}
	})

	t.Run("does not re-attach the token across a cross-host redirect", func(t *testing.T) {
		// Go's http.Client strips Authorization when a redirect changes host.
		// Because this RoundTripper wraps the whole transport it also runs on
		// the redirected request, so without host scoping it would put the
		// header straight back and leak the token to the redirect target.
		var seen string
		attacker := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			seen = r.Header.Get("Authorization")
		}))
		defer attacker.Close()

		gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, attacker.URL, http.StatusFound)
		}))
		defer gh.Close()

		client := newTestClient(t, newTokenSource("super-secret", ""), gh.URL)
		doGet(t, client, gh.URL)

		if seen != "" {
			t.Errorf("token leaked across redirect: Authorization = %q", seen)
		}
	})
}

// newTestClient builds a client whose transport treats the given URLs' hosts as
// the GitHub API hosts, since httptest servers live on 127.0.0.1.
func newTestClient(t *testing.T, src *tokenSource, allowedURLs ...string) *http.Client {
	t.Helper()
	return &http.Client{
		Transport: &authTransport{
			base:  http.DefaultTransport,
			src:   src,
			hosts: hostsOf(t, allowedURLs...),
		},
	}
}

func hostsOf(t *testing.T, rawURLs ...string) map[string]bool {
	t.Helper()
	hosts := make(map[string]bool, len(rawURLs))
	for _, raw := range rawURLs {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %s: %v", raw, err)
		}
		hosts[u.Host] = true
	}
	return hosts
}

func writeToken(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	return path
}

func doGet(t *testing.T, client *http.Client, url string) {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close body: %v", err)
	}
}
