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

// PlatformServices that use inline values in apps/templates/.
var PlatformServices = map[string]bool{
	"mctl-api":   true,
	"mctl-agent": true,
}

// PatchResult contains the generated patch.
type PatchResult struct {
	FilePath   string
	OldContent string
	NewContent string
	Summary    string
}

// DetectFilePath determines the gitops file path for a service.
func DetectFilePath(tenant, service string) string {
	if PlatformServices[service] {
		return fmt.Sprintf("platform-gitops/apps/templates/%s.yaml", service)
	}
	return fmt.Sprintf("platform-gitops/services/%s/%s/values.yaml", tenant, service)
}

// GenerateMemoryBump creates a patch that increases memory limit by 50%.
func GenerateMemoryBump(content string) (string, string, error) {
	// Find memory limit line and bump by 50%.
	lines := strings.Split(content, "\n")
	var modified []string
	var summary string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "memory:") && strings.Contains(content, "limits:") {
			// Extract current value.
			parts := strings.SplitN(trimmed, ":", 2)
			if len(parts) == 2 {
				current := strings.TrimSpace(parts[1])
				newVal := bumpMemory(current)
				if newVal != current {
					indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
					modified = append(modified, indent+"memory: "+newVal)
					summary = fmt.Sprintf("Bump memory limit from %s to %s", current, newVal)
					continue
				}
			}
		}
		modified = append(modified, line)
	}

	if summary == "" {
		return content, "", fmt.Errorf("could not find memory limit to bump")
	}

	return strings.Join(modified, "\n"), summary, nil
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
func GenerateFromDiagnosis(content string, diag *DiagnosisCompat) (string, string, error) {
	if diag.YAMLField == "" || diag.SuggestedValue == "" {
		return content, "", fmt.Errorf("diagnosis missing yaml_field or suggested_value")
	}

	// Simple field replacement for common paths.
	if diag.CurrentValue != "" {
		newContent := strings.Replace(content, diag.CurrentValue, diag.SuggestedValue, 1)
		if newContent != content {
			summary := fmt.Sprintf("Update %s from %s to %s", diag.YAMLField, diag.CurrentValue, diag.SuggestedValue)
			return newContent, summary, nil
		}
	}

	return content, "", fmt.Errorf("could not apply suggested fix for field %s", diag.YAMLField)
}

// GenerateCPUBump creates a patch that increases CPU limit by 50%.
func GenerateCPUBump(content string) (string, string, error) {
	lines := strings.Split(content, "\n")
	var modified []string
	var summary string

	inLimits := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "limits:") {
			inLimits = true
		} else if inLimits && trimmed != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			inLimits = false
		}

		if inLimits && strings.HasPrefix(trimmed, "cpu:") {
			parts := strings.SplitN(trimmed, ":", 2)
			if len(parts) == 2 {
				current := strings.TrimSpace(parts[1])
				current = strings.Trim(current, "\"'")
				newVal := bumpCPU(current)
				if newVal != current {
					indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
					modified = append(modified, indent+"cpu: "+newVal)
					summary = fmt.Sprintf("Bump CPU limit from %s to %s", current, newVal)
					continue
				}
			}
		}
		modified = append(modified, line)
	}

	if summary == "" {
		return content, "", fmt.Errorf("could not find CPU limit to bump")
	}

	return strings.Join(modified, "\n"), summary, nil
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
// Two shapes are recognised, because the agent patches both raw Kubernetes
// manifests and base-service values.yaml files:
//
//	livenessProbe:            # raw manifest (apps/templates/*.yaml)
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

// GenerateWorkflowParamFix replaces 'default:' with 'value:' in workflow arguments.
func GenerateWorkflowParamFix(content string) (string, string, error) {
	newContent := strings.ReplaceAll(content, "default:", "value:")
	if newContent == content {
		return content, "", fmt.Errorf("no 'default:' found in template")
	}
	return newContent, "Replace 'default:' with 'value:' in ClusterWorkflowTemplate", nil
}

// GenerateAppProjectWhitelistFix adds missing API groups to the AppProject whitelist.
func GenerateAppProjectWhitelistFix(content string) (string, string, error) {
	// Simple implementation: find external-secrets.io and append others.
	groups := []string{"argoproj.io", "monitoring.coreos.com"}
	newContent := content
	var added []string

	for _, g := range groups {
		if !strings.Contains(content, g) {
			pattern := "group: external-secrets.io\n      kind: \"*\""
			replacement := pattern + fmt.Sprintf("\n    - group: %s\n      kind: \"*\"", g)
			newContent = strings.Replace(newContent, pattern, replacement, 1)
			added = append(added, g)
		}
	}

	if len(added) == 0 {
		return content, "", fmt.Errorf("API groups already present or anchor not found")
	}

	return newContent, "Add missing API groups to AppProject whitelist: " + strings.Join(added, ", "), nil
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
