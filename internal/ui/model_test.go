package ui

import (
	"testing"

	tea "charm.land/bubbletea/v2"

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

func run(t *testing.T, m Model, msg tea.Msg) Model {
	t.Helper()
	next, cmd := m.Update(msg)
	return runCmd(t, next.(Model), cmd)
}

func runCmd(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	for cmd != nil {
		msg := cmd()
		if _, ok := msg.(tea.QuitMsg); ok {
			t.Fatal("unexpected quit")
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
