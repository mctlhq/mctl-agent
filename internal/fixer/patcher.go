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

// GenerateImageRollback creates a patch that rolls back the image tag.
func GenerateImageRollback(content, previousTag string) (string, string, error) {
	lines := strings.Split(content, "\n")
	var modified []string
	var summary string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "tag:") {
			indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
			currentTag := strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "tag:")), "\"'")
			modified = append(modified, fmt.Sprintf(`%stag: "%s"`, indent, previousTag))
			summary = fmt.Sprintf("Rollback image tag from %s to %s", currentTag, previousTag)
			continue
		}
		modified = append(modified, line)
	}

	if summary == "" {
		return content, "", fmt.Errorf("could not find image tag to rollback")
	}

	return strings.Join(modified, "\n"), summary, nil
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
