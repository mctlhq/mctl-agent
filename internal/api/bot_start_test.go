package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/mctlhq/mctl-agent/internal/botstart"
	"github.com/mctlhq/mctl-agent/internal/metrics"
	"github.com/mctlhq/mctl-agent/internal/notify"
)

const (
	botStartToken  = "fedcba9876543210fedcba9876543210fedcba9876543210"
	operatorChatID = 210408407
	clientChatID   = 777000123
)

// telegramAPIRecorder stands in for api.telegram.org: notify.Telegram uses
// the default transport, so swapping it counts every outbound Bot API call
// (sendMessage and friends) and passes everything else through.
type telegramAPIRecorder struct {
	next http.RoundTripper
	mu   sync.Mutex
	hits []string
}

func (r *telegramAPIRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host != "api.telegram.org" {
		return r.next.RoundTrip(req)
	}
	r.mu.Lock()
	r.hits = append(r.hits, req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:])
	r.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":{}}`)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Request:    req,
	}, nil
}

func (r *telegramAPIRecorder) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.hits...)
}

func recordTelegramAPI(t *testing.T) *telegramAPIRecorder {
	t.Helper()
	rec := &telegramAPIRecorder{next: http.DefaultTransport}
	http.DefaultTransport = rec
	t.Cleanup(func() { http.DefaultTransport = rec.next })
	return rec
}

// bridgeRecorder is a fake mctl-telegram bridge that records each body.
type bridgeRecorder struct {
	mu     sync.Mutex
	bodies []map[string]any
}

func (b *bridgeRecorder) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.bodies)
}

func newBridge(t *testing.T, status int) (*bridgeRecorder, *httptest.Server) {
	t.Helper()
	rec := &bridgeRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		rec.mu.Lock()
		rec.bodies = append(rec.bodies, m)
		rec.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return rec, srv
}

func newForwarder(t *testing.T, url string, cfg botstart.Config) *botstart.Forwarder {
	t.Helper()
	cfg.URL, cfg.Token = url, botStartToken
	if cfg.Backoff == nil {
		cfg.Backoff = []time.Duration{time.Millisecond, time.Millisecond}
	}
	f, err := botstart.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func drain(t *testing.T, f *botstart.Forwarder) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f.Wait(ctx)
	if ctx.Err() != nil {
		t.Fatal("forward did not finish")
	}
}

// startRouter wires only what the /start path may touch. Store and GitHub
// are deliberately nil: any use of them on that path panics, and the
// Recoverer turns the panic into a 500 that the tests reject.
func startRouter(t *testing.T, f *botstart.Forwarder) http.Handler {
	t.Helper()
	return NewRouter(Options{
		Telegram:              notify.NewTelegram("bot-token", "210408407", "", nil),
		TelegramWebhookSecret: "tg-secret",
		BotStart:              f,
		OnAlert:               func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) },
	})
}

func telegramMessage(t *testing.T, updateID int64, chatID int64, chatType, text, command string) []byte {
	t.Helper()
	msg := map[string]any{
		"message_id": 5,
		"date":       1759300000,
		"text":       text,
		"from":       map[string]any{"id": chatID, "is_bot": false, "first_name": "Ada"},
		"chat":       map[string]any{"id": chatID, "type": chatType},
	}
	if command != "" {
		msg["entities"] = []map[string]any{{"type": "bot_command", "offset": 0, "length": len(command)}}
	}
	b, err := json.Marshal(map[string]any{"update_id": updateID, "message": msg})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func postTelegram(t *testing.T, h http.Handler, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/telegram", bytes.NewReader(body))
	req.Header.Set("X-Telegram-Bot-Api-Secret-Token", "tg-secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestTelegramPrivateStartIsForwarded(t *testing.T) {
	tgAPI := recordTelegramAPI(t)
	bridge, srv := newBridge(t, http.StatusAccepted)
	logs := captureLogs(t)

	for i, tc := range []struct{ text, command string }{
		{"/start", "/start"},
		{"/Start", "/Start"},
		{"/START", "/START"},
		{"/start@MCTL_AI_bot", "/start@MCTL_AI_bot"},
		{"/START@MCTL_AI_bot", "/START@MCTL_AI_bot"},
		{"/start onboarding", "/start"},
	} {
		f := newForwarder(t, srv.URL, botstart.Config{})
		h := startRouter(t, f)
		before := bridge.count()
		w := postTelegram(t, h, telegramMessage(t, int64(1000+i), clientChatID, "private", tc.text, tc.command))
		drain(t, f)
		if w.Code != http.StatusOK {
			t.Fatalf("%q: status %d, want 200", tc.text, w.Code)
		}
		if bridge.count() != before+1 {
			t.Fatalf("%q: not forwarded", tc.text)
		}
		got := bridge.bodies[len(bridge.bodies)-1]
		if len(got) != 3 || got["update_id"] != float64(1000+i) || got["telegram_id"] != float64(clientChatID) {
			t.Fatalf("%q: body %v", tc.text, got)
		}
		if got["observed_at"] != time.Unix(1759300000, 0).UTC().Format(time.RFC3339) {
			t.Fatalf("%q: observed_at %v is not the message date", tc.text, got["observed_at"])
		}
	}
	if calls := tgAPI.calls(); len(calls) != 0 {
		t.Fatalf("/start made Bot API calls: %v", calls)
	}
	// Consumed: a client /start never falls through to the allowlist.
	if out := logs.String(); strings.Contains(out, "non-allowlisted") {
		t.Fatalf("/start fell through to command handling:\n%s", out)
	}
}

// A message without a date (malformed, but past the webhook secret) must
// not be recorded as 1970: receipt time stands in.
func TestTelegramStartWithoutDateUsesReceiptTime(t *testing.T) {
	recordTelegramAPI(t)
	bridge, srv := newBridge(t, http.StatusAccepted)
	f := newForwarder(t, srv.URL, botstart.Config{})
	h := startRouter(t, f)

	var update map[string]any
	if err := json.Unmarshal(telegramMessage(t, 48, clientChatID, "private", "/start", "/start"), &update); err != nil {
		t.Fatal(err)
	}
	update["message"].(map[string]any)["date"] = 0
	body, _ := json.Marshal(update)

	before := time.Now().UTC().Truncate(time.Second)
	if w := postTelegram(t, h, body); w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", w.Code)
	}
	drain(t, f)
	if bridge.count() != 1 {
		t.Fatalf("forwarded %d times, want 1", bridge.count())
	}
	got, err := time.Parse(time.RFC3339, bridge.bodies[0]["observed_at"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if got.Before(before) || got.After(time.Now().Add(time.Second)) {
		t.Fatalf("observed_at = %v, want receipt time (~%v)", got, before)
	}
}

// With TELEGRAM_WEBHOOK_SECRET unset nothing on the route is authenticated,
// so even with a configured forwarder a private /start must not reach the
// bridge: the request is refused at the door, as every other update is.
func TestTelegramStartNotForwardedWithoutWebhookSecret(t *testing.T) {
	tgAPI := recordTelegramAPI(t)
	bridge, srv := newBridge(t, http.StatusAccepted)
	f := newForwarder(t, srv.URL, botstart.Config{})
	h := NewRouter(Options{
		Telegram: notify.NewTelegram("bot-token", "210408407", "", nil),
		BotStart: f,
		OnAlert:  func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) },
	})
	sent := testutil.ToFloat64(metrics.BotStartForward.WithLabelValues("sent"))

	for _, header := range []string{"", "anything"} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/telegram",
			bytes.NewReader(telegramMessage(t, 49, clientChatID, "private", "/start", "/start")))
		if header != "" {
			req.Header.Set("X-Telegram-Bot-Api-Secret-Token", header)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("secret header %q: status %d, want 401", header, w.Code)
		}
	}
	drain(t, f)
	if bridge.count() != 0 {
		t.Fatalf("forwarded %d unauthenticated /start", bridge.count())
	}
	if got := testutil.ToFloat64(metrics.BotStartForward.WithLabelValues("sent")) - sent; got != 0 {
		t.Fatalf("sent delta = %v, want 0", got)
	}
	if calls := tgAPI.calls(); len(calls) != 0 {
		t.Fatalf("Bot API calls: %v", calls)
	}
}

func TestTelegramNonStartIsNotForwarded(t *testing.T) {
	recordTelegramAPI(t)
	bridge, srv := newBridge(t, http.StatusAccepted)
	f := newForwarder(t, srv.URL, botstart.Config{})
	h := startRouter(t, f)

	for _, tc := range []struct{ name, chatType, text, command string }{
		{"startx", "private", "/startx", "/startx"},
		{"plain message", "private", "hello", ""},
		{"start text without entity", "private", "/start", ""},
		{"group start", "group", "/start", "/start"},
		{"supergroup start", "supergroup", "/start@MCTL_AI_bot", "/start@MCTL_AI_bot"},
	} {
		w := postTelegram(t, h, telegramMessage(t, 1, clientChatID, tc.chatType, tc.text, tc.command))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d, want 200", tc.name, w.Code)
		}
	}
	drain(t, f)
	if n := bridge.count(); n != 0 {
		t.Fatalf("%d non-start updates forwarded", n)
	}
}

// The operator's own DM is a private chat too: /start there is consumed by
// the forwarder and must not fall through to command handling. Store and
// GitHub are nil in startRouter, so reaching them would be a 500.
func TestTelegramOperatorStartTouchesNothingElse(t *testing.T) {
	tgAPI := recordTelegramAPI(t)
	bridge, srv := newBridge(t, http.StatusAccepted)
	f := newForwarder(t, srv.URL, botstart.Config{})
	h := startRouter(t, f)

	w := postTelegram(t, h, telegramMessage(t, 42, operatorChatID, "private", "/start", "/start"))
	drain(t, f)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", w.Code)
	}
	if bridge.count() != 1 {
		t.Fatalf("operator /start forwarded %d times, want 1", bridge.count())
	}
	if calls := tgAPI.calls(); len(calls) != 0 {
		t.Fatalf("operator /start made Bot API calls: %v", calls)
	}
}

// /status from the operator still runs exactly as before: it reads the
// store and replies through the Bot API, and nothing is forwarded.
func TestTelegramOperatorStatusUnchanged(t *testing.T) {
	tgAPI := recordTelegramAPI(t)
	bridge, srv := newBridge(t, http.StatusAccepted)
	f := newForwarder(t, srv.URL, botstart.Config{})
	store := newTestStore(t)
	h := NewRouter(Options{
		Store:                 store,
		Pipeline:              newTestPipeline(t, store),
		Telegram:              notify.NewTelegram("bot-token", "210408407", "", nil),
		TelegramWebhookSecret: "tg-secret",
		BotStart:              f,
		OnAlert:               func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) },
	})

	w := postTelegram(t, h, telegramMessage(t, 43, operatorChatID, "private", "/status", "/status"))
	drain(t, f)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", w.Code)
	}
	if calls := tgAPI.calls(); len(calls) != 1 || calls[0] != "sendMessage" {
		t.Fatalf("/status Bot API calls = %v, want one sendMessage", calls)
	}
	if bridge.count() != 0 {
		t.Fatal("/status was forwarded")
	}
}

func TestTelegramStartFastWhenBridgeDown(t *testing.T) {
	recordTelegramAPI(t)
	// release unblocks the hanging handler before srv.Close, which waits for
	// in-flight handlers; the client giving up does not always reach it.
	release := make(chan struct{})
	for name, handler := range map[string]http.HandlerFunc{
		"500": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) },
		"hanging": func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(handler)
			defer srv.Close()
			defer func() {
				if name == "hanging" {
					close(release)
				}
			}()
			f := newForwarder(t, srv.URL, botstart.Config{
				Backoff:        []time.Duration{20 * time.Millisecond, 20 * time.Millisecond},
				AttemptTimeout: 100 * time.Millisecond,
				Deadline:       time.Second,
			})
			h := startRouter(t, f)
			failed := testutil.ToFloat64(metrics.BotStartForward.WithLabelValues("failed"))

			start := time.Now()
			w := postTelegram(t, h, telegramMessage(t, 44, clientChatID, "private", "/start", "/start"))
			if d := time.Since(start); d > 50*time.Millisecond {
				t.Fatalf("webhook took %v with the bridge %s", d, name)
			}
			if w.Code != http.StatusOK {
				t.Fatalf("status %d, want 200", w.Code)
			}
			drain(t, f)
			if got := testutil.ToFloat64(metrics.BotStartForward.WithLabelValues("failed")) - failed; got != 1 {
				t.Fatalf("failed delta = %v, want 1", got)
			}
		})
	}
}

// A disabled forwarder still consumes /start: 200, counted as disabled,
// nothing else happens.
func TestTelegramStartWithForwardingDisabled(t *testing.T) {
	tgAPI := recordTelegramAPI(t)
	f, err := botstart.New(botstart.Config{})
	if err != nil {
		t.Fatal(err)
	}
	disabled := testutil.ToFloat64(metrics.BotStartForward.WithLabelValues("disabled"))
	w := postTelegram(t, startRouter(t, f), telegramMessage(t, 45, clientChatID, "private", "/start", "/start"))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", w.Code)
	}
	if got := testutil.ToFloat64(metrics.BotStartForward.WithLabelValues("disabled")) - disabled; got != 1 {
		t.Fatalf("disabled delta = %v, want 1", got)
	}
	if calls := tgAPI.calls(); len(calls) != 0 {
		t.Fatalf("Bot API calls: %v", calls)
	}
}

// Nothing the client sent — text, payload, chat id — nor the bridge token
// reaches the log, on the /start path or the non-allowlisted-chat path.
func TestTelegramStartLogsCarryNoClientData(t *testing.T) {
	recordTelegramAPI(t)
	logs := captureLogs(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	f := newForwarder(t, srv.URL, botstart.Config{})
	h := startRouter(t, f)

	postTelegram(t, h, telegramMessage(t, 46, clientChatID, "private", "/start secretpayload", "/start"))
	postTelegram(t, h, telegramMessage(t, 47, clientChatID, "private", "/pause privatetext", "/pause"))
	drain(t, f)

	out := logs.String()
	if !strings.Contains(out, "non-allowlisted") {
		t.Fatalf("expected the non-allowlisted warning in the log:\n%s", out)
	}
	for _, leak := range []string{"secretpayload", "privatetext", "777000123", botStartToken} {
		if strings.Contains(out, leak) {
			t.Fatalf("log leaked %q:\n%s", leak, out)
		}
	}
}

// lockedBuffer collects log output written from the forwarder goroutine.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func captureLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}
