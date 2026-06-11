package main

// github.go — the only GitHub API we use: the Commit Status API (and, when PR
// polling is enabled, listing open PRs). A plain fine-grained PAT with
// "Commit statuses: write" + "Contents: read" is enough — no GitHub App.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
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

// apiError is a non-2xx GitHub API response. RetryAfter is non-zero when the
// response carried a Retry-After header (GitHub's secondary rate limiting).
type apiError struct {
	Code       int
	RetryAfter time.Duration
	Msg        string
}

func (e *apiError) Error() string { return e.Msg }

// apiErr drains the response body and wraps a non-2xx response as an *apiError.
func apiErr(resp *http.Response, what string) error {
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	e := &apiError{Code: resp.StatusCode, Msg: fmt.Sprintf("%s: %s: %s", what, resp.Status, bytes.TrimSpace(msg))}
	if s := resp.Header.Get("Retry-After"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			e.RetryAfter = time.Duration(n) * time.Second
		}
	}
	return e
}

// retryable reports whether a failed API call is worth retrying, and any
// server-mandated wait. Transport errors and 5xx are transient; 429 and 403
// carrying Retry-After are rate limits. Other 4xx (401, 404, 422 — and a bare
// 403, which means token permissions) are permanent.
func retryable(err error) (wait time.Duration, ok bool) {
	var ae *apiError
	if !errors.As(err, &ae) {
		return 0, true // transport-level error — transient
	}
	switch {
	case ae.Code >= 500, ae.Code == http.StatusTooManyRequests:
		return ae.RetryAfter, true
	case ae.Code == http.StatusForbidden && ae.RetryAfter > 0:
		return ae.RetryAfter, true
	}
	return 0, false
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
		return apiErr(resp, fmt.Sprintf("set status %s=%s", statusContext, state))
	}
	return nil
}

// Status is a commit status (the latest per context, after ListStatuses dedupes).
type Status struct {
	Context string `json:"context"`
	State   string `json:"state"`
}

// ListStatuses returns the latest status per context for a SHA. The endpoint
// lists every historical status newest-first, paginated; three pages is far
// beyond any sane number of contexts.
func (g *GitHub) ListStatuses(ctx context.Context, ref Ref, sha string) ([]Status, error) {
	seen := map[string]bool{}
	var out []Status
	for page := 1; page <= 3; page++ {
		url := fmt.Sprintf("%s/repos/%s/%s/commits/%s/statuses?per_page=100&page=%d", g.apiBase, ref.Owner, ref.Name, sha, page)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		g.setHeaders(req)
		resp, err := g.http.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			err := apiErr(resp, "list statuses")
			resp.Body.Close()
			return nil, err
		}
		var raw []Status
		err = json.NewDecoder(resp.Body).Decode(&raw)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		for _, st := range raw {
			if !seen[st.Context] {
				seen[st.Context] = true
				out = append(out, st)
			}
		}
		if len(raw) < 100 {
			break
		}
	}
	return out, nil
}

// PR is the subset of an open pull request we care about.
type PR struct {
	Number   int
	HeadSHA  string
	HeadRepo string // "owner/name" of the head; empty for deleted forks
}

// ListOpenPRs returns open pull requests for a repo, paginated (capped at 10
// pages / 1000 PRs — far beyond any repo poll-ci should be watching).
func (g *GitHub) ListOpenPRs(ctx context.Context, ref Ref) ([]PR, error) {
	var prs []PR
	for page := 1; page <= 10; page++ {
		url := fmt.Sprintf("%s/repos/%s/%s/pulls?state=open&per_page=100&page=%d", g.apiBase, ref.Owner, ref.Name, page)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		g.setHeaders(req)

		resp, err := g.http.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			err := apiErr(resp, "list PRs")
			resp.Body.Close()
			return nil, err
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
		err = json.NewDecoder(resp.Body).Decode(&raw)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		for _, r := range raw {
			prs = append(prs, PR{Number: r.Number, HeadSHA: r.Head.SHA, HeadRepo: r.Head.Repo.FullName})
		}
		if len(raw) < 100 {
			break
		}
	}
	return prs, nil
}

// CompareCommits returns the commit SHAs reachable from head but not base,
// oldest first (GitHub caps the list at 250 — plenty for marking the skipped
// intermediates of a push).
func (g *GitHub) CompareCommits(ctx context.Context, ref Ref, base, head string) ([]string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/compare/%s...%s", g.apiBase, ref.Owner, ref.Name, base, head)
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
		return nil, apiErr(resp, fmt.Sprintf("compare %s...%s", short(base), short(head)))
	}
	var raw struct {
		Commits []struct {
			SHA string `json:"sha"`
		} `json:"commits"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	shas := make([]string, 0, len(raw.Commits))
	for _, c := range raw.Commits {
		shas = append(shas, c.SHA)
	}
	return shas, nil
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
