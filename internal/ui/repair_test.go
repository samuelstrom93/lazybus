package ui

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/teatest/v2"

	"github.com/samuelstrom93/lazybus/internal/bus"
	"github.com/samuelstrom93/lazybus/internal/bus/fake"
)

// onOrders opens sb-prod-weu orders (DLQ 37, seq 2…38) with Messages
// focused.
func onOrders(t testing.TB, be *fake.Backend) Model {
	t.Helper()
	return keysIn(t, startWith(t, be), "2", "j", "j", "j", "enter")
}

func dlqCount(m Model, path string) int64 {
	for _, e := range m.entities.items {
		if e.Path == path {
			return e.DeadLetterCount
		}
	}
	return -1
}

func activeCount(m Model, path string) int64 {
	for _, e := range m.entities.items {
		if e.Path == path {
			return e.ActiveCount
		}
	}
	return -1
}

func selectedSeq(t *testing.T, m Model) int64 {
	t.Helper()
	msg, ok := m.selectedMessage()
	if !ok {
		t.Fatal("no message selected")
	}
	return msg.SequenceNumber
}

func hasLog(m Model, text string) bool {
	for _, e := range m.log {
		if strings.Contains(e.text, text) {
			return true
		}
	}
	return false
}

// removeOutOfBand repairs seq behind the UI's back (another user).
func removeOutOfBand(t *testing.T, be *fake.Backend, m Model, seq int64) {
	t.Helper()
	res, err := be.Repair(context.Background(), bus.RepairRequest{
		Namespace: *m.openNS, Entity: *m.openEntity, SubQueue: bus.DeadLetter, SequenceNumber: seq,
	})
	if err != nil || res.Outcome != bus.Resubmitted {
		t.Fatalf("out-of-band repair: %v %v", res.Outcome, err)
	}
}

func TestRepairRYRYWalksTheList(t *testing.T) {
	be := fake.New()
	m := onOrders(t, be)
	third := m.messages.items[2].SequenceNumber
	active := activeCount(m, "orders")

	m = keysIn(t, m, "r")
	if m.stack.Top() != CtxConfirm {
		t.Fatalf("r did not open the confirm popup: top %v status %q", m.stack.Top(), m.status.text)
	}
	m = keysIn(t, m, "y", "r", "y")
	if !m.stack.AtRoot() || len(m.messages.items) != 35 || m.messages.cursor != 0 || selectedSeq(t, m) != third {
		t.Fatalf("after r y r y: top %v, %d rows, cursor %d on seq %d", m.stack.Top(), len(m.messages.items), m.messages.cursor, selectedSeq(t, m))
	}
	if got := dlqCount(m, "orders"); got != 35 {
		t.Fatalf("Entities DLQ count = %d, want 35", got)
	}
	if got := activeCount(m, "orders"); got != active+2 {
		t.Fatalf("Entities active count = %d, want %d", got, active+2)
	}
	if got := len(be.Messages("sb-prod-weu", "orders", bus.KindQueue, bus.DeadLetter)); got != 35 {
		t.Fatalf("broker DLQ has %d, want 35", got)
	}
	if !hasLog(m, "resubmit orders/$DLQ seq 3 → orders  Resubmitted") || !hasLog(m, "send orders/$DLQ seq 3 → queue orders") {
		t.Fatalf("log: %+v", m.log)
	}
	if m.status.level != statusOK || !strings.Contains(m.status.text, "Resubmitted orders/$DLQ seq 3 → queue orders") {
		t.Fatalf("status = %+v", m.status)
	}
}

func TestRepairCursorMiddleAndLast(t *testing.T) {
	m := onOrders(t, fake.New())
	next := m.messages.items[2].SequenceNumber
	m = keysIn(t, m, "j", "r", "y")
	if m.messages.cursor != 1 || selectedSeq(t, m) != next {
		t.Fatalf("middle: cursor %d on seq %d, want 1 on %d", m.messages.cursor, selectedSeq(t, m), next)
	}

	m = keysIn(t, m, "G")
	prev := m.messages.items[len(m.messages.items)-2].SequenceNumber
	m = keysIn(t, m, "r", "y")
	if m.messages.cursor != len(m.messages.items)-1 || selectedSeq(t, m) != prev {
		t.Fatalf("last: cursor %d of %d on seq %d, want the new last %d", m.messages.cursor, len(m.messages.items), selectedSeq(t, m), prev)
	}
}

func TestRepairAtPageBoundaryMovesToNextPage(t *testing.T) {
	be := fake.New(fake.WithDeadLetters("sb-prod-weu", "orders", 120))
	m := onOrders(t, be)
	// Hold the next page back so the cursor sits on the last loaded row
	// with more to come.
	be.SetFault(fake.OpPeek, fake.Fault{Err: errors.New("busy")})
	m = keysIn(t, m, "G")
	be.SetFault(fake.OpPeek, fake.Fault{})
	if len(m.messages.items) != 50 || m.messages.cursor != 49 || !m.messages.more {
		t.Fatalf("setup: %d rows, cursor %d, more %v", len(m.messages.items), m.messages.cursor, m.messages.more)
	}
	last := selectedSeq(t, m)
	m = keysIn(t, m, "r", "y")
	if m.messages.cursor != 49 || selectedSeq(t, m) != last+1 || len(m.messages.items) != 99 {
		t.Fatalf("cursor %d on seq %d of %d rows, want 49 on %d", m.messages.cursor, selectedSeq(t, m), len(m.messages.items), last+1)
	}
}

func TestRepairLastMessageEmptiesList(t *testing.T) {
	m := keysIn(t, startWith(t, fake.New()), "1", "j", "enter", "enter") // sb-test-weu orders: DLQ 1
	if len(m.messages.items) != 1 {
		t.Fatalf("setup: %d rows", len(m.messages.items))
	}
	m = keysIn(t, m, "r", "y")
	if len(m.messages.items) != 0 || m.messages.more || m.messages.loading || m.status.level != statusOK {
		t.Fatalf("%d rows, more %v, loading %v, status %+v", len(m.messages.items), m.messages.more, m.messages.loading, m.status)
	}
}

func TestRepairFromMainPane(t *testing.T) {
	m := keysIn(t, onOrders(t, fake.New()), "0", "r")
	if m.stack.Root() != CtxMain || m.stack.Top() != CtxConfirm {
		t.Fatalf("r in the main pane: root %v top %v", m.stack.Root(), m.stack.Top())
	}
	m = keysIn(t, m, "y")
	if len(m.messages.items) != 36 || m.stack.Top() != CtxMain {
		t.Fatalf("%d rows, top %v", len(m.messages.items), m.stack.Top())
	}
}

func TestRepairPreCheckError(t *testing.T) {
	be := fake.New()
	m := onOrders(t, be)
	// (An unauthorized target read is not an error: no Manage rights, see
	// confirm-no-manage.)
	be.SetFault(fake.OpTargetInfo, fake.Fault{Err: &bus.Error{Kind: bus.ErrThrottled, Op: "get queue orders", Msg: "HTTP 503 ServerBusy"}})
	m = keysIn(t, m, "r")
	if !m.stack.AtRoot() || m.status.level != statusErr || !strings.Contains(m.status.text, "HTTP 503 ServerBusy") || !hasLog(m, "HTTP 503 ServerBusy") {
		t.Fatalf("top %v status %+v", m.stack.Top(), m.status)
	}
	if len(m.messages.items) != 37 {
		t.Fatalf("pre-check error changed the rows: %d", len(m.messages.items))
	}
}

func TestFinishCleanupNotFoundAndLockLost(t *testing.T) {
	pending := func(t *testing.T) (*fake.Backend, Model) {
		be := fake.New()
		m := onOrders(t, be)
		be.SetFault(fake.OpComplete, fake.Fault{Err: errors.New("link detached")})
		m = keysIn(t, m, "r", "y")
		be.SetFault(fake.OpComplete, fake.Fault{})
		return be, m
	}
	t.Run("NotFound", func(t *testing.T) {
		be, m := pending(t)
		res, err := be.FinishCleanup(context.Background(), bus.RepairRequest{
			Namespace: *m.openNS, Entity: *m.openEntity, SubQueue: bus.DeadLetter, SequenceNumber: selectedSeq(t, m),
		})
		if err != nil || res.Outcome != bus.Cleaned {
			t.Fatalf("out-of-band cleanup: %v %v", res.Outcome, err)
		}
		m = keysIn(t, m, "c")
		if !m.stack.AtRoot() || len(m.messages.items) != 36 || m.messages.cursor != 0 || len(m.marks) != 0 {
			t.Fatalf("top %v, %d rows, cursor %d, marks %v", m.stack.Top(), len(m.messages.items), m.messages.cursor, m.marks)
		}
	})
	t.Run("LockLost", func(t *testing.T) {
		be, m := pending(t)
		seq := selectedSeq(t, m)
		be.SetLockDuration(5 * time.Second)
		m = keysIn(t, m, "c", "y")
		if mark, ok := m.markOf(seq); !ok || mark.outcome != bus.CleanupPending || len(m.messages.items) != 37 {
			t.Fatalf("mark %v %v, %d rows", mark, ok, len(m.messages.items))
		}
		if !strings.HasPrefix(m.status.text, "LockLost: nothing changed; c to retry") {
			t.Fatalf("status %q", m.status.text)
		}
	})
}

func TestCtrlCWaitsForRepairNotPreCheck(t *testing.T) {
	ctrlC := tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	m := onOrders(t, fake.New())
	next, _ := m.Update(press("r"))
	if _, cmd := next.(Model).Update(ctrlC); !isQuit(cmd) {
		t.Fatal("ctrl-c during the pre-check did not quit")
	}
	m = keysIn(t, m, "r")
	next, _ = m.Update(press("y"))
	if _, cmd := next.(Model).Update(ctrlC); cmd != nil {
		t.Fatal("ctrl-c quit in the middle of a repair")
	}
}

// TestConfirmKeepsWarningsOnSmallScreen: at the smallest layout the
// optional rows go first; the warnings stay.
func TestConfirmKeepsWarningsOnSmallScreen(t *testing.T) {
	be := fake.New()
	m := New(be, testOptions())
	m = run(t, m, tea.WindowSizeMsg{Width: minWidth, Height: minHeight})
	m = runCmd(t, m, m.Init())
	m = keysIn(t, m, "2", "j", "enter")
	be.SetFault(fake.OpSend, fake.Fault{Err: context.DeadlineExceeded})
	m = keysIn(t, m, "r", "y")
	be.SetFault(fake.OpSend, fake.Fault{})
	m = keysIn(t, m, "r")
	screen := m.View().Content
	requireScreenSize(t, screen, minWidth, minHeight)
	text := stripBoxes(screen)
	for _, want := range []string{"Source", "Target", "MessageId", "no match = dropped", "previous attempt may have delivered"} {
		if !strings.Contains(text, want) {
			t.Errorf("confirm at %dx%d lacks %q:\n%s", minWidth, minHeight, want, ansi.Strip(screen))
		}
	}
}

// stripBoxes joins the screen's text without box characters, so wrapped
// words can be searched.
func stripBoxes(screen string) string {
	var b strings.Builder
	for _, l := range strings.Split(ansi.Strip(screen), "\n") {
		l = strings.Trim(strings.TrimSpace(l), "│ ")
		b.WriteString(l + " ")
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func TestRepairNotFoundRemovesRowKeepsIndex(t *testing.T) {
	be := fake.New()
	m := keysIn(t, onOrders(t, be), "j")
	gone, next := selectedSeq(t, m), m.messages.items[2].SequenceNumber
	removeOutOfBand(t, be, m, gone)
	be.ResetEvents()

	m = keysIn(t, m, "r")
	if !m.stack.AtRoot() {
		t.Fatalf("NotFound pre-check opened %v", m.stack.Top())
	}
	if len(m.messages.items) != 36 || m.messages.cursor != 1 || selectedSeq(t, m) != next {
		t.Fatalf("%d rows, cursor %d on %d", len(m.messages.items), m.messages.cursor, selectedSeq(t, m))
	}
	if dlqCount(m, "orders") != 37 || len(be.Events()) != 0 {
		t.Fatalf("NotFound changed the count (%d) or touched the broker: %v", dlqCount(m, "orders"), be.Events())
	}
	if !strings.HasPrefix(m.status.text, "NotFound: orders/$DLQ seq 3 already gone") {
		t.Fatalf("status = %q", m.status.text)
	}
}

func TestRepairLockLostAndSendFailedKeepRow(t *testing.T) {
	for name, setup := range map[string]func(*fake.Backend){
		"LockLost": func(be *fake.Backend) { be.SetLockDuration(5 * time.Second) },
		"SendFailed": func(be *fake.Backend) {
			be.SetFault(fake.OpSend, fake.Fault{Err: &bus.Error{Kind: bus.ErrNotAllowed, Msg: "amqp:not-allowed: SessionId not set"}, Definite: true})
		},
	} {
		t.Run(name, func(t *testing.T) {
			be := fake.New()
			m := keysIn(t, onOrders(t, be), "j")
			seq := selectedSeq(t, m)
			setup(be)
			m = keysIn(t, m, "r", "y")
			if len(m.messages.items) != 37 || selectedSeq(t, m) != seq || len(m.marks) != 0 || dlqCount(m, "orders") != 37 {
				t.Fatalf("%d rows, seq %d, marks %v", len(m.messages.items), selectedSeq(t, m), m.marks)
			}
			if !strings.HasPrefix(m.status.text, name) || !hasLog(m, "→ orders  "+name) {
				t.Fatalf("status %q log %+v", m.status.text, m.log)
			}
		})
	}
}

func TestRepairSendUncertainMarksAndWarnsOnRetry(t *testing.T) {
	be := fake.New()
	m := onOrders(t, be)
	be.SetFault(fake.OpSend, fake.Fault{Err: context.DeadlineExceeded})
	m = keysIn(t, m, "r", "y")
	mark, ok := m.markOf(selectedSeq(t, m))
	if !ok || mark.outcome != bus.SendUncertain || len(m.messages.items) != 37 {
		t.Fatalf("mark %v %v, %d rows", mark, ok, len(m.messages.items))
	}
	be.SetFault(fake.OpSend, fake.Fault{})
	m = keysIn(t, m, "r")
	if m.stack.Top() != CtxConfirm || !m.confirm.uncertain {
		t.Fatalf("retry: top %v uncertain %v", m.stack.Top(), m.confirm.uncertain)
	}
	m = keysIn(t, m, "y")
	if len(m.messages.items) != 36 || len(m.marks) != 0 {
		t.Fatalf("retry: %d rows, marks %v", len(m.messages.items), m.marks)
	}
}

func TestRepairUncertainRetryReusesMessageID(t *testing.T) {
	be := fake.New()
	m := keysIn(t, startWith(t, be), "3") // invoices: duplicate detection
	seq := selectedSeq(t, m)
	// The copy is delivered, but the send's answer is lost.
	be.SetFault(fake.OpSend, fake.Fault{Err: context.DeadlineExceeded, After: true})
	m = keysIn(t, m, "r", "y")
	mark, ok := m.markOf(seq)
	if !ok || mark.outcome != bus.SendUncertain || !mark.newID {
		t.Fatalf("mark %+v %v", mark, ok)
	}
	be.SetFault(fake.OpSend, fake.Fault{})
	m = keysIn(t, m, "r", "m") // m can't switch away from the previous id
	if !m.confirm.newID || m.confirm.plan.NewMessageID != mark.messageID {
		t.Fatalf("retry popup: newID %v id %s, want %s", m.confirm.newID, m.confirm.plan.NewMessageID, mark.messageID)
	}
	m = keysIn(t, m, "y")
	active := be.Messages("sb-prod-weu", "invoices", bus.KindQueue, bus.Active)
	copies := 0
	for _, a := range active {
		if a.MessageID == mark.messageID {
			copies++
		}
	}
	if copies != 1 {
		t.Fatalf("%d copies with MessageId %s, want 1 (the target drops the duplicate)", copies, mark.messageID)
	}
	if _, ok := m.markOf(seq); ok {
		t.Fatal("mark kept after a successful retry")
	}
}

func TestRepairScanNotFoundKeepsRow(t *testing.T) {
	be := fake.New()
	m := keysIn(t, onOrders(t, be), "j", "j", "j", "j", "j", "j", "j", "j", "j", "j", "j", "j")
	seq := selectedSeq(t, m)
	// Every lock expires inside the safety margin: the scan stops after
	// the first batch, before row 12.
	be.SetLockDuration(5 * time.Second)
	m = keysIn(t, m, "r", "y")
	want := "NotFound: orders/$DLQ seq " + strconv.FormatInt(seq, 10) + " not reached within the scan limit — may still be in the DLQ; R to refresh"
	if m.status.text != want {
		t.Fatalf("status %q", m.status.text)
	}
	if len(m.messages.items) != 37 || selectedSeq(t, m) != seq || dlqCount(m, "orders") != 37 {
		t.Fatalf("row removed: %d rows, on %d", len(m.messages.items), selectedSeq(t, m))
	}
}

func TestRepairHoldsHangupWhileRunning(t *testing.T) {
	// The test's own sink keeps a SIGHUP from killing the test binary; it
	// sees the signal sent mid-call and the one sent again after the call.
	sink := make(chan os.Signal, 4)
	signal.Notify(sink, syscall.SIGHUP)
	defer signal.Stop(sink)

	be := fake.New()
	m := onOrders(t, be)
	var sent bool
	be.SetFault(fake.OpSend, fake.Fault{Hook: func() {
		if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
			t.Error(err)
		}
		<-sink                            // delivered
		time.Sleep(20 * time.Millisecond) // to every channel, holdSignals's too
		sent = true
	}})
	m = keysIn(t, m, "r", "y")
	if !sent || len(m.messages.items) != 36 || !strings.HasPrefix(m.status.text, "Resubmitted") {
		t.Fatalf("repair did not finish: sent %v status %q", sent, m.status.text)
	}
	select {
	case <-sink:
	case <-time.After(5 * time.Second):
		t.Fatal("the held SIGHUP was not sent again after the call")
	}
}

func TestFilterDefersQuitDuringCall(t *testing.T) {
	m := onOrders(t, fake.New())
	if got := Filter(m, tea.QuitMsg{}); got != (tea.QuitMsg{}) {
		t.Fatalf("idle: QuitMsg became %T", got)
	}

	// Confirm a Repair and hold its call: the batch is (call, spinner tick).
	next, cmd := keysIn(t, m, "r").Update(press("y"))
	m = next.(Model)
	for _, msg := range []tea.Msg{tea.QuitMsg{}, tea.InterruptMsg{}} {
		if _, ok := Filter(m, msg).(deferredQuitMsg); !ok {
			t.Fatalf("during the call: %T not deferred", msg)
		}
	}
	if got := Filter(m, tea.WindowSizeMsg{Width: 120, Height: 30}); got != (tea.WindowSizeMsg{Width: 120, Height: 30}) {
		t.Fatalf("during the call: WindowSizeMsg became %T", got)
	}
	next, _ = m.Update(Filter(m, tea.QuitMsg{}))
	m = next.(Model)
	if m.status.text != "quit after the call finishes" {
		t.Fatalf("status %q", m.status.text)
	}

	// The outcome is logged, then the program quits.
	next, quit := m.Update(cmd().(tea.BatchMsg)[0]())
	m = next.(Model)
	if quit == nil {
		t.Fatal("no quit after the outcome")
	}
	if msg := quit(); msg != (tea.QuitMsg{}) {
		t.Fatalf("after the outcome: %T, want QuitMsg", msg)
	}
	if last := m.log[len(m.log)-1].text; !strings.Contains(last, "Resubmitted") {
		t.Fatalf("last log line %q", last)
	}
}

// TestExitReportAfterDeferredQuit: the deferred call's outcome outlives the
// alt screen, and fails the exit when the message did not reach its end
// state.
func TestExitReportAfterDeferredQuit(t *testing.T) {
	if text, _ := onOrders(t, fake.New()).ExitReport(); text != "" {
		t.Fatalf("no deferred quit: report %q", text)
	}
	for _, tc := range []struct {
		name   string
		fault  fake.Fault
		want   []string
		failed bool
	}{
		{"Resubmitted", fake.Fault{}, []string{
			"lazybus quit after the call finished: Resubmitted orders/$DLQ seq 2 → queue orders",
			"\n  send orders/$DLQ seq 2 → queue orders",
			"\n  resubmit orders/$DLQ seq 2 → orders  Resubmitted",
		}, false},
		{"SendFailed", fake.Fault{Err: &bus.Error{Kind: bus.ErrUnauthorized, Msg: "unauthorized: 'Send' claim(s) are required to perform this operation. TrackingId:7f3a"}, Definite: true}, []string{
			"lazybus quit after the call finished: SendFailed (nothing changed): queue orders rejected the copy",
			"TrackingId:7f3a", // the full error, not the short detail
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be := fake.New()
			m := onOrders(t, be)
			be.SetFault(fake.OpSend, tc.fault)
			next, cmd := keysIn(t, m, "r").Update(press("y"))
			m = next.(Model)
			m = run(t, m, Filter(m, tea.QuitMsg{}))
			next, _ = m.Update(cmd().(tea.BatchMsg)[0]())
			text, failed := next.(Model).ExitReport()
			for _, w := range tc.want {
				if !strings.Contains(text, w) {
					t.Errorf("report lacks %q:\n%s", w, text)
				}
			}
			if failed != tc.failed {
				t.Errorf("failed = %v, want %v", failed, tc.failed)
			}
		})
	}
}

// TestRepairSIGTERMWaitsForOutcome runs the real program: SIGTERMs sent
// during the call (the first reaches Bubble Tea's handler, the second only
// holdSignals) end the program only after the outcome is in. It runs in a
// child process, so a SIGTERM that is not held kills the child, not the
// test binary.
func TestRepairSIGTERMWaitsForOutcome(t *testing.T) {
	if os.Getenv("LAZYBUS_SIGTERM_CHILD") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestRepairSIGTERMWaitsForOutcome$", "-test.count=1", "-test.v")
		cmd.Env = append(os.Environ(), "LAZYBUS_SIGTERM_CHILD=1")
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "--- PASS: TestRepairSIGTERMWaitsForOutcome") {
			t.Fatalf("child: %v\n%s", err, out)
		}
		return
	}

	be := fake.New()
	var sent atomic.Bool
	be.SetFault(fake.OpSend, fake.Fault{Hook: func() {
		for range 2 {
			if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
				t.Error(err)
			}
			time.Sleep(100 * time.Millisecond) // the quit arrives mid-call
		}
		sent.Store(true)
	}})
	out := &syncBuffer{}
	p := tea.NewProgram(New(be, testOptions()),
		tea.WithInput(bytes.NewBuffer(nil)),
		tea.WithOutput(out),
		tea.WithWindowSize(120, 30),
		tea.WithFilter(Filter),
	)
	done := make(chan tea.Model, 1)
	go func() {
		final, err := p.Run()
		if err != nil {
			t.Error(err)
		}
		done <- final
	}()
	shows := func(text string) {
		t.Helper()
		teatest.WaitFor(t, out, func(b []byte) bool {
			return strings.Contains(strings.Join(strings.Fields(ansi.Strip(string(b))), ""), text)
		}, teatest.WithDuration(5*time.Second), teatest.WithCheckInterval(10*time.Millisecond))
	}
	shows("peekinvoices/$DLQ→3")
	p.Send(press("3"))
	p.Send(press("r"))
	shows("┌Resubmit─")
	p.Send(press("y"))

	select {
	case final := <-done:
		m := final.(Model)
		if !sent.Load() || !strings.HasPrefix(m.status.text, "Resubmitted") {
			t.Fatalf("quit before the outcome: sent %v status %q", sent.Load(), m.status.text)
		}
	case <-time.After(10 * time.Second):
		p.Kill()
		t.Fatal("the program did not quit after the outcome")
	}
}

// syncBuffer is a bytes.Buffer safe for the program's writes and the
// test's reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Read(p)
}

func TestRepairRefusesUnsupportedBody(t *testing.T) {
	be := fake.New()
	m := onOrders(t, be)
	be.SetUnsupported("sb-prod-weu", "orders", bus.KindQueue, selectedSeq(t, m), "AMQP value body")
	be.ResetEvents()
	m = keysIn(t, m, "r")
	if !m.stack.AtRoot() || !strings.Contains(m.status.text, "unsupported AMQP body/message-id; not repaired (AMQP value body)") {
		t.Fatalf("top %v status %q", m.stack.Top(), m.status.text)
	}
	if len(be.Events()) != 0 || len(m.messages.items) != 37 {
		t.Fatalf("events %v, %d rows", be.Events(), len(m.messages.items))
	}
}

func TestRepairCleanupPendingThenFinishCleanup(t *testing.T) {
	be := fake.New()
	m := onOrders(t, be)
	seq := selectedSeq(t, m)
	be.SetFault(fake.OpComplete, fake.Fault{Err: errors.New("link detached")})
	active := activeCount(m, "orders")
	m = keysIn(t, m, "r", "y")
	if mark, ok := m.markOf(seq); !ok || mark.outcome != bus.CleanupPending || dlqCount(m, "orders") != 37 {
		t.Fatalf("mark %v %v count %d", mark, ok, dlqCount(m, "orders"))
	}

	be.ResetEvents()
	m = keysIn(t, m, "r")
	if !m.stack.AtRoot() || m.status.text != "copy already in target — press c to finish cleanup" || len(be.Events()) != 0 {
		t.Fatalf("r on CleanupPending: top %v status %q events %v", m.stack.Top(), m.status.text, be.Events())
	}

	be.SetFault(fake.OpComplete, fake.Fault{})
	m = keysIn(t, m, "c")
	if m.stack.Top() != CtxConfirm || m.confirm.kind != confirmCleanup {
		t.Fatalf("c: top %v", m.stack.Top())
	}
	m = keysIn(t, m, "y")
	if len(m.messages.items) != 36 || selectedSeq(t, m) == seq || len(m.marks) != 0 || dlqCount(m, "orders") != 36 {
		t.Fatalf("after Cleaned: %d rows, marks %v, count %d", len(m.messages.items), m.marks, dlqCount(m, "orders"))
	}
	if got := activeCount(m, "orders"); got != active {
		t.Fatalf("Finish Cleanup changed the active count: %d, want %d", got, active)
	}
	for _, e := range be.Events() {
		if e.Op == fake.OpSend {
			t.Fatal("Finish Cleanup sent a message")
		}
	}
	if !hasLog(m, "finish cleanup orders/$DLQ seq 2  Cleaned") {
		t.Fatalf("log %+v", m.log)
	}
}

func TestRepairRefusals(t *testing.T) {
	cases := []struct {
		name   string
		opts   func(*Options)
		keys   []string
		status string
	}{
		{"read-only r", func(o *Options) { o.ReadOnly = true }, []string{"3", "r"}, "read-only mode (--read-only): r is disabled"},
		{"read-only c", func(o *Options) { o.ReadOnly = true }, []string{"3", "c"}, "read-only mode (--read-only): c is disabled"},
		{"Active", nil, []string{"3", "tab", "r"}, "read-only: Active messages can't be repaired"},
		{"c on a plain row", nil, []string{"3", "c"}, "c finishes cleanup on a CleanupPending row only"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			be := fake.New()
			opts := testOptions()
			if tc.opts != nil {
				tc.opts(&opts)
			}
			m := New(be, opts)
			m = run(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
			m = runCmd(t, m, m.Init())
			m = keysIn(t, m, tc.keys...)
			if !m.stack.AtRoot() || m.status.text != tc.status || len(be.Events()) != 0 {
				t.Fatalf("top %v status %q events %v", m.stack.Top(), m.status.text, be.Events())
			}
			if m = keysIn(t, m, "j"); m.status.text != "" {
				t.Fatal("the next key did not clear the status")
			}
		})
	}
}

func TestConfirmDefaultsToCancel(t *testing.T) {
	be := fake.New()
	m := keysIn(t, onOrders(t, be), "r")
	be.ResetEvents()
	m = keysIn(t, m, "j", "x", "q", "2", "c", "enter")
	if m.stack.Top() != CtxConfirm || m.messages.cursor != 0 || len(be.Events()) != 0 {
		t.Fatalf("keys leaked past the confirm popup: top %v cursor %d", m.stack.Top(), m.messages.cursor)
	}
	for _, k := range []string{"n", "esc"} {
		m = keysIn(t, m, k)
		if !m.stack.AtRoot() || len(be.Events()) != 0 || len(m.messages.items) != 37 {
			t.Fatalf("%s: top %v events %v", k, m.stack.Top(), be.Events())
		}
		m = keysIn(t, m, "r")
	}
}

func TestRepairMessageIDToggle(t *testing.T) {
	be := fake.New()
	m := keysIn(t, startWith(t, be), "3") // invoices: duplicate detection
	orig := m.messages.items[0].MessageID
	m = keysIn(t, m, "r")
	if !m.confirm.newID || !m.confirm.plan.DuplicateDetection {
		t.Fatalf("dedup target: newID %v", m.confirm.newID)
	}
	m = keysIn(t, m, "m")
	if m.confirm.newID {
		t.Fatal("m did not switch to keeping the MessageId")
	}
	m = keysIn(t, m, "m", "m", "y")
	active := be.Messages("sb-prod-weu", "invoices", bus.KindQueue, bus.Active)
	if got := active[len(active)-1].MessageID; got != orig {
		t.Fatalf("copy MessageId %s, want kept %s", got, orig)
	}
}

func TestBusyIgnoresKeys(t *testing.T) {
	m := keysIn(t, onOrders(t, fake.New()), "r")
	next, cmd := m.Update(press("y"))
	m = next.(Model)
	if m.stack.Top() != CtxBusy || cmd == nil {
		t.Fatalf("y: top %v", m.stack.Top())
	}
	for _, k := range []string{"q", "esc", "j", "r"} {
		next, c := m.Update(press(k))
		if c != nil || next.(Model).stack.Top() != CtxBusy {
			t.Fatalf("%s while busy: cmd %v top %v", k, c != nil, next.(Model).stack.Top())
		}
	}
	m = runCmd(t, m, cmd)
	if !m.stack.AtRoot() || len(m.messages.items) != 36 {
		t.Fatalf("after the call: top %v rows %d", m.stack.Top(), len(m.messages.items))
	}
}

// TestRepairGoldens renders the confirm popups, outcome states and
// refusals at 120×30, driven synchronously.
func TestRepairGoldens(t *testing.T) {
	sendFault := func(err error, definite bool) func(*fake.Backend) {
		return func(be *fake.Backend) { be.SetFault(fake.OpSend, fake.Fault{Err: err, Definite: definite}) }
	}
	cleanupPending := func(be *fake.Backend) { be.SetFault(fake.OpComplete, fake.Fault{Err: errors.New("link detached")}) }
	rejected := &bus.Error{Kind: bus.ErrNotAllowed, Msg: "amqp:not-allowed: The message does not have a SessionId"}
	cases := []struct {
		name  string
		setup func(t *testing.T) Model
	}{
		{"confirm-queue", func(t *testing.T) Model { return keysIn(t, onOrders(t, fake.New()), "r") }},
		{"confirm-topic", func(t *testing.T) Model {
			return keysIn(t, startWith(t, fake.New()), "2", "j", "enter", "r")
		}},
		{"confirm-dedup", func(t *testing.T) Model { return keysIn(t, startWith(t, fake.New()), "3", "r") }},
		{"confirm-dedup-keep", func(t *testing.T) Model { return keysIn(t, startWith(t, fake.New()), "3", "r", "m") }},
		{"confirm-no-manage", func(t *testing.T) Model {
			// Listen+Send only: the target can't be read (HTTP 401).
			be := fake.New()
			be.SetFault(fake.OpTargetInfo, fake.Fault{Err: &bus.Error{Kind: bus.ErrUnauthorized, Op: "get topic order-events", Msg: "HTTP 401"}})
			return keysIn(t, startWith(t, be), "2", "j", "enter", "r")
		}},
		{"executing", func(t *testing.T) Model {
			m := keysIn(t, onOrders(t, fake.New()), "r")
			next, _ := m.Update(press("y"))
			return next.(Model)
		}},
		{"outcome-resubmitted", func(t *testing.T) Model { return keysIn(t, onOrders(t, fake.New()), "r", "y") }},
		{"outcome-notfound", func(t *testing.T) Model {
			be := fake.New()
			m := onOrders(t, be)
			removeOutOfBand(t, be, m, selectedSeq(t, m))
			return keysIn(t, m, "r")
		}},
		{"outcome-notfound-scan", func(t *testing.T) Model {
			be := fake.New()
			m := keysIn(t, onOrders(t, be), "j", "j", "j", "j", "j", "j", "j", "j", "j", "j", "j", "j")
			be.SetLockDuration(5 * time.Second)
			return keysIn(t, m, "r", "y")
		}},
		{"outcome-locklost", func(t *testing.T) Model {
			be := fake.New()
			m := onOrders(t, be)
			be.SetLockDuration(5 * time.Second)
			return keysIn(t, m, "r", "y")
		}},
		{"outcome-sendfailed", func(t *testing.T) Model {
			be := fake.New()
			m := onOrders(t, be)
			sendFault(rejected, true)(be)
			return keysIn(t, m, "r", "y")
		}},
		{"outcome-sendfailed-unauthorized", func(t *testing.T) Model {
			// S4: a Listen-only rule; the SDK's whole text goes to the log.
			be := fake.New()
			m := onOrders(t, be)
			sendFault(&bus.Error{Kind: bus.ErrUnauthorized, Op: "send to orders",
				Msg: "(unauthorized): *Error{Condition: amqp:unauthorized-access, Description: Unauthorized access. 'Send' claim(s) are required to perform this operation. Resource: 'sb://sb-prod-weu.servicebus.windows.net/orders'"}, true)(be)
			return keysIn(t, m, "r", "y")
		}},
		{"outcome-senduncertain", func(t *testing.T) Model {
			be := fake.New()
			m := onOrders(t, be)
			sendFault(context.DeadlineExceeded, false)(be)
			return keysIn(t, m, "r", "y")
		}},
		{"outcome-cleanuppending", func(t *testing.T) Model {
			be := fake.New()
			m := onOrders(t, be)
			cleanupPending(be)
			return keysIn(t, m, "r", "y")
		}},
		{"cleanup-blocked", func(t *testing.T) Model {
			be := fake.New()
			m := onOrders(t, be)
			cleanupPending(be)
			return keysIn(t, m, "r", "y", "r")
		}},
		{"confirm-cleanup", func(t *testing.T) Model {
			be := fake.New()
			m := onOrders(t, be)
			cleanupPending(be)
			m = keysIn(t, m, "r", "y")
			be.SetFault(fake.OpComplete, fake.Fault{})
			return keysIn(t, m, "c")
		}},
		{"outcome-cleaned", func(t *testing.T) Model {
			be := fake.New()
			m := onOrders(t, be)
			cleanupPending(be)
			m = keysIn(t, m, "r", "y")
			be.SetFault(fake.OpComplete, fake.Fault{})
			return keysIn(t, m, "c", "y")
		}},
		{"confirm-uncertain-retry", func(t *testing.T) Model {
			be := fake.New()
			m := onOrders(t, be)
			sendFault(context.DeadlineExceeded, false)(be)
			m = keysIn(t, m, "r", "y")
			be.SetFault(fake.OpSend, fake.Fault{})
			return keysIn(t, m, "r")
		}},
		{"refused-read-only", func(t *testing.T) Model {
			opts := testOptions()
			opts.ReadOnly = true
			m := New(fake.New(), opts)
			m = run(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
			m = runCmd(t, m, m.Init())
			return keysIn(t, m, "3", "r")
		}},
		{"refused-active", func(t *testing.T) Model { return keysIn(t, onOrders(t, fake.New()), "tab", "r") }},
	}
	for _, tc := range cases {
		name := tc.name + "-120x30"
		t.Run(name, func(t *testing.T) {
			screen := tc.setup(t).View().Content
			requireScreenSize(t, screen, 120, 30)
			requireGolden(t, name, screen)
		})
	}
}
