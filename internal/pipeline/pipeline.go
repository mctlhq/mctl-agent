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
	"strings"
	"sync/atomic"
	"time"

	"github.com/mctlhq/mctl-agent/internal/capability"
	"github.com/mctlhq/mctl-agent/internal/ctxutil"
	"github.com/mctlhq/mctl-agent/internal/fixer"
	"github.com/mctlhq/mctl-agent/internal/mctlclient"
	"github.com/mctlhq/mctl-agent/internal/notify"
	"github.com/mctlhq/mctl-agent/internal/skill"
	"github.com/mctlhq/mctl-agent/internal/ticket"
	"github.com/mctlhq/mctl-agent/internal/webhook"
)

const (
	quietAlertRecordingRulesNoData   = "RecordingRulesNoData"
	quietAlertScrapePoolHasNoTargets = "ScrapePoolHasNoTargets"
	quietAlertTooManyScrapeErrors    = "TooManyScrapeErrors"
	quietAlertTooManyLogs            = "TooManyLogs"
)

// Pipeline orchestrates the ticket → evidence → skill match → diagnosis → fix → notify flow.
type Pipeline struct {
	store         *ticket.Store
	registry      *skill.Registry
	metrics       *skill.Metrics
	provider      *capability.Provider
	apiClient     *mctlclient.Client
	github        *fixer.GitHubFixer
	telegram      *notify.Telegram
	dispatcher    *webhook.Dispatcher
	dryRun        bool
	autoMerge     bool
	escalationTag string
	paused        atomic.Bool
	sem           chan struct{}
	// baseCtx is the process lifetime context, installed by SetBaseContext.
	// Tickets arriving through the func(*ticket.Ticket) monitor callbacks have
	// no context of their own, and diagnosis must still stop on shutdown.
	baseCtx context.Context
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
	dispatcher *webhook.Dispatcher,
) *Pipeline {
	return &Pipeline{
		store:         store,
		registry:      registry,
		metrics:       metrics,
		provider:      provider,
		apiClient:     apiClient,
		github:        github,
		telegram:      telegram,
		dispatcher:    dispatcher,
		dryRun:        dryRun,
		autoMerge:     autoMerge,
		escalationTag: escalationTag,
		sem:           make(chan struct{}, 3), // Max 3 concurrent analyses.
	}
}

// diagnosisTimeout bounds a single ticket run. processTicketSync ends in
// Skill.Diagnose, which calls out to a model and carries no deadline of its
// own — mctlclient's 15s http.Client timeout covers the alert syncs only. A
// wedged diagnosis would otherwise hold one of the three semaphore slots for
// the life of the process.
const diagnosisTimeout = 15 * time.Minute

// SetBaseContext installs the process lifetime context. Call it once at
// startup, before any ticket can arrive. Without it ProcessTicket falls back
// to context.Background(), which is what tests want and what production must
// not have.
func (p *Pipeline) SetBaseContext(ctx context.Context) {
	p.baseCtx = ctx
}

func (p *Pipeline) baseContext() context.Context {
	if p.baseCtx != nil {
		return p.baseCtx
	}
	return context.Background()
}

// publishAlert and updateAlert mirror a ticket to mctl-api synchronously.
//
// They were goroutines. That raced the caller's own writes, which the previous
// change fixed by sending a snapshot — and the snapshot exposed a second,
// worse problem: two sends for the same ticket had no ordering. A diagnosis
// sync carrying `analyzing` and the escalation sync carrying `escalated` are
// dispatched moments apart, and if the older one arrives last, mctl-api
// silently reverts to `analyzing` with the escalation reason stripped. That is
// the exact state the escalated status exists to prevent, reintroduced through
// the back door.
//
// Every call site is already inside processTicketSync, which runs in its own
// goroutine per ticket bounded by p.sem, so sending inline costs the pipeline
// nothing it was not already paying and makes ordering a property of the code
// rather than of the scheduler. A queue would restore the concurrency at the
// price of machinery that has to be right; there is nothing here that needs it.
//
// Making the publish synchronous also removes a 404 window: UpdateAlert could
// previously overtake the PublishAlert that creates the incident, in which case
// the PATCH was rejected and the status never left `analyzing` in mctl-api.
//
// The nil guard keeps the pipeline usable without an mctl-api client, which is
// how the unit tests construct it.
func (p *Pipeline) publishAlert(t *ticket.Ticket) {
	if p.apiClient == nil {
		return
	}
	p.apiClient.PublishAlert(t)
}

// recordPRLinkage stores the PR a fix produced. It derives a fresh detached
// context at the call site rather than reusing the diagnosis context: that one
// may be seconds from its deadline by the time GitHub answers, and a write
// cancelled there leaves an open PR that nothing points at.
//
// fromStatus is the status this caller last saw. The PR coordinates are
// written unconditionally — the PR exists whatever the ticket now says — while
// the status advances only if nothing else moved the row on; see
// Store.RecordPRLinkage.
// Returns whether the status advanced. It did not when something else moved
// the row on first, and the caller must then not mirror the status it wanted:
// the local store correctly kept the newer state, and pushing the stale one to
// mctl-api would reopen there what is closed here.
func (p *Pipeline) recordPRLinkage(parent context.Context, t *ticket.Ticket, fromStatus string) bool {
	ctx, cancel := ctxutil.DetachedWrite(parent)
	defer cancel()
	advanced, err := p.store.RecordPRLinkage(ctx, t, fromStatus)
	if err != nil {
		slog.Error("failed to record PR linkage", "ticket", t.ID, "pr", t.PRURL, "error", err)
		return false
	}
	if !advanced {
		slog.Info("PR recorded but status not advanced: ticket moved on concurrently",
			"ticket", t.ID, "pr", t.PRURL, "wanted", t.Status, "from_status", fromStatus)
	}
	return advanced
}

// recordAndMirrorPR records the PR a fix produced and mirrors the result to
// mctl-api — but only the status the store actually took. Mirroring
// unconditionally would push fix_proposed or fix_applied over a resolution
// that landed while GitHub was answering, reopening on the dashboard and in
// MCP what the guard in RecordPRLinkage correctly refused to reopen locally.
// Returns whether the transition was persisted, which callers must respect
// before taking further irreversible action on the PR.
func (p *Pipeline) recordAndMirrorPR(ctx context.Context, t *ticket.Ticket, fromStatus string) bool {
	advanced := p.recordPRLinkage(ctx, t, fromStatus)
	if advanced {
		p.updateAlert(t)
	}
	return advanced
}

// persistDiagnosisAndMirror writes a diagnosis outcome (status, analysis,
// confidence, proposed fix)
// and mirrors it to mctl-api only if the store took it. The three call sites it
// serves all sit after a slow step failed, so ctx there is frequently spent
// already — and they previously wrote under it, discarded the error, and
// mirrored regardless, which is the same desync this PR exists to close: the
// local row keeps analyzing while mctl-api is told fix_proposed.
// Returns whether the store took the transition. Callers must not announce
// what it refused: an external EventTicketFixFailed or EventTicketEscalated
// carrying a status neither store accepted tells consumers a resolved ticket
// was escalated.
func (p *Pipeline) persistDiagnosisAndMirror(parent context.Context, t *ticket.Ticket, fromStatus string) bool {
	ctx, cancel := ctxutil.DetachedWrite(parent)
	defer cancel()
	applied, err := p.store.SetDiagnosisFromStatus(ctx, t.ID, fromStatus, t.Status, t.Analysis, t.Confidence, t.ProposedFix)
	if err != nil {
		slog.Error("failed to persist diagnosis outcome", "ticket", t.ID, "status", t.Status, "error", err)
		return false
	}
	if !applied {
		slog.Info("diagnosis outcome not persisted: ticket moved on concurrently",
			"ticket", t.ID, "wanted", t.Status, "from_status", fromStatus)
		return false
	}
	p.updateAlert(t)
	return true
}

// persistEscalation writes the escalated state only while the row is still at
// fromStatus, and only the columns an escalation owns. Returns false when
// something else moved it on, in which case the caller must not mirror or
// announce an escalation that did not happen.
func (p *Pipeline) persistEscalation(parent context.Context, t *ticket.Ticket, fromStatus string) bool {
	ctx, cancel := ctxutil.DetachedWrite(parent)
	defer cancel()
	applied, err := p.store.SetDiagnosisFromStatus(ctx, t.ID, fromStatus, t.Status, t.Analysis, t.Confidence, t.ProposedFix)
	if err != nil {
		slog.Error("failed to persist escalated ticket", "ticket", t.ID, "error", err)
		return false
	}
	return applied
}

func (p *Pipeline) updateAlert(t *ticket.Ticket) {
	if p.apiClient == nil {
		return
	}
	p.apiClient.UpdateAlert(t)
}

// escalate ends the pipeline's involvement with a ticket: it records why no
// fix will be attempted, moves the ticket to StatusEscalated, and syncs both
// to mctl-api.
//
// Every early return that used to leave the ticket in StatusAnalyzing goes
// through here. Those paths did call store.Update, but never changed the
// status — so a ticket the agent had finished with still reported "analyzing"
// indefinitely, and the only thing that ever moved it was the 48h/120h
// watchdog force-resolving it as stuck. Incident dfec7bee sat that way for
// hours on 2026-08-16 with an empty analysis field.
//
// reason is appended to Analysis rather than replacing it: skills write their
// diagnosis there first, and ResolveByIDFromStatus later appends its own note
// the same way.
func (p *Pipeline) escalate(ctx context.Context, t *ticket.Ticket, reason string, diag *skill.DiagnosisResult) {
	// Captured before the mutation below: the detached write is guarded on it,
	// so a ticket that reached a terminal state while the fix step was timing
	// out keeps that state instead of being dragged back to escalated.
	fromStatus := t.Status
	t.Status = ticket.StatusEscalated
	if t.Analysis == "" {
		t.Analysis = reason
	} else {
		t.Analysis = t.Analysis + "\n\n" + reason
	}
	if t.Confidence == "" {
		t.Confidence = ticket.ConfidenceLow
	}
	// Detached: escalation is a TERMINAL state, and the path that reaches it
	// most often is a fix step (GetFileContent, CreatePR) that failed because
	// the 15-minute diagnosis deadline expired — so ctx is already cancelled
	// by the time we get here. Writing under it fails instantly while the rest
	// of escalate still mirrors to mctl-api and emits the external event,
	// leaving the ticket stuck in `analyzing` and disagreeing with everything
	// downstream that was told it escalated.
	//
	// Guarded, because detaching removes the accident that used to protect
	// this: with the write cancelled it could not clobber anything, and now it
	// can. A resolve landing during that slow fix step would otherwise be
	// overwritten by this stale escalated status and its stale nil
	// resolved_at, reopening a closed incident.
	if !p.persistEscalation(ctx, t, fromStatus) {
		slog.Info("escalation skipped: ticket reached a terminal state concurrently",
			"ticket", t.ID, "from_status", fromStatus)
		return
	}
	// Mirror to mctl-api. Without this the incident there keeps the status it
	// was published with (analyzing), which is what MCP and the dashboards read.
	p.updateAlert(t)
	p.emitExternalEvent(ctx, webhook.EventTicketEscalated, t, diag)
}

func (p *Pipeline) emitExternalEvent(ctx context.Context, eventType webhook.EventType, t *ticket.Ticket, diag *skill.DiagnosisResult) {
	if p.dispatcher == nil {
		return
	}
	if err := p.dispatcher.Emit(ctx, eventType, t, diag); err != nil {
		slog.Warn("failed to emit external webhook event", "event_type", eventType, "ticket", t.ID, "error", err)
	}
}

// Metrics returns the pipeline's metrics tracker (can be nil).
func (p *Pipeline) Metrics() *skill.Metrics { return p.metrics }

// Registry returns the pipeline's skill registry.
func (p *Pipeline) Registry() *skill.Registry { return p.registry }

// Pause stops processing new tickets.
func (p *Pipeline) Pause() { p.paused.Store(true) }

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

	// The caller's request may already be gone; do not create a ticket for it.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if err := p.store.Create(ctx, t); err != nil {
		return nil, fmt.Errorf("creating ticket: %w", err)
	}

	// Snapshot before handing the pointer to the pipeline goroutine, which
	// mutates Status and Analysis. The caller holds the returned value and may
	// marshal it, so sharing the pointer is a data race. Taken here rather
	// than after ProcessTicket: by then the goroutine already exists and
	// copying would be the race it is meant to avoid.
	//
	// Diagnosis runs on the process context, not the caller's request context:
	// the request ends as soon as this returns, and the work outlives it.
	snapshot := *t

	// Process asynchronously.
	p.ProcessTicket(t)

	return &snapshot, nil
}

// ProcessTicket runs the full pipeline for a single ticket.
func (p *Pipeline) ProcessTicket(t *ticket.Ticket) {
	if p.paused.Load() {
		slog.Info("pipeline paused, skipping ticket", "id", t.ID)
		return
	}

	go func() {
		// Acquired inside the goroutine, not before it. Blocking here used to
		// block the *caller*, and one caller is the AlertManager webhook
		// handler — a burst past the three concurrent slots held HTTP request
		// goroutines open, which makes AlertManager time out and retry, adding
		// load to the thing that is already saturated. Queueing a cheap
		// goroutine instead lets the webhook answer immediately.
		p.sem <- struct{}{}
		defer func() { <-p.sem }()

		ctx, cancel := context.WithTimeout(p.baseContext(), diagnosisTimeout)
		defer cancel()
		p.processTicketSync(ctx, t)
	}()
}

func (p *Pipeline) processTicketSync(ctx context.Context, t *ticket.Ticket) {
	log := slog.With("ticket", t.ID, "service", t.Tenant+"/"+t.Service)

	// Notify about new ticket.
	if shouldNotifyNewTicket(t) {
		if err := p.telegram.SendNewTicket(t); err != nil {
			log.Error("failed to send new ticket notification", "error", err)
		}
	} else {
		log.Info("suppressing new ticket notification", "alert_name", t.AlertName)
	}

	// Status first, then publish. The order matters and used to be the other
	// way round, which worked only by accident: PublishAlert ran in a goroutine
	// that read the live ticket, so it usually observed the `analyzing` written
	// on the next line. Removing that race (sending a snapshot, then sending
	// synchronously) made the accident stop happening, and mctl-api began
	// receiving `open` with nothing to correct it — leaving every incident
	// reading `open` for the whole diagnosis, however long that took.
	t.Status = ticket.StatusAnalyzing
	if err := p.store.Update(ctx, t); err != nil {
		log.Error("failed to update ticket status", "error", err)
		return
	}

	// Publish to mctl-api alert store.
	p.publishAlert(t)
	p.emitExternalEvent(ctx, webhook.EventTicketCreated, t, nil)

	// Collect evidence.
	p.collectEvidence(ctx, t)
	p.collectHistoricalEvidence(ctx, t)

	// Reload ticket with evidence.
	t, err := p.store.Get(ctx, t.ID)
	if err != nil {
		log.Error("failed to reload ticket", "error", err)
		return
	}

	ev := skill.NewEvidenceSet(t.Evidence)

	// Match skills.
	ranked := p.registry.Match(ctx, t, ev)
	if len(ranked) == 0 {
		log.Info("no skills matched ticket")
		if shouldNotifyDiagnosis(t) {
			_ = p.telegram.SendDiagnosis(t, "No skill matched this ticket type. Manual review required.", ticket.ConfidenceLow, "No auto-diagnosis available")
		}
		p.escalate(ctx, t, fmt.Sprintf(
			"Escalated: no skill matched this ticket (type=%s, alert=%s). Evidence was "+
				"collected, but the agent has no diagnostic rule for this signal, so nothing "+
				"was analysed. Needs a human, or a new skill.", t.Type, t.AlertName), nil)
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
		p.updateAlert(t)

		log.Info("diagnosis complete",
			"skill", rs.Skill.Name(),
			"confidence", diag.Confidence,
			"fixable", diag.Fixable)

		if isHumanReviewOnlyAlert(t) {
			if shouldNotifyDiagnosis(t) {
				_ = p.telegram.SendDiagnosis(t, diag.Diagnosis, diag.Confidence, "Alert is routed for diagnosis only; manual review required")
			}
			p.escalate(ctx, t, fmt.Sprintf(
				"[escalated] Alert %q is on the human-review-only list: the agent diagnoses "+
					"it but never proposes a fix, by policy. The diagnosis above is advisory.",
				t.AlertName), diag)
			return
		}

		// Infrastructure alerts — never auto-fix.
		if isInfraAlert(t) {
			if shouldNotifyDiagnosis(t) {
				_ = p.telegram.SendDiagnosis(t, diag.Diagnosis, diag.Confidence, "Infrastructure alert — manual review only")
			}
			// This branch previously emitted no escalation event at all, on top
			// of leaving the status untouched — infra tickets were the quietest
			// of the five dead ends.
			p.escalate(ctx, t, fmt.Sprintf(
				"[escalated] Infrastructure-scope alert (tenant=%s, service=%s): auto-fix is "+
					"disabled for infrastructure by policy, because a wrong fix here has "+
					"cluster-wide blast radius. The diagnosis above is advisory.",
				t.Tenant, t.Service), diag)
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
			if shouldNotifyDiagnosis(t) {
				_ = p.telegram.SendDiagnosis(t, diag.Diagnosis, diag.Confidence, action)
			}
			medFrom := t.Status
			t.Status = ticket.StatusFixProposed
			if p.persistDiagnosisAndMirror(ctx, t, medFrom) {
				p.emitExternalEvent(ctx, webhook.EventTicketEscalated, t, diag)
			}
			return
		}

		// LOW / not fixable → notify and stop.
		if shouldNotifyDiagnosis(t) {
			_ = p.telegram.SendDiagnosis(t, diag.Diagnosis, diag.Confidence, fmt.Sprintf("No auto-fix available (skill: %s)", rs.Skill.Name()))
		}
		p.escalate(ctx, t, fmt.Sprintf(
			"[escalated] Skill %s produced a %s-confidence diagnosis with fixable=%v. The "+
				"auto-fix threshold is HIGH, or MEDIUM when the skill declares itself "+
				"auto-merge safe, so no PR was created.",
			rs.Skill.Name(), diag.Confidence, diag.Fixable), diag)
		return
	}

	// All skills failed.
	log.Warn("all matched skills failed to diagnose", "skills", rankedSkillNames(ranked))
	if shouldNotifyDiagnosis(t) {
		_ = p.telegram.SendDiagnosis(t, "All matched skills failed to produce a diagnosis. Manual review required.", ticket.ConfidenceLow, "Manual review required")
	}
	p.escalate(ctx, t, fmt.Sprintf(
		"Escalated: %d matched skill(s) [%s] all failed to produce a diagnosis. This is an "+
			"agent-side failure and does not by itself say the service is unhealthy — check "+
			"mctl-agent logs for the per-skill errors.",
		len(ranked), strings.Join(rankedSkillNames(ranked), ", ")), nil)
}

// rankedSkillNames lists the skills that matched, for the escalation note and
// the log line. Previously "all matched skills failed" was recorded without
// naming them, which left no way to tell from the ticket which skill to fix.
func rankedSkillNames(ranked []skill.RankedSkill) []string {
	names := make([]string, 0, len(ranked))
	for _, rs := range ranked {
		names = append(names, rs.Skill.Name())
	}
	return names
}

func shouldNotifyNewTicket(t *ticket.Ticket) bool {
	return !isQuietAlert(t)
}

func shouldNotifyDiagnosis(t *ticket.Ticket) bool {
	return !isQuietAlert(t)
}

func isQuietAlert(t *ticket.Ticket) bool {
	if t == nil || t.Source != ticket.SourceAlertManager {
		return false
	}
	switch t.AlertName {
	case quietAlertRecordingRulesNoData,
		quietAlertScrapePoolHasNoTargets,
		quietAlertTooManyScrapeErrors,
		quietAlertTooManyLogs:
		return true
	default:
		return false
	}
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
		failedFrom := t.Status
		t.Status = ticket.StatusFixProposed
		if p.persistDiagnosisAndMirror(ctx, t, failedFrom) {
			p.emitExternalEvent(ctx, webhook.EventTicketFixFailed, t, diag)
		}
		return
	}
	if !fixResult.Applied {
		if p.metrics != nil {
			p.metrics.RecordFix(s.Name(), t.ID, false, fixDur, "fix not applied by skill")
		}
		log.Info("skill returned fix with Applied=false, skipping PR", "skill", s.Name())
		_ = p.telegram.SendDiagnosis(t, diag.Diagnosis, diag.Confidence,
			fmt.Sprintf("Skill %s declined to apply fix: %s", s.Name(), fixResult.Summary))
		p.escalate(ctx, t, fmt.Sprintf(
			"[escalated] Skill %s diagnosed the ticket but declined to apply a fix: %s. "+
				"No PR was created.", s.Name(), fixResult.Summary), diag)
		return
	}
	if p.metrics != nil {
		p.metrics.RecordFix(s.Name(), t.ID, true, fixDur, fixResult.Summary)
	}

	filePath := fixResult.FilePath
	if filePath == "" {
		filePath = fixer.DetectFilePath(t.Tenant, t.Service)
	}

	// Reject any path outside the configured gitops allowlist before ever
	// reading or writing it. GetFileContent/CreatePR enforce the same check
	// again (defense in depth), but validating here lets a rejection be
	// surfaced through the same "patch generation failed" path as other
	// fix-generation errors, rather than the read-failure escalation below.
	if err := p.github.ValidatePath(filePath); err != nil {
		log.Warn("rejected gitops path", "skill", s.Name(), "ticket", t.ID, "path", filePath, "error", err)
		_ = p.telegram.SendDiagnosis(t, diag.Diagnosis, diag.Confidence,
			"Fix identified but patch generation failed: "+err.Error())
		t.Status = ticket.StatusFixProposed
		_ = p.store.Update(ctx, t)
		p.updateAlert(t)
		p.emitExternalEvent(ctx, webhook.EventTicketFixFailed, t, diag)
		return
	}

	// Get current file content from GitOps repo.
	content, err := p.github.GetFileContent(ctx, filePath, "main")
	if err != nil {
		log.Error("failed to get file content", "path", filePath, "error", err)
		_ = p.telegram.SendDiagnosis(t, diag.Diagnosis, diag.Confidence,
			fmt.Sprintf("Could not read %s: %v", filePath, err))
		p.escalate(ctx, t, fmt.Sprintf(
			"[escalated] Could not read %s from the GitOps repo (%v), so no patch could be "+
				"generated. This is an agent-side failure, not necessarily a service failure.",
			filePath, err), diag)
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
		newContent, summary, patchErr = p.rollbackImage(ctx, filePath, content)
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
		failedFrom := t.Status
		t.Status = ticket.StatusFixProposed
		if p.persistDiagnosisAndMirror(ctx, t, failedFrom) {
			p.emitExternalEvent(ctx, webhook.EventTicketFixFailed, t, diag)
		}
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
		// FixFailed first, while the ticket still reads as it did when the fix
		// failed. escalate emits its own EventTicketEscalated afterwards, so a
		// subscriber sees "the fix failed" and then "it was handed to a human",
		// in that order, each with a payload that matches its own event.
		p.emitExternalEvent(ctx, webhook.EventTicketFixFailed, t, diag)
		p.escalate(ctx, t, "[escalated] A fix was generated but the PR could not be created: "+
			err.Error()+". Nothing was changed in the GitOps repo.", diag)
		return
	}

	fromStatus := t.Status
	t.PRURL = prURL
	t.PRNumber = prNumber
	t.Status = ticket.StatusFixProposed
	// Detached, and derived only now: the PR exists on GitHub already. If the
	// diagnosis deadline expired during CreatePR, writing under ctx would
	// silently drop the PR linkage and leave the ticket looking untouched
	// while a real PR sits open against it. The status half is guarded on
	// fromStatus so a resolve that landed during CreatePR is not overwritten.
	proposedPersisted := p.recordAndMirrorPR(ctx, t, fromStatus)

	if prURL != "" {
		// Never auto-merge on top of a transition that did not land. If the
		// write failed — a transient database outage is exactly when this
		// path is reached — merging anyway puts the change into production
		// while both incident stores still read `analyzing`, the post-merge
		// write is then guarded from an in-memory status the row never took,
		// and Telegram reports a success nobody can find afterwards. The PR
		// stays open for review instead, which is recoverable.
		shouldAutoMerge := p.autoMerge && !p.dryRun && proposedPersisted
		if p.autoMerge && !p.dryRun && !proposedPersisted {
			slog.Warn("skipping auto-merge: the fix_proposed transition was not persisted",
				"ticket", t.ID, "pr", prURL)
		}
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
				mergedFrom := t.Status
				t.Status = ticket.StatusFixApplied
				// Same reasoning as above: the merge already happened, and the
				// mirror follows the store rather than our in-memory copy.
				p.recordAndMirrorPR(ctx, t, mergedFrom)
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

func isHumanReviewOnlyAlert(t *ticket.Ticket) bool {
	if t == nil {
		return false
	}
	switch t.AlertName {
	case "CPUThrottlingHigh",
		"KubeJobNotCompleted",
		"KubePersistentVolumeFillingUp",
		"KubeStatefulSetReplicasMismatch":
		return true
	default:
		return false
	}
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

// rollbackImage resolves the previous distinct image tag from the GitOps
// history of valuesPath and produces a patch flipping the current tag back.
// On any miss (no prior commit, network error, file not parseable) it
// returns a non-nil error so the caller surfaces "Fix identified but
// patch generation failed" via Telegram instead of silently no-oping.
func (p *Pipeline) rollbackImage(ctx context.Context, valuesPath, content string) (string, string, error) {
	prevTag, err := p.github.LookupPreviousImageTag(ctx, valuesPath, currentImageTag(content))
	if err != nil {
		return "", "", fmt.Errorf("looking up previous image tag: %w", err)
	}
	if prevTag == "" {
		return "", "", fmt.Errorf("no prior distinct image tag found in last %d commits of %s", fixer.PreviousTagLookupCommitCap, valuesPath)
	}
	return fixer.GenerateImageRollback(content, prevTag)
}

// currentImageTag is a thin wrapper around fixer.ExtractImageTag exposed
// for the rollback path. Returns "" when no tag line is present (the
// lookup will then return any prior tag without filtering).
func currentImageTag(content string) string {
	tag, _ := fixer.ExtractImageTag(content)
	return tag
}
