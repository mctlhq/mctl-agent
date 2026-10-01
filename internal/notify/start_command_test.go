package notify

import (
	"encoding/json"
	"testing"
	"unicode/utf16"
)

// startUpdate builds an update the way Telegram does: a bot_command entity
// covering the command token, its length in UTF-16 code units.
func startUpdate(t *testing.T, text, command, chatType string) TelegramUpdate {
	t.Helper()
	raw := map[string]any{
		"update_id": 1,
		"message": map[string]any{
			"text": text,
			"date": 1700000000,
			"chat": map[string]any{"id": 4242, "type": chatType},
		},
	}
	if command != "" {
		raw["message"].(map[string]any)["entities"] = []map[string]any{{
			"type": "bot_command", "offset": 0, "length": len(utf16.Encode([]rune(command))),
		}}
	}
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	var u TelegramUpdate
	if err := json.Unmarshal(b, &u); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestIsStartCommand(t *testing.T) {
	tests := []struct {
		name, text, command string
		want                bool
	}{
		{"lower", "/start", "/start", true},
		{"title", "/Start", "/Start", true},
		{"upper", "/START", "/START", true},
		{"bot suffix", "/start@MCTL_AI_bot", "/start@MCTL_AI_bot", true},
		{"upper with bot suffix", "/START@MCTL_AI_bot", "/START@MCTL_AI_bot", true},
		{"with payload", "/start onboarding", "/start", true},
		{"emoji payload", "/start 🚀🚀", "/start", true},
		{"longer command", "/startx", "/startx", false},
		{"other command", "/status", "/status", false},
		{"plain text", "start", "", false},
		{"slash text without entity", "/start", "", false},
		{"empty", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsStartCommand(startUpdate(t, tt.text, tt.command, "private")); got != tt.want {
				t.Fatalf("IsStartCommand(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

// Offsets and lengths are UTF-16 code units: a command after an emoji is
// not at offset 0, and a length that runs past the text is rejected rather
// than sliced.
func TestIsStartCommandEntityBounds(t *testing.T) {
	u := startUpdate(t, "/start", "/start", "private")
	u.Message.Entities[0].Offset = 1
	if IsStartCommand(u) {
		t.Fatal("entity not at offset 0 matched")
	}
	u = startUpdate(t, "/start", "/start", "private")
	u.Message.Entities[0].Length = 50
	if IsStartCommand(u) {
		t.Fatal("entity longer than the text matched")
	}
	u = startUpdate(t, "/start", "/start", "private")
	u.Message.Entities[0].Type = "bold"
	if IsStartCommand(u) {
		t.Fatal("non-command entity matched")
	}
	// "/stärt" is 6 runes and 6 UTF-16 units; the token must decode cleanly
	// and still not equal /start.
	if IsStartCommand(startUpdate(t, "/stärt", "/stärt", "private")) {
		t.Fatal("/stärt matched")
	}
	// "🚀/start": the emoji is two UTF-16 units, so Telegram reports the
	// command at offset 2. It is not a leading command.
	u = startUpdate(t, "🚀/start", "", "private")
	u.Message.Entities = []TelegramMessageEntity{{Type: "bot_command", Offset: 2, Length: 6}}
	if IsStartCommand(u) {
		t.Fatal("command after an emoji matched")
	}
	// Text after the command may hold astral-plane characters; the token is
	// still the first 6 UTF-16 units.
	u = startUpdate(t, "/start🚀", "", "private")
	u.Message.Entities = []TelegramMessageEntity{{Type: "bot_command", Offset: 0, Length: 6}}
	if !IsStartCommand(u) {
		t.Fatal("UTF-16 length not honoured")
	}
	if IsStartCommand(TelegramUpdate{}) {
		t.Fatal("update without message matched")
	}
}

func TestIsPrivateChat(t *testing.T) {
	for chatType, want := range map[string]bool{"private": true, "group": false, "supergroup": false, "channel": false, "": false} {
		if got := IsPrivateChat(startUpdate(t, "/start", "/start", chatType)); got != want {
			t.Fatalf("IsPrivateChat(%q) = %v, want %v", chatType, got, want)
		}
	}
}
