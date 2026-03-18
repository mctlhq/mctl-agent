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

// Package runbook provides per-ticket-type runbooks embedded at build time.
// Runbooks are injected into LLM prompts to guide diagnosis toward documented procedures.
package runbook

import _ "embed"

//go:embed data/pod_crashloop.md
var podCrashloop string

//go:embed data/resource_limit.md
var resourceLimit string

//go:embed data/workflow_failed.md
var workflowFailed string

//go:embed data/argocd_app_degraded.md
var argoCDDegraded string

//go:embed data/generic.md
var generic string

// Get returns the runbook for the given ticket type, or the generic runbook if unknown.
func Get(ticketType string) string {
	switch ticketType {
	case "pod_crashloop":
		return podCrashloop
	case "resource_limit":
		return resourceLimit
	case "workflow_failed":
		return workflowFailed
	case "argocd_app_degraded":
		return argoCDDegraded
	default:
		return generic
	}
}
