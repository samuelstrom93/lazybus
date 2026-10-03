package ui

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/samuelstrom93/lazybus/internal/bus"
)

// DLQ Repair and Finish Cleanup in the UI (spec §6). The algorithm and the
// guards live in the service layer (bus.Repairer); this file runs the
// pre-check, the confirm popup and the call, and applies the outcome to
// the message rows.

// confirmKind tells the two confirm popups apart.
type confirmKind int

const (
	confirmRepair confirmKind = iota
	confirmCleanup
	confirmDiscard // x: discard the Pending Edits of a message (local)
)

// confirmState is the open confirm popup.
type confirmState struct {
	kind confirmKind
	req  bus.RepairRequest
	plan bus.RepairPlan
	// newID: the copy gets plan.NewMessageID instead of the original id.
	// Starts at plan.NewIDByDefault; m toggles it.
	newID bool
	// uncertain: the row is SendUncertain, so an earlier attempt may have
	// delivered a copy.
	uncertain bool
}

// busyState is the busy popup: what runs, and whether it changes state
// (Repair, Finish Cleanup) or is only the pre-check (a peek, no lock).
type busyState struct {
	text    string
	changes bool
}

// statusLevel colours the status bar text.
type statusLevel int

const (
	statusInfo statusLevel = iota
	statusOK
	statusWarn
	statusErr
)

// statusLine is the status bar text; it replaces the key hints in the
// options bar until the next key.
type statusLine struct {
	text  string
	level statusLevel
}

// markKey identifies a message row across reloads of the list.
type markKey struct {
	fqdn, path string
	kind       bus.EntityKind
	seq        int64
}

// rowMark is the outcome a row keeps after a repair: SendUncertain
// (amber) or CleanupPending (red).
type rowMark struct {
	outcome bus.Outcome
	source  string
	target  bus.Target
	// messageID is the MessageId the attempt sent, and newID whether it
	// was a new one: a SendUncertain retry reuses it, so a
	// duplicate-detecting target drops a second copy (spec §6 step 4).
	messageID string
	newID     bool
}

// Messages returned by repair commands.
type (
	planDoneMsg struct {
		kind confirmKind
		req  bus.RepairRequest
		plan bus.RepairPlan
		err  error
	}
	repairDoneMsg struct {
		kind confirmKind
		req  bus.RepairRequest
		res  bus.RepairResult
		err  error
	}
)

func (k confirmKind) key() string {
	switch k {
	case confirmCleanup:
		return "c"
	case confirmDiscard:
		return "x"
	}
	return "r"
}

func (k confirmKind) op(req bus.RepairRequest) string {
	switch k {
	case confirmCleanup:
		return fmt.Sprintf("finish cleanup %s seq %d", entityLabel(req.Entity, bus.DeadLetter), req.SequenceNumber)
	case confirmDiscard:
		return fmt.Sprintf("discard Pending Edits of %s seq %d", entityLabel(req.Entity, bus.DeadLetter), req.SequenceNumber)
	}
	return fmt.Sprintf("resubmit %s seq %d", entityLabel(req.Entity, bus.DeadLetter), req.SequenceNumber)
}

// repairTimeout bounds a pre-check (target guards and a peek) and a Repair
// or FinishCleanup (a scan of several receives, a send, a complete and
// the abandons): each is several calls, so longer than one.
func (m Model) repairTimeout() time.Duration { return 4 * m.opts.CallTimeout }

func (m *Model) setStatus(level statusLevel, format string, args ...any) {
	m.status = statusLine{text: fmt.Sprintf(format, args...), level: level}
}

func (m Model) markKeyOf(seq int64) (markKey, bool) {
	if m.openNS == nil || m.openEntity == nil || m.subQueue != bus.DeadLetter {
		return markKey{}, false
	}
	return markKey{m.openNS.FQDN, m.openEntity.Path, m.openEntity.Kind, seq}, true
}

func reqMarkKey(req bus.RepairRequest) markKey {
	return markKey{req.Namespace.FQDN, req.Entity.Path, req.Entity.Kind, req.SequenceNumber}
}

// markOf returns the mark of the row with sequence number seq in the open
// list.
func (m Model) markOf(seq int64) (rowMark, bool) {
	k, ok := m.markKeyOf(seq)
	if !ok {
		return rowMark{}, false
	}
	mk, ok := m.marks[k]
	return mk, ok
}

// setMark and clearMark copy the map: the Model is a value, and older
// copies must not see the change.
func (m *Model) setMark(k markKey, mk rowMark) {
	marks := maps.Clone(m.marks)
	if marks == nil {
		marks = map[markKey]rowMark{}
	}
	marks[k] = mk
	m.marks = marks
}

func (m *Model) clearMark(k markKey) {
	if _, ok := m.marks[k]; !ok {
		return
	}
	marks := maps.Clone(m.marks)
	delete(marks, k)
	m.marks = marks
}

// startRepair handles r (confirmRepair) and c (confirmCleanup) on the
// selected message: refusals, then the pre-check.
func (m Model) startRepair(kind confirmKind) (Model, tea.Cmd) {
	if m.opts.ReadOnly {
		m.setStatus(statusWarn, "read-only mode (--read-only): %s is disabled", kind.key())
		return m, nil
	}
	if m.subQueue == bus.Active {
		m.setStatus(statusWarn, activeRefusal)
		return m, nil
	}
	msg, ok := m.selectedMessage()
	if !ok || m.openNS == nil || m.openEntity == nil {
		return m, nil
	}
	mark, marked := m.markOf(msg.SequenceNumber)
	switch {
	case kind == confirmRepair && marked && mark.outcome == bus.CleanupPending:
		m.setStatus(statusErr, "copy already in target — press c to finish cleanup")
		return m, nil
	case kind == confirmCleanup && (!marked || mark.outcome != bus.CleanupPending):
		m.setStatus(statusInfo, "c finishes cleanup on a CleanupPending row only")
		return m, nil
	}
	req := bus.RepairRequest{
		Namespace: *m.openNS, Entity: *m.openEntity, SubQueue: bus.DeadLetter,
		SequenceNumber: msg.SequenceNumber, Index: m.messages.allIndex(m.messages.cursor),
	}
	if kind == confirmRepair {
		// The repair consumes the message's Pending Edits.
		req.Edits = m.editsOf(msg.SequenceNumber).Edits
	}
	m.busy = busyState{text: fmt.Sprintf("checking %s seq %d", entityLabel(req.Entity, bus.DeadLetter), req.SequenceNumber)}
	m.stack.Push(CtxBusy)
	// The pre-check is several calls too: target guards, then the peek.
	be, timeout := m.be, m.repairTimeout()
	plan := func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		p, err := call(func() (bus.RepairPlan, error) {
			if kind == confirmCleanup {
				return be.PlanCleanup(ctx, req)
			}
			return be.PlanRepair(ctx, req)
		})
		return planDoneMsg{kind: kind, req: req, plan: p, err: err}
	}
	return m, tea.Batch(plan, m.spin.Tick)
}

// endBusy closes the busy popup.
func (m *Model) endBusy() {
	if m.stack.Top() == CtxBusy {
		m.stack.Pop()
	}
	m.busy = busyState{}
}

func (m Model) planDone(msg planDoneMsg) (Model, tea.Cmd) {
	m.endBusy()
	if msg.err != nil {
		text := errText(msg.kind.op(msg.req), msg.err)
		m.logf(true, "%s", text)
		m.setStatus(statusErr, "%s", text)
		return m, nil
	}
	if !msg.plan.Found {
		// The pre-check did not find it: NotFound, zero messages touched.
		res := bus.RepairResult{
			Outcome: bus.NotFound, Source: msg.plan.Source, SequenceNumber: msg.req.SequenceNumber,
			Target: msg.plan.Target, Detail: "already gone (nothing locked)",
		}
		cmd := m.applyResult(msg.kind, msg.req, res)
		return m, cmd
	}
	mark, marked := m.markOf(msg.req.SequenceNumber)
	m.confirm = confirmState{
		kind: msg.kind, req: msg.req, plan: msg.plan,
		newID:     msg.plan.NewIDByDefault,
		uncertain: marked && mark.outcome == bus.SendUncertain,
	}
	if m.confirm.uncertain && msg.kind == confirmRepair {
		// The retry sends what the uncertain attempt sent.
		m.confirm.newID = mark.newID
		if mark.newID {
			m.confirm.plan.NewMessageID = mark.messageID
		}
	}
	m.stack.Push(CtxConfirm)
	return m, nil
}

// handleConfirmKey: y confirms, n/esc cancel, m toggles the MessageId
// (not on a SendUncertain retry, which reuses the previous attempt's);
// every other key, enter included, is ignored, so the popup defaults to
// cancel (spec §2).
func (m Model) handleConfirmKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	c := m.confirm
	switch {
	case key.Matches(msg, keys.Confirm) && c.kind == confirmDiscard:
		m.stack.Pop()
		m.confirm = confirmState{}
		m.setEdits(reqMarkKey(c.req), bus.Edits{})
		m.setStatus(statusOK, "discarded %d Pending Edit%s of %s seq %d", c.req.Edits.Count(), plural(c.req.Edits.Count()),
			entityLabel(c.req.Entity, bus.DeadLetter), c.req.SequenceNumber)
	case key.Matches(msg, keys.Confirm):
		m.stack.Pop()
		m.confirm = confirmState{}
		return m.execute(c)
	case key.Matches(msg, keys.Cancel):
		m.stack.Pop()
		m.confirm = confirmState{}
		if c.kind == confirmDiscard {
			m.setStatus(statusInfo, "discard canceled; Pending Edits kept")
			break
		}
		m.setStatus(statusInfo, "%s canceled; nothing changed", c.kind.op(c.req))
	case c.kind == confirmRepair && !c.uncertain && key.Matches(msg, keys.ToggleID):
		m.confirm.newID = !m.confirm.newID
	}
	return m, nil
}

// execute runs the confirmed Repair or FinishCleanup.
func (m Model) execute(c confirmState) (Model, tea.Cmd) {
	req := c.req
	src := entityLabel(req.Entity, bus.DeadLetter)
	if c.kind == confirmRepair {
		req.NewMessageID = c.plan.NewMessageID
		req.MessageID = bus.MessageIDKeep
		if c.newID {
			req.MessageID = bus.MessageIDNew
		}
		m.busy = busyState{fmt.Sprintf("resubmitting %s seq %d → %s %s", src, req.SequenceNumber, c.plan.Target.Kind, c.plan.Target.Name), true}
	} else {
		m.busy = busyState{fmt.Sprintf("finishing cleanup of %s seq %d", src, req.SequenceNumber), true}
	}
	m.stack.Push(CtxBusy)
	be, timeout := m.be, m.repairTimeout()
	run := func() tea.Msg {
		// The outcome must reach the log and the row: a closed terminal
		// (SIGHUP) waits for the call like ctrl-c does (spec §2).
		defer holdHangup()()
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		res, err := call(func() (bus.RepairResult, error) {
			if c.kind == confirmCleanup {
				return be.FinishCleanup(ctx, req)
			}
			return be.Repair(ctx, req)
		})
		return repairDoneMsg{kind: c.kind, req: req, res: res, err: err}
	}
	return m, tea.Batch(run, m.spin.Tick)
}

func (m Model) repairDone(msg repairDoneMsg) (Model, tea.Cmd) {
	m.endBusy()
	for _, l := range msg.res.Log {
		m.logf(l.Err, "%s", l.Text)
	}
	if msg.err != nil {
		text := errText(msg.kind.op(msg.req), msg.err)
		m.logf(true, "%s", text)
		m.setStatus(statusErr, "%s", text)
		return m, nil
	}
	cmd := m.applyResult(msg.kind, msg.req, msg.res)
	return m, cmd
}

// applyResult logs the outcome, shows it in the status bar and applies the
// row handling of spec §6 step 4.
func (m *Model) applyResult(kind confirmKind, req bus.RepairRequest, res bus.RepairResult) tea.Cmd {
	src := entityLabel(req.Entity, bus.DeadLetter)
	t := res.Target
	summary := kind.op(req)
	if kind == confirmRepair && t.Name != "" {
		summary += " → " + t.Name
	}
	summary += "  " + res.Outcome.String()
	if res.Detail != "" {
		summary += ": " + res.Detail
	}
	switch res.Outcome {
	case bus.Resubmitted, bus.Cleaned, bus.NotFound:
		m.logf(false, "%s", summary)
	default:
		m.logf(true, "%s", summary)
	}

	var level statusLevel
	var text string
	retry := kind.key()
	switch res.Outcome {
	case bus.Resubmitted:
		level, text = statusOK, fmt.Sprintf("Resubmitted %s seq %d → %s %s", src, req.SequenceNumber, t.Kind, t.Name)
		if res.NewMessageID != res.OldMessageID {
			text += " with new MessageId " + res.NewMessageID
		}
		if n := req.Edits.Count(); n > 0 {
			text += fmt.Sprintf(" (%d edit%s applied)", n, plural(n))
		}
	case bus.Cleaned:
		level, text = statusOK, fmt.Sprintf("Cleaned: %s seq %d removed; nothing sent", src, req.SequenceNumber)
	case bus.NotFound:
		level, text = statusWarn, fmt.Sprintf("NotFound: %s seq %d %s", src, req.SequenceNumber, res.Detail)
		if res.NotFound == bus.NotFoundScan {
			text = fmt.Sprintf("NotFound: %s seq %d not reached within the scan limit — may still be in the DLQ; R to refresh", src, req.SequenceNumber)
		}
	case bus.LockLost:
		level, text = statusWarn, fmt.Sprintf("LockLost: nothing changed; %s to retry", retry)
	case bus.SendFailed:
		level, text = statusErr, fmt.Sprintf("SendFailed: %s %s rejected the copy (%s); nothing changed", t.Kind, t.Name, res.Detail)
	case bus.SendUncertain:
		level, text = statusWarn, fmt.Sprintf("SendUncertain: the copy may or may not be in %s %s; check the target before retrying", t.Kind, t.Name)
	case bus.CleanupPending:
		level, text = statusErr, fmt.Sprintf("CleanupPending: copy is in %s %s and the original is still in %s seq %d — press c to finish cleanup",
			t.Kind, t.Name, src, req.SequenceNumber)
	}
	if n := len(res.AbandonErrors); n > 0 {
		text += fmt.Sprintf(" (%d abandon error%s, see log)", n, plural(n))
		level = max(level, statusWarn)
	}
	m.status = statusLine{text: text, level: level}

	k := reqMarkKey(req)
	switch res.Outcome {
	case bus.Resubmitted, bus.Cleaned:
		m.decDLQCount(req)
		return m.removeRow(req)
	case bus.NotFound:
		if res.NotFound == bus.NotFoundScan {
			return nil // the pre-check saw it: it may still be there
		}
		return m.removeRow(req)
	case bus.SendUncertain, bus.CleanupPending:
		m.setMark(k, rowMark{outcome: res.Outcome, source: src, target: t,
			messageID: res.NewMessageID, newID: res.NewMessageID != res.OldMessageID})
	}
	return nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// isOpenList reports whether req's DLQ is the list on screen.
func (m Model) isOpenList(req bus.RepairRequest) bool {
	return m.openNS != nil && m.openEntity != nil && m.subQueue == bus.DeadLetter &&
		m.openNS.FQDN == req.Namespace.FQDN && m.openEntity.Path == req.Entity.Path && m.openEntity.Kind == req.Entity.Kind
}

// removeRow drops req's row and its Pending Edits. The cursor keeps its index, which puts it on
// the next message (or the new last row), so `r y r y` works down a list.
// With a filter, "next" is the next visible row.
func (m *Model) removeRow(req bus.RepairRequest) tea.Cmd {
	m.clearMark(reqMarkKey(req))
	// The message is gone: its Pending Edits go with it (spec §6 step 4).
	if _, ok := m.pending[reqMarkKey(req)]; ok {
		m.setEdits(reqMarkKey(req), bus.Edits{})
	}
	if !m.isOpenList(req) {
		return nil
	}
	all := m.messages.all
	i := -1
	for j, it := range all {
		if it.SequenceNumber == req.SequenceNumber {
			i = j
			break
		}
	}
	if i < 0 {
		return nil
	}
	v := m.messages.visibleIndex(i)
	// A new slice: older Model copies keep theirs.
	m.messages.setAll(append(all[:i:i], all[i+1:]...), m.messageText)
	if v >= 0 && m.messages.cursor > v {
		m.messages.cursor--
	}
	m.messages.move(m.messages.cursor)
	m.resetMain()
	m.clearLines()
	if v < 0 || v < len(m.messages.items) || !m.messages.more {
		return m.loadMore()
	}
	// The removed row was the last visible one and more may follow: the
	// next message may be on the next page, so the cursor moves there when
	// it arrives (an empty page leaves it on the new last row).
	if m.messages.loading {
		m.afterPage, m.hasAfterPage = v, true
		return nil
	}
	if len(m.messages.all) == 0 {
		m.afterPage, m.hasAfterPage = 0, true
		return m.peekFrom(*m.openNS, *m.openEntity, m.subQueue, req.SequenceNumber+1)
	}
	cmd := m.loadMore()
	if cmd != nil {
		m.afterPage, m.hasAfterPage = v, true
	}
	return cmd
}

// decDLQCount lowers the shown DLQ count of req's entity by one, when it
// is known.
func (m *Model) decDLQCount(req bus.RepairRequest) {
	if m.openNS == nil || m.openNS.FQDN != req.Namespace.FQDN {
		return
	}
	for i, e := range m.entities.all {
		if e.Path == req.Entity.Path && e.Kind == req.Entity.Kind && e.CountsKnown && e.DeadLetterCount > 0 {
			prev, ok := m.entities.selected()
			items := append([]bus.Entity(nil), m.entities.all...)
			items[i].DeadLetterCount--
			m.entities.setAll(sortEntities(items, m.sortDLQ), m.entityText)
			if ok {
				m.selectEntity(prev)
			}
			return
		}
	}
}

// --- rendering -----------------------------------------------------------

// markStyle is the row colour of a mark.
func markStyleOf(o bus.Outcome) statusLevel {
	if o == bus.CleanupPending {
		return statusErr
	}
	return statusWarn
}

// bannerLines are the main pane lines above a marked message.
func (m Model) bannerLines(seq int64, width int) []string {
	mark, ok := m.markOf(seq)
	if !ok {
		return nil
	}
	var text string
	switch mark.outcome {
	case bus.CleanupPending:
		text = fmt.Sprintf("CleanupPending: the copy is in %s %s and the original is still in %s. Press c to finish cleanup (removes the original; nothing is sent).",
			mark.target.Kind, mark.target.Name, mark.source)
	case bus.SendUncertain:
		text = fmt.Sprintf("SendUncertain: the copy may or may not be in %s %s. Check the target before pressing r again.",
			mark.target.Kind, mark.target.Name)
	default:
		return nil
	}
	st := levelStyle(markStyleOf(mark.outcome))
	var out []string
	for _, l := range wrapLines(text, width-2) {
		out = append(out, row([]seg{{" " + l, st}}, width, false))
	}
	return append(out, "")
}

// confirmBlock is one item of the confirm popup: a field or a warning.
// Optional blocks are dropped first when the terminal is too short.
type confirmBlock struct {
	lines    []string
	optional bool
}

// confirmBlocks builds the confirm popup body: label/value rows and
// warnings, wrapped to inner cells.
func (m Model) confirmBlocks(inner int) (title string, blocks []confirmBlock, footer []seg) {
	c := m.confirm
	p := c.plan
	const labelW = 12
	valW := max(10, inner-2-labelW)
	field := func(label, value string, level statusLevel, optional bool) {
		var lines []string
		for i, l := range wrapLines(sanitize(value), valW) {
			lab := ""
			if i == 0 {
				lab = label
			}
			lines = append(lines, row([]seg{{" " + fitPlain(lab, labelW), stDim}, {l, levelStyle(level)}}, inner, false))
		}
		blocks = append(blocks, confirmBlock{lines, optional})
	}
	note := func(text string, level statusLevel) {
		var lines []string
		for i, l := range wrapLines(sanitize(text), inner-4) {
			prefix := "   "
			if i == 0 {
				prefix = " ! "
			}
			lines = append(lines, row([]seg{{prefix + l, levelStyle(level)}}, inner, false))
		}
		blocks = append(blocks, confirmBlock{lines, false})
	}
	src := fmt.Sprintf("%s seq %d", p.Source, p.SequenceNumber)
	target := fmt.Sprintf("%s %s", p.Target.Kind, p.Target.Name)

	if c.kind == confirmDiscard {
		title = "Discard Pending Edits"
		field("Message", src+" (MessageId "+p.Message.MessageID+")", statusInfo, false)
		for _, d := range m.editDiff(p.Message, c.req.Edits) {
			field(d.label, d.text, d.level, d.optional)
		}
		note("the edits live only in memory: discarded edits can't be restored", statusWarn)
		footer = []seg{{"y", stKey}, {" discard  ", stDim}, {"n", stKey}, {" cancel", stDim}}
		return title, blocks, footer
	}
	if c.kind == confirmCleanup {
		title = "Finish Cleanup"
		field("Remove", src+" (MessageId "+p.Message.MessageID+")", statusInfo, false)
		if p.Target.Name != "" {
			field("Copy", "already in "+target, statusInfo, false)
		}
		field("Send", "nothing is sent", statusInfo, false)
		field("Scan", p.ScanCost, statusInfo, true)
		footer = []seg{{"y", stKey}, {" remove  ", stDim}, {"n", stKey}, {" cancel", stDim}}
		return title, blocks, footer
	}

	title = "Resubmit"
	field("Source", src, statusInfo, false)
	field("Target", target, statusInfo, false)
	edits := c.req.Edits
	if !edits.BodyEdited {
		field("Body", bodySummary("unchanged", p.Message.Body), statusInfo, true)
	}
	if edits.IsZero() {
		field("Edits", "none (properties, Subject and ContentType unchanged)", statusInfo, true)
	}
	// Every Pending Edit is listed (what y sends); per-property lines give
	// way on a short terminal, the count stays.
	for _, d := range m.editDiff(p.Message, edits) {
		field(d.label, d.text, d.level, d.optional)
	}
	field("Markers", "removed: "+strings.Join(p.MarkersRemoved, ", "), statusInfo, true)
	same := ""
	if c.uncertain {
		same = ", as the previous attempt"
	}
	if c.newID {
		field("MessageId", fmt.Sprintf("new %s (was %s)%s", p.NewMessageID, p.Message.MessageID, same), statusInfo, false)
	} else {
		level := statusInfo
		if p.DuplicateDetection && !c.uncertain {
			level = statusWarn
		}
		field("MessageId", "kept "+p.Message.MessageID+same, level, false)
	}
	field("Scan", p.ScanCost, statusInfo, true)
	if len(p.Warnings) > 0 || c.uncertain {
		blocks = append(blocks, confirmBlock{[]string{strings.Repeat(" ", inner)}, true})
	}
	for _, w := range p.Warnings {
		note(w, statusWarn)
	}
	if c.uncertain {
		note("the previous attempt may have delivered a copy — check the target first", statusErr)
	}
	footer = []seg{{"y", stKey}, {" resubmit  ", stDim}, {"n", stKey}, {" cancel", stDim}}
	if !c.uncertain {
		keep := "new MessageId"
		if c.newID {
			keep = "keep MessageId"
		}
		footer = append(footer, seg{"  ", stDim}, seg{"m", stKey}, seg{" " + keep, stDim})
	}
	return title, blocks, footer
}

// fitBlocks returns the lines of blocks in at most n rows: optional
// blocks go first, from the last one up, so warnings and the source,
// target and MessageId stay on a short terminal.
func fitBlocks(blocks []confirmBlock, n int) []string {
	total := 0
	for _, b := range blocks {
		total += len(b.lines)
	}
	drop := make([]bool, len(blocks))
	for i := len(blocks) - 1; i >= 0 && total > n; i-- {
		if blocks[i].optional {
			drop[i] = true
			total -= len(blocks[i].lines)
		}
	}
	var out []string
	for i, b := range blocks {
		if !drop[i] {
			out = append(out, b.lines...)
		}
	}
	return out[:min(len(out), n)]
}

func (m Model) overlayConfirm(screen string) string {
	w := min(m.width-4, 104)
	inner := w - 2
	title, blocks, footer := m.confirmBlocks(inner)
	// Rows between the borders: the screen minus the options bar and the
	// two borders; a blank row above and below when they fit.
	room := max(1, m.height-1-2)
	body := fitBlocks(blocks, room)
	pad := len(body)+2 <= room
	blank := boxRow(strings.Repeat(" ", inner), stBorderFocus)
	lines := []string{hBorder(w, "┌", "┐", []seg{{title, stTitleFocus}}, stBorderFocus)}
	if pad {
		lines = append(lines, blank)
	}
	for _, l := range body {
		lines = append(lines, boxRow(fitStyled(l, inner), stBorderFocus))
	}
	if pad {
		lines = append(lines, blank)
	}
	lines = append(lines, hBorder(w, "└", "┘", footer, stBorderFocus))
	return m.overlay(screen, lines, w)
}

func (m Model) overlayBusy(screen string) string {
	text := m.busy.text + "…"
	footer := "peek only, nothing locked"
	if m.busy.changes {
		footer = "locks are released before this returns"
	}
	w := min(m.width-4, max(44, len([]rune(text))+8))
	inner := w - 2
	lines := []string{
		hBorder(w, "┌", "┐", []seg{{"Working", stTitleFocus}}, stBorderFocus),
		boxRow(row([]seg{{" " + m.spin.View() + " ", stKey}, {text, stPlain}}, inner, false), stBorderFocus),
		hBorder(w, "└", "┘", []seg{{footer, stDim}}, stBorderFocus),
	}
	return m.overlay(screen, lines, w)
}
