package main

// state.go — the entire persistence layer: a JSON file recording which
// "owner/name@sha" commits have already been processed, so each commit runs
// exactly once and survives container/host restarts. No database.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// Store remembers processed commits. It is safe for concurrent use and writes
// through to disk on every change (the data is tiny).
type Store struct {
	mu   sync.Mutex
	path string
	Seen map[string]bool `json:"seen"`
}

// LoadStore reads an existing state file or starts empty.
func LoadStore(path string) (*Store, error) {
	s := &Store{path: path, Seen: map[string]bool{}}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, s); err != nil {
			return nil, err
		}
		if s.Seen == nil {
			s.Seen = map[string]bool{}
		}
	}
	return s, nil
}

// key namespaces a SHA by repo so the same commit hash in two repos can't collide.
func key(ref Ref, sha string) string { return ref.Repo() + "@" + sha }

// Has reports whether a commit has already been processed.
func (s *Store) Has(ref Ref, sha string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Seen[key(ref, sha)]
}

// Mark records commits as processed and persists immediately.
func (s *Store) Mark(ref Ref, shas ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sha := range shas {
		if sha != "" {
			s.Seen[key(ref, sha)] = true
		}
	}
	return s.save()
}

// save writes the state atomically (temp file + rename).
func (s *Store) save() error {
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
