package ui

import (
	"fmt"
	"sort"
	"strconv"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/samuelstrom93/lazybus/internal/bus"
)

// Navigation keys of slice S1b: / filter, s sort, R refresh, y/Y copy.

// --- / filter ------------------------------------------------------------------

// entityText is an entity's visible text (path, active and DLQ count),
// for the filter.
func (m Model) entityText(e bus.Entity) string {
	act, count := "?", "?"
	if e.CountsKnown {
		act, count = strconv.FormatInt(e.ActiveCount, 10), strconv.FormatInt(e.DeadLetterCount, 10)
	}
	return e.Path + " act " + act + " DLQ " + count
}

// messageText is a message row's visible text (sequence number, time,
// reason or subject, MessageId), for the filter.
func (m Model) messageText(msg bus.Message) string {
	return strconv.FormatInt(msg.SequenceNumber, 10) + " " +
		listTime(msg.EnqueuedTime, m.opts.Now(), m.opts.Location) + " " +
		m.rowReason(msg) + " " + msg.MessageID
}

// startFilter handles / on a side panel: the filter is typed into the
// panel title, and the list narrows as you type.
func (m Model) startFilter(cur ContextID) (Model, tea.Cmd) {
	if !cur.isSide() {
		m.setStatus(statusInfo, "/ filters a side panel: focus 1, 2 or 3 first")
		return m, nil
	}
	m.stack.Push(CtxFilter)
	return m, nil
}

// handleFilterKey: text edits the filter of the panel underneath, enter
// keeps it and returns to the list, esc clears it, ↑/↓ move the cursor.
func (m Model) handleFilterKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	c := m.stack.Root()
	text := m.filterOf(c)
	switch {
	case key.Matches(msg, keys.FilterKeep):
		m.stack.Pop()
	case key.Matches(msg, keys.FilterClear):
		m.stack.Pop()
		m.clearFilter(c)
	case msg.String() == "up": // not keys.Up: k is text here
		m.moveBy(c, -1)
	case msg.String() == "down":
		m.moveBy(c, 1)
		if c == CtxMessages {
			return m, m.loadMore()
		}
	case key.Matches(msg, keys.HelpErase):
		if r := []rune(text); len(r) > 0 {
			m.setFilter(c, string(r[:len(r)-1]))
		}
	case msg.String() == "ctrl+u":
		m.setFilter(c, "")
	case msg.Text != "" && msg.Mod&(tea.ModCtrl|tea.ModAlt) == 0:
		m.setFilter(c, text+msg.Text)
	}
	return m, nil
}

func (m Model) filterOf(c ContextID) string {
	switch c {
	case CtxNamespaces:
		return m.namespaces.filter
	case CtxEntities:
		return m.entities.filter
	case CtxMessages:
		return m.messages.filter
	}
	return ""
}

// setFilter sets the filter of panel c; the cursor goes to the first match.
func (m *Model) setFilter(c ContextID, text string) {
	switch c {
	case CtxNamespaces:
		m.namespaces.filter, m.namespaces.cursor, m.namespaces.offset = text, 0, 0
		m.namespaces.applyFilter(nsText)
	case CtxEntities:
		m.entities.filter, m.entities.cursor, m.entities.offset = text, 0, 0
		m.entities.applyFilter(m.entityText)
	case CtxMessages:
		m.messages.filter, m.messages.cursor, m.messages.offset = text, 0, 0
		m.messages.applyFilter(m.messageText)
		m.resetMain()
	}
}

// clearFilter drops the filter of panel c, keeping the cursor on the same
// item. It reports whether there was one.
func (m *Model) clearFilter(c ContextID) bool {
	switch c {
	case CtxNamespaces:
		return clearListFilter(&m.namespaces, nsText)
	case CtxEntities:
		return clearListFilter(&m.entities, m.entityText)
	case CtxMessages:
		return clearListFilter(&m.messages, m.messageText)
	}
	return false
}

func clearListFilter[T any](l *list[T], text func(T) string) bool {
	if l.filter == "" {
		return false
	}
	at := l.allIndex(l.cursor)
	l.filter = ""
	l.applyFilter(text)
	if at >= 0 {
		l.cursor = at
	}
	return true
}

// --- s sort ------------------------------------------------------------------

// sortEntities returns items sorted by path, or with byDLQ by DLQ count
// (largest first; unknown counts last), then path.
func sortEntities(items []bus.Entity, byDLQ bool) []bus.Entity {
	out := append([]bus.Entity(nil), items...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if byDLQ {
			if a.CountsKnown != b.CountsKnown {
				return a.CountsKnown
			}
			if a.CountsKnown && a.DeadLetterCount != b.DeadLetterCount {
				return a.DeadLetterCount > b.DeadLetterCount
			}
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Kind < b.Kind
	})
	return out
}

// toggleSort handles s on Entities; the cursor stays on its entity.
func (m *Model) toggleSort() {
	m.sortDLQ = !m.sortDLQ
	prev, ok := m.entities.selected()
	m.entities.setAll(sortEntities(m.entities.all, m.sortDLQ), m.entityText)
	if ok {
		m.selectEntity(prev)
	}
	if m.sortDLQ {
		m.setStatus(statusInfo, "Entities sorted by DLQ count (unknown ? last); s sorts by path")
	} else {
		m.setStatus(statusInfo, "Entities sorted by path; s sorts by DLQ count")
	}
}

// selectEntity puts the Entities cursor on e when it is visible.
func (m *Model) selectEntity(e bus.Entity) bool {
	for i, it := range m.entities.items {
		if it.Path == e.Path && it.Kind == e.Kind {
			m.entities.cursor = i
			return true
		}
	}
	return false
}

// openEntityMessages opens ent's DLQ in Messages and focuses it.
func (m Model) openEntityMessages(ent bus.Entity) (Model, tea.Cmd) {
	m.openEntity = &ent
	m.subQueue = bus.DeadLetter
	m.focus(CtxMessages)
	cmd := m.peek(*m.openNS, ent, m.subQueue)
	return m, cmd
}

// --- R refresh -----------------------------------------------------------------

// refresh handles R: Namespaces discovers again, Entities lists again
// (counts included), Messages and the main pane peek again from the start.
// The rows stay until the new ones arrive; the cursor keeps its entity,
// or for messages its index.
func (m Model) refresh(cur ContextID) (Model, tea.Cmd) {
	switch cur {
	case CtxNamespaces:
		cmd := m.rediscover()
		return m, cmd
	case CtxEntities:
		if m.openNS == nil {
			m.setStatus(statusInfo, "nothing to refresh: open a namespace first")
			return m, nil
		}
		cmd := m.loadEntities(*m.openNS, false, true)
		return m, cmd
	}
	if m.openNS == nil || m.openEntity == nil {
		m.setStatus(statusInfo, "nothing to refresh: open an entity first")
		return m, nil
	}
	m.keepIndex, m.hasKeepIndex = m.messages.cursor, true
	m.hasAfterPage = false
	cmd := m.peekFrom(*m.openNS, *m.openEntity, m.subQueue, 0)
	return m, cmd
}

// --- y / Y copy ------------------------------------------------------------------

// copySelected handles y: the body on the Body tab, the selected property's
// value on Properties, the selected field on System. With Namespaces
// focused (the main pane shows the namespace) it copies the FQDN.
func (m Model) copySelected(cur ContextID) (Model, tea.Cmd) {
	if cur == CtxNamespaces {
		r, ok := m.namespaces.selected()
		switch {
		case !ok:
			m.setStatus(statusInfo, "nothing to copy")
			return m, nil
		case r.kind == rowNamespace:
			return m.copyText(r.ns.FQDN, "FQDN "+r.ns.FQDN)
		}
		return m.copyText(r.sub.ID, "subscription ID "+r.sub.ID)
	}
	msg, ok := m.selectedMessage()
	if !ok || m.openEntity == nil {
		m.setStatus(statusInfo, "no message selected: nothing to copy")
		return m, nil
	}
	switch m.tab {
	case tabProperties:
		props := m.propertyItems(msg)
		if len(props) == 0 {
			m.setStatus(statusInfo, "no application properties: nothing to copy")
			return m, nil
		}
		p := props[max(0, min(m.mainCursor, len(props)-1))]
		return m.copyText(p.raw, p.key+" = "+p.raw)
	case tabSystem:
		fields := m.systemFields(msg)
		f := fields[max(0, min(m.mainCursor, len(fields)-1))]
		if f.value == "" {
			m.setStatus(statusInfo, "%s is empty: nothing copied", f.label)
			return m, nil
		}
		return m.copyText(f.value, f.label+" "+f.value)
	}
	if len(msg.Body) == 0 {
		m.setStatus(statusInfo, "the body is empty: nothing copied")
		return m, nil
	}
	return m.copyText(string(msg.Body), fmt.Sprintf("body of %s seq %d (%d bytes)",
		entityLabel(*m.openEntity, m.subQueue), msg.SequenceNumber, len(msg.Body)))
}

// copyMessageID handles Y.
func (m Model) copyMessageID() (Model, tea.Cmd) {
	msg, ok := m.selectedMessage()
	switch {
	case !ok:
		m.setStatus(statusInfo, "no message selected: nothing to copy")
		return m, nil
	case msg.MessageID == "":
		m.setStatus(statusInfo, "the message has no MessageId: nothing copied")
		return m, nil
	}
	return m.copyText(msg.MessageID, "MessageId "+msg.MessageID)
}

// copyText puts text on the clipboard and says so in the status bar.
func (m Model) copyText(text, what string) (Model, tea.Cmd) {
	m.setStatus(statusOK, "copied %s", what)
	return m, m.clip.copy(text)
}
