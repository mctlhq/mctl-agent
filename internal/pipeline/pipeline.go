package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"

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
	provider  *capability.Provider
	apiClient *mctlclient.Client
	github    *fixer.GitHubFixer
	telegram  *notify.Telegram
	dryRun    bool
	paused    atomic.Bool
	sem       chan struct{}
}

// NewPipeline creates a new pipeline orchestrator.
func NewPipeline(
	store *ticket.Store,
	registry *skill.Registry,
	provider *capability.Provider,
	apiClient *mctlclient.Client,
	github *fixer.GitHubFixer,
	telegram *notify.Telegram,
	dryRun bool,
) *Pipeline {
	return &Pipeline{
		store:     store,
		registry:  registry,
		provider:  provider,
		apiClient: apiClient,
		github:    github,
		telegram:  telegram,
		dryRun:    dryRun,
		sem:       make(chan struct{}, 3), // Max 3 concurrent analyses.
	}
}

// Pause stops processing new tickets.
func (p *Pipeline) Pause()  { p.paused.Store(true) }

// Resume restarts processing.
func (p *Pipeline) Resume() { p.paused.Store(false) }

// IsPaused returns whether processing is paused.
func (p *Pipeline) IsPaused() bool { return p.paused.Load() }

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

	// Update status to analyzing.
	t.Status = ticket.StatusAnalyzing
	if err := p.store.Update(t); err != nil {
		log.Error("failed to update ticket status", "error", err)
		return
	}

	// Collect evidence.
	p.collectEvidence(ctx, t)

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

		diag, err := rs.Skill.Diagnose(ctx, t, ev)
		if err != nil {
			log.Warn("skill diagnosis failed, trying next", "skill", rs.Skill.Name(), "error", err)
			continue
		}

		// Update ticket with analysis.
		t.Analysis = diag.Diagnosis
		t.Confidence = diag.Confidence

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

		// MEDIUM confidence + fixable → suggest in Telegram.
		if diag.Confidence == ticket.ConfidenceMedium && diag.Fixable {
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
	fixResult, err := s.Fix(ctx, t, diag)
	if err != nil || fixResult == nil {
		log.Warn("skill fix generation failed", "skill", s.Name(), "error", err)
		_ = p.telegram.SendDiagnosis(t, diag.Diagnosis, diag.Confidence,
			"Fix identified but generation failed: "+fmt.Sprint(err))
		t.Status = ticket.StatusFixProposed
		_ = p.store.Update(t)
		return
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

	if prURL != "" {
		_ = p.telegram.SendPRCreated(t, prURL, summary)
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
