package main

// github.go — the only GitHub API we use: the Commit Status API (and, when PR
// polling is enabled, listing open PRs). A plain fine-grained PAT with
// "Commit statuses: write" + "Contents: read" is enough — no GitHub App.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Commit status states, per the GitHub API.
const (
	StatePending = "pending"
	StateSuccess = "success"
	StateFailure = "failure"
	StateError   = "error"
)

// GitHub is a tiny client for the Commit Status API.
type GitHub struct {
	token   string
	apiBase string
	http    *http.Client
}

// NewGitHub builds a client. apiBase is https://api.github.com (or a GHES base).
func NewGitHub(token, apiBase string) *GitHub {
	return &GitHub{
		token:   token,
		apiBase: apiBase,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// SetStatus posts a commit status: POST /repos/{owner}/{repo}/statuses/{sha}.
// context is the status "context" shown on GitHub (e.g. "ci/test").
func (g *GitHub) SetStatus(ctx context.Context, ref Ref, sha, state, statusContext, description string) error {
	body, _ := json.Marshal(map[string]string{
		"state":       state,
		"context":     statusContext,
		"description": truncate(description, 140), // GitHub caps description at 140 chars
	})
	url := fmt.Sprintf("%s/repos/%s/%s/statuses/%s", g.apiBase, ref.Owner, ref.Name, sha)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	g.setHeaders(req)

	resp, err := g.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("set status %s=%s: %s: %s", statusContext, state, resp.Status, bytes.TrimSpace(msg))
	}
	return nil
}

// PR is the subset of an open pull request we care about.
type PR struct {
	Number   int
	HeadSHA  string
	HeadRepo string // "owner/name" of the head; empty for deleted forks
}

// ListOpenPRs returns open pull requests for a repo (first page, up to 100).
func (g *GitHub) ListOpenPRs(ctx context.Context, ref Ref) ([]PR, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls?state=open&per_page=100", g.apiBase, ref.Owner, ref.Name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	g.setHeaders(req)

	resp, err := g.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("list PRs: %s: %s", resp.Status, bytes.TrimSpace(msg))
	}

	var raw []struct {
		Number int `json:"number"`
		Head   struct {
			SHA  string `json:"sha"`
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	prs := make([]PR, 0, len(raw))
	for _, r := range raw {
		prs = append(prs, PR{Number: r.Number, HeadSHA: r.Head.SHA, HeadRepo: r.Head.Repo.FullName})
	}
	return prs, nil
}

// FastForwardRef points branch at sha as a fast-forward (force=false, so GitHub
// rejects a non-fast-forward update with 422). It creates the branch if it does
// not exist yet. Needs the token to have "Contents: write".
func (g *GitHub) FastForwardRef(ctx context.Context, ref Ref, branch, sha string) error {
	url := fmt.Sprintf("%s/repos/%s/%s/git/refs/heads/%s", g.apiBase, ref.Owner, ref.Name, branch)
	body, _ := json.Marshal(map[string]any{"sha": sha, "force": false})
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	g.setHeaders(req)

	resp, err := g.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	// A 422 mentioning a missing reference means the branch is new — create it.
	if resp.StatusCode == http.StatusUnprocessableEntity && bytes.Contains(bytes.ToLower(msg), []byte("does not exist")) {
		return g.createRef(ctx, ref, branch, sha)
	}
	return fmt.Errorf("fast-forward %s to %s: %s: %s", branch, short(sha), resp.Status, bytes.TrimSpace(msg))
}

// createRef creates refs/heads/branch pointing at sha.
func (g *GitHub) createRef(ctx context.Context, ref Ref, branch, sha string) error {
	url := fmt.Sprintf("%s/repos/%s/%s/git/refs", g.apiBase, ref.Owner, ref.Name)
	body, _ := json.Marshal(map[string]string{"ref": "refs/heads/" + branch, "sha": sha})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	g.setHeaders(req)

	resp, err := g.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusCreated {
		return nil
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	return fmt.Errorf("create %s at %s: %s: %s", branch, short(sha), resp.Status, bytes.TrimSpace(msg))
}

func (g *GitHub) setHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "poll-ci")
	if req.Body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
}
