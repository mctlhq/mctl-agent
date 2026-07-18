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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newTestClient(handler http.HandlerFunc) (*Client, *httptest.Server) {
	srv := httptest.NewServer(handler)
	return &Client{BaseURL: srv.URL, Timeout: 2 * time.Second, HTTP: srv.Client()}, srv
}

func TestQueryVector(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("query"); got != "up" {
			t.Errorf("query param = %q, want %q", got, "up")
		}
		if got := r.URL.Query().Get("time"); got != "1700000000" {
			t.Errorf("time param = %q, want 1700000000", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
			{"metric":{"pod":"a"},"value":[1700000000,"0.5"]},
			{"metric":{"pod":"b"},"value":[1700000000,"1.5"]}]}}`))
	})
	defer srv.Close()

	samples, err := c.Query(context.Background(), "up", time.Unix(1700000000, 0))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("len(samples) = %d, want 2", len(samples))
	}
	if samples[0].Labels["pod"] != "a" || samples[0].Value != 0.5 {
		t.Errorf("sample[0] = %+v", samples[0])
	}
	if samples[1].Labels["pod"] != "b" || samples[1].Value != 1.5 {
		t.Errorf("sample[1] = %+v", samples[1])
	}
}

func TestQueryScalar(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		status  int
		wantVal float64
		wantOK  bool
		wantErr bool
	}{
		{
			name:    "single value",
			body:    `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1700000000,"42"]}]}}`,
			status:  200,
			wantVal: 42,
			wantOK:  true,
		},
		{
			name:   "empty vector",
			body:   `{"status":"success","data":{"resultType":"vector","result":[]}}`,
			status: 200,
			wantOK: false,
		},
		{
			name:   "NaN ratio",
			body:   `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1700000000,"NaN"]}]}}`,
			status: 200,
			wantOK: false,
		},
		{
			name:    "http error",
			body:    `oops`,
			status:  500,
			wantErr: true,
		},
		{
			name:    "query error status",
			body:    `{"status":"error","error":"bad expr"}`,
			status:  200,
			wantErr: true,
		},
		{
			name:    "bad json",
			body:    `{{{`,
			status:  200,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			})
			defer srv.Close()

			val, ok, err := c.QueryScalar(context.Background(), "x", time.Time{})
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("QueryScalar: %v", err)
			}
			if ok != tt.wantOK || val != tt.wantVal {
				t.Errorf("QueryScalar = (%v, %v), want (%v, %v)", val, ok, tt.wantVal, tt.wantOK)
			}
		})
	}
}
