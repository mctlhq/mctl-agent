package monitor

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGitHubWebhookRejectsMissingSecret(t *testing.T) {
	store := newTestStore(t)
	h := NewGitHubWebhookHandler(store, "", nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/github-webhook", bytes.NewReader([]byte(`{}`)))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("empty secret: status=%d, want 401", w.Code)
	}
}

func TestGitHubWebhookRejectsBadSignature(t *testing.T) {
	store := newTestStore(t)
	h := NewGitHubWebhookHandler(store, "whsec", nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/github-webhook", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature: status=%d, want 401", w.Code)
	}
}
