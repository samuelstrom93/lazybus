// Package fake is an in-memory implementation of the bus interfaces with
// deterministic demo data. It powers `lazybus --demo` and the UI tests.
package fake

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/samuelstrom93/lazybus/internal/bus"
)

// Epoch is the fixed day all demo messages are enqueued on (UTC).
var Epoch = time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC)

// Backend is an in-memory Service Bus. Safe for use from one goroutine at a
// time per call; the demo data is never mutated in S0.
type Backend struct {
	namespaces []bus.Namespace
	entities   map[string][]*entity // by namespace name
}

type entity struct {
	path       string
	kind       bus.EntityKind
	active     []bus.Message
	deadLetter []bus.Message
}

var _ bus.Browser = (*Backend)(nil)

// New returns a Backend seeded with the demo data:
//
//	sb-prod-weu: invoices (DLQ 3), order-events/billing (DLQ 12),
//	             order-events/shipping (DLQ 0), orders (DLQ 37)
//	sb-test-weu: orders (DLQ 1), payments (DLQ 0)
func New() *Backend {
	b := &Backend{entities: map[string][]*entity{}}
	b.addNamespace("sb-prod-weu", "contoso-prod",
		seedEntity("invoices", bus.KindQueue, "inv", 1000, 3, 12, 13*time.Hour),
		seedEntity("order-events/billing", bus.KindSubscription, "bil", 2000, 12, 0, 9*time.Hour),
		seedEntity("order-events/shipping", bus.KindSubscription, "shp", 3000, 0, 4, 8*time.Hour),
		seedEntity("orders", bus.KindQueue, "ord", 4000, 37, 120, 6*time.Hour),
	)
	b.addNamespace("sb-test-weu", "contoso-test",
		seedEntity("orders", bus.KindQueue, "tord", 5000, 1, 0, 10*time.Hour),
		seedEntity("payments", bus.KindQueue, "pay", 6000, 0, 2, 11*time.Hour),
	)
	return b
}

func (b *Backend) addNamespace(name, subscription string, ents ...*entity) {
	b.namespaces = append(b.namespaces, bus.Namespace{
		Name:         name,
		FQDN:         name + ".servicebus.windows.net",
		Subscription: subscription,
	})
	sort.Slice(ents, func(i, j int) bool { return ents[i].path < ents[j].path })
	b.entities[name] = ents
}

// Namespaces implements bus.Discovery.
func (b *Backend) Namespaces(ctx context.Context) ([]bus.Namespace, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return append([]bus.Namespace(nil), b.namespaces...), nil
}

// ListEntities implements bus.Entities.
func (b *Backend) ListEntities(ctx context.Context, ns bus.Namespace) ([]bus.Entity, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ents, ok := b.entities[ns.Name]
	if !ok {
		return nil, fmt.Errorf("namespace %q not found", ns.Name)
	}
	out := make([]bus.Entity, 0, len(ents))
	for _, e := range ents {
		out = append(out, bus.Entity{
			Path:            e.path,
			Kind:            e.kind,
			ActiveCount:     int64(len(e.active)),
			DeadLetterCount: int64(len(e.deadLetter)),
		})
	}
	return out, nil
}

// Peek implements bus.Peeker.
func (b *Backend) Peek(ctx context.Context, req bus.PeekRequest) ([]bus.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var found *entity
	for _, e := range b.entities[req.Namespace.Name] {
		if e.path == req.Entity.Path && e.kind == req.Entity.Kind {
			found = e
			break
		}
	}
	if found == nil {
		return nil, fmt.Errorf("entity %q not found in %q", req.Entity.Path, req.Namespace.Name)
	}
	src := found.deadLetter
	if req.SubQueue == bus.Active {
		src = found.active
	}
	limit := req.Max
	if limit <= 0 {
		limit = bus.PageSize
	}
	var out []bus.Message
	for _, m := range src {
		if m.SequenceNumber < req.FromSequence {
			continue
		}
		if len(out) == limit {
			break
		}
		out = append(out, m)
	}
	return out, nil
}

// seedEntity builds an entity with dlq dead-letter and active messages.
// Dead-letter messages get sequence numbers from 2, one hour apart starting
// at Epoch+start; active messages follow them.
func seedEntity(path string, kind bus.EntityKind, idPrefix string, orderBase, dlq, active int, start time.Duration) *entity {
	e := &entity{path: path, kind: kind}
	seq := int64(2)
	for i := range dlq {
		e.deadLetter = append(e.deadLetter, deadLetterMessage(i, seq, idPrefix, orderBase+int(seq), Epoch.Add(start+time.Duration(i)*time.Hour)))
		seq++
	}
	for i := range active {
		e.active = append(e.active, activeMessage(seq, idPrefix, orderBase+int(seq), Epoch.Add(start+time.Duration(dlq+i)*time.Minute)))
		seq++
	}
	return e
}

const ttl = 14 * 24 * time.Hour

func baseProps(order int, retry bool) []bus.Property {
	return []bus.Property{
		{Key: "tenant", Type: bus.TypeString, Value: "contoso"},
		{Key: "orderId", Type: bus.TypeLong, Value: int64(order)},
		{Key: "attempt", Type: bus.TypeInt, Value: int32(order%4 + 1)},
		{Key: "amount", Type: bus.TypeDouble, Value: float64(order%500) + 0.5},
		{Key: "isRetry", Type: bus.TypeBool, Value: retry},
		{Key: "traceId", Type: bus.TypeGUID, Value: fmt.Sprintf("6f1c2b9e-4d2a-4c1e-9b7a-%012d", order)},
		{Key: "createdAt", Type: bus.TypeDateTime, Value: time.Date(2026, 9, 18, 22, 15, 0, 0, time.UTC)},
	}
}

// deadLetterMessage cycles through four shapes: JSON with a schema problem,
// JSON with a long line, plain text, and invalid JSON.
func deadLetterMessage(i int, seq int64, idPrefix string, order int, enqueued time.Time) bus.Message {
	m := bus.Message{
		SequenceNumber: seq,
		EnqueuedTime:   enqueued,
		MessageID:      fmt.Sprintf("%s-%d", idPrefix, order),
		CorrelationID:  fmt.Sprintf("corr-%d", order),
		Subject:        "OrderPlaced",
		ContentType:    "application/json",
		TimeToLive:     ttl,
		DeliveryCount:  1,
	}
	switch i % 4 {
	case 0:
		m.Body = fmt.Appendf(nil, `{"orderId":%d,"status":"pending","customer":null,"items":[{"sku":"A-100","qty":2},{"sku":"B-220","qty":1}]}`, order)
		m.Properties = baseProps(order, false)
		m.DeadLetterReason = "SchemaMismatch"
		m.DeadLetterErrorDescription = "Required property 'customer.id' is missing."
	case 1:
		m.Body = fmt.Appendf(nil, `{"orderId":%d,"status":"failed","note":"%s"}`, order,
			strings.Repeat("Downstream warehouse API returned 409 Conflict for reservation; ", 4))
		m.Properties = baseProps(order, true)
		m.DeadLetterReason = "MaxDeliveryCountExceeded"
		m.DeadLetterErrorDescription = "Message could not be consumed after 10 delivery attempts."
		m.DeliveryCount = 10
		m.SessionID = fmt.Sprintf("customer-%d", order%7)
	case 2:
		m.Body = fmt.Appendf(nil, "ORDER;%d;SE;299.00\nLINE;A-100;2\nLINE;B-220;1\n", order)
		m.ContentType = "text/plain"
		m.Subject = "OrderImported"
		m.Properties = baseProps(order, false)[:3]
		m.DeadLetterReason = "DownstreamTimeout"
		m.DeadLetterErrorDescription = "POST https://billing.internal/api/v2/invoices timed out after 30s."
		m.ReplyTo = "order-replies"
	case 3:
		m.Body = fmt.Appendf(nil, `{"orderId":%d,"status":"pending","items":[{"sku":"A-100",`, order)
		m.Properties = baseProps(order, false)[:2]
		m.DeadLetterReason = "SchemaMismatch"
		m.DeadLetterErrorDescription = "Unexpected end of JSON input."
		m.PartitionKey = "eu-north"
	}
	return m
}

func activeMessage(seq int64, idPrefix string, order int, enqueued time.Time) bus.Message {
	return bus.Message{
		SequenceNumber: seq,
		EnqueuedTime:   enqueued,
		Body:           fmt.Appendf(nil, `{"orderId":%d,"status":"new"}`, order),
		Properties:     baseProps(order, false)[:2],
		MessageID:      fmt.Sprintf("%s-%d", idPrefix, order),
		CorrelationID:  fmt.Sprintf("corr-%d", order),
		Subject:        "OrderPlaced",
		ContentType:    "application/json",
		TimeToLive:     ttl,
	}
}
