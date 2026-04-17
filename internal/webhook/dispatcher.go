package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/mctlhq/mctl-agent/internal/skill"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

type Dispatcher struct {
	store       *Store
	httpClient  *http.Client
	callbackURL string
	defaultTTL  time.Duration
}

func NewDispatcher(store *Store, callbackURL string, defaultTTL time.Duration) *Dispatcher {
	if defaultTTL <= 0 {
		defaultTTL = 15 * time.Minute
	}
	return &Dispatcher{
		store:       store,
		httpClient:  &http.Client{Timeout: 10 * time.Second},
		callbackURL: callbackURL,
		defaultTTL:  defaultTTL,
	}
}

func (d *Dispatcher) DefaultTTL() time.Duration { return d.defaultTTL }

func (d *Dispatcher) Emit(ctx context.Context, eventType EventType, tk *ticket.Ticket, diag *skill.DiagnosisResult) error {
	if d == nil || d.store == nil {
		return nil
	}
	endpoints, err := d.store.ListEndpointsForEvent(eventType)
	if err != nil {
		return err
	}
	endpoints = filterEndpointsByTenant(endpoints, tk.Tenant)
	if len(endpoints) == 0 {
		slog.Info("no webhook endpoints match event tenant; skipping dispatch",
			"event_type", eventType, "ticket_id", tk.ID, "tenant", tk.Tenant)
		return nil
	}
	eventID := "evt_" + uuid.New().String()
	callbackToken := "mctl_evt_" + uuid.New().String()
	ev := &Event{
		ID:         eventID,
		Type:       eventType,
		OccurredAt: time.Now().UTC(),
		Ticket: TicketPayload{
			ID:         tk.ID,
			Team:       tk.Tenant,
			Service:    tk.Service,
			Type:       tk.Type,
			Severity:   tk.Severity,
			Status:     tk.Status,
			Summary:    tk.Summary,
			Analysis:   tk.Analysis,
			Confidence: tk.Confidence,
		},
		Delivery: DeliveryInfo{
			ClaimURL:           d.callbackURL + "/api/v1/tickets/" + tk.ID + "/external-claims",
			ResultURL:          d.callbackURL + "/api/v1/tickets/" + tk.ID + "/external-results",
			LeaseTTLSeconds:    int(d.defaultTTL.Seconds()),
			CallbackAuthHeader: "Authorization",
			CallbackAuthValue:  "Bearer " + callbackToken,
		},
	}
	if diag != nil {
		ev.Diagnosis = &DiagnosisPayload{
			Diagnosis:  diag.Diagnosis,
			Confidence: diag.Confidence,
			Fixable:    diag.Fixable,
			FixType:    diag.FixType,
		}
	}
	if err := d.store.SaveEvent(ev, tk.ID); err != nil {
		return err
	}
	if err := d.store.CreateDeliveries(ev.ID, endpoints, ev.OccurredAt); err != nil {
		return err
	}
	slog.Info("webhook event queued", "event_id", ev.ID, "event_type", ev.Type, "ticket_id", tk.ID, "deliveries", len(endpoints))
	return nil
}

// filterEndpointsByTenant keeps endpoints whose AllowedTenants list is empty
// (no restriction) or contains the given tenant. Used by Emit to prevent
// external agents from receiving events for tenants they cannot act on.
func filterEndpointsByTenant(endpoints []WebhookEndpoint, tenant string) []WebhookEndpoint {
	out := make([]WebhookEndpoint, 0, len(endpoints))
	for _, ep := range endpoints {
		if len(ep.AllowedTenants) == 0 {
			out = append(out, ep)
			continue
		}
		for _, t := range ep.AllowedTenants {
			if t == tenant {
				out = append(out, ep)
				break
			}
		}
	}
	return out
}

func (d *Dispatcher) Start(ctx context.Context) {
	if d == nil || d.store == nil {
		return
	}
	go d.deliveryLoop(ctx)
	go d.leaseReaper(ctx)
}

func (d *Dispatcher) deliveryLoop(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := d.DispatchPending(ctx, 20); err != nil {
				slog.Warn("dispatch pending failed", "error", err)
			}
		}
	}
}

func (d *Dispatcher) leaseReaper(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := d.store.ExpireStaleLeases(time.Now().UTC()); err != nil {
				slog.Warn("lease reaper failed", "error", err)
			}
		}
	}
}

func (d *Dispatcher) DispatchPending(ctx context.Context, limit int) error {
	deliveries, err := d.store.DispatchBatch(time.Now().UTC(), limit)
	if err != nil {
		return err
	}
	for _, delivery := range deliveries {
		if err := d.dispatchOne(ctx, delivery); err != nil {
			slog.Warn("delivery dispatch failed", "delivery_id", delivery.ID, "error", err)
		}
	}
	return nil
}

func (d *Dispatcher) dispatchOne(ctx context.Context, delivery ExternalDelivery) error {
	ev, err := d.store.GetEvent(delivery.EventID)
	if err != nil {
		return err
	}
	ep, err := d.store.getEndpointByID(delivery.WebhookID)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	timestamp := time.Now().UTC().Format(time.RFC3339)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.URL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Mctl-Event-Id", ev.ID)
	req.Header.Set("X-Mctl-Event-Type", string(ev.Type))
	req.Header.Set("X-Mctl-Webhook-Timestamp", timestamp)
	req.Header.Set("X-Mctl-Webhook-Signature", Sign(payload, timestamp, ep.Secret))
	if ep.AuthHeaderName != "" && ep.AuthHeaderValue != "" {
		req.Header.Set(ep.AuthHeaderName, ep.AuthHeaderValue)
	}

	resp, err := d.httpClient.Do(req)
	attempt := delivery.Attempt + 1
	backoffs := []time.Duration{1 * time.Second, 5 * time.Second, 15 * time.Second}
	if err != nil {
		dead := attempt > len(backoffs)
		nextAttempt := time.Now().UTC()
		if !dead {
			nextAttempt = nextAttempt.Add(backoffs[attempt-1])
		}
		return d.store.MarkDeliveryFailed(delivery.ID, attempt, 0, "", err.Error(), nextAttempt, dead)
	}
	defer resp.Body.Close() //nolint:errcheck
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return d.store.MarkDeliveryDelivered(delivery.ID, attempt, resp.StatusCode, string(body), time.Now().UTC())
	}
	dead := attempt > len(backoffs)
	nextAttempt := time.Now().UTC()
	if !dead {
		nextAttempt = nextAttempt.Add(backoffs[attempt-1])
	}
	return d.store.MarkDeliveryFailed(delivery.ID, attempt, resp.StatusCode, string(body), "unexpected webhook status", nextAttempt, dead)
}
