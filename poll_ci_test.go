package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBoolEnv(t *testing.T) {
	const k = "POLLCI_TEST_SHORT_CIRCUIT"
	os.Unsetenv(k)
	if !boolEnv(k, true) || boolEnv(k, false) {
		t.Error("unset should return the default")
	}
	for _, v := range []string{"1", "true", "TRUE", "yes", "on"} {
		t.Setenv(k, v)
		if !boolEnv(k, false) {
			t.Errorf("boolEnv(%q) should be true", v)
		}
	}
	for _, v := range []string{"0", "false", "No", "off"} {
		t.Setenv(k, v)
		if boolEnv(k, true) {
			t.Errorf("boolEnv(%q) should be false", v)
		}
	}
	t.Setenv(k, "garbage")
	if !boolEnv(k, true) || boolEnv(k, false) {
		t.Error("unrecognized value should return the default")
	}
}

func TestSupersededError(t *testing.T) {
	err := error(&supersededError{by: "abc1234"})
	if err.Error() != "superseded by abc1234" {
		t.Errorf("message: %q", err.Error())
	}
	var se *supersededError
	if !errors.As(err, &se) || se.by != "abc1234" {
		t.Errorf("errors.As should extract the newer SHA, got %+v", se)
	}
	if errors.As(errors.New("unrelated"), &se) {
		t.Error("a plain error must not match supersededError")
	}
}

func TestScanResult(t *testing.T) {
	// Clean: exit 0 → passed, duration in the message.
	ok, desc := scanResult(0, "HIGH,CRITICAL", 5*time.Second)
	if !ok || desc != "no findings in 5s" {
		t.Fatalf("clean: ok=%v desc=%q", ok, desc)
	}
	// Findings: exit 1 → failed, severity-aware message, and crucially NOT a
	// scraped trivy log line (the bug this guards against).
	ok, desc = scanResult(1, "HIGH,CRITICAL", 8*time.Second)
	if ok {
		t.Fatal("exit 1 should fail the scan")
	}
	if !strings.Contains(desc, "HIGH,CRITICAL") {
		t.Errorf("description should name the severity: %q", desc)
	}
	if strings.Contains(desc, "INFO") || strings.Contains(desc, "config files") {
		t.Errorf("description leaked a trivy log line: %q", desc)
	}
}

func TestParseRef(t *testing.T) {
	r, err := parseRef("owner/name", "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Owner != "owner" || r.Name != "name" || r.Branch != "main" {
		t.Fatalf("got %+v", r)
	}
	if r.Repo() != "owner/name" {
		t.Fatalf("Repo()=%q", r.Repo())
	}
	if r.String() != "owner/name@main" {
		t.Fatalf("String()=%q", r.String())
	}
	for _, bad := range []string{"bad", "/x", "x/", "a/b/c", ""} {
		if _, err := parseRef(bad, "main"); err == nil {
			t.Errorf("parseRef(%q) expected error", bad)
		}
	}
}

func TestParseRepoConfig(t *testing.T) {
	rc, err := parseRepoConfig([]byte("image: alpine\nchecks:\n  - name: t\n    run: echo hi\n    timeout: 30\n"))
	if err != nil {
		t.Fatal(err)
	}
	if rc.Image != "alpine" || len(rc.Checks) != 1 || rc.Checks[0].Name != "t" || rc.Checks[0].Timeout != 30 {
		t.Fatalf("got %+v", rc)
	}

	bad := map[string]string{
		"no checks":    "image: alpine\n",
		"empty checks": "checks: []\n",
		"missing run":  "checks:\n  - name: a\n",
		"missing name": "checks:\n  - run: x\n",
		"duplicate":    "checks:\n  - name: a\n    run: x\n  - name: a\n    run: y\n",
		"not yaml":     "::: not yaml :::\n\t- broken",
	}
	for label, in := range bad {
		if _, err := parseRepoConfig([]byte(in)); err == nil {
			t.Errorf("%s: expected error", label)
		}
	}
}

func TestParseRepoConfigPromote(t *testing.T) {
	// A valid promote block parses into Promote.Branch.
	rc, err := parseRepoConfig([]byte("checks:\n  - name: t\n    run: echo hi\npromote:\n  branch: release\n"))
	if err != nil {
		t.Fatal(err)
	}
	if rc.Promote == nil || rc.Promote.Branch != "release" {
		t.Fatalf("promote not parsed: %+v", rc.Promote)
	}

	// No promote block → nil (promotion is opt-in).
	rc2, err := parseRepoConfig([]byte("checks:\n  - name: t\n    run: x\n"))
	if err != nil {
		t.Fatal(err)
	}
	if rc2.Promote != nil {
		t.Errorf("expected nil promote, got %+v", rc2.Promote)
	}

	// promote present but blank/whitespace branch is an error.
	for _, in := range []string{
		"checks:\n  - name: t\n    run: x\npromote:\n  branch: \"\"\n",
		"checks:\n  - name: t\n    run: x\npromote:\n  branch: \"   \"\n",
		"checks:\n  - name: t\n    run: x\npromote: {}\n",
	} {
		if _, err := parseRepoConfig([]byte(in)); err == nil {
			t.Errorf("expected error for promote without a branch: %q", in)
		}
	}
}

func TestParseRepoConfigScan(t *testing.T) {
	// No scan block → nil (scanning is opt-in).
	rc0, err := parseRepoConfig([]byte("checks:\n  - name: t\n    run: x\n"))
	if err != nil {
		t.Fatal(err)
	}
	if rc0.Scan != nil {
		t.Errorf("expected nil scan, got %+v", rc0.Scan)
	}

	// Minimal `scan: {}` → all fields default; ignore-unfixed defaults true.
	rc, err := parseRepoConfig([]byte("checks:\n  - name: t\n    run: x\nscan: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if rc.Scan == nil {
		t.Fatal("scan not parsed")
	}
	if rc.Scan.Image != DefaultTrivyImage || rc.Scan.Scanners != DefaultTrivyScanners ||
		rc.Scan.Severity != DefaultTrivySeverity || rc.Scan.CacheVolume != DefaultTrivyCacheVolume ||
		rc.Scan.Path != "." {
		t.Fatalf("scan defaults not applied: %+v", rc.Scan)
	}
	if rc.Scan.IgnoreUnfixed != nil && *rc.Scan.IgnoreUnfixed != true {
		t.Errorf("ignore-unfixed should default to true, got %v", *rc.Scan.IgnoreUnfixed)
	}

	// Overrides parse, including ignore-unfixed: false.
	rc2, err := parseRepoConfig([]byte("checks:\n  - name: t\n    run: x\n" +
		"scan:\n  image: aquasec/trivy:0.58.1\n  scanners: vuln\n  severity: CRITICAL\n" +
		"  ignore-unfixed: false\n  path: web\n"))
	if err != nil {
		t.Fatal(err)
	}
	if rc2.Scan.Image != "aquasec/trivy:0.58.1" || rc2.Scan.Scanners != "vuln" ||
		rc2.Scan.Severity != "CRITICAL" || rc2.Scan.Path != "web" {
		t.Fatalf("scan overrides not parsed: %+v", rc2.Scan)
	}
	if rc2.Scan.IgnoreUnfixed == nil || *rc2.Scan.IgnoreUnfixed != false {
		t.Errorf("ignore-unfixed: false not parsed: %+v", rc2.Scan.IgnoreUnfixed)
	}

	// Unsafe values (shell metacharacters) are rejected.
	for _, in := range []string{
		"checks:\n  - name: t\n    run: x\nscan:\n  severity: \"HIGH; rm -rf /\"\n",
		"checks:\n  - name: t\n    run: x\nscan:\n  scanners: \"vuln`id`\"\n",
		"checks:\n  - name: t\n    run: x\nscan:\n  image: \"a b\"\n",
	} {
		if _, err := parseRepoConfig([]byte(in)); err == nil {
			t.Errorf("expected error for unsafe scan value: %q", in)
		}
	}

	// Path traversal in scan.path is rejected (".." passes safeArg's charset).
	for _, in := range []string{
		"checks:\n  - name: t\n    run: x\nscan:\n  path: web/../../etc\n",
		"checks:\n  - name: t\n    run: x\nscan:\n  path: a/..\n",
	} {
		if _, err := parseRepoConfig([]byte(in)); err == nil {
			t.Errorf("expected error for traversing scan.path: %q", in)
		}
	}
	// A harmless dotted path element still parses.
	if _, err := parseRepoConfig([]byte("checks:\n  - name: t\n    run: x\nscan:\n  path: web/v1.2\n")); err != nil {
		t.Errorf("dotted scan.path should parse: %v", err)
	}
}

func TestSecondsOrInvalid(t *testing.T) {
	const k = "POLLCI_TEST_SECONDS"
	for _, bad := range []string{"5m", "0", "-3", "abc"} {
		t.Setenv(k, bad)
		if got := secondsOr(k, 60); got != 60*time.Second {
			t.Errorf("%q: got %s, want fallback 60s", bad, got)
		}
	}
	t.Setenv(k, "90")
	if got := secondsOr(k, 60); got != 90*time.Second {
		t.Errorf("valid: got %s", got)
	}
}

// ListOpenPRs must walk pages, not stop at the first 100.
func TestListOpenPRsPagination(t *testing.T) {
	mk := func(n int) map[string]any {
		return map[string]any{
			"number": n,
			"head": map[string]any{
				"sha":  fmt.Sprintf("sha%d", n),
				"repo": map[string]any{"full_name": "o/n"},
			},
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("page") {
		case "1":
			page := make([]map[string]any, 100)
			for i := range page {
				page[i] = mk(i + 1)
			}
			json.NewEncoder(w).Encode(page)
		case "2":
			json.NewEncoder(w).Encode([]map[string]any{mk(101)})
		default:
			json.NewEncoder(w).Encode([]map[string]any{})
		}
	}))
	defer srv.Close()
	gh := NewGitHub("tok", srv.URL)
	prs, err := gh.ListOpenPRs(context.Background(), Ref{Owner: "o", Name: "n"})
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 101 {
		t.Fatalf("got %d PRs, want 101", len(prs))
	}
	if prs[100].Number != 101 || prs[100].HeadSHA != "sha101" || prs[100].HeadRepo != "o/n" {
		t.Errorf("page-2 PR mangled: %+v", prs[100])
	}
}

func TestSafeArg(t *testing.T) {
	ok := []string{"aquasec/trivy:latest", "aquasec/trivy:0.58.1", "vuln,secret,misconfig",
		"HIGH,CRITICAL", "web", ".", "ghcr.io/x/y@sha256:abc"}
	for _, s := range ok {
		if !safeArg(s) {
			t.Errorf("safeArg(%q) = false, want true", s)
		}
	}
	bad := []string{"", "a b", "a;b", "a|b", "a&b", "$(x)", "a`b`", "-flag", "a\nb"}
	for _, s := range bad {
		if safeArg(s) {
			t.Errorf("safeArg(%q) = true, want false", s)
		}
	}
}

func TestLastLine(t *testing.T) {
	cases := map[string]string{
		"a\nb\nc\n":   "c",
		"only":        "only",
		"x\n\n  \n":   "x",
		"":            "",
		"  \n\t\n":    "",
		"first\nlast": "last",
	}
	for in, want := range cases {
		if got := lastLine(in); got != want {
			t.Errorf("lastLine(%q)=%q want %q", in, got, want)
		}
	}
}

func TestTruncate(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello", 3, "he…"},
		{"héllo wörld", 4, "hél…"}, // rune-aware, not byte-aware
	}
	for _, c := range cases {
		if got := truncate(c.in, c.n); got != c.want {
			t.Errorf("truncate(%q,%d)=%q want %q", c.in, c.n, got, c.want)
		}
	}
}

func TestScrubAndOneLine(t *testing.T) {
	if got := scrub("token=abc123 here abc123", "abc123"); got != "token=*** here ***" {
		t.Errorf("scrub: %q", got)
	}
	if got := scrub("nothing", ""); got != "nothing" {
		t.Errorf("scrub empty token: %q", got)
	}
	// Multiple secrets: both the raw token and its base64 header form must go.
	if got := scrub("raw tok123 and basic dG9rMTIz end", "tok123", "dG9rMTIz"); got != "raw *** and basic *** end" {
		t.Errorf("scrub multi: %q", got)
	}
	if got := oneLine("a\n  b\t c \n"); got != "a b c" {
		t.Errorf("oneLine: %q", got)
	}
}

// The store is the only state shared between concurrent ref workers; exercise
// it from several goroutines so `go test -race` covers that path.
func TestStoreConcurrent(t *testing.T) {
	s, err := LoadStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ref := Ref{Owner: "o", Name: fmt.Sprintf("n%d", i), Branch: "main"}
			for j := 0; j < 10; j++ {
				sha := fmt.Sprintf("sha%d", j)
				if err := s.MarkBranch(ref, sha, sha); err != nil {
					t.Error(err)
					return
				}
				s.Has(ref, "sha0")
				s.LastTested(ref)
				s.SetInFlight(ref.String(), sha)
				s.ClearInFlight(ref.String())
			}
		}(i)
	}
	wg.Wait()
}

func TestNeedsPull(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name    string
		present bool
		last    time.Time
		refresh time.Duration
		want    bool
	}{
		{"absent always pulls", false, now, 0, true},
		{"present, no refresh window", true, time.Time{}, 0, false},
		{"present, window not lapsed", true, now.Add(-time.Hour), 24 * time.Hour, false},
		{"present, window lapsed", true, now.Add(-25 * time.Hour), 24 * time.Hour, true},
		{"present, never pulled by us, window set", true, time.Time{}, 24 * time.Hour, true},
	}
	for _, c := range cases {
		if got := needsPull(c.present, c.last, c.refresh); got != c.want {
			t.Errorf("%s: needsPull = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestIntEnv(t *testing.T) {
	const k = "POLLCI_TEST_INT"
	os.Unsetenv(k)
	if got := intEnv(k, 7); got != 7 {
		t.Errorf("unset: got %d", got)
	}
	t.Setenv(k, "12")
	if got := intEnv(k, 7); got != 12 {
		t.Errorf("valid: got %d", got)
	}
	for _, bad := range []string{"garbage", "-3", "1.5"} {
		t.Setenv(k, bad)
		if got := intEnv(k, 7); got != 7 {
			t.Errorf("%q: got %d, want fallback 7", bad, got)
		}
	}
}

func TestLimitArgs(t *testing.T) {
	r := &Runner{cfg: &RunnerConfig{}}
	if got := r.limitArgs("", ""); len(got) != 0 {
		t.Errorf("no limits should add no flags, got %v", got)
	}
	r = &Runner{cfg: &RunnerConfig{CheckPids: "4096"}}
	want := []string{"--memory", "2g", "--cpus", "1.5", "--pids-limit", "4096"}
	got := r.limitArgs("1.5", "2g")
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("limitArgs = %v, want %v", got, want)
	}
}

func TestEffectiveLimits(t *testing.T) {
	r := &Runner{cfg: &RunnerConfig{CheckCPUs: "2", CheckMemory: "4g"}}
	// Per-check hint wins, else the global CHECK_CPUS / CHECK_MEMORY.
	if got := r.effectiveCPUs(Check{}); got != "2" {
		t.Errorf("unhinted cpus = %q, want global 2", got)
	}
	if got := r.effectiveCPUs(Check{CPUs: "0.5"}); got != "0.5" {
		t.Errorf("hinted cpus = %q, want 0.5", got)
	}
	if got := r.effectiveMemory(Check{Memory: "1g"}); got != "1g" {
		t.Errorf("hinted memory = %q, want 1g", got)
	}
	if got := r.effectiveMemory(Check{}); got != "4g" {
		t.Errorf("unhinted memory = %q, want global 4g", got)
	}
	// No hint and no global => no --cpus cap (the check shares the host CPUs).
	r2 := &Runner{cfg: &RunnerConfig{}}
	if got := r2.effectiveCPUs(Check{}); got != "" {
		t.Errorf("unhinted cpus with no global = %q, want empty (uncapped)", got)
	}
}

func TestCPUCost(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want float64
	}{{"", 1}, {"2", 2}, {"0.5", 0.5}, {"garbage", 1}, {"-1", 1}} {
		if got := (Check{CPUs: tc.in}).cpuCost(); got != tc.want {
			t.Errorf("cpuCost(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestInstanceID(t *testing.T) {
	a, b := instanceID("/var/lib/poll-ci/state.json"), instanceID("/tmp/other/state.json")
	if len(a) != 8 || len(b) != 8 {
		t.Errorf("instanceID should be 8 hex chars, got %q, %q", a, b)
	}
	if a == b {
		t.Error("distinct state files must yield distinct instance ids")
	}
	if a != instanceID("/var/lib/poll-ci/state.json") {
		t.Error("instanceID must be stable across restarts")
	}
}

// NewRunner must register both token forms as secrets — git error output could
// echo the auth header, whose value is the base64, not the raw token.
func TestRunnerSecrets(t *testing.T) {
	r := NewRunner(&RunnerConfig{Token: "tok123"}, nil, nil)
	b64 := "eC1hY2Nlc3MtdG9rZW46dG9rMTIz" // base64("x-access-token:tok123")
	if r.authB64 != b64 {
		t.Errorf("authB64 = %q, want %q", r.authB64, b64)
	}
	in := "header AUTHORIZATION: basic " + b64 + " raw tok123"
	if got := scrub(in, r.secrets...); strings.Contains(got, "tok123") || strings.Contains(got, b64) {
		t.Errorf("secrets leaked: %q", got)
	}
}

func TestWriteTar(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", ".dotfile"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := writeTar(&buf, dir, "repo"); err != nil {
		t.Fatal(err)
	}

	names := map[string]bool{}
	tr := tar.NewReader(&buf)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names[h.Name] = true
		if h.Uid != 0 || h.Gid != 0 {
			t.Errorf("entry %q: uid/gid should be 0, got %d/%d", h.Name, h.Uid, h.Gid)
		}
	}
	for _, want := range []string{"repo/file.txt", "repo/sub", "repo/sub/.dotfile"} {
		if !names[want] {
			t.Errorf("tar missing %q; got %v", want, names)
		}
	}
}

func TestReadRepoConfig(t *testing.T) {
	r := &Runner{} // readRepoConfig uses only its argument

	// Missing config is an error.
	if _, err := r.readRepoConfig(t.TempDir()); err == nil {
		t.Error("expected error when no config present")
	}

	// Both .yml and .yaml are accepted.
	for _, name := range []string{".poll-ci.yml", ".poll-ci.yaml"} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("checks:\n  - name: t\n    run: x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		rc, err := r.readRepoConfig(dir)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if len(rc.Checks) != 1 {
			t.Errorf("%s: got %d checks", name, len(rc.Checks))
		}
	}
}

func TestParseContainerState(t *testing.T) {
	cases := []struct {
		in     string
		status string
		code   int
		ok     bool
	}{
		{"exited 0\n", "exited", 0, true},
		{"exited 1", "exited", 1, true},
		{"running 0", "running", 0, true},
		{"created 0", "created", 0, true},
		{"dead 137", "dead", 137, true},
		{"", "", 0, false},
		{"exited", "", 0, false},
		{"exited x", "", 0, false},
		{"a b c", "", 0, false},
	}
	for _, c := range cases {
		status, code, err := parseContainerState(c.in)
		if c.ok && (err != nil || status != c.status || code != c.code) {
			t.Errorf("parseContainerState(%q) = (%q, %d, %v), want (%q, %d, nil)", c.in, status, code, err, c.status, c.code)
		}
		if !c.ok && err == nil {
			t.Errorf("parseContainerState(%q) expected error", c.in)
		}
	}
}

func TestRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		wait time.Duration
		ok   bool
	}{
		{"network", errors.New("dial tcp: connection refused"), 0, true},
		{"500", &apiError{Code: 500}, 0, true},
		{"502 with retry-after", &apiError{Code: 502, RetryAfter: 5 * time.Second}, 5 * time.Second, true},
		{"429", &apiError{Code: 429}, 0, true},
		{"403 secondary limit", &apiError{Code: 403, RetryAfter: 10 * time.Second}, 10 * time.Second, true},
		{"403 permissions", &apiError{Code: 403}, 0, false},
		{"401", &apiError{Code: 401}, 0, false},
		{"404", &apiError{Code: 404}, 0, false},
		{"422", &apiError{Code: 422}, 0, false},
	}
	for _, c := range cases {
		wait, ok := retryable(c.err)
		if wait != c.wait || ok != c.ok {
			t.Errorf("%s: retryable() = (%s, %v), want (%s, %v)", c.name, wait, ok, c.wait, c.ok)
		}
	}
}

func TestSetStatusAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"You have exceeded a secondary rate limit"}`))
	}))
	defer srv.Close()
	gh := NewGitHub("tok", srv.URL)
	err := gh.SetStatus(context.Background(), Ref{Owner: "o", Name: "n"}, "abc", StateSuccess, "ci/test", "ok")
	var ae *apiError
	if !errors.As(err, &ae) {
		t.Fatalf("expected *apiError, got %v", err)
	}
	if ae.Code != 403 || ae.RetryAfter != 7*time.Second {
		t.Errorf("got code=%d retryAfter=%s", ae.Code, ae.RetryAfter)
	}
	if !strings.Contains(ae.Error(), "secondary rate limit") {
		t.Errorf("error should carry the API message: %q", ae.Error())
	}
}

// TestSetStatusRetries: a transient 500 must be retried (the production bug:
// one lost POST left a context pending forever).
func TestSetStatusRetries(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	r := &Runner{cfg: &RunnerConfig{}, gh: NewGitHub("tok", srv.URL)}
	r.setStatus(context.Background(), Ref{Owner: "o", Name: "n", Branch: "main"}, "abc", StateSuccess, "ci/test", "ok")
	if hits != 2 {
		t.Errorf("expected 2 attempts (one retry), got %d", hits)
	}
}

// Permanent failures (bare 403 = token permissions) must NOT be retried.
func TestSetStatusNoRetryOnPermanent(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"Resource not accessible by personal access token"}`))
	}))
	defer srv.Close()
	r := &Runner{cfg: &RunnerConfig{}, gh: NewGitHub("tok", srv.URL)}
	r.setStatus(context.Background(), Ref{Owner: "o", Name: "n", Branch: "main"}, "abc", StateSuccess, "ci/test", "ok")
	if hits != 1 {
		t.Errorf("expected exactly 1 attempt, got %d", hits)
	}
}

// Legacy state files stored {"seen":{"k":true}}; they must load transparently.
func TestStoreLegacyMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	legacy := `{"seen":{"o/n@deadbeef":true,"o/n@cafef00d":true}}`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := LoadStore(path)
	if err != nil {
		t.Fatalf("legacy state should load: %v", err)
	}
	ref := Ref{Owner: "o", Name: "n", Branch: "main"}
	if !s.Has(ref, "deadbeef") || !s.Has(ref, "cafef00d") {
		t.Fatal("legacy seen entries lost in migration")
	}
	// A save must persist the new (timestamped) format and stay loadable.
	if err := s.Mark(ref, "0011223344"); err != nil {
		t.Fatal(err)
	}
	s2, err := LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.Has(ref, "deadbeef") || !s2.Has(ref, "0011223344") {
		t.Fatal("entries lost across migration round-trip")
	}
}

// Pruning drops entries past retention but never a branch's last-tested SHA.
func TestStorePrune(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ref := Ref{Owner: "o", Name: "n", Branch: "main"}
	old := time.Now().Add(-seenRetention - time.Hour).Unix()
	s.Seen[key(ref, "ancient1")] = old
	s.Seen[key(ref, "lasttip")] = old
	s.Last[ref.String()] = "lasttip"
	if err := s.Mark(ref, "fresh"); err != nil { // triggers save → prune
		t.Fatal(err)
	}
	s2, err := LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Has(ref, "ancient1") {
		t.Error("aged-out entry should have been pruned")
	}
	if !s2.Has(ref, "lasttip") {
		t.Error("the branch's last-tested SHA must survive pruning")
	}
	if !s2.Has(ref, "fresh") {
		t.Error("fresh entry lost")
	}
}

func TestStoreInFlight(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetInFlight("o/n@main", "abc123"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetInFlight("o/n#7", "def456"); err != nil {
		t.Fatal(err)
	}
	s2, err := LoadStore(path) // must survive a restart
	if err != nil {
		t.Fatal(err)
	}
	snap := s2.InFlightSnapshot()
	if snap["o/n@main"] != "abc123" || snap["o/n#7"] != "def456" {
		t.Fatalf("in-flight entries lost: %v", snap)
	}
	if err := s2.ClearInFlight("o/n@main"); err != nil {
		t.Fatal(err)
	}
	if err := s2.ClearInFlight("o/n@main"); err != nil { // idempotent
		t.Fatal(err)
	}
	s3, err := LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	snap = s3.InFlightSnapshot()
	if _, ok := snap["o/n@main"]; ok {
		t.Error("cleared entry persisted")
	}
	if snap["o/n#7"] != "def456" {
		t.Error("unrelated entry lost on clear")
	}
}

func TestStoreLastTested(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ref := Ref{Owner: "o", Name: "n", Branch: "main"}
	if got := s.LastTested(ref); got != "" {
		t.Fatalf("fresh store LastTested = %q", got)
	}
	if err := s.MarkBranch(ref, "tipsha", "testedsha"); err != nil {
		t.Fatal(err)
	}
	if !s.Has(ref, "tipsha") || !s.Has(ref, "testedsha") {
		t.Error("MarkBranch must mark both tip and tested SHA")
	}
	s2, err := LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.LastTested(ref); got != "testedsha" {
		t.Fatalf("LastTested = %q, want testedsha", got)
	}
	// Same repo, different branch: independent baseline.
	if got := s2.LastTested(Ref{Owner: "o", Name: "n", Branch: "dev"}); got != "" {
		t.Fatalf("other branch LastTested = %q", got)
	}
}

// ListStatuses must paginate and keep only the newest status per context.
func TestListStatuses(t *testing.T) {
	page1 := make([]map[string]string, 0, 100)
	page1 = append(page1, map[string]string{"context": "ci/a", "state": "success"}) // newest ci/a
	for i := 0; i < 99; i++ {
		page1 = append(page1, map[string]string{"context": "ci/fill" + string(rune('A'+i%26)) + string(rune('0'+i%10)), "state": "pending"})
	}
	page2 := []map[string]string{
		{"context": "ci/a", "state": "pending"}, // older duplicate — must lose
		{"context": "ci/b", "state": "pending"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("page") {
		case "1":
			json.NewEncoder(w).Encode(page1)
		case "2":
			json.NewEncoder(w).Encode(page2)
		default:
			json.NewEncoder(w).Encode([]map[string]string{})
		}
	}))
	defer srv.Close()
	gh := NewGitHub("tok", srv.URL)
	got, err := gh.ListStatuses(context.Background(), Ref{Owner: "o", Name: "n"}, "abc")
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, st := range got {
		states[st.Context] = st.State
	}
	if states["ci/a"] != "success" {
		t.Errorf("ci/a should keep its newest (success) status, got %q", states["ci/a"])
	}
	if states["ci/b"] != "pending" {
		t.Errorf("ci/b missing from page 2: %v", states["ci/b"])
	}
}

func TestCompareCommits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/compare/aaa...bbb") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ahead_by":3,"commits":[{"sha":"c1"},{"sha":"c2"},{"sha":"bbb"}]}`))
	}))
	defer srv.Close()
	gh := NewGitHub("tok", srv.URL)
	shas, err := gh.CompareCommits(context.Background(), Ref{Owner: "o", Name: "n"}, "aaa", "bbb")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"c1", "c2", "bbb"}
	if len(shas) != len(want) {
		t.Fatalf("got %v, want %v", shas, want)
	}
	for i := range want {
		if shas[i] != want[i] {
			t.Fatalf("got %v, want %v", shas, want)
		}
	}
}

func TestStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ref := Ref{Owner: "o", Name: "n", Branch: "main"}
	if s.Has(ref, "deadbeef") {
		t.Fatal("fresh store should be empty")
	}
	if err := s.Mark(ref, "deadbeef"); err != nil {
		t.Fatal(err)
	}
	// Reload from disk: the mark must have persisted.
	s2, err := LoadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.Has(ref, "deadbeef") {
		t.Fatal("mark did not persist across reload")
	}
	// Different repo, same SHA, must not collide.
	if s2.Has(Ref{Owner: "x", Name: "y"}, "deadbeef") {
		t.Fatal("SHA leaked across repos")
	}
}

func TestCheckConcurrencyConfig(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "x")
	t.Setenv("REPO", "o/n")
	t.Setenv("WORK_DIR", t.TempDir())
	ncpu := runtime.NumCPU()
	for _, tc := range []struct {
		set  string
		want int
	}{{"", ncpu}, {"0", 1}, {"-2", ncpu}, {"3", 3}} { // unset/invalid → NumCPU; "0" hits the <1 clamp
		if tc.set == "" {
			os.Unsetenv("CHECK_CONCURRENCY")
		} else {
			t.Setenv("CHECK_CONCURRENCY", tc.set)
		}
		cfg, err := LoadRunnerConfig()
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if cfg.CheckConcurrency != tc.want {
			t.Errorf("CHECK_CONCURRENCY=%q → %d, want %d", tc.set, cfg.CheckConcurrency, tc.want)
		}
	}
	os.Unsetenv("CHECK_CONCURRENCY")
	cfg, err := LoadRunnerConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CheckCPUBudget != float64(ncpu) {
		t.Errorf("default CPU budget = %v, want NumCPU %d", cfg.CheckCPUBudget, ncpu)
	}
	if cfg.CacheEnabled {
		t.Error("cache should default off (opt-in)")
	}
}

func TestSafeCachePath(t *testing.T) {
	for _, p := range []string{"/go/pkg/mod", "/root/.cache/go-build", "/a"} {
		if !safeCachePath(p) {
			t.Errorf("%q should be valid", p)
		}
	}
	for _, p := range []string{"", "/", "relative", "/a/../b", "/a b", "/a;b", "/a|b"} {
		if safeCachePath(p) {
			t.Errorf("%q should be invalid", p)
		}
	}
}

func TestParseRepoConfigCache(t *testing.T) {
	base := "image: x\nchecks:\n  - {name: a, run: b}\n"
	// Absent → auto-detect (nil).
	if rc, err := parseRepoConfig([]byte(base)); err != nil || rc.Cache != nil {
		t.Fatalf("absent cache → nil, got %+v err %v", rc.Cache, err)
	}
	// cache: false → disabled.
	if rc, err := parseRepoConfig([]byte(base + "cache: false\n")); err != nil || rc.Cache == nil || !rc.Cache.Disabled {
		t.Fatalf("cache:false → Disabled, got %+v err %v", rc.Cache, err)
	}
	// Explicit mapping.
	rc, err := parseRepoConfig([]byte(base + "cache:\n  paths: [/go/pkg/mod]\n  prime: go mod download\n"))
	if err != nil || rc.Cache == nil || !rc.Cache.Explicit || len(rc.Cache.Paths) != 1 || rc.Cache.Prime != "go mod download" {
		t.Fatalf("explicit cache parse: %+v err %v", rc.Cache, err)
	}
	// Explicit without prime, bad path, dotdot → errors.
	for _, bad := range []string{
		"cache:\n  paths: [/go/pkg/mod]\n",
		"cache:\n  paths: [relative]\n  prime: x\n",
		"cache:\n  paths: [/a/../b]\n  prime: x\n",
	} {
		if _, err := parseRepoConfig([]byte(base + bad)); err == nil {
			t.Errorf("expected error for:\n%s", bad)
		}
	}
}

func TestParseRepoConfigCheckLimits(t *testing.T) {
	base := "image: x\nchecks:\n  - {name: a, run: b, %s}\n"
	if _, err := parseRepoConfig([]byte(fmt.Sprintf(base, `cpus: "2", memory: "1g"`))); err != nil {
		t.Errorf("valid cpus/memory hints should parse: %v", err)
	}
	for _, bad := range []string{`cpus: "abc"`, `cpus: "-1"`, `memory: "1 g"`} {
		if _, err := parseRepoConfig([]byte(fmt.Sprintf(base, bad))); err == nil {
			t.Errorf("expected error for check hint %q", bad)
		}
	}
}

func TestResolveCache(t *testing.T) {
	r := &Runner{cfg: &RunnerConfig{CacheEnabled: true}}
	dir := t.TempDir()
	if p := r.resolveCache(&RepoConfig{}, dir); len(p.paths) != 0 {
		t.Errorf("no markers → no cache, got %+v", p)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := r.resolveCache(&RepoConfig{}, dir)
	if len(p.paths) == 0 || !strings.Contains(p.prime, "go mod download") {
		t.Errorf("go.mod should auto-detect the go preset, got %+v", p)
	}
	if p := (&Runner{cfg: &RunnerConfig{CacheEnabled: false}}).resolveCache(&RepoConfig{}, dir); len(p.paths) != 0 {
		t.Errorf("CACHE=false → no cache, got %+v", p)
	}
	if p := r.resolveCache(&RepoConfig{Cache: &Cache{Disabled: true}}, dir); len(p.paths) != 0 {
		t.Errorf("cache:false → no cache, got %+v", p)
	}
	pe := r.resolveCache(&RepoConfig{Cache: &Cache{Explicit: true, Paths: []string{"/x", "/x"}, Prime: "p"}}, dir)
	if len(pe.paths) != 1 || pe.prime != "p" {
		t.Errorf("explicit cache (deduped) = %+v", pe)
	}
}

func TestCacheKey(t *testing.T) {
	a := cacheKey("o/n", "main")
	if a == cacheKey("o/n", "dev") || a == cacheKey("o/other", "main") {
		t.Error("different repo/branch must yield different keys")
	}
	if a != cacheKey("o/n", "main") {
		t.Error("cacheKey must be stable")
	}
	if len(a) != 16 || strings.ContainsAny(a, "/ .") {
		t.Errorf("key should be 16 fs-safe hex chars, got %q", a)
	}
}

func TestLockfileHash(t *testing.T) {
	dir := t.TempDir()
	if lockfileHash(dir, []string{"go.sum"}) != "" {
		t.Error("no files → empty hash")
	}
	if err := os.WriteFile(filepath.Join(dir, "go.sum"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	h1 := lockfileHash(dir, []string{"go.sum"})
	if h1 == "" {
		t.Error("present file → non-empty hash")
	}
	if err := os.WriteFile(filepath.Join(dir, "go.sum"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	if lockfileHash(dir, []string{"go.sum"}) == h1 {
		t.Error("a content change must change the hash")
	}
}

func TestBlockBuffer(t *testing.T) {
	b := &blockBuffer{max: 64}
	b.Write([]byte("hello\nworld\n"))
	var sb strings.Builder
	b.flush(&sb, "ci/x — passed in 1s")
	out := sb.String()
	if !strings.Contains(out, "===== ci/x — passed in 1s =====") || !strings.Contains(out, "world") {
		t.Errorf("block missing header or body: %q", out)
	}
	var sb2 strings.Builder
	b.flush(&sb2, "again")
	if sb2.Len() != 0 {
		t.Errorf("second flush must be a no-op, got %q", sb2.String())
	}
	// Over cap: keep the tail, note the truncation.
	b2 := &blockBuffer{max: 8}
	b2.Write([]byte("0123456789ABCDEF"))
	var sb3 strings.Builder
	b2.flush(&sb3, "t")
	if !strings.Contains(sb3.String(), "truncated") || !strings.Contains(sb3.String(), "9ABCDEF") {
		t.Errorf("expected truncation note + tail, got %q", sb3.String())
	}
}

func TestRunUnits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	r := &Runner{cfg: &RunnerConfig{}, gh: NewGitHub("t", srv.URL)}
	ref := Ref{Owner: "o", Name: "n", Branch: "main"}

	var (
		mu              sync.Mutex
		curCost, maxCst float64
		curCnt, maxCnt  int
	)
	mk := func(cost float64, ok bool) unit {
		return unit{sctx: "ci/x", cost: cost, run: func(ctx context.Context) (bool, string) {
			mu.Lock()
			curCnt++
			curCost += cost
			if curCnt > maxCnt {
				maxCnt = curCnt
			}
			if curCost > maxCst {
				maxCst = curCost
			}
			mu.Unlock()
			time.Sleep(15 * time.Millisecond)
			mu.Lock()
			curCnt--
			curCost -= cost
			mu.Unlock()
			return ok, "done"
		}}
	}
	// budget 3: cost-1 units pack ≤3 at a time; the cost-3 unit runs alone.
	units := []unit{mk(1, true), mk(1, false), mk(3, true), mk(1, true), mk(1, false), mk(1, true)}
	failed, completed := r.runUnits(context.Background(), ref, "sha", units, 10, 3)
	if maxCst > 3 {
		t.Errorf("peak running cost %v exceeded budget 3", maxCst)
	}
	if failed != 2 {
		t.Errorf("failed = %d, want 2", failed)
	}
	for i, c := range completed {
		if !c {
			t.Errorf("unit %d should have completed", i)
		}
	}

	// Count cap dominates when the budget is generous.
	maxCnt = 0
	r.runUnits(context.Background(), ref, "sha", []unit{mk(1, true), mk(1, true), mk(1, true), mk(1, true)}, 2, 100)
	if maxCnt > 2 {
		t.Errorf("peak concurrency %d exceeded maxCount 2", maxCnt)
	}

	// Cancelled before launch → nothing runs, nothing completes.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, c := r.runUnits(ctx, ref, "sha", []unit{mk(1, true)}, 10, 3); c[0] {
		t.Error("a unit not launched under a cancelled ctx must not be completed")
	}
}

func TestDetectMonorepo(t *testing.T) {
	dir := t.TempDir()
	mk := func(p, c string) {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("go.mod", "module root")
	mk("svc/api/go.mod", "module api")
	mk("web/package-lock.json", "{}")
	mk("node_modules/x/package-lock.json", "{}") // vendored dep: must be skipped

	plan := detectCachePlan(dir)
	if !hasStr(plan.paths, "/go/pkg/mod") || !hasStr(plan.paths, "/root/.npm") {
		t.Errorf("paths should cover go + npm, got %+v", plan.paths)
	}
	for _, want := range []string{"go mod download", "cd 'svc/api'", "cd 'web'", "npm ci"} {
		if !strings.Contains(plan.prime, want) {
			t.Errorf("prime missing %q in: %s", want, plan.prime)
		}
	}
	if strings.Contains(plan.prime, "node_modules") {
		t.Errorf("prime must skip node_modules: %s", plan.prime)
	}
	if len(plan.markers) != 3 {
		t.Errorf("expected 3 markers (root+nested go, web npm), got %v", plan.markers)
	}
}

func hasStr(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
