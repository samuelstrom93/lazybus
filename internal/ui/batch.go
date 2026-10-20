package ui

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/samuelstrom93/lazybus/internal/bus"
)

// Picking DLQ messages and resubmitting them in one call (bulk DLQ
// Repair). space picks the row, v starts a range that follows the cursor
// (v or space again keeps it), A picks every loaded row with the cursor
// row's DeadLetterReason. r with picks resubmits them all: one scan, then
// each in turn, stopping at the first SendUncertain or CleanupPending.

// Messages returned by the bulk commands.
type (
	batchPlanDoneMsg struct {
		req  bus.BatchRequest
		plan bus.BatchPlan
		err  error
	}
	batchDoneMsg struct {
		req bus.BatchRequest
		res bus.BatchResult
		err error
	}
)

// pickState is the picked rows of the open DLQ list.
type pickState struct {
	// rows are the picked rows; copied on write.
	rows map[markKey]bool
	// ranging: v is on; the rows from anchor to the cursor are picked too.
	ranging bool
	anchor  int64
}

// clearPicks drops every pick: another list opened, or a batch ran.
func (m *Model) clearPicks() { m.picks = pickState{} }

// canPick reports why rows can't be picked now; empty when they can.
func (m Model) canPick() string {
	switch {
	case m.opts.ReadOnly:
		return "read-only mode (--read-only): picking is for resubmit, which is disabled"
	case m.subQueue == bus.Active:
		return activeRefusal
	case m.openNS == nil || m.openEntity == nil:
		return "open an entity first"
	}
	return ""
}

// rangeRows are the visible indices v covers: anchor to cursor.
func (m Model) rangeRows() (from, to int, ok bool) {
	if !m.picks.ranging {
		return 0, 0, false
	}
	a := -1
	for i, msg := range m.messages.items {
		if msg.SequenceNumber == m.picks.anchor {
			a = i
		}
	}
	if a < 0 {
		return 0, 0, false
	}
	return min(a, m.messages.cursor), max(a, m.messages.cursor), true
}

// isPicked reports whether visible row i is picked.
func (m Model) isPicked(i int) bool {
	if i < 0 || i >= len(m.messages.items) {
		return false
	}
	if from, to, ok := m.rangeRows(); ok && i >= from && i <= to {
		return true
	}
	k, ok := m.markKeyOf(m.messages.items[i].SequenceNumber)
	return ok && m.picks.rows[k]
}

// pickedSeqs are the picked loaded rows, in list order.
func (m Model) pickedSeqs() []int64 {
	if _, ok := m.markKeyOf(0); !ok {
		return nil
	}
	inRange := map[int64]bool{}
	if from, to, ok := m.rangeRows(); ok {
		for i := from; i <= to; i++ {
			inRange[m.messages.items[i].SequenceNumber] = true
		}
	}
	var out []int64
	for _, msg := range m.messages.all {
		k, _ := m.markKeyOf(msg.SequenceNumber)
		if m.picks.rows[k] || inRange[msg.SequenceNumber] {
			out = append(out, msg.SequenceNumber)
		}
	}
	return out
}

// setPicked picks or unpicks seqs of the open list.
func (m *Model) setPicked(on bool, seqs ...int64) {
	rows := maps.Clone(m.picks.rows)
	if rows == nil {
		rows = map[markKey]bool{}
	}
	for _, seq := range seqs {
		if k, ok := m.markKeyOf(seq); ok {
			if on {
				rows[k] = true
			} else {
				delete(rows, k)
			}
		}
	}
	m.picks.rows = rows
}

// commitRange ends v, keeping its rows picked.
func (m *Model) commitRange() {
	seqs := m.pickedSeqs()
	m.picks.ranging = false
	m.setPicked(true, seqs...)
}

// togglePick is space on Messages.
func (m Model) togglePick() (Model, tea.Cmd) {
	if why := m.canPick(); why != "" {
		m.setStatus(statusWarn, "%s", why)
		return m, nil
	}
	if m.picks.ranging {
		m.commitRange()
		return m, nil
	}
	msg, ok := m.selectedMessage()
	if !ok {
		return m, nil
	}
	m.setPicked(!m.isPicked(m.messages.cursor), msg.SequenceNumber)
	return m, nil
}

// toggleRange is v on Messages: start a range at the cursor, or keep it.
func (m Model) toggleRange() (Model, tea.Cmd) {
	if why := m.canPick(); why != "" {
		m.setStatus(statusWarn, "%s", why)
		return m, nil
	}
	if m.picks.ranging {
		m.commitRange()
		return m, nil
	}
	msg, ok := m.selectedMessage()
	if !ok {
		return m, nil
	}
	m.picks.ranging, m.picks.anchor = true, msg.SequenceNumber
	return m, nil
}

// pickSameReason is A on Messages: pick every visible loaded row with the
// cursor row's DeadLetterReason (the filter applies), or unpick them when
// all already are.
func (m Model) pickSameReason() (Model, tea.Cmd) {
	if why := m.canPick(); why != "" {
		m.setStatus(statusWarn, "%s", why)
		return m, nil
	}
	cur, ok := m.selectedMessage()
	if !ok {
		return m, nil
	}
	if m.picks.ranging {
		m.commitRange()
	}
	var seqs []int64
	all := true
	for i, msg := range m.messages.items {
		if msg.DeadLetterReason == cur.DeadLetterReason {
			seqs = append(seqs, msg.SequenceNumber)
			all = all && m.isPicked(i)
		}
	}
	reason := orDash(cur.DeadLetterReason)
	m.setPicked(!all, seqs...)
	if all {
		m.setStatus(statusInfo, "unpicked %d with reason %s", len(seqs), reason)
		return m, nil
	}
	text := fmt.Sprintf("picked %d loaded row%s with reason %s", len(seqs), plural(len(seqs)), reason)
	if m.messages.more {
		text += " (more may follow on pages not loaded yet)"
	}
	m.setStatus(statusInfo, "%s", text)
	return m, nil
}

// --- resubmit --------------------------------------------------------------

// startBatch is r with picked rows: refusals, then the pre-check.
func (m Model) startBatch() (Model, tea.Cmd) {
	if why := m.canPick(); why != "" {
		m.setStatus(statusWarn, "%s", why)
		return m, nil
	}
	if m.picks.ranging {
		m.commitRange()
	}
	seqs := m.pickedSeqs()
	if len(seqs) > bus.MaxBatch {
		m.setStatus(statusWarn, "%d picked: one resubmit moves at most %d", len(seqs), bus.MaxBatch)
		return m, nil
	}
	var marked []string
	for _, seq := range seqs {
		if mk, ok := m.markOf(seq); ok {
			marked = append(marked, fmt.Sprintf("seq %d is %s", seq, mk.outcome))
		}
	}
	if len(marked) > 0 {
		m.setStatus(statusWarn, "%s: resolve it with r or c on its own (space unpicks)", strings.Join(marked, ", "))
		return m, nil
	}
	req := bus.BatchRequest{Namespace: *m.openNS, Entity: *m.openEntity, SubQueue: bus.DeadLetter}
	for _, seq := range seqs {
		idx := 0
		for i, msg := range m.messages.all {
			if msg.SequenceNumber == seq {
				idx = i
			}
		}
		// The repair consumes each message's Pending Edits.
		req.Items = append(req.Items, bus.BatchItem{SequenceNumber: seq, Index: idx, Edits: m.editsOf(seq).Edits})
	}
	m.busy = busyState{text: fmt.Sprintf("checking %d messages of %s", len(seqs), entityLabel(req.Entity, bus.DeadLetter))}
	m.stack.Push(CtxBusy)
	be, timeout := m.be, m.repairTimeout()
	plan := func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		p, err := call(func() (bus.BatchPlan, error) { return be.PlanBatch(ctx, req) })
		return batchPlanDoneMsg{req: req, plan: p, err: err}
	}
	return m, tea.Batch(plan, m.spin.Tick)
}

func batchOp(req bus.BatchRequest) string {
	return fmt.Sprintf("resubmit %d messages of %s", len(req.Items), entityLabel(req.Entity, bus.DeadLetter))
}

// itemRequest is item it of req as a single repair request, for the row
// handling and the log.
func itemRequest(req bus.BatchRequest, it bus.BatchItem) bus.RepairRequest {
	return bus.RepairRequest{
		Namespace: req.Namespace, Entity: req.Entity, SubQueue: req.SubQueue,
		SequenceNumber: it.SequenceNumber, Index: it.Index, Edits: it.Edits,
	}
}

func (m Model) batchPlanDone(msg batchPlanDoneMsg) (Model, tea.Cmd) {
	m.endBusy()
	if msg.err != nil {
		text := errText(batchOp(msg.req), msg.err)
		m.logf(true, "%s", text)
		m.setStatus(statusErr, "%s", text)
		return m, nil
	}
	// Rows the pre-check found gone leave now, as for one message.
	var cmds []tea.Cmd
	ready := msg.req
	ready.Items = nil
	for i, it := range msg.req.Items {
		p := msg.plan.Items[i]
		switch {
		case !p.Found:
			res := bus.RepairResult{Outcome: bus.NotFound, Source: msg.plan.Source, SequenceNumber: it.SequenceNumber,
				Target: msg.plan.Target, Detail: "already gone (nothing locked)"}
			r := itemRequest(msg.req, it)
			m.logOutcome(confirmRepair, r, res)
			m.setPicked(false, it.SequenceNumber)
			cmds = append(cmds, m.outcomeRows(confirmRepair, r, res))
		case p.Refused == "":
			ready.Items = append(ready.Items, it)
		default:
			m.logf(true, "resubmit %s seq %d: %s", msg.plan.Source, p.SequenceNumber, p.Refused)
		}
	}
	if len(ready.Items) == 0 {
		m.setStatus(statusWarn, "nothing to resubmit: the picked messages are gone or refused (see log)")
		return m, tea.Batch(cmds...)
	}
	m.confirm = confirmState{kind: confirmBatch, batch: ready, batchPlan: msg.plan, newID: msg.plan.NewIDByDefault}
	m.stack.Push(CtxConfirm)
	return m, tea.Batch(cmds...)
}

// executeBatch runs the confirmed bulk resubmit.
func (m Model) executeBatch(c confirmState) (Model, tea.Cmd) {
	req := c.batch
	req.MessageID = bus.MessageIDKeep
	if c.newID {
		req.MessageID = bus.MessageIDNew
	}
	t := c.batchPlan.Target
	m.busy = busyState{fmt.Sprintf("resubmitting %d messages of %s → %s %s", len(req.Items), entityLabel(req.Entity, bus.DeadLetter), t.Kind, t.Name), true}
	m.stack.Push(CtxBusy)
	// One repair's budget plus a call per message for its send and complete.
	be, timeout := m.be, m.repairTimeout()+time.Duration(len(req.Items))*m.opts.CallTimeout
	run := func() tea.Msg {
		// As for one message: the outcome must reach the log and the rows.
		defer holdSignals()()
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		res, err := call(func() (bus.BatchResult, error) { return be.RepairBatch(ctx, req) })
		return batchDoneMsg{req: req, res: res, err: err}
	}
	return m, tea.Batch(run, m.spin.Tick)
}

func (m Model) batchDone(msg batchDoneMsg) (Model, tea.Cmd) {
	m.endBusy()
	m.clearPicks()
	for _, l := range msg.res.Log {
		m.logf(l.Err, "%s", l.Text)
	}
	if msg.err != nil {
		text := errText(batchOp(msg.req), msg.err)
		m.logf(true, "%s", text)
		m.setStatus(statusErr, "%s", text)
		return m, nil
	}
	counts := map[bus.Outcome]int{}
	skipped := 0
	var cmds []tea.Cmd
	for i, it := range msg.req.Items {
		if i >= len(msg.res.Items) {
			break
		}
		ir := msg.res.Items[i]
		r := itemRequest(msg.req, it)
		if ir.Skipped != "" {
			skipped++
			m.logf(true, "%s: %s", confirmRepair.op(r), ir.Skipped)
			continue
		}
		counts[ir.Outcome]++
		m.logOutcome(confirmRepair, r, ir.RepairResult)
		cmds = append(cmds, m.outcomeRows(confirmRepair, r, ir.RepairResult))
	}
	m.status = batchStatus(msg, counts, skipped)
	return m, tea.Batch(cmds...)
}

// batchStatus sums up a bulk resubmit for the status bar.
func batchStatus(msg batchDoneMsg, counts map[bus.Outcome]int, skipped int) statusLine {
	t := msg.res.Items[0].Target
	var parts []string
	for _, o := range []bus.Outcome{bus.Resubmitted, bus.NotFound, bus.LockLost, bus.SendFailed, bus.SendUncertain, bus.CleanupPending} {
		if n := counts[o]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, o))
		}
	}
	if skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d not attempted", skipped))
	}
	text := fmt.Sprintf("Resubmit %d → %s %s: %s", len(msg.req.Items), t.Kind, t.Name, strings.Join(parts, ", "))
	level := statusOK
	switch {
	case counts[bus.SendFailed] > 0 || counts[bus.CleanupPending] > 0:
		level = statusErr
	case counts[bus.Resubmitted] < len(msg.req.Items):
		level = statusWarn
	}
	if msg.res.Stopped != 0 {
		hint := "check the target before retrying"
		if counts[bus.CleanupPending] > 0 {
			hint = "press c on it to finish cleanup"
		}
		text += fmt.Sprintf(" — stopped at seq %d: %s", msg.res.Stopped, hint)
	}
	if n := len(msg.res.AbandonErrors); n > 0 {
		text += fmt.Sprintf(" (%d abandon error%s, see log)", n, plural(n))
		level = max(level, statusWarn)
	}
	return statusLine{text: text, level: level}
}

// batchFailed reports whether a bulk resubmit left a message short of its
// end state, for the exit code after a deferred quit.
func batchFailed(msg batchDoneMsg) bool {
	if msg.err != nil {
		return true
	}
	for _, ir := range msg.res.Items {
		if ir.Skipped != "" {
			return true
		}
		switch ir.Outcome {
		case bus.SendFailed, bus.SendUncertain, bus.CleanupPending, bus.LockLost:
			return true
		}
	}
	return false
}

// batchBlocks is the confirm popup of a bulk resubmit.
// field adds a label/value block, list one block of several values (cut
// with "… N more lines" on a short terminal), note a warning.
func (m Model) batchBlocks(field func(label, value string, level statusLevel, optional bool), list func(label string, values []string), note func(text string, level statusLevel)) (title string, footer []seg) {
	c := m.confirm
	p := c.batchPlan
	n := len(c.batch.Items)
	title = fmt.Sprintf("Resubmit %d messages", n)
	field("Source", p.Source, statusInfo, false)
	field("Target", fmt.Sprintf("%s %s", p.Target.Kind, p.Target.Name), statusInfo, false)
	plan := map[int64]bus.BatchPlanItem{}
	for _, it := range p.Items {
		plan[it.SequenceNumber] = it
	}
	var lines []string
	for _, it := range c.batch.Items {
		msg := plan[it.SequenceNumber].Message
		line := fmt.Sprintf("seq %d  %s  %s", it.SequenceNumber, msg.MessageID, orDash(msg.DeadLetterReason))
		if e := it.Edits.Count(); e > 0 {
			line += fmt.Sprintf("  (%d edit%s)", e, plural(e))
		}
		lines = append(lines, line)
	}
	list("Messages", lines)
	var skipped []string
	for _, it := range p.Items {
		switch {
		case !it.Found:
			skipped = append(skipped, fmt.Sprintf("seq %d already gone", it.SequenceNumber))
		case it.Refused != "":
			skipped = append(skipped, fmt.Sprintf("seq %d %s", it.SequenceNumber, it.Refused))
		}
	}
	if len(skipped) > 0 {
		field("Skipped", strings.Join(skipped, "; "), statusWarn, false)
	}
	field("Markers", "removed: "+strings.Join(p.MarkersRemoved, ", "), statusInfo, true)
	if c.newID {
		field("MessageId", "a new one for each copy", statusInfo, false)
	} else {
		level := statusInfo
		if p.DuplicateDetection {
			level = statusWarn
		}
		field("MessageId", "kept (each copy keeps its original)", level, false)
	}
	field("Scan", p.ScanCost, statusInfo, true)
	note("one by one after one scan; stops at the first SendUncertain or CleanupPending, and the rest are not sent", statusInfo)
	for _, w := range p.Warnings {
		note(w, statusWarn)
	}
	keep := "new MessageIds"
	if c.newID {
		keep = "keep MessageIds"
	}
	footer = []seg{{"y", stKey}, {fmt.Sprintf(" resubmit %d  ", n), stDim}, {"n", stKey}, {" cancel", stDim},
		{"  ", stDim}, {"m", stKey}, {" " + keep, stDim}}
	return title, footer
}
