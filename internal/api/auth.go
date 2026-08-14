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

// requireBearer authenticates operator control-plane routes. An empty token
// disables the check so local tests and DRY_RUN development keep working;
// production GitOps must set AGENT_API_TOKEN.
func requireBearer(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if token == "" {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !secretEqual(bearerToken(r), token) {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func requireBearerFunc(token string, next http.HandlerFunc) http.HandlerFunc {
	if token == "" {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if !secretEqual(bearerToken(r), token) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

func telegramSecretOK(r *http.Request, secret string) bool {
	if secret == "" {
		return true
	}
	return secretEqual(r.Header.Get(telegramSecretHeader), secret)
}
