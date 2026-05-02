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

// tagLinePattern matches a `tag:` line, optionally with surrounding
// quotes and a trailing inline comment. Three alternatives in the value
// alternation: double-quoted, single-quoted, unquoted-non-whitespace.
// Group 5 (if present) captures the inline comment (`# ...`) including
// the whitespace before `#`, so a rewrite can re-attach it intact.
//
// Used only after we've located the chart-level `image:` block via
// chartImageTagLineIndex — this regex on its own does NOT identify
// the chart image.
var tagLinePattern = regexp.MustCompile(`^(\s*tag:\s*)(?:"([^"]*)"|'([^']*)'|([^\s"'#][^\s#]*))(\s*#.*)?\s*$`)

// parseTagLine returns the tag value and any inline comment (with leading
// whitespace + `#`) for a line that matches tagLinePattern.
func parseTagLine(line string) (value, prefix, comment string, ok bool) {
	m := tagLinePattern.FindStringSubmatch(line)
	if len(m) == 0 {
		return "", "", "", false
	}
	prefix = m[1]
	comment = m[5]
	for _, g := range m[2:5] {
		if g != "" {
			return g, prefix, comment, true
		}
	}
	return "", "", "", false
}

// indentOf returns the leading-whitespace span of line.
func indentOf(line string) string {
	return line[:len(line)-len(strings.TrimLeft(line, " \t"))]
}

// chartImageTagLineIndex returns the index of the chart-level
// `image.tag:` line, or -1 when the file has no such declaration.
//
// "Chart-level" is anchored on two structural rules:
//  1. The owning `image:` key sits at column 0 (top-level Helm values).
//  2. The matching `tag:` line is a *direct child* of that block — its
//     leading indent equals the indent of the FIRST non-blank child of
//     the `image:` block. Lines indented deeper belong to nested maps
//     (sub-objects under sibling keys of `tag:`) and do NOT count.
//
// Lines that sit at top-level (column 0) other than `image:` close the
// block. Comments and blanks are transparent.
func chartImageTagLineIndex(lines []string) int {
	inImageBlock := false
	childIndent := ""
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		// Column-0 key.
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			inImageBlock = trimmed == "image:"
			childIndent = ""
			continue
		}
		if !inImageBlock {
			continue
		}
		ind := indentOf(line)
		if childIndent == "" {
			childIndent = ind
		}
		// Strict equality — deeper indent means we're inside a sub-map
		// (e.g. `image:\n  someField:\n    tag: nested`), not a direct
		// sibling of the chart's repository/tag.
		if ind != childIndent {
			continue
		}
		if _, _, _, ok := parseTagLine(line); ok {
			return i
		}
	}
	return -1
}

// ExtractImageTag returns the chart-level `image.tag` value.
// Returns ok=false when the file has no top-level `image:` block or
// when that block does not declare a direct `tag:` child.
func ExtractImageTag(content string) (string, bool) {
	lines := strings.Split(content, "\n")
	idx := chartImageTagLineIndex(lines)
	if idx < 0 {
		return "", false
	}
	val, _, _, ok := parseTagLine(lines[idx])
	if !ok {
		return "", false
	}
	return val, true
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
