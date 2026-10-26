package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-github/v68/github"

	"github.com/mctlhq/mctl-agent/internal/notify"
	"github.com/mctlhq/mctl-agent/internal/scoutspike"
)

type scoutGH struct {
	mu      sync.Mutex
	calls   int
	creates int
	search  func() (*github.IssuesSearchResult, error)
}

func (g *scoutGH) RepoAccessible(context.Context, string) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	return http.StatusOK, nil
}
func (g *scoutGH) SearchIssues(context.Context, string, int) (*github.IssuesSearchResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	if g.search != nil {
		return g.search()
	}
	return &github.IssuesSearchResult{Total: github.Ptr(0)}, nil
}
func (g *scoutGH) CreateIssue(_ context.Context, repo, _, _ string, _ []string) (int, string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	g.creates++
	return 9, "https://github.com/mctlhq/" + repo + "/issues/9", nil
}

type tgStub struct {
	mu      sync.Mutex
	methods []string
	bodies  []map[string]any
}

func (s *tgStub) handler(w http.ResponseWriter, r *http.Request) {
	var m map[string]any
	_ = json.NewDecoder(r.Body).Decode(&m)
	s.mu.Lock()
	s.methods = append(s.methods, r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
	s.bodies = append(s.bodies, m)
	s.mu.Unlock()
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func scoutSetup(t *testing.T, owner int64, secret string, gh *scoutGH) (http.Handler, *tgStub) {
	t.Helper()
	stub := &tgStub{}
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	t.Cleanup(srv.Close)
	tg := notify.NewTelegram("bot-token", "555", nil).WithAPIBase(srv.URL).WithOwnerUserID(owner)
	return NewRouter(Options{
		Telegram:              tg,
		TelegramWebhookSecret: secret,
		ScoutSpikes:           scoutspike.NewService(gh, []string{"mctl-api"}),
	}), stub
}

func callbackBody(t *testing.T, chat, user int64, data, draftURL string) []byte {
	t.Helper()
	kb := [][]map[string]any{
		{{"text": "Other", "url": "https://example.com"}},
		{{"text": "Draft spike 1", "url": draftURL}, {"text": "Create spike 1", "callback_data": data}},
	}
	b, err := json.Marshal(map[string]any{"update_id": 1, "callback_query": map[string]any{
		"id": "cq1", "from": map[string]any{"id": user}, "data": data,
		"message": map[string]any{"message_id": 42, "date": 1759300000,
			"chat": map[string]any{"id": chat, "type": "private"}, "reply_markup": map[string]any{"inline_keyboard": kb}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func goodDraft() string {
	q := url.Values{"title": {"spike: x"}, "body": {"Source: https://a.example/p"}}
	return "https://github.com/mctlhq/mctl-api/issues/new?" + q.Encode()
}

func post(h http.Handler, secret string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/telegram", bytes.NewReader(body))
	req.Header.Set("X-Telegram-Bot-Api-Secret-Token", secret)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestScoutCallbackHappyPathThenSecondTap(t *testing.T) {
	gh := &scoutGH{}
	h, stub := scoutSetup(t, 77, "s", gh)
	body := callbackBody(t, 555, 77, "scout:create:1", goodDraft())
	if w := post(h, "s", body); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if gh.creates != 1 {
		t.Fatalf("creates=%d", gh.creates)
	}
	if got := strings.Join(stub.methods, ","); got != "answerCallbackQuery,editMessageReplyMarkup" {
		t.Fatalf("methods %s", got)
	}
	if stub.bodies[0]["text"] != "Created mctl-api#9" {
		t.Fatalf("answer %v", stub.bodies[0]["text"])
	}
	rows := stub.bodies[1]["reply_markup"].(map[string]any)["inline_keyboard"].([]any)
	if len(rows) != 2 {
		t.Fatalf("rows %d", len(rows))
	}
	btn := rows[1].([]any)
	if len(btn) != 1 || btn[0].(map[string]any)["text"] != "#9 ->" || btn[0].(map[string]any)["url"] != "https://github.com/mctlhq/mctl-api/issues/9" {
		t.Fatalf("row %v", btn)
	}
	if rows[0].([]any)[0].(map[string]any)["text"] != "Other" {
		t.Fatal("other rows changed")
	}

	post(h, "s", body)
	if gh.creates != 1 {
		t.Fatalf("second tap created again: %d", gh.creates)
	}
	if stub.bodies[2]["text"] != "Already exists: mctl-api#9" {
		t.Fatalf("answer %v", stub.bodies[2]["text"])
	}
}

func TestScoutCallbackAuthRefusals(t *testing.T) {
	// Pins each auth guard: no GitHub call, and no Telegram call to strangers.
	cases := map[string]struct {
		owner         int64
		secret        string
		chat, user    int64
		wantTelegram0 bool
	}{
		"other chat":   {77, "s", 999, 77, true},
		"other user":   {77, "s", 555, 78, true},
		"owner unset":  {0, "s", 555, 77, true},
		"secret unset": {77, "", 555, 77, true},
	}
	for name, c := range cases {
		gh := &scoutGH{}
		h, stub := scoutSetup(t, c.owner, c.secret, gh)
		w := post(h, c.secret, callbackBody(t, c.chat, c.user, "scout:create:1", goodDraft()))
		if (w.Code != 200 && w.Code != 401) || gh.calls != 0 || len(stub.methods) != 0 {
			t.Errorf("%s: code=%d gh=%d tg=%v", name, w.Code, gh.calls, stub.methods)
		}
	}
}

func TestScoutCallbackTargetNotAllowed(t *testing.T) {
	gh := &scoutGH{}
	h, stub := scoutSetup(t, 77, "s", gh)
	post(h, "s", callbackBody(t, 555, 77, "scout:create:1", "https://github.com/evil/x/issues/new?title=spike:+x&body=Source:+y"))
	if gh.calls != 0 || len(stub.methods) != 1 || stub.bodies[0]["text"] != "Failed: draft target not allowed" {
		t.Fatalf("gh=%d tg=%v %v", gh.calls, stub.methods, stub.bodies)
	}
}

func TestScoutCallbackSearchFailureKeepsKeyboard(t *testing.T) {
	gh := &scoutGH{search: func() (*github.IssuesSearchResult, error) {
		return &github.IssuesSearchResult{IncompleteResults: github.Ptr(true)}, nil
	}}
	h, stub := scoutSetup(t, 77, "s", gh)
	post(h, "s", callbackBody(t, 555, 77, "scout:create:1", goodDraft()))
	if gh.creates != 0 || strings.Join(stub.methods, ",") != "answerCallbackQuery" || stub.bodies[0]["text"] != "Failed: could not check for duplicates" {
		t.Fatalf("creates=%d tg=%v %v", gh.creates, stub.methods, stub.bodies)
	}
}

func TestScoutCallbackNeverTouchesWebhookMethods(t *testing.T) {
	gh := &scoutGH{}
	h, stub := scoutSetup(t, 77, "s", gh)
	post(h, "s", callbackBody(t, 555, 77, "scout:create:1", goodDraft()))
	post(h, "s", callbackBody(t, 555, 77, "garbage", goodDraft()))
	for _, m := range stub.methods {
		if m != "answerCallbackQuery" && m != "editMessageReplyMarkup" {
			t.Errorf("unexpected method %s", m)
		}
	}
	if w := post(h, "wrong", callbackBody(t, 555, 77, "scout:create:1", goodDraft())); w.Code != 401 {
		t.Errorf("bad secret code %d", w.Code)
	}
}
