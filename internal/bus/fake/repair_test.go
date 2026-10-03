package fake

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samuelstrom93/lazybus/internal/bus"
)

const prod = "sb-prod-weu"

var prodNS = bus.Namespace{Name: prod, FQDN: prod + ".servicebus.windows.net"}

func queue(path string) bus.Entity { return bus.Entity{Path: path, Kind: bus.KindQueue} }
func sub(path string) bus.Entity   { return bus.Entity{Path: path, Kind: bus.KindSubscription} }

// req asks to repair the DLQ message with sequence number seq at row index.
func req(ent bus.Entity, seq int64, index int) bus.RepairRequest {
	return bus.RepairRequest{Namespace: prodNS, Entity: ent, SubQueue: bus.DeadLetter, SequenceNumber: seq, Index: index}
}

func repair(t *testing.T, b *Backend, r bus.RepairRequest) bus.RepairResult {
	t.Helper()
	res, err := b.Repair(context.Background(), r)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	return res
}

func dlq(b *Backend, ent bus.Entity) []bus.Message {
	return b.Messages(prod, ent.Path, ent.Kind, bus.DeadLetter)
}

func active(b *Backend, ent bus.Entity) []bus.Message {
	return b.Messages(prod, ent.Path, ent.Kind, bus.Active)
}

func seqs(ms []bus.Message) []int64 {
	out := make([]int64, len(ms))
	for i, m := range ms {
		out[i] = m.SequenceNumber
	}
	return out
}

func find(ms []bus.Message, seq int64) (bus.Message, bool) {
	for _, m := range ms {
		if m.SequenceNumber == seq {
			return m, true
		}
	}
	return bus.Message{}, false
}

// checkScanOrder checks the abandon rule: every abandon comes after the
// last receive (non-matches are held until the scan ends), the abandoned
// set is every locked message but the match (post-match siblings
// included), and nothing is abandoned twice.
func checkScanOrder(t *testing.T, events []Event, match int64) (received, abandoned []int64) {
	t.Helper()
	lastReceive, firstAbandon := -1, len(events)
	seen := map[int64]bool{}
	for i, e := range events {
		switch e.Op {
		case OpReceive:
			lastReceive = i
			received = append(received, e.Seq)
		case OpAbandon:
			firstAbandon = min(firstAbandon, i)
			if e.Seq == match {
				continue // the match itself on LockLost / Send*
			}
			if seen[e.Seq] {
				t.Errorf("seq %d abandoned twice", e.Seq)
			}
			seen[e.Seq] = true
			abandoned = append(abandoned, e.Seq)
		}
	}
	if firstAbandon < lastReceive {
		t.Errorf("abandon before the last receive: %v", events)
	}
	for _, s := range received {
		if s != match && !seen[s] {
			t.Errorf("locked seq %d never abandoned: %v", s, events)
		}
	}
	return received, abandoned
}

func TestRepairQueueResubmitted(t *testing.T) {
	b := New()
	orders := queue("orders")
	before := dlq(b, orders)
	orig, _ := find(before, 3) // MaxDeliveryCount shape: has a SessionId
	activeBefore := len(active(b, orders))

	res := repair(t, b, req(orders, 3, 1))
	if res.Outcome != bus.Resubmitted || res.Target != (bus.Target{Name: "orders", Kind: bus.TargetQueue}) {
		t.Fatalf("outcome %v target %v (%s)", res.Outcome, res.Target, res.Detail)
	}
	if res.OldMessageID != orig.MessageID || res.NewMessageID != orig.MessageID {
		t.Fatalf("MessageId %s → %s, want kept %s (no duplicate detection)", res.OldMessageID, res.NewMessageID, orig.MessageID)
	}
	after := dlq(b, orders)
	if len(after) != len(before)-1 {
		t.Fatalf("DLQ %d → %d", len(before), len(after))
	}
	if _, ok := find(after, 3); ok {
		t.Fatal("original still in the DLQ")
	}
	got := active(b, orders)
	if len(got) != activeBefore+1 {
		t.Fatalf("target active %d → %d", activeBefore, len(got))
	}
	cp := got[len(got)-1]
	if cp.SequenceNumber == orig.SequenceNumber || cp.DeliveryCount != 0 || !cp.EnqueuedTime.After(orig.EnqueuedTime) {
		t.Errorf("broker-owned fields copied: seq %d delivery %d enqueued %v", cp.SequenceNumber, cp.DeliveryCount, cp.EnqueuedTime)
	}
	if cp.DeadLetterReason != "" || cp.DeadLetterErrorDescription != "" || cp.DeadLetterSource != "" {
		t.Errorf("copy carries dead-letter markers: %+v", cp)
	}
	for _, p := range cp.Properties {
		if bus.IsMarker(p.Key) {
			t.Errorf("copy has marker property %s", p.Key)
		}
	}
	if !reflect.DeepEqual(cp.Properties, orig.Properties) {
		t.Errorf("properties (types) changed:\n got %+v\nwant %+v", cp.Properties, orig.Properties)
	}
	if cp.SessionID == "" || cp.SessionID != orig.SessionID {
		t.Errorf("SessionId %q, want %q", cp.SessionID, orig.SessionID)
	}
	if string(cp.Body) != string(orig.Body) || cp.Subject != orig.Subject || cp.ContentType != orig.ContentType ||
		cp.CorrelationID != orig.CorrelationID || cp.TimeToLive != orig.TimeToLive || cp.MessageID != orig.MessageID {
		t.Errorf("copied fields differ:\n got %+v\nwant %+v", cp, orig)
	}

	// One batch of 10 (seq 2–11): seq 2 before the match, 4–11 after it;
	// all abandoned at once, then send, then complete.
	ev := b.Events()
	received, abandoned := checkScanOrder(t, ev, 3)
	if len(received) != bus.ScanBatch || len(abandoned) != bus.ScanBatch-1 || res.Locked != bus.ScanBatch {
		t.Fatalf("received %v abandoned %v locked %d", received, abandoned, res.Locked)
	}
	tail := ev[len(ev)-2:]
	if tail[0] != (Event{OpSend, "orders", 3}) || tail[1] != (Event{OpComplete, "orders/$DLQ", 3}) {
		t.Fatalf("last events %v, want send then complete", tail)
	}
	// A DLQ abandon leaves DeliveryCount alone (as on Azure, S4).
	for _, s := range abandoned {
		m, _ := find(after, s)
		was, _ := find(before, s)
		if m.DeliveryCount != was.DeliveryCount {
			t.Errorf("seq %d DeliveryCount %d → %d, want unchanged", s, was.DeliveryCount, m.DeliveryCount)
		}
	}
	for _, want := range []string{
		"receive (peek-lock) orders/$DLQ → locked seq 2-11",
		"abandon orders/$DLQ seq 2,4-11 → ok",
		"send orders/$DLQ seq 3 → queue orders (MessageId ord-4003) → ok",
		"complete orders/$DLQ seq 3 → ok",
	} {
		if !hasLog(res, want) {
			t.Errorf("log lacks %q: %+v", want, res.Log)
		}
	}
}

func hasLog(res bus.RepairResult, want string) bool {
	for _, l := range res.Log {
		if l.Text == want {
			return true
		}
	}
	return false
}

func TestRepairHoldsLocksAcrossBatches(t *testing.T) {
	b := New()
	orders := queue("orders")
	res := repair(t, b, req(orders, 17, 15))
	if res.Outcome != bus.Resubmitted {
		t.Fatalf("outcome %v (%s)", res.Outcome, res.Detail)
	}
	// Batch 1 (2–11) has no match and stays locked while batch 2 (12–21)
	// is received; then 19 are abandoned at once.
	received, abandoned := checkScanOrder(t, b.Events(), 17)
	if len(received) != 20 || len(abandoned) != 19 {
		t.Fatalf("received %v abandoned %v", received, abandoned)
	}
	if got := seqs(dlq(b, orders)); got[0] != 2 || len(got) != 36 {
		t.Fatalf("DLQ after: %v", got)
	}
}

func TestRepairScanLimit(t *testing.T) {
	b := New(WithDeadLetters(prod, "orders", 120))
	orders := queue("orders")
	// Row index 0 but seq 80 (stale list): K = 50 messages, then NotFound.
	res := repair(t, b, req(orders, 80, 0))
	if res.Outcome != bus.NotFound || res.NotFound != bus.NotFoundScan || !strings.Contains(res.Detail, "first 50 messages") {
		t.Fatalf("outcome %v (%s)", res.Outcome, res.Detail)
	}
	received, abandoned := checkScanOrder(t, b.Events(), 80)
	if len(received) != 50 || len(abandoned) != 50 || res.Locked != 50 {
		t.Fatalf("received %d abandoned %d", len(received), len(abandoned))
	}
	if len(dlq(b, orders)) != 120 || len(res.AbandonErrors) != 0 {
		t.Fatalf("DLQ changed or abandon errors: %d %v", len(dlq(b, orders)), res.AbandonErrors)
	}
	for _, e := range b.Events() {
		if e.Op == OpSend || e.Op == OpComplete {
			t.Fatalf("NotFound sent or completed: %v", e)
		}
	}
}

// clock is a fake broker clock the test moves.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestRepairScanStopsBeforeFirstLockExpires(t *testing.T) {
	c := &clock{t: Epoch.Add(20 * time.Hour)}
	b := New(WithClock(c.now))
	b.SetLockDuration(15 * time.Second)
	// Every receive takes 3 s: the first lock (taken at +3 s, until +18 s)
	// is trusted until +8 s, so the scan stops after the third batch.
	b.SetFault(OpReceive, Fault{Hook: func() { c.add(3 * time.Second) }})
	orders := queue("orders")
	res := repair(t, b, req(orders, 35, 33))
	if res.Outcome != bus.NotFound || res.NotFound != bus.NotFoundScan || !strings.Contains(res.Detail, "lock") {
		t.Fatalf("outcome %v %v (%s)", res.Outcome, res.NotFound, res.Detail)
	}
	received, abandoned := checkScanOrder(t, b.Events(), 35)
	if len(received) != 30 || len(abandoned) != 30 || len(res.AbandonErrors) != 0 {
		t.Fatalf("received %d abandoned %d errors %v", len(received), len(abandoned), res.AbandonErrors)
	}
}

func TestRepairLockLost(t *testing.T) {
	b := New()
	b.SetLockDuration(5 * time.Second) // inside the 10 s margin
	orders := queue("orders")
	before := len(active(b, orders))
	res := repair(t, b, req(orders, 2, 0))
	if res.Outcome != bus.LockLost {
		t.Fatalf("outcome %v (%s)", res.Outcome, res.Detail)
	}
	_, abandoned := checkScanOrder(t, b.Events(), 2)
	if len(abandoned) != 9 || len(dlq(b, orders)) != 37 || len(active(b, orders)) != before {
		t.Fatalf("abandoned %d, DLQ %d, active %d", len(abandoned), len(dlq(b, orders)), len(active(b, orders)))
	}
	for _, e := range b.Events() {
		if e.Op == OpSend || e.Op == OpComplete {
			t.Fatalf("LockLost sent or completed: %v", e)
		}
	}
	if !hasEvent(b, Event{OpAbandon, "orders/$DLQ", 2}) {
		t.Fatal("match not abandoned")
	}
}

func hasEvent(b *Backend, want Event) bool {
	for _, e := range b.Events() {
		if e == want {
			return true
		}
	}
	return false
}

func TestRepairSendFailed(t *testing.T) {
	b := New()
	orders := queue("orders")
	rejected := &bus.Error{Kind: bus.ErrNotAllowed, Msg: "amqp:not-allowed: SessionId not set"}
	b.SetFault(OpSend, Fault{Err: rejected, Definite: true})
	res := repair(t, b, req(orders, 2, 0))
	if res.Outcome != bus.SendFailed || !errors.Is(res.Err, rejected) || res.Detail != rejected.Msg {
		t.Fatalf("outcome %v err %v detail %q", res.Outcome, res.Err, res.Detail)
	}
	if len(dlq(b, orders)) != 37 || len(active(b, orders)) != 120 || !hasEvent(b, Event{OpAbandon, "orders/$DLQ", 2}) {
		t.Fatal("SendFailed changed something or kept the lock")
	}

	// S4, a Listen-only rule: Detail is the short form, the log has the
	// SDK's whole text.
	long := &bus.Error{Kind: bus.ErrUnauthorized, Msg: "(unauthorized): *Error{Condition: amqp:unauthorized-access, " +
		"Description: Unauthorized access. 'Send' claim(s) are required to perform this operation. Resource: 'sb://x/orders'"}
	b.SetFault(OpSend, Fault{Err: long, Definite: true})
	res = repair(t, b, req(orders, 2, 0))
	if res.Detail != "unauthorized: 'Send' claim(s) are required to perform…" ||
		!hasLog(res, "send orders/$DLQ seq 2 → queue orders (MessageId ord-4002) → rejected: "+long.Msg) {
		t.Fatalf("detail %q log %+v", res.Detail, res.Log)
	}
}

func TestRepairSendUncertain(t *testing.T) {
	for _, delivered := range []bool{false, true} {
		b := New()
		orders := queue("orders")
		b.SetFault(OpSend, Fault{Err: context.DeadlineExceeded, After: delivered})
		res := repair(t, b, req(orders, 2, 0))
		if res.Outcome != bus.SendUncertain {
			t.Fatalf("delivered=%v: outcome %v (%s)", delivered, res.Outcome, res.Detail)
		}
		if _, ok := find(dlq(b, orders), 2); !ok || !hasEvent(b, Event{OpAbandon, "orders/$DLQ", 2}) {
			t.Fatalf("delivered=%v: original not back in the DLQ", delivered)
		}
		want := 120
		if delivered {
			want = 121
		}
		if got := len(active(b, orders)); got != want {
			t.Fatalf("delivered=%v: target has %d, want %d", delivered, got, want)
		}
		if hasEvent(b, Event{OpComplete, "orders/$DLQ", 2}) {
			t.Fatal("SendUncertain completed the original")
		}
	}
}

func TestRepairCleanupPendingThenFinishCleanup(t *testing.T) {
	b := New()
	orders := queue("orders")
	b.SetFault(OpComplete, Fault{Err: errors.New("link detached")})
	res := repair(t, b, req(orders, 4, 2))
	if res.Outcome != bus.CleanupPending || res.Err == nil {
		t.Fatalf("outcome %v (%s)", res.Outcome, res.Detail)
	}
	if len(active(b, orders)) != 121 {
		t.Fatal("copy not in the target")
	}
	if _, ok := find(dlq(b, orders), 4); !ok {
		t.Fatal("original not in the DLQ")
	}
	// No lock outlives the call: the failed complete is followed by an
	// abandon.
	if !hasEvent(b, Event{OpAbandon, "orders/$DLQ", 4}) {
		t.Fatalf("match still locked: %v", b.Events())
	}

	b.SetFault(OpComplete, Fault{})
	b.ResetEvents()
	plan, err := b.PlanCleanup(context.Background(), req(orders, 4, 2))
	if err != nil || !plan.Found || plan.ScanLimit != 52 {
		t.Fatalf("plan cleanup: %+v %v", plan, err)
	}
	done, err := b.FinishCleanup(context.Background(), req(orders, 4, 2))
	if err != nil || done.Outcome != bus.Cleaned {
		t.Fatalf("finish cleanup: %v %v (%s)", done.Outcome, err, done.Detail)
	}
	if _, ok := find(dlq(b, orders), 4); ok || len(active(b, orders)) != 121 {
		t.Fatal("finish cleanup did not remove the original, or sent")
	}
	for _, e := range b.Events() {
		if e.Op == OpSend {
			t.Fatal("finish cleanup sent")
		}
	}
	checkScanOrder(t, b.Events(), 4)

	again, err := b.FinishCleanup(context.Background(), req(orders, 4, 2))
	if err != nil || again.Outcome != bus.NotFound {
		t.Fatalf("second finish cleanup: %v %v", again.Outcome, err)
	}
}

func TestFinishCleanupLockLost(t *testing.T) {
	b := New()
	b.SetLockDuration(5 * time.Second)
	res, err := b.FinishCleanup(context.Background(), req(queue("orders"), 2, 0))
	if err != nil || res.Outcome != bus.LockLost || len(dlq(b, queue("orders"))) != 37 {
		t.Fatalf("outcome %v err %v", res.Outcome, err)
	}
}

func TestRepairNotFoundByPrecheck(t *testing.T) {
	b := New()
	res := repair(t, b, req(queue("orders"), 1, 0))
	if res.Outcome != bus.NotFound || res.NotFound != bus.NotFoundGone || len(b.Events()) != 0 {
		t.Fatalf("outcome %v %v events %v", res.Outcome, res.NotFound, b.Events())
	}
	plan, err := b.PlanRepair(context.Background(), req(queue("orders"), 1, 0))
	if err != nil || plan.Found {
		t.Fatalf("plan found a missing message: %+v %v", plan, err)
	}
}

func TestRepairAbandonErrorReported(t *testing.T) {
	b := New()
	boom := errors.New("abandon boom")
	b.SetFault(OpAbandon, Fault{Seq: 5, Err: boom})
	res := repair(t, b, req(queue("orders"), 3, 1))
	if res.Outcome != bus.Resubmitted {
		t.Fatalf("outcome %v", res.Outcome)
	}
	if len(res.AbandonErrors) != 1 || res.AbandonErrors[0].SequenceNumber != 5 || !errors.Is(res.AbandonErrors[0].Err, boom) {
		t.Fatalf("abandon errors %+v", res.AbandonErrors)
	}
	if !hasLog(res, "abandon orders/$DLQ seq 5 → error: abandon boom") {
		t.Fatalf("abandon error not logged: %+v", res.Log)
	}
}

func TestRepairDuplicateDetection(t *testing.T) {
	// invoices has duplicate detection; seq 2 was enqueued at Epoch+13h,
	// so one minute later its MessageId is inside the window.
	c := &clock{t: Epoch.Add(13*time.Hour + time.Minute)}
	invoices := queue("invoices")

	b := New(WithClock(c.now))
	plan, err := b.PlanRepair(context.Background(), req(invoices, 2, 0))
	if err != nil || !plan.DuplicateDetection || !plan.NewIDByDefault || plan.NewMessageID == "" {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	r := req(invoices, 2, 0)
	r.NewMessageID = plan.NewMessageID
	res := repair(t, b, r)
	if res.Outcome != bus.Resubmitted || res.NewMessageID != plan.NewMessageID || res.OldMessageID != "inv-1002" {
		t.Fatalf("default: %v %s → %s", res.Outcome, res.OldMessageID, res.NewMessageID)
	}
	got := active(b, invoices)
	if len(got) != 13 || got[12].MessageID != plan.NewMessageID {
		t.Fatalf("new-id copy did not arrive: %d", len(got))
	}
	if !hasLog(res, "send invoices/$DLQ seq 2 → queue invoices (MessageId inv-1002 → "+plan.NewMessageID+") → ok") {
		t.Fatalf("old → new not logged: %+v", res.Log)
	}

	// m: keep the original id. The broker accepts and drops the copy, and
	// the original is completed: the message is lost (S−1). The default
	// exists to prevent exactly this.
	b = New(WithClock(c.now))
	r = req(invoices, 2, 0)
	r.MessageID = bus.MessageIDKeep
	res = repair(t, b, r)
	if res.Outcome != bus.Resubmitted || res.NewMessageID != "inv-1002" {
		t.Fatalf("keep: %v %s", res.Outcome, res.NewMessageID)
	}
	if len(active(b, invoices)) != 12 || len(dlq(b, invoices)) != 2 {
		t.Fatalf("keep: duplicate not dropped: active %d DLQ %d", len(active(b, invoices)), len(dlq(b, invoices)))
	}

	// No duplicate detection: keep by default, m (New) asks for a new id.
	b = New()
	r = req(queue("orders"), 2, 0)
	r.MessageID = bus.MessageIDNew
	if res = repair(t, b, r); res.NewMessageID == res.OldMessageID || res.NewMessageID == "" {
		t.Fatalf("new on orders: %s → %s", res.OldMessageID, res.NewMessageID)
	}
}

func TestRepairSubscriptionToTopic(t *testing.T) {
	b := New()
	billing, shipping := sub("order-events/billing"), sub("order-events/shipping")
	plan, err := b.PlanRepair(context.Background(), req(billing, 2, 0))
	if err != nil || plan.Target != (bus.Target{Name: "order-events", Kind: bus.TargetTopic}) || plan.Subscriptions != 2 {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "delivered only to subscriptions whose rules match; no match = dropped") ||
		!strings.Contains(plan.Warnings[0], "2 subscriptions") {
		t.Fatalf("warnings %q", plan.Warnings)
	}
	if plan.ScanCost != "locks up to 50 messages ahead of it briefly; their DeliveryCount does not change" ||
		!reflect.DeepEqual(plan.MarkersRemoved, []string{"DeadLetterReason", "DeadLetterErrorDescription"}) {
		t.Fatalf("scan cost %q markers %v", plan.ScanCost, plan.MarkersRemoved)
	}
	res := repair(t, b, req(billing, 2, 0))
	if res.Outcome != bus.Resubmitted {
		t.Fatalf("outcome %v (%s)", res.Outcome, res.Detail)
	}
	// billing takes everything; shipping's rule (Subject = OrderShipped)
	// does not match an OrderPlaced message.
	if len(active(b, billing)) != 1 || len(active(b, shipping)) != 4 || len(dlq(b, billing)) != 11 {
		t.Fatalf("fan-out: billing %d shipping %d DLQ %d", len(active(b, billing)), len(active(b, shipping)), len(dlq(b, billing)))
	}
}

func TestRepairTargetInfoUnauthorized(t *testing.T) {
	// Listen+Send without Manage: the admin API refuses to read the target
	// (HTTP 401, S4), while peek, receive and send work.
	b := New()
	ctx := context.Background()
	b.SetFault(OpTargetInfo, Fault{Err: &bus.Error{Kind: bus.ErrUnauthorized, Op: "get queue orders", Msg: "HTTP 401"}})
	orders := queue("orders")
	plan, err := b.PlanRepair(ctx, req(orders, 2, 0))
	if err != nil || !plan.TargetUnknown || !plan.DuplicateDetection || !plan.NewIDByDefault || plan.NewMessageID == "" {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	if len(plan.Warnings) != 1 || !strings.HasPrefix(plan.Warnings[0], "duplicate detection unknown (no Manage rights): new MessageId") {
		t.Fatalf("warnings %q", plan.Warnings)
	}
	r := req(orders, 2, 0)
	r.NewMessageID = plan.NewMessageID
	res := repair(t, b, r)
	if res.Outcome != bus.Resubmitted || res.OldMessageID != "ord-4002" || res.NewMessageID != plan.NewMessageID {
		t.Fatalf("default: %v %s → %s (%s)", res.Outcome, res.OldMessageID, res.NewMessageID, res.Detail)
	}
	// m still keeps the original id.
	r = req(orders, 3, 0)
	r.MessageID = bus.MessageIDKeep
	if res = repair(t, b, r); res.Outcome != bus.Resubmitted || res.NewMessageID != "ord-4003" {
		t.Fatalf("keep: %v %s", res.Outcome, res.NewMessageID)
	}

	// A subscription DLQ: the parent topic's subscriptions can't be
	// counted, so the zero-subscription guard is skipped.
	plan, err = b.PlanRepair(ctx, req(sub("order-events/billing"), 2, 0))
	if err != nil || !plan.TargetUnknown || len(plan.Warnings) != 2 ||
		!strings.HasPrefix(plan.Warnings[0], "fans out to an unknown number of subscriptions (no Manage rights): delivered only") {
		t.Fatalf("topic plan: %+v %v", plan, err)
	}

	// Any other failure to read the target still stops the repair.
	b.SetFault(OpTargetInfo, Fault{Err: &bus.Error{Kind: bus.ErrConnection, Msg: "dial tcp: refused"}})
	b.ResetEvents()
	if _, err := b.Repair(ctx, req(orders, 4, 0)); bus.KindOf(err) != bus.ErrConnection || len(b.Events()) != 0 {
		t.Fatalf("connection error: %v, events %v", err, b.Events())
	}
}

func TestRepairGuards(t *testing.T) {
	b := New(WithEmptyTopic(prod, "empty-topic"))
	ctx := context.Background()
	cases := []struct {
		name string
		r    bus.RepairRequest
		want string
	}{
		{"active source", func() bus.RepairRequest { r := req(queue("orders"), 2, 0); r.SubQueue = bus.Active; return r }(), "dead-letter"},
		{"zero-subscription topic", req(sub("empty-topic/gone"), 2, 0), "no subscriptions"},
		{"missing target", req(queue("nope"), 2, 0), "does not exist"},
		{"marker edit", func() bus.RepairRequest {
			r := req(queue("orders"), 2, 0)
			r.Edits.Properties = []bus.PropertyEdit{{Key: bus.MarkerDeadLetterReason, Remove: true}}
			return r
		}(), "dead-letter markers"},
	}
	for _, tc := range cases {
		for name, call := range map[string]func() error{
			"plan":   func() error { _, err := b.PlanRepair(ctx, tc.r); return err },
			"repair": func() error { _, err := b.Repair(ctx, tc.r); return err },
		} {
			err := call()
			if bus.KindOf(err) != bus.ErrRefused || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s %s: err %v, want refused %q", tc.name, name, err, tc.want)
			}
		}
	}
	if len(b.Events()) != 0 {
		t.Fatalf("a refused request touched the broker: %v", b.Events())
	}
	if _, err := b.FinishCleanup(ctx, cases[0].r); bus.KindOf(err) != bus.ErrRefused {
		t.Fatalf("finish cleanup on Active: %v", err)
	}
}

func TestRepairWithEdits(t *testing.T) {
	b := New()
	orders := queue("orders")
	orig, _ := find(dlq(b, orders), 2) // SchemaMismatch shape: all seven property types
	activeBefore := len(active(b, orders))
	subject, contentType := "OrderFixed", "text/plain"
	r := req(orders, 2, 0)
	r.Edits = bus.Edits{
		Properties: []bus.PropertyEdit{
			{Key: "orderId", Type: bus.TypeInt, Value: int32(7)},
			{Key: "isRetry", Remove: true},
			{Key: "region", Type: bus.TypeGUID, Value: "6f1c2b9e-4d2a-4c1e-9b7a-000000000009"},
		},
		Subject: &subject, ContentType: &contentType,
		Body: []byte("fixed"), BodyEdited: true,
	}
	res := repair(t, b, r)
	// The send's log line counts the edits: 3 Property Edits, Subject,
	// ContentType, body.
	if res.Outcome != bus.Resubmitted || !hasLine(res.Log, "(MessageId ord-4002, 6 edits) → ok") {
		t.Fatalf("outcome %v (%s), log %+v", res.Outcome, res.Detail, res.Log)
	}
	got := active(b, orders)
	if len(got) != activeBefore+1 {
		t.Fatalf("target active %d → %d", activeBefore, len(got))
	}
	cp := got[len(got)-1]
	want := r.Edits.ApplyProperties(orig.Properties)
	if !reflect.DeepEqual(cp.Properties, want) {
		t.Fatalf("properties:\n got %+v\nwant %+v", cp.Properties, want)
	}
	if pe := cp.Properties[1]; pe.Key != "orderId" || pe.Type != bus.TypeInt {
		t.Fatalf("changed key not in place with its new type: %+v", pe)
	}
	if string(cp.Body) != "fixed" || cp.Subject != "OrderFixed" || cp.ContentType != "text/plain" ||
		cp.CorrelationID != orig.CorrelationID || cp.MessageID != orig.MessageID {
		t.Fatalf("copy = %+v", cp)
	}
}

func hasLine(log []bus.LogLine, text string) bool {
	for _, l := range log {
		if strings.Contains(l.Text, text) {
			return true
		}
	}
	return false
}

func TestRepairReceiveErrorAbandonsHeld(t *testing.T) {
	c := &clock{t: Epoch.Add(20 * time.Hour)}
	b := New(WithClock(c.now))
	orders := queue("orders")
	calls := 0
	boom := errors.New("link detached")
	// The first batch succeeds, the second fails. In the worst case the
	// failed receive takes the link down, so abandoning the 10 held
	// messages on the same receiver fails: every failure is reported,
	// nothing panics, and the receiver is still closed (the next call can
	// scan).
	b.SetFault(OpReceive, Fault{Hook: func() {
		if calls++; calls == 1 {
			b.SetFault(OpReceive, Fault{Err: boom}) // from the next call on
		}
	}})
	res, err := b.Repair(context.Background(), req(orders, 20, 18))
	if !errors.Is(err, boom) {
		t.Fatalf("err %v", err)
	}
	if len(res.AbandonErrors) != 10 {
		t.Fatalf("abandon errors %v", res.AbandonErrors)
	}
	for i, ae := range res.AbandonErrors {
		if ae.SequenceNumber != int64(i+2) || !strings.Contains(ae.Err.Error(), "link detached") {
			t.Errorf("abandon error %d: %+v", i, ae)
		}
	}
	for _, e := range b.Events() {
		if e.Op != OpReceive {
			t.Fatalf("settled on a broken receiver: %v", b.Events())
		}
	}

	b.SetFault(OpReceive, Fault{})
	c.add(2 * time.Minute) // the stranded locks expire
	if res, err := b.Repair(context.Background(), req(orders, 20, 18)); err != nil || res.Outcome != bus.Resubmitted {
		t.Fatalf("next repair: %v %v", res.Outcome, err)
	}
}

func TestRepairExpiredContextBeforeSendIsLockLost(t *testing.T) {
	b := New()
	orders := queue("orders")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The call's time runs out while the scan's siblings are abandoned,
	// after the match is locked and before the send.
	b.SetFault(OpAbandon, Fault{Hook: cancel})
	res, err := b.Repair(ctx, req(orders, 2, 0))
	if err != nil || res.Outcome != bus.LockLost || !strings.Contains(res.Detail, "ran out of time") {
		t.Fatalf("outcome %v err %v (%s)", res.Outcome, err, res.Detail)
	}
	for _, e := range b.Events() {
		if e.Op == OpSend || e.Op == OpComplete {
			t.Fatalf("sent or completed after the context expired: %v", b.Events())
		}
	}
	if _, ok := find(dlq(b, orders), 2); !ok || len(active(b, orders)) != 120 {
		t.Fatal("the original left the DLQ or a copy was sent")
	}
}

func TestRepairRefusesUnsupportedBody(t *testing.T) {
	b := New()
	orders := queue("orders")
	b.SetUnsupported(prod, "orders", bus.KindQueue, 2, "AMQP value body")
	for name, call := range map[string]func() error{
		"plan":   func() error { _, err := b.PlanRepair(context.Background(), req(orders, 2, 0)); return err },
		"repair": func() error { _, err := b.Repair(context.Background(), req(orders, 2, 0)); return err },
	} {
		err := call()
		if bus.KindOf(err) != bus.ErrRefused || !strings.Contains(err.Error(), "unsupported AMQP body/message-id; not repaired") {
			t.Errorf("%s: err %v", name, err)
		}
	}
	if len(b.Events()) != 0 {
		t.Fatalf("a refused repair locked: %v", b.Events())
	}
	if res := repair(t, b, req(orders, 3, 1)); res.Outcome != bus.Resubmitted {
		t.Fatalf("a supported neighbour: %v", res.Outcome)
	}
}

func TestRepairSettlePanicIsReported(t *testing.T) {
	b := New()
	orders := queue("orders")
	b.SetFault(OpAbandon, Fault{Hook: func() { panic("nil receiver") }})
	res := repair(t, b, req(orders, 2, 0))
	if res.Outcome != bus.Resubmitted || len(res.AbandonErrors) != 9 {
		t.Fatalf("outcome %v abandon errors %v", res.Outcome, res.AbandonErrors)
	}
	if !strings.Contains(res.AbandonErrors[0].Err.Error(), "internal error: nil receiver") {
		t.Fatalf("abandon error %v", res.AbandonErrors[0].Err)
	}
}
