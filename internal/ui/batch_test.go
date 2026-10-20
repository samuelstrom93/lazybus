package ui

import (
	"context"
	"strings"
	"testing"

	"github.com/samuelstrom93/lazybus/internal/bus"
	"github.com/samuelstrom93/lazybus/internal/bus/fake"
)

func TestPickAndBulkResubmit(t *testing.T) {
	be := fake.New()
	m := onOrders(t, be)
	a := selectedSeq(t, m)
	m = keysIn(t, m, "space", "j", "j")
	b := selectedSeq(t, m)
	m = keysIn(t, m, "space")
	if got := m.pickedSeqs(); len(got) != 2 || got[0] != a || got[1] != b {
		t.Fatalf("picked %v, want [%d %d]", got, a, b)
	}
	m = keysIn(t, m, "r")
	if m.stack.Top() != CtxConfirm || m.confirm.kind != confirmBatch || len(m.confirm.batch.Items) != 2 {
		t.Fatalf("top %v kind %v items %d", m.stack.Top(), m.confirm.kind, len(m.confirm.batch.Items))
	}
	m = keysIn(t, m, "y")
	if len(m.messages.items) != 35 || len(m.pickedSeqs()) != 0 || dlqCount(m, "orders") != 35 {
		t.Fatalf("%d rows, picks %v, dlq %d", len(m.messages.items), m.pickedSeqs(), dlqCount(m, "orders"))
	}
	if !strings.HasPrefix(m.status.text, "Resubmit 2 → queue orders: 2 Resubmitted") || m.status.level != statusOK {
		t.Fatalf("status %q", m.status.text)
	}
	n := 0
	for _, e := range m.log {
		if strings.Contains(e.text, "→ orders  Resubmitted") {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("%d per-message log lines, want 2: %+v", n, m.log)
	}
}

func TestPickRangeReasonAndEsc(t *testing.T) {
	m := onOrders(t, fake.New())
	m = keysIn(t, m, "v", "j", "j")
	if len(m.pickedSeqs()) != 3 || !m.picks.ranging {
		t.Fatalf("range: %v", m.pickedSeqs())
	}
	m = keysIn(t, m, "esc")
	if len(m.pickedSeqs()) != 0 || m.stack.Top() != CtxMessages {
		t.Fatalf("esc cancels the range: %v top %v", m.pickedSeqs(), m.stack.Top())
	}
	m = keysIn(t, m, "v", "j", "v", "j")
	if len(m.pickedSeqs()) != 2 || m.picks.ranging {
		t.Fatalf("v keeps the range: %v", m.pickedSeqs())
	}
	m = keysIn(t, m, "esc")
	if len(m.pickedSeqs()) != 0 || m.stack.Top() != CtxMessages {
		t.Fatalf("esc clears picks: %v top %v", m.pickedSeqs(), m.stack.Top())
	}

	cur, _ := m.selectedMessage()
	want := 0
	for _, msg := range m.messages.items {
		if msg.DeadLetterReason == cur.DeadLetterReason {
			want++
		}
	}
	m = keysIn(t, m, "A")
	if got := len(m.pickedSeqs()); got != want || want < 2 {
		t.Fatalf("A picked %d, want %d", got, want)
	}
	if m = keysIn(t, m, "A"); len(m.pickedSeqs()) != 0 {
		t.Fatalf("A again unpicks: %v", m.pickedSeqs())
	}
}

func TestBulkResubmitStopsAtSendUncertain(t *testing.T) {
	be := fake.New()
	m := keysIn(t, onOrders(t, be), "v", "j", "j", "v")
	seqs := m.pickedSeqs()
	be.SetFault(fake.OpSend, fake.Fault{Err: context.DeadlineExceeded, Seq: seqs[1]})
	m = keysIn(t, m, "r", "y")
	if len(m.messages.items) != 36 {
		t.Fatalf("%d rows, want 36", len(m.messages.items))
	}
	if mk, ok := m.markOf(seqs[1]); !ok || mk.outcome != bus.SendUncertain {
		t.Fatalf("seq %d mark %v %v", seqs[1], mk, ok)
	}
	if _, ok := m.markOf(seqs[2]); ok {
		t.Fatalf("seq %d not attempted but marked", seqs[2])
	}
	if !strings.Contains(m.status.text, "1 Resubmitted, 1 SendUncertain, 1 not attempted") ||
		!strings.Contains(m.status.text, "stopped at seq") || m.status.level != statusWarn {
		t.Fatalf("status %q level %v", m.status.text, m.status.level)
	}
	if !hasLog(m, "not attempted") {
		t.Fatalf("log %+v", m.log)
	}

	// The marked row is refused in the next batch.
	m = keysIn(t, m, "g", "v", "j", "j", "v", "r")
	if m.stack.Top() != CtxMessages || !strings.Contains(m.status.text, "is SendUncertain") {
		t.Fatalf("top %v status %q", m.stack.Top(), m.status.text)
	}
}

func TestBulkResubmitRefusesOverCap(t *testing.T) {
	m := keysIn(t, onOrders(t, fake.New()), "v")
	for range bus.MaxBatch {
		m = keysIn(t, m, "j")
	}
	m = keysIn(t, m, "r")
	if m.stack.Top() != CtxMessages || !strings.Contains(m.status.text, "26 picked") {
		t.Fatalf("top %v status %q", m.stack.Top(), m.status.text)
	}
}

func TestBulkResubmitGoneRowsLeave(t *testing.T) {
	be := fake.New()
	m := keysIn(t, onOrders(t, be), "v", "j", "j", "v")
	seqs := m.pickedSeqs()
	removeOutOfBand(t, be, m, seqs[0])
	m = keysIn(t, m, "r")
	if m.stack.Top() != CtxConfirm || len(m.confirm.batch.Items) != 2 || len(m.messages.items) != 36 {
		t.Fatalf("top %v items %d rows %d", m.stack.Top(), len(m.confirm.batch.Items), len(m.messages.items))
	}
	m = keysIn(t, m, "y")
	if len(m.messages.items) != 34 || !strings.HasPrefix(m.status.text, "Resubmit 2 ") {
		t.Fatalf("rows %d status %q", len(m.messages.items), m.status.text)
	}
}

func TestPickRefusedOnActiveAndClearedByTab(t *testing.T) {
	m := keysIn(t, onOrders(t, fake.New()), "space", "tab", "space")
	if len(m.pickedSeqs()) != 0 || m.status.level != statusWarn {
		t.Fatalf("picks %v status %q", m.pickedSeqs(), m.status.text)
	}
	if m = keysIn(t, m, "tab"); len(m.pickedSeqs()) != 0 {
		t.Fatalf("back on DLQ: picks %v", m.pickedSeqs())
	}
}

func TestBulkResubmitStopHintAndRefusedLog(t *testing.T) {
	be := fake.New()
	m := keysIn(t, onOrders(t, be), "v", "j", "j", "v")
	seqs := m.pickedSeqs()
	be.SetUnsupported("sb-prod-weu", "orders", bus.KindQueue, seqs[2], "AmqpValue body")
	be.SetFault(fake.OpComplete, fake.Fault{Err: context.DeadlineExceeded, Seq: seqs[0]})
	m = keysIn(t, m, "r")
	if len(m.confirm.batch.Items) != 2 || !hasLog(m, "AmqpValue body") {
		t.Fatalf("items %d log %+v", len(m.confirm.batch.Items), m.log)
	}
	m = keysIn(t, m, "y")
	if !strings.Contains(m.status.text, "stopped at seq") || !strings.Contains(m.status.text, "press c") {
		t.Fatalf("status %q", m.status.text)
	}
}
