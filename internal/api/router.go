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

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/mctlhq/mctl-agent/internal/fixer"
	"github.com/mctlhq/mctl-agent/internal/mcp"
	"github.com/mctlhq/mctl-agent/internal/notify"
	"github.com/mctlhq/mctl-agent/internal/pipeline"
	"github.com/mctlhq/mctl-agent/internal/skill/remote"
	"github.com/mctlhq/mctl-agent/internal/ticket"
	"github.com/mctlhq/mctl-agent/internal/webhook"
)

// Options holds all dependencies for the API router.
type Options struct {
	Store         *ticket.Store
	Pipeline      *pipeline.Pipeline
	Telegram      *notify.Telegram
	GitHub        *fixer.GitHubFixer
	RemoteManager *remote.Manager
	WebhookStore  *webhook.Store
	WebhookTTL    time.Duration
	// OnAlert is called when AlertManager sends an alert.
	OnAlert func(w http.ResponseWriter, r *http.Request)
	// OnGitHubWebhook handles GitHub webhook events (optional).
	OnGitHubWebhook func(w http.ResponseWriter, r *http.Request)
}

// NewRouter creates the HTTP router.
func NewRouter(opts Options) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)

	// Health checks.
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	r.Get("/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	})

	// AlertManager webhook.
	r.Post("/api/v1/alerts", opts.OnAlert)

	// GitHub Actions webhook.
	if opts.OnGitHubWebhook != nil {
		r.Post("/api/v1/github-webhook", opts.OnGitHubWebhook)
	}

	// Telegram webhook.
	r.Post("/api/v1/telegram", telegramWebhookHandler(opts))

	// Ticket list.
	r.Get("/api/v1/tickets", ticketListHandler(opts.Store))

	// Skill endpoints.
	r.Get("/api/v1/skills", skillListHandler(opts.Pipeline))
	r.Get("/api/v1/skills/{name}/metrics", skillMetricsHandler(opts.Pipeline))

	// Remote skill registration.
	if opts.RemoteManager != nil {
		r.Post("/api/v1/skills/register", remoteSkillRegisterHandler(opts.RemoteManager))
		r.Delete("/api/v1/skills/{name}", remoteSkillUnregisterHandler(opts.RemoteManager))
		r.Get("/api/v1/skills/remote", remoteSkillListHandler(opts.RemoteManager))
	}

	// MCP endpoint — JSON-RPC over HTTP POST.
	mcpServer := mcp.NewServer(opts.Pipeline, opts.WebhookStore)
	r.Post("/mcp", mcpServer.ServeHTTP)

	if opts.WebhookStore != nil {
		r.Get("/api/v1/webhooks", webhookListHandler(opts.WebhookStore))
		r.Post("/api/v1/webhooks", webhookCreateHandler(opts.WebhookStore))
		r.Delete("/api/v1/webhooks/{id}", webhookDeleteHandler(opts.WebhookStore))
		r.Post("/api/v1/tickets/{id}/external-claims", externalClaimHandler(opts.Store, opts.WebhookStore, int(opts.WebhookTTL.Seconds())))
		r.Patch("/api/v1/tickets/{id}/external-results", externalResultHandler(opts.Store, opts.WebhookStore, opts.Telegram))
	}

	return r
}

func ticketListHandler(store *ticket.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tickets, err := store.ListAll()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}

		q := r.URL.Query()
		status := strings.TrimSpace(q.Get("status"))
		tenant := strings.TrimSpace(q.Get("tenant"))
		service := strings.TrimSpace(q.Get("service"))

		if status != "" || tenant != "" || service != "" {
			filtered := tickets[:0]
			for _, t := range tickets {
				if status != "" && t.Status != status {
					continue
				}
				if tenant != "" && t.Tenant != tenant {
					continue
				}
				if service != "" && t.Service != service {
					continue
				}
				filtered = append(filtered, t)
			}
			tickets = filtered
		}

		writeJSON(w, http.StatusOK, map[string]interface{}{
			"items": tickets,
			"count": len(tickets),
		})
	}
}

func telegramWebhookHandler(opts Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "failed to read body", http.StatusBadRequest)
			return
		}

		var update notify.TelegramUpdate
		if err := json.Unmarshal(body, &update); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}

		if update.Message == nil || update.Message.Text == "" {
			w.WriteHeader(http.StatusOK)
			return
		}

		cmd := notify.ParseCommand(update.Message.Text)
		if cmd == nil {
			w.WriteHeader(http.StatusOK)
			return
		}

		handleTelegramCommand(cmd, opts)
		w.WriteHeader(http.StatusOK)
	}
}

func handleTelegramCommand(cmd *notify.TelegramCommand, opts Options) {
	switch cmd.Command {
	case "status":
		tickets, err := opts.Store.ListOpen()
		if err != nil {
			slog.Error("failed to list tickets for /status", "error", err)
			return
		}
		_ = opts.Telegram.SendStatus(tickets)

	case "ticket":
		t := findTicketByPrefix(opts.Store, cmd.TicketID)
		if t == nil {
			_ = opts.Telegram.SendText("Ticket not found: " + cmd.TicketID)
			return
		}
		_ = opts.Telegram.SendTicketDetail(t)

	case "approve":
		t := findTicketByPrefix(opts.Store, cmd.TicketID)
		if t == nil {
			_ = opts.Telegram.SendText("Ticket not found: " + cmd.TicketID)
			return
		}
		if t.PRNumber == 0 {
			_ = opts.Telegram.SendText("No PR associated with ticket " + cmd.TicketID)
			return
		}
		if err := opts.GitHub.MergePR(context.Background(), t.PRNumber); err != nil {
			_ = opts.Telegram.SendText("Failed to merge PR: " + err.Error())
			return
		}
		t.Status = ticket.StatusFixApplied
		_ = opts.Store.Update(t)
		_ = opts.Telegram.SendText("PR #" + strings.TrimSpace(fmt.Sprint(t.PRNumber)) + " merged for " + t.Service)

	case "reject":
		t := findTicketByPrefix(opts.Store, cmd.TicketID)
		if t == nil {
			_ = opts.Telegram.SendText("Ticket not found: " + cmd.TicketID)
			return
		}
		if t.PRNumber > 0 {
			_ = opts.GitHub.ClosePR(context.Background(), t.PRNumber, cmd.Reason)
		}
		t.Status = ticket.StatusSuppressed
		_ = opts.Store.Update(t)
		_ = opts.Telegram.SendText("Ticket " + cmd.TicketID + " rejected: " + cmd.Reason)

	case "ignore":
		t := findTicketByPrefix(opts.Store, cmd.TicketID)
		if t == nil {
			_ = opts.Telegram.SendText("Ticket not found: " + cmd.TicketID)
			return
		}
		t.Status = ticket.StatusSuppressed
		_ = opts.Store.Update(t)
		_ = opts.Telegram.SendText("Ticket " + cmd.TicketID + " suppressed")

	case "pause":
		opts.Pipeline.Pause()
		_ = opts.Telegram.SendText("Pipeline paused. Use /resume to restart.")

	case "resume":
		opts.Pipeline.Resume()
		_ = opts.Telegram.SendText("Pipeline resumed.")

	case "digest":
		open, err := opts.Store.ListOpen()
		if err != nil {
			_ = opts.Telegram.SendText("Failed to load tickets: " + err.Error())
			return
		}
		resolved, _ := opts.Store.CountResolvedInWindow(24)
		prs, _ := opts.Store.CountPRsInWindow(24)
		_ = opts.Telegram.SendDailyDigest(open, resolved, prs)
	}
}

// findTicketByPrefix finds a ticket by the first 8 chars of its ID.
func findTicketByPrefix(store *ticket.Store, prefix string) *ticket.Ticket {
	// Try exact match first.
	t, err := store.Get(prefix)
	if err == nil {
		return t
	}

	// Search by prefix in open tickets.
	tickets, err := store.ListAll()
	if err != nil {
		return nil
	}
	for _, t := range tickets {
		if strings.HasPrefix(t.ID, prefix) {
			return t
		}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func skillListHandler(pipe *pipeline.Pipeline) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		skills := pipe.Registry().List()
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"items": skills,
			"count": len(skills),
		})
	}
}

func skillMetricsHandler(pipe *pipeline.Pipeline) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := chi.URLParam(r, "name")
		if name == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "skill name required"})
			return
		}
		m := pipe.Metrics()
		if m == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "metrics not enabled"})
			return
		}
		snap := m.GetSnapshot(name)
		writeJSON(w, http.StatusOK, snap)
	}
}

func remoteSkillRegisterHandler(mgr *remote.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var reg remote.Registration
		if err := json.NewDecoder(r.Body).Decode(&reg); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
			return
		}
		if err := mgr.Register(reg); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{
			"status":  "registered",
			"name":    reg.Name,
			"message": "Remote skill registered successfully",
		})
	}
}

func remoteSkillUnregisterHandler(mgr *remote.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := chi.URLParam(r, "name")
		if !mgr.Unregister(name) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "remote skill not found: " + name})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "unregistered", "name": name})
	}
}

func remoteSkillListHandler(mgr *remote.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		regs := mgr.List()
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"items": regs,
			"count": len(regs),
		})
	}
}
