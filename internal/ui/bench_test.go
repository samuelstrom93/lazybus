package ui

import (
	"runtime"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/samuelstrom93/lazybus/internal/bus"
	"github.com/samuelstrom93/lazybus/internal/bus/fake"
)

// Release benchmarks. Their names are stable: tools/benchreport turns them
// into metric names (ViewLargeList/ns_per_op, ...) that are compared across
// releases, so renaming one drops its history. See docs/releasing.md.
//
// Everything runs in-process on the fake bus with the fixed clock of
// testOptions, in a 120×30 window. There is no process-level startup
// benchmark: the real program needs a TTY and go.mod has no pty library.

// largeDLQ is the dead-letter count of the large-list benchmarks.
const largeDLQ = 10000

// loadAll opens the sb-prod-weu orders DLQ and pages to the end: G moves to
// the last loaded row, which loads the next page; only an empty page ends
// paging.
func loadAll(b *testing.B, be *fake.Backend) Model {
	m := onOrders(b, be)
	for i := 0; m.messages.more; i++ {
		if i > largeDLQ/bus.PageSize+2 {
			b.Fatalf("still paging after %d pages: %d rows", i, len(m.messages.items))
		}
		m = keysIn(b, m, "G")
	}
	if len(m.messages.items) != largeDLQ {
		b.Fatalf("loaded %d rows, want %d", len(m.messages.items), largeDLQ)
	}
	return m
}

// loadedLarge returns a model with the whole large DLQ loaded and the
// cursor in the middle of the list.
func loadedLarge(b *testing.B) Model {
	m := loadAll(b, fake.New(fake.WithDeadLetters("sb-prod-weu", "orders", largeDLQ)))
	for m.messages.cursor > largeDLQ/2 {
		m = run(b, m, tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	}
	if m.messages.more || m.messages.cursor <= 0 {
		b.Fatalf("setup: more %v, cursor %d", m.messages.more, m.messages.cursor)
	}
	return m
}

// BenchmarkStartupFirstFrame: a fresh backend and model, the window size,
// the startup cascade (namespaces, entities, first peek) and the first View.
func BenchmarkStartupFirstFrame(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		m := New(fake.New(), testOptions())
		m = run(b, m, tea.WindowSizeMsg{Width: 120, Height: 30})
		m = runCmd(b, m, m.Init())
		_ = m.View()
	}
}

// BenchmarkLoadLargeList: start, open a 10k-message DLQ and page through
// all of it. Also reports heap-bytes: the live heap one fully loaded model
// retains, measured after the timed loop.
func BenchmarkLoadLargeList(b *testing.B) {
	be := fake.New(fake.WithDeadLetters("sb-prod-weu", "orders", largeDLQ))
	b.ReportAllocs()
	for b.Loop() {
		_ = loadAll(b, be)
	}

	b.StopTimer()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	m := loadAll(b, be)
	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(m)
	b.ReportMetric(float64(int64(after.HeapAlloc)-int64(before.HeapAlloc)), "heap-bytes")
}

// BenchmarkViewLargeList: one frame with 10k rows loaded, cursor mid-list.
func BenchmarkViewLargeList(b *testing.B) {
	m := loadedLarge(b)
	b.ReportAllocs()
	for b.Loop() {
		_ = m.View()
	}
}

// BenchmarkNavStepLargeList: one cursor step (j, then k) and the frame
// after it, with 10k rows loaded. Paging is over, so no step loads.
func BenchmarkNavStepLargeList(b *testing.B) {
	m := loadedLarge(b)
	keys := [2]tea.KeyPressMsg{press("j"), press("k")}
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		m = run(b, m, keys[i&1])
		_ = m.View()
		i++
	}
	b.StopTimer()
	if len(m.messages.items) != largeDLQ || m.messages.more || m.messages.loading {
		b.Fatalf("after nav: %d rows, more %v, loading %v", len(m.messages.items), m.messages.more, m.messages.loading)
	}
}

// BenchmarkRepairFlow: r (plan, confirm popup), y (resubmit), and the
// frame after it, on a fresh backend each time.
func BenchmarkRepairFlow(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		b.StopTimer()
		be := fake.New()
		m := onOrders(b, be)
		before := len(be.Messages("sb-prod-weu", "orders", bus.KindQueue, bus.DeadLetter))
		b.StartTimer()

		m = keysIn(b, m, "r", "y")
		_ = m.View()

		b.StopTimer()
		if got := len(be.Messages("sb-prod-weu", "orders", bus.KindQueue, bus.DeadLetter)); got != before-1 {
			b.Fatalf("broker DLQ has %d after repair, want %d", got, before-1)
		}
		b.StartTimer()
	}
}
