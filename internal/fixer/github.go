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
	"log/slog"
	"time"

	"github.com/google/go-github/v68/github"
	"github.com/mctlhq/mctl-agent/internal/ticket"
)

// GitHubFixer creates PRs in the GitOps repo via the GitHub API.
type GitHubFixer struct {
	client *github.Client
	owner  string
	repo   string
	store  *ticket.Store
	dryRun bool
}

// NewGitHubFixer creates a new GitHub PR fixer.
func NewGitHubFixer(token, owner, repo string, store *ticket.Store, dryRun bool) *GitHubFixer {
	client := github.NewClient(nil).WithAuthToken(token)
	return &GitHubFixer{
		client: client,
		owner:  owner,
		repo:   repo,
		store:  store,
		dryRun: dryRun,
	}
}

// PRRequest describes a PR to create.
type PRRequest struct {
	Ticket     *ticket.Ticket
	FilePath   string
	NewContent string
	Summary    string
	Diagnosis  string
	Confidence string
}

// CreatePR creates a branch, commits the change, and opens a PR.
func (f *GitHubFixer) CreatePR(ctx context.Context, req PRRequest) (string, int, error) {
	if f.dryRun {
		slog.Info("dry-run: would create PR",
			"ticket", req.Ticket.ID,
			"file", req.FilePath,
			"summary", req.Summary)
		return "", 0, nil
	}

	// Rate limiting.
	hourCount, err := f.store.CountPRsInWindow(1)
	if err != nil {
		return "", 0, fmt.Errorf("checking hourly PR count: %w", err)
	}
	dayCount, err := f.store.CountPRsInWindow(24)
	if err != nil {
		return "", 0, fmt.Errorf("checking daily PR count: %w", err)
	}
	if hourCount >= 5 {
		return "", 0, fmt.Errorf("hourly PR limit reached (%d/5)", hourCount)
	}
	if dayCount >= 20 {
		return "", 0, fmt.Errorf("daily PR limit reached (%d/20)", dayCount)
	}

	branchName := fmt.Sprintf("agent/fix/%s/%s-%s-%d",
		req.Ticket.Service, req.Ticket.Type,
		req.Ticket.ID[:8], time.Now().Unix())

	// 1. Get main branch SHA.
	mainRef, _, err := f.client.Git.GetRef(ctx, f.owner, f.repo, "refs/heads/main")
	if err != nil {
		return "", 0, fmt.Errorf("getting main ref: %w", err)
	}
	mainSHA := mainRef.Object.GetSHA()

	// 2. Create branch.
	newRef := &github.Reference{
		Ref:    github.Ptr("refs/heads/" + branchName),
		Object: &github.GitObject{SHA: github.Ptr(mainSHA)},
	}
	if _, _, err := f.client.Git.CreateRef(ctx, f.owner, f.repo, newRef); err != nil {
		return "", 0, fmt.Errorf("creating branch: %w", err)
	}

	// 3. Get current file content + SHA.
	fileContent, _, _, err := f.client.Repositories.GetContents(ctx, f.owner, f.repo, req.FilePath,
		&github.RepositoryContentGetOptions{Ref: "main"})
	if err != nil {
		return "", 0, fmt.Errorf("getting file content: %w", err)
	}

	// 4. Update file on branch.
	commitMsg := fmt.Sprintf("fix(%s): %s\n\nTicket: %s\nConfidence: %s\n\nAutomated fix by mctl-agent",
		req.Ticket.Service, req.Summary, req.Ticket.ID, req.Confidence)

	updateOpts := &github.RepositoryContentFileOptions{
		Message: github.Ptr(commitMsg),
		Content: []byte(req.NewContent),
		SHA:     github.Ptr(fileContent.GetSHA()),
		Branch:  github.Ptr(branchName),
		Author: &github.CommitAuthor{
			Name:  github.Ptr("mctl-agent"),
			Email: github.Ptr("agent@mctl.ai"),
		},
	}
	if _, _, err := f.client.Repositories.UpdateFile(ctx, f.owner, f.repo, req.FilePath, updateOpts); err != nil {
		return "", 0, fmt.Errorf("updating file: %w", err)
	}

	// 5. Create PR.
	prBody := fmt.Sprintf("## Automated Fix by mctl-agent\n\n"+
		"**Ticket:** %s\n"+
		"**Type:** %s\n"+
		"**Service:** %s/%s\n"+
		"**Confidence:** %s\n\n"+
		"### Diagnosis\n%s\n\n"+
		"### Changes\n%s\n\n"+
		"### Telegram Commands\n"+
		"- `/approve %s` — merge this PR\n"+
		"- `/reject %s <reason>` — close this PR\n\n"+
		"---\n"+
		"*This PR was automatically created by mctl-agent. Review carefully before merging.*",
		req.Ticket.ID, req.Ticket.Type, req.Ticket.Tenant, req.Ticket.Service,
		req.Confidence, req.Diagnosis, req.Summary,
		req.Ticket.ID[:8], req.Ticket.ID[:8])

	prTitle := fmt.Sprintf("fix(%s): %s", req.Ticket.Service, req.Summary)
	if len(prTitle) > 70 {
		prTitle = prTitle[:67] + "..."
	}

	pr, _, err := f.client.PullRequests.Create(ctx, f.owner, f.repo, &github.NewPullRequest{
		Title: github.Ptr(prTitle),
		Body:  github.Ptr(prBody),
		Head:  github.Ptr(branchName),
		Base:  github.Ptr("main"),
	})
	if err != nil {
		return "", 0, fmt.Errorf("creating PR: %w", err)
	}

	slog.Info("PR created",
		"ticket", req.Ticket.ID,
		"pr", pr.GetHTMLURL(),
		"number", pr.GetNumber())

	return pr.GetHTMLURL(), pr.GetNumber(), nil
}

// MergePR merges a PR by number.
func (f *GitHubFixer) MergePR(ctx context.Context, prNumber int) error {
	_, _, err := f.client.PullRequests.Merge(ctx, f.owner, f.repo, prNumber,
		"Approved via mctl-agent Telegram command",
		&github.PullRequestOptions{MergeMethod: "squash"})
	return err
}

// ClosePR closes a PR without merging.
func (f *GitHubFixer) ClosePR(ctx context.Context, prNumber int, reason string) error {
	_, _, err := f.client.PullRequests.Edit(ctx, f.owner, f.repo, prNumber,
		&github.PullRequest{
			State: github.Ptr("closed"),
			Body:  github.Ptr(fmt.Sprintf("Closed by mctl-agent: %s", reason)),
		})
	return err
}

// ListDir returns the names of entries in a repo directory at the given ref.
func (f *GitHubFixer) ListDir(ctx context.Context, path, ref string) ([]string, error) {
	_, dirContent, _, err := f.client.Repositories.GetContents(ctx, f.owner, f.repo, path,
		&github.RepositoryContentGetOptions{Ref: ref})
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(dirContent))
	for _, entry := range dirContent {
		if entry.GetType() == "dir" {
			names = append(names, entry.GetName())
		}
	}
	return names, nil
}

// GetFileContent fetches a file from the repo.
func (f *GitHubFixer) GetFileContent(ctx context.Context, path, ref string) (string, error) {
	fileContent, _, _, err := f.client.Repositories.GetContents(ctx, f.owner, f.repo, path,
		&github.RepositoryContentGetOptions{Ref: ref})
	if err != nil {
		return "", err
	}
	content, err := fileContent.GetContent()
	if err != nil {
		return "", fmt.Errorf("decoding file content: %w", err)
	}
	return content, nil
}
