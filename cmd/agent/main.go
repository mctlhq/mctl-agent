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
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"regexp"
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

	// Initialize database store.
	store, err := ticket.NewStore(cfg.DatabaseURL)
	if err != nil {
		slog.Error("failed to initialize ticket store", "error", err, "url", cfg.DatabaseURL)
		os.Exit(1)
	}
	defer store.Close() //nolint:errcheck

	// Initialize components.
	mctlClient := mctlclient.NewClient(cfg.MctlAPIURL, cfg.MctlAPIToken)
	githubFixer := fixer.NewGitHubFixer(cfg.GitHubToken, cfg.GitHubOwner, cfg.GitHubRepo, store, cfg.DryRun)
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
			go mctlClient.ResolveAlert(id)
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
		Store:         store,
		Pipeline:      pipe,
		Telegram:      telegram,
		GitHub:        githubFixer,
		RemoteManager: remoteMgr,
		WebhookStore:  webhookStore,
		WebhookTTL:    cfg.WebhookDefaultTTL,
		OnAlert:       alertHandler.ServeHTTP,
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
	go poller.Run(ctx, cfg.PollInterval)
	if webhookDispatcher != nil {
		webhookDispatcher.Start(ctx)
	}

	// Daily digest at 09:00 UTC.
	go runDailyDigest(ctx, store, telegram)

	// Start server.
	go func() {
		slog.Info("mctl-agent starting",
			"port", cfg.Port,
			"dry_run", cfg.DryRun,
			"poll_interval", cfg.PollInterval,
			"mctl_api", cfg.MctlAPIURL,
		)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

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
