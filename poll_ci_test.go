package main

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

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
