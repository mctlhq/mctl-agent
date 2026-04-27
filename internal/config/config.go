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

package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Port                    string
	MctlAPIURL              string
	MctlAPIToken            string
	AnthropicAPIKey         string
	GitHubToken             string
	GitHubOwner             string
	GitHubRepo              string
	GitHubWebhookSecret     string
	TelegramBotToken        string
	TelegramChatID          string            // global default fallback
	TelegramTenantChatIDs   map[string]string // per-tenant routing (admins/labs/ovk → chat_id)
	OpenClawBotUsername     string
	PollInterval            time.Duration
	DryRun                  bool
	DatabaseURL             string
	MaxPRPerHour            int
	MaxPRPerDay             int
	AutoMergeEnabled        bool
	EscalationTag           string
	WebhookEnabled          bool
	WebhookCallbackURL      string
	WebhookDefaultTTL       time.Duration
	AlertFlapCooldown       time.Duration
	AutoResolveStaleAfter   time.Duration
	AlertIgnoreServiceRegex string
}

func Load() Config {
	pollInterval := 5 * time.Minute
	if v := os.Getenv("POLL_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			pollInterval = d
		}
	}

	dryRun := true
	if v := os.Getenv("DRY_RUN"); v == "false" {
		dryRun = false
	}

	maxPRPerHour := 5
	if v := os.Getenv("MAX_PR_PER_HOUR"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			maxPRPerHour = n
		}
	}

	maxPRPerDay := 20
	if v := os.Getenv("MAX_PR_PER_DAY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			maxPRPerDay = n
		}
	}

	autoMergeEnabled := true
	if v := os.Getenv("AUTO_MERGE_ENABLED"); v == "false" {
		autoMergeEnabled = false
	}

	webhookEnabled := false
	if v := os.Getenv("WEBHOOK_ENABLED"); v == "true" {
		webhookEnabled = true
	}

	webhookDefaultTTL := 15 * time.Minute
	if v := os.Getenv("WEBHOOK_DEFAULT_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			webhookDefaultTTL = d
		}
	}

	alertFlapCooldown := 10 * time.Minute
	if v := os.Getenv("ALERT_FLAP_COOLDOWN"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			alertFlapCooldown = d
		}
	}

	autoResolveStaleAfter := 24 * time.Hour
	if v := os.Getenv("AUTO_RESOLVE_STALE_AFTER"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			autoResolveStaleAfter = d
		}
	}

	// Default pattern; empty env explicitly disables the filter, so we
	// cannot use envOr (which treats "" as "fall back to default").
	alertIgnoreServiceRegex := `^(openclawpr\d+|.*-demo\d*|hooktest-.*|svcprobe-.*|external-agent-demo.*|auto-remediation-demo)$`
	if v, ok := os.LookupEnv("ALERT_IGNORE_SERVICE_REGEX"); ok {
		alertIgnoreServiceRegex = v
	}

	// Priority: DATABASE_URL > DB_PATH > default
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = envOr("DB_PATH", "/data/mctl-agent.db")
	}

	githubToken := strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
	mctlAPIToken := strings.TrimSpace(os.Getenv("MCTL_API_TOKEN"))
	// mctl-api currently authenticates bearer tokens through the GitHub/Dex path.
	// Older service tokens like "mctl-prod-token-..." no longer validate there.
	// Prefer a GitHub token when the dedicated API token is absent or clearly legacy.
	if githubToken != "" && (mctlAPIToken == "" || strings.HasPrefix(mctlAPIToken, "mctl-")) {
		mctlAPIToken = githubToken
	}

	return Config{
		Port:                    envOr("PORT", "8081"),
		MctlAPIURL:              envOr("MCTL_API_URL", "http://mctl-api.mctl-api.svc:8080"),
		MctlAPIToken:            mctlAPIToken,
		AnthropicAPIKey:         os.Getenv("ANTHROPIC_API_KEY"),
		GitHubToken:             githubToken,
		GitHubOwner:             envOr("GITHUB_OWNER", "mctlhq"),
		GitHubRepo:              envOr("GITHUB_REPO", "mctl-gitops"),
		GitHubWebhookSecret:     os.Getenv("GITHUB_WEBHOOK_SECRET"),
		TelegramBotToken:        os.Getenv("TELEGRAM_BOT_TOKEN"),
		TelegramChatID:          os.Getenv("TELEGRAM_CHAT_ID"),
		TelegramTenantChatIDs:   parseTenantChatIDs(),
		OpenClawBotUsername:     envOr("OPENCLAW_BOT_USERNAME", "@mctl_me_bot"),
		PollInterval:            pollInterval,
		DryRun:                  dryRun,
		DatabaseURL:             dbURL,
		MaxPRPerHour:            maxPRPerHour,
		MaxPRPerDay:             maxPRPerDay,
		AutoMergeEnabled:        autoMergeEnabled,
		EscalationTag:           envOr("ESCALATION_TAG", "@mashkovd"),
		WebhookEnabled:          webhookEnabled,
		WebhookCallbackURL:      envOr("WEBHOOK_CALLBACK_URL", "http://localhost:8081"),
		WebhookDefaultTTL:       webhookDefaultTTL,
		AlertFlapCooldown:       alertFlapCooldown,
		AutoResolveStaleAfter:   autoResolveStaleAfter,
		AlertIgnoreServiceRegex: alertIgnoreServiceRegex,
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// parseTenantChatIDs reads per-tenant Telegram chat overrides from env.
// Two formats are accepted (env var checked in order):
//
//  1. TELEGRAM_TENANT_CHAT_IDS="admins=123,labs=456,ovk=789"
//     comma-separated key=value pairs. Whitespace around = and , is ignored.
//
//  2. TELEGRAM_CHAT_ID_<TENANT>=<id> per-tenant variables (e.g.
//     TELEGRAM_CHAT_ID_ADMINS, TELEGRAM_CHAT_ID_LABS) — uppercase tenant.
//     Used as a fallback for any tenant absent from the comma-list.
//
// Returns nil when no overrides are set; callers must fall back to the
// global Config.TelegramChatID in that case.
func parseTenantChatIDs() map[string]string {
	out := map[string]string{}
	if csv := strings.TrimSpace(os.Getenv("TELEGRAM_TENANT_CHAT_IDS")); csv != "" {
		for _, pair := range strings.Split(csv, ",") {
			kv := strings.SplitN(pair, "=", 2)
			if len(kv) != 2 {
				continue
			}
			tenant := strings.TrimSpace(kv[0])
			chatID := strings.TrimSpace(kv[1])
			if tenant != "" && chatID != "" {
				out[tenant] = chatID
			}
		}
	}
	// Per-tenant env fallback (only fills tenants not already set above).
	for _, env := range os.Environ() {
		const prefix = "TELEGRAM_CHAT_ID_"
		if !strings.HasPrefix(env, prefix) {
			continue
		}
		eq := strings.Index(env, "=")
		if eq <= len(prefix) {
			continue
		}
		tenant := strings.ToLower(env[len(prefix):eq])
		chatID := env[eq+1:]
		if tenant == "" || chatID == "" {
			continue
		}
		if _, exists := out[tenant]; !exists {
			out[tenant] = chatID
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
