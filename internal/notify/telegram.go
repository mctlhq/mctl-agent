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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/mctlhq/mctl-agent/internal/ticket"
)

// Telegram handles sending and receiving Telegram messages.
type Telegram struct {
	botToken      string
	chatID        string            // global default fallback
	tenantChatIDs map[string]string // per-tenant routing (admins/labs/ovk → chat_id)
	httpClient    *http.Client
	apiBase       string

	// pending holds the header of a ticket announced with HoldNewTicket and
	// not yet sent, keyed by ticket ID. The next message about that ticket
	// carries it, so an incident is one message instead of a "new ticket"
	// line followed seconds later by its diagnosis.
	mu      sync.Mutex
	pending map[string]string
}

// NewTelegram creates a new Telegram notifier.
//
// chatID is the global default — used when a notification has no tenant
// context (SendText, SendStatus, SendDailyDigest) or when tenantChatIDs
// has no entry for the ticket's tenant. Pass nil/empty tenantChatIDs to
// preserve the previous single-chat behaviour.
func NewTelegram(botToken, chatID string, tenantChatIDs map[string]string) *Telegram {
	return &Telegram{
		botToken:      botToken,
		chatID:        chatID,
		tenantChatIDs: tenantChatIDs,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		apiBase: "https://api.telegram.org",
		pending: map[string]string{},
	}
}

// WithAPIBase points the notifier at another Bot API root. Tests use it to
// read what would have been sent.
func (tg *Telegram) WithAPIBase(base string) *Telegram {
	tg.apiBase = strings.TrimRight(base, "/")
	return tg
}

// chatIDFor returns the chat ID to use for a notification originating from
// the given tenant. Falls back to the global default chatID when there's
// no per-tenant override (preserves previous behaviour for unconfigured
// tenants and for empty/unknown tenant strings).
func (tg *Telegram) chatIDFor(tenant string) string {
	if tenant != "" && len(tg.tenantChatIDs) > 0 {
		if id, ok := tg.tenantChatIDs[tenant]; ok && id != "" {
			return id
		}
	}
	return tg.chatID
}

// TelegramCommand represents a parsed inbound Telegram command.
type TelegramCommand struct {
	Command  string // approve, reject, ignore, status, pause, resume, ticket
	TicketID string
	Reason   string
}

// ticketHeader is the "new ticket" line: what fired, where, and the alert's
// own summary.
func ticketHeader(t *ticket.Ticket) string {
	return fmt.Sprintf("%s %s in %s/%s [%s]\n%s",
		severityIcon(t.Severity), t.Type, t.Tenant, t.Service, t.Severity,
		escapeHTML(t.Summary))
}

// SendNewTicket notifies about a new ticket.
func (tg *Telegram) SendNewTicket(t *ticket.Ticket) error {
	msg := fmt.Sprintf("%s\nID: <code>%s</code>", ticketHeader(t), t.ID[:8])
	return tg.doSendTo(tg.chatIDFor(t.Tenant), msg)
}

// HoldNewTicket records the ticket's header instead of sending it: the next
// Send* about the same ticket leads with it. The caller must pair it with
// FlushNewTicket, which sends the header alone if nothing carried it, so a
// header is never held past the ticket's processing.
func (tg *Telegram) HoldNewTicket(t *ticket.Ticket) {
	tg.mu.Lock()
	defer tg.mu.Unlock()
	if tg.pending == nil {
		tg.pending = map[string]string{}
	}
	tg.pending[t.ID] = ticketHeader(t)
}

// FlushNewTicket sends a held header that no later message carried, as the
// plain new-ticket notification. A ticket whose processing stopped early is
// still announced.
func (tg *Telegram) FlushNewTicket(t *ticket.Ticket) error {
	if _, ok := tg.takeHeader(t.ID); !ok {
		return nil
	}
	return tg.SendNewTicket(t)
}

func (tg *Telegram) takeHeader(id string) (string, bool) {
	tg.mu.Lock()
	defer tg.mu.Unlock()
	h, ok := tg.pending[id]
	delete(tg.pending, id)
	return h, ok
}

// withHeader prefixes msg with the ticket's held header, if there is one, and
// releases it.
func (tg *Telegram) withHeader(t *ticket.Ticket, msg string) string {
	if h, ok := tg.takeHeader(t.ID); ok {
		return h + "\n\n" + msg
	}
	return msg
}

// SendDiagnosis notifies about a completed diagnosis.
func (tg *Telegram) SendDiagnosis(t *ticket.Ticket, diagnosis, confidence, action string) error {
	confIcon := confidenceIcon(confidence)
	msg := fmt.Sprintf(`🔍 %s/%s — %s %s
%s
%s
ID: <code>%s</code>`,
		t.Tenant, t.Service, confIcon, confidence,
		escapeHTML(diagnosis), action, t.ID[:8])

	return tg.doSendTo(tg.chatIDFor(t.Tenant), tg.withHeader(t, msg))
}

// SendPRCreated notifies about a PR being created.
func (tg *Telegram) SendPRCreated(t *ticket.Ticket, prURL, summary string) error {
	msg := fmt.Sprintf(`🔧 %s/%s — fix PR
%s
%s

<code>/approve %s</code> | <code>/reject %s reason</code>`,
		t.Tenant, t.Service, escapeHTML(summary), prURL, t.ID[:8], t.ID[:8])

	return tg.doSendTo(tg.chatIDFor(t.Tenant), tg.withHeader(t, msg))
}

// SendPRAutoMerged notifies that a PR was auto-merged (informational, no action needed).
func (tg *Telegram) SendPRAutoMerged(t *ticket.Ticket, prURL, summary string) error {
	msg := fmt.Sprintf("✅ AUTO-MERGED %s/%s\n%s\n%s\nID: <code>%s</code>",
		t.Tenant, t.Service, escapeHTML(summary), prURL, t.ID[:8])
	return tg.doSendTo(tg.chatIDFor(t.Tenant), tg.withHeader(t, msg))
}

// SendPRNeedsReview notifies that a PR needs human review, tagging the escalation contact.
func (tg *Telegram) SendPRNeedsReview(t *ticket.Ticket, prURL, summary, escalationTag, reason string) error {
	msg := fmt.Sprintf("⚠️ REVIEW NEEDED %s/%s %s\n%s\n%s",
		t.Tenant, t.Service, escalationTag, escapeHTML(summary), prURL)
	if reason != "" {
		msg += "\n" + escapeHTML(reason)
	}
	msg += fmt.Sprintf("\n\n<code>/approve %s</code> | <code>/reject %s reason</code>", t.ID[:8], t.ID[:8])
	return tg.doSendTo(tg.chatIDFor(t.Tenant), tg.withHeader(t, msg))
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

	return tg.doSendTo(tg.chatIDFor(t.Tenant), sb.String())
}

// SendDailyDigest sends a morning summary of ticket and PR activity.
func (tg *Telegram) SendDailyDigest(open []*ticket.Ticket, resolved24h, prsMerged24h int) error {
	var sb strings.Builder
	fmt.Fprintf(&sb, "<b>Daily Digest</b>\n\n")
	fmt.Fprintf(&sb, "Open incidents: <b>%d</b>\n", len(open))
	fmt.Fprintf(&sb, "Resolved (24h): <b>%d</b>\n", resolved24h)
	fmt.Fprintf(&sb, "PRs merged (24h): <b>%d</b>\n", prsMerged24h)

	critical := 0
	for _, t := range open {
		if t.Severity == ticket.SeverityCritical {
			critical++
		}
	}
	if critical > 0 {
		fmt.Fprintf(&sb, "Critical: <b>%d</b>\n", critical)
	}

	if len(open) > 0 {
		fmt.Fprintf(&sb, "\n<b>Active:</b>\n")
		for _, t := range open {
			icon := severityIcon(t.Severity)
			fmt.Fprintf(&sb, "%s <code>%s</code> %s/%s — %s\n",
				icon, t.ID[:8], t.Tenant, t.Service, t.Type)
		}
	} else {
		fmt.Fprintf(&sb, "\nAll clear — no open incidents.")
	}

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
	case "digest":
		return &TelegramCommand{Command: "digest"}
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
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		Text string `json:"text"`
		// Date is the message time as Unix seconds, set by Telegram.
		Date int64 `json:"date"`
		From *struct {
			ID int64 `json:"id"`
		} `json:"from"`
		Chat struct {
			ID   int64  `json:"id"`
			Type string `json:"type"`
		} `json:"chat"`
		Entities []TelegramMessageEntity `json:"entities"`
	} `json:"message"`
}

// TelegramMessageEntity is one entity of a message. Offset and Length are
// counted in UTF-16 code units, as the Bot API defines them.
type TelegramMessageEntity struct {
	Type   string `json:"type"`
	Offset int    `json:"offset"`
	Length int    `json:"length"`
}

// IsPrivateChat reports whether the update carries a message from a
// one-to-one chat with the bot.
func IsPrivateChat(update TelegramUpdate) bool {
	return update.Message != nil && update.Message.Chat.Type == "private"
}

// IsStartCommand reports whether the message is the /start bot command:
// its first entity is a bot_command at offset 0 whose text, with any
// "@botname" suffix removed, equals /start ignoring case. Trailing
// arguments ("/start onboarding") do not matter; "/startx" and plain text
// that merely begins with "/start" but carries no bot_command entity do not
// match. Any "@suffix" is stripped without comparing it to this bot's
// username, which notify.Telegram does not know: "/start@SomeOtherBot" typed
// into the DM with this bot is accepted as a /start too. That is harmless
// because only private chats are acted on, so the sender is already talking
// to this bot one-to-one.
func IsStartCommand(update TelegramUpdate) bool {
	m := update.Message
	if m == nil || len(m.Entities) == 0 {
		return false
	}
	e := m.Entities[0]
	if e.Type != "bot_command" || e.Offset != 0 || e.Length <= 0 {
		return false
	}
	units := utf16.Encode([]rune(m.Text))
	if e.Length > len(units) {
		return false
	}
	token := string(utf16.Decode(units[:e.Length]))
	if at := strings.IndexByte(token, '@'); at >= 0 {
		token = token[:at]
	}
	return strings.EqualFold(token, "/start")
}

// HasChatAllowlist reports whether at least one chat ID is configured.
// When false, CommandChatAllowed runs in open mode — acceptable for local
// development only, so callers running with production auth enabled must
// refuse commands instead of trusting the open mode.
func (tg *Telegram) HasChatAllowlist() bool {
	return tg != nil && (tg.chatID != "" || len(tg.tenantChatIDs) > 0)
}

// CommandChatAllowed reports whether inbound bot commands from this chat
// should be executed. An unconfigured notifier (no chat IDs) allows all
// chats so unit tests keep working; production always sets TELEGRAM_CHAT_ID.
func (tg *Telegram) CommandChatAllowed(chatID int64) bool {
	if tg == nil {
		return false
	}
	if !tg.HasChatAllowlist() {
		return true
	}
	got := strconv.FormatInt(chatID, 10)
	if tg.chatID != "" && got == tg.chatID {
		return true
	}
	for _, id := range tg.tenantChatIDs {
		if id == got {
			return true
		}
	}
	return false
}

// SetWebhook registers the public Telegram webhook. secret_token is the
// value Telegram will send back as X-Telegram-Bot-Api-Secret-Token.
func (tg *Telegram) SetWebhook(ctx context.Context, publicBaseURL, secretToken string) error {
	if tg == nil || tg.botToken == "" || strings.TrimSpace(publicBaseURL) == "" {
		return nil
	}
	payload := map[string]string{
		"url": strings.TrimRight(publicBaseURL, "/") + "/api/v1/telegram",
	}
	if secretToken != "" {
		payload["secret_token"] = secretToken
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("telegram setWebhook: encode: %w", err)
	}
	url := fmt.Sprintf("%s/bot%s/setWebhook", tg.apiBase, tg.botToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("telegram setWebhook: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := tg.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("telegram setWebhook: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("telegram setWebhook returned %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

func (tg *Telegram) sendMessage(text string) error {
	return tg.doSend(text)
}

// doSend posts to the global default chat. Use doSendTo when the
// notification has tenant context — that variant routes per tenant.
func (tg *Telegram) doSend(text string) error {
	return tg.doSendTo(tg.chatID, text)
}

// doSendTo posts to a specific chat. The empty-config short-circuit treats
// missing botToken or missing chatID as a no-op (matches previous behaviour
// when the agent runs without Telegram configured).
func (tg *Telegram) doSendTo(chatID, text string) error {
	if tg.botToken == "" || chatID == "" {
		slog.Debug("telegram: skipping send (not configured)", "text_len", len(text))
		return nil
	}

	payload := map[string]any{
		"chat_id":    chatID,
		"text":       text,
		"parse_mode": "HTML",
	}
	body, _ := json.Marshal(payload)

	url := fmt.Sprintf("%s/bot%s/sendMessage", tg.apiBase, tg.botToken)
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
