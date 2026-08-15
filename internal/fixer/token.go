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
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
)

// tokenSource resolves the GitHub token to use for each API request.
//
// GitHub App installation tokens expire after 60 minutes. The platform mints a
// fresh one every 30 minutes into Vault and ExternalSecrets syncs it into the
// Kubernetes Secret, but a value read out of the environment at process start
// is frozen for the lifetime of the process — environment variables of a
// running container are never updated. A long-running agent is therefore
// authenticated for at most one hour after it boots and then fails every call
// with 401 until something restarts it.
//
// Reading the mounted Secret file per request is what makes rotation actually
// land: kubelet keeps a projected Secret volume current. GITHUB_TOKEN_FILE
// unset preserves the old behaviour (use the env value as-is), so this is safe
// to deploy before the volume mount exists.
type tokenSource struct {
	path string

	mu     sync.RWMutex
	cached string
}

// newTokenSource seeds the cache with the environment token, which is also the
// value used verbatim when path is empty.
func newTokenSource(envToken, path string) *tokenSource {
	return &tokenSource{
		path:   strings.TrimSpace(path),
		cached: strings.TrimSpace(envToken),
	}
}

// token returns the freshest token available, falling back to the last
// known-good value.
//
// It deliberately never downgrades to an empty token once one is known: a
// Secret volume update is not atomic from the reader's side, so a read landing
// mid-rotation can observe a missing or truncated file. Treating that as "no
// credential" would turn a sub-second race into an auth failure, which is
// exactly the class of bug this type exists to remove.
func (s *tokenSource) token() string {
	if s.path == "" {
		return s.current()
	}

	raw, err := os.ReadFile(s.path)
	if err != nil {
		slog.Warn("github token file unreadable, keeping previous token",
			"path", s.path, "error", err)
		return s.current()
	}

	fresh := strings.TrimSpace(string(raw))
	if fresh == "" {
		slog.Warn("github token file is empty, keeping previous token", "path", s.path)
		return s.current()
	}

	s.mu.Lock()
	s.cached = fresh
	s.mu.Unlock()
	return fresh
}

func (s *tokenSource) current() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cached
}

// authTransport injects the current token into every outgoing request.
//
// This replaces github.NewClient(nil).WithAuthToken(token), which binds one
// token at client construction and can never see a rotated value.
type authTransport struct {
	base http.RoundTripper
	src  *tokenSource
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}

	token := t.src.token()
	if token == "" {
		return base.RoundTrip(req)
	}

	// RoundTripper must not modify the request it is given.
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+token)
	return base.RoundTrip(clone)
}
