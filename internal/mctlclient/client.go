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

package mctlclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/mctlhq/mctl-agent/internal/ticket"
)

// Client communicates with the mctl-api REST API.
type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

// NewClient creates a new mctl-api client.
func NewClient(baseURL, token string) *Client {
	return &Client{
		baseURL: baseURL,
		token:   token,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

// Service represents a service from mctl-api.
type Service struct {
	Team string `json:"team"`
	App  string `json:"app"`
	Name string `json:"name"`
}

// StatusResponse is the response from GET /api/v1/status/{team}/{app}.
type StatusResponse struct {
	ArgoCD  *ArgoStatus `json:"argocd"`
	Service *Service    `json:"service"`
}

// ArgoStatus contains ArgoCD application status fields.
type ArgoStatus struct {
	Name       string `json:"name"`
	Health     string `json:"health"`
	SyncStatus string `json:"syncStatus"`
	Revision   string `json:"revision"`
	Message    string `json:"message"`
	Namespace  string `json:"namespace"`
	Project    string `json:"project"`
}

// LogLine represents a single log line.
type LogLine struct {
	Timestamp string `json:"timestamp"`
	Line      string `json:"line"`
}

// LogsResponse is the response from GET /api/v1/logs/{team}/{app}.
type LogsResponse struct {
	Team  string    `json:"team"`
	App   string    `json:"app"`
	Lines []LogLine `json:"lines"`
	Count int       `json:"count"`
}

// ResourceUsage is the response from GET /api/v1/resources/{tenant}.
type ResourceUsage struct {
	Tenant    string            `json:"tenant"`
	Allocated map[string]string `json:"allocated"`
	Used      map[string]string `json:"used"`
}

// AuditEntry represents an audit log entry.
type AuditEntry struct {
	User      string    `json:"user"`
	Action    string    `json:"action"`
	Target    string    `json:"target"`
	Timestamp time.Time `json:"timestamp"`
}

// ListServices returns all services.
func (c *Client) ListServices() ([]Service, error) {
	body, err := c.doGet("/api/v1/services")
	if err != nil {
		return nil, err
	}
	var resp struct {
		Items []Service `json:"items"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parsing services: %w", err)
	}
	return resp.Items, nil
}

// GetServiceStatus returns the ArgoCD status for a service.
func (c *Client) GetServiceStatus(team, app string) (*StatusResponse, error) {
	body, err := c.doGet(fmt.Sprintf("/api/v1/status/%s/%s", team, app))
	if err != nil {
		return nil, err
	}
	var resp StatusResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parsing status: %w", err)
	}
	return &resp, nil
}

// GetServiceConfig returns the gitops config for a service.
func (c *Client) GetServiceConfig(team, app string) (*Service, error) {
	body, err := c.doGet(fmt.Sprintf("/api/v1/services/%s/%s", team, app))
	if err != nil {
		return nil, err
	}
	var svc Service
	if err := json.Unmarshal(body, &svc); err != nil {
		return nil, fmt.Errorf("parsing service config: %w", err)
	}
	return &svc, nil
}

// GetServiceLogs returns recent logs for a service.
func (c *Client) GetServiceLogs(team, app string, lines int, since time.Duration) (*LogsResponse, error) {
	path := fmt.Sprintf("/api/v1/logs/%s/%s?lines=%d&since=%s", team, app, lines, since)
	body, err := c.doGet(path)
	if err != nil {
		return nil, err
	}
	var resp LogsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parsing logs: %w", err)
	}
	return &resp, nil
}

// GetResourceUsage returns resource usage for a tenant.
func (c *Client) GetResourceUsage(tenant string) (*ResourceUsage, error) {
	body, err := c.doGet(fmt.Sprintf("/api/v1/resources/%s", tenant))
	if err != nil {
		return nil, err
	}
	var resp ResourceUsage
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parsing resources: %w", err)
	}
	return &resp, nil
}

// ListAudit returns recent audit entries.
func (c *Client) ListAudit() ([]AuditEntry, error) {
	body, err := c.doGet("/api/v1/audit")
	if err != nil {
		return nil, err
	}
	var resp struct {
		Items []AuditEntry `json:"items"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parsing audit: %w", err)
	}
	return resp.Items, nil
}

// GetWorkflow returns live workflow status.
func (c *Client) GetWorkflow(name string) (map[string]any, error) {
	body, err := c.doGet(fmt.Sprintf("/api/v1/workflows/%s", name))
	if err != nil {
		return nil, err
	}
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parsing workflow: %w", err)
	}
	return resp, nil
}

// PublishAlert sends a new incident to mctl-api.
func (c *Client) PublishAlert(t *ticket.Ticket) {
	body := map[string]interface{}{
		"id":            t.ID,
		"source":        t.Source,
		"type":          t.Type,
		"tenant":        t.Tenant,
		"service":       t.Service,
		"summary":       t.Summary,
		"severity":      t.Severity,
		"status":        t.Status,
		"analysis":      t.Analysis,
		"proposed_fix":  t.ProposedFix,
		"pr_url":        t.PRURL,
		"pr_number":     t.PRNumber,
		"pr_repo":       t.PRRepo,
		"pr_branch":     t.PRBranch,
		"pr_commit_sha": t.PRCommitSHA,
		"confidence":    t.Confidence,
		"created_at":    t.CreatedAt,
		"updated_at":    t.UpdatedAt,
	}
	if t.ResolvedAt != nil {
		body["resolved_at"] = t.ResolvedAt
	}

	var evidence []map[string]interface{}
	for _, ev := range t.Evidence {
		evidence = append(evidence, map[string]interface{}{
			"type":         ev.Type,
			"content":      ev.Content,
			"collected_at": ev.CollectedAt,
		})
	}
	if len(evidence) > 0 {
		body["evidence"] = evidence
	}

	if err := c.doPost("/api/v1/incidents", body); err != nil {
		slog.Error("failed to publish alert to mctl-api", "id", t.ID, "error", err)
	}
}

// UpdateAlert sends an incident update to mctl-api.
func (c *Client) UpdateAlert(t *ticket.Ticket) {
	body := map[string]interface{}{
		"status":        t.Status,
		"analysis":      t.Analysis,
		"proposed_fix":  t.ProposedFix,
		"pr_url":        t.PRURL,
		"pr_number":     t.PRNumber,
		"pr_repo":       t.PRRepo,
		"pr_branch":     t.PRBranch,
		"pr_commit_sha": t.PRCommitSHA,
		"confidence":    t.Confidence,
	}

	if err := c.doPatch("/api/v1/incidents/"+t.ID, body); err != nil {
		slog.Error("failed to update alert in mctl-api", "id", t.ID, "error", err)
	}
}

func (c *Client) doPost(path string, body interface{}) error {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshaling body: %w", err)
	}

	req, err := http.NewRequest("POST", c.baseURL+path, bytes.NewReader(jsonBody))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("mctl-api request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("mctl-api returned %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

func (c *Client) doPatch(path string, body interface{}) error {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshaling body: %w", err)
	}

	req, err := http.NewRequest("PATCH", c.baseURL+path, bytes.NewReader(jsonBody))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("mctl-api request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("mctl-api returned %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

func (c *Client) doGet(path string) ([]byte, error) {
	req, err := http.NewRequest("GET", c.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mctl-api request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mctl-api returned %d: %s", resp.StatusCode, string(body))
	}

	return body, nil
}
