package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
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
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg := config.Load()

	// Initialize SQLite store.
	store, err := ticket.NewStore(cfg.DBPath)
	if err != nil {
		slog.Error("failed to initialize ticket store", "error", err, "path", cfg.DBPath)
		os.Exit(1)
	}
	defer store.Close() //nolint:errcheck

	// Initialize components.
	mctlClient := mctlclient.NewClient(cfg.MctlAPIURL, cfg.MctlAPIToken)
	githubFixer := fixer.NewGitHubFixer(cfg.GitHubToken, cfg.GitHubOwner, cfg.GitHubRepo, store, cfg.DryRun)
	telegram := notify.NewTelegram(cfg.TelegramBotToken, cfg.TelegramChatID)

	// Initialize skill registry.
	registry := skill.NewRegistry()
	builtin.RegisterAll(registry, cfg.AnthropicAPIKey)

	slog.Info("skills registered", "count", registry.Count())

	// Initialize capability provider.
	capProvider := capability.NewProvider(mctlClient, githubFixer, telegram, store)

	// Pipeline wires everything together.
	pipe := pipeline.NewPipeline(store, registry, capProvider, mctlClient, githubFixer, telegram, cfg.DryRun)

	// Alert handler (used by both webhook and poller).
	alertHandler := monitor.NewAlertHandler(store, pipe.ProcessTicket)
	poller := monitor.NewPoller(mctlClient, store, pipe.ProcessTicket)

	// Router.
	router := agentapi.NewRouter(agentapi.Options{
		Store:    store,
		Pipeline: pipe,
		Telegram: telegram,
		GitHub:   githubFixer,
		OnAlert:  alertHandler.ServeHTTP,
	})

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
