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
	"fmt"
	"regexp"
	"strings"

	"github.com/google/go-github/v68/github"
)

// previousTagLookupCommitCap bounds how far back we walk a values file's
// history searching for a prior distinct image tag. Beyond this, we stop
// rather than chase a wild rollback target.
const previousTagLookupCommitCap = 20

// imageTagPattern matches the chart-level `image.tag:` declaration.
//
// The first `tag:` line directly under the top-level `image:` block is the
// chart image. Sub-image declarations (initContainers, sidecars) may also
// have `tag:` keys but are not the rollback target — they live in nested
// blocks deeper in the file. Matching the first occurrence keeps us
// aligned with GenerateImageRollback (which also rewrites the first
// `tag:` line it sees).
var imageTagPattern = regexp.MustCompile(`(?m)^\s*tag:\s*"?([^"\s]+)"?\s*$`)

// ExtractImageTag returns the first `tag:` value found in content.
// Returns ok=false when no tag line is present.
func ExtractImageTag(content string) (string, bool) {
	m := imageTagPattern.FindStringSubmatch(content)
	if len(m) < 2 {
		return "", false
	}
	return strings.TrimSpace(m[1]), true
}

// LookupPreviousImageTag walks the GitHub history of valuesPath and returns
// the most recent prior distinct value of `image.tag`.
//
// currentTag is the tag we want to roll back FROM — commits whose file
// content carries the same tag are skipped (they predate the bad bump too).
// Returns ("", nil) when no prior distinct tag exists in the cap window;
// the caller should degrade to diagnosis-only Telegram in that case.
func (f *GitHubFixer) LookupPreviousImageTag(ctx context.Context, valuesPath, currentTag string) (string, error) {
	commits, _, err := f.client.Repositories.ListCommits(ctx, f.owner, f.repo, &github.CommitsListOptions{
		Path: valuesPath,
		ListOptions: github.ListOptions{
			PerPage: previousTagLookupCommitCap,
		},
	})
	if err != nil {
		return "", fmt.Errorf("listing commits for %s: %w", valuesPath, err)
	}

	for _, c := range commits {
		sha := c.GetSHA()
		if sha == "" {
			continue
		}
		content, err := f.GetFileContent(ctx, valuesPath, sha)
		if err != nil {
			// File didn't exist at this revision (e.g. service was
			// onboarded after this commit). Skip.
			continue
		}
		tag, ok := ExtractImageTag(content)
		if !ok || tag == "" {
			continue
		}
		if tag != currentTag {
			return tag, nil
		}
	}

	return "", nil
}
