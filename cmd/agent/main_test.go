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

package main

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func TestRedactDSNHidesPassword(t *testing.T) {
	const dsn = "postgresql://mctl-agent:s3cr3t@shared-pg-rw.platform-db.svc:5432/mctl-agent"

	got := redactDSN(dsn)

	if strings.Contains(got, "s3cr3t") {
		t.Fatalf("password leaked in redacted DSN: %q", got)
	}
	for _, want := range []string{"mctl-agent", "shared-pg-rw.platform-db.svc:5432"} {
		if !strings.Contains(got, want) {
			t.Errorf("redacted DSN lost %q: %q", want, got)
		}
	}
}

func TestRedactDSNLeavesSQLitePathAlone(t *testing.T) {
	const path = "/data/mctl-agent.db"

	if got := redactDSN(path); got != path {
		t.Errorf("redactDSN(%q) = %q, want unchanged", path, got)
	}
}

func TestIsTransientDialError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"connection refused", errors.New(`migrating: dial tcp 10.43.131.86:5432: connect: connection refused`), true},
		{"dns not ready", errors.New(`dial tcp: lookup shared-pg-rw: no such host`), true},
		{"net.OpError", &net.OpError{Op: "dial", Err: errors.New("boom")}, true},
		{"bad migration", errors.New(`migrating: syntax error at or near "CREAT"`), false},
		{"bad dsn", errors.New(`opening postgres: missing "=" after "postgres" in connection info string`), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTransientDialError(tt.err); got != tt.want {
				t.Errorf("isTransientDialError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// A permanent error must not be retried: the pod should exit immediately
// instead of sitting out the whole init budget on an error that cannot heal.
func TestInitTicketStoreFailsFastOnPermanentError(t *testing.T) {
	start := time.Now()

	_, err := initTicketStore(context.Background(), "postgres://user@127.0.0.1:5432/db?sslmode=bogus-mode")

	if err == nil {
		t.Fatal("expected an error for an invalid DSN")
	}
	if elapsed := time.Since(start); elapsed > storeInitBudget {
		t.Errorf("permanent error was retried: took %s, budget is %s", elapsed, storeInitBudget)
	}
}

// A cancelled context (SIGTERM during startup) must stop the retry loop
// rather than hold the process for the remainder of the budget.
func TestInitTicketStoreStopsOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()

	// Port 1 on loopback refuses connections, so this is the transient path
	// the retry loop is meant to cover.
	_, err := initTicketStore(ctx, "postgres://user:pass@127.0.0.1:1/db?sslmode=disable")

	if err == nil {
		t.Fatal("expected an error when the context is already cancelled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error does not wrap context.Canceled: %v", err)
	}
	if elapsed := time.Since(start); elapsed > storeInitBudget {
		t.Errorf("cancellation ignored: took %s, budget is %s", elapsed, storeInitBudget)
	}
}
