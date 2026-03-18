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
	botToken        string
	chatID          string
	openClawBotUser string
	httpClient      *http.Client
}

// NewTelegram creates a new Telegram notifier.
func NewTelegram(botToken, chatID, openClawBotUser string) *Telegram {
	if openClawBotUser == "" {
		openClawBotUser = "@mctl_me_bot"
	}
	return &Telegram{
		botToken:        botToken,
		chatID:          chatID,
		openClawBotUser: openClawBotUser,
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
	msg := fmt.Sprintf(`%s %s in %s/%s [%s]
%s
ID: <code>%s</code>

Ask %s: "explain incident %s"`,
		icon, t.Type, t.Tenant, t.Service, t.Severity,
		escapeHTML(t.Summary), t.ID[:8],
		tg.openClawBotUser, t.ID[:8])

	return tg.sendMessage(msg)
}

// SendDiagnosis notifies about a completed diagnosis.
func (tg *Telegram) SendDiagnosis(t *ticket.Ticket, diagnosis, confidence, action string) error {
	confIcon := confidenceIcon(confidence)
	msg := fmt.Sprintf(`🔍 %s/%s — %s %s
%s
%s
ID: <code>%s</code>

Ask %s: "explain incident %s"`,
		t.Tenant, t.Service, confIcon, confidence,
		escapeHTML(diagnosis), action, t.ID[:8],
		tg.openClawBotUser, t.ID[:8])

	return tg.sendMessage(msg)
}

// SendPRCreated notifies about a PR being created.
func (tg *Telegram) SendPRCreated(t *ticket.Ticket, prURL, summary string) error {
	msg := fmt.Sprintf(`🔧 %s/%s — fix PR
%s
%s

<code>/approve %s</code> | <code>/reject %s reason</code>

Ask %s: "explain incident %s"`,
		t.Tenant, t.Service, escapeHTML(summary), prURL, t.ID[:8], t.ID[:8],
		tg.openClawBotUser, t.ID[:8])

	return tg.sendMessage(msg)
}

// SendPRAutoMerged notifies that a PR was auto-merged (informational, no action needed).
func (tg *Telegram) SendPRAutoMerged(t *ticket.Ticket, prURL, summary string) error {
	msg := fmt.Sprintf("✅ AUTO-MERGED %s/%s\n%s\n%s\nID: <code>%s</code>",
		t.Tenant, t.Service, escapeHTML(summary), prURL, t.ID[:8])
	return tg.sendMessage(msg)
}

// SendPRNeedsReview notifies that a PR needs human review, tagging the escalation contact.
func (tg *Telegram) SendPRNeedsReview(t *ticket.Ticket, prURL, summary, escalationTag, reason string) error {
	msg := fmt.Sprintf("⚠️ REVIEW NEEDED %s/%s %s\n%s\n%s",
		t.Tenant, t.Service, escalationTag, escapeHTML(summary), prURL)
	if reason != "" {
		msg += "\n" + escapeHTML(reason)
	}
	msg += fmt.Sprintf("\n\n<code>/approve %s</code> | <code>/reject %s reason</code>", t.ID[:8], t.ID[:8])
	msg += fmt.Sprintf("\n\nAsk %s: \"explain incident %s\"", tg.openClawBotUser, t.ID[:8])
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
