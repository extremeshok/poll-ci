package main

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	if got := oneLine("a\n  b\t c \n"); got != "a b c" {
		t.Errorf("oneLine: %q", got)
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
