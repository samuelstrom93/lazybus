package ui

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/key"
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
	l.leftW = max(26, min(48, w/3))
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
	case CtxEdit:
		screen = m.overlayEdit(screen)
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
	digits, actDigits := 1, 1
	for _, e := range m.entities.all {
		if e.CountsKnown {
			digits = max(digits, len(strconv.FormatInt(e.DeadLetterCount, 10)))
			actDigits = max(actDigits, len(strconv.FormatInt(e.ActiveCount, 10)))
		}
	}
	// cursor(2) path " " ["act " N " "] "DLQ " N. The active column gives
	// way when it would cut the path below minEntityPathW (80×24), or cut
	// any path that fits without it. Decided over all rows, so the columns
	// stay aligned.
	countW := len("DLQ ") + digits
	actW := len("act ") + actDigits + 1
	fullW := width - 2 - 1 - countW
	pathW := fullW - actW
	showActive := pathW >= minEntityPathW
	for _, e := range m.entities.all {
		if pw := ansi.StringWidth(sanitize(e.Path)); pw > pathW && pw <= fullW {
			showActive = false
			break
		}
	}
	if !showActive {
		pathW = fullW
	}
	pathW = max(1, pathW)
	return listRows(m.entities, m.stack.Root() == CtxEntities, width, n,
		listStatus(m.entities, empty),
		func(e bus.Entity) []seg {
			segs := []seg{{fitPlain(e.Path, pathW) + " ", stPlain}}
			if showActive {
				text, st := countText(e, e.ActiveCount, stPlain)
				segs = append(segs, seg{"act " + padLeft(text, actDigits) + " ", st})
			}
			text, st := countText(e, e.DeadLetterCount, stWarn)
			return append(segs, seg{"DLQ " + padLeft(text, digits), st})
		})
}

// minEntityPathW is the narrowest path column the Entities panel keeps
// before it drops the active count.
const minEntityPathW = 12

// countText is a runtime count of e and its style: nonzero in st, 0 and
// unknown dimmed. Unknown counts (emulator, no Manage rights) show "?",
// never a made-up 0.
func countText(e bus.Entity, n int64, st lipgloss.Style) (string, lipgloss.Style) {
	if !e.CountsKnown {
		return "?", stDim
	}
	if n == 0 {
		st = stDim
	}
	return strconv.FormatInt(n, 10), st
}

func (m Model) messageRows(width, n int) []string {
	empty := "no messages"
	if m.openEntity == nil {
		empty = "open an entity"
	} else if m.subQueue == bus.DeadLetter {
		empty = "dead-letter queue is empty"
	}
	digits := max(1, m.msgCols.digits)
	reasonMax := m.msgCols.dlqReason
	if m.subQueue == bus.Active {
		reasonMax = m.msgCols.subject
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

// msgCols are the widest values of the message list's columns: the
// sequence number's digits and the sanitized reason for both
// sub-queues, so a sub-queue switch needs no new measure.
type msgCols struct {
	digits    int
	subject   int // reason column on the Active sub-queue
	dlqReason int // reason column on the dead-letter sub-queue
}

// add returns c widened to fit msgs.
func (c msgCols) add(msgs []bus.Message) msgCols {
	for _, msg := range msgs {
		c.digits = max(c.digits, len(strconv.FormatInt(msg.SequenceNumber, 10)))
		c.subject = max(c.subject, ansi.StringWidth(sanitize(msg.Subject)))
		c.dlqReason = max(c.dlqReason, ansi.StringWidth(sanitize(msg.DeadLetterReason)))
	}
	return c
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
	rev      int // Pending Edits revision: an edit redraws the body
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
		key = linesKey{m.openNS.FQDN, m.openEntity.Path, m.openEntity.Kind, m.subQueue, msg.SequenceNumber, width, m.editsOf(msg.SequenceNumber).rev}
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

// bodyLines is the Body tab: wrapped, not cut. A pending body edit is
// shown instead of the original, under an (edited) marker, with a flag
// when JSON is expected and it is not valid JSON.
func (m Model) bodyLines(msg bus.Message, width int) []string {
	body := msg.Body
	var out []string
	if ed := m.editsOf(msg.SequenceNumber); ed.BodyEdited {
		body = ed.Body
		head := []seg{{" (edited)", stWarn}, {" pending body, sent on resubmit · E edits it again · x discards all edits", stDim}}
		out = append(out, row(head, width, false))
		if why := bodyJSONError(msg, ed.Edits); why != "" {
			for i, l := range wrapLines("invalid JSON: "+sanitize(why)+" (kept as the pending edit)", width-4) {
				prefix := "   "
				if i == 0 {
					prefix = " ! "
				}
				out = append(out, row([]seg{{prefix + l, stErr}}, width, false))
			}
		}
		out = append(out, "")
	}
	text, _ := bodyText(body)
	if text == "" {
		return append(out, " "+stDim.Render("(empty body)"))
	}
	for _, l := range wrapLines(text, width-2) {
		out = append(out, " "+l)
	}
	return out
}

const removedOnResubmit = "✕ removed on resubmit"

// pendMark is a row's Pending Edit marker (spec §4).
type pendMark int

const (
	pendNone    pendMark = iota
	pendChanged          // *
	pendAdded            // +
	pendRemoved          // −
)

func (p pendMark) seg() seg {
	switch p {
	case pendChanged:
		return seg{"*", stWarn}
	case pendAdded:
		return seg{"+", stTitleFocus}
	case pendRemoved:
		return seg{"−", stErr}
	}
	return seg{" ", stPlain}
}

// propItem is one row of the Properties tab. raw is the value y copies;
// rawKey the unsanitized key.
type propItem struct {
	key, typ, val, raw string
	rawKey             string
	marker             bool
	pend               pendMark
	was                string // a changed row: the original value, with its type when that changed
}

// propertyItems are the application properties with their Pending Edits
// (changed and removed in place, added after them), then the Dead-letter
// Markers, in display order.
func (m Model) propertyItems(msg bus.Message) []propItem {
	ed := m.editsOf(msg.SequenceNumber)
	var props []propItem
	item := func(p bus.Property) propItem {
		raw := propertyValue(p, m.opts.Location)
		return propItem{key: sanitize(p.Key), typ: p.Type.String(), val: sanitize(raw), raw: raw, rawKey: p.Key}
	}
	for _, p := range msg.Properties {
		it := item(p)
		if pe, ok := ed.Property(p.Key); ok {
			if pe.Remove {
				it.pend = pendRemoved
			} else {
				was := it.val
				if pe.Type != p.Type {
					was = it.typ + " " + was
				}
				it = item(bus.Property{Key: pe.Key, Type: pe.Type, Value: pe.Value})
				it.pend, it.was = pendChanged, was
			}
		}
		props = append(props, it)
	}
	for _, pe := range ed.Properties {
		if _, ok := findProp(msg.Properties, pe.Key); !ok && !pe.Remove {
			it := item(bus.Property{Key: pe.Key, Type: pe.Type, Value: pe.Value})
			it.pend = pendAdded
			props = append(props, it)
		}
	}
	if msg.DeadLetterReason != "" {
		props = append(props, propItem{bus.MarkerDeadLetterReason, bus.TypeString.String(), sanitize(msg.DeadLetterReason), msg.DeadLetterReason, bus.MarkerDeadLetterReason, true, pendNone, ""})
	}
	if msg.DeadLetterErrorDescription != "" {
		props = append(props, propItem{bus.MarkerDeadLetterErrorDescription, bus.TypeString.String(), sanitize(msg.DeadLetterErrorDescription), msg.DeadLetterErrorDescription, bus.MarkerDeadLetterErrorDescription, true, pendNone, ""})
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

	// Each row: cursor, Pending Edit marker, key, type, value. The marker
	// column is there only while the message has Pending Edits, so the
	// values keep their full width otherwise. Markers go in their own
	// dimmed section under one "✕ removed on resubmit" label, so their
	// values (often the only error text there is) keep the full column
	// instead of sharing it with a per-row suffix.
	pad := " "
	for _, p := range props {
		if p.pend != pendNone {
			pad = "  "
			break
		}
	}
	out := []string{row([]seg{{pad + fitPlain("Key", keyW) + "  " + fitPlain("Type", typeW) + "  Value", stDim}}, width, false)}
	rows := make([]int, 0, len(props))
	markerHeader := false
	for i, p := range props {
		mark, sel := m.cursorMark(i)
		if !p.marker {
			rows = append(rows, len(out))
			keySt, valSt := stPlain, stPlain
			if p.pend == pendRemoved {
				keySt, valSt = stDim, stDim
			}
			segs := []seg{{mark, stPlain}}
			if pad != " " {
				segs = append(segs, p.pend.seg())
			}
			segs = append(segs,
				seg{fitPlain(p.key, keyW) + "  ", keySt},
				seg{fitPlain(p.typ, typeW) + "  ", stDim},
				seg{p.val, valSt},
			)
			switch p.pend {
			case pendChanged:
				segs = append(segs, seg{"  (was " + p.was + ")", stDim})
			case pendRemoved:
				segs = append(segs, seg{"  " + removedOnResubmit, stDim})
			}
			out = append(out, row(segs, width, sel))
			continue
		}
		if !markerHeader {
			markerHeader = true
			out = append(out, "", row([]seg{{pad + "Dead-letter markers  " + removedOnResubmit, stDim}}, width, false))
		}
		rows = append(rows, len(out))
		out = append(out, row([]seg{
			{mark + pad[1:] + fitPlain(p.key, keyW) + "  " + fitPlain(p.typ, typeW) + "  " + p.val, stDim},
		}, width, sel))
	}
	return out, rows
}

// sysField is one row of the System tab. value is what the copy gets
// (the pending value when edited); was the original of an edited field.
type sysField struct {
	label, value string
	editable     bool
	edited       bool
	was          string
}

func (m Model) systemFields(msg bus.Message) []sysField {
	ed := m.editsOf(msg.SequenceNumber)
	editable := func(label, orig string, cur *string) sysField {
		f := sysField{label: label, value: orig, editable: true}
		if cur != nil {
			f.value, f.edited, f.was = *cur, true, orig
		}
		return f
	}
	return []sysField{
		{label: "MessageId", value: msg.MessageID},
		{label: "CorrelationId", value: msg.CorrelationID},
		editable("Subject", msg.Subject, ed.Subject),
		editable("ContentType", msg.ContentType, ed.ContentType),
		{label: "SessionId", value: msg.SessionID},
		{label: "PartitionKey", value: msg.PartitionKey},
		{label: "To", value: msg.To},
		{label: "ReplyTo", value: msg.ReplyTo},
		{label: "TTL", value: ttlText(msg.TimeToLive)},
		{label: "DeliveryCount", value: strconv.FormatUint(uint64(msg.DeliveryCount), 10)},
		{label: "EnqueuedTime", value: fullTime(msg.EnqueuedTime, m.opts.Location)},
		{label: "SequenceNumber", value: strconv.FormatInt(msg.SequenceNumber, 10)},
		{label: "DeadLetterSource", value: msg.DeadLetterSource},
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
		pend := pendNone
		if f.edited {
			pend = pendChanged
		}
		mark, sel := m.cursorMark(i)
		// The marker takes the label's first cell: the labels are shorter
		// than labelW, so the values do not move.
		segs := []seg{{mark, stPlain}, pend.seg(), {fitPlain(label, labelW-1), stDim}, {" ", stPlain}, val}
		if f.edited {
			segs = append(segs, seg{"  (was " + orDash(f.was) + ")", stDim})
		}
		rows = append(rows, len(out))
		out = append(out, row(segs, width, sel))
	}
	return out, rows
}

// --- options bar -------------------------------------------------------------

func (m Model) renderOptions() string {
	var right []seg
	if n := m.pendingCount(); n > 0 {
		// Pending Edits live in memory only: the flag says how many
		// messages would lose theirs on quit.
		right = append(right, seg{fmt.Sprintf("✎ %d pending", n), stWarn}, seg{" ", stPlain})
	}
	if m.opts.ReadOnly {
		right = append(right, seg{"READ-ONLY", stReadOnly}, seg{" ", stPlain})
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
	f := optionFlags{tab: m.tab}
	if msg, ok := m.selectedMessage(); ok && m.subQueue == bus.DeadLetter {
		mark, marked := m.markOf(msg.SequenceNumber)
		f.cleanup = marked && mark.outcome == bus.CleanupPending
		f.edit = true
		f.discard = !m.editsOf(msg.SequenceNumber).IsZero()
	}
	f.repair = !m.opts.ReadOnly && m.subQueue == bus.DeadLetter
	f.cleanup = f.cleanup && f.repair
	bindings := optionsBindings(m.stack.Top(), f)
	if m.stack.Top() == CtxFilter || m.stack.Top() == CtxJump {
		bindings = contextBindings(m.stack.Top())
	}
	if m.stack.Top() == CtxConfirm && m.confirm.kind == confirmRepair && !m.confirm.uncertain {
		bindings = append(bindings, keys.ToggleID)
	}
	bindings = fitBindings(bindings, max(0, m.width-rw-2))
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

// optionsDropOrder are the options bar keys left out first when the bar
// is too narrow, so the context's own keys and ? / q stay visible.
var optionsDropOrder = []key.Help{
	keys.FocusMainOp.Help(), keys.Jump.Help(), keys.Filter.Help(), {Key: "[ ]", Desc: "tab"}, keys.SubQueue.Help(),
}

// fitBindings drops bindings in optionsDropOrder until they fit in width
// cells; what still does not fit is cut by the row.
func fitBindings(bs []key.Binding, width int) []key.Binding {
	size := func(bs []key.Binding) int {
		w := 0
		for i, b := range bs {
			h := b.Help()
			if i > 0 {
				w += 2
			}
			w += ansi.StringWidth(h.Key) + 1 + ansi.StringWidth(h.Desc)
		}
		return w
	}
	for _, drop := range optionsDropOrder {
		if size(bs) <= width {
			break
		}
		bs = slices.DeleteFunc(slices.Clone(bs), func(b key.Binding) bool { return b.Help() == drop })
	}
	return bs
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
		case entries[j].key == "":
			r = row([]seg{{"   " + entries[j].desc, stDim}}, inner, false)
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
