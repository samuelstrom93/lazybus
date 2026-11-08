package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/samuelstrom93/lazybus/internal/bus"
	"github.com/samuelstrom93/lazybus/internal/bus/fake"
)

// resized returns m redrawn at w×h.
func resized(t *testing.T, m Model, w, h int) Model {
	t.Helper()
	return run(t, m, tea.WindowSizeMsg{Width: w, Height: h})
}

// mainText is the main pane content as one line of words.
func mainText(m Model, n int) string {
	lines := m.mainLines(m.layout().mainW - 2)
	return strings.Join(strings.Fields(ansi.Strip(strings.Join(lines[:min(n, len(lines))], " "))), " ")
}

// The failure strip leads every tab of a dead-lettered message, with the
// whole description at 80 columns, and is absent on an Active message.
func TestFailureStrip(t *testing.T) {
	m := resized(t, keysIn(t, loaded(t), "3"), 80, 24)
	want := "SchemaMismatch Required property 'customer.id' is missing."
	for _, tab := range []string{"Body", "Properties", "System"} {
		if got := mainText(m, 2); got != want {
			t.Errorf("%s tab starts %q, want %q", tab, got, want)
		}
		m = keysIn(t, m, "]")
	}
	m = keysIn(t, m, "tab")
	if got := mainText(m, 1); got != "{" {
		t.Errorf("Active message starts %q, want the body", got)
	}
}

func TestFailureStripCapsAnEnormousDescription(t *testing.T) {
	msg := bus.Message{DeadLetterReason: "Boom", DeadLetterErrorDescription: strings.Repeat("word ", 200)}
	lines := failureLines(msg, 40)
	if len(lines) != failureMaxLines {
		t.Fatalf("%d lines, want %d", len(lines), failureMaxLines)
	}
	last := strings.TrimRight(ansi.Strip(lines[len(lines)-1]), " ")
	if !strings.HasSuffix(last, "…") || ansi.StringWidth(lines[len(lines)-1]) != 40 {
		t.Errorf("last line %q", last)
	}
}

// The main pane title names the message; at 80 columns the MessageId
// gives way first.
func TestMainTitleIdentity(t *testing.T) {
	m := keysIn(t, loaded(t), "3")
	top := func(m Model) string { return strings.SplitN(screenText(m), "\n", 2)[0] }
	if got := top(m); !strings.Contains(got, "System · invoices/$DLQ seq 2 · inv-1002 ─") {
		t.Errorf("120 columns: %q", got)
	}
	m = resized(t, m, 80, 24)
	if got := top(m); !strings.Contains(got, "System · invoices/$DLQ seq 2 ─") || strings.Contains(got, "inv-1002") {
		t.Errorf("80 columns: %q", got)
	}
}

// The reason column keeps its full width; the MessageId gives way.
func TestMessageReasonBeforeID(t *testing.T) {
	s := screenText(loaded(t))
	if !strings.Contains(s, "MaxDeliveryCountExceeded") || !strings.Contains(s, "DownstreamTimeout") {
		t.Errorf("reasons cut:\n%s", s)
	}
}

func TestSideRows(t *testing.T) {
	for _, tc := range []struct {
		name     string
		counts   [3]int
		expanded int
		want     [3]int
	}{
		{"focused short list gives its rows to Messages", [3]int{2, 4, 20}, 0, [3]int{2, 4, 19}},
		{"focused long list grows", [3]int{2, 4, 120}, 2, [3]int{2, 4, 19}},
		{"focused Entities grows past a quarter", [3]int{2, 40, 3}, 1, [3]int{2, 20, 3}},
		{"capped Messages: spare rows pad it", [3]int{2, 4, 3}, 2, [3]int{2, 4, 19}},
		{"capped Messages: spare rows show more Entities", [3]int{2, 9, 3}, 2, [3]int{2, 9, 14}},
		{"empty lists keep a row", [3]int{0, 0, 0}, 1, [3]int{1, 1, 23}},
	} {
		got := sideRows(25, tc.counts, tc.expanded)
		if got != tc.want || got[0]+got[1]+got[2] != 25 {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Popups sit over the main pane at 120 columns, with a blank cell around
// the frame.
func TestPopupOverMainPaneWithGutter(t *testing.T) {
	m := keysIn(t, onOrders(t, fake.New()), "r")
	lines := strings.Split(screenText(m), "\n")
	top := -1
	for i, l := range lines {
		if strings.Contains(l, "┌ Resubmit") {
			top = i
		}
	}
	if top < 1 {
		t.Fatalf("no popup:\n%s", strings.Join(lines, "\n"))
	}
	row := []rune(lines[top])
	x := strings.Index(string(row), "┌ Resubmit")
	x = len([]rune(lines[top][:x]))
	if l := m.layout(); x <= l.leftW {
		t.Errorf("popup at column %d covers the side panels (width %d)", x, l.leftW)
	}
	if row[x-1] != ' ' || strings.TrimSpace(string([]rune(lines[top-1])[x-1:x+maxPopupW+1])) != "" {
		t.Errorf("no gutter around the popup:\n%s\n%s", lines[top-1], lines[top])
	}
}

func TestOptionsBar(t *testing.T) {
	bar := func(m Model) string { return ansi.Strip(m.renderOptions()) }
	m := keysIn(t, loaded(t), "3")
	if got := bar(m); !strings.Contains(got, "[ ] details") || strings.Contains(got, "ö") {
		t.Errorf("tab hint: %q", got)
	}
	if got := bar(resized(t, m, 80, 24)); strings.Contains(got, "space pick") || !strings.Contains(got, "[ ] details") {
		t.Errorf("80 columns: pick before tabs: %q", got)
	}
	m = keysIn(t, onOrders(t, fake.New()), "r")
	if got := strings.TrimSpace(bar(m)); got != "" {
		t.Errorf("confirm popup repeats its keys: %q", got)
	}
	next, _ := m.Update(press("y"))
	if got := strings.TrimSpace(bar(next.(Model))); got != "working…" {
		t.Errorf("busy: %q", got)
	}
}
