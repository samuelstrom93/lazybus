package ui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/samuelstrom93/lazybus/internal/bus"
)

// hostileBackend serves names and message data with wide runes, line breaks
// and escape sequences.
type hostileBackend struct{}

func (hostileBackend) Namespaces(context.Context) ([]bus.Namespace, error) {
	return []bus.Namespace{{Name: "注文キュー本番ネームスペース西ヨーロッパ"}}, nil
}

func (hostileBackend) ListEntities(context.Context, bus.Namespace) ([]bus.Entity, error) {
	return []bus.Entity{{Path: "注文キュー/請求サブスクリプション\x1b[2J", Kind: bus.KindSubscription, DeadLetterCount: 1}}, nil
}

func (hostileBackend) Peek(context.Context, bus.PeekRequest) ([]bus.Message, error) {
	return []bus.Message{{
		SequenceNumber: 7,
		EnqueuedTime:   testNow,
		Body:           []byte("line one\x1b[31m red\r\nline\ttwo\u009b\x07 注文キュー"),
		Properties: []bus.Property{
			{Key: "note\nkey", Type: bus.TypeString, Value: "first\nsecond\x1b]0;pwned\x07 注文キュー注文キュー注文キュー注文キュー注文キュー注文キュー"},
		},
		MessageID:                  "id\r\n\x1b[H",
		Subject:                    "注文キュー\tsubject",
		DeadLetterReason:           "注文キュー\x1b[5mReason",
		DeadLetterErrorDescription: "boom\n\tat Foo()\n\tat Bar()",
	}}, nil
}

// TestHostileTextKeepsLayout checks that control characters in external
// text are neutralised and wide runes are truncated without breaking the
// w×h grid, on every main tab and both sizes.
func TestHostileTextKeepsLayout(t *testing.T) {
	for _, size := range [][2]int{{120, 30}, {80, 24}} {
		m := New(hostileBackend{}, Options{Location: time.UTC, Now: func() time.Time { return testNow }})
		m = run(t, m, tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		m = runCmd(t, m, m.Init())
		for _, keys := range []string{"", "2", "3", "]", "]"} {
			for _, k := range keys {
				m = run(t, m, press(string(k)))
			}
			screen := m.render()
			requireScreenSize(t, screen, size[0], size[1])
			for _, bad := range []string{"\x1b[2J", "\x1b[31m red", "\x1b]0;", "\x1b[H", "\x1b[5m", "\x07", "\r", "\t", "\u009b"} {
				if strings.Contains(screen, bad) {
					t.Fatalf("%dx%d after %q: screen contains %q", size[0], size[1], keys, bad)
				}
			}
		}
	}
}

func TestSanitize(t *testing.T) {
	got := sanitize("a\r\nb\nc\td\x1b[31me\x7f\u009bf\xff")
	if want := "a⏎b⏎c d[31mef�"; got != want {
		t.Fatalf("sanitize = %q, want %q", got, want)
	}
}
