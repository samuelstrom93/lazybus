package ui

import (
	"fmt"
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
	// Exactly failureMaxLines lines: nothing is cut.
	msg.DeadLetterErrorDescription = strings.Repeat("abcdefghijklmnopqrstuvwxyz0123456789ab ", failureMaxLines-1)
	lines = failureLines(msg, 40)
	if len(lines) != failureMaxLines || strings.Contains(ansi.Strip(strings.Join(lines, "")), "…") {
		t.Errorf("%d lines, cut: %q", len(lines), ansi.Strip(strings.Join(lines, "\n")))
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

// A long entity path is cut so "seq N" stays in the title at 80 columns.
func TestMainTitleKeepsSeq(t *testing.T) {
	m := resized(t, keysIn(t, loaded(t), "2", "j", "enter"), 80, 24)
	top := strings.SplitN(screenText(m), "\n", 2)[0]
	// The cut path fills the title: no border dash is left before ┐.
	if !strings.Contains(top, "System · order-events/") || !strings.HasSuffix(top, "… seq 2 ┐") {
		t.Errorf("80 columns: %q", top)
	}
}

// The MessageId is kept when it fits exactly.
func TestMainTitleIdentityExactFit(t *testing.T) {
	m := keysIn(t, loaded(t), "3")
	full := " · invoices/$DLQ seq 2 · inv-1002"
	w := ansi.StringWidth(full)
	if got := m.titleTail(nil, 10, 10+w); len(got) != 1 || got[0].text != full {
		t.Errorf("exact fit: %v", got)
	}
}

// While searching at 80 columns the search and the long path are both cut
// so "seq N" stays.
func TestMainTitleKeepsSeqWhileSearching(t *testing.T) {
	m := keysIn(t, loaded(t), "2", "j", "enter", "0", "/", "c", "u", "s", "t", "o", "m", "e", "r", "enter")
	top := strings.SplitN(screenText(resized(t, m, 80, 24)), "\n", 2)[0]
	if !strings.Contains(top, "System  /cu") || !strings.Contains(top, " · order-") || !strings.HasSuffix(top, "… seq 2 ┐") {
		t.Errorf("80 columns: %q", top)
	}
}

// The Type row lists every type when it fits, else the chosen one with
// its cycle keys.
func TestEditTypeRowFits(t *testing.T) {
	e := editState{kind: editProperty, typ: bus.TypeDateTime, field: fieldType}
	full := ansi.Strip(row(e.typeSegs(80), 80, false))
	if !strings.Contains(full, "String") || !strings.Contains(full, "DateTime") {
		t.Errorf("wide row: %q", full)
	}
	compact := ansi.Strip(row(e.typeSegs(48), 48, false))
	if strings.Contains(compact, "String") || !strings.Contains(compact, "Type    ← DateTime →") {
		t.Errorf("narrow row: %q", compact)
	}
}

// The edit popups at 80×24: the Type row is compact and inside the frame.
func TestEditGoldens80x24(t *testing.T) {
	for name, setup := range map[string]func() Model{
		"prop-edit-80x24": func() Model { return keysIn(t, onProperties(t, fake.New()), "j", "e") },
		"prop-add-type-80x24": func() Model {
			m := keysIn(t, typeText(t, keysIn(t, onProperties(t, fake.New()), "a"), "region"), "tab")
			return keysIn(t, m, "left") // String wraps back to DateTime
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := resized(t, setup(), 80, 24)
			if name == "prop-add-type-80x24" && m.edit.typ != bus.TypeDateTime {
				t.Fatalf("type %v, want DateTime", m.edit.typ)
			}
			screen := m.View().Content
			requireScreenSize(t, screen, 80, 24)
			requireGolden(t, name, screen)
		})
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

// A long edit error is cut so the edit popup stays above the Log rule.
func TestEditPopupLongErrorStaysInMainPane(t *testing.T) {
	m := resized(t, keysIn(t, onProperties(t, fake.New()), "j", "e"), 80, 24)
	m.edit.err = strings.Repeat("abcdefgh ", 80)
	footer, logRule := -1, -1
	for i, l := range strings.Split(screenText(m), "\n") {
		if strings.Contains(l, "└ enter save") {
			footer = i
		}
		if strings.Contains(l, "├ Log ") {
			logRule = i
		}
	}
	if footer < 0 || logRule < 0 || footer >= logRule {
		t.Errorf("popup footer row %d, Log rule row %d:\n%s", footer, logRule, screenText(m))
	}
}

// A long Pending Edit shows editPreviewLines rows in the confirm popup;
// cut further on a short terminal, the summary still counts every hidden
// line.
func TestConfirmCapsALongEdit(t *testing.T) {
	b := newConfirmBuilder(40)
	b.edits([]diffLine{{label: "Properties", text: strings.Repeat("abcdefghi ", 40), level: statusWarn}})
	all := len(wrapLines(strings.TrimSpace(strings.Repeat("abcdefghi ", 40)), b.valW))
	text := func(lines []string) string { return ansi.Strip(lines[len(lines)-1]) }
	lines := fitBlocks(b.blocks, 20)
	if len(lines) != editPreviewLines || !strings.Contains(text(lines), fmt.Sprintf("… %d more lines", all-editPreviewLines+1)) {
		t.Errorf("capped: %d lines, last %q", len(lines), text(lines))
	}
	lines = fitBlocks(b.blocks, 3)
	if len(lines) != 3 || !strings.Contains(text(lines), fmt.Sprintf("… %d more lines", all-2)) {
		t.Errorf("cut to 3: %d lines, last %q", len(lines), text(lines))
	}
}

// The confirm popup at 80×24 stays inside the main pane: the side panels
// and their right border are untouched.
func TestConfirmGolden80x24(t *testing.T) {
	screen := resized(t, keysIn(t, onOrders(t, fake.New()), "r"), 80, 24).View().Content
	requireScreenSize(t, screen, 80, 24)
	requireGolden(t, "confirm-queue-80x24", screen)
}

// At 80×24 the confirm popup fits in the main pane and still shows
// every field, the optional ones too.
func TestConfirmFitsAt80x24(t *testing.T) {
	m := resized(t, keysIn(t, onOrders(t, fake.New()), "r"), 80, 24)
	text := stripBoxes(m.View().Content)
	for _, want := range []string{"Source orders/$DLQ seq 2", "Target queue orders", "Body unchanged", "Edits none", "Markers removed", "MessageId kept ord-4002", "Scan locks up to", "y resubmit"} {
		if !strings.Contains(text, want) {
			t.Errorf("confirm at 80x24 lacks %q:\n%s", want, screenText(m))
		}
	}
	if strings.Contains(text, "more lines") {
		t.Errorf("confirm at 80x24 is cut:\n%s", screenText(m))
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
