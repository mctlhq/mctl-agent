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

package notify

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testBotToken = "123456:AAH-secret-bot-token"

// Every Bot API entry point, so a new one that bypasses post() shows up here.
var botCalls = map[string]func(tg *Telegram, ctx context.Context) error{
	"SetWebhook": func(tg *Telegram, ctx context.Context) error {
		return tg.SetWebhook(ctx, "https://agent.example", "secret")
	},
	"SendText": func(tg *Telegram, _ context.Context) error { return tg.SendText("hi") },
	"AnswerCallbackQuery": func(tg *Telegram, ctx context.Context) error {
		return tg.AnswerCallbackQuery(ctx, "cb", "ok")
	},
}

func assertNoToken(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	for e := err; e != nil; e = errors.Unwrap(e) {
		if strings.Contains(e.Error(), testBotToken) {
			t.Fatalf("token leaked in error chain: %q", e.Error())
		}
	}
}

// A closed server makes net/http return a *url.Error whose text is the full
// request URL: the startup setWebhook timeout that logged the token.
func TestBotAPITransportErrorsHideToken(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close()
	for name, call := range botCalls {
		t.Run(name, func(t *testing.T) {
			tg := NewTelegram(testBotToken, "1", nil).WithAPIBase(base)
			err := call(tg, context.Background())
			assertNoToken(t, err)
			if !strings.Contains(err.Error(), "connection refused") && !strings.Contains(err.Error(), "connect") {
				t.Errorf("cause dropped from error: %q", err)
			}
		})
	}
}

// A bad API base fails inside http.NewRequest with a parse error that quotes
// the whole URL.
func TestBotAPIRequestBuildErrorsHideToken(t *testing.T) {
	for name, call := range botCalls {
		t.Run(name, func(t *testing.T) {
			tg := NewTelegram(testBotToken, "1", nil).WithAPIBase("http://bad host\x7f")
			assertNoToken(t, call(tg, context.Background()))
		})
	}
}

// A proxy or a misbehaving upstream can echo the request path in its body.
func TestBotAPIStatusErrorsHideToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream rejected "+r.URL.Path, http.StatusBadGateway)
	}))
	defer srv.Close()
	for name, call := range botCalls {
		t.Run(name, func(t *testing.T) {
			tg := NewTelegram(testBotToken, "1", nil).WithAPIBase(srv.URL)
			assertNoToken(t, call(tg, context.Background()))
		})
	}
}

func TestBotAPIErrorsKeepContextCause(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tg := NewTelegram(testBotToken, "1", nil).WithAPIBase(srv.URL)
	err := tg.SetWebhook(ctx, "https://agent.example", "secret")
	assertNoToken(t, err)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false for %q", err)
	}
}
