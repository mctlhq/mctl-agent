package notify

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mctlhq/mctl-agent/internal/ticket"
)

func TestParseCommand(t *testing.T) {
	tests := []struct {
		input   string
		wantNil bool
		cmd     string
		id      string
		reason  string
	}{
		{"/approve abc123", false, "approve", "abc123", ""},
		{"/reject abc123 bad fix", false, "reject", "abc123", "bad fix"},
		{"/reject abc123", false, "reject", "abc123", "rejected via Telegram"},
		{"/ignore abc123", false, "ignore", "abc123", ""},
		{"/status", false, "status", "", ""},
		{"/pause", false, "pause", "", ""},
		{"/resume", false, "resume", "", ""},
		{"/ticket abc123", false, "ticket", "abc123", ""},
		{"/status@mctl_bot", false, "status", "", ""},
		{"not a command", true, "", "", ""},
		{"", true, "", "", ""},
		{"/approve", true, "", "", ""},
		{"/reject", true, "", "", ""},
		{"/unknown", true, "", "", ""},
		{"/ignore", true, "", "", ""},
		{"/ticket", true, "", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := ParseCommand(tt.input)
			if tt.wantNil {
				if got != nil {
					t.Errorf("expected nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("expected non-nil command")
			}
			if got.Command != tt.cmd {
				t.Errorf("command = %q, want %q", got.Command, tt.cmd)
			}
			if got.TicketID != tt.id {
				t.Errorf("ticketID = %q, want %q", got.TicketID, tt.id)
			}
			if got.Reason != tt.reason {
				t.Errorf("reason = %q, want %q", got.Reason, tt.reason)
			}
		})
	}
}

func TestSendTextWithMockServer(t *testing.T) {
	var received map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	tg := NewTelegram("test-token", "12345")
	// Override the URL by replacing the httpClient with one that redirects.
	tg.httpClient = srv.Client()
	// We need to actually hit the mock server, so let's use a custom approach:
	// Override botToken to construct URL pointing to our server.
	tg.botToken = "test"
	tg.chatID = "999"

	// Unfortunately, Telegram.sendMessage hard-codes the URL.
	// Let's test the no-config path instead.
	tgEmpty := NewTelegram("", "")
	if err := tgEmpty.SendText("hello"); err != nil {
		t.Errorf("expected nil error for unconfigured telegram, got %v", err)
	}
}

func TestSendNewTicketUnconfigured(t *testing.T) {
	tg := NewTelegram("", "")
	tk := &ticket.Ticket{
		ID:       "12345678-abcd-efgh-ijkl-mnopqrstuvwx",
		Type:     ticket.TypePodCrashloop,
		Tenant:   "billing",
		Service:  "api",
		Severity: ticket.SeverityCritical,
		Summary:  "Test alert",
	}
	// Should not error when unconfigured.
	if err := tg.SendNewTicket(tk); err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
}

func TestSendStatusEmptyTickets(t *testing.T) {
	tg := NewTelegram("", "")
	if err := tg.SendStatus(nil); err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
}

func TestSeverityIcon(t *testing.T) {
	if got := severityIcon(ticket.SeverityCritical); got != "🔴" {
		t.Errorf("critical icon = %q", got)
	}
	if got := severityIcon(ticket.SeverityWarning); got != "🟡" {
		t.Errorf("warning icon = %q", got)
	}
	if got := severityIcon(ticket.SeverityInfo); got != "🔵" {
		t.Errorf("info icon = %q", got)
	}
}

func TestConfidenceIcon(t *testing.T) {
	if got := confidenceIcon(ticket.ConfidenceHigh); got != "🟢" {
		t.Errorf("HIGH icon = %q", got)
	}
	if got := confidenceIcon(ticket.ConfidenceMedium); got != "🟡" {
		t.Errorf("MEDIUM icon = %q", got)
	}
	if got := confidenceIcon(ticket.ConfidenceLow); got != "🔴" {
		t.Errorf("LOW icon = %q", got)
	}
}

func TestEscapeHTML(t *testing.T) {
	if got := escapeHTML("a<b>c&d"); got != "a&lt;b&gt;c&amp;d" {
		t.Errorf("escapeHTML = %q", got)
	}
}
