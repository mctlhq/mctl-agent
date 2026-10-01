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
	"net"
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
	// defaultDeadline covers the worst case of all three attempts against a
	// bridge that times out rather than answering: 5s + 1s + 5s + 4s + 5s =
	// 20s, plus a second of slack. A smaller cap would silently cut the third
	// attempt exactly when the bridge is slow.
	//
	// It also sets the shutdown drain (deadline + 1s, see cmd/agent), which
	// makes worst-case shutdown about 22s + 5s trace flush = 27s. The
	// mctl-agent Deployment sets no terminationGracePeriodSeconds, so the
	// Kubernetes default of 30s applies: raising this, or the attempt
	// timeout or backoff, past ~24s needs a longer grace period in
	// mctl-gitops first, or the tail of the drain is cut by SIGKILL.
	defaultDeadline    = 21 * time.Second
	defaultMaxInFlight = 32
	maxResponseBytes   = 4 << 10
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
	// Deadline bounds one observation across all attempts. Zero means 21s.
	Deadline time.Duration
	// MaxInFlight caps observations being sent at once; a /start arriving
	// while the cap is full is dropped and counted as such. Zero means 32.
	MaxInFlight int
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
	// sem holds one token per observation in flight, bounding goroutines
	// and sockets when /start arrives faster than the bridge answers.
	sem chan struct{}
	// base parents every send; Shutdown cancels it to end the stragglers.
	base   context.Context
	cancel context.CancelFunc
	// mu orders wg.Add in Forward against the final wg.Wait in Shutdown.
	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
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
	maxInFlight := defaultMaxInFlight
	if cfg.MaxInFlight > 0 {
		maxInFlight = cfg.MaxInFlight
	}
	f.sem = make(chan struct{}, maxInFlight)
	f.base, f.cancel = context.WithCancel(context.Background())
	transport := cfg.Transport
	if transport == nil {
		// A dedicated transport rather than http.DefaultTransport, so the
		// bridge never gets more connections than there are sends in flight
		// and the forwarder shares no pool with the rest of the process.
		transport = &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: attemptTimeout, KeepAlive: 30 * time.Second}).DialContext,
			MaxConnsPerHost:       maxInFlight,
			MaxIdleConnsPerHost:   4,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   attemptTimeout,
			ResponseHeaderTimeout: attemptTimeout,
		}
	}
	f.client = &http.Client{
		Timeout:   attemptTimeout,
		Transport: transport,
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

// Deadline is how long one observation may take across all its attempts;
// a shutdown drain sized to it lets every send in flight finish.
func (f *Forwarder) Deadline() time.Duration {
	if f == nil {
		return 0
	}
	return f.deadline
}

// Forward sends one observation in the background and returns at once.
// observedAt is the Telegram message date, not the time of receipt. When
// MaxInFlight sends are already running the observation is dropped and
// counted as dropped: shedding is visible, and a burst of /start never
// turns into unbounded goroutines and sockets against a struggling bridge.
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
	select {
	case f.sem <- struct{}{}:
	default:
		metrics.BotStartForward.WithLabelValues("dropped").Inc()
		slog.Warn("bot-start forward dropped: too many in flight", "max_in_flight", cap(f.sem))
		return
	}
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		<-f.sem
		metrics.BotStartForward.WithLabelValues("dropped").Inc()
		slog.Warn("bot-start forward dropped: shutting down")
		return
	}
	f.wg.Add(1)
	f.mu.Unlock()
	go func() {
		defer f.wg.Done()
		defer func() { <-f.sem }()
		// Detached from the webhook request: Telegram's connection is
		// answered and closed long before the retries are done. Only
		// Shutdown cancels base.
		ctx, cancel := context.WithTimeout(f.base, f.deadline)
		defer cancel()
		f.send(ctx, body)
	}()
}

// Shutdown stops accepting new observations, lets sends in flight finish
// until ctx ends, then cancels the rest and waits for them to record their
// outcome, so no observation leaves mctl_agent_bot_start_forward_total
// uncounted. A send cut short is counted failed; Forward calls from the
// moment Shutdown starts are dropped. It returns promptly once ctx ends:
// the HTTP request and the backoff timer both honour cancellation.
func (f *Forwarder) Shutdown(ctx context.Context) {
	if !f.Enabled() {
		return
	}
	// closed is set before the first wg.Wait, under the same lock Forward
	// holds around its check and wg.Add, so no Add can run concurrently with
	// a Wait (sync.WaitGroup panics on that) and nothing new joins the drain.
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	f.Wait(ctx)
	f.cancel()
	f.wg.Wait()
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
			slog.Warn("bot-start forward failed", "status", status, "attempt", attempt, "err", f.scrub(err), "reason", f.stopReason(ctx))
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
			slog.Warn("bot-start forward failed", "attempt", attempt, "reason", f.stopReason(ctx))
			return
		}
	}
}

// stopReason names why a send ended early, for the failure log.
func (f *Forwarder) stopReason(ctx context.Context) string {
	switch {
	case f.base.Err() != nil:
		return "shutdown"
	case ctx.Err() != nil:
		return "deadline"
	default:
		return "attempts exhausted"
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
