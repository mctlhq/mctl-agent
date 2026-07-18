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

// Package optimizer implements GitOps resource right-sizing: it observes
// per-workload usage in VictoriaMetrics, persists hourly rollups, proposes
// deterministic resource-request changes as PRs to the gitops repo, and
// evaluates the outcome after merge (SUCCESS/REGRESSION verdict, rollback
// PR on regression). The optimizer never touches the cluster directly —
// Git stays the source of truth.
//
// MVP scope: plain Deployment-based services rendered by base-service,
// main container only, resources.requests only. Explicitly deferred:
// limits/replicas/HPA, blueGreen (Rollout) services, sidecars and init
// containers, CronJobs/Argo Workflows, StatefulSets/storage, seasonality
// beyond the 14d window, latency/error-rate evaluation, auto-merge,
// node-consolidation savings estimation.
package optimizer

import (
	"github.com/mctlhq/mctl-agent/internal/config"
	"github.com/mctlhq/mctl-agent/internal/ticket"
	"time"
)

// Optimizer owns the three optimizer loops (collector, recommender,
// evaluator). Construction wires shared dependencies; loops are started by
// Run (added with the recommender/evaluator).
type Optimizer struct {
	store       *Store
	ticketStore *ticket.Store
	vm          MetricsQuerier
	cfg         config.OptimizerConfig

	// collectedThrough tracks, per workload, the last hour a collection
	// pass fully processed (including empty hours), so scaled-to-zero
	// services don't re-query the whole backfill window every tick.
	// In-memory only: a restart triggers at most one full backfill pass.
	collectedThrough map[string]time.Time
}

// New creates an Optimizer. Additional dependencies (gitops writer,
// notifier) are wired in as the corresponding loops land.
func New(store *Store, ticketStore *ticket.Store, vm MetricsQuerier, cfg config.OptimizerConfig) *Optimizer {
	return &Optimizer{
		store:            store,
		ticketStore:      ticketStore,
		vm:               vm,
		cfg:              cfg,
		collectedThrough: make(map[string]time.Time),
	}
}
