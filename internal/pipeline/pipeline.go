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

package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/mctlhq/mctl-agent/internal/capability"
	"github.com/mctlhq/mctl-agent/internal/fixer"
	"github.com/mctlhq/mctl-agent/internal/mctlclient"
	"github.com/mctlhq/mctl-agent/internal/notify"
	"github.com/mctlhq/mctl-agent/internal/skill"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

// Pipeline orchestrates the ticket → evidence → skill match → diagnosis → fix → notify flow.
type Pipeline struct {
	store     *ticket.Store
	registry  *skill.Registry
	metrics   *skill.Metrics
	provider  *capability.Provider
	apiClient *mctlclient.Client
	github    *fixer.GitHubFixer
	telegram  *notify.Telegram
	dryRun        bool
	autoMerge     bool
	escalationTag string
	paused        atomic.Bool
	sem           chan struct{}
}

// NewPipeline creates a new pipeline orchestrator.
func NewPipeline(
	store *ticket.Store,
	registry *skill.Registry,
	metrics *skill.Metrics,
	provider *capability.Provider,
	apiClient *mctlclient.Client,
	github *fixer.GitHubFixer,
	telegram *notify.Telegram,
	dryRun bool,
	autoMerge bool,
	escalationTag string,
) *Pipeline {
	return &Pipeline{
		store:         store,
		registry:      registry,
		metrics:       metrics,
		provider:      provider,
		apiClient:     apiClient,
		github:        github,
		telegram:      telegram,
		dryRun:        dryRun,
		autoMerge:     autoMerge,
		escalationTag: escalationTag,
		sem:           make(chan struct{}, 3), // Max 3 concurrent analyses.
	}
}

// Metrics returns the pipeline's metrics tracker (can be nil).
func (p *Pipeline) Metrics() *skill.Metrics { return p.metrics }

// Registry returns the pipeline's skill registry.
func (p *Pipeline) Registry() *skill.Registry { return p.registry }

// Pause stops processing new tickets.
func (p *Pipeline) Pause()  { p.paused.Store(true) }

// Resume restarts processing.
func (p *Pipeline) Resume() { p.paused.Store(false) }

// IsPaused returns whether processing is paused.
func (p *Pipeline) IsPaused() bool { return p.paused.Load() }

// TriggerAnalysis creates a synthetic ticket for the given team/service and
// processes it through the pipeline. Used for manual MCP-triggered analysis.
func (p *Pipeline) TriggerAnalysis(ctx context.Context, team, service, reason string) (*ticket.Ticket, error) {
	t := &ticket.Ticket{
		Source:   ticket.SourceManual,
		Type:     ticket.TypeArgoCDDegraded,
		Tenant:   team,
		Service:  service,
		Summary:  reason,
		Severity: "info",
		Status:   ticket.StatusOpen,
	}

	if err := p.store.Create(t); err != nil {
		return nil, fmt.Errorf("creating ticket: %w", err)
	}

	// Process asynchronously.
	p.ProcessTicket(t)

	return t, nil
}

// ProcessTicket runs the full pipeline for a single ticket.
func (p *Pipeline) ProcessTicket(t *ticket.Ticket) {
	if p.paused.Load() {
		slog.Info("pipeline paused, skipping ticket", "id", t.ID)
		return
	}

	// Acquire semaphore.
	p.sem <- struct{}{}
	go func() {
		defer func() { <-p.sem }()
		p.processTicketSync(context.Background(), t)
	}()
}

func (p *Pipeline) processTicketSync(ctx context.Context, t *ticket.Ticket) {
	log := slog.With("ticket", t.ID, "service", t.Tenant+"/"+t.Service)

	// Notify about new ticket.
	if err := p.telegram.SendNewTicket(t); err != nil {
		log.Error("failed to send new ticket notification", "error", err)
	}

	// Publish to mctl-api alert store.
	go p.apiClient.PublishAlert(t)

	// Update status to analyzing.
	t.Status = ticket.StatusAnalyzing
	if err := p.store.Update(t); err != nil {
		log.Error("failed to update ticket status", "error", err)
		return
	}

	// Collect evidence.
	p.collectEvidence(ctx, t)
	p.collectHistoricalEvidence(t)

	// Reload ticket with evidence.
	t, err := p.store.Get(t.ID)
	if err != nil {
		log.Error("failed to reload ticket", "error", err)
		return
	}

	ev := skill.NewEvidenceSet(t.Evidence)

	// Match skills.
	ranked := p.registry.Match(ctx, t, ev)
	if len(ranked) == 0 {
		log.Info("no skills matched ticket")
		_ = p.telegram.SendDiagnosis(t, "No skill matched this ticket type. Manual review required.", ticket.ConfidenceLow, "No auto-diagnosis available")
		_ = p.store.Update(t)
		return
	}

	// Try skills in ranked order.
	for _, rs := range ranked {
		log.Info("running skill",
			"skill", rs.Skill.Name(),
			"confidence", rs.Result.Confidence,
			"reason", rs.Result.Reason)

		// Record match.
		if p.metrics != nil {
			p.metrics.RecordMatch(rs.Skill.Name(), t.ID)
		}

		diagStart := time.Now()
		diag, err := rs.Skill.Diagnose(ctx, t, ev)
		diagDur := time.Since(diagStart)
		if err != nil {
			if p.metrics != nil {
				p.metrics.RecordDiagnosis(rs.Skill.Name(), t.ID, false, diagDur, err.Error())
			}
			log.Warn("skill diagnosis failed, trying next", "skill", rs.Skill.Name(), "error", err)
			continue
		}
		if p.metrics != nil {
			p.metrics.RecordDiagnosis(rs.Skill.Name(), t.ID, true, diagDur, diag.Diagnosis)
		}

		// Update ticket with analysis.
		t.Analysis = diag.Diagnosis
		t.Confidence = diag.Confidence

		// Sync diagnosis to mctl-api.
		go p.apiClient.UpdateAlert(t)

		log.Info("diagnosis complete",
			"skill", rs.Skill.Name(),
			"confidence", diag.Confidence,
			"fixable", diag.Fixable)

		// Infrastructure alerts — never auto-fix.
		if isInfraAlert(t) {
			_ = p.telegram.SendDiagnosis(t, diag.Diagnosis, diag.Confidence, "Infrastructure alert — manual review only")
			_ = p.store.Update(t)
			return
		}

		// HIGH confidence + fixable → create PR.
		if diag.Confidence == ticket.ConfidenceHigh && diag.Fixable {
			p.handleHighConfidenceFix(ctx, t, rs.Skill, diag, log)
			return
		}

		// MEDIUM confidence + fixable + auto-merge safe → create PR (same as HIGH).
		if diag.Confidence == ticket.ConfidenceMedium && diag.Fixable {
			if am, ok := rs.Skill.(skill.AutoMerger); ok && am.AutoMergeSafe() {
				p.handleHighConfidenceFix(ctx, t, rs.Skill, diag, log)
				return
			}
			// Not auto-merge safe → suggest in Telegram.
			action := fmt.Sprintf("Fixable but MEDIUM confidence (skill: %s) — sending suggestion to Telegram for review", rs.Skill.Name())
			_ = p.telegram.SendDiagnosis(t, diag.Diagnosis, diag.Confidence, action)
			t.Status = ticket.StatusFixProposed
			_ = p.store.Update(t)
			return
		}

		// LOW / not fixable → notify and stop.
		_ = p.telegram.SendDiagnosis(t, diag.Diagnosis, diag.Confidence, fmt.Sprintf("No auto-fix available (skill: %s)", rs.Skill.Name()))
		_ = p.store.Update(t)
		return
	}

	// All skills failed.
	log.Warn("all matched skills failed to diagnose")
	_ = p.telegram.SendDiagnosis(t, "All matched skills failed to produce a diagnosis. Manual review required.", ticket.ConfidenceLow, "Manual review required")
	_ = p.store.Update(t)
}

func (p *Pipeline) handleHighConfidenceFix(ctx context.Context, t *ticket.Ticket, s skill.Skill, diag *skill.DiagnosisResult, log *slog.Logger) {
	fixStart := time.Now()
	fixResult, err := s.Fix(ctx, t, diag)
	fixDur := time.Since(fixStart)
	if err != nil || fixResult == nil {
		if p.metrics != nil {
			detail := ""
			if err != nil {
				detail = err.Error()
			}
			p.metrics.RecordFix(s.Name(), t.ID, false, fixDur, detail)
		}
		log.Warn("skill fix generation failed", "skill", s.Name(), "error", err)
		_ = p.telegram.SendDiagnosis(t, diag.Diagnosis, diag.Confidence,
			"Fix identified but generation failed: "+fmt.Sprint(err))
		t.Status = ticket.StatusFixProposed
		_ = p.store.Update(t)
		return
	}
	if !fixResult.Applied {
		if p.metrics != nil {
			p.metrics.RecordFix(s.Name(), t.ID, false, fixDur, "fix not applied by skill")
		}
		log.Info("skill returned fix with Applied=false, skipping PR", "skill", s.Name())
		_ = p.telegram.SendDiagnosis(t, diag.Diagnosis, diag.Confidence,
			fmt.Sprintf("Skill %s declined to apply fix: %s", s.Name(), fixResult.Summary))
		_ = p.store.Update(t)
		return
	}
	if p.metrics != nil {
		p.metrics.RecordFix(s.Name(), t.ID, true, fixDur, fixResult.Summary)
	}

	filePath := fixResult.FilePath
	if filePath == "" {
		filePath = fixer.DetectFilePath(t.Tenant, t.Service)
	}

	// Get current file content from GitOps repo.
	content, err := p.github.GetFileContent(ctx, filePath, "main")
	if err != nil {
		log.Error("failed to get file content", "path", filePath, "error", err)
		_ = p.telegram.SendDiagnosis(t, diag.Diagnosis, diag.Confidence,
			fmt.Sprintf("Could not read %s: %v", filePath, err))
		_ = p.store.Update(t)
		return
	}

	// Generate the actual patch based on fix type.
	var newContent, summary string
	var patchErr error

	switch diag.FixType {
	case "bump_memory":
		newContent, summary, patchErr = fixer.GenerateMemoryBump(content)
	case "bump_cpu":
		newContent, summary, patchErr = fixer.GenerateCPUBump(content)
	case "adjust_probe":
		newContent, summary, patchErr = fixer.GenerateProbeFix(content, diag.YAMLField)
	case "fix_workflow_params":
		newContent, summary, patchErr = fixer.GenerateWorkflowParamFix(content)
	case "fix_appproject_whitelist":
		newContent, summary, patchErr = fixer.GenerateAppProjectWhitelistFix(content)
	case "rollback_image":
		patchErr = fmt.Errorf("image rollback requires previous tag — not yet implemented")
	default:
		// For LLM-generated fixes, try to apply from diagnosis fields.
		if diag.CurrentValue != "" && diag.SuggestedValue != "" {
			newContent, summary, patchErr = fixer.GenerateFromDiagnosis(content, toLegacyDiag(diag))
		} else if fixResult.NewContent != "" {
			newContent = fixResult.NewContent
			summary = fixResult.Summary
		} else {
			patchErr = fmt.Errorf("no applicable fix strategy for type: %s", diag.FixType)
		}
	}

	if patchErr != nil {
		log.Warn("patch generation failed", "error", patchErr)
		_ = p.telegram.SendDiagnosis(t, diag.Diagnosis, diag.Confidence,
			"Fix identified but patch generation failed: "+patchErr.Error())
		t.Status = ticket.StatusFixProposed
		_ = p.store.Update(t)
		return
	}

	t.ProposedFix = summary

	// Create PR.
	prURL, prNumber, err := p.github.CreatePR(ctx, fixer.PRRequest{
		Ticket:     t,
		FilePath:   filePath,
		NewContent: newContent,
		Summary:    summary,
		Diagnosis:  diag.Diagnosis,
		Confidence: diag.Confidence,
	})
	if err != nil {
		log.Error("failed to create PR", "error", err)
		_ = p.telegram.SendDiagnosis(t, diag.Diagnosis, diag.Confidence,
			"PR creation failed: "+err.Error())
		_ = p.store.Update(t)
		return
	}

	t.PRURL = prURL
	t.PRNumber = prNumber
	t.Status = ticket.StatusFixProposed
	_ = p.store.Update(t)

	// Sync PR info to mctl-api.
	go p.apiClient.UpdateAlert(t)

	if prURL != "" {
		shouldAutoMerge := p.autoMerge && !p.dryRun
		if shouldAutoMerge {
			if am, ok := s.(skill.AutoMerger); !ok || !am.AutoMergeSafe() {
				shouldAutoMerge = false
			}
		}

		if shouldAutoMerge {
			if err := p.github.MergePR(ctx, prNumber); err != nil {
				log.Error("auto-merge failed", "error", err)
				_ = p.telegram.SendPRNeedsReview(t, prURL, summary, p.escalationTag,
					"Auto-merge failed: "+err.Error())
			} else {
				t.Status = ticket.StatusFixApplied
				_ = p.store.Update(t)
				go p.apiClient.UpdateAlert(t)
				_ = p.telegram.SendPRAutoMerged(t, prURL, summary)
			}
		} else {
			_ = p.telegram.SendPRNeedsReview(t, prURL, summary, p.escalationTag, "")
		}
	} else {
		// Dry-run mode.
		_ = p.telegram.SendDiagnosis(t, diag.Diagnosis, diag.Confidence,
			"[DRY-RUN] Would create PR: "+summary)
	}
}

func isInfraAlert(t *ticket.Ticket) bool {
	if t.Type != ticket.TypeResourceLimit {
		return false
	}
	return t.Service == ""
}

// toLegacyDiag converts a skill.DiagnosisResult to the legacy diagnosis format
// needed by fixer.GenerateFromDiagnosis.
func toLegacyDiag(d *skill.DiagnosisResult) *legacyDiag {
	return &legacyDiag{
		Diagnosis:      d.Diagnosis,
		Confidence:     d.Confidence,
		Fixable:        d.Fixable,
		YAMLPath:       d.YAMLPath,
		YAMLField:      d.YAMLField,
		CurrentValue:   d.CurrentValue,
		SuggestedValue: d.SuggestedValue,
		Reasoning:      d.Reasoning,
	}
}

// legacyDiag mirrors diagnosis.DiagnosisResult for backward compatibility with fixer.
type legacyDiag = fixer.DiagnosisCompat
