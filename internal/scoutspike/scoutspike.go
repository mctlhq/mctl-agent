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

// Package scoutspike turns a scout-digest "Draft spike N" URL button into a
// GitHub issue, idempotently, for the "Create spike N" Telegram callback.
package scoutspike

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/go-github/v68/github"

	"github.com/mctlhq/mctl-agent/internal/notify"
)

const (
	// Owner is the only GitHub org a draft may target.
	Owner = "mctlhq"
	// Label is the only label ever applied.
	Label = "spike:scout"

	maxTitleRunes = 256
	maxBodyBytes  = 60000
	searchPerPage = 100
	searchMaxPage = 10
	memoTTL       = 10 * time.Minute
	searchQuery   = `org:mctlhq label:"spike:scout"`
)

// Errors map one-to-one onto the short feedback texts of the callback.
var (
	ErrDraftNotFound     = errors.New("draft not found")
	ErrTargetNotAllowed  = errors.New("draft target not allowed")
	ErrDraftRejected     = errors.New("draft rejected")
	ErrRepoNotAccessible = errors.New("repo not accessible")
	ErrSearchFailed      = errors.New("could not check for duplicates")
	ErrCreateFailed      = errors.New("create failed")
)

var (
	callbackRe = regexp.MustCompile(`^scout:create:([1-9][0-9]?)$`)
	sourceRe   = regexp.MustCompile(`(?m)^Source:[ \t]*(\S+)`)
	repoNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// ParseCallbackData parses "scout:create:N" with N in 1..99.
func ParseCallbackData(s string) (int, bool) {
	m := callbackRe.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	return n, err == nil
}

// ValidRepoName reports whether name is an acceptable allowlist entry.
func ValidRepoName(name string) bool {
	return repoNameRe.MatchString(name) && name != "." && name != ".."
}

// FindDraft returns the row and URL of the single "Draft spike N" URL button.
func FindDraft(markup notify.InlineKeyboardMarkup, n int) (int, string, error) {
	want := "Draft spike " + strconv.Itoa(n)
	row, rawURL, found := -1, "", 0
	for i, r := range markup.InlineKeyboard {
		for _, b := range r {
			if strings.TrimSpace(b.Text) == want && b.URL != "" {
				row, rawURL = i, b.URL
				found++
			}
		}
	}
	if found != 1 {
		return 0, "", ErrDraftNotFound
	}
	return row, rawURL, nil
}

// Draft is a parsed, validated issue draft.
type Draft struct {
	Repo   string
	Title  string
	Body   string
	Labels []string
	Source string // normalised
}

// ParseDraft validates a GitHub issues/new URL against the repo allowlist.
// The draft's own labels are ignored: Labels is always exactly [spike:scout].
func ParseDraft(rawURL string, allowedRepos map[string]bool) (Draft, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.Opaque != "" {
		return Draft{}, ErrTargetNotAllowed
	}
	parts := strings.Split(u.EscapedPath(), "/")
	if len(parts) != 5 || parts[0] != "" || parts[1] != Owner || parts[3] != "issues" || parts[4] != "new" {
		return Draft{}, ErrTargetNotAllowed
	}
	repo := parts[2]
	if !ValidRepoName(repo) || !allowedRepos[repo] {
		return Draft{}, ErrTargetNotAllowed
	}
	q := u.Query()
	title := strings.TrimSpace(q.Get("title"))
	body := q.Get("body")
	if !strings.HasPrefix(title, "spike:") || utf8.RuneCountInString(title) > maxTitleRunes || len(body) > maxBodyBytes {
		return Draft{}, ErrDraftRejected
	}
	src := SourceOf(body)
	if src == "" {
		return Draft{}, ErrDraftRejected
	}
	return Draft{Repo: repo, Title: title, Body: body, Labels: []string{Label}, Source: NormaliseURL(src)}, nil
}

// SourceOf returns the first "Source:" value of body, or "".
func SourceOf(body string) string {
	m := sourceRe.FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return m[1]
}

// NormaliseURL canonicalises a source URL for comparison: lowercase scheme
// and host, no fragment, no trailing slash, no utm_* parameters, remaining
// query sorted. Port of mctl-agents dedupe.normalise_url as described in the
// proposal; mctl-agents wins if the two differ.
func NormaliseURL(s string) string {
	s = strings.TrimSpace(s)
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return strings.ToLower(s)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Fragment = ""
	u.RawFragment = ""
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	var pairs []string
	for _, kv := range strings.Split(u.RawQuery, "&") {
		if kv == "" {
			continue
		}
		if strings.HasPrefix(strings.ToLower(kv), "utm_") {
			continue
		}
		pairs = append(pairs, kv)
	}
	sort.Strings(pairs)
	u.RawQuery = strings.Join(pairs, "&")
	return u.String()
}

// GitHub is the subset of the GitHub fixer this package needs.
type GitHub interface {
	RepoAccessible(ctx context.Context, repo string) (int, error)
	SearchIssues(ctx context.Context, query string, page int) (*github.IssuesSearchResult, error)
	CreateIssue(ctx context.Context, repo, title, body string, labels []string) (int, string, error)
}

// Result describes the issue a press resolved to.
type Result struct {
	Repo    string
	Number  int
	HTMLURL string
	Existed bool
	Row     int // keyboard row that held the draft button
}

type entry struct {
	mu      sync.Mutex
	created *Result
	expires time.Time
}

// Service creates spike issues from drafts.
type Service struct {
	gh      GitHub
	repos   map[string]bool
	now     func() time.Time
	mu      sync.Mutex
	entries map[string]*entry
}

// NewService builds a Service. An empty repo list disables it.
func NewService(gh GitHub, repos []string) *Service {
	m := map[string]bool{}
	for _, r := range repos {
		if ValidRepoName(r) {
			m[r] = true
		}
	}
	return &Service{gh: gh, repos: m, now: time.Now, entries: map[string]*entry{}}
}

// Enabled reports whether any repo is allowlisted.
func (s *Service) Enabled() bool { return s != nil && s.gh != nil && len(s.repos) > 0 }

// RepoCount is the size of the repo allowlist.
func (s *Service) RepoCount() int {
	if s == nil {
		return 0
	}
	return len(s.repos)
}

func (s *Service) entryFor(key string) *entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, e := range s.entries {
		if k != key && e.mu.TryLock() {
			if e.created == nil || now.After(e.expires) {
				delete(s.entries, k)
			}
			e.mu.Unlock()
		}
	}
	e, ok := s.entries[key]
	if !ok {
		e = &entry{}
		s.entries[key] = e
	}
	return e
}

// Create resolves press n on markup to an issue, creating it only when no
// issue with the same Source exists. Any failure to observe duplicates
// creates nothing.
func (s *Service) Create(ctx context.Context, markup notify.InlineKeyboardMarkup, n int) (Result, error) {
	if !s.Enabled() {
		return Result{}, ErrTargetNotAllowed
	}
	row, rawURL, err := FindDraft(markup, n)
	if err != nil {
		return Result{}, err
	}
	d, err := ParseDraft(rawURL, s.repos)
	if err != nil {
		return Result{}, err
	}

	e := s.entryFor(d.Source)
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.created != nil && s.now().Before(e.expires) {
		r := *e.created
		r.Existed, r.Row = true, row
		return r, nil
	}

	if code, err := s.gh.RepoAccessible(ctx, d.Repo); err != nil || code != http.StatusOK {
		return Result{}, ErrRepoNotAccessible
	}

	found, err := s.findExisting(ctx, d.Source)
	if err != nil {
		return Result{}, err
	}
	if found != nil {
		found.Row = row
		return *found, nil
	}

	num, htmlURL, err := s.gh.CreateIssue(ctx, d.Repo, d.Title, d.Body, d.Labels)
	if err != nil {
		return Result{}, ErrCreateFailed
	}
	res := Result{Repo: d.Repo, Number: num, HTMLURL: htmlURL, Row: row}
	memo := res
	e.created, e.expires = &memo, s.now().Add(memoTTL)
	return res, nil
}

// findExisting searches every spike:scout issue (any state). Anything short
// of a complete, successful search is ErrSearchFailed.
func (s *Service) findExisting(ctx context.Context, source string) (*Result, error) {
	for page := 1; page <= searchMaxPage; page++ {
		res, err := s.gh.SearchIssues(ctx, searchQuery, page)
		if err != nil || res == nil || res.GetIncompleteResults() {
			return nil, ErrSearchFailed
		}
		for _, is := range res.Issues {
			src := SourceOf(is.GetBody())
			if src == "" || NormaliseURL(src) != source {
				continue
			}
			return &Result{
				Repo:    repoOf(is),
				Number:  is.GetNumber(),
				HTMLURL: is.GetHTMLURL(),
				Existed: true,
			}, nil
		}
		if len(res.Issues) < searchPerPage || page*searchPerPage >= res.GetTotal() {
			return nil, nil
		}
	}
	return nil, ErrSearchFailed
}

func repoOf(is *github.Issue) string {
	if r := is.GetRepository(); r != nil && r.GetName() != "" {
		return r.GetName()
	}
	// repository_url ends in /repos/<owner>/<repo>.
	u := is.GetRepositoryURL()
	return u[strings.LastIndex(u, "/")+1:]
}
