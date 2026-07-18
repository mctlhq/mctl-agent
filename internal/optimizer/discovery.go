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
	"context"
	"fmt"
	"log/slog"
	"strings"

	"gopkg.in/yaml.v3"
)

const servicesRoot = "platform-gitops/services"

// GitOpsReader is the subset of fixer.GitHubFixer needed for discovery.
type GitOpsReader interface {
	ListDir(ctx context.Context, path, ref string) ([]string, error)
	GetFileContent(ctx context.Context, path, ref string) (string, error)
}

// ServiceSpec is a candidate workload discovered from the gitops repo.
type ServiceSpec struct {
	Tenant     string `json:"tenant"`
	Service    string `json:"service"`
	FilePath   string `json:"file_path"`
	RawValues  string `json:"-"`
	ImageTag   string `json:"image_tag"`
	CPURequest string `json:"cpu_request"`
	MemRequest string `json:"mem_request"`
	CPULimit   string `json:"cpu_limit"`
	MemLimit   string `json:"mem_limit"`

	ReplicaCount       int  `json:"replica_count"`
	BlueGreen          bool `json:"blue_green"`
	Autoscaling        bool `json:"autoscaling"`
	Persistence        bool `json:"persistence"`
	HasExtraContainers bool `json:"has_extra_containers"`
	HasInitContainers  bool `json:"has_init_containers"`
}

// valuesDoc is a read-only view of a base-service values.yaml. Writing back
// is done by string surgery elsewhere — never by re-marshalling this struct.
type valuesDoc struct {
	Image struct {
		Tag string `yaml:"tag"`
	} `yaml:"image"`
	ReplicaCount *int `yaml:"replicaCount"`
	Resources    struct {
		Requests map[string]any `yaml:"requests"`
		Limits   map[string]any `yaml:"limits"`
	} `yaml:"resources"`
	BlueGreen struct {
		Enabled bool `yaml:"enabled"`
	} `yaml:"blueGreen"`
	Autoscaling struct {
		Enabled bool `yaml:"enabled"`
	} `yaml:"autoscaling"`
	Persistence struct {
		Enabled bool `yaml:"enabled"`
	} `yaml:"persistence"`
	ExtraContainers []any `yaml:"extraContainers"`
	InitContainers  []any `yaml:"initContainers"`
}

// DiscoverServices lists every services/<tenant>/<service>/values.yaml on
// main and parses the fields the optimizer cares about. Individual broken
// files are skipped with a log line, not fatal.
func DiscoverServices(ctx context.Context, gh GitOpsReader) ([]ServiceSpec, error) {
	tenants, err := gh.ListDir(ctx, servicesRoot, "main")
	if err != nil {
		return nil, fmt.Errorf("listing tenants: %w", err)
	}

	var out []ServiceSpec
	for _, tenant := range tenants {
		services, err := gh.ListDir(ctx, servicesRoot+"/"+tenant, "main")
		if err != nil {
			slog.Warn("optimizer: listing tenant services failed", "tenant", tenant, "error", err)
			continue
		}
		for _, service := range services {
			path := fmt.Sprintf("%s/%s/%s/values.yaml", servicesRoot, tenant, service)
			raw, err := gh.GetFileContent(ctx, path, "main")
			if err != nil {
				slog.Warn("optimizer: fetching values.yaml failed", "path", path, "error", err)
				continue
			}
			spec, err := parseServiceSpec(tenant, service, path, raw)
			if err != nil {
				slog.Warn("optimizer: parsing values.yaml failed", "path", path, "error", err)
				continue
			}
			out = append(out, spec)
		}
	}
	return out, nil
}

func parseServiceSpec(tenant, service, path, raw string) (ServiceSpec, error) {
	var doc valuesDoc
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		return ServiceSpec{}, err
	}

	replicas := 1
	if doc.ReplicaCount != nil {
		replicas = *doc.ReplicaCount
	}

	return ServiceSpec{
		Tenant:             tenant,
		Service:            service,
		FilePath:           path,
		RawValues:          raw,
		ImageTag:           doc.Image.Tag,
		CPURequest:         quantityString(doc.Resources.Requests["cpu"]),
		MemRequest:         quantityString(doc.Resources.Requests["memory"]),
		CPULimit:           quantityString(doc.Resources.Limits["cpu"]),
		MemLimit:           quantityString(doc.Resources.Limits["memory"]),
		ReplicaCount:       replicas,
		BlueGreen:          doc.BlueGreen.Enabled,
		Autoscaling:        doc.Autoscaling.Enabled,
		Persistence:        doc.Persistence.Enabled,
		HasExtraContainers: len(doc.ExtraContainers) > 0,
		HasInitContainers:  len(doc.InitContainers) > 0,
	}, nil
}

// quantityString renders a YAML scalar resource quantity ("500m", 1, 0.5)
// as the string Kubernetes would parse.
func quantityString(v any) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	default:
		return strings.TrimSpace(fmt.Sprintf("%v", t))
	}
}
