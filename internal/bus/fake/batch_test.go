package fake

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/samuelstrom93/lazybus/internal/bus"
)

func batchReq(ent bus.Entity, items ...bus.BatchItem) bus.BatchRequest {
	return bus.BatchRequest{Namespace: prodNS, Entity: ent, SubQueue: bus.DeadLetter, Items: items, MessageID: bus.MessageIDKeep}
}

func item(seq int64, index int) bus.BatchItem {
	return bus.BatchItem{SequenceNumber: seq, Index: index}
}

func repairBatch(t *testing.T, b *Backend, r bus.BatchRequest) bus.BatchResult {
	t.Helper()
	res, err := b.RepairBatch(context.Background(), r)
	if err != nil {
		t.Fatalf("repair batch: %v", err)
	}
	if len(res.Items) != len(r.Items) {
		t.Fatalf("%d results for %d items", len(res.Items), len(r.Items))
	}
	return res
}

func TestBatchOneScanForEveryMessage(t *testing.T) {
	b := New()
	orders := queue("orders")
	before, activeBefore := len(dlq(b, orders)), len(active(b, orders))
	withEdit := item(6, 4)
	withEdit.Edits = bus.Edits{Properties: []bus.PropertyEdit{{Key: "fixed", Type: bus.TypeBool, Value: true}}}

	res := repairBatch(t, b, batchReq(orders, item(2, 0), item(4, 2), withEdit))
	for i, r := range res.Items {
		if r.Outcome != bus.Resubmitted || r.Skipped != "" {
			t.Fatalf("item %d: %v %q (%s)", i, r.Outcome, r.Skipped, r.Detail)
		}
	}
	if got := len(dlq(b, orders)); got != before-3 {
		t.Fatalf("DLQ %d → %d", before, got)
	}
	if got := len(active(b, orders)); got != activeBefore+3 {
		t.Fatalf("active %d → %d", activeBefore, got)
	}
	// One pass: one batch of ScanBatch holds all three, and every other
	// message it locked is abandoned when the scan ends, before the first
	// send.
	var received []int64
	abandoned := map[int64]bool{}
	for _, e := range eventsBefore(b.Events(), OpSend) {
		switch e.Op {
		case OpReceive:
			received = append(received, e.Seq)
		case OpAbandon:
			abandoned[e.Seq] = true
		}
	}
	if len(received) != bus.ScanBatch || res.Locked != bus.ScanBatch {
		t.Fatalf("received %v, locked %d: %v", received, res.Locked, b.Events())
	}
	for _, seq := range received {
		target := seq == 2 || seq == 4 || seq == 6
		if abandoned[seq] == target {
			t.Fatalf("seq %d: abandoned %v before the sends: %v", seq, abandoned[seq], b.Events())
		}
	}
	copied := false
	for _, m := range active(b, orders) {
		for _, p := range m.Properties {
			copied = copied || p.Key == "fixed"
		}
	}
	if !copied {
		t.Fatal("the edit of seq 6 is not on its copy")
	}
}

// eventsBefore are the events before the first of op.
func eventsBefore(events []Event, op Op) []Event {
	for i, e := range events {
		if e.Op == op {
			return events[:i]
		}
	}
	return events
}

func TestBatchStopsAtSendUncertain(t *testing.T) {
	b := New()
	orders := queue("orders")
	b.SetFault(OpSend, Fault{Err: context.DeadlineExceeded, Seq: 4})
	res := repairBatch(t, b, batchReq(orders, item(2, 0), item(4, 2), item(6, 4)))
	if res.Items[0].Outcome != bus.Resubmitted || res.Items[1].Outcome != bus.SendUncertain {
		t.Fatalf("outcomes %v %v", res.Items[0].Outcome, res.Items[1].Outcome)
	}
	if !strings.Contains(res.Items[2].Skipped, "SendUncertain") {
		t.Fatalf("seq 6 not skipped: %+v", res.Items[2])
	}
	if hasEvent(b, Event{OpSend, "orders", 6}) {
		t.Fatal("seq 6 was sent after the batch stopped")
	}
	if _, ok := find(dlq(b, orders), 6); !ok || !hasEvent(b, Event{OpAbandon, "orders/$DLQ", 6}) {
		t.Fatalf("seq 6 not abandoned back to the DLQ: %v", b.Events())
	}
	if res.Stopped != 4 {
		t.Fatalf("stopped %d", res.Stopped)
	}
}

func TestBatchContinuesAfterSendFailedAndGone(t *testing.T) {
	b := New()
	orders := queue("orders")
	b.SetFault(OpSend, Fault{Err: errors.New("quota exceeded"), Definite: true, Seq: 4})
	res := repairBatch(t, b, batchReq(orders, item(2, 0), item(999, 1), item(4, 2), item(6, 4)))
	want := []bus.Outcome{bus.Resubmitted, bus.NotFound, bus.SendFailed, bus.Resubmitted}
	for i, r := range res.Items {
		if r.Outcome != want[i] || r.Skipped != "" {
			t.Fatalf("item %d: %v %q, want %v", i, r.Outcome, r.Skipped, want[i])
		}
	}
	if res.Items[1].NotFound != bus.NotFoundGone || res.Stopped != 0 {
		t.Fatalf("gone item %+v stopped %d", res.Items[1], res.Stopped)
	}
	if _, ok := find(dlq(b, orders), 4); !ok {
		t.Fatal("SendFailed original left the DLQ")
	}
}

func TestBatchRefusals(t *testing.T) {
	b := New()
	orders := queue("orders")
	var many []bus.BatchItem
	for i := range bus.MaxBatch + 1 {
		many = append(many, item(int64(i+2), i))
	}
	for name, r := range map[string]bus.BatchRequest{
		"too many":  batchReq(orders, many...),
		"duplicate": batchReq(orders, item(2, 0), item(2, 0)),
		"empty":     batchReq(orders),
	} {
		if _, err := b.RepairBatch(context.Background(), r); bus.KindOf(err) != bus.ErrRefused {
			t.Errorf("%s: err %v", name, err)
		}
	}
	if len(b.Events()) != 0 {
		t.Fatalf("a refused batch touched the broker: %v", b.Events())
	}

	// An unsupported body is skipped; the others go.
	b.SetUnsupported(prod, "orders", bus.KindQueue, 4, "amqp-value body")
	res := repairBatch(t, b, batchReq(orders, item(2, 0), item(4, 2)))
	if res.Items[0].Outcome != bus.Resubmitted || !strings.Contains(res.Items[1].Skipped, "unsupported") {
		t.Fatalf("items %+v", res.Items)
	}
}

func TestPlanBatch(t *testing.T) {
	b := New(WithDuplicateDetection(prod, "orders"))
	orders := queue("orders")
	plan, err := b.PlanBatch(context.Background(), batchReq(orders, item(2, 0), item(999, 1), item(6, 4)))
	if err != nil {
		t.Fatal(err)
	}
	if !plan.NewIDByDefault || plan.Target.Name != "orders" || plan.ScanLimit != bus.ScanLimit(4) {
		t.Fatalf("plan %+v", plan.RepairPlan)
	}
	if len(plan.Items) != 3 || !plan.Items[0].Found || plan.Items[1].Found || plan.Items[2].Message.SequenceNumber != 6 {
		t.Fatalf("items %+v", plan.Items)
	}
	if len(b.Events()) != 0 {
		t.Fatalf("the plan locked something: %v", b.Events())
	}
}

func TestBatchNotFoundInScan(t *testing.T) {
	b := New(WithDeadLetters(prod, "orders", 120))
	orders := queue("orders")
	// Seq 80 at row index 0 (stale list): the scan of 50 misses it, seq 2 goes.
	res := repairBatch(t, b, batchReq(orders, item(2, 0), item(80, 0)))
	if res.Items[0].Outcome != bus.Resubmitted {
		t.Fatalf("seq 2: %v (%s)", res.Items[0].Outcome, res.Items[0].Detail)
	}
	if r := res.Items[1]; r.Outcome != bus.NotFound || r.NotFound != bus.NotFoundScan || !strings.Contains(r.Detail, "first 50 messages") {
		t.Fatalf("seq 80: %v %v (%s)", r.Outcome, r.NotFound, r.Detail)
	}
	if res.Stopped != 0 {
		t.Fatalf("stopped %d", res.Stopped)
	}
}

func TestBatchEveryItemGoneLocksNothing(t *testing.T) {
	b := New()
	res := repairBatch(t, b, batchReq(queue("orders"), item(998, 0), item(999, 1)))
	for i, r := range res.Items {
		if r.Outcome != bus.NotFound || r.NotFound != bus.NotFoundGone || r.SequenceNumber != int64(998+i) {
			t.Fatalf("item %d: %+v", i, r)
		}
	}
	if len(b.Events()) != 0 || res.Locked != 0 {
		t.Fatalf("nothing to scan, but: events %v locked %d", b.Events(), res.Locked)
	}
}

func TestBatchErrorsSendNothing(t *testing.T) {
	boom := &bus.Error{Kind: bus.ErrConnection, Msg: "dial tcp: refused"}
	for _, op := range []Op{OpTargetInfo, OpPeek, OpReceive} {
		t.Run(op.String(), func(t *testing.T) {
			b := New()
			orders := queue("orders")
			b.SetFault(op, Fault{Err: boom})
			before := len(dlq(b, orders))
			res, err := b.RepairBatch(context.Background(), batchReq(orders, item(2, 0), item(4, 2)))
			if bus.KindOf(err) != bus.ErrConnection {
				t.Fatalf("err %v", err)
			}
			if len(res.Items) != 2 || res.Items[1].SequenceNumber != 4 || res.Items[1].Target.Name != "orders" {
				t.Fatalf("items %+v", res.Items)
			}
			for _, e := range b.Events() {
				if e.Op != OpReceive {
					t.Fatalf("a failed batch settled or sent: %v", b.Events())
				}
			}
			if got := len(dlq(b, orders)); got != before {
				t.Fatalf("DLQ %d → %d", before, got)
			}
		})
	}
}
