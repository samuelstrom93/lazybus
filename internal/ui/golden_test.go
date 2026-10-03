package ui

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/aymanbagabas/go-udiff"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/teatest/v2"

	"github.com/samuelstrom93/lazybus/internal/bus/fake"
)

// testNow is the fixed clock for UI tests: the demo data's day, in UTC.
var testNow = fake.Epoch.Add(16 * time.Hour)

func testOptions() Options {
	return Options{Location: time.UTC, Now: func() time.Time { return testNow }}
}

// TestGoldens drives the real program with teatest and compares the final
// screen with internal/ui/testdata/<name>.golden. Regenerate with
// `go test ./internal/ui -update`.
func TestGoldens(t *testing.T) {
	cases := []struct {
		name string
		w, h int
		opts func(*Options)
		keys string
	}{
		{name: "initial", w: 120, h: 30},
		{name: "focus-namespaces", w: 120, h: 30, keys: "21"},
		{name: "focus-entities", w: 120, h: 30, keys: "2"},
		{name: "focus-messages", w: 120, h: 30, keys: "3"},
		{name: "focus-main", w: 120, h: 30, keys: "0"},
		{name: "tab-body", w: 120, h: 30, keys: "3j"},
		{name: "tab-body-raw", w: 120, h: 30, keys: "3jj"},
		{name: "tab-properties", w: 120, h: 30, keys: "3]"},
		{name: "tab-system", w: 120, h: 30, keys: "3]]"},
		{name: "help-open", w: 120, h: 30, keys: "3?"},
		{name: "help-filtered", w: 120, h: 30, keys: "3?tab"},
		{name: "read-only", w: 120, h: 30, opts: func(o *Options) { o.ReadOnly = true }},
		{name: "initial", w: 80, h: 24},
		{name: "focus-messages", w: 80, h: 24, keys: "3]"},
	}
	for _, tc := range cases {
		name := fmt.Sprintf("%s-%dx%d", tc.name, tc.w, tc.h)
		t.Run(name, func(t *testing.T) {
			opts := testOptions()
			if tc.opts != nil {
				tc.opts(&opts)
			}
			tm := teatest.NewTestModel(t, New(fake.New(), opts),
				teatest.WithInitialTermSize(tc.w, tc.h),
				teatest.WithProgramOptions(tea.WithColorProfile(colorprofile.ANSI)),
			)
			waitLoaded(t, tm)
			tm.Type(tc.keys)
			if err := tm.Quit(); err != nil {
				t.Fatal(err)
			}
			final := tm.FinalModel(t, teatest.WithFinalTimeout(5*time.Second)).(Model)
			screen := final.View().Content
			requireScreenSize(t, screen, tc.w, tc.h)
			requireGolden(t, name, screen)
		})
	}
}

// waitLoaded waits until the startup cascade (namespaces → entities → DLQ
// peek) has rendered. The terminal stream skips unchanged cells, so the
// check ignores escapes and spaces.
func waitLoaded(t *testing.T, tm *teatest.TestModel) {
	t.Helper()
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		s := strings.Join(strings.Fields(ansi.Strip(string(b))), "")
		return strings.Contains(s, "peekinvoices/$DLQ→3")
	}, teatest.WithDuration(5*time.Second), teatest.WithCheckInterval(10*time.Millisecond))
}

// requireScreenSize checks the screen is exactly w×h cells: nothing wraps,
// nothing is short.
func requireScreenSize(t *testing.T, screen string, w, h int) {
	t.Helper()
	lines := strings.Split(screen, "\n")
	if len(lines) != h {
		t.Errorf("screen has %d lines, want %d", len(lines), h)
	}
	for i, l := range lines {
		if got := ansi.StringWidth(l); got != w {
			t.Errorf("line %d is %d cells wide, want %d: %q", i+1, got, w, ansi.Strip(l))
		}
	}
}

// requireGolden compares out with testdata/<name>.golden, rewriting it when
// -update is set (flag registered by x/exp/golden via teatest).
func requireGolden(t *testing.T, name, out string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if f := flag.Lookup("update"); f != nil && f.Value.String() == "true" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run `go test ./internal/ui -update` to create it)", err)
	}
	if string(want) != out {
		diff := udiff.Unified(path, "got", ansi.Strip(string(want)), ansi.Strip(out))
		if diff == "" {
			diff = "(text equal, styling differs)"
		}
		t.Errorf("screen differs from %s:\n%s", path, diff)
	}
}
