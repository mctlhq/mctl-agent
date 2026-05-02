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
// image tag. Only the FIRST `tag:` line in the file is rewritten — values
// files routinely embed sidecar / initContainer images deeper in the
// document with their own `tag:` keys, and a rollback must not collateral-
// damage those. This matches ExtractImageTag's first-occurrence semantics
// so the read-back invariant holds.
func GenerateImageRollback(content, previousTag string) (string, string, error) {
	lines := strings.Split(content, "\n")
	var summary string
	rewritten := false

	for i, line := range lines {
		if rewritten {
			break
		}
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "tag:") {
			continue
		}
		indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		currentTag := strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "tag:")), "\"'")
		lines[i] = fmt.Sprintf(`%stag: "%s"`, indent, previousTag)
		summary = fmt.Sprintf("Rollback image tag from %s to %s", currentTag, previousTag)
		rewritten = true
	}

	if !rewritten {
		return content, "", fmt.Errorf("could not find image tag to rollback")
	}

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

// GenerateProbeFix creates a patch that increases initialDelaySeconds for a probe.
func GenerateProbeFix(content string, probeField string) (string, string, error) {
	lines := strings.Split(content, "\n")
	var modified []string
	summary := ""
	probeName := strings.TrimSuffix(probeField, ".initialDelaySeconds")
	inProbe := false
	probeIndent := ""

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		if trimmed == probeName+":" {
			inProbe = true
			probeIndent = indent
		}
		if inProbe && indent == probeIndent && trimmed != "" && trimmed != probeName+":" && !strings.HasPrefix(trimmed, "initialDelaySeconds:") && !strings.HasPrefix(indent, probeIndent+" ") && !strings.HasPrefix(indent, probeIndent+"\t") {
			inProbe = false
		}
		if inProbe && strings.HasPrefix(trimmed, "initialDelaySeconds:") {
			parts := strings.Split(line, ":")
			if len(parts) == 2 {
				val, _ := strconv.Atoi(strings.TrimSpace(parts[1]))
				newVal := val + 15
				if newVal < 30 {
					newVal = 30
				}
				line = strings.Replace(line, parts[1], fmt.Sprintf(" %d", newVal), 1)
				summary = fmt.Sprintf("Increase %s initialDelaySeconds to %d", probeField, newVal)
				inProbe = false
			}
		}
		modified = append(modified, line)
	}

	if summary == "" {
		return content, "", fmt.Errorf("could not find %s initialDelaySeconds to bump", probeField)
	}

	return strings.Join(modified, "\n"), summary, nil
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
