package main

// state.go — the entire persistence layer: a JSON file recording which
// "owner/name@sha" commits have already been processed (so each commit runs
// exactly once and survives container/host restarts), each branch's last
// tested SHA, and any run that was in flight when the process died. No
// database.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// seenRetention bounds the processed-commit set: entries older than this are
// pruned on save. Each branch's last-tested SHA is exempt, so an idle repo's
// tip is never re-run just because it aged out.
const seenRetention = 180 * 24 * time.Hour

// seenMap maps "owner/name@sha" to the unix time it was recorded. Older state
// files stored booleans; UnmarshalJSON migrates them transparently.
type seenMap map[string]int64

func (m *seenMap) UnmarshalJSON(data []byte) error {
	var v map[string]int64
	if err := json.Unmarshal(data, &v); err == nil {
		*m = v
		return nil
	}
	var legacy map[string]bool
	if err := json.Unmarshal(data, &legacy); err != nil {
		return err
	}
	now := time.Now().Unix()
	*m = make(map[string]int64, len(legacy))
	for k, ok := range legacy {
		if ok {
			(*m)[k] = now
		}
	}
	return nil
}

// Store remembers processed commits. It is safe for concurrent use and writes
// through to disk on every change (the data is tiny).
type Store struct {
	mu   sync.Mutex
	path string
	Seen seenMap `json:"seen"`
	// Last maps "owner/name@branch" to the branch's most recently tested SHA —
	// the baseline for skipped-commit marking, and shielded from pruning.
	Last map[string]string `json:"last,omitempty"`
	// InFlight maps a run key ("owner/name@branch" or "owner/name#<pr>") to the
	// SHA being tested, so a run cut short by a crash/restart can be reconciled.
	InFlight map[string]string `json:"in_flight,omitempty"`
}

// LoadStore reads an existing state file or starts empty.
func LoadStore(path string) (*Store, error) {
	s := &Store{path: path}
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, s); err != nil {
			return nil, err
		}
	}
	if s.Seen == nil {
		s.Seen = seenMap{}
	}
	if s.Last == nil {
		s.Last = map[string]string{}
	}
	if s.InFlight == nil {
		s.InFlight = map[string]string{}
	}
	return s, nil
}

// key namespaces a SHA by repo so the same commit hash in two repos can't collide.
func key(ref Ref, sha string) string { return ref.Repo() + "@" + sha }

// Has reports whether a commit has already been processed.
func (s *Store) Has(ref Ref, sha string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Seen[key(ref, sha)] != 0
}

// Mark records commits as processed and persists immediately.
func (s *Store) Mark(ref Ref, shas ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()
	for _, sha := range shas {
		if sha != "" {
			s.Seen[key(ref, sha)] = now
		}
	}
	return s.save()
}

// MarkBranch records a finished branch run: the sampled tip and the tested SHA
// become seen, and the tested SHA is remembered as the branch's last result.
func (s *Store) MarkBranch(ref Ref, tip, sha string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()
	for _, x := range []string{tip, sha} {
		if x != "" {
			s.Seen[key(ref, x)] = now
		}
	}
	if sha != "" {
		s.Last[ref.String()] = sha
	}
	return s.save()
}

// LastTested returns the branch's most recently tested SHA ("" if none).
func (s *Store) LastTested(ref Ref) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Last[ref.String()]
}

// SetInFlight records that a run for key is starting on sha.
func (s *Store) SetInFlight(k, sha string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.InFlight[k] = sha
	return s.save()
}

// ClearInFlight removes a resolved run.
func (s *Store) ClearInFlight(k string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.InFlight[k]; !ok {
		return nil
	}
	delete(s.InFlight, k)
	return s.save()
}

// InFlightSnapshot returns a copy of the in-flight runs.
func (s *Store) InFlightSnapshot() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.InFlight))
	for k, v := range s.InFlight {
		out[k] = v
	}
	return out
}

// save writes the state atomically (temp file + rename), pruning seen entries
// past retention first. Callers hold s.mu.
func (s *Store) save() error {
	cutoff := time.Now().Add(-seenRetention).Unix()
	keep := map[string]bool{}
	for r, sha := range s.Last {
		// r is "owner/name@branch"; owner/name cannot contain "@", so the first
		// "@" splits off the repo even when the branch name contains one.
		if i := strings.Index(r, "@"); i > 0 {
			keep[r[:i]+"@"+sha] = true
		}
	}
	for k, t := range s.Seen {
		if t < cutoff && !keep[k] {
			delete(s.Seen, k)
		}
	}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Clean(s.path))
}
