package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/mctlhq/mctl-agent/internal/ticket"
)

// Telegram handles sending and receiving Telegram messages.
type Telegram struct {
	botToken   string
	chatID     string
	httpClient *http.Client
}

// NewTelegram creates a new Telegram notifier.
func NewTelegram(botToken, chatID string) *Telegram {
	return &Telegram{
		botToken: botToken,
		chatID:   chatID,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// TelegramCommand represents a parsed inbound Telegram command.
type TelegramCommand struct {
	Command  string // approve, reject, ignore, status, pause, resume, ticket
	TicketID string
	Reason   string
}

// SendNewTicket notifies about a new ticket.
func (tg *Telegram) SendNewTicket(t *ticket.Ticket) error {
	icon := severityIcon(t.Severity)
	msg := fmt.Sprintf(`%s <b>New Incident</b>

<b>Type:</b> %s
<b>Service:</b> %s/%s
<b>Severity:</b> %s
<b>Summary:</b> %s
<b>ID:</b> <code>%s</code>`,
		icon, t.Type, t.Tenant, t.Service, t.Severity, escapeHTML(t.Summary), t.ID[:8])

	return tg.sendMessage(msg)
}

// SendDiagnosis notifies about a completed diagnosis.
func (tg *Telegram) SendDiagnosis(t *ticket.Ticket, diagnosis, confidence, action string) error {
	confIcon := confidenceIcon(confidence)
	msg := fmt.Sprintf(`🔍 <b>Diagnosis Complete</b>

<b>Service:</b> %s/%s
<b>Confidence:</b> %s %s
<b>Analysis:</b> %s
<b>Action:</b> %s
<b>ID:</b> <code>%s</code>`,
		t.Tenant, t.Service, confIcon, confidence, escapeHTML(diagnosis), action, t.ID[:8])

	return tg.sendMessage(msg)
}

// SendPRCreated notifies about a PR being created.
func (tg *Telegram) SendPRCreated(t *ticket.Ticket, prURL, summary string) error {
	msg := fmt.Sprintf(`🔧 <b>Fix PR Created</b>

<b>Service:</b> %s/%s
<b>Fix:</b> %s
<b>PR:</b> %s

Commands:
<code>/approve %s</code> — merge PR
<code>/reject %s reason</code> — close PR`,
		t.Tenant, t.Service, escapeHTML(summary), prURL, t.ID[:8], t.ID[:8])

	return tg.sendMessage(msg)
}

// SendStatus sends a summary of open tickets.
func (tg *Telegram) SendStatus(tickets []*ticket.Ticket) error {
	if len(tickets) == 0 {
		return tg.sendMessage("✅ No open tickets")
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "📋 <b>Open Tickets (%d)</b>\n\n", len(tickets))
	for _, t := range tickets {
		icon := severityIcon(t.Severity)
		fmt.Fprintf(&sb, "%s <code>%s</code> %s/%s — %s [%s]\n",
			icon, t.ID[:8], t.Tenant, t.Service, t.Type, t.Status)
	}

	return tg.sendMessage(sb.String())
}

// SendTicketDetail sends full ticket detail.
func (tg *Telegram) SendTicketDetail(t *ticket.Ticket) error {
	var sb strings.Builder
	fmt.Fprintf(&sb, "📝 <b>Ticket %s</b>\n\n", t.ID[:8])
	fmt.Fprintf(&sb, "<b>Type:</b> %s\n", t.Type)
	fmt.Fprintf(&sb, "<b>Service:</b> %s/%s\n", t.Tenant, t.Service)
	fmt.Fprintf(&sb, "<b>Severity:</b> %s\n", t.Severity)
	fmt.Fprintf(&sb, "<b>Status:</b> %s\n", t.Status)
	fmt.Fprintf(&sb, "<b>Summary:</b> %s\n", escapeHTML(t.Summary))
	if t.Analysis != "" {
		fmt.Fprintf(&sb, "<b>Analysis:</b> %s\n", escapeHTML(t.Analysis))
	}
	if t.Confidence != "" {
		fmt.Fprintf(&sb, "<b>Confidence:</b> %s\n", t.Confidence)
	}
	if t.PRURL != "" {
		fmt.Fprintf(&sb, "<b>PR:</b> %s\n", t.PRURL)
	}
	fmt.Fprintf(&sb, "<b>Created:</b> %s\n", t.CreatedAt.Format(time.RFC3339))

	return tg.sendMessage(sb.String())
}

// SendText sends a plain text message.
func (tg *Telegram) SendText(text string) error {
	return tg.sendMessage(escapeHTML(text))
}

// ParseCommand parses an inbound Telegram message into a command.
func ParseCommand(text string) *TelegramCommand {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return nil
	}

	parts := strings.Fields(text)
	if len(parts) == 0 {
		return nil
	}

	cmd := strings.TrimPrefix(parts[0], "/")
	// Strip bot mention (e.g., /status@mybot).
	if idx := strings.Index(cmd, "@"); idx >= 0 {
		cmd = cmd[:idx]
	}

	switch cmd {
	case "approve":
		if len(parts) < 2 {
			return nil
		}
		return &TelegramCommand{Command: "approve", TicketID: parts[1]}
	case "reject":
		if len(parts) < 2 {
			return nil
		}
		reason := "rejected via Telegram"
		if len(parts) > 2 {
			reason = strings.Join(parts[2:], " ")
		}
		return &TelegramCommand{Command: "reject", TicketID: parts[1], Reason: reason}
	case "ignore":
		if len(parts) < 2 {
			return nil
		}
		return &TelegramCommand{Command: "ignore", TicketID: parts[1]}
	case "status":
		return &TelegramCommand{Command: "status"}
	case "pause":
		return &TelegramCommand{Command: "pause"}
	case "resume":
		return &TelegramCommand{Command: "resume"}
	case "ticket":
		if len(parts) < 2 {
			return nil
		}
		return &TelegramCommand{Command: "ticket", TicketID: parts[1]}
	default:
		return nil
	}
}

// TelegramUpdate is the Telegram Bot API update structure.
type TelegramUpdate struct {
	Message *struct {
		Text string `json:"text"`
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
	} `json:"message"`
}

func (tg *Telegram) sendMessage(text string) error {
	if tg.botToken == "" || tg.chatID == "" {
		slog.Debug("telegram: skipping send (not configured)", "text_len", len(text))
		return nil
	}

	payload := map[string]interface{}{
		"chat_id":    tg.chatID,
		"text":       text,
		"parse_mode": "HTML",
	}
	body, _ := json.Marshal(payload)

	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", tg.botToken)
	resp, err := tg.httpClient.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("telegram send: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("telegram returned %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

func severityIcon(s string) string {
	switch s {
	case ticket.SeverityCritical:
		return "🔴"
	case ticket.SeverityWarning:
		return "🟡"
	default:
		return "🔵"
	}
}

func confidenceIcon(c string) string {
	switch c {
	case ticket.ConfidenceHigh:
		return "🟢"
	case ticket.ConfidenceMedium:
		return "🟡"
	default:
		return "🔴"
	}
}

func escapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}
