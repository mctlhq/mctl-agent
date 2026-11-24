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

package fixer

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/go-github/v68/github"
)

// MaxCommitClockSkew is how far ahead of this process's clock a committer
// date may be before the history is refused. Committer dates are set by the
// client that made the commit, so a wrong clock can put one in the future,
// where it would stay "recent", and the newest change, until that date.
const MaxCommitClockSkew = 5 * time.Minute

// RecentBumpCommitCap is how many commits to one values file a single
// RecentImageBumps read accepts. A file that changed more often than this
// inside the window is reported as unreadable rather than as a shorter list.
const RecentBumpCommitCap = 20

// ImageBump is one commit that changed the chart-level image.tag of a values
// file: a deploy made through GitOps instead of through mctl-api.
type ImageBump struct {
	SHA         string    `json:"sha"`
	CommittedAt time.Time `json:"committed_at"`
	Subject     string    `json:"subject"`
	FromTag     string    `json:"from_tag"`
	ToTag       string    `json:"to_tag"`
}

// ImageBumpHistory is the result of one complete read of a values file's
// recent history. Bumps is empty, never nil, when the read succeeded and no
// commit since Since changed the tag: that is an observation. A read that
// failed has no ImageBumpHistory at all.
type ImageBumpHistory struct {
	Repo  string      `json:"repo"`
	Path  string      `json:"path"`
	Since time.Time   `json:"since"`
	Bumps []ImageBump `json:"bumps"`
}

// RecentImageBumps returns the commits to valuesPath since `since` that
// changed the chart-level image.tag, newest first.
//
// A bump is recognised by what the commit did to the file (image.tag before
// and after), not by its subject: the release-deploy and gitops-bump
// workflows write "chore: bump <team>/<service> to <tag>", but a bump made
// by hand deploys just the same, and a subject can name a service whose file
// the commit never touched.
//
// The commit time is the committer date. For the bots, which push straight
// to main, that is when the change landed. A bump merged through a pull
// request carries the date it was committed on its branch, so one that sat in
// review for longer than the window is not reported: that errs towards
// "no recent deploy seen", never towards a rollback of something else.
//
// Every failure is an error: the listing, a later page that was not read, a
// commit without a SHA or a date, a file revision that could not be fetched
// or whose image block was not understood, a tag change dated in the future.
// Only GitHub's own absence signals mean absent: an empty listing, and a 404
// for the file at a revision (created or deleted by that commit, so there is
// no tag change to report).
func (f *GitHubFixer) RecentImageBumps(ctx context.Context, valuesPath string, since time.Time) (*ImageBumpHistory, error) {
	if err := f.allowlist.Validate(valuesPath); err != nil {
		return nil, fmt.Errorf("gitops path rejected: %w", err)
	}

	commits, resp, err := f.client.Repositories.ListCommits(ctx, f.owner, f.repo, &github.CommitsListOptions{
		Path:        valuesPath,
		Since:       since,
		ListOptions: github.ListOptions{PerPage: RecentBumpCommitCap},
	})
	if err != nil {
		return nil, fmt.Errorf("listing commits for %s: %w", valuesPath, err)
	}
	if resp != nil && resp.NextPage != 0 {
		return nil, fmt.Errorf("more than %d commits to %s since %s: history not read in full",
			RecentBumpCommitCap, valuesPath, since.UTC().Format(time.RFC3339))
	}

	history := &ImageBumpHistory{
		Repo:  f.RepoFullName(),
		Path:  valuesPath,
		Since: since.UTC(),
		Bumps: []ImageBump{},
	}
	for _, c := range commits {
		sha := c.GetSHA()
		if sha == "" {
			return nil, fmt.Errorf("commit without a SHA in the history of %s", valuesPath)
		}
		at := c.GetCommit().GetCommitter().GetDate().Time
		if at.IsZero() {
			return nil, fmt.Errorf("commit %s has no committer date", sha)
		}
		if at.Before(since) {
			continue
		}
		if len(c.Parents) == 0 || c.Parents[0].GetSHA() == "" {
			continue // root commit: nothing before it to have changed
		}

		after, ok, err := f.imageTagAt(ctx, valuesPath, sha)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		before, ok, err := f.imageTagAt(ctx, valuesPath, c.Parents[0].GetSHA())
		if err != nil {
			return nil, err
		}
		if !ok || before == after {
			continue
		}

		if at.After(time.Now().Add(MaxCommitClockSkew)) {
			return nil, fmt.Errorf("commit %s changed the image tag of %s but is dated %s, in the future: commit times cannot be trusted",
				sha, valuesPath, at.UTC().Format(time.RFC3339))
		}

		subject, _, _ := strings.Cut(c.GetCommit().GetMessage(), "\n")
		history.Bumps = append(history.Bumps, ImageBump{
			SHA:         sha,
			CommittedAt: at.UTC(),
			Subject:     subject,
			FromTag:     before,
			ToTag:       after,
		})
	}
	return history, nil
}

// imageTagAt reads the chart-level image.tag of valuesPath at ref. ok is
// false when the file does not exist there (GitHub's 404) or declares no
// chart-level tag; any other failure is returned as an error, including a
// file whose image block was not understood.
func (f *GitHubFixer) imageTagAt(ctx context.Context, valuesPath, ref string) (tag string, ok bool, err error) {
	content, err := f.GetFileContent(ctx, valuesPath, ref)
	if err != nil {
		if ctx.Err() == nil && isGitHubNotFound(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("reading %s at %s: %w", valuesPath, ref, err)
	}
	image, ok, err := ReadImageRef(content)
	if err != nil {
		return "", false, fmt.Errorf("reading %s at %s: %w", valuesPath, ref, err)
	}
	return image.Tag, ok, nil
}
