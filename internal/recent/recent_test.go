package recent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAddMovesToFrontAndCaps(t *testing.T) {
	var entries []Entry
	for i := range Max + 5 {
		entries = Add(entries, Entry{FQDN: "ns", Path: string(rune('a' + i))})
	}
	if len(entries) != Max || entries[0].Path != string(rune('a'+Max+4)) {
		t.Fatalf("len %d first %q", len(entries), entries[0].Path)
	}
	again := entries[3]
	entries = Add(entries, again)
	if len(entries) != Max || !entries[0].Same(again) {
		t.Fatalf("re-add: len %d first %+v", len(entries), entries[0])
	}
	for _, e := range entries[1:] {
		if e.Same(again) {
			t.Fatal("re-added entry kept its old place too")
		}
	}
}

func TestStoreRoundTripDropsStaleSaves(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "lazybus", "recent.json")
	s := NewStore(path)
	if got, err := s.Load(); err != nil || got != nil {
		t.Fatalf("missing file: %v %v", got, err)
	}
	at := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	newer := []Entry{{FQDN: "sb.servicebus.windows.net", Name: "sb", Auth: "SAS connection string", Path: "orders", OpenAt: at}}
	stale, rev := s.Next(), s.Next()
	if err := s.Save(rev, newer); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(stale, nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil || len(got) != 1 || got[0] != newer[0] {
		t.Fatalf("load: %+v %v", got, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode: %v %v", info, err)
	}
}

func TestDefaultPath(t *testing.T) {
	env := map[string]string{"XDG_STATE_HOME": "/state"}
	if p, _ := DefaultPath(func(k string) string { return env[k] }); p != "/state/lazybus/recent.json" {
		t.Fatalf("XDG_STATE_HOME: %s", p)
	}
	env["XDG_STATE_HOME"] = "relative"
	p, err := DefaultPath(func(k string) string { return env[k] })
	if err != nil || !strings.HasSuffix(p, filepath.Join(".local", "state", "lazybus", "recent.json")) {
		t.Fatalf("fallback: %s %v", p, err)
	}
}
