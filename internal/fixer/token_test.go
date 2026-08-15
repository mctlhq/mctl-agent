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
		client := &http.Client{
			Transport: &authTransport{base: http.DefaultTransport, src: newTokenSource("", path)},
		}

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

		client := &http.Client{
			Transport: &authTransport{base: http.DefaultTransport, src: newTokenSource("tok", "")},
		}
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

	t.Run("omits the header when no token is available", func(t *testing.T) {
		var seen string
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			seen = r.Header.Get("Authorization")
		}))
		defer srv.Close()

		client := &http.Client{
			Transport: &authTransport{base: http.DefaultTransport, src: newTokenSource("", "")},
		}
		doGet(t, client, srv.URL)

		if seen != "" {
			t.Errorf("Authorization = %q, want empty", seen)
		}
	})
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
