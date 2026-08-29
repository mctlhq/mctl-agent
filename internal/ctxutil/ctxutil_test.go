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

package ctxutil

import (
	"context"
	"testing"
	"time"
)

func TestDetachedWriteSurvivesParentCancellation(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	ctx, done := DetachedWrite(parent)
	defer done()
	cancel()

	if err := ctx.Err(); err != nil {
		t.Fatalf("a detached write must outlive its caller: %v", err)
	}
	if _, ok := ctx.Deadline(); !ok {
		t.Error("dropping cancellation without setting a deadline would leave the work unbounded")
	}
}

func TestDetachedBatchCapsTotalWork(t *testing.T) {
	// n * WriteTimeout is the whole point of the cap: with request
	// cancellation stripped, a large batch would otherwise be work that
	// nothing can stop, and AlertManager's retries stack on top of it.
	cases := []struct {
		name string
		n    int
		want time.Duration
	}{
		{"small batch scales with size", 2, 2 * WriteTimeout},
		{"large batch is capped", 1000, MaxBatchWrite},
		{"empty batch still bounded", 0, MaxBatchWrite},
		{"negative count cannot disable the bound", -1, MaxBatchWrite},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := DetachedBatch(context.Background(), tc.n)
			defer cancel()
			d, ok := ctx.Deadline()
			if !ok {
				t.Fatal("no deadline set")
			}
			got := time.Until(d)
			// Generous slack: the assertion is about which bound was chosen,
			// not about clock precision.
			if got > tc.want+time.Second || got < tc.want-time.Second {
				t.Errorf("budget for n=%d: want ~%s, got ~%s", tc.n, tc.want, got.Round(time.Second))
			}
		})
	}
}
