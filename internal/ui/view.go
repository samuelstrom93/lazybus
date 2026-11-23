package ui

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

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
	if m.zoomed() {
		l.leftW = 0
	}
	l.mainW = w - l.leftW
	l.logRows = max(2, min(5, frameH/7))
	l.mainRows = max(1, frameH-3-l.logRows)

	// Side panels share borders: top, two separators, bottom.
	counts := [3]int{len(m.namespaces.items), len(m.entities.items), len(m.messages.items)}
	l.side = sideRows(max(3, frameH-4), counts, int(m.focusedSide()-CtxNamespaces))
	return l
}

// sideRows splits total rows between the three side panels with counts
// items. A collapsed panel gets up to a quarter, the expanded one up to
// its content; rows left over show more of the other panels, Messages
// first, and what is still left pads Messages. Every panel keeps a row.
func sideRows(total int, counts [3]int, expanded int) [3]int {
	var side [3]int
	unit := max(1, total/4)
	rest := total
	for i := range side {
		if i != expanded {
			side[i] = max(1, min(counts[i], unit))
			rest -= side[i]
		}
	}
	side[expanded] = max(1, min(counts[expanded], rest))
	rest -= side[expanded]
	for _, i := range []int{2, 1, 0} {
		if i != expanded {
			grow := max(0, min(rest, counts[i]-side[i]))
			side[i] += grow
			rest -= grow
		}
	}
	side[2] += rest
	return side
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
	right := m.renderMain(l)
	lines := make([]string, 0, m.height)
	if m.zoomed() {
		lines = append(lines, right...)
	} else {
		left := m.renderSide(l)
		for i := range left {
			lines = append(lines, left[i]+right[i])
		}
	}
	lines = append(lines, m.renderOptions())
	screen := strings.Join(lines, "\n")
	switch m.stack.Top() {
	case CtxHelp:
		screen = m.overlayHelp(screen)
	case CtxJump:
		screen = m.overlayJump(screen)
	case CtxRecent:
		screen = m.overlayRecent(screen)
	case CtxEdit:
		screen = m.overlayEdit(screen)
	case CtxConfirm:
		screen = m.overlayConfirm(screen)
	case CtxBusy:
		screen = m.overlayBusy(screen)
	}
	return screen
}

// zoomed reports whether the main pane takes the full width: + while the
// main pane is focused.
func (m Model) zoomed() bool {
	return m.zoom && m.stack.Root() == CtxMain
}

// panelFocused reports whether panel c currently owns input (or is hosting
// inline list filtering / main-pane search). When a modal popup is open, no
// background panel is focused so only the popup gets the green focus border.
func (m Model) panelFocused(c ContextID) bool {
	top := m.stack.Top()
	return m.stack.Root() == c && (top == c || top == CtxFilter || top == CtxSearch)
}

// --- side panels -----------------------------------------------------------

func (m Model) renderSide(l layout) []string {
	focus := ContextID(-1)
	if m.panelFocused(m.stack.Root()) {
		focus = m.stack.Root()
	}
	inner := l.leftW - 2
	panels := []struct {
		ctx   ContextID
		title []seg
		rows  []string
	}{
		{CtxNamespaces, m.sideTitle(CtxNamespaces, "1 Namespaces", m.namespaces.loading, m.namespaces.filter, len(m.namespaces.items), len(m.namespaces.all)), m.namespaceRows(inner, l.side[0])},
		{CtxEntities, m.entitiesTitle(), m.entityRows(inner, l.side[1])},
		{CtxMessages, m.messagesTitle(inner - 2), m.messageRows(inner, l.side[2])},
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
	if m.panelFocused(c) {
		return stTitleFocus
	}
	return stPlain
}

func (m Model) sideTitle(c ContextID, name string, loading bool, filter string, shown, total int) []seg {
	t := []seg{{name, m.titleStyle(c)}}
	if loading {
		t = append(t, seg{" loading…", stDim})
	}
	return append(t, m.filterTitle(c, filter, shown, total)...)
}

// filterTitle is the filter part of a panel title: " /text shown/total"
// while it applies, with a cursor while it is being typed.
func (m Model) filterTitle(c ContextID, filter string, shown, total int) []seg {
	typing := m.stack.Top() == CtxFilter && m.stack.Root() == c
	if filter == "" && !typing {
		return nil
	}
	t := []seg{{" /" + filter, stKey}}
	if typing {
		t = append(t, seg{"▏", stTitleFocus})
	}
	if filter != "" && total > 0 {
		t = append(t, seg{fmt.Sprintf(" %d/%d", shown, total), stDim})
	}
	return t
}

func (m Model) entitiesTitle() []seg {
	t := m.sideTitle(CtxEntities, "2 Entities", m.entities.loading, "", 0, 0)
	if m.sortDLQ {
		t = append(t, seg{" by DLQ", stDim})
	}
	return append(t, m.filterTitle(CtxEntities, m.entities.filter, len(m.entities.items), len(m.entities.all))...)
}

func (m Model) messagesTitle(maxW int) []seg {
	active := m.titleStyle(CtxMessages)
	if !m.panelFocused(CtxMessages) {
		active = stBold
	}
	dlq, act := active, stDim
	if m.subQueue == bus.Active {
		dlq, act = stDim, active
	}
	t := []seg{{"3 Messages", m.titleStyle(CtxMessages)}, {"  ", stPlain}, {"DLQ", dlq}, {"│", stDim}, {"Active", act}}
	t = append(t, m.messagesStatusSegs(false)...)
	t = append(t, m.messagesPickSegs(false)...)
	t = append(t, m.filterTitle(CtxMessages, m.messages.filter, len(m.messages.items), len(m.messages.all))...)
	if segsWidth(t) <= maxW {
		return t
	}
	return m.compactMessagesTitle(maxW, active)
}

func (m Model) messagesStatusSegs(short bool) []seg {
	switch {
	case m.messages.loading:
		if short {
			return []seg{{" …", stDim}}
		}
		return []seg{{" loading…", stDim}}
	case m.messages.err != nil && len(m.messages.items) > 0:
		// A failed next page: the rows stay, the title says so.
		if short {
			return []seg{{" err", stErr}}
		}
		return []seg{{" error", stErr}}
	}
	return nil
}

func (m Model) messagesPickSegs(short bool) []seg {
	n := len(m.pickedSeqs())
	if n == 0 && !m.picks.ranging {
		return nil
	}
	if short {
		text := fmt.Sprintf(" %d●", n)
		if m.picks.ranging {
			text += "v"
		}
		return []seg{{text, stKey}}
	}
	text := fmt.Sprintf(" %d picked", n)
	if m.picks.ranging {
		text += " (v range)"
	}
	return []seg{{text, stKey}}
}

// compactMessagesTitle keeps picks and filter visible on narrow screens
// (80×24) by dropping the inactive sub-queue label, match count, and full
// wording before truncating active state.
func (m Model) compactMessagesTitle(maxW int, active lipgloss.Style) []seg {
	sub := "DLQ"
	if m.subQueue == bus.Active {
		sub = "Act"
	}
	filterShort := m.filterTitle(CtxMessages, m.messages.filter, 0, 0)
	status := m.messagesStatusSegs(true)
	picks := m.messagesPickSegs(true)
	titleSt := m.titleStyle(CtxMessages)
	candidates := [][]seg{
		append(append(append([]seg{{"3 Messages", titleSt}, {" ", stPlain}, {sub, active}}, status...), picks...), filterShort...),
		append(append(append([]seg{{"3 Messages", titleSt}}, status...), picks...), filterShort...),
		append(append(append([]seg{{"3 Msg", titleSt}, {" ", stPlain}, {sub, active}}, status...), picks...), filterShort...),
		append(append(append([]seg{{"3 Msg", titleSt}}, status...), picks...), filterShort...),
	}
	for _, c := range candidates {
		if segsWidth(c) <= maxW {
			return c
		}
	}
	return candidates[len(candidates)-1]
}

// listRows renders n rows of a list with the default selection highlight.
func listRows[T any](l list[T], focused bool, width, n int, status []seg, picked func(int) bool, render func(T) []seg) []string {
	return listRowsStyled(l, focused, width, n, status, picked, nil, render)
}

// listRowsStyled renders n rows of a list: the visible window from offset,
// the cursor row marked, picked rows (picked, nil for none) marked ●, empty
// rows padded. selStyle, when non-nil, chooses the background of a selected
// focused row. status replaces the rows when the list has nothing to show.
func listRowsStyled[T any](l list[T], focused bool, width, n int, status []seg, picked func(int) bool, selStyle func(T) *lipgloss.Style, render func(T) []seg) []string {
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
		segs := []seg{{mark, stPlain}}
		if picked != nil && picked(i) {
			segs = []seg{{mark[:len(mark)-1], stPlain}, {"●", stKey}}
		}
		segs = append(segs, render(l.items[i])...)
		var bg *lipgloss.Style
		if cursor && focused {
			bg = &stSelected
			if selStyle != nil {
				bg = selStyle(l.items[i])
			}
		}
		out = append(out, rowBG(segs, width, bg))
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
// entity: not a failure, a v0.1 limit (spec §4 panel 3). Other
// not-allowed refusals (an auto-forwarding entity, say) are errors.
func (m Model) isSessionful(err error) bool {
	return m.subQueue == bus.Active && bus.KindOf(err) == bus.ErrNotAllowed && shortErr(err) == bus.SessionfulActive
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
	return listRows(m.namespaces, m.panelFocused(CtxNamespaces), width, n,
		listStatus(m.namespaces, "no namespaces"), nil,
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
	return listRows(m.entities, m.panelFocused(CtxEntities), width, n,
		listStatus(m.entities, empty), nil,
		func(e bus.Entity) []seg {
			segs := []seg{{fitEntityPath(e.Path, pathW) + " ", stPlain}}
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
	seqW, showTime, reasonW, idW := messageRowLayout(width, digits, reasonMax)
	now := m.opts.Now()
	status := listStatus(m.messages, empty)
	if m.isSessionful(m.messages.err) {
		status = []seg{{shortErr(m.messages.err), stWarn}}
	}
	return listRowsStyled(m.messages, m.panelFocused(CtxMessages), width, n,
		status, m.isPicked, m.messageSelStyle,
		func(msg bus.Message) []seg {
			return m.messageRowSegs(msg, seqW, showTime, reasonW, idW, now)
		})
}

// messageRowLayout computes column widths for the Messages list. When the
// remaining reason column would be narrower than "CleanupPending" (14 cells),
// the 5-cell time column drops first; if long sequence numbers (e.g. 17-digit
// partitioned entities) still leave under 14 cells, the seq column is
// abbreviated to its trailing digits so reasons and outcomes stay legible.
func messageRowLayout(width, digits, reasonMax int) (seqW int, showTime bool, reasonW, idW int) {
	minOutcomeW := len(bus.CleanupPending.String())
	seqW = digits
	rest := width - 2 - seqW - 1 - 5 - 1
	showTime = rest >= minOutcomeW
	if !showTime {
		avail := width - 2
		if avail-seqW-1 < minOutcomeW && avail >= minOutcomeW+3 {
			seqW = max(2, avail-1-minOutcomeW)
		}
		rest = max(1, avail-seqW-1)
	}
	reasonW = min(reasonMax, rest)
	idW = rest - reasonW - 1
	if idW < minMsgIDW {
		reasonW, idW = rest, 0
	}
	return seqW, showTime, reasonW, idW
}

func formatSeq(seq int64, seqW int) string {
	raw := strconv.FormatInt(seq, 10)
	if len(raw) > seqW && seqW >= 2 {
		return "…" + raw[len(raw)-(seqW-1):]
	}
	return padLeft(raw, seqW)
}

func (m Model) messageSelStyle(msg bus.Message) *lipgloss.Style {
	if mark, ok := m.markOf(msg.SequenceNumber); ok {
		if mark.outcome == bus.SendUncertain {
			return &stSelectedWarn
		}
		return &stSelectedErr
	}
	return &stSelected
}

func (m Model) messageRowSegs(msg bus.Message, seqW int, showTime bool, reasonW, idW int, now time.Time) []seg {
	seq := formatSeq(msg.SequenceNumber, seqW) + " "
	at := ""
	if showTime {
		at = listTime(msg.EnqueuedTime, now, m.opts.Location) + " "
	}
	if mark, ok := m.markOf(msg.SequenceNumber); ok {
		return m.markedMessageSegs(msg, mark, seq, at, reasonW, idW)
	}
	segs := []seg{{seq, stPlain}}
	if at != "" {
		segs = append(segs, seg{at, stDim})
	}
	segs = append(segs, seg{fitPlain(m.rowReason(msg), reasonW), stPlain})
	if idW > 0 {
		segs = append(segs, seg{" " + msg.MessageID, stDim})
	}
	return segs
}

// markedMessageSegs renders a SendUncertain or CleanupPending row, keeping
// the full outcome name before appending the reason.
func (m Model) markedMessageSegs(msg bus.Message, mark rowMark, seq, at string, reasonW, idW int) []seg {
	st := levelStyle(markStyleOf(mark.outcome))
	labelW := reasonW
	if idW > 0 {
		labelW += 1 + idW
	}
	label := mark.outcome.String()
	if reason := m.rowReason(msg); reason != "" && labelW > len(label)+3 {
		label += " · " + reason
	}
	segs := []seg{{seq, st}}
	if at != "" {
		segs = append(segs, seg{at, st})
	}
	return append(segs, seg{fitPlain(label, labelW), st})
}

// minMsgIDW is the narrowest MessageId column the Messages panel shows.
const minMsgIDW = 4

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
	focused := m.panelFocused(CtxMain)
	edge := stBorder
	if focused {
		edge = stBorderFocus
	}
	inner := l.mainW - 2

	tabs := m.tabBar(focused)
	lines, scroll := m.mainLines(inner), m.mainScroll
	if m.stack.Root() == CtxNamespaces {
		// Namespaces focused: the main pane shows the selected namespace.
		var title string
		title, lines = m.namespaceDetails(inner)
		tabs, scroll = []seg{{title, stBold}}, 0
	} else {
		tabs = append(tabs, m.titleTail(m.searchTitle(lines), segsWidth(tabs), inner-2)...)
	}

	out := []string{hBorder(l.mainW, "┌", "┐", tabs, edge)}
	for i := range l.mainRows {
		var s string
		if j := scroll + i; j < len(lines) {
			s = lines[j]
			if m.search.term != "" && m.stack.Root() != CtxNamespaces {
				s = highlight(s, m.search.term, j == m.search.line, inner)
			}
		}
		out = append(out, boxRow(fitStyled(s, inner), edge))
	}
	out = append(out, hBorder(l.mainW, "├", "┤", []seg{{"Log", stPlain}}, edge))
	for _, r := range m.logLines(l.logRows, inner) {
		out = append(out, boxRow(r, stBorder))
	}
	return append(out, hBorder(l.mainW, "└", "┘", nil, stBorder))
}

// tabBar is the main pane's tab bar, the current tab highlighted.
func (m Model) tabBar(focused bool) []seg {
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
	return tabs
}

// titleTail is the main pane title after the tabs: the search (its
// segments) and a dim identity of the message, " · orders/$DLQ seq 2 ·
// ord-4002". used cells of the width are taken. "seq N" keeps its room
// while at least two cells are left for the search and the path; the
// MessageId goes first when the rest does not fit, then the entity path
// and the search are cut, each to at least half of what is left.
func (m Model) titleTail(search []seg, used, width int) []seg {
	msg, ok := m.selectedMessage()
	if !ok || m.openEntity == nil {
		return search
	}
	const sep = " · "
	label := sanitize(entityLabel(*m.openEntity, m.subQueue))
	seq := fmt.Sprintf(" seq %d", msg.SequenceNumber)
	rest := width - used - ansi.StringWidth(sep) - len(seq) // for the search and the path
	sw, lw := segsWidth(search), ansi.StringWidth(label)
	if id := sep + sanitize(msg.MessageID); msg.MessageID != "" && sw+lw+ansi.StringWidth(id) <= rest {
		return append(search, seg{sep + label + seq + id, stDim})
	}
	if sw+lw > rest && rest > 1 {
		if keep := max(1, rest/2, rest-sw); keep < lw {
			label = ansi.Truncate(label, keep, "…")
		}
		search = cutSegs(search, rest-ansi.StringWidth(label))
	}
	return append(search, seg{sep + label + seq, stDim})
}

// cutSegs cuts segs to width cells, the last one kept ending in "…".
func cutSegs(segs []seg, width int) []seg {
	if segsWidth(segs) <= width {
		return segs
	}
	var out []seg
	for _, s := range segs {
		w := ansi.StringWidth(s.text)
		if w < width {
			out = append(out, s)
			width -= w
			continue
		}
		if width > 0 {
			out = append(out, seg{ansi.Truncate(s.text, width-1, "") + "…", s.style})
		}
		break
	}
	return out
}

// logLines are the last n log entries, one per row of width cells, blank
// rows below when there are fewer.
func (m Model) logLines(n, width int) []string {
	out := make([]string, n)
	start := max(0, len(m.log)-n)
	for i := range n {
		if j := start + i; j < len(m.log) {
			e := m.log[j]
			out[i] = row([]seg{{" " + e.at.In(m.opts.Location).Format("15:04") + " ", stDim}, {e.text, levelStyle(e.level)}}, width, false)
		} else {
			out[i] = strings.Repeat(" ", width)
		}
	}
	return out
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
	if banner := m.bannerLines(msg, width); banner != nil {
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
		head := []seg{{" (edited)", stWarn}, {" pending body · E edit · x discard", stDim}}
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

// stripCache holds the failure banner for one (reason, description,
// width), all that failureLines reads, so it never goes stale.
type stripCache struct {
	key   stripKey
	lines []string
}

type stripKey struct {
	reason, desc string
	width        int
}

// failureBanner is failureLines(msg, width) and a blank row, or nil, from
// the cache when it matches. Callers must not write its elements; its
// capacity is its length, so appending to it never writes into the cache.
// The zero key needs no flag: its strip is nil either way.
func (m Model) failureBanner(msg bus.Message, width int) []string {
	key := stripKey{msg.DeadLetterReason, msg.DeadLetterErrorDescription, width}
	if m.strip != nil && m.strip.key == key {
		return m.strip.lines
	}
	lines := failureLines(msg, width)
	if lines != nil {
		lines = slices.Clip(append(lines, ""))
	}
	if m.strip != nil {
		*m.strip = stripCache{key: key, lines: lines}
	}
	return lines
}

// failureMaxLines caps the failure strip; a longer description is cut
// with "…" (the Properties tab and y have it in full).
const failureMaxLines = 6

// failureLines are the failure strip of a message with Dead-letter
// Markers: the DeadLetterReason in warning colour, then the
// DeadLetterErrorDescription, wrapped to width; only past failureMaxLines
// is it cut.
func failureLines(msg bus.Message, width int) []string {
	reason := strings.TrimSpace(sanitize(msg.DeadLetterReason))
	desc := strings.TrimSpace(sanitize(msg.DeadLetterErrorDescription))
	if reason == "" && desc == "" {
		return nil
	}
	w := width - 2
	// The reason leads the first line and the description follows on it;
	// wrapping keeps a line's start, so a reason that fits stays whole. A
	// wider reason gets its own lines.
	var lines []string
	reasonRows := 0 // leading lines that are all reason
	if ansi.StringWidth(reason) <= w {
		lines = wrapLines(strings.TrimSpace(reason+"  "+desc), w)
	} else {
		lines = wrapLines(reason, w)
		reasonRows = len(lines)
		if desc != "" {
			lines = append(lines, wrapLines(desc, w)...)
		}
	}
	if len(lines) > failureMaxLines {
		lines = lines[:failureMaxLines]
		last := lines[len(lines)-1]
		if ansi.StringWidth(last) >= w {
			last = ansi.Truncate(last, w-1, "")
		}
		lines[len(lines)-1] = last + "…"
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		segs := []seg{{" " + l, stPlain}}
		switch {
		case i < reasonRows:
			segs = []seg{{" " + l, stFailure}}
		case i == 0 && reason != "" && strings.HasPrefix(l, reason):
			segs = []seg{{" " + reason, stFailure}, {l[len(reason):], stPlain}}
		}
		out[i] = row(segs, width, false)
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
	if m.panelFocused(CtxMain) {
		return " ", true
	}
	return "▸", false
}

// propKeyWidths returns the key column widths for regular properties and
// dead-letter markers independently, so 26-char DeadLetterErrorDescription
// does not bloat the application-property key column.
func propKeyWidths(props []propItem, width int) (keyW, markerKeyW int) {
	keyW = len("Key")
	for _, p := range props {
		w := ansi.StringWidth(p.key)
		if p.marker {
			markerKeyW = max(markerKeyW, w)
		} else {
			keyW = max(keyW, w)
		}
	}
	keyW = min(keyW, max(8, width/4))
	markerKeyW = min(markerKeyW, max(14, width/3))
	return keyW, markerKeyW
}

func (m Model) propertyLines(msg bus.Message, width int) ([]string, []int) {
	props := m.propertyItems(msg)
	if len(props) == 0 {
		return []string{" " + stDim.Render("(no application properties)")}, nil
	}
	keyW, markerKeyW := propKeyWidths(props, width)
	typeW := len("DateTime")

	// Reserve the 2-cell cursor+marker gutter consistently (matching the
	// System tab) so columns stay aligned when edits are added.
	const pad = "  "
	out := []string{row([]seg{{pad + fitPlain("Key", keyW) + "  " + fitPlain("Type", typeW) + "  Value", stDim}}, width, false)}
	rows := make([]int, 0, len(props))
	markerHeader := false
	for i, p := range props {
		mark, sel := m.cursorMark(i)
		if !p.marker {
			rows = append(rows, len(out))
			out = append(out, propertyRow(p, mark, keyW, typeW, width, sel))
			continue
		}
		if !markerHeader {
			markerHeader = true
			out = append(out, "", row([]seg{{pad + "Dead-letter markers  " + removedOnResubmit, stDim}}, width, false))
		}
		rows = append(rows, len(out))
		out = append(out, row([]seg{
			{mark + " " + fitPlain(p.key, markerKeyW) + "  ", stDim},
			{p.val, stPlain},
		}, width, sel))
	}
	return out, rows
}

func propertyRow(p propItem, mark string, keyW, typeW, width int, sel bool) string {
	keySt, valSt := stPlain, stPlain
	if p.pend == pendRemoved {
		keySt, valSt = stDim, stDim
	}
	segs := []seg{
		{mark, stPlain},
		p.pend.seg(),
		{fitPlain(p.key, keyW) + "  ", keySt},
		{fitPlain(p.typ, typeW) + "  ", stDim},
		{p.val, valSt},
	}
	switch p.pend {
	case pendChanged:
		segs = append(segs, seg{"  (was " + p.was + ")", stDim})
	case pendRemoved:
		segs = append(segs, seg{"  " + removedOnResubmit, stDim})
	}
	return row(segs, width, sel)
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
		segs := []seg{{mark, stPlain}, pend.seg()}
		if f.editable {
			gap := max(0, labelW-1-ansi.StringWidth(f.label)-2)
			segs = append(segs, seg{f.label, stDim}, seg{" ✎", stKey}, seg{strings.Repeat(" ", gap+1), stPlain}, val)
		} else {
			segs = append(segs, seg{fitPlain(f.label, labelW-1), stDim}, seg{" ", stPlain}, val)
		}
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
		right = append(right, seg{fmt.Sprintf("✎ %d edited msg%s", n, plural(n)), stWarn}, seg{" ", stPlain})
	}
	if m.opts.ReadOnly {
		right = append(right, seg{"READ-ONLY", stReadOnly}, seg{" ", stPlain})
	}
	rw := segsWidth(right)

	left := []seg{{" ", stPlain}}
	// A quit deferred during a call says so while the busy popup is open.
	deferredQuit := m.stack.Top() == CtxBusy && m.quitAfterCall != nil
	if m.status.text != "" && (m.stack.AtRoot() || deferredQuit) {
		// The status bar: an outcome or a refusal, until the next key.
		left = append(left, seg{m.status.text, levelStyle(m.status.level)})
		l := row(left, max(0, m.width-rw-1), false)
		r, _ := renderSegs(right, rw, nil)
		return l + " " + r
	}
	if m.stack.Top() == CtxBusy {
		left = append(left, seg{"working…", stDim})
	}
	bindings := fitBindings(m.optionsBarBindings(), max(0, m.width-rw-2))
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

// optionsBarBindings are the keys the options bar offers for the top
// context and the selected message, before fitting them to the width.
func (m Model) optionsBarBindings() []key.Binding {
	top := m.stack.Top()
	if top == CtxFilter || top == CtxSearch {
		return contextBindings(top)
	}
	return optionsBindings(top, m.barFlags())
}

// barFlags say which of the context's keys apply to the selected
// message.
func (m Model) barFlags() optionFlags {
	f := optionFlags{tab: m.tab, search: m.search.term != ""}
	dlqWrite := !m.opts.ReadOnly && m.subQueue == bus.DeadLetter
	f.pick = dlqWrite
	f.repair = dlqWrite
	if msg, ok := m.selectedMessage(); ok && m.subQueue == bus.DeadLetter {
		mark, marked := m.markOf(msg.SequenceNumber)
		f.cleanup = dlqWrite && marked && mark.outcome == bus.CleanupPending
		// Pending Edits are locked on SendUncertain and CleanupPending rows.
		f.edit = !marked
		f.discard = !marked && !m.editsOf(msg.SequenceNumber).IsZero()
		if marked && mark.outcome == bus.CleanupPending {
			f.repair = false
		}
	}
	if len(m.pickedSeqs()) > 0 || m.picks.ranging {
		// When rows are picked, r operates on the picks rather than the cursor row.
		f.repair = dlqWrite && m.hasEligibleBatch()
	}
	return f
}

// hasEligibleBatch reports whether picked rows exist and none of them are
// marked SendUncertain/CleanupPending.
func (m Model) hasEligibleBatch() bool {
	seqs := m.pickedSeqs()
	if len(seqs) == 0 {
		return false
	}
	for _, seq := range seqs {
		if _, ok := m.markOf(seq); ok {
			return false
		}
	}
	return true
}

// optionsDropOrder are the options bar keys left out first when the bar
// is too narrow, so the context's primary navigation, resubmit, and ? / q
// stay visible: bulk picking and secondary shortcuts first, inspect and
// tab navigation last.
var optionsDropOrder = []key.Help{
	keys.Pick.Help(), keys.Jump.Help(), keys.Filter.Help(), keys.Search.Help(),
	keys.AddPropMsg.Help(), keys.EditBody.Help(),
	keys.FocusMainOp.Help(), keys.SubQueue.Help(), tabsHint.Help(),
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
	w = m.popupW(56)
	entries := len(m.helpEntries())
	h = min(m.popupH(), max(entries, 3)+4)
	return w, h, h - 4
}

func (m Model) overlayHelp(screen string) string {
	w, _, n := m.helpBox()
	inner := w - 2
	entries := m.helpEntries()
	offset := max(0, min(m.help.offset, len(entries)-n))

	hideLast := n > 1 && offset+n-1 < len(entries) && entries[offset+n-1].section != "" && offset+n < len(entries)
	title := []seg{{"Keybindings", stTitleFocus}}
	if len(entries) > n {
		end := min(len(entries), offset+n)
		if hideLast {
			end--
		}
		title = append(title, seg{fmt.Sprintf(" %d–%d/%d", offset+1, end, len(entries)), stDim})
	}
	lines := []string{hBorder(w, "┌", "┐", title, stBorderFocus)}
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
		case i == n-1 && hideLast:
			// Avoid ending the viewport on a dangling section header.
			r = strings.Repeat(" ", inner)
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
	footer := []seg{
		{"↑↓", stKey}, {" scroll  ", stDim},
		{"esc", stKey}, {" close", stDim},
	}
	lines = append(lines, hBorder(w, "└", "┘", footer, stBorderFocus))

	return m.overlay(screen, lines, w)
}

// Popup widths: at most maxPopupW, with a blank cell either side inside
// the popup area. A main pane whose interior cannot hold minPopupW plus
// those cells (under 80 columns) is too narrow to be the popup area.
const (
	maxPopupW = 72
	minPopupW = 50
)

// rect is a screen region in cells.
type rect struct{ x, y, w, h int }

// popupArea is where popups are drawn: the main pane's interior, below
// its title and above the Log rule, so the side panels and the log stay
// in view. Below 80 columns it is the whole screen inside the outer
// borders, side panels included.
func (m Model) popupArea() rect {
	l := m.layout()
	if a := (rect{l.leftW + 1, 1, l.mainW - 2, l.mainRows}); a.w-2 >= minPopupW {
		return a
	}
	// Rows 0 and height-2 are the outer borders; height-1 the options bar.
	return rect{1, 1, m.width - 2, m.height - 3}
}

// popupW is the width of a popup that would like want cells.
func (m Model) popupW(want int) int {
	return min(want, maxPopupW, m.popupArea().w-2)
}

// popupH is the most rows a popup has with a blank row above and below
// it in the popup area.
func (m Model) popupH() int {
	return max(3, m.popupArea().h-2)
}

// overlay draws a popup of width w centred in the popup area, blanking the
// entire popup area so no stray content lines peek out above or below it
// while keeping the side panels, main title/tabs, and Log intact.
func (m Model) overlay(screen string, lines []string, w int) string {
	a := m.popupArea()
	h := len(lines)
	x := a.x + max(0, (a.w-w)/2)
	y := a.y + max(0, (a.h-h)/2)
	blank := strings.TrimSuffix(strings.Repeat(strings.Repeat(" ", a.w)+"\n", max(0, a.h)), "\n")
	base := lipgloss.NewLayer(screen)
	gutter := lipgloss.NewLayer(blank).X(a.x).Y(a.y).Z(1)
	popup := lipgloss.NewLayer(strings.Join(lines, "\n")).X(x).Y(y).Z(2)
	// The compositor trims trailing blanks; pad back to full width.
	out := strings.Split(lipgloss.NewCompositor(base, gutter, popup).Render(), "\n")
	for i, l := range out {
		out[i] = fitStyled(l, m.width)
	}
	return strings.Join(out, "\n")
}
