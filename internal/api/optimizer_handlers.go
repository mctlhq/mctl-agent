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

package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mctlhq/mctl-agent/internal/optimizer"
)

func optimizerCandidatesHandler(opt *optimizer.Optimizer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		items := opt.Candidates()
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"items": items,
			"count": len(items),
		})
	}
}

func optimizerRecommendationsHandler(opt *optimizer.Optimizer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status := strings.TrimSpace(r.URL.Query().Get("status"))
		limit := 50
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				limit = n
			}
		}
		items, err := opt.Store().ListRecommendations(status, limit)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"items": items,
			"count": len(items),
		})
	}
}

func optimizerRunsHandler(opt *optimizer.Optimizer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		items, err := opt.Store().RunsByStatus(
			optimizer.RunStatusWaitingMerge, optimizer.RunStatusWarmup,
			optimizer.RunStatusEvaluating, optimizer.RunStatusDone)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"items": items,
			"count": len(items),
		})
	}
}

func optimizerScanHandler(opt *optimizer.Optimizer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		go opt.RecommendPass(context.Background(), time.Now().UTC())
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "scan started"})
	}
}
