package api

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

const telegramSecretHeader = "X-Telegram-Bot-Api-Secret-Token"

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) < 8 || !strings.EqualFold(h[:7], "bearer ") {
		return ""
	}
	return strings.TrimSpace(h[7:])
}

func secretEqual(got, want string) bool {
	if want == "" {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1 {
		return true
	}
	return false
}

// requireBearer authenticates operator control-plane routes. It fails
// closed: an empty (unconfigured) token is never treated as "auth off" —
// secretEqual rejects every request once want is "", so a missing
// AGENT_API_TOKEN (e.g. an absent Vault key) makes these routes reject
// everything instead of silently going public.
func requireBearer(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !secretEqual(bearerToken(r), token) {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// requireBearerFunc is the http.HandlerFunc equivalent of requireBearer; it
// fails closed the same way when token is empty.
func requireBearerFunc(token string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !secretEqual(bearerToken(r), token) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

// telegramSecretOK fails closed: secretEqual rejects every comparison once
// secret is "", so an unconfigured TELEGRAM_WEBHOOK_SECRET rejects every
// inbound request instead of accepting all of them.
func telegramSecretOK(r *http.Request, secret string) bool {
	return secretEqual(r.Header.Get(telegramSecretHeader), secret)
}
