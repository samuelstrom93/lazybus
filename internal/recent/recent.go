// Package recent keeps the entities lazybus opened last, on disk, so the
// ctrl-r popup can offer them again. An entry names a namespace by FQDN and
// says how it was authenticated (a label, never a secret): a connection
// string or key is never written.
package recent

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// Max is the most entries kept.
const Max = 20

// Entry is one opened entity.
type Entry struct {
	FQDN   string    `json:"fqdn"`
	Name   string    `json:"name"`
	Auth   string    `json:"auth"` // bus.Namespace.Auth: a label such as "SAS connection string"
	Path   string    `json:"path"` // queue, or topic/subscription
	Sub    bool      `json:"subscription,omitempty"`
	OpenAt time.Time `json:"openedAt"`
}

// Same reports whether e and o name the same entity.
func (e Entry) Same(o Entry) bool {
	return e.FQDN == o.FQDN && e.Path == o.Path && e.Sub == o.Sub
}

// Add returns entries with e first, an older entry for the same entity
// dropped, cut to Max. entries is not changed.
func Add(entries []Entry, e Entry) []Entry {
	out := make([]Entry, 0, min(len(entries)+1, Max))
	out = append(out, e)
	for _, o := range entries {
		if len(out) == Max {
			break
		}
		if !o.Same(e) {
			out = append(out, o)
		}
	}
	return out
}

// DefaultPath is $XDG_STATE_HOME/lazybus/recent.json, or
// ~/.local/state/lazybus/recent.json when XDG_STATE_HOME is unset.
func DefaultPath(getenv func(string) string) (string, error) {
	dir := getenv("XDG_STATE_HOME")
	if !filepath.IsAbs(dir) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "lazybus", "recent.json"), nil
}

// Store reads and writes the entries file. Saves may run concurrently
// (each is a tea.Cmd); a save older than the newest written is dropped.
type Store struct {
	path string
	rev  atomic.Int64 // the last revision handed out by Next
	mu   sync.Mutex
	last int64 // the newest revision written
}

// Next numbers a snapshot to save: call it in the order the snapshots are
// taken.
func (s *Store) Next() int64 { return s.rev.Add(1) }

// NewStore returns a store for the file at path.
func NewStore(path string) *Store { return &Store{path: path} }

// Path is the file the store uses.
func (s *Store) Path() string { return s.path }

// Load reads the entries; a missing file is no entries.
func (s *Store) Load() ([]Entry, error) {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var entries []Entry
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, err
	}
	if len(entries) > Max {
		entries = entries[:Max]
	}
	return entries, nil
}

// Save writes entries as revision rev (from Next), atomically (temp file
// and rename), readable by the user only. A rev not newer than the last
// saved is dropped.
func (s *Store) Save(rev int64, entries []Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rev <= s.last {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".recent-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	// On disk before the rename, so a crash can't leave an empty file.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		return err
	}
	s.last = rev
	return nil
}
