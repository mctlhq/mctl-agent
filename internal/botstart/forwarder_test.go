package botstart

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/mctlhq/mctl-agent/internal/metrics"
)

const testToken = "0123456789abcdef0123456789abcdef0123456789abcdef" // 48 chars

var outcomes = []string{"sent", "retry", "failed", "disabled", "rejected", "dropped"}

// snapshot reads every outcome counter so a test can assert deltas; the
// counters are process-global.
func snapshot() map[string]float64 {
	m := map[string]float64{}
	for _, o := range outcomes {
		m[o] = testutil.ToFloat64(metrics.BotStartForward.WithLabelValues(o))
	}
	return m
}

func assertDeltas(t *testing.T, before map[string]float64, want map[string]float64) {
	t.Helper()
	after := snapshot()
	for _, o := range outcomes {
		if got := after[o] - before[o]; got != want[o] {
			t.Errorf("outcome %q: delta %v, want %v", o, got, want[o])
		}
	}
}

// captureLogs routes slog to a buffer for the test's duration.
func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newForwarder(t *testing.T, url string) *Forwarder {
	t.Helper()
	f, err := New(Config{URL: url, Token: testToken, Backoff: []time.Duration{time.Millisecond, time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func wait(t *testing.T, f *Forwarder) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f.Wait(ctx)
	if ctx.Err() != nil {
		t.Fatal("forward did not finish")
	}
}

func TestForwardSendsExactlyThreeFields(t *testing.T) {
	var (
		mu     sync.Mutex
		body   []byte
		header http.Header
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		body, header = b, r.Header.Clone()
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	logs := captureLogs(t)
	before := snapshot()

	f := newForwarder(t, srv.URL)
	observed := time.Date(2026, 10, 1, 9, 30, 0, 0, time.FixedZone("x", 3*3600))
	f.Forward(987654321, 555000111, observed)
	wait(t, f)

	mu.Lock()
	defer mu.Unlock()
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("body has %d keys, want exactly 3: %s", len(got), body)
	}
	if got["update_id"] != float64(987654321) || got["telegram_id"] != float64(555000111) {
		t.Fatalf("ids wrong: %s", body)
	}
	if got["observed_at"] != "2026-10-01T06:30:00Z" {
		t.Fatalf("observed_at = %v, want RFC 3339 UTC of the message date", got["observed_at"])
	}
	if header.Get("Authorization") != "Bearer "+testToken {
		t.Fatal("missing or wrong bearer")
	}
	if header.Get("Content-Type") != "application/json" {
		t.Fatalf("content-type = %q", header.Get("Content-Type"))
	}
	assertDeltas(t, before, map[string]float64{"sent": 1})
	for _, leak := range []string{testToken, "987654321", "555000111"} {
		if strings.Contains(logs.String(), leak) {
			t.Fatalf("log leaked %q:\n%s", leak, logs.String())
		}
	}
}

func TestForwardRetriesOn5xxThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	before := snapshot()

	f := newForwarder(t, srv.URL)
	f.Forward(1, 2, time.Now())
	wait(t, f)

	if calls.Load() != 3 {
		t.Fatalf("calls = %d, want 3", calls.Load())
	}
	assertDeltas(t, before, map[string]float64{"retry": 2, "sent": 1})
}

func TestForwardGivesUpAfterThreeAttempts(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	logs := captureLogs(t)
	before := snapshot()

	f := newForwarder(t, srv.URL)
	f.Forward(31337, 4242, time.Now())
	wait(t, f)

	if calls.Load() != 3 {
		t.Fatalf("calls = %d, want 3", calls.Load())
	}
	assertDeltas(t, before, map[string]float64{"retry": 2, "failed": 1})
	for _, leak := range []string{testToken, "31337", "4242"} {
		if strings.Contains(logs.String(), leak) {
			t.Fatalf("log leaked %q:\n%s", leak, logs.String())
		}
	}
}

func TestForwardDoesNotRetry4xx(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusRequestEntityTooLarge, http.StatusFound} {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			if status == http.StatusFound {
				w.Header().Set("Location", "http://example.invalid/steal")
			}
			w.WriteHeader(status)
		}))
		before := snapshot()

		f := newForwarder(t, srv.URL)
		f.Forward(1, 2, time.Now())
		wait(t, f)
		srv.Close()

		if calls.Load() != 1 {
			t.Fatalf("status %d: calls = %d, want 1", status, calls.Load())
		}
		assertDeltas(t, before, map[string]float64{"rejected": 1})
	}
}

type failingTransport struct{ calls atomic.Int32 }

func (ft *failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	ft.calls.Add(1)
	return nil, errors.New("dial tcp: connection refused")
}

func TestForwardRetriesTransportErrors(t *testing.T) {
	ft := &failingTransport{}
	before := snapshot()
	f, err := New(Config{URL: "http://bridge.invalid/x", Token: testToken, Backoff: []time.Duration{time.Millisecond, time.Millisecond}, Transport: ft})
	if err != nil {
		t.Fatal(err)
	}
	f.Forward(1, 2, time.Now())
	wait(t, f)
	if ft.calls.Load() != 3 {
		t.Fatalf("calls = %d, want 3", ft.calls.Load())
	}
	assertDeltas(t, before, map[string]float64{"retry": 2, "failed": 1})
}

// A bridge that never answers must not hold anything up: Forward returns at
// once, and the background send ends at the deadline as failed.
func TestForwardHangingBridgeFailsAtDeadline(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	before := snapshot()

	f, err := New(Config{URL: srv.URL, Token: testToken, Backoff: []time.Duration{time.Millisecond, time.Millisecond}, AttemptTimeout: time.Minute, Deadline: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	f.Forward(1, 2, time.Now())
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("Forward blocked for %v", d)
	}
	wait(t, f)
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("send outlived its deadline: %v", d)
	}
	assertDeltas(t, before, map[string]float64{"failed": 1})
}

func TestForwardDisabledWhenUnset(t *testing.T) {
	for name, cfg := range map[string]Config{
		"nothing":  {},
		"no url":   {Token: testToken},
		"no token": {URL: "http://bridge.example/x"},
	} {
		f, err := New(cfg)
		if err != nil {
			t.Fatalf("%s: unexpected error %v", name, err)
		}
		if f.Enabled() {
			t.Fatalf("%s: forwarder enabled", name)
		}
		before := snapshot()
		f.Forward(1, 2, time.Now())
		assertDeltas(t, before, map[string]float64{"disabled": 1})
	}
	// A nil forwarder (no wiring at all) behaves the same.
	var f *Forwarder
	before := snapshot()
	f.Forward(1, 2, time.Now())
	assertDeltas(t, before, map[string]float64{"disabled": 1})
}

func TestNewRejectsShortTokenAndBadURL(t *testing.T) {
	short := strings.Repeat("a", MinTokenLen-1)
	if _, err := New(Config{URL: "http://bridge.example/x", Token: short}); err == nil {
		t.Fatal("short token accepted")
	}
	// Short is fatal even without a URL: a half-configured secret is a
	// mistake to surface, not a reason to stay quietly disabled.
	if _, err := New(Config{Token: short}); err == nil {
		t.Fatal("short token without URL accepted")
	}
	if _, err := New(Config{URL: "http://bridge.example/x", Token: strings.Repeat("a", MinTokenLen)}); err != nil {
		t.Fatalf("token of exactly %d characters rejected: %v", MinTokenLen, err)
	}
	for _, bad := range []string{"bridge.example/x", "ftp://bridge.example/x", "http:///x", "http://user:pw@bridge.example/x"} {
		_, err := New(Config{URL: bad, Token: testToken})
		if err == nil {
			t.Fatalf("URL %q accepted", bad)
		}
		if strings.Contains(err.Error(), testToken) || strings.Contains(err.Error(), "pw") {
			t.Fatalf("error leaks secret: %v", err)
		}
	}
	if _, err := New(Config{URL: "http://bridge.example/x", Token: short}); err != nil && strings.Contains(err.Error(), short) {
		t.Fatalf("error leaks the token: %v", err)
	}
}

func TestScrubRemovesToken(t *testing.T) {
	f := newForwarder(t, "http://bridge.example/x")
	got := f.scrub(errors.New("bad header Bearer " + testToken))
	if strings.Contains(got, testToken) {
		t.Fatalf("scrub kept the token: %q", got)
	}
}

// With the cap full, the next /start is shed and counted, the bridge never
// sees more than the cap at once, and every slot comes back afterwards.
func TestForwardCapsInFlight(t *testing.T) {
	const capN = 2
	var inFlight, peak atomic.Int32
	release := make(chan struct{})
	arrived := make(chan struct{}, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		arrived <- struct{}{}
		<-release
		inFlight.Add(-1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	before := snapshot()

	f, err := New(Config{URL: srv.URL, Token: testToken, MaxInFlight: capN, Backoff: []time.Duration{time.Millisecond, time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < capN; i++ {
		f.Forward(int64(i+1), 2, time.Now())
	}
	for i := 0; i < capN; i++ {
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			t.Fatal("capped sends never reached the bridge")
		}
	}
	f.Forward(99, 2, time.Now()) // cap+1: shed
	assertDeltas(t, before, map[string]float64{"dropped": 1})

	close(release)
	wait(t, f)
	if len(f.sem) != 0 {
		t.Fatalf("%d slots still held after every send finished", len(f.sem))
	}
	if p := peak.Load(); p > capN {
		t.Fatalf("bridge saw %d concurrent sends, cap %d", p, capN)
	}
	// The slots are free again: the next /start goes through.
	f.Forward(100, 2, time.Now())
	wait(t, f)
	assertDeltas(t, before, map[string]float64{"dropped": 1, "sent": capN + 1})
}

// Without an injected transport the forwarder owns one, capped to the
// in-flight limit, instead of sharing http.DefaultTransport.
func TestForwarderUsesDedicatedCappedTransport(t *testing.T) {
	f, err := New(Config{URL: "http://bridge.example/x", Token: testToken, MaxInFlight: 7})
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := f.client.Transport.(*http.Transport)
	if !ok || tr == http.DefaultTransport {
		t.Fatalf("transport = %T, want a dedicated *http.Transport", f.client.Transport)
	}
	if tr.MaxConnsPerHost != 7 {
		t.Fatalf("MaxConnsPerHost = %d, want 7", tr.MaxConnsPerHost)
	}
}

// The default deadline must leave room for every attempt against a bridge
// that times out, or the last retry is silently cut.
func TestDefaultDeadlineCoversAllAttempts(t *testing.T) {
	worst := time.Duration(len(defaultBackoff)+1) * defaultAttemptTimeout
	for _, b := range defaultBackoff {
		worst += b
	}
	f, err := New(Config{URL: "http://bridge.example/x", Token: testToken})
	if err != nil {
		t.Fatal(err)
	}
	if f.Deadline() <= worst {
		t.Fatalf("deadline %v does not cover the worst case %v", f.Deadline(), worst)
	}
}

// Shutdown is bounded by its context, and a send it cuts short still
// records a terminal outcome instead of vanishing from the metric. Forward
// after Shutdown is shed.
func TestShutdownCountsAbandonedSends(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	before := snapshot()

	f, err := New(Config{URL: srv.URL, Token: testToken, AttemptTimeout: time.Minute, Deadline: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	f.Forward(1, 2, time.Now())
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	f.Shutdown(ctx)
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("Shutdown took %v with a 100ms budget", d)
	}
	assertDeltas(t, before, map[string]float64{"failed": 1})
	if len(f.sem) != 0 {
		t.Fatalf("%d slots still held after Shutdown", len(f.sem))
	}

	f.Forward(3, 4, time.Now())
	assertDeltas(t, before, map[string]float64{"failed": 1, "dropped": 1})
}
