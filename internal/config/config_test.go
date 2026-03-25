package config

import (
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	// Clear env vars that might be set.
	envVars := []string{"PORT", "MCTL_API_URL", "GITHUB_OWNER", "GITHUB_REPO",
		"POLL_INTERVAL", "DRY_RUN", "DB_PATH", "MAX_PR_PER_HOUR", "MAX_PR_PER_DAY"}
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

func TestLoadInvalidPollInterval(t *testing.T) {
	t.Setenv("POLL_INTERVAL", "not-a-duration")
	cfg := Load()
	// Should fall back to default.
	if cfg.PollInterval != 5*time.Minute {
		t.Errorf("PollInterval = %v, want 5m (default)", cfg.PollInterval)
	}
}

func TestLoadUsesGitHubTokenForLegacyMctlAPIToken(t *testing.T) {
	t.Setenv("MCTL_API_TOKEN", "mctl-prod-token-2026-v1")
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
