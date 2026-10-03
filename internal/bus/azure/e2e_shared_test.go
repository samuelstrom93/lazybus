//go:build emulator || azure

// Helpers shared by the emulator E2E (-tags emulator) and the real-namespace
// E2E (-tags azure).
package azure_test

import (
	"context"
	"testing"
	"time"

	"github.com/Azure/go-amqp"

	"github.com/samuelstrom93/lazybus/internal/bus"
	"github.com/samuelstrom93/lazybus/internal/bus/azure"
	"github.com/samuelstrom93/lazybus/internal/seed"
)

// opTimeout bounds each broker operation; the emulator can stall for
// seconds when it throttles (S−1).
const opTimeout = 30 * time.Second

// expect is the bus type and value a seeded property value reads back as.
func expect(v any) (bus.PropertyType, any) {
	switch v := v.(type) {
	case string:
		return bus.TypeString, v
	case int32:
		return bus.TypeInt, v
	case int64:
		return bus.TypeLong, v
	case float64:
		return bus.TypeDouble, v
	case bool:
		return bus.TypeBool, v
	case amqp.UUID:
		return bus.TypeGUID, v.String()
	case time.Time:
		return bus.TypeDateTime, v
	}
	panic("unexpected seed property type")
}

// repairTimeout bounds one repair: pre-check, guards, scan, send, complete.
const repairTimeout = 2 * opTimeout

// peekAll peeks every message of ent's sub-queue, page by page. The
// emulator has no runtime counts, so this is how the E2E counts.
func peekAll(t *testing.T, b *azure.Backend, ns bus.Namespace, ent bus.Entity, sub bus.SubQueue) []bus.Message {
	t.Helper()
	var out []bus.Message
	from := int64(0)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		page, err := b.Peek(ctx, bus.PeekRequest{Namespace: ns, Entity: ent, SubQueue: sub, FromSequence: from})
		cancel()
		if err != nil {
			t.Fatalf("peek %s %v: %v", ent.Path, sub, err)
		}
		if len(page) == 0 {
			return out
		}
		out = append(out, page...)
		from = page[len(page)-1].SequenceNumber + 1
	}
}

func repairReq(ns bus.Namespace, ent bus.Entity, seq int64) bus.RepairRequest {
	return bus.RepairRequest{Namespace: ns, Entity: ent, SubQueue: bus.DeadLetter, SequenceNumber: seq}
}

func plan(t *testing.T, b *azure.Backend, r bus.RepairRequest) bus.RepairPlan {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	p, err := b.PlanRepair(ctx, r)
	if err != nil {
		t.Fatalf("plan %s seq %d: %v", r.Entity.Path, r.SequenceNumber, err)
	}
	if !p.Found {
		t.Fatalf("plan %s seq %d: pre-check did not find it", r.Entity.Path, r.SequenceNumber)
	}
	return p
}

func doRepair(t *testing.T, b *azure.Backend, r bus.RepairRequest) bus.RepairResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), repairTimeout)
	defer cancel()
	res, err := b.Repair(ctx, r)
	for _, l := range res.Log {
		t.Logf("log: %s", l.Text)
	}
	if err != nil {
		t.Fatalf("repair %s seq %d: %v", r.Entity.Path, r.SequenceNumber, err)
	}
	if res.Outcome != bus.Resubmitted || len(res.AbandonErrors) != 0 {
		t.Fatalf("repair %s seq %d: %v (%s), abandon errors %v", r.Entity.Path, r.SequenceNumber, res.Outcome, res.Detail, res.AbandonErrors)
	}
	return res
}

// checkCopy compares a resubmitted copy with the seeded message: no
// markers, same body and fields, property types kept.
func checkCopy(t *testing.T, got bus.Message, want seed.Message, messageID string) {
	t.Helper()
	if got.MessageID != messageID || string(got.Body) != string(want.Body) || got.Subject != want.Subject ||
		got.ContentType != want.ContentType || got.CorrelationID != want.CorrelationID {
		t.Errorf("copy = %s %q %q %q, want %s %q %q %q", got.MessageID, got.Subject, got.ContentType, got.Body,
			messageID, want.Subject, want.ContentType, want.Body)
	}
	if got.DeadLetterReason != "" || got.DeadLetterErrorDescription != "" {
		t.Errorf("copy carries markers %q / %q", got.DeadLetterReason, got.DeadLetterErrorDescription)
	}
	if len(got.Properties) != len(want.Properties) {
		t.Errorf("copy has %d properties, want %d: %+v", len(got.Properties), len(want.Properties), got.Properties)
	}
	for _, p := range got.Properties {
		wv, ok := want.Properties[p.Key]
		if !ok {
			t.Errorf("copy: unexpected property %s", p.Key)
			continue
		}
		wantType, wantVal := expect(wv)
		equal := p.Value == wantVal
		if tv, ok := p.Value.(time.Time); ok {
			equal = tv.Equal(wantVal.(time.Time))
		}
		if p.Type != wantType || !equal {
			t.Errorf("copy: %s = %v %v, want %v %v", p.Key, p.Type, p.Value, wantType, wantVal)
		}
	}
}

func findByID(msgs []bus.Message, id string) (bus.Message, bool) {
	for _, m := range msgs {
		if m.MessageID == id {
			return m, true
		}
	}
	return bus.Message{}, false
}

// checkProps compares the received properties, with their AMQP types,
// against want.
func checkProps(t *testing.T, got []bus.Property, want map[string]bus.Property) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("copy has %d properties, want %d: %+v", len(got), len(want), got)
	}
	for _, p := range got {
		w, ok := want[p.Key]
		if !ok {
			t.Errorf("copy: unexpected property %s", p.Key)
			continue
		}
		equal := p.Value == w.Value
		if tv, ok := p.Value.(time.Time); ok {
			equal = tv.Equal(w.Value.(time.Time))
		}
		if p.Type != w.Type || !equal {
			t.Errorf("copy: %s = %v %v (%T), want %v %v", p.Key, p.Type, p.Value, p.Value, w.Type, w.Value)
		}
	}
}
