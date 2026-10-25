package notify

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mctlhq/mctl-agent/internal/ticket"
)

func recordingTelegram(t *testing.T) (*Telegram, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var texts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		texts = append(texts, body.Text)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return NewTelegram("token", "1", nil).WithAPIBase(srv.URL), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), texts...)
	}
}

func heldTicket() *ticket.Ticket {
	return &ticket.Ticket{
		ID:       "abcdef12-0000-0000-0000-000000000000",
		Type:     ticket.TypeGeneric,
		Tenant:   "labs",
		Service:  "mctl-alice",
		Severity: ticket.SeverityWarning,
		Summary:  "pod restarted 6 times",
	}
}

// A held header rides on the next message and is then gone, so the flush that
// follows sends nothing. Mutation check: drop the delete in takeHeader and the
// flush sends a second message.
func TestHeldHeaderRidesOnTheNextMessageOnce(t *testing.T) {
	tg, sent := recordingTelegram(t)
	tk := heldTicket()

	tg.HoldNewTicket(tk)
	if err := tg.SendPRNeedsReview(tk, "https://example/pr/1", "Bump memory", "@owner", ""); err != nil {
		t.Fatal(err)
	}
	if err := tg.FlushNewTicket(tk); err != nil {
		t.Fatal(err)
	}

	got := sent()
	if len(got) != 1 {
		t.Fatalf("want 1 message, got %d: %q", len(got), got)
	}
	if !strings.HasPrefix(got[0], "🟡 generic in labs/mctl-alice [warning]\npod restarted 6 times\n\n⚠️ REVIEW NEEDED") {
		t.Fatalf("header should lead the review message, got %q", got[0])
	}
}

// Nothing carried the header: the flush announces the ticket on its own,
// once.
func TestFlushOfAnUncarriedHeader(t *testing.T) {
	tg, sent := recordingTelegram(t)
	tk := heldTicket()

	tg.HoldNewTicket(tk)
	for range 2 {
		if err := tg.FlushNewTicket(tk); err != nil {
			t.Fatal(err)
		}
	}
	if got := sent(); len(got) != 1 || !strings.Contains(got[0], "ID: <code>abcdef12</code>") {
		t.Fatalf("flush should send the plain new-ticket message once, got %q", got)
	}
}

// A message that fails to send does not take the header with it: the flush
// still announces the ticket. Mutation check: drop the put-back in
// sendWithHeader and the flush sends nothing.
func TestFailedSendKeepsTheHeader(t *testing.T) {
	var mu sync.Mutex
	var texts []string
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		texts = append(texts, body.Text)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	tg := NewTelegram("token", "1", nil).WithAPIBase(srv.URL)
	tk := heldTicket()

	tg.HoldNewTicket(tk)
	if err := tg.SendDiagnosis(tk, "d", ticket.ConfidenceLow, "a"); err == nil {
		t.Fatal("the first send should fail")
	}
	if err := tg.FlushNewTicket(tk); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(texts) != 1 || !strings.Contains(texts[0], "pod restarted 6 times") {
		t.Fatalf("flush should announce the ticket after a failed send, got %q", texts)
	}
}
