package fake

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samuelstrom93/lazybus/internal/bus"
)

func TestEntitiesSortedWithCounts(t *testing.T) {
	b := New()
	ctx := context.Background()
	nss, err := b.Namespaces(ctx)
	if err != nil || len(nss) != 2 {
		t.Fatalf("namespaces = %v, %v", nss, err)
	}
	ents, err := b.ListEntities(ctx, nss[0])
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		path string
		dlq  int64
	}{{"invoices", 3}, {"order-events/billing", 12}, {"order-events/shipping", 0}, {"orders", 37}}
	if len(ents) != len(want) {
		t.Fatalf("got %d entities, want %d", len(ents), len(want))
	}
	for i, w := range want {
		if ents[i].Path != w.path || ents[i].DeadLetterCount != w.dlq {
			t.Errorf("entity %d = %s DLQ %d, want %s DLQ %d", i, ents[i].Path, ents[i].DeadLetterCount, w.path, w.dlq)
		}
	}
}

func TestPeekFromSequenceAndPageSize(t *testing.T) {
	b := New()
	ctx := context.Background()
	nss, _ := b.Namespaces(ctx)
	ents, _ := b.ListEntities(ctx, nss[0])
	orders := ents[3]

	page, err := b.Peek(ctx, bus.PeekRequest{Namespace: nss[0], Entity: orders, SubQueue: bus.Active, FromSequence: 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != bus.PageSize {
		t.Fatalf("active page = %d messages, want %d", len(page), bus.PageSize)
	}

	dlq, err := b.Peek(ctx, bus.PeekRequest{Namespace: nss[0], Entity: orders, SubQueue: bus.DeadLetter, FromSequence: 10, Max: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(dlq) != 3 || dlq[0].SequenceNumber != 10 || dlq[2].SequenceNumber != 12 {
		t.Fatalf("dlq from 10 = %+v", dlq)
	}
	if dlq[0].DeadLetterReason == "" {
		t.Error("dead-letter message without reason")
	}
}

func TestOptionsAndFaults(t *testing.T) {
	b := New(WithUnknownCounts(), WithDeadLetters("sb-prod-weu", "orders", 120))
	ctx := context.Background()
	nss, _ := b.Namespaces(ctx)
	ents, err := b.ListEntities(ctx, nss[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if e.CountsKnown || e.DeadLetterCount != 0 {
			t.Fatalf("%s: counts known %v, DLQ %d", e.Path, e.CountsKnown, e.DeadLetterCount)
		}
	}
	page, _ := b.Peek(ctx, bus.PeekRequest{Namespace: nss[0], Entity: ents[3], FromSequence: 100})
	if len(page) != 22 || page[21].SequenceNumber != 121 {
		t.Fatalf("orders DLQ from 100 = %d messages", len(page))
	}

	boom := errors.New("boom")
	b.SetFault(OpPeek, Fault{Err: boom})
	if _, err := b.Peek(ctx, bus.PeekRequest{Namespace: nss[0], Entity: ents[0]}); !errors.Is(err, boom) {
		t.Fatalf("peek with fault = %v", err)
	}
	b.SetFault(OpEntities, Fault{Block: true})
	short, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	if _, err := b.ListEntities(short, nss[0]); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked list = %v", err)
	}
}
