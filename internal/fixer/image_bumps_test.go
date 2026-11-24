package fixer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

const bumpPath = "platform-gitops/services/labs/mctl-telegram-preview/values.yaml"

func valuesWithTag(tag string) string {
	return "image:\n  repository: ghcr.io/mctlhq/mctl-telegram\n  tag: \"" + tag + "\"\nservice:\n  port: 8080\n"
}

// historyCommit is one row of a fake commit listing.
type historyCommit struct {
	sha, parent, subject string
	at                   time.Time
}

func (c historyCommit) json() map[string]any {
	row := map[string]any{"sha": c.sha}
	commit := map[string]any{"message": c.subject + "\n\nbody"}
	if !c.at.IsZero() {
		commit["committer"] = map[string]any{"name": "mctl-ci", "date": c.at.UTC().Format(time.RFC3339)}
	}
	row["commit"] = commit
	if c.parent != "" {
		row["parents"] = []map[string]any{{"sha": c.parent}}
	}
	return row
}

// historyAPI stubs the two GitHub routes RecentImageBumps reads. files maps a
// ref to the values file at that revision; a ref that is absent answers 404,
// and the value "!503" answers 503.
type historyAPI struct {
	commits    []historyCommit
	files      map[string]string
	listStatus int    // non-zero: the listing answers this status
	nextPage   bool   // the listing advertises a further page
	gotSince   string // the since parameter of the last listing
}

func (h *historyAPI) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/commits"):
			h.gotSince = r.URL.Query().Get("since")
			if got := r.URL.Query().Get("path"); got != bumpPath {
				t.Errorf("listing asked for path %q, want %q", got, bumpPath)
			}
			if h.listStatus != 0 {
				http.Error(w, `{"message":"unavailable"}`, h.listStatus)
				return
			}
			if h.nextPage {
				w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=2>; rel="next"`, r.Host, r.URL.Path))
			}
			rows := make([]map[string]any, 0, len(h.commits))
			for _, c := range h.commits {
				rows = append(rows, c.json())
			}
			_ = json.NewEncoder(w).Encode(rows)
		case strings.Contains(r.URL.Path, "/contents/"):
			body, ok := h.files[r.URL.Query().Get("ref")]
			switch {
			case !ok:
				http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			case body == "!503":
				http.Error(w, `{"message":"server overloaded"}`, http.StatusServiceUnavailable)
			default:
				_ = json.NewEncoder(w).Encode(map[string]any{
					"type": "file", "encoding": "base64", "sha": "blob", "content": encodeContent(body),
				})
			}
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}
}

func TestRecentImageBumps(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	since := now.Add(-time.Hour)
	bump := historyCommit{sha: "b1", parent: "p0", subject: "chore: bump labs/mctl-telegram-preview to main-new", at: now.Add(-5 * time.Minute)}

	tests := []struct {
		name    string
		api     historyAPI
		want    []ImageBump // nil with wantErr == "": an empty, observed history
		wantErr string
	}{
		{
			name: "a commit that changed the tag is a bump",
			api: historyAPI{
				commits: []historyCommit{bump},
				files:   map[string]string{"b1": valuesWithTag("main-new"), "p0": valuesWithTag("main-old")},
			},
			want: []ImageBump{{SHA: "b1", CommittedAt: bump.at, Subject: bump.subject, FromTag: "main-old", ToTag: "main-new"}},
		},
		{
			name: "a commit that left the tag alone is not a bump, whatever its subject",
			api: historyAPI{
				commits: []historyCommit{bump},
				files:   map[string]string{"b1": valuesWithTag("main-old") + "replicas: 2\n", "p0": valuesWithTag("main-old")},
			},
		},
		{
			name: "an empty listing is an observed empty history",
			api:  historyAPI{},
		},
		{
			name: "the commit that created the file changed no tag",
			api: historyAPI{
				commits: []historyCommit{bump},
				files:   map[string]string{"b1": valuesWithTag("main-new")},
			},
		},
		{
			name:    "a failed listing is an error",
			api:     historyAPI{listStatus: http.StatusServiceUnavailable},
			wantErr: "listing commits",
		},
		{
			name: "a further page that was not read is an error",
			api: historyAPI{
				nextPage: true,
				commits:  []historyCommit{bump},
				files:    map[string]string{"b1": valuesWithTag("main-new"), "p0": valuesWithTag("main-old")},
			},
			wantErr: "not read in full",
		},
		{
			name: "an unreadable revision is an error",
			api: historyAPI{
				commits: []historyCommit{bump},
				files:   map[string]string{"b1": valuesWithTag("main-new"), "p0": "!503"},
			},
			wantErr: "reading " + bumpPath + " at p0",
		},
		{
			name: "a commit without a date is an error",
			api: historyAPI{
				commits: []historyCommit{{sha: "b1", parent: "p0", subject: "chore: bump"}},
				files:   map[string]string{"b1": valuesWithTag("main-new"), "p0": valuesWithTag("main-old")},
			},
			wantErr: "no committer date",
		},
		{
			name: "a commit without a SHA is an error",
			api: historyAPI{
				commits: []historyCommit{{parent: "p0", subject: "chore: bump", at: bump.at}},
			},
			wantErr: "without a SHA",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := tt.api
			f, cleanup := fakeGH(t, api.handler(t))
			defer cleanup()

			got, err := f.RecentImageBumps(context.Background(), bumpPath, since)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("no error, history = %+v", got)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", err, tt.wantErr)
				}
				if got != nil {
					t.Fatalf("a failed read returned a history: %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Path != bumpPath || got.Repo != "mctlhq/mctl-gitops" || !got.Since.Equal(since) {
				t.Errorf("history header = %+v", got)
			}
			if got.Bumps == nil {
				t.Error("Bumps is nil; an observed empty history must marshal as [], not null")
			}
			if len(got.Bumps) != len(tt.want) {
				t.Fatalf("bumps = %+v, want %+v", got.Bumps, tt.want)
			}
			for i := range tt.want {
				if got.Bumps[i] != tt.want[i] {
					t.Errorf("bump %d = %+v, want %+v", i, got.Bumps[i], tt.want[i])
				}
			}
			if api.gotSince != since.Format(time.RFC3339) {
				t.Errorf("listing since = %q, want %q", api.gotSince, since.Format(time.RFC3339))
			}
		})
	}
}

// The allowlist bounds this read like every other: a path outside it is
// refused before any request is made.
func TestRecentImageBumpsRejectsOutOfAllowlistPath(t *testing.T) {
	f, cleanup := fakeGH(t, func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("request made for a rejected path: %s", r.URL.Path)
	})
	defer cleanup()

	_, err := f.RecentImageBumps(context.Background(),
		"platform-gitops/bootstrap/templates/mctl-platform/mctl-api.yaml", time.Now().Add(-time.Hour))
	if err == nil || !strings.Contains(err.Error(), "gitops path rejected") {
		t.Fatalf("error = %v, want a path rejection", err)
	}
}
