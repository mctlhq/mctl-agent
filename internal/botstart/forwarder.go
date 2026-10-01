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

// Package botstart forwards private /start observations from the login
// bot's webhook, which mctl-agent owns, to mctl-telegram's bot-start bridge
// (POST /internal/bot-start-observations, mctlhq/mctl-telegram#679).
//
// The payload is exactly {update_id, telegram_id, observed_at}: no message
// text, no start payload and no internal state crosses the boundary.
// Forwarding never blocks the webhook: each observation is sent from its own
// goroutine on a detached context, so Telegram always gets its 200 at once.
package botstart

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/mctlhq/mctl-agent/internal/metrics"
)

// MinTokenLen is the shortest bearer token accepted, matching the
// mctl-telegram side, which refuses to mount the bridge with a shorter one.
const MinTokenLen = 32

const (
	defaultAttemptTimeout = 5 * time.Second
	defaultDeadline       = 15 * time.Second
	maxResponseBytes      = 4 << 10
)

// defaultBackoff is the wait before the second and the third attempt; three
// attempts in total.
var defaultBackoff = []time.Duration{1 * time.Second, 4 * time.Second}

// Config configures a Forwarder. Zero values take the defaults.
type Config struct {
	// URL is the full bridge endpoint. Empty disables forwarding.
	URL string
	// Token is the shared bearer. Empty disables forwarding; a value
	// shorter than MinTokenLen is a configuration error.
	Token string
	// Backoff lists the waits between attempts; len(Backoff)+1 attempts are
	// made. Nil means 1s then 4s.
	Backoff []time.Duration
	// AttemptTimeout bounds a single HTTP attempt. Zero means 5s.
	AttemptTimeout time.Duration
	// Deadline bounds one observation across all attempts. Zero means 15s.
	Deadline time.Duration
	// Transport overrides the HTTP transport (tests).
	Transport http.RoundTripper
}

// Forwarder posts /start observations to the bot-start bridge.
type Forwarder struct {
	url      string
	token    string
	backoff  []time.Duration
	deadline time.Duration
	client   *http.Client
	wg       sync.WaitGroup
}

// New validates cfg and returns a Forwarder. With no URL or no token the
// Forwarder is disabled: Forward only counts the observation as disabled.
// A token that is set but shorter than MinTokenLen, or a URL that is not an
// absolute http(s) URL, is an error so that a misconfiguration fails startup
// instead of silently dropping every /start.
func New(cfg Config) (*Forwarder, error) {
	token := strings.TrimSpace(cfg.Token)
	rawURL := strings.TrimSpace(cfg.URL)
	if token != "" && len(token) < MinTokenLen {
		return nil, fmt.Errorf("BOT_START_FORWARD_TOKEN is set but shorter than %d characters", MinTokenLen)
	}
	if rawURL != "" {
		u, err := url.Parse(rawURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, errors.New("BOT_START_FORWARD_URL must be an absolute http(s) URL")
		}
		if u.User != nil {
			return nil, errors.New("BOT_START_FORWARD_URL must not carry credentials")
		}
	}
	f := &Forwarder{backoff: defaultBackoff, deadline: defaultDeadline}
	if rawURL == "" || token == "" {
		return f, nil
	}
	f.url, f.token = rawURL, token
	if cfg.Backoff != nil {
		f.backoff = cfg.Backoff
	}
	if cfg.Deadline > 0 {
		f.deadline = cfg.Deadline
	}
	attemptTimeout := defaultAttemptTimeout
	if cfg.AttemptTimeout > 0 {
		attemptTimeout = cfg.AttemptTimeout
	}
	f.client = &http.Client{
		Timeout:   attemptTimeout,
		Transport: cfg.Transport,
		// Never follow a redirect: it would resend the bearer to wherever
		// the Location points. A 3xx is answered as a rejection.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return f, nil
}

// Enabled reports whether observations are actually sent.
func (f *Forwarder) Enabled() bool {
	return f != nil && f.url != "" && f.token != ""
}

// Forward sends one observation in the background and returns at once.
// observedAt is the Telegram message date, not the time of receipt.
func (f *Forwarder) Forward(updateID, telegramID int64, observedAt time.Time) {
	if !f.Enabled() {
		metrics.BotStartForward.WithLabelValues("disabled").Inc()
		return
	}
	body, err := json.Marshal(observation{
		UpdateID:   updateID,
		TelegramID: telegramID,
		ObservedAt: observedAt.UTC().Format(time.RFC3339),
	})
	if err != nil {
		metrics.BotStartForward.WithLabelValues("failed").Inc()
		slog.Error("bot-start forward: encode failed")
		return
	}
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		// Detached from the webhook request: Telegram's connection is
		// answered and closed long before the retries are done.
		ctx, cancel := context.WithTimeout(context.Background(), f.deadline)
		defer cancel()
		f.send(ctx, body)
	}()
}

// Wait blocks until every in-flight observation has finished or ctx ends.
func (f *Forwarder) Wait(ctx context.Context) {
	if f == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		f.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

type observation struct {
	UpdateID   int64  `json:"update_id"`
	TelegramID int64  `json:"telegram_id"`
	ObservedAt string `json:"observed_at"`
}

func (f *Forwarder) send(ctx context.Context, body []byte) {
	attempts := len(f.backoff) + 1
	for attempt := 1; ; attempt++ {
		status, err := f.post(ctx, body)
		switch {
		case err == nil && status >= 200 && status < 300:
			metrics.BotStartForward.WithLabelValues("sent").Inc()
			return
		case err == nil && status < 500:
			// 4xx (and an unexpected 1xx/3xx) will not change on a resend.
			metrics.BotStartForward.WithLabelValues("rejected").Inc()
			slog.Warn("bot-start forward rejected", "status", status, "attempt", attempt)
			return
		}
		if attempt >= attempts || ctx.Err() != nil {
			metrics.BotStartForward.WithLabelValues("failed").Inc()
			slog.Warn("bot-start forward failed", "status", status, "attempt", attempt, "err", f.scrub(err))
			return
		}
		metrics.BotStartForward.WithLabelValues("retry").Inc()
		slog.Info("bot-start forward will retry", "status", status, "attempt", attempt, "err", f.scrub(err))
		timer := time.NewTimer(f.backoff[attempt-1])
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			metrics.BotStartForward.WithLabelValues("failed").Inc()
			slog.Warn("bot-start forward failed", "attempt", attempt, "err", "deadline exceeded")
			return
		}
	}
}

// post makes one attempt. A transport error returns status 0.
func (f *Forwarder) post(ctx context.Context, body []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.token)
	resp, err := f.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
	return resp.StatusCode, nil
}

// scrub renders err for a log line with the bearer token removed, should a
// transport ever echo request headers into an error.
func (f *Forwarder) scrub(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if f.token != "" {
		msg = strings.ReplaceAll(msg, f.token, "[redacted]")
	}
	return msg
}
