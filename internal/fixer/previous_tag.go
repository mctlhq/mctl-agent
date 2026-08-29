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

// isImageBlockKey reports whether trimmed is `image:` opening a mapping
// block — value-less, with an optional trailing inline comment.
// Examples:  `image:` / `image: # primary app image` → true
//
//	`image: foo` / `images:` → false
func isImageBlockKey(trimmed string) bool {
	if !strings.HasPrefix(trimmed, "image:") {
		return false
	}
	rest := strings.TrimSpace(trimmed[len("image:"):])
	return rest == "" || strings.HasPrefix(rest, "#")
}

// chartImageTagLineIndex returns the index of the chart-level
// `image.tag:` line, or -1 when the file has no such declaration.
//
// Two indent shapes need to work:
//  1. Tenant `services/<tenant>/<svc>/values.yaml` — `image:` is at
//     column 0 with `tag:` indented two spaces under it.
//  2. Platform `apps/templates/<svc>.yaml` — the chart values are
//     inlined under `helm.values: |`, so `image:` ends up indented
//     several levels deep (typically 8 spaces) with `tag:` deeper still.
//
// Algorithm: collect every `image:` line in the file, then prefer the
// one(s) at the SHALLOWEST indent — that's the chart image both for
// tenant values (indent 0) and for platform inline-values (indent 8,
// the only `image:` in the document). Sub-image declarations like
// `sidecar.image:` or `extraContainers[].image:` always sit at a
// deeper indent than the chart image, so anchoring on min-indent
// avoids the "first match wins" trap where a sub-image declared
// before the chart image would steal the rollback target.
//
// Among shallowest candidates (rare — would mean two top-level
// `image:` siblings), the first to expose a direct `tag:` child wins.
// "Direct" means at the indent of the block's first non-blank child;
// deeper-indented `tag:` keys belong to nested maps and don't count.
//
// Known limitations (deliberately deferred — neither shape exists in
// today's mctl-gitops; revisit if a future values file regresses):
//   - `tag: ""` / `tag: ''` (empty quoted) is treated as no-tag.
//     Helm sometimes uses this when defaults supply the effective tag,
//     but rollback has nothing to roll back to in that case anyway.
//   - If the SHALLOWEST `image:` block has no direct `tag:` child but
//     a deeper one does, we return -1 instead of falling back to the
//     deeper block. A wrapped manifest with a non-chart shallow
//     `image:` map plus chart values further down would degrade to
//     diagnosis-only; the operator can still merge a manual rollback
//     PR. Switching the line-scanner to a YAML-AST walker would
//     eliminate both — left to a follow-up since the cost (yaml.v3
//     decoder + line-tracking via yaml.Node.Line) outweighs the
//     benefit until a real values file actually breaks.
func chartImageTagLineIndex(lines []string) int {
	type candidate struct {
		idx    int
		indent int
	}
	var cands []candidate
	minIndent := -1
	for i, line := range lines {
		if !isImageBlockKey(strings.TrimSpace(line)) {
			continue
		}
		ind := len(indentOf(line))
		cands = append(cands, candidate{idx: i, indent: ind})
		if minIndent == -1 || ind < minIndent {
			minIndent = ind
		}
	}
	for _, c := range cands {
		if c.indent != minIndent {
			continue
		}
		if idx := tagInImageBlock(lines, c.idx, c.indent); idx >= 0 {
			return idx
		}
	}
	return -1
}

// tagInImageBlock walks forward from imageIdx (a line whose trimmed
// text is `image:`) and returns the index of the direct-child `tag:`
// line, or -1 if the block has no such child. The block ends when a
// line at indent ≤ imageIndent appears.
func tagInImageBlock(lines []string, imageIdx, imageIndent int) int {
	childIndent := ""
	for j := imageIdx + 1; j < len(lines); j++ {
		l := lines[j]
		trimmed := strings.TrimSpace(l)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		ind := indentOf(l)
		if len(ind) <= imageIndent {
			return -1
		}
		if childIndent == "" {
			childIndent = ind
		}
		if ind != childIndent {
			continue
		}
		if _, _, _, ok := parseTagLine(l); ok {
			return j
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
