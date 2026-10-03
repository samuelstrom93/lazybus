package ui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/samuelstrom93/lazybus/internal/bus"
	"github.com/samuelstrom93/lazybus/internal/bus/fake"
)

// keys presses each key in turn, running the commands inline. Tokens are
// single characters except "enter", "esc" and "tab".
func keysIn(t *testing.T, m Model, keys ...string) Model {
	t.Helper()
	for _, k := range keys {
		m = run(t, m, press(k))
	}
	return m
}

// startWith returns a 120×30 model on be with the startup cascade applied.
func startWith(t *testing.T, be bus.Backend) Model {
	t.Helper()
	m := New(be, testOptions())
	m = run(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
	return runCmd(t, m, m.Init())
}

// TestStateGoldens renders loading, error, paging and body states by
// driving the model synchronously, so in-flight loads can be held back.
// Same goldens and -update flag as TestGoldens; "initial" is rendered by
// both and must match.
func TestStateGoldens(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T) Model
	}{
		{"initial", func(t *testing.T) Model { return loaded(t) }},
		{"loading", func(t *testing.T) Model {
			// Hold the DLQ peek of the startup cascade.
			m := New(fake.New(), testOptions())
			m = run(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
			m, list := initHeld(t, m)
			next, peek := m.Update(list())
			if peek == nil {
				t.Fatal("no peek after entities loaded")
			}
			return next.(Model)
		}},
		{"error-entities", func(t *testing.T) Model {
			be := fake.New()
			// Raw, hostile error text: it must arrive sanitized.
			be.SetFault(fake.OpEntities, fake.Fault{Err: errors.New("dial tcp 10.1.2.3:443: connect: connection refused\r\n\x1b[31mretry 3/3\x07")})
			return startWith(t, be)
		}},
		{"error-peek", func(t *testing.T) Model {
			be := fake.New()
			be.SetFault(fake.OpPeek, fake.Fault{Err: &bus.Error{
				Kind: bus.ErrNotFound, Op: "peek invoices/$DLQ",
				Msg: "amqp:not-found: The messaging entity 'sb://sb-prod-weu.servicebus.windows.net/invoices/$deadletterqueue' could not be found. To know more visit https://aka.ms/sbResourceMgrExceptions",
			}})
			return startWith(t, be)
		}},
		{"unknown-counts", func(t *testing.T) Model {
			return keysIn(t, startWith(t, fake.New(fake.WithUnknownCounts())), "2")
		}},
		{"paging", func(t *testing.T) Model {
			// orders gets 120 DLQ messages; G puts the cursor on row 50,
			// which loads the next page; j steps onto its first row.
			m := startWith(t, fake.New(fake.WithDeadLetters("sb-prod-weu", "orders", 120)))
			return keysIn(t, m, "2", "j", "j", "j", "enter", "G", "j")
		}},
		{"tab-body-raw", func(t *testing.T) Model {
			// The last row of invoices: the next-page load (empty) runs
			// synchronously here, not racing a teatest capture.
			return keysIn(t, loaded(t), "3", "j", "j")
		}},
		{"body-long-line", func(t *testing.T) Model {
			// order-events/billing message 5: one unbroken non-JSON line.
			return keysIn(t, loaded(t), "2", "j", "enter", "j", "j", "j", "j")
		}},
		// S1b navigation.
		{"discovery-loading", func(t *testing.T) Model {
			// Subscriptions are in; their namespace listings are held.
			m := New(fake.New(fake.WithDiscovery()), testOptions())
			m = run(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
			m, _ = initHeld(t, m)
			return keysIn(t, m, "1", "j", "j")
		}},
		{"discovery-error", func(t *testing.T) Model {
			be := fake.New(fake.WithDiscovery(), fake.WithDiscoveryError("contoso-broken",
				errors.New("GET https://management.azure.com/subscriptions/00000000-0000-4000-8000-000000000003/providers/Microsoft.ServiceBus/namespaces: 403 AuthorizationFailed")))
			return keysIn(t, startWith(t, be), "1", "j", "j")
		}},
		{"namespace-details", func(t *testing.T) Model {
			return keysIn(t, startWith(t, fake.New(fake.WithDiscovery())), "1", "G")
		}},
		{"tab-active", func(t *testing.T) Model { return keysIn(t, loaded(t), "3", "tab", "j") }},
		{"active-sessionful", func(t *testing.T) Model {
			return keysIn(t, startWith(t, fake.New(fake.WithSessions("sb-prod-weu", "invoices"))), "3", "tab")
		}},
		{"filter-namespaces", func(t *testing.T) Model {
			// Still typing: the title shows the input cursor.
			return keysIn(t, startWith(t, fake.New(fake.WithDiscovery())), "1", "/", "d", "e", "v")
		}},
		{"filter-entities", func(t *testing.T) Model { return keysIn(t, loaded(t), "2", "/", "o", "r", "d", "enter") }},
		{"filter-messages", func(t *testing.T) Model { return keysIn(t, loaded(t), "3", "/", "d", "o", "w", "n", "enter") }},
		{"jump", func(t *testing.T) Model { return keysIn(t, loaded(t), "3", ":", "o", "r", "d") }},
		{"sort-dlq", func(t *testing.T) Model { return keysIn(t, loaded(t), "2", "s") }},
		{"copy-status", func(t *testing.T) Model { return keysIn(t, loaded(t), "3", "j", "Y") }},
		{"refresh", func(t *testing.T) Model {
			// Another client resubmits the first message; R shows it gone,
			// the cursor keeps its row index.
			be := fake.New()
			m := keysIn(t, startWith(t, be), "3", "j")
			removeOutOfBand(t, be, m, 2)
			return keysIn(t, m, "R")
		}},
	}
	for _, tc := range cases {
		name := tc.name + "-120x30"
		t.Run(name, func(t *testing.T) {
			screen := tc.setup(t).View().Content
			requireScreenSize(t, screen, 120, 30)
			requireGolden(t, name, screen)
		})
	}
}

// initHeld applies the results of Init's calls (configured namespaces,
// subscriptions) and returns the commands they start, unrun, as one batch.
func initHeld(t *testing.T, m Model) (Model, tea.Cmd) {
	t.Helper()
	batch, ok := m.Init()().(tea.BatchMsg)
	if !ok {
		t.Fatal("Init is not a batch")
	}
	var cmds []tea.Cmd
	for _, c := range batch {
		next, cmd := m.Update(c())
		m = next.(Model)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	return m, tea.Sequence(cmds...)
}

func TestPagingAppendsAndStops(t *testing.T) {
	m := startWith(t, fake.New(fake.WithDeadLetters("sb-prod-weu", "orders", 120)))
	m = keysIn(t, m, "2", "j", "j", "j", "enter")
	if len(m.messages.items) != 50 || !m.messages.more {
		t.Fatalf("first page: %d items, more %v", len(m.messages.items), m.messages.more)
	}
	m = keysIn(t, m, "G")
	if len(m.messages.items) != 100 || m.messages.cursor != 49 {
		t.Fatalf("after G: %d items, cursor %d", len(m.messages.items), m.messages.cursor)
	}
	if got := m.messages.items[50].SequenceNumber; got != 52 {
		t.Fatalf("page 2 starts at seq %d, want 52", got)
	}
	m = keysIn(t, m, "G") // row 100 → last 20: a short page does not end paging
	if len(m.messages.items) != 120 || !m.messages.more {
		t.Fatalf("short page: %d items, more %v", len(m.messages.items), m.messages.more)
	}
	m = keysIn(t, m, "G") // row 120 → empty page: no more loads
	if len(m.messages.items) != 120 || m.messages.more {
		t.Fatalf("end: %d items, more %v", len(m.messages.items), m.messages.more)
	}
	if _, cmd := m.Update(press("j")); cmd != nil {
		t.Fatal("j on the last row loaded again after an empty page")
	}
}

func TestFailedPageKeepsRowsAndRetries(t *testing.T) {
	be := fake.New(fake.WithDeadLetters("sb-prod-weu", "orders", 120))
	m := keysIn(t, startWith(t, be), "2", "j", "j", "j", "enter")
	be.SetFault(fake.OpPeek, fake.Fault{Err: errors.New("link detached")})
	m = keysIn(t, m, "G")
	if len(m.messages.items) != 50 || m.messages.err == nil {
		t.Fatalf("failed page: %d items, err %v", len(m.messages.items), m.messages.err)
	}
	if _, ok := m.selectedMessage(); !ok {
		t.Fatal("selection lost after a failed page")
	}
	if !strings.Contains(ansi.Strip(m.render()), "DLQ│Active error") {
		t.Fatal("title does not show the failed page")
	}
	be.SetFault(fake.OpPeek, fake.Fault{})
	m = keysIn(t, m, "j")
	if len(m.messages.items) != 100 || m.messages.err != nil {
		t.Fatalf("retry: %d items, err %v", len(m.messages.items), m.messages.err)
	}
}

// TestSupersededLoadIsCanceled opens one entity while another's peek hangs:
// the first call's context is canceled and its late response dropped.
func TestSupersededLoadIsCanceled(t *testing.T) {
	be := fake.New()
	m := startWith(t, be)
	be.SetFault(fake.OpPeek, fake.Fault{Block: true})
	m = run(t, m, press("2"))
	next, first := m.Update(press("enter")) // invoices: blocks
	m = keysIn(t, next.(Model), "2", "j")
	next, second := m.Update(press("enter")) // order-events/billing
	m = next.(Model)

	done := make(chan tea.Msg, 1)
	go func() { done <- first() }()
	var stale tea.Msg
	select {
	case stale = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("superseded peek was not canceled")
	}
	if err := stale.(messagesLoadedMsg).err; !errors.Is(err, context.Canceled) {
		t.Fatalf("superseded peek err = %v", err)
	}
	logs := len(m.log)
	m = run(t, m, stale)
	if len(m.log) != logs || !m.messages.loading {
		t.Fatal("stale response was not dropped")
	}

	be.SetFault(fake.OpPeek, fake.Fault{})
	m = runCmd(t, m, second)
	if m.openEntity.Path != "order-events/billing" || len(m.messages.items) != 12 {
		t.Fatalf("after second load: %s, %d messages", m.openEntity.Path, len(m.messages.items))
	}
}

func TestCallTimeout(t *testing.T) {
	be := fake.New()
	be.SetFault(fake.OpEntities, fake.Fault{Delay: time.Minute})
	opts := testOptions()
	opts.CallTimeout = 20 * time.Millisecond
	m := New(be, opts)
	m = run(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
	m = runCmd(t, m, m.Init())
	if !errors.Is(m.entities.err, context.DeadlineExceeded) || m.entities.loading {
		t.Fatalf("entities err %v loading %v", m.entities.err, m.entities.loading)
	}
}

type panicBackend struct{ fake.Backend }

func (*panicBackend) ListEntities(context.Context, bus.Namespace) ([]bus.Entity, error) {
	panic("boom")
}

func TestBackendPanicBecomesError(t *testing.T) {
	m := startWith(t, &panicBackend{Backend: *fake.New()})
	if m.entities.err == nil || !strings.Contains(m.entities.err.Error(), "internal error: boom") {
		t.Fatalf("entities err = %v", m.entities.err)
	}
}

// TestBodyLinesCached checks that moving in the main pane does not
// re-format the body.
func TestBodyLinesCached(t *testing.T) {
	m := keysIn(t, loaded(t), "3", "j")
	w := m.layout().mainW - 2
	first := m.mainLines(w)
	m = keysIn(t, m, "0", "j")
	if again := m.mainLines(w); &first[0] != &again[0] {
		t.Fatal("body lines re-rendered for the same message, tab and width")
	}
	m = keysIn(t, m, "]")
	if props := m.mainLines(w); &props[0] == &first[0] {
		t.Fatal("cache returned body lines on the Properties tab")
	}
}

// TestLinesCacheClearedOnReload reopens the entity: the new list's first
// message has the same key (sequence number, tab, width) as the cached
// one, but its lines must be rendered from the new list.
func TestLinesCacheClearedOnReload(t *testing.T) {
	m := loaded(t)
	w := m.layout().mainW - 2
	before := m.mainLines(w)
	m = keysIn(t, m, "2", "enter")
	if after := m.mainLines(w); &after[0] == &before[0] {
		t.Fatal("cache kept after the message list was replaced")
	}
}

func TestPropertyTimeInLocation(t *testing.T) {
	at := time.Date(2026, 9, 18, 22, 15, 0, 0, time.UTC)
	p := bus.Property{Key: "createdAt", Type: bus.TypeDateTime, Value: at}
	if got, want := propertyValue(p, time.FixedZone("CEST", 2*3600)), "2026-09-19T00:15:00+02:00"; got != want {
		t.Fatalf("propertyValue = %q, want %q", got, want)
	}
}
