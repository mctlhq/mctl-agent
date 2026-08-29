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
	"time"
)

// WriteTimeout bounds the database work a single webhook may do. It is
// generous relative to the queries involved (single-row writes against an
// indexed table) — the point is not to be tight, it is to be finite.
const WriteTimeout = 30 * time.Second

// DetachedWrite derives the context that an inbound request's WRITE work runs
// under. It deliberately does NOT inherit the request's cancellation.
//
// Reads should keep using r.Context(): a query whose caller has gone away is
// work nobody wants finished, and cancelling it frees a connection. Writes are
// the opposite, and that asymmetry is the whole reason this exists.
//
// The obvious thing — passing r.Context() straight down — is wrong for a
// write. AlertManager gives a webhook a short deadline and hangs up when it
// expires; Go then cancels the request context, which would abort a ticket
// write partway through. The alert keeps firing while the incident it
// describes exists nowhere, and the resolve that eventually arrives finds no
// ticket to close. Losing the write is strictly worse than doing it after the
// caller stopped listening, and the 5xx retry path exists precisely so a
// caller that gave up still gets a consistent result later.
//
// What the request context is still good for is its values (trace/request
// IDs), which WithoutCancel keeps. The explicit timeout supplies the bound
// that cancellation used to provide, so a wedged database piles up neither
// goroutines nor pooled connections.
func DetachedWrite(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), WriteTimeout)
}
