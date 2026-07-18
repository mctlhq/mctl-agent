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

// Package optimizer implements GitOps resource right-sizing: it observes
// per-workload usage in VictoriaMetrics, persists hourly rollups, proposes
// deterministic resource-request changes as PRs to the gitops repo, and
// evaluates the outcome after merge (SUCCESS/REGRESSION verdict, rollback
// PR on regression). The optimizer never touches the cluster directly —
// Git stays the source of truth.
//
// MVP scope: plain Deployment-based services rendered by base-service,
// main container only, resources.requests only. Explicitly deferred:
// limits/replicas/HPA, blueGreen (Rollout) services, sidecars and init
// containers, CronJobs/Argo Workflows, StatefulSets/storage, seasonality
// beyond the 14d window, latency/error-rate evaluation, auto-merge,
// node-consolidation savings estimation.
package optimizer

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"sync"
	"time"

	"github.com/google/go-github/v68/github"
	"gopkg.in/yaml.v3"

	"github.com/mctlhq/mctl-agent/internal/config"
	"github.com/mctlhq/mctl-agent/internal/fixer"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

// GitOpsClient is the subset of fixer.GitHubFixer the optimizer needs.
type GitOpsClient interface {
	ListDir(ctx context.Context, path, ref string) ([]string, error)
	GetFileContent(ctx context.Context, path, ref string) (string, error)
	CreatePRRaw(ctx context.Context, req fixer.RawPRRequest) (string, int, error)
	GetPR(ctx context.Context, prNumber int) (*github.PullRequest, error)
	CreatePRComment(ctx context.Context, prNumber int, body string) error
	ClosePR(ctx context.Context, prNumber int, reason string) error
}

// Notifier delivers optimizer events; notify.Telegram satisfies it.
type Notifier interface {
	SendText(text string) error
}

// Candidate is one workload's latest optimizer assessment, exposed by the
// candidates API for observability of the guardrails.
type Candidate struct {
	ServiceSpec
	SkipReason SkipReason `json:"skip_reason,omitempty"`
	CheckedAt  time.Time  `json:"checked_at"`
}

// Optimizer owns the three optimizer loops (collector, recommender,
// evaluator).
type Optimizer struct {
	store       *Store
	ticketStore *ticket.Store
	vm          MetricsQuerier
	gitops      GitOpsClient
	notifier    Notifier
	cfg         config.OptimizerConfig

	ignoreRe *regexp.Regexp

	// collectedThrough tracks, per workload, the last hour a collection
	// pass fully processed (including empty hours), so scaled-to-zero
	// services don't re-query the whole backfill window every tick.
	// In-memory only: a restart triggers at most one full backfill pass.
	collectedThrough map[string]time.Time

	mu         sync.Mutex
	candidates []Candidate
}

// New creates an Optimizer.
func New(store *Store, ticketStore *ticket.Store, vm MetricsQuerier, gitops GitOpsClient, notifier Notifier, cfg config.OptimizerConfig) *Optimizer {
	var ignoreRe *regexp.Regexp
	if cfg.IgnoreServiceRegex != "" {
		var err error
		if ignoreRe, err = regexp.Compile(cfg.IgnoreServiceRegex); err != nil {
			slog.Warn("optimizer: invalid ignore regex, filter disabled", "regex", cfg.IgnoreServiceRegex, "error", err)
		}
	}
	return &Optimizer{
		store:            store,
		ticketStore:      ticketStore,
		vm:               vm,
		gitops:           gitops,
		notifier:         notifier,
		cfg:              cfg,
		ignoreRe:         ignoreRe,
		collectedThrough: make(map[string]time.Time),
	}
}

// Run starts the collector, recommender, and evaluator loops and blocks
// until ctx is cancelled.
func (o *Optimizer) Run(ctx context.Context) {
	slog.Info("optimizer started",
		"dry_run", o.cfg.DryRun,
		"collect_interval", o.cfg.CollectInterval,
		"recommend_hour_utc", o.cfg.RecommendHourUTC,
		"tenant_allowlist", o.cfg.TenantAllowlist)

	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()
		o.collectTick(ctx) // initial pass before the first tick
		ticker := time.NewTicker(o.cfg.CollectInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				o.collectTick(ctx)
			}
		}
	}()

	go func() {
		defer wg.Done()
		for {
			next := nextHourUTC(time.Now().UTC(), o.cfg.RecommendHourUTC)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Until(next)):
				o.RecommendPass(ctx, time.Now().UTC())
			}
		}
	}()

	go func() {
		defer wg.Done()
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				o.evaluatorTick(ctx, time.Now().UTC())
			}
		}
	}()

	wg.Wait()
}

func (o *Optimizer) collectTick(ctx context.Context) {
	specs, err := DiscoverServices(ctx, o.gitops)
	if err != nil {
		slog.Error("optimizer: service discovery failed", "error", err)
		return
	}
	o.Collect(ctx, specs, time.Now().UTC())
}

// RecommendPass runs one full recommendation sweep. Exported so the REST
// API can trigger it on demand.
func (o *Optimizer) RecommendPass(ctx context.Context, now time.Time) {
	specs, err := DiscoverServices(ctx, o.gitops)
	if err != nil {
		slog.Error("optimizer: service discovery failed", "error", err)
		return
	}

	openTickets, err := o.openTicketIndex()
	if err != nil {
		slog.Error("optimizer: open ticket index failed", "error", err)
		return
	}

	prBudget := o.cfg.MaxPRsPerDay
	if n, err := o.store.CountRecommendationPRsInWindow(24); err == nil {
		prBudget -= n
	} else {
		slog.Error("optimizer: PR budget query failed", "error", err)
		return
	}

	limitRanges := map[string][2]int64{} // tenant → {maxCPUm, maxMemBytes}
	candidates := make([]Candidate, 0, len(specs))

	for _, spec := range specs {
		skip := o.assess(ctx, spec, openTickets, limitRanges, &prBudget, now)
		candidates = append(candidates, Candidate{ServiceSpec: spec, SkipReason: skip, CheckedAt: now})
	}

	o.mu.Lock()
	o.candidates = candidates
	o.mu.Unlock()
}

// SkipRateLimited is reported when the daily optimizer PR budget is spent;
// the workload is reconsidered on the next daily pass.
const SkipRateLimited SkipReason = "rate_limited"

// SkipRecommended marks workloads that got a recommendation this pass.
const SkipRecommended SkipReason = "recommended"

func (o *Optimizer) assess(ctx context.Context, spec ServiceSpec, openTickets map[string]bool, limitRanges map[string][2]int64, prBudget *int, now time.Time) SkipReason {
	guards := GuardInputs{
		Ignored: o.isIgnored(spec),
	}
	guards.OpenTickets = openTickets[spec.Tenant+"/"] || openTickets[spec.Tenant+"/"+spec.Service]

	if openRec, err := o.store.OpenRecommendation(spec.Tenant, spec.Service); err != nil {
		slog.Error("optimizer: open recommendation query failed", "tenant", spec.Tenant, "service", spec.Service, "error", err)
		return SkipInvalidSpec
	} else if openRec != nil {
		guards.HasOpenRecommendation = true
	}

	rollups, err := o.store.RollupsInWindow(spec.Tenant, spec.Service,
		now.Add(-time.Duration(o.cfg.TargetDays*24*float64(time.Hour))), now)
	if err != nil {
		slog.Error("optimizer: rollup query failed", "tenant", spec.Tenant, "service", spec.Service, "error", err)
		return SkipInvalidSpec
	}

	// Live guard inputs are only worth fetching if the cheap checks pass.
	if !guards.Ignored && !guards.OpenTickets && !guards.HasOpenRecommendation && len(rollups) > 0 {
		o.fillQuota(ctx, spec.Tenant, &guards, now)
		o.fillLimitRange(ctx, spec.Tenant, &guards, limitRanges)
	}

	rec, skip := Recommend(spec, rollups, o.cfg, guards, now)
	if rec == nil {
		return skip
	}
	if *prBudget <= 0 {
		return SkipRateLimited
	}

	if err := o.publish(ctx, spec, rec); err != nil {
		slog.Error("optimizer: publishing recommendation failed", "tenant", spec.Tenant, "service", spec.Service, "error", err)
		return SkipInvalidSpec
	}
	if !o.cfg.DryRun {
		*prBudget--
	}
	return SkipRecommended
}

// publish stores the recommendation and either opens the PR or (dry run)
// sends the would-be PR body to Telegram.
func (o *Optimizer) publish(ctx context.Context, spec ServiceSpec, rec *Recommendation) error {
	var ev Evidence
	_ = json.Unmarshal([]byte(rec.EvidenceJSON), &ev)
	body := BuildPRBody(rec, ev, spec.ReplicaCount, o.cfg.Warmup, o.cfg.EvalWindow)

	if o.cfg.DryRun {
		rec.Status = RecStatusDryRun
		if err := o.store.CreateRecommendation(rec); err != nil {
			return fmt.Errorf("storing dry-run recommendation: %w", err)
		}
		o.notify(fmt.Sprintf("Optimizer (dry run) would right-size %s/%s: cpu %s -> %s, memory %s -> %s (confidence %s, risk %s). Recommendation %s.",
			rec.Tenant, rec.Service, rec.OldCPURequest, rec.NewCPURequest,
			rec.OldMemRequest, rec.NewMemRequest, rec.Confidence, rec.Risk, rec.ID))
		return nil
	}

	newCPU, newMem := rec.NewCPURequest, rec.NewMemRequest
	if newCPU == rec.OldCPURequest {
		newCPU = ""
	}
	if newMem == rec.OldMemRequest {
		newMem = ""
	}
	patched, summary, err := GenerateRequestsPatch(spec.RawValues, newCPU, newMem)
	if err != nil {
		return fmt.Errorf("patching values.yaml: %w", err)
	}

	// Never claude/*: that branch prefix is auto-approved and auto-merged
	// by the gitops repo's workflows, and optimizer PRs must stay
	// review-gated in the MVP.
	branch := fmt.Sprintf("agent/optimize/%s-%s-%s", rec.Tenant, rec.Service, time.Now().UTC().Format("20060102"))

	rec.Status = RecStatusPROpen
	rec.Branch = branch
	if err := o.store.CreateRecommendation(rec); err != nil {
		return fmt.Errorf("storing recommendation: %w", err)
	}
	// The body references rec.ID, so render after CreateRecommendation.
	body = BuildPRBody(rec, ev, spec.ReplicaCount, o.cfg.Warmup, o.cfg.EvalWindow)

	url, num, err := o.gitops.CreatePRRaw(ctx, fixer.RawPRRequest{
		Branch: branch,
		Title:  fmt.Sprintf("mctl optimizer: right-size %s/%s", rec.Tenant, rec.Service),
		Body:   body,
		CommitMessage: fmt.Sprintf("chore(%s): right-size resource requests\n\n%s\nRecommendation: %s\nAutomated by mctl-agent optimizer",
			rec.Service, summary, rec.ID),
		FilePath:   spec.FilePath,
		NewContent: patched,
	})
	if err != nil {
		rec.Status = RecStatusClosed
		_ = o.store.UpdateRecommendation(rec)
		return fmt.Errorf("creating PR: %w", err)
	}

	rec.PRNumber = num
	rec.PRURL = url
	if err := o.store.UpdateRecommendation(rec); err != nil {
		return fmt.Errorf("updating recommendation: %w", err)
	}
	if err := o.store.CreateRun(&Run{
		RecommendationID: rec.ID,
		BaselineStart:    rec.WindowStart,
		BaselineEnd:      rec.WindowEnd,
		Status:           RunStatusWaitingMerge,
	}); err != nil {
		return fmt.Errorf("creating run: %w", err)
	}

	o.notify(fmt.Sprintf("Optimizer: right-size PR for %s/%s — cpu %s -> %s, memory %s -> %s (confidence %s, risk %s)\n%s",
		rec.Tenant, rec.Service, rec.OldCPURequest, rec.NewCPURequest,
		rec.OldMemRequest, rec.NewMemRequest, rec.Confidence, rec.Risk, url))
	return nil
}

// Candidates returns the last recommendation pass's assessment per workload.
func (o *Optimizer) Candidates() []Candidate {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]Candidate, len(o.candidates))
	copy(out, o.candidates)
	return out
}

// Store exposes the optimizer store for the REST API.
func (o *Optimizer) Store() *Store {
	return o.store
}

func (o *Optimizer) isIgnored(spec ServiceSpec) bool {
	if len(o.cfg.TenantAllowlist) > 0 {
		allowed := false
		for _, t := range o.cfg.TenantAllowlist {
			if t == spec.Tenant {
				allowed = true
				break
			}
		}
		if !allowed {
			return true
		}
	}
	return o.ignoreRe != nil && o.ignoreRe.MatchString(spec.Service)
}

// openTicketIndex maps "tenant/" (tenant-wide) and "tenant/service" keys to
// unresolved incidents.
func (o *Optimizer) openTicketIndex() (map[string]bool, error) {
	tickets, err := o.ticketStore.ListOpen()
	if err != nil {
		return nil, err
	}
	idx := make(map[string]bool, len(tickets))
	for _, t := range tickets {
		if t.Tenant == "" {
			continue
		}
		idx[t.Tenant+"/"+t.Service] = true
		if t.Service == "" {
			idx[t.Tenant+"/"] = true
		}
	}
	return idx, nil
}

// fillQuota populates namespace ResourceQuota usage from kube-state-metrics.
// Absence leaves the fields zero, which disables the quota guard rather than
// blocking on it.
func (o *Optimizer) fillQuota(ctx context.Context, tenant string, g *GuardInputs, now time.Time) {
	read := func(resource, kind string) (float64, bool) {
		v, ok, err := o.vm.QueryScalar(ctx, fmt.Sprintf(
			`max(kube_resourcequota{namespace=%q, resource=%q, type=%q})`, tenant, resource, kind), now)
		if err != nil {
			slog.Warn("optimizer: quota query failed", "tenant", tenant, "resource", resource, "error", err)
			return 0, false
		}
		return v, ok
	}
	if used, ok := read("requests.cpu", "used"); ok {
		g.QuotaCPUUsedM = int64(used * 1000)
	}
	if hard, ok := read("requests.cpu", "hard"); ok {
		g.QuotaCPUHardM = int64(hard * 1000)
	}
	if used, ok := read("requests.memory", "used"); ok {
		g.QuotaMemUsed = int64(used)
	}
	if hard, ok := read("requests.memory", "hard"); ok {
		g.QuotaMemHard = int64(hard)
	}
}

// fillLimitRange loads the tenant LimitRange max from
// platform-gitops/tenants/<tenant>/values.yaml, cached per pass.
func (o *Optimizer) fillLimitRange(ctx context.Context, tenant string, g *GuardInputs, cache map[string][2]int64) {
	if v, ok := cache[tenant]; ok {
		g.LimitRangeMaxCPUM, g.LimitRangeMaxMem = v[0], v[1]
		return
	}
	var maxCPU, maxMem int64
	defer func() {
		cache[tenant] = [2]int64{maxCPU, maxMem}
		g.LimitRangeMaxCPUM, g.LimitRangeMaxMem = maxCPU, maxMem
	}()

	content, err := o.gitops.GetFileContent(ctx, fmt.Sprintf("platform-gitops/tenants/%s/values.yaml", tenant), "main")
	if err != nil {
		slog.Warn("optimizer: tenant values fetch failed", "tenant", tenant, "error", err)
		return
	}
	var doc struct {
		Tenant struct {
			LimitRange struct {
				Max struct {
					CPU    string `yaml:"cpu"`
					Memory string `yaml:"memory"`
				} `yaml:"max"`
			} `yaml:"limitRange"`
		} `yaml:"tenant"`
	}
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		slog.Warn("optimizer: tenant values parse failed", "tenant", tenant, "error", err)
		return
	}
	if v, err := ParseCPUMillis(doc.Tenant.LimitRange.Max.CPU); err == nil {
		maxCPU = v
	}
	if v, err := ParseMemBytes(doc.Tenant.LimitRange.Max.Memory); err == nil {
		maxMem = v
	}
}

func (o *Optimizer) notify(text string) {
	if o.notifier == nil {
		return
	}
	if err := o.notifier.SendText(text); err != nil {
		slog.Warn("optimizer: notification failed", "error", err)
	}
}

// nextHourUTC returns the next occurrence of hour:00 UTC strictly after now.
func nextHourUTC(now time.Time, hour int) time.Time {
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, time.UTC)
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	return next
}
