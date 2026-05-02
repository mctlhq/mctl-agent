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
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/go-github/v68/github"
)

// PreviousTagLookupCommitCap bounds how far back we walk a values file's
// history searching for a prior distinct image tag. Beyond this, we stop
// rather than chase a wild rollback target.
const PreviousTagLookupCommitCap = 20

// tagLinePattern matches a `tag:` line and captures the value with quotes
// stripped. Used only after we've located the chart-level `image:` block —
// it does NOT identify the chart image on its own.
var tagLinePattern = regexp.MustCompile(`^\s*tag:\s*["']?([^"'\s]+)["']?\s*$`)

// chartImageTagLineIndex returns the index of the `tag:` line directly
// under the top-level `image:` block, or -1 if not found.
//
// "Directly under" means: scanning forward from a column-0 `image:` key
// until either a `tag:` line is hit or another column-0 key appears. We
// deliberately ignore `tag:` keys that sit under siblings like
// `global:` / `sidecar:` / `extraContainers:` — only the chart-level
// image tag is the rollback target.
//
// Comment lines and blank lines inside the block are skipped.
func chartImageTagLineIndex(lines []string) int {
	inImageBlock := false
	for i, line := range lines {
		// Treat blank / comment lines as transparent — they don't change
		// block context.
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		// A line at column 0 is a top-level key. Entering "image:" opens
		// the block; entering anything else closes it.
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			inImageBlock = trimmed == "image:"
			continue
		}
		// Indented line. Only relevant while we're inside `image:`.
		if !inImageBlock {
			continue
		}
		if tagLinePattern.MatchString(line) {
			return i
		}
	}
	return -1
}

// ExtractImageTag returns the chart-level `image.tag` value.
// Returns ok=false when the file has no top-level `image:` block or
// when that block does not declare a `tag:`.
func ExtractImageTag(content string) (string, bool) {
	lines := strings.Split(content, "\n")
	idx := chartImageTagLineIndex(lines)
	if idx < 0 {
		return "", false
	}
	m := tagLinePattern.FindStringSubmatch(lines[idx])
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
			PerPage: PreviousTagLookupCommitCap,
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
			if ctx.Err() != nil {
				// Context was cancelled or timed out — stop immediately.
				return "", ctx.Err()
			}
			// 404 = file didn't exist at this revision (e.g. service
			// was onboarded after this commit). Skip and keep walking.
			// Anything else (rate limit, 5xx, auth) means we can't
			// trust the rest of the walk — propagate so the caller
			// degrades to diagnosis-only Telegram instead of silently
			// returning "no previous tag found".
			var ghErr *github.ErrorResponse
			if errors.As(err, &ghErr) && ghErr.Response != nil && ghErr.Response.StatusCode == 404 {
				continue
			}
			return "", fmt.Errorf("reading %s at %s: %w", valuesPath, sha, err)
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
