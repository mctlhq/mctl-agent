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
	"strconv"
	"strings"
)

// ParseCPUMillis parses a Kubernetes CPU quantity into millicores:
// "500m" → 500, "1" → 1000, "0.5" → 500.
func ParseCPUMillis(s string) (int64, error) {
	s = strings.Trim(strings.TrimSpace(s), `"'`)
	if s == "" {
		return 0, fmt.Errorf("empty cpu quantity")
	}
	if strings.HasSuffix(s, "m") {
		n, err := strconv.ParseFloat(strings.TrimSuffix(s, "m"), 64)
		if err != nil {
			return 0, fmt.Errorf("cpu quantity %q: %w", s, err)
		}
		return int64(n), nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("cpu quantity %q: %w", s, err)
	}
	return int64(f * 1000), nil
}

// ParseMemBytes parses a Kubernetes memory quantity into bytes.
func ParseMemBytes(s string) (int64, error) {
	s = strings.Trim(strings.TrimSpace(s), `"'`)
	if s == "" {
		return 0, fmt.Errorf("empty memory quantity")
	}
	suffixes := []struct {
		suffix string
		mult   float64
	}{
		{"Ki", 1 << 10},
		{"Mi", 1 << 20},
		{"Gi", 1 << 30},
		{"Ti", 1 << 40},
		{"k", 1e3},
		{"M", 1e6},
		{"G", 1e9},
		{"T", 1e12},
	}
	for _, sfx := range suffixes {
		if strings.HasSuffix(s, sfx.suffix) {
			n, err := strconv.ParseFloat(strings.TrimSuffix(s, sfx.suffix), 64)
			if err != nil {
				return 0, fmt.Errorf("memory quantity %q: %w", s, err)
			}
			return int64(n * sfx.mult), nil
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("memory quantity %q: %w", s, err)
	}
	return int64(f), nil
}

// FormatCPUMillis renders millicores in the values.yaml house style ("250m").
func FormatCPUMillis(m int64) string {
	return fmt.Sprintf("%dm", m)
}

// FormatMemBytes renders bytes as Mi (the dominant unit in service values),
// falling back to Gi for clean gibibyte multiples of 4Gi and above.
func FormatMemBytes(b int64) string {
	const mi = 1 << 20
	const gi = 1 << 30
	if b >= 4*gi && b%gi == 0 {
		return fmt.Sprintf("%dGi", b/gi)
	}
	return fmt.Sprintf("%dMi", (b+mi-1)/mi)
}

// roundUpTo rounds v up to the next multiple of step.
func roundUpTo(v, step int64) int64 {
	if step <= 0 {
		return v
	}
	if rem := v % step; rem != 0 {
		return v + step - rem
	}
	return v
}
