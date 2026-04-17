package api

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/mctlhq/mctl-agent/internal/ticket"
	"github.com/mctlhq/mctl-agent/internal/webhook"
)

type webhookCreateRequest struct {
	AgentID         string   `json:"agent_id"`
	URL             string   `json:"url"`
	Secret          string   `json:"secret"`
	AuthHeaderName  string   `json:"auth_header_name"`
	AuthHeaderValue string   `json:"auth_header_value"`
	EventTypes      []string `json:"event_types"`
	AllowedTenants  []string `json:"allowed_tenants"`
}

func webhookListHandler(store *webhook.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		items, err := store.ListEndpoints()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		for i := range items {
			if items[i].Secret != "" {
				items[i].Secret = "redacted"
			}
			if items[i].AuthHeaderValue != "" {
				items[i].AuthHeaderValue = "redacted"
			}
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"items": items, "count": len(items)})
	}
}

func webhookCreateHandler(store *webhook.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req webhookCreateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
			return
		}
		if req.AgentID == "" || req.URL == "" || req.Secret == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "agent_id, url, and secret are required"})
			return
		}
		ep := &webhook.WebhookEndpoint{
			ID:              uuid.New().String(),
			AgentID:         req.AgentID,
			URL:             req.URL,
			Secret:          req.Secret,
			AuthHeaderName:  req.AuthHeaderName,
			AuthHeaderValue: req.AuthHeaderValue,
			EventTypes:      req.EventTypes,
			AllowedTenants:  req.AllowedTenants,
			Active:          true,
		}
		if err := store.CreateEndpoint(ep); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		ep.Secret = "redacted"
		writeJSON(w, http.StatusCreated, ep)
	}
}

func webhookDeleteHandler(store *webhook.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := store.DeleteEndpoint(chi.URLParam(r, "id")); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	}
}

func authorizeExternalCallback(
	r *http.Request,
	body []byte,
	agentID string,
	event *webhook.Event,
	webhookStore *webhook.Store,
) bool {
	if event != nil && event.Delivery.CallbackAuthValue != "" {
		headerName := strings.TrimSpace(event.Delivery.CallbackAuthHeader)
		if headerName == "" {
			headerName = "Authorization"
		}
		if r.Header.Get(headerName) == event.Delivery.CallbackAuthValue {
			return true
		}
	}
	ep, err := webhookStore.GetEndpointByAgentID(agentID)
	if err != nil {
		return false
	}
	ts := r.Header.Get("X-Mctl-Webhook-Timestamp")
	sig := r.Header.Get("X-Mctl-Webhook-Signature")
	return webhook.Verify(body, ts, sig, ep.Secret)
}

func externalClaimHandler(ticketStore *ticket.Store, webhookStore *webhook.Store, leaseTTLSeconds int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ticketID := chi.URLParam(r, "id")
		var req webhook.ClaimRequest
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to read body"})
			return
		}
		if err := json.Unmarshal(body, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		if _, err := webhookStore.GetEndpointByAgentID(req.AgentID); err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unknown agent_id"})
			return
		}
		ev, err := webhookStore.GetEvent(req.EventID)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "event not found"})
			return
		}
		if ev.Ticket.ID != ticketID {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ticket/event mismatch"})
			return
		}
		if !authorizeExternalCallback(r, body, req.AgentID, ev, webhookStore) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid callback auth"})
			return
		}
		if _, err := ticketStore.Get(ticketID); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "ticket not found"})
			return
		}
		resp, err := webhookStore.CreateClaim(ticketID, req, timeDurationSeconds(leaseTTLSeconds))
		if err == webhook.ErrAlreadyClaimed {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "already claimed"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

func timeDurationSeconds(seconds int) time.Duration {
	return time.Duration(seconds) * time.Second
}

var prNumberPattern = regexp.MustCompile(`/pull/(\d+)`)

func parsePRNumber(prURL string) int {
	match := prNumberPattern.FindStringSubmatch(prURL)
	if len(match) != 2 {
		return 0
	}
	n, _ := strconv.Atoi(match[1])
	return n
}

func externalResultHandler(ticketStore *ticket.Store, webhookStore *webhook.Store, tg interface {
	SendExternalAgentResult(*ticket.Ticket, string, string, string, map[string]string) error
}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ticketID := chi.URLParam(r, "id")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to read body"})
			return
		}
		var req webhook.ExternalResult
		if err := json.Unmarshal(body, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		if _, err := webhookStore.GetEndpointByAgentID(req.AgentID); err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unknown agent_id"})
			return
		}
		ev, err := webhookStore.GetEvent(req.EventID)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "event not found"})
			return
		}
		if ev.Ticket.ID != ticketID {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ticket/event mismatch"})
			return
		}
		if !authorizeExternalCallback(r, body, req.AgentID, ev, webhookStore) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid callback auth"})
			return
		}
		claim, err := webhookStore.GetActiveClaim(req.LeaseID, req.EventID, req.AgentID)
		if err == webhook.ErrLeaseExpired {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "lease expired"})
			return
		}
		if err != nil && err != webhook.ErrClaimNotFound {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if err == webhook.ErrClaimNotFound {
			claim, err = webhookStore.GetClaim(req.LeaseID, req.EventID, req.AgentID)
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "claim not found"})
				return
			}
		}
		if claim.Status == webhook.ClaimCompleted && claim.IdempotencyKey == req.IdempotencyKey {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "idempotent": "true"})
			return
		}
		if claim.Status == webhook.ClaimCompleted && claim.IdempotencyKey != req.IdempotencyKey {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "claim already completed"})
			return
		}
		tk, err := ticketStore.Get(ticketID)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "ticket not found"})
			return
		}

		switch req.Status {
		case webhook.ResultAccepted:
			// audit-only in v1
		case webhook.ResultPRCreated:
			tk.ProposedFix = strings.TrimSpace(req.Summary)
			tk.PRURL = req.Artifacts["pr_url"]
			tk.PRNumber = parsePRNumber(tk.PRURL)
			if raw := strings.TrimSpace(req.Artifacts["pr_number"]); raw != "" {
				if parsed, err := strconv.Atoi(raw); err == nil {
					tk.PRNumber = parsed
				}
			}
			tk.PRRepo = strings.TrimSpace(req.Artifacts["repo"])
			tk.PRBranch = strings.TrimSpace(req.Artifacts["branch"])
			tk.PRCommitSHA = strings.TrimSpace(req.Artifacts["commit_sha"])
			tk.Status = ticket.StatusFixProposed
		case webhook.ResultNeedsHuman:
			tk.ProposedFix = strings.TrimSpace(req.Summary)
		case webhook.ResultDeclined, webhook.ResultFailed:
			note := strings.TrimSpace(req.Summary)
			if note != "" {
				if tk.Analysis != "" {
					tk.Analysis += "\n\n"
				}
				tk.Analysis += "External agent " + req.AgentID + ": " + note
			}
		default:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid result status"})
			return
		}
		if err := ticketStore.Update(tk); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if err := webhookStore.CompleteClaim(req.LeaseID, req); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if tg != nil {
			_ = tg.SendExternalAgentResult(tk, req.AgentID, req.Summary, req.MessageTemplate, req.Artifacts)
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}
