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

package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/mctlhq/mctl-agent/internal/metrics"
)

// AlertManagerClient fetches active alert fingerprints from AlertManager's
// v2 API. HTTP is injectable for tests.
type AlertManagerClient struct {
	BaseURL string
	Timeout time.Duration
	HTTP    *http.Client
}

type amAlert struct {
	Fingerprint string `json:"fingerprint"`
	Status      struct {
		State string `json:"state"`
	} `json:"status"`
}

// ActiveFingerprints returns the set of fingerprints for currently active,
// non-silenced alerts. Returns (nil, err) on any error — callers MUST treat
// that as "do not act on absence".
func (c *AlertManagerClient) ActiveFingerprints(ctx context.Context) (map[string]struct{}, error) {
	reqCtx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()

	start := time.Now()
	outcome := "success"
	defer func() {
		metrics.AMRequestDuration.WithLabelValues(outcome).Observe(time.Since(start).Seconds())
	}()

	url := c.BaseURL + "/api/v2/alerts?active=true&silenced=false"
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("alertmanager: build request: %w", err)
	}
	req.Header.Set("User-Agent", "mctl-agent")

	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		outcome = "transport_error"
		return nil, fmt.Errorf("alertmanager: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode/100 != 2 {
		outcome = "http_error"
		return nil, fmt.Errorf("alertmanager: HTTP %d", resp.StatusCode)
	}

	var alerts []amAlert
	if err := json.NewDecoder(resp.Body).Decode(&alerts); err != nil {
		outcome = "decode_error"
		return nil, fmt.Errorf("alertmanager: decode: %w", err)
	}

	out := make(map[string]struct{}, len(alerts))
	for _, a := range alerts {
		if a.Fingerprint != "" && a.Status.State == "active" {
			out[a.Fingerprint] = struct{}{}
		}
	}
	return out, nil
}
