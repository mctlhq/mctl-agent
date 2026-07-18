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

package optimizer

import (
	"fmt"
	"strings"
)

// GenerateRequestsPatch rewrites only the cpu/memory values under the
// top-level (column-0) resources.requests block of a values.yaml.
//
// Line surgery, not YAML re-marshalling: several service files carry
// load-bearing incident-history comments that must survive byte-identical,
// and sidecar resources blocks under extraContainers/initContainers are
// deeper-indented so they are never entered. An empty newCPU/newMem leaves
// that resource untouched. Returns the patched content and a human summary
// of what changed.
func GenerateRequestsPatch(content, newCPU, newMem string) (string, string, error) {
	lines := strings.Split(content, "\n")

	inResources := false
	inRequests := false
	requestsIndent := 0
	cpuPatched, memPatched := false, false
	var changes []string

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))

		if indent == 0 {
			inResources = trimmed == "resources:" || strings.HasPrefix(trimmed, "resources: #")
			inRequests = false
			continue
		}
		if !inResources {
			continue
		}

		if inRequests && indent <= requestsIndent {
			// Sibling of requests (e.g. limits:) — the requests block ended.
			inRequests = false
		}

		if !inRequests {
			if trimmed == "requests:" || strings.HasPrefix(trimmed, "requests: #") {
				inRequests = true
				requestsIndent = indent
			}
			continue
		}

		key, oldVal := splitKeyValue(trimmed)
		switch key {
		case "cpu":
			if newCPU != "" && oldVal != newCPU {
				lines[i] = replaceScalarValue(line, newCPU)
				changes = append(changes, fmt.Sprintf("cpu request %s -> %s", oldVal, newCPU))
			}
			cpuPatched = true
		case "memory":
			if newMem != "" && oldVal != newMem {
				lines[i] = replaceScalarValue(line, newMem)
				changes = append(changes, fmt.Sprintf("memory request %s -> %s", oldVal, newMem))
			}
			memPatched = true
		}
	}

	if newCPU != "" && !cpuPatched {
		return "", "", fmt.Errorf("no top-level resources.requests.cpu found")
	}
	if newMem != "" && !memPatched {
		return "", "", fmt.Errorf("no top-level resources.requests.memory found")
	}
	if len(changes) == 0 {
		return "", "", fmt.Errorf("nothing to change")
	}

	return strings.Join(lines, "\n"), strings.Join(changes, "; "), nil
}

// splitKeyValue splits a trimmed "key: value  # comment" line, stripping any
// inline comment and surrounding quotes from the value.
func splitKeyValue(trimmed string) (key, value string) {
	idx := strings.Index(trimmed, ":")
	if idx < 0 {
		return trimmed, ""
	}
	key = strings.TrimSpace(trimmed[:idx])
	value = strings.TrimSpace(trimmed[idx+1:])
	if c := strings.Index(value, " #"); c >= 0 {
		value = strings.TrimSpace(value[:c])
	}
	return key, strings.Trim(value, `"'`)
}

// replaceScalarValue swaps the scalar value of a "key: value" line while
// preserving indentation, the key, and any inline comment.
func replaceScalarValue(line, newVal string) string {
	colon := strings.Index(line, ":")
	if colon < 0 {
		return line
	}
	prefix := line[:colon+1]
	rest := line[colon+1:]

	comment := ""
	if c := strings.Index(rest, " #"); c >= 0 {
		comment = rest[c:]
	}
	return prefix + " " + newVal + comment
}
