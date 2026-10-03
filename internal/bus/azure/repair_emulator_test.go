//go:build emulator

package azure_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/samuelstrom93/lazybus/internal/bus"
	"github.com/samuelstrom93/lazybus/internal/bus/azure"
	"github.com/samuelstrom93/lazybus/internal/seed"
	"github.com/samuelstrom93/lazybus/internal/ui"
)

func TestEmulatorRepair(t *testing.T) {
	amqpPort := port(t, "LAZYBUS_EMULATOR_AMQP_PORT", 5682)
	adminPort := port(t, "LAZYBUS_EMULATOR_ADMIN_PORT", 5310)
	cs := azure.EmulatorConnectionString("localhost", amqpPort)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := seed.Run(ctx, seed.Config{ConnectionString: cs, AdminPort: adminPort, Reset: true}); err != nil {
		t.Fatal(err)
	}
	b := azure.New()
	defer b.Close(context.Background())
	ns, err := b.AddConnectionString(cs, adminPort)
	if err != nil {
		t.Fatal(err)
	}
	orders := bus.Entity{Path: seed.Queue, Kind: bus.KindQueue}
	billing := bus.Entity{Path: seed.Topic + "/" + seed.Subscription, Kind: bus.KindSubscription}
	shipping := bus.Entity{Path: seed.Topic + "/shipping", Kind: bus.KindSubscription}
	dedup := bus.Entity{Path: seed.DedupQueue, Kind: bus.KindQueue}

	t.Run("queue", func(t *testing.T) {
		dlq := peekAll(t, b, ns, orders, bus.DeadLetter)
		if len(dlq) != len(seed.QueueMessages()) {
			t.Fatalf("orders/$DLQ has %d", len(dlq))
		}
		first := dlq[0]
		p := plan(t, b, repairReq(ns, orders, first.SequenceNumber))
		if p.Target != (bus.Target{Name: seed.Queue, Kind: bus.TargetQueue}) || p.DuplicateDetection || p.NewIDByDefault {
			t.Fatalf("plan: %+v", p)
		}
		res := doRepair(t, b, repairReq(ns, orders, first.SequenceNumber))
		if res.NewMessageID != first.MessageID {
			t.Fatalf("MessageId %s → %s, want kept", first.MessageID, res.NewMessageID)
		}
		after := peekAll(t, b, ns, orders, bus.DeadLetter)
		if len(after) != len(dlq)-1 || after[0].SequenceNumber == first.SequenceNumber {
			t.Fatalf("orders/$DLQ: %d → %d messages", len(dlq), len(after))
		}
		got := peekAll(t, b, ns, orders, bus.Active)
		if len(got) != 1 {
			t.Fatalf("orders has %d active messages, want the copy", len(got))
		}
		checkCopy(t, got[0], seed.QueueMessages()[0], first.MessageID)
	})

	t.Run("ui r y", func(t *testing.T) {
		before := peekAll(t, b, ns, orders, bus.DeadLetter)
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		ents, err := b.ListEntities(ctx, ns)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		var m tea.Model = ui.New(b, ui.Options{Location: time.UTC, CallTimeout: opTimeout})
		m = drive(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
		m = runCmds(m, m.Init())
		m = drive(t, m, key("2"))
		for _, e := range ents {
			if e.Path == orders.Path && e.Kind == orders.Kind {
				break
			}
			m = drive(t, m, key("j"))
		}
		m = drive(t, m, key("enter"))
		m = drive(t, m, key("r"))
		if s := screen(m); !strings.Contains(s, "Resubmit") || !strings.Contains(s, "queue orders") {
			t.Fatalf("no confirm popup:\n%s", s)
		}
		m = drive(t, m, key("y"))
		want := fmt.Sprintf("Resubmitted orders/$DLQ seq %d → queue orders", before[0].SequenceNumber)
		if s := screen(m); !strings.Contains(s, want) {
			t.Fatalf("status lacks %q:\n%s", want, s)
		}
		if after := peekAll(t, b, ns, orders, bus.DeadLetter); len(after) != len(before)-1 {
			t.Fatalf("orders/$DLQ: %d → %d", len(before), len(after))
		}
	})

	t.Run("edits", func(t *testing.T) {
		dlq := peekAll(t, b, ns, orders, bus.DeadLetter)
		first := dlq[0] // the text/plain seed message
		subject, contentType := "OrderFixed", "application/json"
		guid := "0f8fad5b-d9cb-469f-a165-70867728950e"
		at := time.Date(2026, 10, 3, 12, 0, 0, 500e6, time.UTC)
		r := repairReq(ns, orders, first.SequenceNumber)
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
				{Key: "addGuid", Type: bus.TypeGUID, Value: guid},
				{Key: "addTime", Type: bus.TypeDateTime, Value: at},
			},
			Subject:     &subject,
			ContentType: &contentType,
			Body:        []byte("{\n  \"fixed\": true\n}"),
			BodyEdited:  true,
		}
		res := doRepair(t, b, r)
		if res.NewMessageID != first.MessageID {
			t.Fatalf("MessageId %s → %s, want kept", first.MessageID, res.NewMessageID)
		}
		cp, ok := findByID(peekAll(t, b, ns, orders, bus.Active), first.MessageID)
		if !ok {
			t.Fatalf("no copy with MessageId %s in orders", first.MessageID)
		}
		if cp.Subject != subject || cp.ContentType != contentType || string(cp.Body) != string(r.Edits.Body) ||
			cp.DeadLetterReason != "" || cp.DeadLetterErrorDescription != "" {
			t.Fatalf("copy = %q %q %q, markers %q %q", cp.Subject, cp.ContentType, cp.Body, cp.DeadLetterReason, cp.DeadLetterErrorDescription)
		}
		seeded := seed.QueueMessages()[2].Properties
		want := map[string]bus.Property{}
		for k, v := range seeded {
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

	t.Run("ui edit popup", func(t *testing.T) {
		before := peekAll(t, b, ns, orders, bus.DeadLetter)
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		ents, err := b.ListEntities(ctx, ns)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		var m tea.Model = ui.New(b, ui.Options{Location: time.UTC, CallTimeout: opTimeout})
		m = drive(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
		m = runCmds(m, m.Init())
		m = drive(t, m, key("2"))
		for _, e := range ents {
			if e.Path == orders.Path && e.Kind == orders.Kind {
				break
			}
			m = drive(t, m, key("j"))
		}
		m = drive(t, m, key("enter"))
		// e: add fixedBy as Int 42 (→ from String to Int).
		for _, k := range []string{"e", "f", "i", "x", "e", "d", "B", "y", "tab", "right", "tab", "4", "2", "enter"} {
			m = drive(t, m, key(k))
		}
		if s := screen(m); !strings.Contains(s, "+fixedBy") || !strings.Contains(s, "✎ 1 pending") {
			t.Fatalf("no pending add:\n%s", s)
		}
		m = drive(t, m, key("r"))
		if s := screen(m); !strings.Contains(s, "+ fixedBy: Int 42") {
			t.Fatalf("confirm lacks the diff:\n%s", s)
		}
		m = drive(t, m, key("y"))
		if s := screen(m); !strings.Contains(s, "(1 edits applied)") && !strings.Contains(s, "(1 edit applied)") {
			t.Fatalf("status:\n%s", s)
		}
		cp, ok := findByID(peekAll(t, b, ns, orders, bus.Active), before[0].MessageID)
		if !ok {
			t.Fatalf("no copy with MessageId %s", before[0].MessageID)
		}
		p, ok := findProp(cp.Properties, "fixedBy")
		if !ok || p.Type != bus.TypeInt || p.Value != int32(42) {
			t.Fatalf("fixedBy = %+v", p)
		}
	})

	t.Run("subscription to topic", func(t *testing.T) {
		dlq := peekAll(t, b, ns, billing, bus.DeadLetter)
		shipBefore := len(peekAll(t, b, ns, shipping, bus.Active))
		first := dlq[0]
		p := plan(t, b, repairReq(ns, billing, first.SequenceNumber))
		if p.Target != (bus.Target{Name: seed.Topic, Kind: bus.TargetTopic}) || p.Subscriptions != 2 ||
			len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "no match = dropped") {
			t.Fatalf("plan: %+v", p)
		}
		doRepair(t, b, repairReq(ns, billing, first.SequenceNumber))
		if after := peekAll(t, b, ns, billing, bus.DeadLetter); len(after) != len(dlq)-1 {
			t.Fatalf("billing/$DLQ: %d → %d", len(dlq), len(after))
		}
		got := peekAll(t, b, ns, billing, bus.Active)
		if len(got) != 1 {
			t.Fatalf("billing has %d active messages, want the copy", len(got))
		}
		checkCopy(t, got[0], seed.SubscriptionMessages()[0], first.MessageID)
		// shipping's rule (eventType = 'OrderShipped') does not match.
		if n := len(peekAll(t, b, ns, shipping, bus.Active)); n != shipBefore {
			t.Fatalf("shipping: %d → %d active messages", shipBefore, n)
		}
	})

	t.Run("duplicate detection gets a new MessageId", func(t *testing.T) {
		dlq := peekAll(t, b, ns, dedup, bus.DeadLetter)
		if len(dlq) != 1 {
			t.Fatalf("orders-dedup/$DLQ has %d", len(dlq))
		}
		first := dlq[0]
		p := plan(t, b, repairReq(ns, dedup, first.SequenceNumber))
		if !p.DuplicateDetection || !p.NewIDByDefault || p.NewMessageID == "" {
			t.Fatalf("plan: %+v", p)
		}
		r := repairReq(ns, dedup, first.SequenceNumber)
		r.NewMessageID = p.NewMessageID
		res := doRepair(t, b, r)
		if res.OldMessageID != first.MessageID || res.NewMessageID != p.NewMessageID {
			t.Fatalf("MessageId %s → %s", res.OldMessageID, res.NewMessageID)
		}
		if after := peekAll(t, b, ns, dedup, bus.DeadLetter); len(after) != 0 {
			t.Fatalf("orders-dedup/$DLQ still has %d", len(after))
		}
		got := peekAll(t, b, ns, dedup, bus.Active)
		if len(got) != 1 || got[0].MessageID != p.NewMessageID {
			t.Fatalf("orders-dedup active = %+v, want the new-id copy", got)
		}
	})

	t.Run("zero-subscription topic refused", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		defer cancel()
		ghost := bus.Entity{Path: seed.EmptyTopic + "/none", Kind: bus.KindSubscription}
		_, err := b.Repair(ctx, repairReq(ns, ghost, 1))
		if bus.KindOf(err) != bus.ErrRefused || !strings.Contains(err.Error(), "no subscriptions") {
			t.Fatalf("err = %v", err)
		}
	})
}

func findProp(props []bus.Property, key string) (bus.Property, bool) {
	for _, p := range props {
		if p.Key == key {
			return p, true
		}
	}
	return bus.Property{}, false
}
