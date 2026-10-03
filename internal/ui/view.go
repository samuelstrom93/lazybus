package ui

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/samuelstrom93/lazybus/internal/bus"
)

// Smallest screen lazybus draws a layout for.
const (
	minWidth  = 60
	minHeight = 16
)

// layout holds the sizes derived from the terminal size and focus.
type layout struct {
	leftW, mainW int
	side         [3]int // content rows of Namespaces, Entities, Messages
	mainRows     int    // content rows of the main pane
	logRows      int
}

func (m Model) layout() layout {
	w, h := m.width, m.height
	frameH := h - 1 // options bar
	var l layout
	l.leftW = max(26, min(40, w*3/10))
	l.mainW = w - l.leftW
	l.logRows = max(2, min(5, frameH/7))
	l.mainRows = max(1, frameH-3-l.logRows)

	// Side panels share borders: top, two separators, bottom.
	total := max(3, frameH-4)
	unit := max(1, total/4)
	counts := [3]int{len(m.namespaces.items), len(m.entities.items), len(m.messages.items)}
	expanded := int(m.focusedSide() - CtxNamespaces)
	rest := total
	for i := range l.side {
		if i == expanded {
			continue
		}
		l.side[i] = max(1, min(counts[i], unit))
		rest -= l.side[i]
	}
	l.side[expanded] = max(1, rest)
	return l
}

func (m Model) render() string {
	if m.width <= 0 || m.height <= 0 {
		return ""
	}
	if m.width < minWidth || m.height < minHeight {
		msg := fmt.Sprintf("lazybus needs at least %d×%d (now %d×%d)", minWidth, minHeight, m.width, m.height)
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, fitPlain(msg, m.width))
	}
	l := m.layout()
	left := m.renderSide(l)
	right := m.renderMain(l)
	lines := make([]string, 0, m.height)
	for i := range left {
		lines = append(lines, left[i]+right[i])
	}
	lines = append(lines, m.renderOptions())
	screen := strings.Join(lines, "\n")
	if m.stack.Top() == CtxHelp {
		screen = m.overlayHelp(screen)
	}
	return screen
}

// --- side panels -----------------------------------------------------------

func (m Model) renderSide(l layout) []string {
	focus := m.stack.Root()
	inner := l.leftW - 2
	panels := []struct {
		ctx   ContextID
		title []seg
		rows  []string
	}{
		{CtxNamespaces, m.sideTitle(CtxNamespaces, "1 Namespaces", m.namespaces.loading), m.namespaceRows(inner, l.side[0])},
		{CtxEntities, m.sideTitle(CtxEntities, "2 Entities", m.entities.loading), m.entityRows(inner, l.side[1])},
		{CtxMessages, m.messagesTitle(), m.messageRows(inner, l.side[2])},
	}
	var out []string
	for i, p := range panels {
		left, right := "├", "┤"
		if i == 0 {
			left, right = "┌", "┐"
		}
		edge := stBorder
		if p.ctx == focus || i > 0 && panels[i-1].ctx == focus {
			edge = stBorderFocus
		}
		out = append(out, hBorder(l.leftW, left, right, p.title, edge))
		side := stBorder
		if p.ctx == focus {
			side = stBorderFocus
		}
		for _, r := range p.rows {
			out = append(out, boxRow(r, side))
		}
	}
	bottom := stBorder
	if focus == CtxMessages {
		bottom = stBorderFocus
	}
	return append(out, hBorder(l.leftW, "└", "┘", nil, bottom))
}

func (m Model) titleStyle(c ContextID) lipgloss.Style {
	if m.stack.Root() == c {
		return stTitleFocus
	}
	return stPlain
}

func (m Model) sideTitle(c ContextID, name string, loading bool) []seg {
	t := []seg{{name, m.titleStyle(c)}}
	if loading {
		t = append(t, seg{" loading…", stDim})
	}
	return t
}

func (m Model) messagesTitle() []seg {
	active := m.titleStyle(CtxMessages)
	if m.stack.Root() != CtxMessages {
		active = stBold
	}
	dlq, act := active, stDim
	if m.subQueue == bus.Active {
		dlq, act = stDim, active
	}
	t := []seg{{"3 Messages", m.titleStyle(CtxMessages)}, {"  ", stPlain}, {"DLQ", dlq}, {"│", stDim}, {"Active", act}}
	switch {
	case m.messages.loading:
		t = append(t, seg{" loading…", stDim})
	case m.messages.err != nil && len(m.messages.items) > 0:
		// A failed next page: the rows stay, the title says so.
		t = append(t, seg{" error", stErr})
	}
	return t
}

// listRows renders n rows of a list: the visible window from offset, the
// cursor row marked, empty rows padded. status replaces the rows when the
// list has nothing to show.
func listRows[T any](l list[T], focused bool, width, n int, status []seg, render func(T) []seg) []string {
	out := make([]string, 0, n)
	if len(l.items) == 0 && status != nil {
		out = append(out, row(append([]seg{{"  ", stPlain}}, status...), width, false))
	}
	for i := l.offset; i < len(l.items) && len(out) < n; i++ {
		cursor := i == l.cursor
		mark := "  "
		if cursor {
			mark = "▸ "
		}
		segs := append([]seg{{mark, stPlain}}, render(l.items[i])...)
		out = append(out, row(segs, width, cursor && focused))
	}
	for len(out) < n {
		out = append(out, strings.Repeat(" ", width))
	}
	return out
}

func listStatus[T any](l list[T], empty string) []seg {
	switch {
	case l.err != nil:
		return []seg{{"error: " + shortErr(l.err), stErr}}
	case l.loading:
		return []seg{{"loading…", stDim}}
	case empty != "":
		return []seg{{empty, stDim}}
	}
	return nil
}

// shortErr is the error text for a panel: a *bus.Error without its
// operation (the panel already says what failed).
func shortErr(err error) string {
	var be *bus.Error
	if errors.As(err, &be) && be.Msg != "" {
		return be.Msg
	}
	return err.Error()
}

func (m Model) namespaceRows(width, n int) []string {
	return listRows(m.namespaces, m.stack.Root() == CtxNamespaces, width, n,
		listStatus(m.namespaces, "no namespaces"),
		func(ns bus.Namespace) []seg { return []seg{{ns.Name, stPlain}} })
}

func (m Model) entityRows(width, n int) []string {
	empty := "no queues or subscriptions"
	if m.openNS == nil {
		empty = "open a namespace"
	}
	digits := 1
	for _, e := range m.entities.items {
		if e.CountsKnown {
			digits = max(digits, len(strconv.FormatInt(e.DeadLetterCount, 10)))
		}
	}
	countW := len("DLQ ") + digits
	pathW := max(1, width-2-1-countW)
	return listRows(m.entities, m.stack.Root() == CtxEntities, width, n,
		listStatus(m.entities, empty),
		func(e bus.Entity) []seg {
			// Unknown counts (emulator) show "?", never a made-up 0.
			count, text := stWarn, "?"
			if e.CountsKnown {
				text = strconv.FormatInt(e.DeadLetterCount, 10)
			}
			if !e.CountsKnown || e.DeadLetterCount == 0 {
				count = stDim
			}
			return []seg{
				{fitPlain(e.Path, pathW) + " ", stPlain},
				{"DLQ " + padLeft(text, digits), count},
			}
		})
}

func (m Model) messageRows(width, n int) []string {
	empty := "no messages"
	if m.openEntity == nil {
		empty = "open an entity"
	} else if m.subQueue == bus.DeadLetter {
		empty = "dead-letter queue is empty"
	}
	digits := 1
	reasonMax := 0
	for _, msg := range m.messages.items {
		digits = max(digits, len(strconv.FormatInt(msg.SequenceNumber, 10)))
		reasonMax = max(reasonMax, ansi.StringWidth(sanitize(m.rowReason(msg))))
	}
	// cursor(2) seq " " time(5) " " reason " " id
	rest := width - 2 - digits - 1 - 5 - 1
	reasonW := min(reasonMax, max(10, rest*3/5))
	idW := rest - reasonW - 1
	if idW < 4 {
		reasonW, idW = rest, 0
	}
	now := m.opts.Now()
	return listRows(m.messages, m.stack.Root() == CtxMessages, width, n,
		listStatus(m.messages, empty),
		func(msg bus.Message) []seg {
			segs := []seg{
				{padLeft(strconv.FormatInt(msg.SequenceNumber, 10), digits) + " ", stPlain},
				{listTime(msg.EnqueuedTime, now, m.opts.Location) + " ", stDim},
				{fitPlain(m.rowReason(msg), reasonW), stPlain},
			}
			if idW > 0 {
				segs = append(segs, seg{" " + msg.MessageID, stDim})
			}
			return segs
		})
}

func (m Model) rowReason(msg bus.Message) string {
	if m.subQueue == bus.Active {
		return msg.Subject
	}
	return msg.DeadLetterReason
}

// --- main pane and log -----------------------------------------------------

func (m Model) renderMain(l layout) []string {
	focused := m.stack.Root() == CtxMain
	edge := stBorder
	if focused {
		edge = stBorderFocus
	}
	inner := l.mainW - 2

	var tabs []seg
	for t := range tabCount {
		if t > 0 {
			tabs = append(tabs, seg{" │ ", stDim})
		}
		st := stDim
		if t == m.tab {
			st = stBold
			if focused {
				st = stTitleFocus
			}
		}
		tabs = append(tabs, seg{t.String(), st})
	}

	out := []string{hBorder(l.mainW, "┌", "┐", tabs, edge)}
	lines := m.mainLines(inner)
	for i := range l.mainRows {
		var s string
		if j := m.mainScroll + i; j < len(lines) {
			s = lines[j]
		}
		out = append(out, boxRow(fitStyled(s, inner), edge))
	}
	out = append(out, hBorder(l.mainW, "├", "┤", []seg{{"Log", stPlain}}, edge))
	start := max(0, len(m.log)-l.logRows)
	for i := range l.logRows {
		var r string
		if j := start + i; j < len(m.log) {
			e := m.log[j]
			st := stPlain
			if e.err {
				st = stErr
			}
			r = row([]seg{{" " + e.at.In(m.opts.Location).Format("15:04") + " ", stDim}, {e.text, st}}, inner, false)
		} else {
			r = strings.Repeat(" ", inner)
		}
		out = append(out, boxRow(r, stBorder))
	}
	return append(out, hBorder(l.mainW, "└", "┘", nil, stBorder))
}

// fitStyled truncates or pads an already styled line to width cells.
func fitStyled(s string, width int) string {
	if ansi.StringWidth(s) > width {
		s = ansi.Truncate(s, width, "…")
	}
	// Measure after truncating: a wide rune that does not fit is dropped
	// whole, which can leave the line one cell short.
	return s + strings.Repeat(" ", max(0, width-ansi.StringWidth(s)))
}

// linesCache holds the main pane lines for one (message, tab, width).
type linesCache struct {
	key   linesKey
	lines []string
}

type linesKey struct {
	ns, path string
	kind     bus.EntityKind
	sub      bus.SubQueue
	seq      int64
	tab      mainTab
	width    int
}

// mainLines is the full content of the main pane for the current tab, one
// styled line per screen row, each at most width cells. Lines for a
// message are cached per (message, tab, width): View and every Update ask
// for them, and formatting a large body each time would lag the keys.
func (m Model) mainLines(width int) []string {
	msg, ok := m.selectedMessage()
	if !ok {
		return m.statusLines(width)
	}
	var key linesKey
	if m.openNS != nil && m.openEntity != nil {
		key = linesKey{m.openNS.FQDN, m.openEntity.Path, m.openEntity.Kind, m.subQueue, msg.SequenceNumber, m.tab, width}
		if m.lines != nil && m.lines.lines != nil && m.lines.key == key {
			return m.lines.lines
		}
	}
	lines := m.messageLines(msg, width)
	if m.lines != nil && m.openNS != nil && m.openEntity != nil {
		*m.lines = linesCache{key: key, lines: lines}
	}
	return lines
}

// statusLines fill the main pane when no message is selected: the newest
// error (wrapped, in full) or the loading/empty state.
func (m Model) statusLines(width int) []string {
	for _, err := range []error{m.messages.err, m.entities.err, m.namespaces.err} {
		if err == nil {
			continue
		}
		var out []string
		for _, l := range wrapLines("error: "+sanitize(err.Error()), width-2) {
			out = append(out, row([]seg{{" " + l, stErr}}, width, false))
		}
		return out
	}
	status := listStatus(m.messages, "")
	if status == nil {
		status = []seg{{"no message selected", stDim}}
	}
	return []string{row(append([]seg{{" ", stPlain}}, status...), width, false)}
}

func (m Model) messageLines(msg bus.Message, width int) []string {
	switch m.tab {
	case tabProperties:
		return m.propertyLines(msg, width)
	case tabSystem:
		return m.systemLines(msg, width)
	}
	text, _ := bodyText(msg.Body)
	if text == "" {
		return []string{" " + stDim.Render("(empty body)")}
	}
	var out []string
	for _, l := range wrapLines(text, width-2) {
		out = append(out, " "+l)
	}
	return out
}

const removedOnResubmit = "✕ removed on resubmit"

func (m Model) propertyLines(msg bus.Message, width int) []string {
	type prop struct {
		key, typ, val string
		marker        bool
	}
	var props []prop
	for _, p := range msg.Properties {
		props = append(props, prop{sanitize(p.Key), p.Type.String(), sanitize(propertyValue(p)), false})
	}
	if msg.DeadLetterReason != "" {
		props = append(props, prop{bus.MarkerDeadLetterReason, bus.TypeString.String(), sanitize(msg.DeadLetterReason), true})
	}
	if msg.DeadLetterErrorDescription != "" {
		props = append(props, prop{bus.MarkerDeadLetterErrorDescription, bus.TypeString.String(), sanitize(msg.DeadLetterErrorDescription), true})
	}
	if len(props) == 0 {
		return []string{" " + stDim.Render("(no application properties)")}
	}
	keyW := len("Key")
	for _, p := range props {
		keyW = max(keyW, ansi.StringWidth(p.key))
	}
	keyW = min(keyW, max(8, width/3))
	typeW := len("DateTime")

	// Markers go in their own dimmed section under one "✕ removed on
	// resubmit" label, so their values (often the only error text there is)
	// keep the full column instead of sharing it with a per-row suffix.
	out := []string{row([]seg{{" " + fitPlain("Key", keyW) + "  " + fitPlain("Type", typeW) + "  Value", stDim}}, width, false)}
	markerHeader := false
	for _, p := range props {
		if !p.marker {
			out = append(out, row([]seg{
				{" " + fitPlain(p.key, keyW) + "  ", stPlain},
				{fitPlain(p.typ, typeW) + "  ", stDim},
				{p.val, stPlain},
			}, width, false))
			continue
		}
		if !markerHeader {
			markerHeader = true
			out = append(out, "", row([]seg{{" Dead-letter markers  " + removedOnResubmit, stDim}}, width, false))
		}
		out = append(out, row([]seg{
			{" " + fitPlain(p.key, keyW) + "  " + fitPlain(p.typ, typeW) + "  " + p.val, stDim},
		}, width, false))
	}
	return out
}

func (m Model) systemLines(msg bus.Message, width int) []string {
	fields := []struct {
		label, value string
		editable     bool
	}{
		{"MessageId", msg.MessageID, false},
		{"CorrelationId", msg.CorrelationID, false},
		{"Subject", msg.Subject, true},
		{"ContentType", msg.ContentType, true},
		{"SessionId", msg.SessionID, false},
		{"PartitionKey", msg.PartitionKey, false},
		{"To", msg.To, false},
		{"ReplyTo", msg.ReplyTo, false},
		{"TTL", ttlText(msg.TimeToLive), false},
		{"DeliveryCount", strconv.FormatUint(uint64(msg.DeliveryCount), 10), false},
		{"EnqueuedTime", fullTime(msg.EnqueuedTime, m.opts.Location), false},
		{"SequenceNumber", strconv.FormatInt(msg.SequenceNumber, 10), false},
		{"DeadLetterSource", msg.DeadLetterSource, false},
	}
	const labelW = 18
	var out []string
	for _, f := range fields {
		label := f.label
		if f.editable {
			label += " ✎"
		}
		val := seg{f.value, stPlain}
		if f.value == "" {
			val = seg{"—", stDim}
		}
		out = append(out, row([]seg{{" " + fitPlain(label, labelW), stDim}, {" ", stPlain}, val}, width, false))
	}
	return out
}

// --- options bar -------------------------------------------------------------

func (m Model) renderOptions() string {
	var right []seg
	if m.opts.ReadOnly {
		right = []seg{{"READ-ONLY", stReadOnly}, {" ", stPlain}}
	}
	rw := segsWidth(right)

	left := []seg{{" ", stPlain}}
	for i, b := range optionsBindings(m.stack.Top()) {
		if i > 0 {
			left = append(left, seg{"  ", stPlain})
		}
		h := b.Help()
		left = append(left, seg{h.Key, stKey}, seg{" " + h.Desc, stPlain})
	}
	l := row(left, max(0, m.width-rw-1), false)
	r, _ := renderSegs(right, rw, nil)
	return l + " " + r
}

// --- `?` menu -----------------------------------------------------------------

// helpBox returns the popup size and list rows for the `?` menu.
func (m Model) helpBox() (w, h, listRows int) {
	w = min(m.width-4, 56)
	entries := len(m.helpEntries())
	h = min(m.height-4, max(entries, 3)+4)
	return w, h, h - 4
}

func (m Model) overlayHelp(screen string) string {
	w, h, n := m.helpBox()
	inner := w - 2
	entries := m.helpEntries()
	offset := max(0, min(m.help.offset, len(entries)-n))

	lines := []string{hBorder(w, "┌", "┐", []seg{{"Keybindings", stTitleFocus}}, stBorderFocus)}
	lines = append(lines, boxRow(row([]seg{{" Filter: ", stDim}, {m.help.filter, stPlain}, {"▏", stTitleFocus}}, inner, false), stBorderFocus))
	lines = append(lines, stBorderFocus.Render("├"+strings.Repeat("─", inner)+"┤"))
	for i := range n {
		var r string
		switch j := offset + i; {
		case j >= len(entries):
			if i == 0 && len(entries) == 0 {
				r = row([]seg{{" no matching keys", stDim}}, inner, false)
			} else {
				r = strings.Repeat(" ", inner)
			}
		case entries[j].section != "":
			r = row([]seg{{" " + entries[j].section, stBold}}, inner, false)
		default:
			e := entries[j]
			r = row([]seg{{"   " + fitPlain(e.key, 8) + " ", stKey}, {e.desc, stPlain}}, inner, false)
		}
		lines = append(lines, boxRow(r, stBorderFocus))
	}
	lines = append(lines, hBorder(w, "└", "┘", []seg{{"type to filter · esc close", stDim}}, stBorderFocus))

	x := (m.width - w) / 2
	y := max(0, (m.height-1-h)/2)
	base := lipgloss.NewLayer(screen)
	popup := lipgloss.NewLayer(strings.Join(lines, "\n")).X(x).Y(y).Z(1)
	// The compositor trims trailing blanks; pad back to full width.
	out := strings.Split(lipgloss.NewCompositor(base, popup).Render(), "\n")
	for i, l := range out {
		out[i] = fitStyled(l, m.width)
	}
	return strings.Join(out, "\n")
}
