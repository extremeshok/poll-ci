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
