package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/samuelstrom93/lazybus/internal/bus/fake"
)

// loaded returns a 120×30 model with the startup cascade applied, running
// the async commands inline.
func loaded(t *testing.T) Model {
	t.Helper()
	m := New(fake.New(), testOptions())
	m = run(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
	return runCmd(t, m, m.Init())
}

func run(t testing.TB, m Model, msg tea.Msg) Model {
	t.Helper()
	next, cmd := m.Update(msg)
	return runCmd(t, next.(Model), cmd)
}

// runCmd runs cmd and everything it leads to inline. A batch runs depth
// first, in order: the broker call before the spinner tick, so the tick
// finds the busy popup closed and ends.
func runCmd(t testing.TB, m Model, cmd tea.Cmd) Model {
	t.Helper()
	for cmd != nil {
		msg := cmd()
		switch msg := msg.(type) {
		case tea.QuitMsg:
			t.Fatal("unexpected quit")
		case tea.BatchMsg:
			for _, c := range msg {
				m = runCmd(t, m, c)
			}
			return m
		}
		var next tea.Model
		next, cmd = m.Update(msg)
		m = next.(Model)
	}
	return m
}

func press(s string) tea.KeyPressMsg {
	switch s {
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "shift+tab":
		return tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	case "backspace":
		return tea.KeyPressMsg{Code: tea.KeyBackspace}
	case "left":
		return tea.KeyPressMsg{Code: tea.KeyLeft}
	case "right":
		return tea.KeyPressMsg{Code: tea.KeyRight}
	case "ctrl+u":
		return tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl}
	}
	r := []rune(s)[0]
	return tea.KeyPressMsg{Code: r, Text: s}
}

func isQuit(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

func TestQuitOnlyFromRoot(t *testing.T) {
	m := loaded(t)
	if _, cmd := m.Update(press("q")); !isQuit(cmd) {
		t.Fatal("q at root did not quit")
	}

	m = run(t, m, press("?"))
	next, cmd := m.Update(press("q"))
	if isQuit(cmd) {
		t.Fatal("q with the ? menu open quit")
	}
	if got := next.(Model).help.filter; got != "q" {
		t.Fatalf("filter = %q, want q", got)
	}
}

func TestPopupOwnsKeys(t *testing.T) {
	m := loaded(t)
	m = run(t, m, press("?"))
	for _, k := range []string{"2", "]", "j"} {
		m = run(t, m, press(k))
	}
	if m.stack.Top() != CtxHelp || m.stack.Root() != CtxNamespaces {
		t.Fatalf("keys leaked past the popup: top %v root %v", m.stack.Top(), m.stack.Root())
	}
	if m.tab != tabBody || m.help.filter != "2]j" {
		t.Fatalf("tab %v filter %q", m.tab, m.help.filter)
	}

	m = run(t, m, press("esc"))
	if m.stack.Top() != CtxHelp || m.help.filter != "" {
		t.Fatalf("first esc should clear the filter: top %v filter %q", m.stack.Top(), m.help.filter)
	}
	m = run(t, m, press("esc"))
	if !m.stack.AtRoot() || m.stack.Top() != CtxNamespaces {
		t.Fatalf("esc on an empty filter did not pop the popup: top %v", m.stack.Top())
	}
}

func TestSwedishTabKeys(t *testing.T) {
	m := loaded(t)
	m = run(t, m, press("ä"))
	if m.tab != tabProperties {
		t.Fatalf("ä did not switch to the next tab: %v", m.tab)
	}
	m = run(t, m, press("ö"))
	if m.tab != tabBody {
		t.Fatalf("ö did not switch to the previous tab: %v", m.tab)
	}

	m = keysIn(t, m, "2", "/", "ö")
	if got := m.entities.filter; got != "ö" || m.tab != tabBody {
		t.Fatalf("ö typed into the filter: filter %q tab %v", got, m.tab)
	}
}

func TestFocusAndTabKeys(t *testing.T) {
	m := loaded(t)
	m = run(t, m, press("]"))
	if m.tab != tabProperties || m.stack.Top() != CtxNamespaces {
		t.Fatalf("] from Namespaces: tab %v focus %v", m.tab, m.stack.Top())
	}
	m = run(t, m, press("["))
	m = run(t, m, press("["))
	if m.tab != tabSystem {
		t.Fatalf("[ did not wrap to System: %v", m.tab)
	}

	m = run(t, m, press("h"))
	if m.stack.Top() != CtxNamespaces {
		t.Fatalf("h on the first panel moved to %v", m.stack.Top())
	}
	for _, k := range []string{"l", "l", "l", "l"} {
		m = run(t, m, press(k))
	}
	if m.stack.Top() != CtxMain || m.lastSide != CtxMessages {
		t.Fatalf("l past the last panel: focus %v lastSide %v", m.stack.Top(), m.lastSide)
	}
	m = run(t, m, press("esc"))
	if m.stack.Top() != CtxMessages {
		t.Fatalf("esc from main went to %v", m.stack.Top())
	}
}

func TestEnterOpensNextPanel(t *testing.T) {
	m := loaded(t)
	m = run(t, m, press("j")) // sb-test-weu
	m = run(t, m, press("enter"))
	if m.stack.Top() != CtxEntities || m.openNS == nil || m.openNS.Name != "sb-test-weu" {
		t.Fatalf("enter on Namespaces: focus %v ns %v", m.stack.Top(), m.openNS)
	}
	if len(m.entities.items) != 2 || len(m.messages.items) != 0 {
		t.Fatalf("entities %d messages %d", len(m.entities.items), len(m.messages.items))
	}

	m = run(t, m, press("enter")) // orders on sb-test-weu
	if m.stack.Top() != CtxMessages || len(m.messages.items) != 1 {
		t.Fatalf("enter on Entities: focus %v messages %d", m.stack.Top(), len(m.messages.items))
	}
	m = run(t, m, press("enter"))
	if m.stack.Top() != CtxMain {
		t.Fatalf("enter on Messages: focus %v", m.stack.Top())
	}
}

func TestZoom(t *testing.T) {
	m := keysIn(t, loaded(t), "3", "+")
	if !m.zoom || m.stack.Root() != CtxMain {
		t.Fatalf("+ did not zoom the main pane: zoom %v focus %v", m.zoom, m.stack.Root())
	}
	if l := m.layout(); l.leftW != 0 || l.mainW != 120 {
		t.Fatalf("zoomed layout: leftW %d mainW %d", l.leftW, l.mainW)
	}
	requireScreenSize(t, m.render(), 120, 30)
	if m = run(t, m, press("+")); m.zoom || m.layout().leftW == 0 {
		t.Fatal("+ again did not unzoom")
	}

	m = keysIn(t, m, "+", "2")
	if m.zoom || m.stack.Root() != CtxEntities {
		t.Fatalf("focusing a side panel kept the zoom: zoom %v focus %v", m.zoom, m.stack.Root())
	}
}

func TestSearchMainPane(t *testing.T) {
	m := keysIn(t, loaded(t), "3", "enter", "/", "s", "k", "u")
	if m.stack.Top() != CtxSearch || m.search.term != "sku" {
		t.Fatalf("/ on the main pane: top %v term %q", m.stack.Top(), m.search.term)
	}
	lines := m.mainLines(m.layout().mainW - 2)
	matches := searchLines(lines, "SKU")
	if len(matches) != 2 || m.search.line != matches[0] {
		t.Fatalf("matches %v, current %d", matches, m.search.line)
	}
	m = keysIn(t, m, "enter", "n")
	if m.stack.Top() != CtxMain || m.search.line != matches[1] {
		t.Fatalf("n: top %v line %d, want %d", m.stack.Top(), m.search.line, matches[1])
	}
	if m = run(t, m, press("n")); m.search.line != matches[0] {
		t.Fatalf("n did not wrap: line %d", m.search.line)
	}
	if m = run(t, m, press("N")); m.search.line != matches[1] {
		t.Fatalf("N did not wrap back: line %d", m.search.line)
	}
	if !strings.Contains(ansi.Strip(m.render()), "/sku 2/2") {
		t.Fatal("the main pane title does not show the search")
	}

	// Properties: the cursor goes to the matching row.
	m = keysIn(t, m, "]", "/", "i", "s", "r", "enter")
	_, rows := m.mainContent(m.layout().mainW - 2)
	if m.search.line < 0 || rows[m.mainCursor] != m.search.line || m.mainCursor != 4 {
		t.Fatalf("Properties: line %d cursor row %v", m.search.line, rows)
	}

	m = run(t, m, press("esc"))
	if m.search.term != "" || m.stack.Root() != CtxMain {
		t.Fatalf("esc did not clear the search first: term %q focus %v", m.search.term, m.stack.Root())
	}
	if m = keysIn(t, m, "/", "x", "y", "z", "esc"); m.search.term != "" || m.stack.Top() != CtxMain {
		t.Fatalf("esc while typing: term %q top %v", m.search.term, m.stack.Top())
	}
}
