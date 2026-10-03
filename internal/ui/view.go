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
	switch m.stack.Top() {
	case CtxHelp:
		screen = m.overlayHelp(screen)
	case CtxJump:
		screen = m.overlayJump(screen)
	case CtxConfirm:
		screen = m.overlayConfirm(screen)
	case CtxBusy:
		screen = m.overlayBusy(screen)
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
		{CtxNamespaces, m.sideTitle(CtxNamespaces, "1 Namespaces", m.namespaces.loading, m.namespaces.filter), m.namespaceRows(inner, l.side[0])},
		{CtxEntities, m.entitiesTitle(), m.entityRows(inner, l.side[1])},
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

func (m Model) sideTitle(c ContextID, name string, loading bool, filter string) []seg {
	t := []seg{{name, m.titleStyle(c)}}
	if loading {
		t = append(t, seg{" loading…", stDim})
	}
	return append(t, m.filterTitle(c, filter)...)
}

// filterTitle is the filter part of a panel title: " /text" while it
// applies, with a cursor while it is being typed.
func (m Model) filterTitle(c ContextID, filter string) []seg {
	typing := m.stack.Top() == CtxFilter && m.stack.Root() == c
	if filter == "" && !typing {
		return nil
	}
	t := []seg{{" /" + filter, stKey}}
	if typing {
		t = append(t, seg{"▏", stTitleFocus})
	}
	return t
}

func (m Model) entitiesTitle() []seg {
	t := m.sideTitle(CtxEntities, "2 Entities", m.entities.loading, "")
	if m.sortDLQ {
		t = append(t, seg{" by DLQ", stDim})
	}
	return append(t, m.filterTitle(CtxEntities, m.entities.filter)...)
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
	return append(t, m.filterTitle(CtxMessages, m.messages.filter)...)
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
	case l.loading && len(l.all) == 0:
		return []seg{{"loading…", stDim}}
	case len(l.all) > 0 && l.filter != "":
		return []seg{{"no match for /" + l.filter, stDim}}
	case empty != "":
		return []seg{{empty, stDim}}
	}
	return nil
}

// isSessionful reports whether err is the Active-tab peek of a sessionful
// entity: not a failure, a v0.1 limit (spec §4 panel 3).
func (m Model) isSessionful(err error) bool {
	return m.subQueue == bus.Active && bus.KindOf(err) == bus.ErrNotAllowed
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
		func(r nsRow) []seg { return nsRowSegs(r, width-2) })
}

func (m Model) entityRows(width, n int) []string {
	empty := "no queues or subscriptions"
	if m.openNS == nil {
		empty = "open a namespace"
	}
	digits := 1
	for _, e := range m.entities.all {
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
	for _, msg := range m.messages.all {
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
	status := listStatus(m.messages, empty)
	if m.isSessionful(m.messages.err) {
		status = []seg{{shortErr(m.messages.err), stWarn}}
	}
	return listRows(m.messages, m.stack.Root() == CtxMessages, width, n,
		status,
		func(msg bus.Message) []seg {
			seq := padLeft(strconv.FormatInt(msg.SequenceNumber, 10), digits) + " "
			at := listTime(msg.EnqueuedTime, now, m.opts.Location) + " "
			if mark, ok := m.markOf(msg.SequenceNumber); ok {
				// A marked row (SendUncertain, CleanupPending) is amber or
				// red and names its outcome in the reason and id columns.
				st := levelStyle(markStyleOf(mark.outcome))
				labelW := reasonW
				if idW > 0 {
					labelW += 1 + idW
				}
				return []seg{{seq, st}, {at, st}, {fitPlain(mark.outcome.String()+" · "+m.rowReason(msg), labelW), st}}
			}
			segs := []seg{
				{seq, stPlain},
				{at, stDim},
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
	lines, scroll := m.mainLines(inner), m.mainScroll
	if m.stack.Root() == CtxNamespaces {
		// Namespaces focused: the main pane shows the selected namespace.
		var title string
		title, lines = m.namespaceDetails(inner)
		tabs, scroll = []seg{{title, stBold}}, 0
	}

	out := []string{hBorder(l.mainW, "┌", "┐", tabs, edge)}
	for i := range l.mainRows {
		var s string
		if j := scroll + i; j < len(lines) {
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

// linesCache holds the Body tab lines for one (message, width).
type linesCache struct {
	key   linesKey
	lines []string
}

type linesKey struct {
	ns, path string
	kind     bus.EntityKind
	sub      bus.SubQueue
	seq      int64
	width    int
}

// mainLines is the full content of the main pane for the current tab, one
// styled line per screen row, each at most width cells.
func (m Model) mainLines(width int) []string {
	lines, _ := m.mainContent(width)
	return lines
}

// mainContent is mainLines plus, on the Properties and System tabs, the
// line of each selectable row (what y copies). Body lines are cached per
// (message, width): View and every Update ask for them, and formatting a
// large body each time would lag the keys. The other tabs are short and
// show the row cursor, so they are rendered each time.
func (m Model) mainContent(width int) (lines []string, rows []int) {
	msg, ok := m.selectedMessage()
	if !ok {
		return m.statusLines(width), nil
	}
	switch m.tab {
	case tabProperties:
		lines, rows = m.propertyLines(msg, width)
	case tabSystem:
		lines, rows = m.systemLines(msg, width)
	default:
		lines = m.cachedLines(msg, width)
	}
	if banner := m.bannerLines(msg.SequenceNumber, width); banner != nil {
		for i := range rows {
			rows[i] += len(banner)
		}
		lines = append(banner, lines...)
	}
	return lines, rows
}

// cachedLines is the Body tab of msg, from the cache when it matches.
func (m Model) cachedLines(msg bus.Message, width int) []string {
	var key linesKey
	if m.openNS != nil && m.openEntity != nil {
		key = linesKey{m.openNS.FQDN, m.openEntity.Path, m.openEntity.Kind, m.subQueue, msg.SequenceNumber, width}
		if m.lines != nil && m.lines.lines != nil && m.lines.key == key {
			return m.lines.lines
		}
	}
	lines := m.bodyLines(msg, width)
	if m.lines != nil && m.openNS != nil && m.openEntity != nil {
		*m.lines = linesCache{key: key, lines: lines}
	}
	return lines
}

// statusLines fill the main pane when no message is selected: the newest
// error (wrapped, in full) or the loading/empty state.
func (m Model) statusLines(width int) []string {
	if m.isSessionful(m.messages.err) {
		var out []string
		for _, l := range wrapLines(shortErr(m.messages.err)+". The DLQ tab works: press tab.", width-2) {
			out = append(out, row([]seg{{" " + l, stWarn}}, width, false))
		}
		return out
	}
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

// bodyLines is the Body tab: wrapped, not cut.
func (m Model) bodyLines(msg bus.Message, width int) []string {
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

// propItem is one row of the Properties tab. raw is the value y copies.
type propItem struct {
	key, typ, val, raw string
	marker             bool
}

// propertyItems are the application properties, then the Dead-letter
// Markers, in display order.
func (m Model) propertyItems(msg bus.Message) []propItem {
	var props []propItem
	for _, p := range msg.Properties {
		raw := propertyValue(p, m.opts.Location)
		props = append(props, propItem{sanitize(p.Key), p.Type.String(), sanitize(raw), raw, false})
	}
	if msg.DeadLetterReason != "" {
		props = append(props, propItem{bus.MarkerDeadLetterReason, bus.TypeString.String(), sanitize(msg.DeadLetterReason), msg.DeadLetterReason, true})
	}
	if msg.DeadLetterErrorDescription != "" {
		props = append(props, propItem{bus.MarkerDeadLetterErrorDescription, bus.TypeString.String(), sanitize(msg.DeadLetterErrorDescription), msg.DeadLetterErrorDescription, true})
	}
	return props
}

// cursorMark is the first cell of a Properties or System row: ▸ on the
// selected row while the main pane is not focused (focused, the row is
// highlighted instead).
func (m Model) cursorMark(i int) (mark string, selected bool) {
	if i != m.mainCursor {
		return " ", false
	}
	if m.stack.Root() == CtxMain {
		return " ", true
	}
	return "▸", false
}

func (m Model) propertyLines(msg bus.Message, width int) ([]string, []int) {
	props := m.propertyItems(msg)
	if len(props) == 0 {
		return []string{" " + stDim.Render("(no application properties)")}, nil
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
	rows := make([]int, 0, len(props))
	markerHeader := false
	for i, p := range props {
		mark, sel := m.cursorMark(i)
		if !p.marker {
			rows = append(rows, len(out))
			out = append(out, row([]seg{
				{mark + fitPlain(p.key, keyW) + "  ", stPlain},
				{fitPlain(p.typ, typeW) + "  ", stDim},
				{p.val, stPlain},
			}, width, sel))
			continue
		}
		if !markerHeader {
			markerHeader = true
			out = append(out, "", row([]seg{{" Dead-letter markers  " + removedOnResubmit, stDim}}, width, false))
		}
		rows = append(rows, len(out))
		out = append(out, row([]seg{
			{mark + fitPlain(p.key, keyW) + "  " + fitPlain(p.typ, typeW) + "  " + p.val, stDim},
		}, width, sel))
	}
	return out, rows
}

// sysField is one row of the System tab.
type sysField struct {
	label, value string
	editable     bool
}

func (m Model) systemFields(msg bus.Message) []sysField {
	return []sysField{
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
}

func (m Model) systemLines(msg bus.Message, width int) ([]string, []int) {
	const labelW = 18
	var out []string
	var rows []int
	for i, f := range m.systemFields(msg) {
		label := f.label
		if f.editable {
			label += " ✎"
		}
		val := seg{f.value, stPlain}
		if f.value == "" {
			val = seg{"—", stDim}
		}
		mark, sel := m.cursorMark(i)
		rows = append(rows, len(out))
		out = append(out, row([]seg{{mark + fitPlain(label, labelW), stDim}, {" ", stPlain}, val}, width, sel))
	}
	return out, rows
}

// --- options bar -------------------------------------------------------------

func (m Model) renderOptions() string {
	var right []seg
	if m.opts.ReadOnly {
		right = []seg{{"READ-ONLY", stReadOnly}, {" ", stPlain}}
	}
	rw := segsWidth(right)

	left := []seg{{" ", stPlain}}
	if m.status.text != "" && m.stack.AtRoot() {
		// The status bar: an outcome or a refusal, until the next key.
		left = append(left, seg{m.status.text, levelStyle(m.status.level)})
		l := row(left, max(0, m.width-rw-1), false)
		r, _ := renderSegs(right, rw, nil)
		return l + " " + r
	}
	cleanup := false
	if msg, ok := m.selectedMessage(); ok {
		mark, marked := m.markOf(msg.SequenceNumber)
		cleanup = marked && mark.outcome == bus.CleanupPending
	}
	repair := !m.opts.ReadOnly && m.subQueue == bus.DeadLetter
	bindings := optionsBindings(m.stack.Top(), repair, cleanup && repair)
	if m.stack.Top() == CtxFilter || m.stack.Top() == CtxJump {
		bindings = contextBindings(m.stack.Top())
	}
	if m.stack.Top() == CtxConfirm && m.confirm.kind == confirmRepair && !m.confirm.uncertain {
		bindings = append(bindings, keys.ToggleID)
	}
	for i, b := range bindings {
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
	w, _, n := m.helpBox()
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

	return m.overlay(screen, lines, w)
}

// overlay draws a popup of width w, centred over the screen above the
// options bar.
func (m Model) overlay(screen string, lines []string, w int) string {
	x := (m.width - w) / 2
	y := max(0, (m.height-1-len(lines))/2)
	base := lipgloss.NewLayer(screen)
	popup := lipgloss.NewLayer(strings.Join(lines, "\n")).X(x).Y(y).Z(1)
	// The compositor trims trailing blanks; pad back to full width.
	out := strings.Split(lipgloss.NewCompositor(base, popup).Render(), "\n")
	for i, l := range out {
		out[i] = fitStyled(l, m.width)
	}
	return strings.Join(out, "\n")
}
