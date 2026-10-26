package scoutspike

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"testing"

	"github.com/google/go-github/v68/github"

	"github.com/mctlhq/mctl-agent/internal/notify"
)

func TestParseCallbackData(t *testing.T) {
	for s, want := range map[string]bool{
		"scout:create:1": true, "scout:create:12": true, "scout:create:99": true,
		"scout:create:0": false, "scout:create:01": false, "scout:create:100": false,
		"scout:create:": false, "scout:create:1x": false, "scout:other:1": false,
	} {
		if _, ok := ParseCallbackData(s); ok != want {
			t.Errorf("%q: got %v want %v", s, ok, want)
		}
	}
}

func draftURL(repo, title, body, labels string) string {
	q := url.Values{"title": {title}, "body": {body}}
	if labels != "" {
		q.Set("labels", labels)
	}
	return "https://github.com/mctlhq/" + repo + "/issues/new?" + q.Encode()
}

func markupFor(n string, raw string) notify.InlineKeyboardMarkup {
	return notify.InlineKeyboardMarkup{InlineKeyboard: [][]notify.InlineKeyboardButton{
		{{Text: "Other", URL: "https://example.com"}},
		{{Text: "Draft spike " + n, URL: raw}, {Text: "Create spike " + n, CallbackData: "scout:create:" + n}},
	}}
}

func TestFindDraft(t *testing.T) {
	m := markupFor("3", "https://x")
	if row, u, err := FindDraft(m, 3); err != nil || row != 1 || u != "https://x" {
		t.Fatalf("got %d %q %v", row, u, err)
	}
	if _, _, err := FindDraft(m, 4); !errors.Is(err, ErrDraftNotFound) {
		t.Error("missing button must fail")
	}
	if _, _, err := FindDraft(markupFor("3", ""), 3); !errors.Is(err, ErrDraftNotFound) {
		t.Error("button without url must fail")
	}
	dup := markupFor("3", "https://x")
	dup.InlineKeyboard = append(dup.InlineKeyboard, dup.InlineKeyboard[1])
	if _, _, err := FindDraft(dup, 3); !errors.Is(err, ErrDraftNotFound) {
		t.Error("duplicate button must fail")
	}
}

func TestParseDraftRefusals(t *testing.T) {
	allowed := map[string]bool{"mctl-api": true}
	good := "spike: x"
	body := "Source: https://a.example/p"
	q := "?title=spike%3A+x&body=Source%3A+https%3A%2F%2Fa.example%2Fp"
	cases := map[string]struct {
		raw  string
		want error
	}{
		"http":        {"http://github.com/mctlhq/mctl-api/issues/new" + q, ErrTargetNotAllowed},
		"suffix host": {"https://github.com.evil.io/mctlhq/mctl-api/issues/new" + q, ErrTargetNotAllowed},
		"gist":        {"https://gist.github.com/mctlhq/mctl-api/issues/new" + q, ErrTargetNotAllowed},
		"port":        {"https://github.com:443/mctlhq/mctl-api/issues/new" + q, ErrTargetNotAllowed},
		"userinfo":    {"https://u@github.com/mctlhq/mctl-api/issues/new" + q, ErrTargetNotAllowed},
		"owner":       {"https://github.com/other/mctl-api/issues/new" + q, ErrTargetNotAllowed},
		"repo":        {"https://github.com/mctlhq/nope/issues/new" + q, ErrTargetNotAllowed},
		"dotdot":      {"https://github.com/mctlhq/../issues/new" + q, ErrTargetNotAllowed},
		"extra path":  {"https://github.com/mctlhq/mctl-api/issues/new/x" + q, ErrTargetNotAllowed},
		"pulls":       {"https://github.com/mctlhq/mctl-api/pulls/new" + q, ErrTargetNotAllowed},
		"title":       {draftURL("mctl-api", "feat: x", body, ""), ErrDraftRejected},
		"no source":   {draftURL("mctl-api", good, "nothing", ""), ErrDraftRejected},
		"long title":  {draftURL("mctl-api", "spike: "+string(make([]rune, 0))+repeat("a", 300), body, ""), ErrDraftRejected},
		"long body":   {draftURL("mctl-api", good, body+"\n"+repeat("a", 70000), ""), ErrDraftRejected},
	}
	for name, c := range cases {
		if _, err := ParseDraft(c.raw, allowed); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v want %v", name, err, c.want)
		}
	}
}

func repeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}

func TestParseDraftSuccessForcesLabels(t *testing.T) {
	d, err := ParseDraft(draftURL("mctl-api", "spike: a b", "Intro\nSource: https://A.example/p/", "bug,spike:scout"), map[string]bool{"mctl-api": true})
	if err != nil {
		t.Fatal(err)
	}
	if d.Repo != "mctl-api" || d.Title != "spike: a b" || d.Body != "Intro\nSource: https://A.example/p/" {
		t.Fatalf("bad draft %+v", d)
	}
	if len(d.Labels) != 1 || d.Labels[0] != "spike:scout" {
		t.Fatalf("labels %v", d.Labels)
	}
	if d.Source != "https://a.example/p" {
		t.Fatalf("source %q", d.Source)
	}
}

func TestNormaliseURL(t *testing.T) {
	for in, want := range map[string]string{
		"HTTPS://Example.COM/a/":                     "https://example.com/a",
		"https://example.com/a#frag":                 "https://example.com/a",
		"https://example.com/a?utm_source=x&b=2&a=1": "https://example.com/a?a=1&b=2",
		"https://example.com/a?utm_medium=x":         "https://example.com/a",
	} {
		if got := NormaliseURL(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

type fakeGH struct {
	mu        sync.Mutex
	repoCode  int
	search    func(page int) (*github.IssuesSearchResult, error)
	creates   int
	searches  int
	createdAt []string
}

func (f *fakeGH) RepoAccessible(context.Context, string) (int, error) {
	if f.repoCode == 0 {
		return http.StatusOK, nil
	}
	return f.repoCode, nil
}
func (f *fakeGH) SearchIssues(_ context.Context, _ string, page int) (*github.IssuesSearchResult, error) {
	f.mu.Lock()
	f.searches++
	f.mu.Unlock()
	if f.search == nil {
		return &github.IssuesSearchResult{Total: github.Ptr(0)}, nil
	}
	return f.search(page)
}
func (f *fakeGH) CreateIssue(_ context.Context, repo, _, _ string, labels []string) (int, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates++
	f.createdAt = append(f.createdAt, repo+":"+labels[0])
	return 7, "https://github.com/mctlhq/" + repo + "/issues/7", nil
}

func testMarkup() notify.InlineKeyboardMarkup {
	return markupFor("1", draftURL("mctl-api", "spike: x", "Source: https://a.example/p", ""))
}

func TestCreateExistingMatchCreatesNothing(t *testing.T) {
	// Pins the duplicate check: a case/slash-variant Source must match.
	gh := &fakeGH{search: func(int) (*github.IssuesSearchResult, error) {
		return &github.IssuesSearchResult{Total: github.Ptr(1), Issues: []*github.Issue{{
			Number: github.Ptr(3), HTMLURL: github.Ptr("https://github.com/mctlhq/mctl-api/issues/3"),
			Body:          github.Ptr("Source: HTTPS://A.example/p/"),
			RepositoryURL: github.Ptr("https://api.github.com/repos/mctlhq/mctl-api"),
		}}}, nil
	}}
	res, err := NewService(gh, []string{"mctl-api"}).Create(context.Background(), testMarkup(), 1)
	if err != nil || !res.Existed || res.Number != 3 || res.Repo != "mctl-api" || gh.creates != 0 {
		t.Fatalf("res=%+v err=%v creates=%d", res, err, gh.creates)
	}
}

func TestCreateFailsClosedOnSearchProblems(t *testing.T) {
	// Pins fail-closed search: could not observe is never observed absent.
	full := func(page int) (*github.IssuesSearchResult, error) {
		issues := make([]*github.Issue, 100)
		for i := range issues {
			issues[i] = &github.Issue{Body: github.Ptr("Source: https://other.example")}
		}
		return &github.IssuesSearchResult{Total: github.Ptr(5000), Issues: issues}, nil
	}
	for name, fn := range map[string]func(int) (*github.IssuesSearchResult, error){
		"error": func(int) (*github.IssuesSearchResult, error) { return nil, errors.New("boom") },
		"incomplete": func(int) (*github.IssuesSearchResult, error) {
			return &github.IssuesSearchResult{IncompleteResults: github.Ptr(true)}, nil
		},
		"page cap": full,
	} {
		gh := &fakeGH{search: fn}
		if _, err := NewService(gh, []string{"mctl-api"}).Create(context.Background(), testMarkup(), 1); !errors.Is(err, ErrSearchFailed) || gh.creates != 0 {
			t.Errorf("%s: err=%v creates=%d", name, err, gh.creates)
		}
	}
}

func TestCreateRepoNotAccessible(t *testing.T) {
	gh := &fakeGH{repoCode: http.StatusNotFound}
	_, err := NewService(gh, []string{"mctl-api"}).Create(context.Background(), testMarkup(), 1)
	if !errors.Is(err, ErrRepoNotAccessible) || gh.creates != 0 || gh.searches != 0 {
		t.Fatalf("err=%v creates=%d searches=%d", err, gh.creates, gh.searches)
	}
}

func TestCreateDoubleTapCreatesOnce(t *testing.T) {
	// Pins the per-source guard: search lag (always empty) must not allow a second create.
	gh := &fakeGH{}
	svc := NewService(gh, []string{"mctl-api"})
	var wg sync.WaitGroup
	results := make([]Result, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := svc.Create(context.Background(), testMarkup(), 1)
			if err != nil {
				t.Error(err)
			}
			results[i] = r
		}(i)
	}
	wg.Wait()
	if gh.creates != 1 || gh.createdAt[0] != "mctl-api:spike:scout" {
		t.Fatalf("creates=%d %v", gh.creates, gh.createdAt)
	}
	if results[0].Existed == results[1].Existed {
		t.Fatalf("exactly one call should create: %+v", results)
	}
}
