package ui

import (
	"os"
	"os/signal"
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

// exitReport builds the report of msg's call from its status line and the
// log lines from start on.
func (m Model) exitReport(msg repairDoneMsg, start int) exitReport {
	var b strings.Builder
	b.WriteString("lazybus quit after the call finished: " + m.status.text)
	for _, e := range m.log[start:] {
		b.WriteString("\n  " + e.text)
	}
	failed := msg.err != nil
	switch msg.res.Outcome {
	case bus.SendFailed, bus.SendUncertain, bus.CleanupPending, bus.LockLost:
		failed = true
	}
	return exitReport{b.String(), failed}
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
