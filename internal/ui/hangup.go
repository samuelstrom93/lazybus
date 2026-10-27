package ui

import (
	"cmp"
	"fmt"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	tea "charm.land/bubbletea/v2"

	"github.com/samuelstrom93/lazybus/internal/bus"
)

// holdSignals holds back SIGHUP, SIGINT and SIGTERM until the returned
// func runs, so neither closing the terminal nor a kill ends lazybus in the
// middle of a state-changing call (spec §2). It uses Notify and Stop, not
// Ignore: Go can't undo signal.Ignore, while Stop puts back the previous
// handling (default: exit; still ignored when started under nohup).
//
// A SIGHUP that came in during the call is sent again afterwards: the
// terminal is gone, and the kernel sends it only once. SIGINT and SIGTERM
// are not: Bubble Tea's handler got the first one as a quit, which Filter
// deferred, and then stops listening, so without this hold a second one
// would kill lazybus with the default action.
func holdSignals() (restore func()) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	return func() {
		signal.Stop(quit)
		signal.Stop(hup)
		select {
		case <-hup:
			_ = syscall.Kill(os.Getpid(), syscall.SIGHUP)
		default:
		}
	}
}

// deferredQuitMsg is a quit Bubble Tea asked for while a state-changing
// call ran (Filter): the model quits once the call's outcome is in.
type deferredQuitMsg struct {
	quit tea.Cmd // tea.Quit, or tea.Interrupt for a SIGINT
}

// Filter is the program's message filter (tea.WithFilter). Bubble Tea
// handles SIGTERM (and SIGINT when the input is not a terminal) itself,
// with a QuitMsg or InterruptMsg that never reaches Update and would end
// the program in the middle of a Repair or Finish Cleanup. While such a
// call runs, Filter turns them into a deferredQuitMsg, so lazybus quits
// after the outcome (spec §2).
func Filter(model tea.Model, msg tea.Msg) tea.Msg {
	m, ok := model.(Model)
	if !ok || !m.callRunning() {
		return msg
	}
	switch msg.(type) {
	case tea.QuitMsg:
		return deferredQuitMsg{tea.Quit}
	case tea.InterruptMsg:
		return deferredQuitMsg{tea.Interrupt}
	}
	return msg
}

// exitReport is the outcome of the call a deferred quit waited for: the
// alt screen and the in-memory log are gone once lazybus exits.
type exitReport struct {
	text   string
	failed bool
}

// exitReport builds the report of the call from its status line and the
// log lines from start on; failed sets the exit code.
func (m Model) exitReport(failed bool, start int) exitReport {
	var b strings.Builder
	b.WriteString("lazybus quit after the call finished: " + m.status.text)
	for _, e := range m.log[start:] {
		b.WriteString("\n  " + e.text)
	}
	return exitReport{b.String(), failed}
}

// repairFailed reports whether msg's message did not reach its end state.
func repairFailed(msg repairDoneMsg) bool {
	switch msg.res.Outcome {
	case bus.SendFailed, bus.SendUncertain, bus.CleanupPending, bus.LockLost:
		return true
	}
	return msg.err != nil
}

// ExitReport is the outcome of the state-changing call a signal's quit
// waited for (Filter), to print once the terminal is restored; failed
// when the message did not reach its end state (SendFailed,
// SendUncertain, CleanupPending, LockLost, or an error). Empty text when
// no quit was deferred.
func (m Model) ExitReport() (text string, failed bool) {
	return m.exit.text, m.exit.failed
}

// callRunning reports whether a state-changing call (Repair, Finish
// Cleanup) is running.
func (m Model) callRunning() bool {
	return m.stack.Top() == CtxBusy && m.busy.changes
}

// OpenOutcomes lists the rows that keep a SendUncertain or CleanupPending
// outcome, to print once the terminal is restored: the marks live in
// memory only, and after a restart the row looks like any dead-letter
// message, so a resubmit would send a second copy. Empty when there are
// none.
func (m Model) OpenOutcomes() string {
	if len(m.marks) == 0 {
		return ""
	}
	keys := slices.SortedFunc(maps.Keys(m.marks), func(a, b markKey) int {
		return cmp.Or(cmp.Compare(a.fqdn, b.fqdn), cmp.Compare(a.path, b.path), cmp.Compare(a.kind, b.kind), cmp.Compare(a.seq, b.seq))
	})
	var b strings.Builder
	fmt.Fprintf(&b, "lazybus quit with %d open outcome%s; check the target before you resubmit again, or a second copy is sent:", len(keys), plural(len(keys)))
	for _, k := range keys {
		mk := m.marks[k]
		fmt.Fprintf(&b, "\n  %s: %s seq %d → %s %s, MessageId %s (%s)", mk.outcome, mk.source, k.seq, mk.target.Kind, mk.target.Name, mk.messageID, k.fqdn)
	}
	return b.String()
}
