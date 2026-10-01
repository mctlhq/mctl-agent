package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

const testForwardToken = "0123456789abcdef0123456789abcdef0123456789abcdef"

// Without TELEGRAM_WEBHOOK_SECRET a /start is unauthenticated input, so a
// fully configured forwarder must come up disabled, and say why.
func TestBotStartForwarderFailsClosedWithoutWebhookSecret(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	f, err := newBotStartForwarder("http://bridge.example/x", testForwardToken, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.Enabled() {
		t.Fatal("forwarder enabled without a webhook secret")
	}
	if !strings.Contains(buf.String(), "TELEGRAM_WEBHOOK_SECRET") {
		t.Fatalf("no warning naming TELEGRAM_WEBHOOK_SECRET:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), testForwardToken) {
		t.Fatal("log leaked the forward token")
	}

	f, err = newBotStartForwarder("http://bridge.example/x", testForwardToken, "tg-secret")
	if err != nil || !f.Enabled() {
		t.Fatalf("forwarder with a webhook secret: enabled=%v err=%v, want enabled", f.Enabled(), err)
	}
}

// A short token stays fatal whatever the webhook secret, so the guard above
// never hides a misconfiguration.
func TestBotStartForwarderShortTokenIsFatal(t *testing.T) {
	for _, secret := range []string{"", "tg-secret"} {
		if _, err := newBotStartForwarder("http://bridge.example/x", "short", secret); err == nil {
			t.Fatalf("short token accepted (webhook secret %q)", secret)
		}
	}
}
