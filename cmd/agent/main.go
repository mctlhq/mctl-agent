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

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	agentapi "github.com/mctlhq/mctl-agent/internal/api"
	"github.com/mctlhq/mctl-agent/internal/capability"
	"github.com/mctlhq/mctl-agent/internal/config"
	"github.com/mctlhq/mctl-agent/internal/fixer"
	"github.com/mctlhq/mctl-agent/internal/mctlclient"
	"github.com/mctlhq/mctl-agent/internal/monitor"
	"github.com/mctlhq/mctl-agent/internal/notify"
	"github.com/mctlhq/mctl-agent/internal/pipeline"
	"github.com/mctlhq/mctl-agent/internal/skill"
	"github.com/mctlhq/mctl-agent/internal/skill/builtin"
	"github.com/mctlhq/mctl-agent/internal/skill/remote"
	yamlskill "github.com/mctlhq/mctl-agent/internal/skill/yaml"
	"github.com/mctlhq/mctl-agent/internal/ticket"
	"github.com/mctlhq/mctl-agent/internal/webhook"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg := config.Load()

	// Initialize database store. The pod's network namespace is not always
	// ready when the process starts: a same-node dial to shared-pg gets
	// "connection refused" for the first few seconds, and a single attempt
	// then exits 1 and burns a CrashLoopBackOff restart on every rollout.
	initCtx, stopInitSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	store, err := initTicketStore(initCtx, cfg.DatabaseURL)
	stopInitSignals()
	if err != nil {
		// cfg.DatabaseURL carries the DB password — log the redacted form only.
		slog.Error("failed to initialize ticket store", "error", err, "url", redactDSN(cfg.DatabaseURL))
		os.Exit(1)
	}
	defer store.Close() //nolint:errcheck

	// Initialize components.
	mctlClient := mctlclient.NewClient(cfg.MctlAPIURL, cfg.MctlAPIToken)
	githubFixer := fixer.NewGitHubFixer(cfg.GitHubToken, cfg.GitHubTokenFile, cfg.GitHubOwner, cfg.GitHubRepo, store, cfg.DryRun)
	telegram := notify.NewTelegram(cfg.TelegramBotToken, cfg.TelegramChatID, cfg.OpenClawBotUsername, cfg.TelegramTenantChatIDs)
	var webhookStore *webhook.Store
	var webhookDispatcher *webhook.Dispatcher
	if cfg.WebhookEnabled {
		webhookStore, err = webhook.NewStore(store.DB(), store.Dialect())
		if err != nil {
			slog.Error("failed to initialize webhook store", "error", err)
			os.Exit(1)
		}
		webhookDispatcher = webhook.NewDispatcher(webhookStore, cfg.WebhookCallbackURL, cfg.WebhookDefaultTTL)
	}

	// Initialize skill registry.
	registry := skill.NewRegistry()
	builtin.RegisterAll(registry, cfg.AnthropicAPIKey)

	slog.Info("skills registered", "count", registry.Count())

	// Load YAML-defined skills.
	yamlLoader := yamlskill.NewLoader("skills/custom", registry)
	yamlCount := yamlLoader.LoadAll()
	if yamlCount > 0 {
		slog.Info("yaml skills loaded", "count", yamlCount)
	}

	// Initialize skill metrics and circuit breaker.
	skillMetrics, err := skill.NewMetrics(store.DB(), 0.3, 10)
	if err != nil {
		slog.Error("failed to initialize skill metrics", "error", err)
		os.Exit(1)
	}

	// Initialize capability provider.
	capProvider := capability.NewProvider(mctlClient, githubFixer, telegram, store)

	// Pipeline wires everything together.
	pipe := pipeline.NewPipeline(store, registry, skillMetrics, capProvider, mctlClient, githubFixer, telegram, cfg.DryRun, cfg.AutoMergeEnabled, cfg.EscalationTag, webhookDispatcher)

	// Alert handler (used by both webhook and poller).
	alertHandler := monitor.NewAlertHandler(store, pipe.ProcessTicket)
	alertHandler.FlapCooldown = cfg.AlertFlapCooldown
	alertHandler.OnResolve = func(ids []string) {
		for _, id := range ids {
			go mctlClient.ResolveAlert(id, "alert resolved by AlertManager")
		}
	}
	if cfg.AlertIgnoreServiceRegex != "" {
		re, err := regexp.Compile(cfg.AlertIgnoreServiceRegex)
		if err != nil {
			slog.Error("invalid ALERT_IGNORE_SERVICE_REGEX; ignoring",
				"error", err, "pattern", cfg.AlertIgnoreServiceRegex)
		} else {
			alertHandler.IgnoreService = re
			slog.Info("alert service filter enabled", "pattern", cfg.AlertIgnoreServiceRegex)
		}
	}
	poller := monitor.NewPoller(mctlClient, store, pipe.ProcessTicket)
	poller.StaleAfter = cfg.AutoResolveStaleAfter
	poller.AnalyzingAfter = cfg.AutoResolveAnalyzingAfter
	poller.EscalatedAfter = cfg.AutoResolveEscalatedAfter
	poller.FixProposedAfter = cfg.AutoResolveFixProposedAfter
	poller.OrphanAfter = cfg.AutoResolveOrphanAfter
	poller.MaxAnalyzingAge = cfg.MaxAnalyzingAge
	poller.AMReconcileEnabled = cfg.AMReconcileEnabled
	poller.AMReconcileMinAge = cfg.AMReconcileMinAge
	if cfg.AMReconcileEnabled {
		poller.AMClient = &monitor.AlertManagerClient{
			BaseURL: cfg.AlertManagerURL,
			Timeout: cfg.AMReconcileTimeout,
			HTTP:    &http.Client{Timeout: cfg.AMReconcileTimeout + 5*time.Second},
		}
	}

	// GitHub Actions webhook handler (optional — enabled when GITHUB_WEBHOOK_SECRET is set).
	var ghWebhookHandler *monitor.GitHubWebhookHandler
	if cfg.GitHubWebhookSecret != "" {
		ghWebhookHandler = monitor.NewGitHubWebhookHandler(store, cfg.GitHubWebhookSecret, pipe.ProcessTicket)
		slog.Info("GitHub Actions webhook handler enabled")
	}

	// Remote skill manager.
	remoteMgr := remote.NewManager(registry)

	// Router.
	routerOpts := agentapi.Options{
		Store:                 store,
		Pipeline:              pipe,
		Telegram:              telegram,
		GitHub:                githubFixer,
		RemoteManager:         remoteMgr,
		WebhookStore:          webhookStore,
		WebhookTTL:            cfg.WebhookDefaultTTL,
		APIToken:              cfg.AgentAPIToken,
		TelegramWebhookSecret: cfg.TelegramWebhookSecret,
		AlertWebhookToken:     cfg.AlertWebhookToken,
		OnAlert:               alertHandler.ServeHTTP,
	}
	if ghWebhookHandler != nil {
		routerOpts.OnGitHubWebhook = ghWebhookHandler.ServeHTTP
	}
	router := agentapi.NewRouter(routerOpts)

	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Start poller goroutine.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Tickets arriving through the monitor callbacks carry no context of their
	// own; give the pipeline the process one so diagnosis stops on shutdown.
	pipe.SetBaseContext(ctx)
	go poller.Run(ctx, cfg.PollInterval)
	if webhookDispatcher != nil {
		webhookDispatcher.Start(ctx)
	}

	// Daily digest at 09:00 UTC.
	go runDailyDigest(ctx, store, telegram)

	// Inbound auth tokens fail closed when unset (internal/api/auth.go
	// rejects every request rather than disabling the check), so a missing
	// Vault key silently turns these routes into "reject everything" rather
	// than "public". Warn at startup for each one so that's visible in boot
	// logs instead of discovered via a wave of 401s.
	warnUnsetAuthTokens(cfg.AgentAPIToken, cfg.AlertWebhookToken, cfg.TelegramWebhookSecret)

	// Start server.
	go func() {
		slog.Info("mctl-agent starting",
			"port", cfg.Port,
			"dry_run", cfg.DryRun,
			"poll_interval", cfg.PollInterval,
			"mctl_api", cfg.MctlAPIURL,
			"api_auth", cfg.AgentAPIToken != "",
			"telegram_webhook_auth", cfg.TelegramWebhookSecret != "",
			"alert_webhook_auth", cfg.AlertWebhookToken != "",
		)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	if cfg.TelegramWebhookSecret != "" && cfg.WebhookCallbackURL != "" {
		go func() {
			setWebhookCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := telegram.SetWebhook(setWebhookCtx, cfg.WebhookCallbackURL, cfg.TelegramWebhookSecret); err != nil {
				slog.Error("telegram setWebhook failed", "error", err)
				return
			}
			slog.Info("telegram webhook registered", "url", cfg.WebhookCallbackURL+"/api/v1/telegram")
		}()
	}

	// Graceful shutdown.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	slog.Info("shutting down")

	cancel() // Stop poller.

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown error", "error", err)
	}
}

// warnUnsetAuthTokens logs a slog.Warn for each inbound auth token that is
// empty, naming the env var an operator needs to set. Extracted from main()
// so it can be unit-tested directly without booting the full binary.
func warnUnsetAuthTokens(agentAPIToken, alertWebhookToken, telegramWebhookSecret string) {
	if agentAPIToken == "" {
		slog.Warn("inbound auth token not configured; affected routes will reject all requests",
			"variable", "AGENT_API_TOKEN")
	}
	if alertWebhookToken == "" {
		slog.Warn("inbound auth token not configured; affected routes will reject all requests",
			"variable", "ALERTMANAGER_WEBHOOK_TOKEN")
	}
	if telegramWebhookSecret == "" {
		slog.Warn("inbound auth token not configured; affected routes will reject all requests",
			"variable", "TELEGRAM_WEBHOOK_SECRET")
	}
}

// runDailyDigest sends a summary to Telegram at 09:00 UTC every day.
func runDailyDigest(ctx context.Context, store *ticket.Store, tg *notify.Telegram) {
	for {
		now := time.Now().UTC()
		next := time.Date(now.Year(), now.Month(), now.Day(), 9, 0, 0, 0, time.UTC)
		if !next.After(now) {
			next = next.Add(24 * time.Hour)
		}
		select {
		case <-time.After(time.Until(next)):
			sendDigest(store, tg)
		case <-ctx.Done():
			return
		}
	}
}

func sendDigest(store *ticket.Store, tg *notify.Telegram) {
	open, err := store.ListOpen()
	if err != nil {
		slog.Error("daily digest: failed to list open tickets", "error", err)
		return
	}
	resolved, _ := store.CountResolvedInWindow(24)
	prs, _ := store.CountPRsInWindow(24)
	if err := tg.SendDailyDigest(open, resolved, prs); err != nil {
		slog.Error("daily digest: failed to send", "error", err)
	}
}

// storeInitBudget bounds how long startup waits for the database to accept
// connections. Kept under the readiness probe's first-failure window so a
// genuinely dead database still surfaces as a failing pod rather than a pod
// that hangs in "starting" forever.
const storeInitBudget = 8 * time.Second

// newTicketStore is a seam so tests can drive the retry loop without a live
// database. Production always uses ticket.NewStore.
var newTicketStore = ticket.NewStore

// initTicketStore opens the ticket store, retrying while the database refuses
// connections. Errors that are not transient (a malformed DSN, a failed
// migration) fail on the first attempt — retrying those only delays the exit.
func initTicketStore(ctx context.Context, connStr string) (*ticket.Store, error) {
	deadline := time.Now().Add(storeInitBudget)
	delay := 250 * time.Millisecond
	connStr = withConnectTimeout(connStr)

	for attempt := 1; ; attempt++ {
		store, err := newTicketStore(ctx, connStr)
		if err == nil {
			if attempt > 1 {
				slog.Info("ticket store initialised after retry", "attempts", attempt)
			}
			return store, nil
		}
		if !isTransientDialError(err) || time.Now().After(deadline) {
			return nil, err
		}
		slog.Warn("ticket store not reachable yet, retrying",
			"attempt", attempt, "retry_in", delay, "error", err)

		select {
		case <-ctx.Done():
			// SIGTERM during startup: stop retrying and report why.
			return nil, fmt.Errorf("interrupted while waiting for database: %w", ctx.Err())
		case <-time.After(delay):
		}
		if delay < time.Second {
			delay *= 2
		}
	}
}

// isTransientDialError reports whether err is the pod-network-not-ready
// signature (connection refused / no route / DNS not resolving yet) rather
// than a permanent configuration or migration failure.
func isTransientDialError(err error) bool {
	var netErr *net.OpError
	if errors.As(err, &netErr) {
		return true
	}
	msg := err.Error()
	for _, s := range []string{
		"connection refused",
		"no such host",
		"network is unreachable",
		"i/o timeout",
		// Postgres accepts the TCP connection while it is still recovering and
		// rejects queries with SQLSTATE 57P03. A CNPG failover or a restarted
		// primary hits this, and it clears within seconds — exactly what the
		// retry budget is for.
		"the database system is starting up",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// redactDSN strips the password from a postgres URL so it never reaches the
// logs. A non-URL DSN (the SQLite file path) is returned unchanged.
//
// Fails closed: a DSN that will not parse is exactly the malformed input most
// likely to accompany a real startup failure, so it is replaced wholesale
// rather than echoed back with a password still in it.
func redactDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return "<unparseable dsn>"
	}
	if u.User == nil {
		// libpq also accepts key-value DSNs ("host=... password=..."), which
		// url.Parse happily reads as a bare path — no user info, nothing
		// redacted, password intact. Catch that form before giving up.
		return keyValueDSNPassword.ReplaceAllString(dsn, "password=redacted")
	}
	if _, hasPassword := u.User.Password(); hasPassword {
		u.User = url.UserPassword(u.User.Username(), "redacted")
	}
	// A URI-form DSN may also carry the password as a connection parameter
	// ("postgres://user@host/db?password=..."), which never appears in the
	// userinfo the branch above rewrites.
	if q := u.Query(); q.Has("password") {
		q.Set("password", "redacted")
		u.RawQuery = q.Encode()
	}
	return u.String()
}

// keyValueDSNPassword matches the password field of a libpq key-value DSN.
// Values may be single-quoted (with backslash escapes) or bare up to the next
// space, per the libpq connection-string grammar.
var keyValueDSNPassword = regexp.MustCompile(`password\s*=\s*('(?:[^'\\]|\\.)*'|\S+)`)

// withConnectTimeout bounds a single dial attempt. Without it the retry loop
// only controls the gaps between attempts: a network that drops packets
// (rather than refusing the connection) leaves one dial blocked for the OS TCP
// connect timeout — ~130s on Linux — which overruns both the init budget and
// the SIGTERM grace period. Non-postgres DSNs (the SQLite path) pass through.
func withConnectTimeout(dsn string) string {
	if !strings.HasPrefix(dsn, "postgres://") && !strings.HasPrefix(dsn, "postgresql://") {
		return dsn
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	q := u.Query()
	if q.Get("connect_timeout") != "" {
		return dsn // operator-supplied value wins
	}
	// Seconds, per libpq. Two dials fit inside the budget, so a hung attempt
	// still leaves room for one retry.
	q.Set("connect_timeout", "3")
	u.RawQuery = q.Encode()
	return u.String()
}
