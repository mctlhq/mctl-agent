package pipeline

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mctlhq/mctl-agent/internal/mctlclient"
	"github.com/mctlhq/mctl-agent/internal/notify"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

// botAPI records the text of every sendMessage the pipeline makes.
type botAPI struct {
	mu    sync.Mutex
	texts []string
}

func (b *botAPI) sent() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.texts...)
}

func newRecordingTelegram(t *testing.T) (*notify.Telegram, *botAPI) {
	t.Helper()
	b := &botAPI{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		b.mu.Lock()
		b.texts = append(b.texts, body.Text)
		b.mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return notify.NewTelegram("token", "1", nil).WithAPIBase(srv.URL), b
}

// An incident the agent cannot diagnose used to post twice: the bare ticket,
// then "No skill matched" seconds later. It is one message now, ticket first.
//
// Mutation check: call p.telegram.SendNewTicket(t) instead of HoldNewTicket
// and this sees two messages.
func TestUndiagnosedTicketIsOneMessage(t *testing.T) {
	p, store := newAsyncTestPipeline(t, 1)
	tg, bot := newRecordingTelegram(t)
	p.telegram = tg

	tk := &ticket.Ticket{
		Source:    ticket.SourceAlertManager,
		AlertName: "MctlAliceHigh5xxRatio",
		Type:      ticket.TypeGeneric,
		Tenant:    "platform",
		Service:   "MctlAliceHigh5xxRatio",
		Summary:   "mctl-alice: 17% of requests answered 5xx",
		Severity:  ticket.SeverityWarning,
		Status:    ticket.StatusOpen,
	}
	if err := store.Create(context.Background(), tk); err != nil {
		t.Fatal(err)
	}
	p.processTicketSync(context.Background(), tk)

	got := bot.sent()
	if len(got) != 1 {
		t.Fatalf("want one message per incident, got %d: %q", len(got), got)
	}
	if !strings.Contains(got[0], "17% of requests answered 5xx") || !strings.Contains(got[0], "No skill matched") {
		t.Fatalf("the message should carry both the ticket and the outcome, got %q", got[0])
	}
	if strings.Index(got[0], "5xx") > strings.Index(got[0], "No skill matched") {
		t.Fatalf("the ticket should lead, got %q", got[0])
	}
}

// Warning-level alerts that are diagnosed only (CPU throttling, a StatefulSet
// mid-rollout) stay out of the chat; the ticket still exists for the digest.
// The same alert at critical severity still posts.
//
// Mutation check: make isDigestOnlyAlert return false and the warning case
// posts one message.
func TestDigestOnlyWarningDoesNotPost(t *testing.T) {
	for _, tc := range []struct {
		severity string
		want     int
	}{
		{ticket.SeverityWarning, 0},
		{ticket.SeverityCritical, 1},
	} {
		t.Run(tc.severity, func(t *testing.T) {
			p, store := newAsyncTestPipeline(t, 1)
			tg, bot := newRecordingTelegram(t)
			p.telegram = tg

			tk := &ticket.Ticket{
				Source:    ticket.SourceAlertManager,
				AlertName: "CPUThrottlingHigh",
				Type:      ticket.TypeResourceLimit,
				Tenant:    "argocd",
				Service:   "argocd-repo-server",
				Summary:   "Processes experience elevated CPU throttling.",
				Severity:  tc.severity,
				Status:    ticket.StatusOpen,
			}
			if err := store.Create(context.Background(), tk); err != nil {
				t.Fatal(err)
			}
			p.processTicketSync(context.Background(), tk)

			if got := bot.sent(); len(got) != tc.want {
				t.Fatalf("want %d message(s), got %d: %q", tc.want, len(got), got)
			}
			stored, err := store.Get(context.Background(), tk.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Status == ticket.StatusOpen {
				t.Fatalf("the ticket should still be processed, status %q", stored.Status)
			}
		})
	}
}

// A ticket whose processing stops before any outcome (here the reload after
// evidence fails) is still announced: the deferred flush sends the held
// header on its own. The reload sets t to nil, which the flush must not read.
//
// Mutation check: flush `t` instead of `held` and this test panics; remove
// the deferred flush and it sees no message.
func TestTicketIsAnnouncedWhenProcessingStopsEarly(t *testing.T) {
	p, store := newAsyncTestPipeline(t, 1)
	tg, bot := newRecordingTelegram(t)
	p.telegram = tg
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			once.Do(func() { _ = store.Close() })
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	p.apiClient = mctlclient.NewClient(srv.URL, "test-token")

	tk := &ticket.Ticket{Source: ticket.SourceAlertManager, Type: ticket.TypePodCrashloop, Tenant: "billing", Service: "api", Summary: "crashloop", Severity: ticket.SeverityCritical, Status: ticket.StatusOpen}
	if err := store.Create(context.Background(), tk); err != nil {
		t.Fatal(err)
	}
	p.processTicketSync(context.Background(), tk)

	got := bot.sent()
	if len(got) != 1 || !strings.Contains(got[0], "crashloop") || !strings.Contains(got[0], "ID: <code>") {
		t.Fatalf("want the plain new-ticket message, got %q", got)
	}
}
