package ui

import (
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/samuelstrom93/lazybus/internal/bus"
	"github.com/samuelstrom93/lazybus/internal/recent"
)

// The ctrl-r popup lists the entities opened last, newest first; enter
// opens one. Only namespaces this run can reach open: one from discovery or
// the command line. lazybus never stores a connection string, so a SAS or
// emulator entry from an earlier run is a hint of the flag to pass.

type recentState struct {
	cursor int
}

type recentSavedMsg struct{ err error }

// remember records ent of ns as the newest recent entry and saves the list.
func (m *Model) remember(ns bus.Namespace, ent bus.Entity) tea.Cmd {
	if m.opts.Recent == nil {
		return nil
	}
	m.recents = recent.Add(m.recents, recent.Entry{
		FQDN: ns.FQDN, Name: ns.Name, Auth: ns.Auth,
		Path: ent.Path, Sub: ent.Kind == bus.KindSubscription, OpenAt: m.opts.Now(),
	})
	store, rev, entries := m.opts.Recent, m.opts.Recent.Next(), m.recents
	return func() tea.Msg {
		return recentSavedMsg{err: store.Save(rev, entries)}
	}
}

func (m Model) startRecent() (Model, tea.Cmd) {
	if m.opts.Recent == nil {
		m.setStatus(statusInfo, "recent entities are not kept in this mode")
		return m, nil
	}
	if len(m.recents) == 0 {
		m.setStatus(statusInfo, "no recent entities yet: they are kept as you open them")
		return m, nil
	}
	m.recent = recentState{}
	m.stack.Push(CtxRecent)
	return m, nil
}

func (m Model) handleRecentKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	switch {
	case key.Matches(msg, keys.JumpCancel):
		m.stack.Pop()
	case key.Matches(msg, keys.JumpUp, keys.Up):
		m.recent.cursor = max(0, m.recent.cursor-1)
	case key.Matches(msg, keys.JumpDown, keys.Down):
		m.recent.cursor = min(len(m.recents)-1, m.recent.cursor+1)
	case key.Matches(msg, keys.JumpOpen):
		if m.recent.cursor >= len(m.recents) {
			return m, nil
		}
		e := m.recents[m.recent.cursor]
		ns, ok := m.reachable(e)
		if !ok {
			m.setStatus(statusWarn, "%s: %s", e.Name, m.recentHint(e))
			return m, nil
		}
		m.stack.Pop()
		ent := bus.Entity{Path: e.Path, Kind: bus.KindQueue}
		if e.Sub {
			ent.Kind = bus.KindSubscription
		}
		m.selectNamespaceRow("ns:" + ns.FQDN)
		if m.openNS != nil && m.openNS.FQDN == ns.FQDN && !m.entities.loading && m.entities.err == nil {
			return m.openRecentEntity(ent)
		}
		m = m.openNamespace(ns)
		m.afterEntities = &ent
		return m, m.loadEntities(ns, false, false)
	}
	return m, nil
}

// openRecentEntity opens ent from the loaded Entities list.
func (m Model) openRecentEntity(ent bus.Entity) (Model, tea.Cmd) {
	for _, e := range m.entities.all {
		if e.Path == ent.Path && e.Kind == ent.Kind {
			if !m.selectEntity(e) {
				m.entities.filter = ""
				m.entities.applyFilter(m.entityText)
				m.selectEntity(e)
			}
			return m.openEntityMessages(e)
		}
	}
	m.setStatus(statusWarn, "%s is not in %s any more", ent.Path, m.openNS.Name)
	return m, nil
}

// reachable is the namespace of e when this run can open it: it is in the
// Namespaces panel (discovered, or given on the command line).
func (m Model) reachable(e recent.Entry) (bus.Namespace, bool) {
	for _, r := range m.namespaces.all {
		if r.kind == rowNamespace && r.ns.FQDN == e.FQDN {
			return r.ns, true
		}
	}
	return bus.Namespace{}, false
}

// recentHint says how to reach an entry this run cannot open.
func (m Model) recentHint(e recent.Entry) string {
	switch {
	case strings.Contains(e.Auth, "emulator"):
		return "start lazybus with --emulator to open it"
	case strings.Contains(e.Auth, "SAS"):
		return "start lazybus with --connection-string to open it (lazybus does not store it)"
	case m.namespaces.loading:
		return "not discovered yet: discovery is still running"
	}
	return "not discovered: start lazybus with --namespace " + e.FQDN
}

func (m Model) overlayRecent(screen string) string {
	w := min(m.width-4, 80)
	inner := w - 2
	n := max(1, min(len(m.recents), m.height-1-6))
	cursor := max(0, min(m.recent.cursor, len(m.recents)-1))
	offset := max(0, cursor-n+1)

	lines := []string{hBorder(w, "┌", "┐", []seg{{"Recent entities", stTitleFocus}}, stBorderFocus)}
	nameW := 0
	for _, e := range m.recents {
		nameW = max(nameW, ansi.StringWidth(sanitize(e.Name)))
	}
	nameW = min(nameW, inner/3)
	for i := range n {
		j := offset + i
		if j >= len(m.recents) {
			lines = append(lines, boxRow(strings.Repeat(" ", inner), stBorderFocus))
			continue
		}
		e := m.recents[j]
		mark := "  "
		if j == cursor {
			mark = "▸ "
		}
		_, ok := m.reachable(e)
		st := stPlain
		segs := []seg{{mark + fitPlain(e.Name, nameW) + "  ", st}, {e.Path, st}}
		if !ok {
			segs = []seg{{mark + fitPlain(e.Name, nameW) + "  ", stDim}, {e.Path, stDim}, {"  " + m.recentHint(e), stDim}}
		}
		lines = append(lines, boxRow(row(segs, inner, j == cursor), stBorderFocus))
	}
	lines = append(lines, hBorder(w, "└", "┘", []seg{{"enter open · esc cancel", stDim}}, stBorderFocus))
	return m.overlay(screen, lines, w)
}
