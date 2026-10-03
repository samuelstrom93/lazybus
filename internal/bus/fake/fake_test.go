package fake

import (
	"context"
	"testing"

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
