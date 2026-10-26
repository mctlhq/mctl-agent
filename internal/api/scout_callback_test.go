package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-github/v68/github"

	"github.com/mctlhq/mctl-agent/internal/fixer"
	"github.com/mctlhq/mctl-agent/internal/gitopspath"
	"github.com/mctlhq/mctl-agent/internal/notify"
	"github.com/mctlhq/mctl-agent/internal/scoutspike"
)

type scoutGH struct {
	mu      sync.Mutex
	calls   int
	creates int
	search  func() (*github.IssuesSearchResult, error)
}

func (g *scoutGH) RepoAccessible(context.Context, string, string) (int, error) {
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
func (g *scoutGH) CreateIssue(_ context.Context, _, repo, _, _ string, _ []string) (int, string, error) {
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

func scoutSetup(t *testing.T, owner int64, secret string, gh scoutspike.GitHub) (http.Handler, *tgStub) {
	return scoutSetupChat(t, "555", owner, secret, gh)
}

func scoutSetupChat(t *testing.T, chatID string, owner int64, secret string, gh scoutspike.GitHub) (http.Handler, *tgStub) {
	t.Helper()
	stub := &tgStub{}
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	t.Cleanup(srv.Close)
	tg := notify.NewTelegram("bot-token", chatID, nil).WithAPIBase(srv.URL).WithOwnerUserID(owner)
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
	// Pins each auth guard: no GitHub call, no Telegram call to strangers,
	// and no chat or user id in the logs.
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	cases := map[string]struct {
		allowlist  string
		owner      int64
		secret     string
		chat, user int64
	}{
		"other chat":      {"555123", 77777, "s", 999888, 77777},
		"other user":      {"555123", 77777, "s", 555123, 78787},
		"owner unset":     {"555123", 0, "s", 555123, 77777},
		"secret unset":    {"555123", 77777, "", 555123, 77777},
		"empty allowlist": {"", 77777, "s", 555123, 77777},
	}
	for name, c := range cases {
		buf.Reset()
		gh := &scoutGH{}
		h, stub := scoutSetupChat(t, c.allowlist, c.owner, c.secret, gh)
		w := post(h, c.secret, callbackBody(t, c.chat, c.user, "scout:create:1", goodDraft()))
		if (w.Code != 200 && w.Code != 401) || gh.calls != 0 || gh.creates != 0 || len(stub.methods) != 0 {
			t.Errorf("%s: code=%d gh=%d tg=%v", name, w.Code, gh.calls, stub.methods)
		}
		logs := buf.String()
		for _, id := range []int64{c.chat, c.user} {
			if strings.Contains(logs, strconv.FormatInt(id, 10)) {
				t.Errorf("%s: log contains id %d: %s", name, id, logs)
			}
		}
	}
}

// githubStub is an httptest GitHub API that records what the real fixer sends.
type githubStub struct {
	mu      sync.Mutex
	queries []string
	creates []string // "<path> <labels>"
}

func (g *githubStub) handler(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/repos/mctlhq/mctl-api":
		_, _ = w.Write([]byte(`{"name":"mctl-api"}`))
	case r.Method == http.MethodGet && r.URL.Path == "/search/issues":
		g.queries = append(g.queries, r.URL.Query().Get("q"))
		_, _ = w.Write([]byte(`{"total_count":0,"incomplete_results":false,"items":[]}`))
	case r.Method == http.MethodPost && r.URL.Path == "/repos/mctlhq/mctl-api/issues":
		var req struct {
			Labels []string `json:"labels"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		g.creates = append(g.creates, r.URL.Path+" "+strings.Join(req.Labels, ","))
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"number":9,"html_url":"https://github.com/mctlhq/mctl-api/issues/9"}`))
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusTeapot)
	}
}

func TestScoutCallbackCreatesThroughGitHubAPI(t *testing.T) {
	// T9 at the HTTP layer: one POST /repos/mctlhq/<repo>/issues with only
	// spike:scout, even though the draft asks for other labels and the fixer
	// is configured for another owner.
	gs := &githubStub{}
	srv := httptest.NewServer(http.HandlerFunc(gs.handler))
	t.Cleanup(srv.Close)
	f := fixer.NewGitHubFixer("tok", "", "someone-else", "mctl-gitops", nil, false, 0, 0, gitopspath.Allowlist{})
	if err := f.SetBaseURL(srv.URL + "/"); err != nil {
		t.Fatal(err)
	}
	h, stub := scoutSetup(t, 77, "s", f)
	q := url.Values{"title": {"spike: x"}, "body": {"Source: https://a.example/p"}, "labels": {"bug,urgent"}}
	post(h, "s", callbackBody(t, 555, 77, "scout:create:1", "https://github.com/mctlhq/mctl-api/issues/new?"+q.Encode()))
	if len(gs.creates) != 1 || gs.creates[0] != "/repos/mctlhq/mctl-api/issues spike:scout" {
		t.Fatalf("creates %v", gs.creates)
	}
	if len(gs.queries) != 1 || gs.queries[0] != `org:mctlhq is:issue label:"spike:scout"` {
		t.Fatalf("queries %v", gs.queries)
	}
	if len(stub.bodies) == 0 || stub.bodies[0]["text"] != "Created mctl-api#9" {
		t.Fatalf("answer %v", stub.bodies)
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
