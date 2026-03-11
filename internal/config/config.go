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
		GitHubRepo:       envOr("GITHUB_REPO", "mctl-gitops"),
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
