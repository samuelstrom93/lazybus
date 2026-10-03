//go:build azure

// Real-namespace E2E against a Standard-tier Service Bus namespace:
//
//	LAZYBUS_AZURE_NAMESPACE=<name>.servicebus.windows.net go test -tags azure ./...
//
// Skipped when LAZYBUS_AZURE_NAMESPACE is unset. It authenticates with the
// az CLI credential (az login); the account needs Azure Service Bus Data
// Owner on the namespace. It creates the entities it needs when they are
// missing (all named lazybus-e2e-*, kept between runs), never touches any
// other entity, and drains its entities before and after each subtest.
// Results are recorded in docs/spike-s-1.md ("S4 real-namespace
// verification").
package azure_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus/admin"

	"github.com/samuelstrom93/lazybus/internal/bus"
	"github.com/samuelstrom93/lazybus/internal/bus/azure"
	"github.com/samuelstrom93/lazybus/internal/seed"
)

// Test entities. Every name starts with e2ePrefix; drain refuses others.
const (
	e2ePrefix = "lazybus-e2e-"

	qMain  = "lazybus-e2e-q"      // LockDuration 30 s
	qDedup = "lazybus-e2e-dedup"  // duplicate detection, window 10 min
	qPart  = "lazybus-e2e-part"   // partitioned
	qMDC   = "lazybus-e2e-mdc"    // MaxDeliveryCount 1
	qFwd   = "lazybus-e2e-dlfwd"  // ForwardDeadLetteredMessagesTo qSink
	qSink  = "lazybus-e2e-dlsink" //
	topic  = "lazybus-e2e-topic"
	subAll = "lazybus-e2e-all"     // $Default rule (true filter)
	subOff = "lazybus-e2e-shipped" // eventType = 'OrderShipped'

	listenRule = "lazybus-e2e-listen" // Listen-only SAS rule on qMain
)

// loc is a queue or a topic subscription of the test namespace.
type loc struct{ queue, topic, sub string }

func queue(name string) loc     { return loc{queue: name} }
func subscription(s string) loc { return loc{topic: topic, sub: s} }
func (l loc) entity() bus.Entity {
	if l.queue != "" {
		return bus.Entity{Path: l.queue, Kind: bus.KindQueue}
	}
	return bus.Entity{Path: l.topic + "/" + l.sub, Kind: bus.KindSubscription}
}
func (l loc) String() string { return l.entity().Path }

type azEnv struct {
	fqdn  string
	admin *admin.Client
	sdk   *azservicebus.Client
	b     *azure.Backend
	ns    bus.Namespace
}

func setupAzure(t *testing.T) *azEnv {
	t.Helper()
	fqdn := os.Getenv("LAZYBUS_AZURE_NAMESPACE")
	if fqdn == "" {
		t.Skip("LAZYBUS_AZURE_NAMESPACE not set")
	}
	raw, err := azidentity.NewAzureCLICredential(nil)
	if err != nil {
		t.Fatal(err)
	}
	cred := azure.CachedCredential(raw)
	e := &azEnv{fqdn: fqdn, b: azure.New()}
	t.Cleanup(func() { _ = e.b.Close(context.Background()) })
	if e.ns, err = e.b.AddNamespace(fqdn, cred); err != nil {
		t.Fatal(err)
	}
	if e.admin, err = admin.NewClient(fqdn, cred, nil); err != nil {
		t.Fatal(err)
	}
	if e.sdk, err = azservicebus.NewClient(fqdn, cred, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.sdk.Close(context.Background()) })
	e.ensureEntities(t)
	return e
}

func ptr[T any](v T) *T { return &v }

// ensureEntities creates the test entities that are missing.
func (e *azEnv) ensureEntities(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	queues := []struct {
		name  string
		props *admin.QueueProperties
	}{
		{qMain, &admin.QueueProperties{LockDuration: ptr("PT30S")}},
		{qDedup, &admin.QueueProperties{RequiresDuplicateDetection: ptr(true), DuplicateDetectionHistoryTimeWindow: ptr("PT10M")}},
		{qPart, &admin.QueueProperties{EnablePartitioning: ptr(true)}},
		{qMDC, &admin.QueueProperties{MaxDeliveryCount: ptr(int32(1)), LockDuration: ptr("PT30S")}},
		{qSink, nil},
		// The forward target must be an absolute URI.
		{qFwd, &admin.QueueProperties{ForwardDeadLetteredMessagesTo: ptr("https://" + e.fqdn + "/" + qSink)}},
	}
	for _, q := range queues {
		got, err := e.admin.GetQueue(ctx, q.name, nil)
		if err != nil {
			t.Fatalf("get queue %s: %v", q.name, err)
		}
		if got != nil { // (nil, nil) for a missing entity
			continue
		}
		if _, err := e.admin.CreateQueue(ctx, q.name, &admin.CreateQueueOptions{Properties: q.props}); err != nil {
			t.Fatalf("create queue %s: %v", q.name, err)
		}
		t.Logf("created queue %s", q.name)
	}
	if got, err := e.admin.GetTopic(ctx, topic, nil); err != nil {
		t.Fatalf("get topic: %v", err)
	} else if got == nil {
		if _, err := e.admin.CreateTopic(ctx, topic, nil); err != nil {
			t.Fatalf("create topic: %v", err)
		}
		t.Logf("created topic %s", topic)
	}
	subs := []struct {
		name string
		rule *admin.RuleProperties
	}{
		{subAll, nil},
		{subOff, &admin.RuleProperties{Name: "$Default", Filter: &admin.SQLFilter{Expression: "eventType = 'OrderShipped'"}}},
	}
	for _, s := range subs {
		got, err := e.admin.GetSubscription(ctx, topic, s.name, nil)
		if err != nil {
			t.Fatalf("get subscription %s: %v", s.name, err)
		}
		if got != nil {
			continue
		}
		if _, err := e.admin.CreateSubscription(ctx, topic, s.name, &admin.CreateSubscriptionOptions{
			Properties: &admin.SubscriptionProperties{DefaultRule: s.rule},
		}); err != nil {
			t.Fatalf("create subscription %s: %v", s.name, err)
		}
		t.Logf("created subscription %s/%s", topic, s.name)
	}
}

func (e *azEnv) receiver(t *testing.T, l loc, opts *azservicebus.ReceiverOptions) *azservicebus.Receiver {
	t.Helper()
	var r *azservicebus.Receiver
	var err error
	if l.queue != "" {
		r, err = e.sdk.NewReceiverForQueue(l.queue, opts)
	} else {
		r, err = e.sdk.NewReceiverForSubscription(l.topic, l.sub, opts)
	}
	if err != nil {
		t.Fatalf("receiver %s: %v", l, err)
	}
	return r
}

// drain deletes every message of l (or its DLQ) with receive-and-delete
// until a peek finds it empty. It refuses entities outside lazybus-e2e-*.
func (e *azEnv) drain(t *testing.T, l loc, dlq bool) int {
	t.Helper()
	for _, name := range []string{l.queue, l.topic} {
		if name != "" && !strings.HasPrefix(name, e2ePrefix) {
			t.Fatalf("drain %s: not a %s* entity", name, e2ePrefix)
		}
	}
	opts := &azservicebus.ReceiverOptions{ReceiveMode: azservicebus.ReceiveModeReceiveAndDelete}
	if dlq {
		opts.SubQueue = azservicebus.SubQueueDeadLetter
	}
	r := e.receiver(t, l, opts)
	defer r.Close(context.Background())
	n := 0
	for range 20 {
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		from := int64(0)
		left, err := r.PeekMessages(ctx, 1, &azservicebus.PeekMessagesOptions{FromSequenceNumber: &from})
		cancel()
		if err != nil {
			t.Fatalf("drain %s (dlq %v): peek: %v", l, dlq, err)
		}
		if len(left) == 0 {
			return n
		}
		ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
		got, err := r.ReceiveMessages(ctx, 100, nil)
		cancel()
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("drain %s (dlq %v): %v", l, dlq, err)
		}
		n += len(got)
	}
	t.Fatalf("drain %s (dlq %v): still not empty", l, dlq)
	return n
}

func (e *azEnv) drainAll(t *testing.T, ls ...loc) {
	t.Helper()
	for _, l := range ls {
		e.drain(t, l, false)
		e.drain(t, l, true)
	}
}

// send sends msgs to a queue or topic in batches.
func (e *azEnv) send(t *testing.T, to string, msgs []seed.Message) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*opTimeout)
	defer cancel()
	s, err := e.sdk.NewSender(to, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	batch, err := s.NewMessageBatch(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		out := &azservicebus.Message{
			MessageID: &m.MessageID, Body: m.Body, ApplicationProperties: m.Properties,
			Subject: optional(m.Subject), ContentType: optional(m.ContentType), CorrelationID: optional(m.CorrelationID),
		}
		if err := batch.AddMessage(out, nil); errors.Is(err, azservicebus.ErrMessageTooLarge) {
			if err := s.SendMessageBatch(ctx, batch, nil); err != nil {
				t.Fatalf("send to %s: %v", to, err)
			}
			if batch, err = s.NewMessageBatch(ctx, nil); err != nil {
				t.Fatal(err)
			}
			if err := batch.AddMessage(out, nil); err != nil {
				t.Fatal(err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SendMessageBatch(ctx, batch, nil); err != nil {
		t.Fatalf("send to %s: %v", to, err)
	}
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// deadLetter receives msgs from l in peek-lock and dead-letters each with
// its reason and description.
func (e *azEnv) deadLetter(t *testing.T, l loc, msgs []seed.Message) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*opTimeout)
	defer cancel()
	r := e.receiver(t, l, nil)
	defer r.Close(context.Background())
	want := map[string]seed.Message{}
	for _, m := range msgs {
		want[m.MessageID] = m
	}
	for len(want) > 0 {
		got, err := r.ReceiveMessages(ctx, min(len(want), 50), nil)
		if err != nil {
			t.Fatalf("dead-letter from %s (%d missing): %v", l, len(want), err)
		}
		for _, rm := range got {
			m, ok := want[rm.MessageID]
			if !ok {
				t.Fatalf("dead-letter from %s: unexpected message %s", l, rm.MessageID)
			}
			if err := r.DeadLetterMessage(ctx, rm, &azservicebus.DeadLetterOptions{
				Reason: optional(m.Reason), ErrorDescription: optional(m.Description),
			}); err != nil {
				t.Fatalf("dead-letter %s: %v", m.MessageID, err)
			}
			delete(want, rm.MessageID)
		}
	}
}

// seedDLQ sends msgs to the entity of l and dead-letters them there.
func (e *azEnv) seedDLQ(t *testing.T, l loc, msgs []seed.Message) {
	t.Helper()
	to := l.queue
	if to == "" {
		to = l.topic
	}
	e.send(t, to, msgs)
	e.deadLetter(t, l, msgs)
}

// eventually polls f every second until it returns "" or d passes, then
// fails with f's last answer.
func eventually(t *testing.T, d time.Duration, f func() string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		why := f()
		if why == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(why)
		}
		time.Sleep(time.Second)
	}
}

func (e *azEnv) entity(t *testing.T, l loc) bus.Entity {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	ents, err := e.b.ListEntities(ctx, e.ns)
	if err != nil {
		t.Fatalf("list entities: %v", err)
	}
	for _, ent := range ents {
		if ent.Path == l.entity().Path && ent.Kind == l.entity().Kind {
			return ent
		}
	}
	t.Fatalf("list entities: %s missing", l)
	return bus.Entity{}
}

func deliveryCounts(ms []bus.Message) map[string]uint32 {
	out := map[string]uint32{}
	for _, m := range ms {
		out[m.MessageID] = m.DeliveryCount
	}
	return out
}

func TestAzure(t *testing.T) {
	e := setupAzure(t)
	all := []loc{queue(qMain), queue(qDedup), queue(qPart), queue(qMDC), queue(qSink), subscription(subAll), subscription(subOff)}
	t.Cleanup(func() {
		e.drainAll(t, all...)
		e.drain(t, queue(qFwd), false) // its DLQ forwards; no receiver allowed there
	})

	t.Run("queue repair", func(t *testing.T) {
		q := queue(qMain)
		e.drainAll(t, q)
		e.seedDLQ(t, q, seed.QueueMessages())
		eventually(t, 30*time.Second, func() string {
			if got := e.entity(t, q); !got.CountsKnown || got.DeadLetterCount != 4 || got.ActiveCount != 0 {
				return fmt.Sprintf("runtime counts %+v, want DLQ 4, active 0", got)
			}
			return ""
		})

		before := peekAll(t, e.b, e.ns, q.entity(), bus.DeadLetter)
		if len(before) != 4 {
			t.Fatalf("%s/$DLQ has %d", q, len(before))
		}
		for _, m := range before {
			if m.DeadLetterSource != "" {
				t.Errorf("%s: DeadLetterSource %q after an explicit dead-letter, want empty", m.MessageID, m.DeadLetterSource)
			}
		}
		target := before[1]
		p := plan(t, e.b, repairReq(e.ns, q.entity(), target.SequenceNumber))
		if p.Target != (bus.Target{Name: qMain, Kind: bus.TargetQueue}) || p.DuplicateDetection || p.NewIDByDefault {
			t.Fatalf("plan: %+v", p)
		}
		r := repairReq(e.ns, q.entity(), target.SequenceNumber)
		r.Index = 1
		res := doRepair(t, e.b, r)
		if res.NewMessageID != target.MessageID || res.Locked < 2 {
			t.Fatalf("MessageId %s → %s, locked %d", target.MessageID, res.NewMessageID, res.Locked)
		}

		// The siblings the scan locked and abandoned: DeliveryCount
		// unchanged on a real DLQ (S4).
		after := peekAll(t, e.b, e.ns, q.entity(), bus.DeadLetter)
		was, now := deliveryCounts(before), deliveryCounts(after)
		t.Logf("DeliveryCount before %v, after %v (scan locked %d)", was, now, res.Locked)
		if len(after) != 3 {
			t.Fatalf("%s/$DLQ: %d → %d", q, len(before), len(after))
		}
		for id, dc := range now {
			if was[id] != dc {
				t.Errorf("%s: DeliveryCount %d → %d after the scan", id, was[id], dc)
			}
		}

		// No sibling stays locked: a new receiver gets all of them at once
		// (the scan's last receive left credits on its link; S4 measured
		// stranded locks without the close wait).
		start := time.Now()
		rcv := e.receiver(t, q, &azservicebus.ReceiverOptions{SubQueue: azservicebus.SubQueueDeadLetter, ReceiveMode: azservicebus.ReceiveModeReceiveAndDelete})
		got := 0
		for got < len(after) && time.Since(start) < 5*time.Second {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second-time.Since(start))
			ms, _ := rcv.ReceiveMessages(ctx, 10, nil)
			cancel()
			got += len(ms)
		}
		rcv.Close(context.Background())
		if got != len(after) {
			t.Fatalf("only %d of %d abandoned siblings receivable within 5 s: the rest are still locked", got, len(after))
		}
		t.Logf("all %d siblings receivable after %v", got, time.Since(start).Round(time.Millisecond))

		copies := peekAll(t, e.b, e.ns, q.entity(), bus.Active)
		if len(copies) != 1 {
			t.Fatalf("%s has %d active messages, want the copy", q, len(copies))
		}
		checkCopy(t, copies[0], seed.QueueMessages()[1], target.MessageID)
		eventually(t, 30*time.Second, func() string {
			if got := e.entity(t, q); got.DeadLetterCount != 0 || got.ActiveCount != 1 {
				return fmt.Sprintf("runtime counts %+v, want DLQ 0, active 1", got)
			}
			return ""
		})
	})

	t.Run("edits round-trip", func(t *testing.T) {
		q := queue(qMain)
		e.drainAll(t, q)
		e.seedDLQ(t, q, seed.QueueMessages())
		dlq := peekAll(t, e.b, e.ns, q.entity(), bus.DeadLetter)
		target := dlq[2] // the text/plain seed message
		subject, contentType := "OrderFixed", "application/json"
		r := repairReq(e.ns, q.entity(), target.SequenceNumber)
		r.Index = 2
		r.Edits = bus.Edits{
			Properties: []bus.PropertyEdit{
				{Key: "orderId", Type: bus.TypeLong, Value: int64(9_000_000_000)},
				{Key: "attempt", Type: bus.TypeInt, Value: int32(-42)},
				{Key: "amount", Type: bus.TypeDouble, Value: 1e-3},
				{Key: "isRetry", Remove: true},
				{Key: "addString", Type: bus.TypeString, Value: "eu-north"},
				{Key: "addInt", Type: bus.TypeInt, Value: int32(7)},
				{Key: "addLong", Type: bus.TypeLong, Value: int64(1) << 40},
				{Key: "addDouble", Type: bus.TypeDouble, Value: 3.25},
				{Key: "addBool", Type: bus.TypeBool, Value: true},
				{Key: "addGuid", Type: bus.TypeGUID, Value: "0f8fad5b-d9cb-469f-a165-70867728950e"},
				{Key: "addTime", Type: bus.TypeDateTime, Value: time.Date(2026, 10, 3, 12, 0, 0, 500e6, time.UTC)},
			},
			Subject: &subject, ContentType: &contentType,
			Body: []byte("{\n  \"fixed\": true\n}"), BodyEdited: true,
		}
		doRepair(t, e.b, r)
		cp, ok := findByID(peekAll(t, e.b, e.ns, q.entity(), bus.Active), target.MessageID)
		if !ok {
			t.Fatalf("no copy with MessageId %s", target.MessageID)
		}
		if cp.Subject != subject || cp.ContentType != contentType || string(cp.Body) != string(r.Edits.Body) ||
			cp.DeadLetterReason != "" || cp.DeadLetterErrorDescription != "" {
			t.Fatalf("copy = %q %q %q, markers %q %q", cp.Subject, cp.ContentType, cp.Body, cp.DeadLetterReason, cp.DeadLetterErrorDescription)
		}
		want := map[string]bus.Property{}
		for k, v := range seed.QueueMessages()[2].Properties {
			ty, val := expect(v)
			want[k] = bus.Property{Key: k, Type: ty, Value: val}
		}
		delete(want, "isRetry")
		for _, pe := range r.Edits.Properties {
			if !pe.Remove {
				want[pe.Key] = bus.Property{Key: pe.Key, Type: pe.Type, Value: pe.Value}
			}
		}
		checkProps(t, cp.Properties, want)
	})

	t.Run("subscription to topic", func(t *testing.T) {
		sa, so := subscription(subAll), subscription(subOff)
		e.drainAll(t, sa, so)
		e.seedDLQ(t, sa, seed.SubscriptionMessages())
		dlq := peekAll(t, e.b, e.ns, sa.entity(), bus.DeadLetter)
		if len(dlq) != 4 {
			t.Fatalf("%s/$DLQ has %d", sa, len(dlq))
		}
		if dlq[0].DeadLetterSource != "" {
			t.Errorf("subscription DLQ: DeadLetterSource %q, want empty", dlq[0].DeadLetterSource)
		}
		p := plan(t, e.b, repairReq(e.ns, sa.entity(), dlq[0].SequenceNumber))
		if p.Target != (bus.Target{Name: topic, Kind: bus.TargetTopic}) || p.Subscriptions != 2 ||
			len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "no match = dropped") {
			t.Fatalf("plan: %+v", p)
		}
		doRepair(t, e.b, repairReq(e.ns, sa.entity(), dlq[0].SequenceNumber))
		if after := peekAll(t, e.b, e.ns, sa.entity(), bus.DeadLetter); len(after) != 3 {
			t.Fatalf("%s/$DLQ: 4 → %d", sa, len(after))
		}
		got := peekAll(t, e.b, e.ns, sa.entity(), bus.Active)
		if len(got) != 1 {
			t.Fatalf("%s has %d active messages, want the copy", sa, len(got))
		}
		checkCopy(t, got[0], seed.SubscriptionMessages()[0], dlq[0].MessageID)
		// subOff's rule (eventType = 'OrderShipped') does not match.
		if n := len(peekAll(t, e.b, e.ns, so.entity(), bus.Active)); n != 0 {
			t.Fatalf("%s got %d messages", so, n)
		}
		eventually(t, 30*time.Second, func() string {
			if got := e.entity(t, sa); !got.CountsKnown || got.DeadLetterCount != 3 || got.ActiveCount != 1 {
				return fmt.Sprintf("runtime counts %+v, want DLQ 3, active 1", got)
			}
			return ""
		})
	})

	t.Run("duplicate detection", func(t *testing.T) {
		q := queue(qDedup)
		e.drainAll(t, q)
		run := time.Now().UnixNano()
		msg := func(n int) seed.Message {
			return seed.Message{MessageID: fmt.Sprintf("e2e-dedup-%d-%d", run, n), Body: []byte("dedup"), Reason: "E2E"}
		}
		e.seedDLQ(t, q, []seed.Message{msg(1)})
		dlq := peekAll(t, e.b, e.ns, q.entity(), bus.DeadLetter)
		p := plan(t, e.b, repairReq(e.ns, q.entity(), dlq[0].SequenceNumber))
		if !p.DuplicateDetection || !p.NewIDByDefault || p.NewMessageID == "" {
			t.Fatalf("plan: %+v", p)
		}
		r := repairReq(e.ns, q.entity(), dlq[0].SequenceNumber)
		r.NewMessageID = p.NewMessageID
		res := doRepair(t, e.b, r)
		if res.OldMessageID != msg(1).MessageID || res.NewMessageID != p.NewMessageID {
			t.Fatalf("MessageId %s → %s", res.OldMessageID, res.NewMessageID)
		}
		active := peekAll(t, e.b, e.ns, q.entity(), bus.Active)
		if len(active) != 1 || active[0].MessageID != p.NewMessageID {
			t.Fatalf("%s active = %d messages, want the new-id copy", q, len(active))
		}

		// Why the new id is the default: keeping the original id inside the
		// detection window drops the copy and still completes the original.
		e.drain(t, q, false)
		e.seedDLQ(t, q, []seed.Message{msg(2)})
		dlq = peekAll(t, e.b, e.ns, q.entity(), bus.DeadLetter)
		r = repairReq(e.ns, q.entity(), dlq[0].SequenceNumber)
		r.MessageID = bus.MessageIDKeep
		doRepair(t, e.b, r)
		if n := len(peekAll(t, e.b, e.ns, q.entity(), bus.DeadLetter)); n != 0 {
			t.Fatalf("%s/$DLQ still has %d", q, n)
		}
		if n := len(peekAll(t, e.b, e.ns, q.entity(), bus.Active)); n != 0 {
			t.Fatalf("%s active = %d, want 0: the same-id copy was not dropped", q, n)
		}
		t.Logf("same-MessageId repair on a duplicate-detecting queue: send ok, original completed, copy dropped (message lost)")
	})

	t.Run("DeadLetterSource", func(t *testing.T) {
		mdc, sink, fwd := queue(qMDC), queue(qSink), queue(qFwd)
		e.drainAll(t, mdc, sink)
		e.drain(t, fwd, false)
		ctx, cancel := context.WithTimeout(context.Background(), 2*opTimeout)
		defer cancel()

		// MaxDeliveryCount 1: one abandon dead-letters it.
		e.send(t, qMDC, []seed.Message{{MessageID: "e2e-mdc", Body: []byte("x")}})
		r := e.receiver(t, mdc, nil)
		ms, err := r.ReceiveMessages(ctx, 1, nil)
		if err != nil || len(ms) != 1 {
			t.Fatalf("receive %s: %d, %v", mdc, len(ms), err)
		}
		if err := r.AbandonMessage(ctx, ms[0], nil); err != nil {
			t.Fatal(err)
		}
		r.Close(ctx)
		eventually(t, 15*time.Second, func() string {
			dlq := peekAll(t, e.b, e.ns, mdc.entity(), bus.DeadLetter)
			if len(dlq) != 1 {
				return fmt.Sprintf("%s/$DLQ has %d", mdc, len(dlq))
			}
			if m := dlq[0]; m.DeadLetterReason != "MaxDeliveryCountExceeded" || m.DeadLetterSource != "" {
				t.Fatalf("MaxDeliveryCount dead-letter: reason %q, source %q, want MaxDeliveryCountExceeded and empty", m.DeadLetterReason, m.DeadLetterSource)
			}
			return ""
		})

		// ForwardDeadLetteredMessagesTo: the forwarded message names its
		// source, also after it is dead-lettered again in the sink.
		e.seedDLQ(t, fwd, []seed.Message{{MessageID: "e2e-fwd", Body: []byte("x"), Reason: "E2EForward", Description: "forwarded"}})
		eventually(t, 15*time.Second, func() string {
			got := peekAll(t, e.b, e.ns, sink.entity(), bus.Active)
			if len(got) != 1 {
				return fmt.Sprintf("%s has %d active, want the forwarded message", sink, len(got))
			}
			if got[0].DeadLetterSource != qFwd || got[0].DeadLetterReason != "E2EForward" {
				t.Fatalf("forwarded: source %q reason %q", got[0].DeadLetterSource, got[0].DeadLetterReason)
			}
			return ""
		})
		pctx, pcancel := context.WithTimeout(context.Background(), opTimeout)
		_, err = e.b.Peek(pctx, bus.PeekRequest{Namespace: e.ns, Entity: fwd.entity(), SubQueue: bus.DeadLetter})
		pcancel()
		if bus.KindOf(err) != bus.ErrNotAllowed {
			t.Fatalf("peek %s/$DLQ (forwards dead letters): %v, want not allowed", fwd, err)
		}
		e.deadLetter(t, sink, []seed.Message{{MessageID: "e2e-fwd", Reason: "E2ESink"}})
		dlq := peekAll(t, e.b, e.ns, sink.entity(), bus.DeadLetter)
		if len(dlq) != 1 || dlq[0].DeadLetterSource != qFwd {
			t.Fatalf("%s/$DLQ = %+v, want the message with DeadLetterSource %s", sink, dlq, qFwd)
		}
		// The target is the entity the user opened, not DeadLetterSource.
		p := plan(t, e.b, repairReq(e.ns, sink.entity(), dlq[0].SequenceNumber))
		if p.Target != (bus.Target{Name: qSink, Kind: bus.TargetQueue}) {
			t.Fatalf("plan target %+v, want queue %s", p.Target, qSink)
		}
		doRepair(t, e.b, repairReq(e.ns, sink.entity(), dlq[0].SequenceNumber))
		cp := peekAll(t, e.b, e.ns, sink.entity(), bus.Active)
		if len(cp) != 1 || cp[0].DeadLetterSource != "" || cp[0].DeadLetterReason != "" || cp[0].DeadLetterErrorDescription != "" {
			t.Fatalf("copy in %s = %+v", sink, cp)
		}
	})

	t.Run("auth errors", func(t *testing.T) {
		q := queue(qMain)
		check := func(label string, b *azure.Backend, ns bus.Namespace) {
			t.Helper()
			ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
			defer cancel()
			if _, err := b.ListEntities(ctx, ns); bus.KindOf(err) != bus.ErrUnauthorized {
				t.Errorf("%s: list entities: %v, want unauthorized", label, err)
			}
			if _, err := b.Peek(ctx, bus.PeekRequest{Namespace: ns, Entity: q.entity(), SubQueue: bus.DeadLetter}); bus.KindOf(err) != bus.ErrUnauthorized {
				t.Errorf("%s: peek: %v, want unauthorized", label, err)
			} else {
				t.Logf("%s: peek → %v", label, err)
			}
		}
		badKey := azure.New()
		defer badKey.Close(context.Background())
		ns, err := badKey.AddConnectionString("Endpoint=sb://"+e.fqdn+"/;SharedAccessKeyName=RootManageSharedAccessKey;SharedAccessKey="+randomKey(t), 0)
		if err != nil {
			t.Fatal(err)
		}
		check("wrong SAS key", badKey, ns)
		badToken := azure.New()
		defer badToken.Close(context.Background())
		if ns, err = badToken.AddNamespace(e.fqdn, bogusToken{}); err != nil {
			t.Fatal(err)
		}
		check("invalid Entra token", badToken, ns)

		// A Listen-only SAS rule can peek and receive but not send: the
		// repair's send is a definite rejection → SendFailed, nothing
		// changed.
		key := e.listenOnlyRule(t)
		listen := azure.New()
		defer listen.Close(context.Background())
		lns, err := listen.AddConnectionString("Endpoint=sb://"+e.fqdn+"/;SharedAccessKeyName="+listenRule+";SharedAccessKey="+key, 0)
		if err != nil {
			t.Fatal(err)
		}
		e.drainAll(t, q)
		e.seedDLQ(t, q, seed.QueueMessages()[:3])
		before := peekAll(t, e.b, e.ns, q.entity(), bus.DeadLetter)
		eventually(t, 60*time.Second, func() string { // the new rule takes a moment
			ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
			defer cancel()
			if _, err := listen.Peek(ctx, bus.PeekRequest{Namespace: lns, Entity: q.entity(), SubQueue: bus.DeadLetter}); err != nil {
				return "listen-only peek: " + err.Error()
			}
			return ""
		})
		svc := azure.SplitService(listen, e.b)
		ctx, cancel := context.WithTimeout(context.Background(), repairTimeout)
		defer cancel()
		r := repairReq(lns, q.entity(), before[1].SequenceNumber)
		r.Index = 1
		res, err := svc.Repair(ctx, r)
		if err != nil || res.Outcome != bus.SendFailed || bus.KindOf(res.Err) != bus.ErrUnauthorized || len(res.AbandonErrors) != 0 {
			t.Fatalf("listen-only repair: %v (%s), err %v, send err kind %v, abandon errors %v", res.Outcome, res.Detail, err, bus.KindOf(res.Err), res.AbandonErrors)
		}
		t.Logf("listen-only repair: %v: %s", res.Outcome, res.Detail)
		after := peekAll(t, e.b, e.ns, q.entity(), bus.DeadLetter)
		if len(after) != 3 {
			t.Fatalf("%s/$DLQ: 3 → %d after SendFailed", q, len(after))
		}
		was := deliveryCounts(before)
		for id, dc := range deliveryCounts(after) {
			if was[id] != dc {
				t.Errorf("%s: DeliveryCount %d → %d after SendFailed", id, was[id], dc)
			}
		}
		if n := len(peekAll(t, e.b, e.ns, q.entity(), bus.Active)); n != 0 {
			t.Fatalf("%s has %d active after SendFailed", q, n)
		}
	})

	t.Run("partitioned paging and repair", func(t *testing.T) {
		q := queue(qPart)
		e.drainAll(t, q)
		const n = 130
		msgs := make([]seed.Message, n)
		for i := range msgs {
			msgs[i] = seed.Message{MessageID: fmt.Sprintf("e2e-part-%03d", i), Body: []byte("part"), Reason: "E2E"}
		}
		e.seedDLQ(t, q, msgs)
		var pages []bus.Message
		for _, size := range []int{bus.PageSize, 7} {
			pages = pageAll(t, e.b, e.ns, q.entity(), size)
			seen := map[string]int{}
			sorted := true
			for i, m := range pages {
				seen[m.MessageID]++
				if i > 0 && m.SequenceNumber < pages[i-1].SequenceNumber {
					sorted = false
				}
			}
			if len(pages) != n || len(seen) != n {
				t.Fatalf("pages of %d: %d messages, %d distinct, want %d: paging from last+1 skips or repeats", size, len(pages), len(seen), n)
			}
			for i, m := range pages { // peek order = enqueue order
				if m.MessageID != msgs[i].MessageID {
					t.Fatalf("pages of %d: message %d is %s, want %s", size, i, m.MessageID, msgs[i].MessageID)
				}
			}
			t.Logf("pages of %d: all %d messages once, in enqueue order; sorted by sequence number: %v", size, n, sorted)
		}
		target := pages[60]
		r := repairReq(e.ns, q.entity(), target.SequenceNumber)
		r.Index = 60
		res := doRepair(t, e.b, r)
		t.Logf("partitioned repair of row 60: locked %d", res.Locked)
		after := pageAll(t, e.b, e.ns, q.entity(), bus.PageSize)
		if _, ok := findByID(after, target.MessageID); ok || len(after) != n-1 {
			t.Fatalf("%s/$DLQ: %d → %d", q, n, len(after))
		}
		if _, ok := findByID(peekAll(t, e.b, e.ns, q.entity(), bus.Active), target.MessageID); !ok {
			t.Fatalf("no copy of %s in %s", target.MessageID, q)
		}
	})
}

// pageAll peeks the DLQ of ent in pages of size, each from the last
// sequence number + 1, as the Messages panel does.
func pageAll(t *testing.T, b *azure.Backend, ns bus.Namespace, ent bus.Entity, size int) []bus.Message {
	t.Helper()
	var out []bus.Message
	from := int64(0)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		page, err := b.Peek(ctx, bus.PeekRequest{Namespace: ns, Entity: ent, SubQueue: bus.DeadLetter, FromSequence: from, Max: size})
		cancel()
		if err != nil {
			t.Fatalf("peek %s: %v", ent.Path, err)
		}
		if len(page) == 0 {
			return out
		}
		out = append(out, page...)
		from = page[len(page)-1].SequenceNumber + 1
	}
}

// listenOnlyRule sets a Listen-only SAS rule with a fresh random key on
// qMain and returns the key. The key stays in memory.
func (e *azEnv) listenOnlyRule(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	q, err := e.admin.GetQueue(ctx, qMain, nil)
	if err != nil || q == nil {
		t.Fatalf("get queue %s: %v", qMain, err)
	}
	key := randomKey(t)
	props := q.QueueProperties
	props.AuthorizationRules = []admin.AuthorizationRule{{
		KeyName:      ptr(listenRule),
		AccessRights: []admin.AccessRight{admin.AccessRightListen},
		PrimaryKey:   &key,
		SecondaryKey: ptr(randomKey(t)),
	}}
	if _, err := e.admin.UpdateQueue(ctx, qMain, props, nil); err != nil {
		t.Fatalf("update queue %s: %v", qMain, err)
	}
	return key
}

func randomKey(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// bogusToken returns a token Service Bus rejects.
type bogusToken struct{}

func (bogusToken) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: "eyJhbGciOiJub25lIn0.eyJhdWQiOiJ4In0.", ExpiresOn: time.Now().Add(time.Hour)}, nil
}
