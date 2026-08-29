package config

import (
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	// Clear env vars that might be set.
	envVars := []string{"PORT", "MCTL_API_URL", "GITHUB_OWNER", "GITHUB_REPO",
		"POLL_INTERVAL", "DRY_RUN", "DB_PATH", "MAX_PR_PER_HOUR", "MAX_PR_PER_DAY",
		"AUTO_MERGE_ENABLED"}
	for _, k := range envVars {
		t.Setenv(k, "")
	}

	cfg := Load()

	if cfg.Port != "8081" {
		t.Errorf("Port = %q, want 8081", cfg.Port)
	}
	if cfg.MctlAPIURL != "http://mctl-api.mctl-api.svc:8080" {
		t.Errorf("MctlAPIURL = %q", cfg.MctlAPIURL)
	}
	if cfg.GitHubOwner != "mctlhq" {
		t.Errorf("GitHubOwner = %q, want mctlhq", cfg.GitHubOwner)
	}
	if cfg.GitHubRepo != "mctl-gitops" {
		t.Errorf("GitHubRepo = %q, want mctl-gitops", cfg.GitHubRepo)
	}
	if cfg.PollInterval != 5*time.Minute {
		t.Errorf("PollInterval = %v, want 5m", cfg.PollInterval)
	}
	if !cfg.DryRun {
		t.Error("DryRun should default to true")
	}
	if cfg.DatabaseURL != "/data/mctl-agent.db" {
		t.Errorf("DatabaseURL = %q", cfg.DatabaseURL)
	}
	if cfg.MaxPRPerHour != 5 {
		t.Errorf("MaxPRPerHour = %d, want 5", cfg.MaxPRPerHour)
	}
	if cfg.MaxPRPerDay != 20 {
		t.Errorf("MaxPRPerDay = %d, want 20", cfg.MaxPRPerDay)
	}
	if cfg.AutoMergeEnabled {
		t.Error("AutoMergeEnabled should default to false")
	}
}

func TestLoadAutoMergeEnabled(t *testing.T) {
	tests := []struct {
		envVal string
		want   bool
	}{
		{"true", true},
		{"false", false},
		{"1", false},
		{"", false},
		{"TRUE", false},
	}
	for _, tt := range tests {
		t.Run("AUTO_MERGE_ENABLED="+tt.envVal, func(t *testing.T) {
			t.Setenv("AUTO_MERGE_ENABLED", tt.envVal)
			cfg := Load()
			if cfg.AutoMergeEnabled != tt.want {
				t.Errorf("AutoMergeEnabled = %v, want %v", cfg.AutoMergeEnabled, tt.want)
			}
		})
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("PORT", "9090")
	t.Setenv("POLL_INTERVAL", "10m")
	t.Setenv("DRY_RUN", "false")
	t.Setenv("MAX_PR_PER_HOUR", "10")
	t.Setenv("MAX_PR_PER_DAY", "50")
	t.Setenv("DB_PATH", "/tmp/test.db")
	t.Setenv("GITHUB_TOKEN", "ghp_test123")

	cfg := Load()

	if cfg.Port != "9090" {
		t.Errorf("Port = %q, want 9090", cfg.Port)
	}
	if cfg.PollInterval != 10*time.Minute {
		t.Errorf("PollInterval = %v, want 10m", cfg.PollInterval)
	}
	if cfg.DryRun {
		t.Error("DryRun should be false")
	}
	if cfg.MaxPRPerHour != 10 {
		t.Errorf("MaxPRPerHour = %d, want 10", cfg.MaxPRPerHour)
	}
	if cfg.MaxPRPerDay != 50 {
		t.Errorf("MaxPRPerDay = %d, want 50", cfg.MaxPRPerDay)
	}
	if cfg.DatabaseURL != "/tmp/test.db" {
		t.Errorf("DatabaseURL = %q", cfg.DatabaseURL)
	}
	if cfg.GitHubToken != "ghp_test123" {
		t.Errorf("GitHubToken = %q", cfg.GitHubToken)
	}
	if cfg.WebhookDefaultTTL != 15*time.Minute {
		t.Errorf("WebhookDefaultTTL = %v, want 15m", cfg.WebhookDefaultTTL)
	}
}

func TestLoadInboundAuthEnv(t *testing.T) {
	t.Setenv("AGENT_API_TOKEN", " api-tok ")
	t.Setenv("TELEGRAM_WEBHOOK_SECRET", " tg-sec ")
	t.Setenv("ALERTMANAGER_WEBHOOK_TOKEN", " am-tok ")
	cfg := Load()
	if cfg.AgentAPIToken != "api-tok" {
		t.Errorf("AgentAPIToken = %q", cfg.AgentAPIToken)
	}
	if cfg.TelegramWebhookSecret != "tg-sec" {
		t.Errorf("TelegramWebhookSecret = %q", cfg.TelegramWebhookSecret)
	}
	if cfg.AlertWebhookToken != "am-tok" {
		t.Errorf("AlertWebhookToken = %q", cfg.AlertWebhookToken)
	}
}

func TestLoadInvalidPollInterval(t *testing.T) {
	t.Setenv("POLL_INTERVAL", "not-a-duration")
	cfg := Load()
	// Should fall back to default.
	if cfg.PollInterval != 5*time.Minute {
		t.Errorf("PollInterval = %v, want 5m (default)", cfg.PollInterval)
	}
}

// The "mctl-" prefixed service token is exactly what mctl-api's
// staticServiceUser matches against MCTL_AGENT_SERVICE_TOKEN, ahead of the
// GitHub and Dex paths. Substituting the GitHub token here is what left the
// poller answering 401 against a credential that works.
func TestLoadKeepsServiceStyleMctlAPIToken(t *testing.T) {
	t.Setenv("MCTL_API_TOKEN", "mctl-prod-token-2026-v1")
	t.Setenv("GITHUB_TOKEN", "gho_live123")

	cfg := Load()

	if cfg.MctlAPIToken != "mctl-prod-token-2026-v1" {
		t.Fatalf("MctlAPIToken = %q, want the configured service token kept as-is", cfg.MctlAPIToken)
	}
}

// The fallback itself stays: with no dedicated token there is nothing to lose
// by trying the GitHub one.
func TestLoadFallsBackToGitHubTokenWhenAPITokenUnset(t *testing.T) {
	t.Setenv("MCTL_API_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "gho_live123")

	cfg := Load()

	if cfg.MctlAPIToken != "gho_live123" {
		t.Fatalf("MctlAPIToken = %q, want GitHub token fallback", cfg.MctlAPIToken)
	}
}

func TestLoadPreservesExplicitGitHubStyleMctlAPIToken(t *testing.T) {
	t.Setenv("MCTL_API_TOKEN", "ghp_api_override")
	t.Setenv("GITHUB_TOKEN", "gho_live123")

	cfg := Load()

	if cfg.MctlAPIToken != "ghp_api_override" {
		t.Fatalf("MctlAPIToken = %q, want explicit override", cfg.MctlAPIToken)
	}
}

func TestLoadAlertIgnoreServiceRegexDefaultAndEmpty(t *testing.T) {
	// Default: env unset → a non-empty pattern is used.
	cfg := Load()
	if cfg.AlertIgnoreServiceRegex == "" {
		t.Error("expected non-empty default for AlertIgnoreServiceRegex")
	}

	// Empty explicit value disables the filter (main.go skips compile
	// when empty). envOr would have swallowed this and returned the
	// default.
	t.Setenv("ALERT_IGNORE_SERVICE_REGEX", "")
	cfg = Load()
	if cfg.AlertIgnoreServiceRegex != "" {
		t.Errorf("empty env should disable filter, got %q", cfg.AlertIgnoreServiceRegex)
	}

	// Custom pattern honored.
	t.Setenv("ALERT_IGNORE_SERVICE_REGEX", "^custom-.*$")
	cfg = Load()
	if cfg.AlertIgnoreServiceRegex != "^custom-.*$" {
		t.Errorf("custom pattern not honored: %q", cfg.AlertIgnoreServiceRegex)
	}
}
