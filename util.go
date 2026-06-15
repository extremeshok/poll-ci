package main

// util.go — small, pure helpers used by the runner: streaming a directory as a
// tar, bounding captured output, and tidying strings for status descriptions.

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// writeTar streams `src`'s contents into w as a tar, each entry under `prefix/`.
// Built in Go (not the shell) so no host-specific xattrs sneak in and break
// extraction inside the Linux check container.
func writeTar(w io.Writer, src, prefix string) error {
	tw := tar.NewWriter(w)
	defer tw.Close()
	return filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		mode := fi.Mode()
		link := ""
		if mode&os.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		} else if !mode.IsRegular() && !fi.IsDir() {
			return nil // skip sockets, devices, fifos
		}
		hdr, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return err
		}
		hdr.Name = filepath.Join(prefix, rel)
		hdr.Uid, hdr.Gid = 0, 0
		hdr.Uname, hdr.Gname = "", ""
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !mode.IsRegular() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
}

// tailBuffer keeps only the last `max` bytes written — bounds memory for chatty checks.
type tailBuffer struct {
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.buf) }

// instanceID derives a stable per-instance tag from the state file path, so
// containers can be labeled and swept at startup without ever touching a
// co-located poll-ci instance's containers on a shared Docker daemon.
func instanceID(stateFile string) string {
	sum := sha256.Sum256([]byte(stateFile))
	return hex.EncodeToString(sum[:4])
}

// short abbreviates a SHA to 7 characters for logging.
func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// oneLine collapses all runs of whitespace (including newlines) to single spaces.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// lastLine returns the last non-empty line of s — the most useful summary line.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n \t"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}

// scrub replaces each secret (the raw token and its base64 header form) with
// *** so none ever lands in a log line or status description.
func scrub(s string, secrets ...string) string {
	for _, sec := range secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, "***")
		}
	}
	return s
}

// cacheKey is a stable, filesystem-safe token for a repo+branch's golden cache,
// namespacing it so two repos (or two branches) never share — or poison — a
// cache. Branch names can contain slashes and other awkward characters, so the
// whole key is hashed rather than used as a path component.
func cacheKey(repo, branch string) string {
	sum := sha256.Sum256([]byte(repo + "\x00" + branch))
	return hex.EncodeToString(sum[:8])
}

// cachePathDir is the host directory holding the golden copy of one container
// cache path, under a repo+branch's cache root.
func cachePathDir(root, containerPath string) string {
	sum := sha256.Sum256([]byte(containerPath))
	return filepath.Join(root, hex.EncodeToString(sum[:8]))
}

// lockfileHash hashes the contents of the given repo-relative files (those that
// exist) into a stable token, so a golden cache can be re-primed exactly when
// dependencies change. Returns "" when none of the files exist.
func lockfileHash(dir string, files []string) string {
	h := sha256.New()
	any := false
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			continue
		}
		any = true
		h.Write([]byte(f))
		h.Write([]byte{0})
		h.Write(data)
		h.Write([]byte{0})
	}
	if !any {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// blockBuffer captures one check's output (bounded) so parallel checks can each
// be flushed as a single labeled block instead of interleaving their streams
// live. Past max bytes it keeps the tail (where failures print) and notes the
// truncation. Safe for concurrent writes (docker stdout+stderr stream into it
// from separate goroutines).
type blockBuffer struct {
	mu        sync.Mutex
	max       int
	buf       []byte
	truncated bool
	flushed   bool
}

func (b *blockBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.max {
		b.buf = b.buf[len(b.buf)-b.max:]
		b.truncated = true
	}
	return len(p), nil
}

// flush writes the captured output to w as one labeled block (a `===== label
// =====` header, a truncation note if it overflowed, then the body). It is a
// no-op after the first call, so a defer-flush on cancellation and an explicit
// flush on completion can both fire without double-printing.
func (b *blockBuffer) flush(w io.Writer, label string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.flushed {
		return
	}
	b.flushed = true
	var sb strings.Builder
	sb.WriteString("===== " + label + " =====\n")
	if b.truncated {
		sb.WriteString("…(output head truncated; see docker logs for the full log)\n")
	}
	sb.Write(b.buf)
	if n := len(b.buf); n > 0 && b.buf[n-1] != '\n' {
		sb.WriteByte('\n')
	}
	io.WriteString(w, sb.String())
}

// truncate shortens s to at most n runes (GitHub counts characters), adding an
// ellipsis when it cuts.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}
