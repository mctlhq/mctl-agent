package config

import (
	"os"
	"strconv"
	"time"
)

type Config struct {
	Port             string
	MctlAPIURL       string
	MctlAPIToken     string
	AnthropicAPIKey  string
	GitHubToken      string
	GitHubOwner      string
	GitHubRepo       string
	TelegramBotToken string
	TelegramChatID   string
	PollInterval     time.Duration
	DryRun           bool
	DBPath           string
	MaxPRPerHour     int
	MaxPRPerDay      int
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

	return Config{
		Port:             envOr("PORT", "8081"),
		MctlAPIURL:       envOr("MCTL_API_URL", "http://mctl-api.mctl-api.svc:8080"),
		MctlAPIToken:     os.Getenv("MCTL_API_TOKEN"),
		AnthropicAPIKey:  os.Getenv("ANTHROPIC_API_KEY"),
		GitHubToken:      os.Getenv("GITHUB_TOKEN"),
		GitHubOwner:      envOr("GITHUB_OWNER", "mctlhq"),
		GitHubRepo:       envOr("GITHUB_REPO", "mctl-core"),
		TelegramBotToken: os.Getenv("TELEGRAM_BOT_TOKEN"),
		TelegramChatID:   os.Getenv("TELEGRAM_CHAT_ID"),
		PollInterval:     pollInterval,
		DryRun:           dryRun,
		DBPath:           envOr("DB_PATH", "/data/mctl-agent.db"),
		MaxPRPerHour:     maxPRPerHour,
		MaxPRPerDay:      maxPRPerDay,
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
