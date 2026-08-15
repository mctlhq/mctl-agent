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

// defaultGitHubHosts are the only hosts the token is ever sent to.
//
// Matched against url.URL.Host, i.e. including a port when one is explicit.
// go-github targets https://api.github.com/... so the port is implicit and Host
// is the bare name; anything carrying an unexpected port is not GitHub and does
// not get the credential.
var defaultGitHubHosts = map[string]bool{
	"api.github.com":     true,
	"uploads.github.com": true,
}

// authTransport injects the current token into every outgoing request.
//
// This replaces github.NewClient(nil).WithAuthToken(token), which binds one
// token at client construction and can never see a rotated value.
type authTransport struct {
	base http.RoundTripper
	src  *tokenSource

	// hosts overrides defaultGitHubHosts; only tests set it.
	hosts map[string]bool
}

func (t *authTransport) allowed(host string) bool {
	hosts := t.hosts
	if hosts == nil {
		hosts = defaultGitHubHosts
	}
	return hosts[host]
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}

	// Scope the credential to GitHub's own API hosts.
	//
	// This RoundTripper wraps the whole transport, so it runs again on the
	// request http.Client builds when following a redirect. http.Client strips
	// Authorization when a redirect changes host (Go 1.8+) exactly to stop
	// credential leaks; re-attaching it unconditionally here would undo that
	// protection and hand a highly privileged GitHub App token to whatever host
	// the redirect pointed at.
	if !t.allowed(req.URL.Host) {
		return base.RoundTrip(req)
	}

	token := t.src.token()
	if token == "" {
		return base.RoundTrip(req)
	}

	// RoundTripper must not modify the request it is given.
	clone := req.Clone(req.Context())
	// req.Clone copies a nil Header as nil, and Set on a nil map panics.
	// go-github always initialises it, but the assumption costs nothing to drop.
	if clone.Header == nil {
		clone.Header = make(http.Header)
	}
	clone.Header.Set("Authorization", "Bearer "+token)
	return base.RoundTrip(clone)
}
