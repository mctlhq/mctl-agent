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

// Package svcname resolves the canonical registered app name for a
// pod-scoped ticket, deterministically and without any network call.
//
// For an app deployed through the platform's base-service Helm chart, the
// pod is named "{tenant}-{app}-base-service-<rs>-<id>". Stripping only the
// ReplicaSet hash and pod suffix (as internal/monitor.extractService does)
// leaves the chart's fullname, "{tenant}-{app}-base-service", instead of the
// registered app name — e.g. "labs-mctl-telegram-base-service" rather than
// "mctl-telegram". This package derives the ordered set of names a caller
// might mean by that value, most authoritative first, so the caller can pick
// the first one, or — where it can actually check which one exists, such as
// probing paths in mctl-gitops — try them in order.
//
// This package has no dependency on internal/mctlclient or internal/fixer so
// it can be unit tested with plain strings, the same design as
// internal/gitopspath.
package svcname

import "strings"

// ChartFullnameSuffixes are the Helm chart fullname suffixes appended to a
// release name. Seeded with the base-service chart only; add
// "-worker-service" here if that chart is ever found to use the same
// release-name-plus-suffix pattern.
var ChartFullnameSuffixes = []string{"-base-service"}

// IdentityLabels are the alert label keys, in preference order, that name
// the canonical app directly. kube_pod_labels exposes Kubernetes labels as
// "label_<sanitised>" series, so both the raw and "label_" spellings are
// accepted for each source label.
var IdentityLabels = []string{
	"backstage.io/kubernetes-id",
	"label_backstage_io_kubernetes_id",
	"app.kubernetes.io/instance",
	"label_app_kubernetes_io_instance",
}

// instanceLabels is the subset of IdentityLabels whose value is a Helm
// release name ("{tenant}-{app}") rather than the app name directly, so a
// leading "{namespace}-" is stripped from it the same way it is stripped
// from a pod-derived name below. "backstage.io/kubernetes-id" (and its
// "label_" spelling) name the app directly and are used as-is.
var instanceLabels = map[string]bool{
	"app.kubernetes.io/instance":       true,
	"label_app_kubernetes_io_instance": true,
}

// Candidates returns the ordered candidate app names for a pod-derived
// service name, most authoritative first, deduplicated, and never empty
// unless derived == "" (in which case it returns nil).
//
// Order:
//  1. An IdentityLabels hit, if labels carries one. For the
//     "app.kubernetes.io/instance" spellings the value is a release name, so
//     a leading "{namespace}-" is stripped; for "backstage.io/kubernetes-id"
//     the value is used as-is.
//  2. derived with a ChartFullnameSuffixes entry removed AND a leading
//     "{namespace}-" removed.
//  3. derived with only the suffix removed. Covers an app whose registered
//     name legitimately begins with the tenant name.
//  4. derived unchanged — always last, so today's behaviour is the floor.
//
// The "{namespace}-" strip in candidates 1 (instance spellings) and 2 is
// gated on there being a signature to strip: for candidate 2 that means a
// ChartFullnameSuffixes entry actually matched derived. Without that gate, a
// plain StatefulSet pod named "labs-something-0" in namespace "labs" would
// be silently renamed to "something". With it, only names carrying the
// chart's own fullname signature are touched, and any derived value that
// does not end in one of ChartFullnameSuffixes (e.g. "myapp-6d4b5c7f8-abc12"
// already reduced to "myapp", "two-parts", "a-b-c-d-e") falls straight
// through to candidate 4 unchanged.
func Candidates(namespace, derived string, labels map[string]string) []string {
	if derived == "" {
		return nil
	}

	var out []string
	seen := make(map[string]bool)
	add := func(s string) {
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}

	for _, key := range IdentityLabels {
		val := labels[key]
		if val == "" {
			continue
		}
		if instanceLabels[key] {
			val = stripNamespacePrefix(val, namespace)
		}
		add(val)
		break
	}

	for _, suffix := range ChartFullnameSuffixes {
		if !strings.HasSuffix(derived, suffix) {
			continue
		}
		stripped := strings.TrimSuffix(derived, suffix)
		add(stripNamespacePrefix(stripped, namespace))
		add(stripped)
		break
	}

	add(derived)

	return out
}

// stripNamespacePrefix removes a leading "{namespace}-" from s, if present.
// namespace == "" never strips anything.
func stripNamespacePrefix(s, namespace string) string {
	if namespace == "" {
		return s
	}
	prefix := namespace + "-"
	if strings.HasPrefix(s, prefix) {
		return s[len(prefix):]
	}
	return s
}

// Resolve returns the single most authoritative candidate name —
// Candidates(...)[0] — or "" when derived is empty. There is no network
// call and no registry lookup here: the check that matters, whether a
// candidate's GitOps file actually exists, happens later, when the pipeline
// probes candidate paths in mctl-gitops.
func Resolve(namespace, derived string, labels map[string]string) string {
	c := Candidates(namespace, derived, labels)
	if len(c) == 0 {
		return ""
	}
	return c[0]
}
