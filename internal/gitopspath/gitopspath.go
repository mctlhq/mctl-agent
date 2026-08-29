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

// Package gitopspath validates that a candidate GitOps file path is
// confined to an explicit allowlist of path prefixes before mctl-agent is
// allowed to read or write it in mctl-gitops.
//
// This package has no dependency on internal/fixer or the GitHub client so
// it can be unit tested with plain strings.
package gitopspath

import (
	"fmt"
	"path"
	"strings"
)

// DefaultPrefixes are the path prefixes mctl-agent already writes to today:
// tenant/service values files, platform-service inline values, and the
// Argo Workflow templates patched by the workflow-related builtin skills.
var DefaultPrefixes = []string{
	"platform-gitops/services/",
	"platform-gitops/apps/templates/",
	"platform-gitops/argo-workflows/workflow-templates/",
}

// Allowlist holds the set of path prefixes GitOps reads/writes may target.
type Allowlist struct {
	prefixes []string
}

// NewAllowlist builds an Allowlist from the given prefixes. Each prefix is
// normalized to end with exactly one "/" so a candidate path must have at
// least one path segment under the prefix directory to match — the prefix
// directory itself is never treated as a valid file target.
func NewAllowlist(prefixes []string) Allowlist {
	cp := make([]string, len(prefixes))
	for i, p := range prefixes {
		cp[i] = strings.TrimSuffix(p, "/") + "/"
	}
	return Allowlist{prefixes: cp}
}

// DefaultAllowlist returns an Allowlist built from DefaultPrefixes.
func DefaultAllowlist() Allowlist {
	return NewAllowlist(DefaultPrefixes)
}

// Validate cleans path and checks that it resolves under one of the
// allowlisted prefixes with no directory traversal. Returns a descriptive
// error when the path is rejected.
func (a Allowlist) Validate(candidate string) error {
	if candidate == "" {
		return fmt.Errorf("gitops path is empty")
	}
	if strings.Contains(candidate, "..") {
		return fmt.Errorf("gitops path %q contains a traversal segment", candidate)
	}
	if path.IsAbs(candidate) || strings.HasPrefix(candidate, "/") {
		return fmt.Errorf("gitops path %q is absolute", candidate)
	}

	cleaned := path.Clean(candidate)
	// The ".."/"../" arms are unreachable today (any ".." substring is
	// already rejected above) — kept deliberately as defense in depth so
	// this line stays correct even if the substring check above is ever
	// relaxed to allow ".." inside a filename.
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || path.IsAbs(cleaned) {
		return fmt.Errorf("gitops path %q resolves outside the repository", candidate)
	}

	for _, p := range a.prefixes {
		if strings.HasPrefix(cleaned, p) {
			return nil
		}
	}
	return fmt.Errorf("gitops path %q is not under an allowed prefix", candidate)
}
