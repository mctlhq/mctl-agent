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

package vmetrics

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client queries a VictoriaMetrics (PromQL-compatible) endpoint via
// /api/v1/query. HTTP is injectable for tests.
type Client struct {
	BaseURL string
	Timeout time.Duration
	HTTP    *http.Client
}

// Sample is one element of an instant-query vector result.
type Sample struct {
	Labels map[string]string
	Value  float64
	Time   time.Time
}

type queryResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  []any             `json:"value"`
		} `json:"result"`
	} `json:"data"`
	Error string `json:"error"`
}

// Query runs an instant PromQL query at ts. Returns (nil, err) on any
// transport/HTTP/decode error — callers MUST treat that as "no data", never
// as a zero measurement. An empty vector is a valid (empty, nil) result.
func (c *Client) Query(ctx context.Context, promql string, ts time.Time) ([]Sample, error) {
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	params := url.Values{}
	params.Set("query", promql)
	if !ts.IsZero() {
		params.Set("time", strconv.FormatInt(ts.Unix(), 10))
	}
	u := strings.TrimRight(c.BaseURL, "/") + "/api/v1/query?" + params.Encode()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("vmetrics: build request: %w", err)
	}
	req.Header.Set("User-Agent", "mctl-agent")

	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vmetrics: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("vmetrics: HTTP %d", resp.StatusCode)
	}

	var qr queryResponse
	if err := json.NewDecoder(resp.Body).Decode(&qr); err != nil {
		return nil, fmt.Errorf("vmetrics: decode: %w", err)
	}
	if qr.Status != "success" {
		return nil, fmt.Errorf("vmetrics: query status %q: %s", qr.Status, qr.Error)
	}
	if qr.Data.ResultType != "vector" {
		return nil, fmt.Errorf("vmetrics: unexpected result type %q", qr.Data.ResultType)
	}

	out := make([]Sample, 0, len(qr.Data.Result))
	for _, r := range qr.Data.Result {
		if len(r.Value) != 2 {
			continue
		}
		tsF, ok := r.Value[0].(float64)
		if !ok {
			continue
		}
		valS, ok := r.Value[1].(string)
		if !ok {
			continue
		}
		v, err := strconv.ParseFloat(valS, 64)
		if err != nil {
			continue
		}
		out = append(out, Sample{
			Labels: r.Metric,
			Value:  v,
			Time:   time.Unix(int64(tsF), 0).UTC(),
		})
	}
	return out, nil
}

// QueryScalar runs an instant query expected to yield at most one sample.
// Returns (0, false, nil) for an empty result or a NaN/Inf value (common for
// 0/0 ratio expressions), and (0, false, err) on query failure.
func (c *Client) QueryScalar(ctx context.Context, promql string, ts time.Time) (float64, bool, error) {
	samples, err := c.Query(ctx, promql, ts)
	if err != nil {
		return 0, false, err
	}
	if len(samples) == 0 {
		return 0, false, nil
	}
	v := samples[0].Value
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false, nil
	}
	return v, true, nil
}
