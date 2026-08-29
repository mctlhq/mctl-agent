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

package skill

import "testing"

// TestAllCapabilityIDsMatchesConstBlock is a regression guard: if a new
// CapabilityID constant is ever added to the const block above without
// updating AllCapabilityIDs, this test must be extended to fail until it is.
// Bump wantCount (and the membership list) whenever the const block changes.
func TestAllCapabilityIDsMatchesConstBlock(t *testing.T) {
	const wantCount = 11
	want := []CapabilityID{
		CapReadLogs,
		CapReadConfig,
		CapReadStatus,
		CapReadResources,
		CapReadAudit,
		CapModifyGitOps,
		CapCreatePR,
		CapMergePR,
		CapSendNotify,
		CapCallLLM,
		CapExecWorkflow,
	}
	if len(want) != wantCount {
		t.Fatalf("test fixture out of date: want list has %d entries, wantCount=%d", len(want), wantCount)
	}

	got := AllCapabilityIDs()
	if len(got) != wantCount {
		t.Fatalf("AllCapabilityIDs() returned %d entries, want %d — a CapabilityID constant "+
			"was added/removed without updating AllCapabilityIDs", len(got), wantCount)
	}

	seen := make(map[CapabilityID]bool, len(got))
	for _, c := range got {
		if seen[c] {
			t.Errorf("AllCapabilityIDs() contains duplicate %q", c)
		}
		seen[c] = true
	}

	for _, w := range want {
		if !seen[w] {
			t.Errorf("AllCapabilityIDs() missing %q", w)
		}
	}
}
