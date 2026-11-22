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

package capability

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"testing"

	"github.com/mctlhq/mctl-agent/internal/skill"
)

// auditRow is one line of the declared-vs-used audit table.
//
// "Declared" means the capability is a CapabilityID constant in
// internal/skill/skill.go. "Used" means a method of capability.Context in
// capability.go guards an operation with c.check(skill.CapX).
//
// This is an audit, not enforcement: the test pins today's picture so that a
// capability added, removed or newly wired shows up as a failing row that a
// human must classify. See docs/capability-audit.md.
type auditRow struct {
	constName string
	id        skill.CapabilityID
	used      bool
}

var auditTable = []auditRow{
	{"CapReadLogs", skill.CapReadLogs, true},
	{"CapReadConfig", skill.CapReadConfig, true},
	{"CapReadStatus", skill.CapReadStatus, true},
	{"CapReadResources", skill.CapReadResources, true},
	{"CapReadAudit", skill.CapReadAudit, true},
	{"CapModifyGitOps", skill.CapModifyGitOps, true},
	{"CapCreatePR", skill.CapCreatePR, true},
	{"CapSendNotify", skill.CapSendNotify, true},
	// Declared but no capability.Context method checks them.
	{"CapMergePR", skill.CapMergePR, false},
	{"CapCallLLM", skill.CapCallLLM, false},
	{"CapExecWorkflow", skill.CapExecWorkflow, false},
}

func parseFile(t *testing.T, path string) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("could not parse %s: %v", path, err)
	}
	return f
}

// declaredConsts returns constName -> id for every CapabilityID constant in
// skill.go. Anything it cannot resolve is a hard failure, never an empty set.
func declaredConsts(t *testing.T, path string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, d := range parseFile(t, path).Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			ident, ok := vs.Type.(*ast.Ident)
			if !ok || ident.Name != "CapabilityID" {
				continue
			}
			for i, n := range vs.Names {
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("const %s: value is not a string literal, cannot audit", n.Name)
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("const %s: %v", n.Name, err)
				}
				out[n.Name] = v
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("found no CapabilityID constants in %s: parser or layout changed", path)
	}
	return out
}

// usedConsts returns the const names passed to c.check(...) in capability.go.
func usedConsts(t *testing.T, path string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	checks := 0
	ast.Inspect(parseFile(t, path), func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "check" {
			return true
		}
		checks++
		if len(call.Args) != 1 {
			t.Errorf("check call with %d args, cannot audit", len(call.Args))
			return true
		}
		arg, ok := call.Args[0].(*ast.SelectorExpr)
		if !ok {
			t.Errorf("check argument at %v is not skill.CapX, cannot audit", call.Pos())
			return true
		}
		if pkg, ok := arg.X.(*ast.Ident); !ok || pkg.Name != "skill" {
			t.Errorf("check argument %v is not from package skill", arg.Sel.Name)
			return true
		}
		out[arg.Sel.Name] = true
		return true
	})
	if checks == 0 {
		t.Fatalf("found no check() calls in %s: parser or layout changed", path)
	}
	return out
}

func TestCapabilityDeclaredVsUsedAudit(t *testing.T) {
	declared := declaredConsts(t, "../skill/skill.go")
	used := usedConsts(t, "capability.go")

	table := map[string]auditRow{}
	for _, r := range auditTable {
		if _, dup := table[r.constName]; dup {
			t.Fatalf("duplicate audit row %s", r.constName)
		}
		table[r.constName] = r
	}

	// Each table row must agree with the source of truth, row by row.
	for _, r := range auditTable {
		r := r
		t.Run(r.constName, func(t *testing.T) {
			id, isDeclared := declared[r.constName]
			if !isDeclared {
				t.Fatalf("%s is in the audit table but no longer declared in skill.go", r.constName)
			}
			if id != string(r.id) {
				t.Errorf("%s: skill.go value %q != table value %q", r.constName, id, r.id)
			}
			if got := used[r.constName]; got != r.used {
				t.Errorf("%s: used by capability.Context = %v, audit table says %v (update the table and docs/capability-audit.md)",
					r.constName, got, r.used)
			}
		})
	}

	// Declared without an audit row: new capability nobody classified.
	for _, name := range sortedKeys(declared) {
		if _, ok := table[name]; !ok {
			t.Errorf("%s is declared in skill.go but has no audit row (used or deliberately unused?)", name)
		}
	}
	// Used without declaration: check() on something not a declared constant.
	for name := range used {
		if _, ok := declared[name]; !ok {
			t.Errorf("%s is used in capability.go but not declared in skill.go", name)
		}
	}

	// AllCapabilityIDs (what remote registration validates against) must list
	// exactly the declared constants.
	listed := map[string]bool{}
	for _, id := range skill.AllCapabilityIDs() {
		listed[string(id)] = true
	}
	for _, name := range sortedKeys(declared) {
		if !listed[declared[name]] {
			t.Errorf("%s (%q) is declared but missing from AllCapabilityIDs()", name, declared[name])
		}
	}
	declaredValues := map[string]bool{}
	for _, v := range declared {
		declaredValues[v] = true
	}
	for id := range listed {
		if !declaredValues[id] {
			t.Errorf("AllCapabilityIDs() lists %q which is not a declared constant", id)
		}
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
