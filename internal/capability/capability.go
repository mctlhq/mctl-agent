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
	"context"
	"fmt"
	"time"

	"github.com/mctlhq/mctl-agent/internal/fixer"
	"github.com/mctlhq/mctl-agent/internal/mctlclient"
	"github.com/mctlhq/mctl-agent/internal/notify"
	"github.com/mctlhq/mctl-agent/internal/skill"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

// Provider gives skills access to platform capabilities.
// Skills don't call mctlclient/fixer/telegram directly — they go through this.
type Provider struct {
	apiClient *mctlclient.Client
	github    *fixer.GitHubFixer
	telegram  *notify.Telegram
	store     *ticket.Store
}

// NewProvider creates a capability provider with all platform integrations.
func NewProvider(
	apiClient *mctlclient.Client,
	github *fixer.GitHubFixer,
	telegram *notify.Telegram,
	store *ticket.Store,
) *Provider {
	return &Provider{
		apiClient: apiClient,
		github:    github,
		telegram:  telegram,
		store:     store,
	}
}

// --- Read capabilities ---

// GetServiceStatus returns ArgoCD status for a service.
func (p *Provider) GetServiceStatus(team, service string) (*mctlclient.StatusResponse, error) {
	return p.apiClient.GetServiceStatus(team, service)
}

// GetServiceConfig returns the GitOps config for a service.
func (p *Provider) GetServiceConfig(team, service string) (*mctlclient.Service, error) {
	return p.apiClient.GetServiceConfig(team, service)
}

// GetServiceLogs returns recent logs for a service.
func (p *Provider) GetServiceLogs(team, service string, lines int, since time.Duration) (*mctlclient.LogsResponse, error) {
	return p.apiClient.GetServiceLogs(team, service, lines, since)
}

// GetResourceUsage returns resource quota usage for a tenant.
func (p *Provider) GetResourceUsage(tenant string) (*mctlclient.ResourceUsage, error) {
	return p.apiClient.GetResourceUsage(tenant)
}

// ListAudit returns recent audit log entries.
func (p *Provider) ListAudit() ([]mctlclient.AuditEntry, error) {
	return p.apiClient.ListAudit()
}

// --- Write capabilities ---

// GetFileContent fetches a file from the GitOps repo.
func (p *Provider) GetFileContent(ctx context.Context, path, ref string) (string, error) {
	return p.github.GetFileContent(ctx, path, ref)
}

// CreatePR creates a pull request in the GitOps repo.
func (p *Provider) CreatePR(ctx context.Context, req fixer.PRRequest) (string, int, error) {
	return p.github.CreatePR(ctx, req)
}

// --- Notification capabilities ---

// SendNotification sends a text message via Telegram.
func (p *Provider) SendNotification(text string) error {
	return p.telegram.SendText(text)
}

// --- Sandboxed context ---

// Context provides a capability-restricted view for a specific skill.
// Skills receive this instead of the raw Provider.
type Context struct {
	provider  *Provider
	skillName string
	allowed   map[skill.CapabilityID]bool
	Ticket    *ticket.Ticket
	Evidence  skill.EvidenceSet
}

// NewContext creates a sandboxed context for a skill, granting only the requested capabilities.
func NewContext(provider *Provider, s skill.Skill, t *ticket.Ticket, ev skill.EvidenceSet) *Context {
	allowed := make(map[skill.CapabilityID]bool, len(s.RequiredCapabilities()))
	for _, cap := range s.RequiredCapabilities() {
		allowed[cap] = true
	}
	return &Context{
		provider:  provider,
		skillName: s.Name(),
		allowed:   allowed,
		Ticket:    t,
		Evidence:  ev,
	}
}

func (c *Context) check(cap skill.CapabilityID) error {
	if !c.allowed[cap] {
		return fmt.Errorf("skill %q lacks capability %s", c.skillName, cap)
	}
	return nil
}

// GetServiceStatus returns ArgoCD status (requires CapReadStatus).
func (c *Context) GetServiceStatus(team, service string) (*mctlclient.StatusResponse, error) {
	if err := c.check(skill.CapReadStatus); err != nil {
		return nil, err
	}
	return c.provider.GetServiceStatus(team, service)
}

// GetServiceLogs returns service logs (requires CapReadLogs).
func (c *Context) GetServiceLogs(team, service string, lines int, since time.Duration) (*mctlclient.LogsResponse, error) {
	if err := c.check(skill.CapReadLogs); err != nil {
		return nil, err
	}
	return c.provider.GetServiceLogs(team, service, lines, since)
}

// GetResourceUsage returns resource usage (requires CapReadResources).
func (c *Context) GetResourceUsage(tenant string) (*mctlclient.ResourceUsage, error) {
	if err := c.check(skill.CapReadResources); err != nil {
		return nil, err
	}
	return c.provider.GetResourceUsage(tenant)
}

// GetServiceConfig returns config (requires CapReadConfig).
func (c *Context) GetServiceConfig(team, service string) (*mctlclient.Service, error) {
	if err := c.check(skill.CapReadConfig); err != nil {
		return nil, err
	}
	return c.provider.GetServiceConfig(team, service)
}

// ListAudit returns audit entries (requires CapReadAudit).
func (c *Context) ListAudit() ([]mctlclient.AuditEntry, error) {
	if err := c.check(skill.CapReadAudit); err != nil {
		return nil, err
	}
	return c.provider.ListAudit()
}

// GetFileContent reads a GitOps file (requires CapModifyGitOps).
func (c *Context) GetFileContent(ctx context.Context, path, ref string) (string, error) {
	if err := c.check(skill.CapModifyGitOps); err != nil {
		return "", err
	}
	return c.provider.GetFileContent(ctx, path, ref)
}

// CreatePR creates a PR (requires CapCreatePR).
func (c *Context) CreatePR(ctx context.Context, req fixer.PRRequest) (string, int, error) {
	if err := c.check(skill.CapCreatePR); err != nil {
		return "", 0, err
	}
	return c.provider.CreatePR(ctx, req)
}

// SendNotification sends a message (requires CapSendNotify).
func (c *Context) SendNotification(text string) error {
	if err := c.check(skill.CapSendNotify); err != nil {
		return err
	}
	return c.provider.SendNotification(text)
}
