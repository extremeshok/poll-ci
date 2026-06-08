package main

// runner.go — the heart of poll-ci. For each watched ref it reads the branch
// HEAD via `git ls-remote`, and when the SHA is new it checks the commit out,
// reads .poll-ci.yml, runs each check in the repo-declared image via the host
// Docker socket, and reports the results as GitHub commit statuses.
//
// Sources reach the check container by streaming a tar of the checkout in over
// `docker cp` — so the canonical run needs nothing mounted but the Docker
// socket (no shared host work directory).

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Runner ties together config, the GitHub client, and the seen-SHA store.
type Runner struct {
	cfg   *RunnerConfig
	gh    *GitHub
	store *Store
}

// NewRunner constructs a Runner.
func NewRunner(cfg *RunnerConfig, gh *GitHub, store *Store) *Runner {
	return &Runner{cfg: cfg, gh: gh, store: store}
}

// PollOnce sweeps every watched ref once (and open PRs, if enabled).
func (r *Runner) PollOnce(ctx context.Context) {
	for _, ref := range r.cfg.Refs {
		if ctx.Err() != nil {
			return
		}
		if err := r.processBranch(ctx, ref); err != nil {
			log.Printf("[%s] %v", ref, err)
		}
		if r.cfg.PollPRs {
			if err := r.processPRs(ctx, ref); err != nil {
				log.Printf("[%s] pulls: %v", ref.Repo(), err)
			}
		}
	}
}

// processBranch handles one watched branch: detect a new HEAD, then run it.
func (r *Runner) processBranch(ctx context.Context, ref Ref) error {
	tip, err := r.gitLsRemote(ctx, ref)
	if err != nil {
		return fmt.Errorf("ls-remote: %w", err)
	}
	if r.store.Has(ref, tip) {
		return nil // already processed this exact commit
	}
	log.Printf("[%s] new commit %s", ref, short(tip))

	dir, sha, err := r.cloneBranch(ctx, ref)
	if err != nil {
		return fmt.Errorf("clone: %w", err)
	}
	defer os.RemoveAll(dir)

	// The branch may have moved between ls-remote and clone; trust the cloned SHA.
	if r.store.Has(ref, sha) {
		return r.store.Mark(ref, tip)
	}
	passed := r.runCommit(ctx, ref, sha, dir)
	if ctx.Err() != nil {
		return nil // interrupted by shutdown — leave unmarked so it retries on restart
	}
	if passed {
		r.maybePromote(ctx, ref, sha, dir)
	}
	return r.store.Mark(ref, tip, sha)
}

// processPRs runs same-repo open PR heads. Fork PRs are intentionally skipped:
// running untrusted contributor code beside the Docker socket is dangerous.
func (r *Runner) processPRs(ctx context.Context, ref Ref) error {
	prs, err := r.gh.ListOpenPRs(ctx, ref)
	if err != nil {
		return err
	}
	for _, pr := range prs {
		if ctx.Err() != nil {
			return nil
		}
		if !strings.EqualFold(pr.HeadRepo, ref.Repo()) {
			continue // fork PR — skip for safety
		}
		if pr.HeadSHA == "" || r.store.Has(ref, pr.HeadSHA) {
			continue
		}
		log.Printf("[%s] PR #%d head %s", ref.Repo(), pr.Number, short(pr.HeadSHA))
		dir, err := r.cloneCommit(ctx, ref, pr.HeadSHA)
		if err != nil {
			log.Printf("[%s] PR #%d clone: %v", ref.Repo(), pr.Number, err)
			continue
		}
		r.runCommit(ctx, ref, pr.HeadSHA, dir)
		os.RemoveAll(dir)
		if ctx.Err() != nil {
			return nil // shutting down — leave unmarked so it retries on restart
		}
		if err := r.store.Mark(ref, pr.HeadSHA); err != nil {
			log.Printf("[%s] state: %v", ref.Repo(), err)
		}
	}
	return nil
}

// RunOnce processes a single SHA without consulting or updating state — for the
// ad-hoc `--once <sha>` mode. Returns true if every check passed.
func (r *Runner) RunOnce(ctx context.Context, ref Ref, sha string) bool {
	dir, err := r.cloneCommit(ctx, ref, sha)
	if err != nil {
		log.Printf("[%s] clone %s: %v", ref.Repo(), short(sha), err)
		return false
	}
	defer os.RemoveAll(dir)
	return r.runCommit(ctx, ref, sha, dir)
}

// runCommit reads the repo's .poll-ci.yml and runs every check, posting commit
// statuses throughout. Returns true if all checks passed.
func (r *Runner) runCommit(ctx context.Context, ref Ref, sha, dir string) bool {
	rc, err := r.readRepoConfig(dir)
	if err != nil {
		// No usable config: report one error status so the commit isn't silently ignored.
		log.Printf("[%s] %s: %v", ref, short(sha), err)
		r.setStatus(ctx, ref, sha, StateError, "ci", err.Error())
		return false
	}
	image := rc.Image
	if image == "" {
		image = r.cfg.DefaultImage
	}
	r.ensureImage(ctx, image)

	// Total gate steps = checks + the optional built-in trivy scan.
	total := len(rc.Checks)
	if rc.Scan != nil {
		total++
	}

	// Announce work immediately: overall pending + each step pending.
	r.setStatus(ctx, ref, sha, StatePending, "ci", fmt.Sprintf("running %d checks", total))
	for _, ck := range rc.Checks {
		r.setStatus(ctx, ref, sha, StatePending, "ci/"+ck.Name, "queued")
	}
	if rc.Scan != nil {
		r.setStatus(ctx, ref, sha, StatePending, "ci/trivy", "queued")
	}

	failed := 0
	for _, ck := range rc.Checks {
		if ctx.Err() != nil {
			return false
		}
		ok, desc := r.runCheck(ctx, image, dir, ck)
		state := StateSuccess
		if !ok {
			state, failed = StateFailure, failed+1
		}
		log.Printf("[%s] %s ci/%s → %s (%s)", ref, short(sha), ck.Name, state, desc)
		r.setStatus(ctx, ref, sha, state, "ci/"+ck.Name, desc)
	}

	// Built-in trivy scan of the sources (opt-in via scan:). A finding fails the
	// gate just like any check, so a promote: target won't advance.
	if rc.Scan != nil && ctx.Err() == nil {
		scanImage := rc.Scan.Image
		r.ensureImage(ctx, scanImage)
		ok, desc := r.runScan(ctx, scanImage, dir, rc.Scan)
		state := StateSuccess
		if !ok {
			state, failed = StateFailure, failed+1
		}
		log.Printf("[%s] %s ci/trivy → %s (%s)", ref, short(sha), state, desc)
		r.setStatus(ctx, ref, sha, state, "ci/trivy", desc)
	}

	if failed == 0 {
		r.setStatus(ctx, ref, sha, StateSuccess, "ci", fmt.Sprintf("all %d checks passed", total))
		return true
	}
	r.setStatus(ctx, ref, sha, StateFailure, "ci", fmt.Sprintf("%d of %d checks failed", failed, total))
	return false
}

// runScan runs the configured trivy filesystem scan against the copied-in
// checkout and returns (passed, one-line description). It mirrors runCheck but
// uses trivy's own entrypoint with argv passing (no shell) and mounts a named
// volume as the trivy cache so the vulnerability DB persists between runs.
func (r *Runner) runScan(ctx context.Context, image, dir string, sc *Scan) (bool, string) {
	timeout := r.cfg.DefaultTimeout
	if sc.Timeout > 0 {
		timeout = time.Duration(sc.Timeout) * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()

	target := "/repo"
	if p := strings.TrimSpace(sc.Path); p != "" && p != "." {
		target = "/repo/" + strings.TrimPrefix(p, "/")
	}
	ignoreUnfixed := sc.IgnoreUnfixed == nil || *sc.IgnoreUnfixed
	// trivy --cache-dir and --quiet are global, so they precede the subcommand.
	// --quiet silences trivy's INFO logger (stderr); the findings report still
	// prints to stdout, so it's streamed to the engine logs for the operator.
	args := []string{"--cache-dir", "/trivy-cache", "--quiet",
		"fs", "--scanners", sc.Scanners, "--severity", sc.Severity,
		"--exit-code", "1", "--no-progress"}
	if ignoreUnfixed {
		args = append(args, "--ignore-unfixed")
	}
	args = append(args, target)

	cid, err := r.dockerCreateScan(cctx, image, sc.CacheVolume, args)
	if err != nil {
		return false, "scan container create failed: " + oneLine(scrub(err.Error(), r.cfg.Token))
	}
	defer r.dockerRemove(cid)

	if err := r.dockerCopyIn(cctx, dir, cid); err != nil {
		return false, "copy sources failed: " + oneLine(err.Error())
	}

	r.dockerStart(cctx, cid) // streams trivy's report to the logs; blocks until exit
	if cctx.Err() == context.DeadlineExceeded {
		return false, fmt.Sprintf("timed out after %s", timeout)
	}
	if ctx.Err() != nil {
		return false, "cancelled"
	}
	return scanResult(r.dockerExitCode(cid), sc.Severity, time.Since(start).Round(time.Second))
}

// scanResult maps a trivy exit code to (passed, one-line description). trivy
// exits 0 when nothing is found and 1 when it finds something at or above the
// configured severity. The findings themselves go to the streamed logs, so the
// description is a clear summary rather than a scraped output line (trivy writes
// its report to stdout and INFO logs to stderr — the opposite of a normal check,
// which is what made the old "last stderr line" heuristic misleading here).
func scanResult(exitCode int, severity string, dur time.Duration) (bool, string) {
	if exitCode == 0 {
		return true, "no findings in " + dur.String()
	}
	return false, "trivy found issues at " + severity + " — see ci/trivy logs"
}

// maybePromote fast-forwards the repo's configured promote.branch to the tested
// commit. It runs only for branch runs that passed every check — never for PRs
// or --once. A failure is reported as a ci/promote commit status and logged, but
// does not block marking the commit processed: the operator fixes the cause
// (usually a token lacking Contents:write, or a diverged target) and the next
// green commit promotes.
func (r *Runner) maybePromote(ctx context.Context, ref Ref, sha, dir string) {
	rc, err := r.readRepoConfig(dir)
	if err != nil || rc.Promote == nil {
		return
	}
	target := strings.TrimSpace(rc.Promote.Branch)
	if target == "" {
		return
	}
	if target == ref.Branch {
		log.Printf("[%s] %s: promote.branch is the watched branch; skipping", ref, short(sha))
		return
	}

	r.setStatus(ctx, ref, sha, StatePending, "ci/promote", "promoting to "+target)
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if ctx.Err() != nil {
			return // shutting down — leave the target as-is; re-runs on restart
		}
		// Detached timeout so an in-flight ref update still completes on shutdown.
		pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		err := r.gh.FastForwardRef(pctx, ref, target, sha)
		cancel()
		if err == nil {
			log.Printf("[%s] promoted %s → %s", ref, short(sha), target)
			r.setStatus(ctx, ref, sha, StateSuccess, "ci/promote", "fast-forwarded "+target+" to "+short(sha))
			return
		}
		lastErr = err
		if attempt < 3 {
			time.Sleep(2 * time.Second)
		}
	}
	log.Printf("[%s] %s: promote to %s failed: %v", ref, short(sha), target, lastErr)
	r.setStatus(ctx, ref, sha, StateError, "ci/promote", "fast-forward "+target+" failed: "+oneLine(lastErr.Error()))
}

// runCheck runs one check in a fresh container built from `image`, with the
// checkout copied to /repo. Returns (passed, one-line description).
func (r *Runner) runCheck(ctx context.Context, image, dir string, ck Check) (bool, string) {
	timeout := r.cfg.DefaultTimeout
	if ck.Timeout > 0 {
		timeout = time.Duration(ck.Timeout) * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()

	cid, err := r.dockerCreate(cctx, image, ck.Run)
	if err != nil {
		return false, "container create failed: " + oneLine(scrub(err.Error(), r.cfg.Token))
	}
	defer r.dockerRemove(cid)

	if err := r.dockerCopyIn(cctx, dir, cid); err != nil {
		return false, "copy sources failed: " + oneLine(err.Error())
	}

	stdout, stderr := r.dockerStart(cctx, cid)
	if cctx.Err() == context.DeadlineExceeded {
		return false, fmt.Sprintf("timed out after %s", timeout)
	}
	if ctx.Err() != nil {
		return false, "cancelled"
	}

	code := r.dockerExitCode(cid)
	dur := time.Since(start).Round(time.Second)
	if code == 0 {
		return true, "passed in " + dur.String()
	}
	// Prefer the last stderr line (where tools usually print the real error),
	// falling back to stdout.
	last := lastLine(stderr)
	if last == "" {
		last = lastLine(stdout)
	}
	if last == "" {
		last = "no output"
	}
	return false, fmt.Sprintf("exit %d: %s", code, last)
}

func (r *Runner) readRepoConfig(dir string) (*RepoConfig, error) {
	for _, name := range []string{".poll-ci.yml", ".poll-ci.yaml"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err == nil {
			return parseRepoConfig(data)
		}
		if !os.IsNotExist(err) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("no .poll-ci.yml at repo root")
}

// setStatus posts a status, logging (but not aborting) on failure.
func (r *Runner) setStatus(ctx context.Context, ref Ref, sha, state, sctx, desc string) {
	// Use a detached context so shutdown still records the final result.
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := r.gh.SetStatus(pctx, ref, sha, state, sctx, desc); err != nil {
		log.Printf("[%s] %s: status %s=%s failed: %v", ref, short(sha), sctx, state, err)
	}
}

// --- Docker helpers (sibling containers via the host socket) ----------------

// ensureImage pulls the image only if it isn't already present, so the per-check
// timeout never includes a first-time pull.
func (r *Runner) ensureImage(ctx context.Context, image string) {
	if exec.CommandContext(ctx, r.cfg.DockerBin, "image", "inspect", image).Run() == nil {
		return
	}
	log.Printf("pulling image %s", image)
	// Bound the pull so a hung registry can't stall the whole poll loop.
	pctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(pctx, r.cfg.DockerBin, "pull", image)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		log.Printf("pull %s failed (will let create surface it): %v", image, err)
	}
}

// dockerCreate creates (but does not start) a container that runs the check.
func (r *Runner) dockerCreate(ctx context.Context, image, run string) (string, error) {
	out, errb, err := r.dockerRun(ctx, "create", "--label", "poll-ci", "-w", "/repo", image, "sh", "-ec", run)
	if err != nil {
		return "", fmt.Errorf("%v: %s", err, oneLine(errb))
	}
	return strings.TrimSpace(out), nil
}

// dockerCreateScan creates (but does not start) the trivy scan container. Unlike
// dockerCreate it does NOT wrap the command in `sh -ec`: it relies on the trivy
// image's own entrypoint and passes trivyArgs as separate argv elements (so no
// shell parses user-supplied scanners/severity/path). A named volume is mounted
// as the trivy cache to persist the vulnerability DB across runs.
func (r *Runner) dockerCreateScan(ctx context.Context, image, cacheVolume string, trivyArgs []string) (string, error) {
	args := append([]string{
		"create", "--label", "poll-ci", "-w", "/repo",
		"-v", cacheVolume + ":/trivy-cache", image,
	}, trivyArgs...)
	out, errb, err := r.dockerRun(ctx, args...)
	if err != nil {
		return "", fmt.Errorf("%v: %s", err, oneLine(errb))
	}
	return strings.TrimSpace(out), nil
}

// dockerCopyIn streams the checkout into the container at /repo via `docker cp`.
func (r *Runner) dockerCopyIn(ctx context.Context, dir, cid string) error {
	cmd := exec.CommandContext(ctx, r.cfg.DockerBin, "cp", "-", cid+":/")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		return err
	}
	// Entries are prefixed with "repo/", so extracting at "/" yields "/repo".
	tarErr := writeTar(stdin, dir, "repo")
	stdin.Close()
	waitErr := cmd.Wait()
	if tarErr != nil {
		return tarErr
	}
	if waitErr != nil {
		return fmt.Errorf("%v: %s", waitErr, oneLine(errb.String()))
	}
	return nil
}

// dockerStart starts + attaches the container, streaming output live to stdout
// while keeping the tail of each stream separately for the status description.
func (r *Runner) dockerStart(ctx context.Context, cid string) (stdout, stderr string) {
	so := &tailBuffer{max: 64 << 10}
	se := &tailBuffer{max: 64 << 10}
	cmd := exec.CommandContext(ctx, r.cfg.DockerBin, "start", "-a", cid)
	cmd.Stdout = io.MultiWriter(so, os.Stdout)
	cmd.Stderr = io.MultiWriter(se, os.Stdout)
	_ = cmd.Run() // a non-zero check exit is expected; the real code comes from inspect
	return so.String(), se.String()
}

// dockerExitCode reads the container's exit code. Detached context so it still
// works after a per-check timeout cancelled the parent context.
func (r *Runner) dockerExitCode(cid string) int {
	out, _, err := r.dockerRun(context.Background(), "inspect", "-f", "{{.State.ExitCode}}", cid)
	if err != nil {
		return -1
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return -1
	}
	return n
}

// dockerRemove force-removes the container (kills it if still running).
func (r *Runner) dockerRemove(cid string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, r.cfg.DockerBin, "rm", "-f", cid).Run()
}

func (r *Runner) dockerRun(ctx context.Context, args ...string) (stdout, stderr string, err error) {
	cmd := exec.CommandContext(ctx, r.cfg.DockerBin, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	return out.String(), errb.String(), err
}

// --- Git helpers (outbound only) --------------------------------------------

func (r *Runner) repoURL(ref Ref) string {
	return fmt.Sprintf("https://%s/%s/%s.git", r.cfg.GitHost, ref.Owner, ref.Name)
}

// gitArgs prepends an http.extraHeader carrying the token. Because it is passed
// with -c (not configured), the token is never written to the clone's
// .git/config — which we later tar into the check container.
func (r *Runner) gitArgs(extra ...string) []string {
	auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + r.cfg.Token))
	return append([]string{"-c", "http.extraHeader=AUTHORIZATION: basic " + auth}, extra...)
}

// gitLsRemote returns the HEAD SHA of the watched branch.
func (r *Runner) gitLsRemote(ctx context.Context, ref Ref) (string, error) {
	out, err := r.git(ctx, "", r.gitArgs("ls-remote", r.repoURL(ref), "refs/heads/"+ref.Branch)...)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "", fmt.Errorf("branch %q not found", ref.Branch)
	}
	return fields[0], nil
}

// cloneBranch shallow-clones the branch tip and returns its directory and SHA.
func (r *Runner) cloneBranch(ctx context.Context, ref Ref) (dir, sha string, err error) {
	dir, err = os.MkdirTemp(r.cfg.WorkDir, "co-")
	if err != nil {
		return "", "", err
	}
	args := r.gitArgs("clone", "--depth", "1", "--single-branch", "--branch", ref.Branch, r.repoURL(ref), dir)
	if _, err = r.git(ctx, "", args...); err != nil {
		os.RemoveAll(dir)
		return "", "", err
	}
	sha, err = r.git(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		os.RemoveAll(dir)
		return "", "", err
	}
	return dir, sha, nil
}

// cloneCommit fetches a specific commit (branch head, PR head, or arbitrary SHA).
func (r *Runner) cloneCommit(ctx context.Context, ref Ref, sha string) (string, error) {
	dir, err := os.MkdirTemp(r.cfg.WorkDir, "co-")
	if err != nil {
		return "", err
	}
	steps := [][]string{
		{"init", "-q", dir},
		r.gitArgs("-C", dir, "fetch", "--depth", "1", r.repoURL(ref), sha),
		{"-C", dir, "checkout", "-q", "FETCH_HEAD"},
	}
	for _, s := range steps {
		if _, err := r.git(ctx, "", s...); err != nil {
			os.RemoveAll(dir)
			return "", err
		}
	}
	return dir, nil
}

// git runs a git command, scrubbing the token from any error output. Args are
// never echoed (they carry the auth header).
func (r *Runner) git(ctx context.Context, workdir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if workdir != "" {
		cmd.Dir = workdir
	}
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0") // never hang on a credential prompt
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%v: %s", err, scrub(oneLine(errb.String()), r.cfg.Token))
	}
	return strings.TrimSpace(out.String()), nil
}
