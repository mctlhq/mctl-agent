package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mctlhq/mctl-agent/internal/fixer"
	"github.com/mctlhq/mctl-agent/internal/gitopspath"
	"github.com/mctlhq/mctl-agent/internal/notify"
	"github.com/mctlhq/mctl-agent/internal/pipeline"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

const (
	guardOperatorChat = 100
	guardAcmeChat     = 200
	guardOtherChat    = 300
)

// commandHarness drives the real Telegram webhook route with a fake Bot API
// (records replies) and a fake GitHub API (records PR merges).
type commandHarness struct {
	t      *testing.T
	store  *ticket.Store
	pipe   *pipeline.Pipeline
	router http.Handler
	opts   Options

	mu      sync.Mutex
	replies []string
	merged  []int
}

func newCommandHarness(t *testing.T) *commandHarness {
	t.Helper()
	h := &commandHarness{t: t, store: newTestStore(t)}
	h.pipe = newTestPipeline(t, h.store)

	bot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			Text string `json:"text"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &msg)
		h.mu.Lock()
		h.replies = append(h.replies, msg.Text)
		h.mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	t.Cleanup(bot.Close)

	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var n int
		if _, err := fmt.Sscanf(r.URL.Path, "/repos/o/r/pulls/%d/merge", &n); err == nil && r.Method == http.MethodPut {
			h.mu.Lock()
			h.merged = append(h.merged, n)
			h.mu.Unlock()
			_, _ = w.Write([]byte(`{"merged":true}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(gh.Close)

	gf := fixer.NewGitHubFixer("x", "", "o", "r", h.store, false, 10, 10, gitopspath.Allowlist{})
	if err := gf.SetBaseURL(gh.URL + "/"); err != nil {
		t.Fatal(err)
	}

	tg := notify.NewTelegram("token", fmt.Sprint(guardOperatorChat), map[string]string{
		"acme":  fmt.Sprint(guardAcmeChat),
		"other": fmt.Sprint(guardOtherChat),
	}).WithAPIBase(bot.URL)

	h.opts = Options{
		Store:                 h.store,
		Pipeline:              h.pipe,
		Telegram:              tg,
		GitHub:                gf,
		TelegramWebhookSecret: "tg-secret",
		OnAlert:               func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) },
	}
	h.router = NewRouter(h.opts)
	return h
}

func (h *commandHarness) create(tenant, status string, pr int) *ticket.Ticket {
	h.t.Helper()
	tk := &ticket.Ticket{Source: ticket.SourceManual, Type: ticket.TypeGeneric, Tenant: tenant, Service: "api", Status: status, PRNumber: pr}
	if err := h.store.Create(context.Background(), tk); err != nil {
		h.t.Fatal(err)
	}
	return tk
}

func (h *commandHarness) send(chat int64, text string) {
	h.t.Helper()
	body, _ := json.Marshal(map[string]any{"message": map[string]any{"text": text, "chat": map[string]any{"id": chat}}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/telegram", bytes.NewReader(body))
	req.Header.Set("X-Telegram-Bot-Api-Secret-Token", "tg-secret")
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		h.t.Fatalf("%s: status %d", text, w.Code)
	}
}

func (h *commandHarness) mergedPRs() []int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]int(nil), h.merged...)
}

func (h *commandHarness) lastReply() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.replies) == 0 {
		return ""
	}
	return h.replies[len(h.replies)-1]
}

func (h *commandHarness) status(id string) string {
	h.t.Helper()
	tk, err := h.store.Get(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return tk.Status
}

// A short ID must resolve however old the ticket is. The old lookup scanned
// the newest 100 tickets only, so the 101st-newest answered "not found".
func TestTelegramApproveFindsTicketOlderThanNewest100(t *testing.T) {
	h := newCommandHarness(t)
	old := h.create("acme", ticket.StatusFixProposed, 7)
	for i := 0; i < 100; i++ {
		h.create("acme", ticket.StatusOpen, 0)
	}

	h.send(guardOperatorChat, "/approve "+old.ID[:8])

	if got := h.mergedPRs(); len(got) != 1 || got[0] != 7 {
		t.Fatalf("merged PRs = %v, want [7]; last reply %q", got, h.lastReply())
	}
	if st := h.status(old.ID); st != ticket.StatusFixApplied {
		t.Fatalf("status = %q, want %q", st, ticket.StatusFixApplied)
	}
}

// /approve merges a PR, so it may only act on a ticket whose fix is still a
// proposal. Resolved and suppressed tickets keep their PR number.
func TestTelegramApproveRequiresFixProposed(t *testing.T) {
	for _, st := range []string{ticket.StatusResolved, ticket.StatusSuppressed, ticket.StatusOpen, ticket.StatusFixApplied} {
		t.Run(st, func(t *testing.T) {
			h := newCommandHarness(t)
			tk := h.create("acme", st, 9)

			h.send(guardOperatorChat, "/approve "+tk.ID[:8])

			if got := h.mergedPRs(); len(got) != 0 {
				t.Fatalf("merged PRs = %v for a %s ticket, want none", got, st)
			}
			if got := h.status(tk.ID); got != st {
				t.Fatalf("status changed to %q, want %q", got, st)
			}
			if r := h.lastReply(); !strings.Contains(r, "only a fix_proposed ticket can be approved") {
				t.Fatalf("reply = %q", r)
			}
		})
	}
}

// A tenant chat may only act on its own tenant's tickets; another tenant's
// ticket reads as not found. The operator chat sees every tenant.
func TestTelegramCommandsScopedToChatTenant(t *testing.T) {
	h := newCommandHarness(t)
	tk := h.create("acme", ticket.StatusFixProposed, 11)

	for _, cmd := range []string{"/approve ", "/reject ", "/ignore ", "/ticket "} {
		h.send(guardOtherChat, cmd+tk.ID[:8]+" reason")
		if r := h.lastReply(); !strings.Contains(r, "Ticket not found") {
			t.Fatalf("%s from another tenant's chat: reply %q, want not found", cmd, r)
		}
		h.send(guardOtherChat, cmd+tk.ID)
		if r := h.lastReply(); !strings.Contains(r, "Ticket not found") {
			t.Fatalf("%s full ID from another tenant's chat: reply %q, want not found", cmd, r)
		}
	}
	if got := h.mergedPRs(); len(got) != 0 {
		t.Fatalf("another tenant's chat merged %v", got)
	}
	if st := h.status(tk.ID); st != ticket.StatusFixProposed {
		t.Fatalf("another tenant's chat changed status to %q", st)
	}

	h.send(guardAcmeChat, "/approve "+tk.ID[:8])
	if got := h.mergedPRs(); len(got) != 1 || got[0] != 11 {
		t.Fatalf("own tenant's chat: merged %v, want [11]; reply %q", got, h.lastReply())
	}
}

// Commands that act on the whole agent are operator-only.
func TestTelegramTenantChatCannotPausePipeline(t *testing.T) {
	h := newCommandHarness(t)
	h.send(guardAcmeChat, "/pause")
	if h.pipe.IsPaused() {
		t.Fatal("a tenant chat paused the pipeline")
	}
	h.pipe.Pause()
	h.send(guardAcmeChat, "/resume")
	if !h.pipe.IsPaused() {
		t.Fatal("a tenant chat resumed the pipeline")
	}
	h.send(guardAcmeChat, "/digest")
	if r := h.lastReply(); !strings.Contains(r, "only available in the operator chat") {
		t.Fatalf("tenant /digest reply %q, want operator-only refusal", r)
	}
	h.pipe.Resume()
	h.send(guardOperatorChat, "/pause")
	if !h.pipe.IsPaused() {
		t.Fatal("the operator chat could not pause the pipeline")
	}
}

// /status from a tenant chat lists only that tenant's tickets.
func TestTelegramStatusScopedToChatTenant(t *testing.T) {
	h := newCommandHarness(t)
	mine := h.create("acme", ticket.StatusOpen, 0)
	theirs := h.create("other", ticket.StatusOpen, 0)

	h.send(guardAcmeChat, "/status")
	r := h.lastReply()
	if !strings.Contains(r, mine.ID[:8]) || strings.Contains(r, theirs.ID[:8]) {
		t.Fatalf("tenant /status reply %q: want %s and not %s", r, mine.ID[:8], theirs.ID[:8])
	}
}

// The tenant command list is an allow-list: a command nobody has classified
// yet is operator-only, not open to tenant chats by default.
func TestTelegramUnclassifiedCommandIsOperatorOnly(t *testing.T) {
	h := newCommandHarness(t)
	handleTelegramCommand(context.Background(), &notify.TelegramCommand{Command: "some-future-command"}, h.opts,
		commandScope{tenant: "acme"})
	if r := h.lastReply(); !strings.Contains(r, "only available in the operator chat") {
		t.Fatalf("reply %q, want operator-only refusal", r)
	}
}
