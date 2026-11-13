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
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// DiagnosisCompat provides the fields needed by GenerateFromDiagnosis,
// decoupling fixer from the diagnosis package.
type DiagnosisCompat struct {
	Diagnosis      string `json:"diagnosis"`
	Confidence     string `json:"confidence"`
	Fixable        bool   `json:"fixable"`
	YAMLPath       string `json:"yaml_path"`
	YAMLField      string `json:"yaml_field"`
	CurrentValue   string `json:"current_value"`
	SuggestedValue string `json:"suggested_value"`
	Reasoning      string `json:"reasoning"`
}

// defaultProbeDelay is the initialDelaySeconds written into a probe block
// that relies on the chart default and therefore has no explicit value.
const defaultProbeDelay = 30

// PlatformServices have no live GitOps values file the agent can patch:
// their manifests are ArgoCD Application definitions inlined under
// platform-gitops/bootstrap/templates/mctl-platform/, which is not in the
// agent's write allowlist (internal/gitopspath). DetectFilePath returns ""
// for these; callers must escalate rather than attempt a GitOps read — see
// IsPlatformService.
//
// This used to point at a per-service YAML file under the directory that was
// renamed to platform-gitops/bootstrap/templates/ in mctl-gitops (commit
// 18d64715); the old location no longer exists.
var PlatformServices = map[string]bool{
	"mctl-api":   true,
	"mctl-agent": true,
}

// IsPlatformService reports whether service is a platform service with no
// GitOps values file the agent can read or patch automatically. A caller
// that sees this must escalate instead of calling DetectFilePath's result
// (which is "") through GetFileContent/CreatePR.
func IsPlatformService(service string) bool {
	return PlatformServices[service]
}

// PatchResult contains the generated patch.
type PatchResult struct {
	FilePath   string
	OldContent string
	NewContent string
	Summary    string
}

// DetectFilePath determines the gitops values file path for a service, or
// "" for a platform service (see IsPlatformService) or an empty tenant/
// service — the caller must escalate rather than treat "" as a path to read
// or write.
func DetectFilePath(tenant, service string) string {
	if tenant == "" || service == "" {
		return ""
	}
	if PlatformServices[service] {
		return ""
	}
	return fmt.Sprintf("platform-gitops/services/%s/%s/values.yaml", tenant, service)
}

// CandidatePaths maps every service name in services through DetectFilePath
// for tenant, drops empty results (platform services, which have nothing to
// probe), and deduplicates while preserving order.
func CandidatePaths(tenant string, services []string) []string {
	var paths []string
	seen := make(map[string]bool)
	for _, svc := range services {
		p := DetectFilePath(tenant, svc)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		paths = append(paths, p)
	}
	return paths
}

// GenerateMemoryBump creates a patch that increases the chart-level memory
// limit (top-level resources.limits.memory) by 50%.
//
// Only that one scalar is rewritten. Values files also carry
// resources.requests.memory, sidecar resources under extraContainers, and
// resources of containers embedded in extraObjects; matching every
// "memory:" line used to raise all of them under a summary that named only
// the last one (mctl-gitops#2067, #2129, #2463).
func GenerateMemoryBump(content string) (string, string, error) {
	return bumpChartLimit(content, "memory", bumpMemory, "memory limit")
}

// GenerateImageRollback creates a patch that rolls back the chart-level
// image tag. Only the `tag:` line that's a direct child of the top-level
// `image:` block is rewritten — values files routinely embed sidecar /
// initContainer images deeper in the document, and unrelated tag keys
// like `global.tag` may appear earlier in the file. Inline comments on
// the tag line (e.g. `tag: "1.2.3"  # bumped 2026-04-30`) are preserved.
//
// chartImageTagLineIndex (location), parseTagLine (read), and the rewrite
// here all agree on what counts as the chart image, so the
// read-then-rewrite invariant holds.
func GenerateImageRollback(content, previousTag string) (string, string, error) {
	lines := strings.Split(content, "\n")
	idx := chartImageTagLineIndex(lines)
	if idx < 0 {
		return content, "", fmt.Errorf("could not find chart-level image.tag to rollback")
	}
	currentTag, prefix, comment, ok := parseTagLine(lines[idx])
	if !ok {
		return content, "", fmt.Errorf("chart-level tag line is not parseable: %q", lines[idx])
	}
	indent := prefix[:len(prefix)-len(strings.TrimLeft(prefix, " \t"))]
	lines[idx] = fmt.Sprintf(`%stag: "%s"%s`, indent, previousTag, comment)
	summary := fmt.Sprintf("Rollback image tag from %s to %s", currentTag, previousTag)
	return strings.Join(lines, "\n"), summary, nil
}

// GenerateFromDiagnosis applies a Claude-suggested fix.
//
// diag.YAMLField is a dotted path from the document root
// ("resources.limits.memory", "autoscaling.maxReplicas"). Only the scalar at
// that path is rewritten, and only when it currently holds
// diag.CurrentValue: replacing the first textual occurrence of the value
// anywhere in the file used to rewrite unrelated keys that happened to share
// it (replicas: 30 instead of probe timeoutSeconds: 30).
func GenerateFromDiagnosis(content string, diag *DiagnosisCompat) (string, string, error) {
	if diag.YAMLField == "" || diag.SuggestedValue == "" {
		return content, "", fmt.Errorf("diagnosis missing yaml_field or suggested_value")
	}
	if diag.CurrentValue == "" {
		return content, "", fmt.Errorf("diagnosis for %s has no current_value to verify against", diag.YAMLField)
	}

	path := strings.Split(diag.YAMLField, ".")
	lines := strings.Split(content, "\n")
	idx, err := findScalarLine(lines, path)
	if err != nil {
		return content, "", fmt.Errorf("could not apply suggested fix for field %s: %w", diag.YAMLField, err)
	}
	sc := parseScalarLine(lines[idx])
	if sc.value != unquote(diag.CurrentValue) {
		return content, "", fmt.Errorf("field %s holds %q, not the diagnosed current value %q", diag.YAMLField, sc.value, diag.CurrentValue)
	}
	suggested := unquote(diag.SuggestedValue)
	if err := checkPlainValue(suggested, sc.quote); err != nil {
		return content, "", fmt.Errorf("suggested value for %s: %w", diag.YAMLField, err)
	}
	lines[idx] = sc.render(suggested)
	summary := fmt.Sprintf("Update %s from %s to %s", diag.YAMLField, sc.value, suggested)
	return strings.Join(lines, "\n"), summary, nil
}

// GenerateCPUBump creates a patch that increases the chart-level CPU limit
// (top-level resources.limits.cpu) by 50%. See GenerateMemoryBump for why
// only that one scalar is touched.
func GenerateCPUBump(content string) (string, string, error) {
	return bumpChartLimit(content, "cpu", bumpCPU, "CPU limit")
}

// bumpChartLimit rewrites the scalar at resources.limits.<resource> with
// bump(current). It fails rather than guessing when the path is absent, the
// value is not one bump understands, or the file is not block-style YAML.
func bumpChartLimit(content, resource string, bump func(string) string, label string) (string, string, error) {
	lines := strings.Split(content, "\n")
	idx, err := findScalarLine(lines, []string{"resources", "limits", resource})
	if err != nil {
		return content, "", fmt.Errorf("could not find %s to bump: %w", label, err)
	}
	sc := parseScalarLine(lines[idx])
	next := bump(sc.value)
	if next == sc.value {
		return content, "", fmt.Errorf("could not bump %s: unsupported value %q", label, sc.value)
	}
	lines[idx] = sc.render(next)
	summary := fmt.Sprintf("Bump %s from %s to %s", label, sc.value, next)
	return strings.Join(lines, "\n"), summary, nil
}

// findScalarLine returns the index of the line holding the scalar at path,
// where path[0] is a key at column 0 and every later element is a direct
// child mapping key of the one before it. Intermediate keys must open a
// block mapping (nothing but an optional comment after the colon); the last
// key must carry a value. Sequences, flow mappings and anchors are not
// followed: the caller gets an error and escalates instead of a guess.
func findScalarLine(lines []string, path []string) (int, error) {
	if len(path) == 0 {
		return -1, fmt.Errorf("empty path")
	}
	start, end, parentIndent := 0, len(lines), -1
	for depth, key := range path {
		if key == "" {
			return -1, fmt.Errorf("empty segment in path %q", strings.Join(path, "."))
		}
		childIndent := -1
		if depth == 0 {
			childIndent = 0 // path[0] is a document-root key
		}
		found := -1
		for i := start; i < end; i++ {
			trimmed := strings.TrimSpace(lines[i])
			if trimmed == "" || isCommentLine(trimmed) {
				continue
			}
			ind := len(indentOf(lines[i]))
			if ind <= parentIndent {
				end = i // the parent block ended
				break
			}
			if childIndent < 0 {
				childIndent = ind
			}
			if ind != childIndent {
				continue // deeper line belonging to a sibling's block
			}
			if k, ok := mappingKey(trimmed); ok && k == key {
				found = i
				break
			}
		}
		if found < 0 {
			return -1, fmt.Errorf("key %q not found", strings.Join(path[:depth+1], "."))
		}
		value, _ := splitInlineComment(lineValue(lines[found]))
		value = strings.TrimSpace(value)
		last := depth == len(path)-1
		if last {
			if value == "" {
				return -1, fmt.Errorf("%q is a block, not a scalar", strings.Join(path, "."))
			}
			return found, nil
		}
		if value != "" {
			return -1, fmt.Errorf("%q is not a block mapping", strings.Join(path[:depth+1], "."))
		}
		start, parentIndent = found+1, childIndent
	}
	return -1, fmt.Errorf("key %q not found", strings.Join(path, "."))
}

// mappingKey returns the key of a trimmed "key: value" or "key:" line. A
// sequence item ("- name: x") is not a mapping key at this level.
func mappingKey(trimmed string) (string, bool) {
	if strings.HasPrefix(trimmed, "-") {
		return "", false
	}
	k, rest, ok := strings.Cut(trimmed, ":")
	if !ok || (rest != "" && rest[0] != ' ' && rest[0] != '\t') {
		return "", false // "http://x" or similar, not a key
	}
	return strings.Trim(strings.TrimSpace(k), "\"'"), true
}

// lineValue returns everything after the first ": " separator of a line.
func lineValue(line string) string {
	_, rest, _ := strings.Cut(line, ":")
	return rest
}

// scalarLine is a parsed "key: value  # comment" line, kept so a rewrite
// preserves indentation, quoting style and the trailing comment.
type scalarLine struct {
	prefix  string // indentation, key and colon, e.g. "    memory:"
	value   string // unquoted value
	quote   string // "" or the quote character the value was written with
	comment string // trailing comment with its leading whitespace, or ""
}

func parseScalarLine(line string) scalarLine {
	key, rest, _ := strings.Cut(line, ":")
	v, comment := splitInlineComment(rest)
	v = strings.TrimSpace(v)
	q := ""
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		q = v[:1]
	}
	return scalarLine{prefix: key + ":", value: unquote(v), quote: q, comment: comment}
}

func (s scalarLine) render(value string) string {
	return s.prefix + " " + s.quote + value + s.quote + s.comment
}

// checkPlainValue rejects a value that, spliced into a scalar line with the
// given quoting, could be read as anything other than that one scalar: extra
// lines, a comment, a nested mapping, a flow collection, an anchor or alias,
// or a tag. A diagnosis value is LLM output built partly from pod logs, so it
// is untrusted, and refusing is safer than escaping it.
func checkPlainValue(v, quote string) error {
	if v == "" || strings.TrimSpace(v) != v {
		return fmt.Errorf("%q is empty or has surrounding whitespace", v)
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%q contains a control character", v)
		}
	}
	if quote != "" {
		if strings.Contains(v, quote) || (quote == `"` && strings.Contains(v, `\`)) {
			return fmt.Errorf("%q cannot be placed inside %s quotes as is", v, quote)
		}
		return nil
	}
	if strings.ContainsAny(v[:1], "-?:,[]{}#&*!|>'\"%@`") ||
		strings.Contains(v, ": ") || strings.Contains(v, " #") || strings.HasSuffix(v, ":") {
		return fmt.Errorf("%q would not stay a plain YAML scalar", v)
	}
	return nil
}

func unquote(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}

// probeKinds maps a diagnosis YAMLField onto the probe kinds it refers to.
// The probe_fix skill emits "livenessProbe.initialDelaySeconds",
// "readinessProbe.initialDelaySeconds", or — when the side is unknown —
// "liveness/readinessProbe.initialDelaySeconds". That last form is not a YAML
// key anywhere; it means "both", and used to be looked up verbatim, which
// could never match and made every ambiguous probe ticket fail to patch.
func probeKinds(probeField string) []string {
	name := strings.TrimSuffix(probeField, ".initialDelaySeconds")
	name = strings.TrimSuffix(name, "Probe")
	switch name {
	case "liveness", "readiness", "startup":
		return []string{name}
	case "liveness/readiness":
		return []string{"liveness", "readiness"}
	}
	return nil
}

// splitInlineComment separates a YAML scalar from a trailing "# ..." comment,
// returning the value and the comment with its leading whitespace intact so a
// rewritten line can carry the comment through untouched.
func splitInlineComment(s string) (value, comment string) {
	i := strings.Index(s, "#")
	if i < 0 {
		return s, ""
	}
	// The gap between the value and the "#" belongs to the comment, so a
	// rewritten line keeps its original spacing rather than jamming the
	// comment against the new number.
	head := s[:i]
	trimmed := strings.TrimRight(head, " \t")
	return trimmed, head[len(trimmed):] + s[i:]
}

// isCommentLine reports whether a trimmed line is a YAML comment. Comments
// carry arbitrary indentation, so they must never be mistaken for a block's
// first child — doing so takes the block's child indent from the comment and
// splices later insertions in at the wrong depth.
func isCommentLine(trimmed string) bool {
	return strings.HasPrefix(trimmed, "#")
}

// probeBlock is one probe definition located inside a YAML document.
type probeBlock struct {
	// kind is the probe this block describes ("liveness"/"readiness"/"startup"),
	// kept per block so the summary can name the right value for each one.
	kind string
	// firstChild is the index of the block's first child line, where an
	// initialDelaySeconds is inserted when the block does not have one.
	firstChild int
	// childIndent is the indentation shared by the block's direct children.
	childIndent string
	// delayLine is the index of the block's own initialDelaySeconds line,
	// or -1 when the block does not set one.
	delayLine int
}

// findProbeBlocks locates every block describing one of the given probe kinds.
// Two shapes are recognised, because the agent historically patched both raw
// Kubernetes manifests and base-service values.yaml files:
//
//	livenessProbe:            # raw manifest
//	  initialDelaySeconds: 10
//
//	probes:                   # base-service chart (services/<tenant>/*/values.yaml)
//	  liveness:
//	    initialDelaySeconds: 10
//
// Only the chart shape appears in tenant values.yaml, so the manifest-only
// lookup previously left every tenant service unpatchable.
func findProbeBlocks(lines []string, kinds []string) []probeBlock {
	wanted := make(map[string]string, len(kinds)*2)
	for _, k := range kinds {
		wanted[k+"Probe:"] = k
		wanted[k+":"] = k
	}

	var blocks []probeBlock
	probesIndent := ""
	inProbes := false

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || isCommentLine(trimmed) {
			continue
		}
		indent := indentOf(line)

		// Track the chart-shaped "probes:" mapping so that a bare
		// "liveness:" key is only treated as a probe inside it.
		if inProbes && len(indent) <= len(probesIndent) {
			inProbes = false
		}
		// A block key may carry a trailing comment ("liveness: # startup is slow").
		key, _ := splitInlineComment(trimmed)
		key = strings.TrimSpace(key)
		if key == "probes:" {
			inProbes = true
			probesIndent = indent
			continue
		}

		kind, ok := wanted[key]
		if !ok {
			continue
		}
		if !strings.HasSuffix(key, "Probe:") && !inProbes {
			// A bare "liveness:"/"readiness:" outside a probes: mapping is
			// some unrelated key — do not touch it.
			continue
		}

		if b, ok := scanProbeBlock(lines, i, indent); ok {
			b.kind = kind
			blocks = append(blocks, b)
		}
	}
	return blocks
}

// scanProbeBlock walks the children of the probe block opened at start.
func scanProbeBlock(lines []string, start int, blockIndent string) (probeBlock, bool) {
	b := probeBlock{firstChild: -1, delayLine: -1}
	for i := start + 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			continue
		}
		indent := indentOf(lines[i])
		if isCommentLine(trimmed) {
			// A comment may sit at any depth, including deeper than the keys
			// it documents. It can neither end the block nor define its child
			// indent, so skip it outright.
			continue
		}
		if len(indent) <= len(blockIndent) {
			break // block ended
		}
		if b.firstChild < 0 {
			b.firstChild = i
			b.childIndent = indent
		}
		if indent == b.childIndent && strings.HasPrefix(trimmed, "initialDelaySeconds:") {
			b.delayLine = i
			break
		}
	}
	return b, b.firstChild >= 0
}

// bumpedDelay raises a probe delay by 15s, with a 30s floor.
func bumpedDelay(current int) int {
	next := current + 15
	if next < 30 {
		next = 30
	}
	return next
}

// GenerateProbeFix creates a patch that increases initialDelaySeconds for a
// probe. When the probe block exists but sets no initialDelaySeconds (the
// chart supplies a default), the field is inserted rather than reported as
// unpatchable.
func GenerateProbeFix(content string, probeField string) (string, string, error) {
	kinds := probeKinds(probeField)
	if len(kinds) == 0 {
		return content, "", fmt.Errorf("unrecognised probe field %q", probeField)
	}

	lines := strings.Split(content, "\n")
	blocks := findProbeBlocks(lines, kinds)
	if len(blocks) == 0 {
		return content, "", fmt.Errorf("could not find a %s probe block to patch", strings.Join(kinds, "/"))
	}

	// One entry per block, filled in place so the summary keeps file order
	// even though the patching below runs bottom-up.
	parts := make([]string, len(blocks))
	// Apply from the bottom up so that inserting lines does not shift the
	// indices of the blocks still to be patched.
	for i := len(blocks) - 1; i >= 0; i-- {
		b := blocks[i]
		if b.delayLine >= 0 {
			key, rest, ok := strings.Cut(lines[b.delayLine], ":")
			if !ok {
				continue
			}
			value, comment := splitInlineComment(rest)
			current, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				continue
			}
			next := bumpedDelay(current)
			lines[b.delayLine] = fmt.Sprintf("%s: %d%s", key, next, comment)
			parts[i] = fmt.Sprintf("%s %d -> %d", b.kind, current, next)
			continue
		}
		inserted := fmt.Sprintf("%sinitialDelaySeconds: %d", b.childIndent, defaultProbeDelay)
		lines = append(lines[:b.firstChild], append([]string{inserted}, lines[b.firstChild:]...)...)
		parts[i] = fmt.Sprintf("%s -> %d", b.kind, defaultProbeDelay)
	}

	var changed []string
	for _, p := range parts {
		if p != "" {
			changed = append(changed, p)
		}
	}
	if len(changed) == 0 {
		return content, "", fmt.Errorf("found a %s probe block but its initialDelaySeconds is not a number", strings.Join(kinds, "/"))
	}

	summary := "Increase probe initialDelaySeconds: " + strings.Join(changed, ", ")
	return strings.Join(lines, "\n"), summary, nil
}

// workflowDefaultKey matches a "default:" mapping key, optionally as the
// first key of a sequence item, and nothing that merely ends in "default:"
// (isdefault:, x-default:).
var workflowDefaultKey = regexp.MustCompile(`(?m)^([ \t]*(?:-[ \t]+)?)default:`)

// GenerateWorkflowParamFix replaces 'default:' keys with 'value:' in workflow arguments.
func GenerateWorkflowParamFix(content string) (string, string, error) {
	newContent := workflowDefaultKey.ReplaceAllString(content, "${1}value:")
	if newContent == content {
		return content, "", fmt.Errorf("no 'default:' key found in template")
	}
	return newContent, "Replace 'default:' with 'value:' in ClusterWorkflowTemplate", nil
}

// bumpCPU increases a Kubernetes CPU string by 50%.
func bumpCPU(cpu string) string {
	cpu = strings.TrimSpace(cpu)
	cpu = strings.Trim(cpu, "\"'")

	if strings.HasSuffix(cpu, "m") {
		val := strings.TrimSuffix(cpu, "m")
		if n, err := strconv.Atoi(val); err == nil {
			newVal := n + n/2
			return fmt.Sprintf("%dm", newVal)
		}
	}

	// Whole-core values like "1", "2".
	if f, err := strconv.ParseFloat(cpu, 64); err == nil {
		newMillis := int(f * 1500)
		return fmt.Sprintf("%dm", newMillis)
	}

	return cpu
}

// bumpMemory increases a Kubernetes memory string by 50%.
func bumpMemory(mem string) string {
	mem = strings.TrimSpace(mem)
	mem = strings.Trim(mem, "\"'")

	if strings.HasSuffix(mem, "Gi") {
		val := strings.TrimSuffix(mem, "Gi")
		if f, err := strconv.ParseFloat(val, 64); err == nil {
			newVal := f * 1.5
			if newVal == float64(int(newVal)) {
				return fmt.Sprintf("%dGi", int(newVal))
			}
			return fmt.Sprintf("%.1fGi", newVal)
		}
	}

	if strings.HasSuffix(mem, "Mi") {
		val := strings.TrimSuffix(mem, "Mi")
		if n, err := strconv.Atoi(val); err == nil {
			newVal := n + n/2
			// Round up to nearest 64Mi.
			if remainder := newVal % 64; remainder != 0 {
				newVal += 64 - remainder
			}
			return fmt.Sprintf("%dMi", newVal)
		}
	}

	return mem
}
