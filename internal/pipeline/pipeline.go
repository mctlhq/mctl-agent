package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/mctlhq/mctl-agent/internal/diagnosis"
	"github.com/mctlhq/mctl-agent/internal/fixer"
	"github.com/mctlhq/mctl-agent/internal/notify"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

// Pipeline orchestrates the ticket → diagnosis → fix → notify flow.
type Pipeline struct {
	store    *ticket.Store
	analyzer *diagnosis.Analyzer
	github   *fixer.GitHubFixer
	telegram *notify.Telegram
	dryRun   bool
	paused   atomic.Bool
	sem      chan struct{}
}

// NewPipeline creates a new pipeline orchestrator.
func NewPipeline(
	store *ticket.Store,
	analyzer *diagnosis.Analyzer,
	github *fixer.GitHubFixer,
	telegram *notify.Telegram,
	dryRun bool,
) *Pipeline {
	return &Pipeline{
		store:    store,
		analyzer: analyzer,
		github:   github,
		telegram: telegram,
		dryRun:   dryRun,
		sem:      make(chan struct{}, 3), // Max 3 concurrent analyses.
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

	// Run diagnosis.
	diag, err := p.analyzer.Analyze(ctx, t)
	if err != nil {
		log.Error("diagnosis failed", "error", err)
		_ = p.telegram.SendDiagnosis(t, "Diagnosis failed: "+err.Error(), ticket.ConfidenceLow, "Manual review required")
		return
	}

	// Update ticket with analysis.
	t.Analysis = diag.Diagnosis
	t.Confidence = diag.Confidence

	log.Info("diagnosis complete",
		"confidence", diag.Confidence,
		"fixable", diag.Fixable,
		"pattern", diag.FromPattern)

	// Infrastructure alerts — never auto-fix.
	if isInfraAlert(t) {
		_ = p.telegram.SendDiagnosis(t, diag.Diagnosis, diag.Confidence, "Infrastructure alert — manual review only")
		_ = p.store.Update(t)
		return
	}

	// HIGH confidence + fixable → create PR.
	if diag.Confidence == ticket.ConfidenceHigh && diag.Fixable {
		p.handleHighConfidenceFix(ctx, t, diag, log)
		return
	}

	// MEDIUM confidence + fixable → suggest in Telegram.
	if diag.Confidence == ticket.ConfidenceMedium && diag.Fixable {
		action := "Fixable but MEDIUM confidence — sending suggestion to Telegram for review"
		_ = p.telegram.SendDiagnosis(t, diag.Diagnosis, diag.Confidence, action)
		t.Status = ticket.StatusFixProposed
		_ = p.store.Update(t)
		return
	}

	// LOW / not fixable → notify only.
	_ = p.telegram.SendDiagnosis(t, diag.Diagnosis, diag.Confidence, "No auto-fix available")
	_ = p.store.Update(t)
}

func (p *Pipeline) handleHighConfidenceFix(ctx context.Context, t *ticket.Ticket, diag *diagnosis.DiagnosisResult, log *slog.Logger) {
	filePath := fixer.DetectFilePath(t.Tenant, t.Service)

	// Get current file content.
	content, err := p.github.GetFileContent(ctx, filePath, "main")
	if err != nil {
		log.Error("failed to get file content", "path", filePath, "error", err)
		_ = p.telegram.SendDiagnosis(t, diag.Diagnosis, diag.Confidence,
			fmt.Sprintf("Could not read %s: %v", filePath, err))
		_ = p.store.Update(t)
		return
	}

	// Generate patch.
	var newContent, summary string
	var patchErr error

	if diag.FromPattern {
		switch diag.PatternFixType {
		case "bump_memory":
			newContent, summary, patchErr = fixer.GenerateMemoryBump(content)
		case "rollback_image":
			// TODO: detect previous tag from audit/history.
			patchErr = fmt.Errorf("image rollback requires previous tag — not yet implemented")
		default:
			patchErr = fmt.Errorf("unknown pattern fix type: %s", diag.PatternFixType)
		}
	} else {
		newContent, summary, patchErr = fixer.GenerateFromDiagnosis(content, diag)
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
	// Infrastructure alerts have no tenant service (node-level or vault).
	if t.Service == "" {
		return true
	}
	return false
}
