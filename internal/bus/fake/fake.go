// Package fake is an in-memory implementation of the bus interfaces with
// deterministic demo data. It powers `lazybus --demo` and the UI tests.
//
// DLQ Repair runs the real algorithm (bus.Service) on an in-memory broker:
// peek-lock with lock expiry and DeliveryCount, abandon, complete, send to
// queues and topics (fan-out by subscription filter), duplicate detection,
// and fault injection for every step.
package fake

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/samuelstrom93/lazybus/internal/bus"
)

// Epoch is the fixed day all demo messages are enqueued on (UTC).
var Epoch = time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC)

// Backend is an in-memory Service Bus. It implements bus.Backend. All
// methods are safe for concurrent use; faults may be changed at any time.
type Backend struct {
	namespaces    []bus.Namespace
	unknownCounts bool
	now           func() time.Time
	svc           *bus.Service

	mu           sync.Mutex                   // guards everything below
	entities     map[string][]*entity         // by namespace name
	topics       map[string]map[string]*topic // namespace → topic name
	faults       map[Op]Fault
	lockDuration time.Duration
	token        int64
	ids          int
	events       []Event
}

// Op names a backend call for fault injection.
type Op int

const (
	OpNamespaces Op = iota
	OpEntities
	OpPeek
	OpTargetInfo
	OpReceive
	OpAbandon
	OpComplete
	OpSend
)

func (o Op) String() string {
	return [...]string{"namespaces", "entities", "peek", "target", "receive", "abandon", "complete", "send"}[o]
}

// Fault changes how calls of one Op behave: wait Delay (or, with Block,
// until the context ends), then fail with Err when it is set.
type Fault struct {
	Delay time.Duration
	Block bool
	Err   error
	// After applies the call first and then returns Err: a send that
	// delivered, a complete or abandon that took effect.
	After bool
	// Definite marks a send error as a definite rejection (OpSend only).
	Definite bool
	// Seq limits the fault to one sequence number (abandon, complete,
	// send); zero matches every call.
	Seq int64
	// Hook runs at the start of every call the fault applies to (tests
	// move a fake clock with it).
	Hook func()
}

// SetFault installs f for op; the zero Fault removes it.
func (b *Backend) SetFault(op Op, f Fault) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.faults[op] = f
}

// fault returns the fault for op on sequence number seq (zero Fault when
// none applies).
func (b *Backend) fault(op Op, seq int64) Fault {
	b.mu.Lock()
	defer b.mu.Unlock()
	f := b.faults[op]
	if f.Seq != 0 && f.Seq != seq {
		return Fault{}
	}
	return f
}

// wait applies f's Delay or Block.
func wait(ctx context.Context, f Fault) error {
	if f.Hook != nil {
		f.Hook()
	}
	if f.Block {
		<-ctx.Done()
		return ctx.Err()
	}
	if f.Delay > 0 {
		t := time.NewTimer(f.Delay)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// before applies the fault for op and returns the error the call fails with.
func (b *Backend) before(ctx context.Context, op Op) error {
	f := b.fault(op, 0)
	if err := wait(ctx, f); err != nil {
		return err
	}
	if f.Err != nil && !f.After {
		return f.Err
	}
	return ctx.Err()
}

// Option configures New.
type Option func(*Backend)

// WithUnknownCounts makes ListEntities report no runtime counts, like the
// emulator.
func WithUnknownCounts() Option {
	return func(b *Backend) { b.unknownCounts = true }
}

// WithDeadLetters replaces the dead-letter queue of entity path in namespace
// ns with n demo messages (to exercise paging).
func WithDeadLetters(ns, path string, n int) Option {
	return func(b *Backend) {
		for _, e := range b.entities[ns] {
			if e.path == path {
				fresh := seedEntity(e.path, e.kind, e.idPrefix, e.orderBase, n, 0, e.start)
				e.deadLetter = fresh.deadLetter
				e.nextSeq = max(e.nextSeq, fresh.nextSeq)
			}
		}
	}
}

// WithClock sets the broker clock (lock expiry, enqueued times, duplicate
// detection window, and the repair's lock checks). Default time.Now.
func WithClock(now func() time.Time) Option {
	return func(b *Backend) { b.now = now }
}

// WithEmptyTopic adds a topic without subscriptions to namespace ns.
func WithEmptyTopic(ns, name string) Option {
	return func(b *Backend) {
		if b.topics[ns] == nil {
			b.topics[ns] = map[string]*topic{}
		}
		b.topics[ns][name] = &topic{name: name}
	}
}

// WithDuplicateDetection turns on duplicate detection for the queue or
// topic name in namespace ns.
func WithDuplicateDetection(ns, name string) Option {
	return func(b *Backend) {
		d := &dedup{window: dedupWindow, seen: map[string]time.Time{}}
		if t := b.topics[ns][name]; t != nil {
			t.dedup = d
			return
		}
		for _, e := range b.entities[ns] {
			if e.kind == bus.KindQueue && e.path == name {
				e.dedup = d
				for _, l := range [][]*stored{e.deadLetter, e.active} {
					for _, s := range l {
						d.seen[s.msg.MessageID] = s.msg.EnqueuedTime
					}
				}
			}
		}
	}
}

// SetLockDuration sets the peek-lock duration (default 60 s).
func (b *Backend) SetLockDuration(d time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lockDuration = d
}

// dedupWindow is the duplicate-detection history window of the fake.
const dedupWindow = 10 * time.Minute

type dedup struct {
	window time.Duration
	seen   map[string]time.Time
}

// stored is one message in an entity, with its lock.
type stored struct {
	msg         bus.Message
	lockedUntil time.Time
	token       int64
}

type entity struct {
	path       string
	kind       bus.EntityKind
	active     []*stored
	deadLetter []*stored
	nextSeq    int64
	// filter is a subscription's rule; nil matches everything.
	filter func(bus.Message) bool
	dedup  *dedup // queues only

	// scan serializes users of the DLQ's receiver, like the one Azure
	// receiver per DLQ.
	scan sync.Mutex

	idPrefix  string
	orderBase int
	start     time.Duration
}

type topic struct {
	name  string
	dedup *dedup
}

var _ bus.Backend = (*Backend)(nil)

// New returns a Backend seeded with the demo data:
//
//	sb-prod-weu: invoices (DLQ 3, duplicate detection), order-events/billing
//	             (DLQ 12), order-events/shipping (DLQ 0, rule: Subject =
//	             OrderShipped), orders (DLQ 37)
//	sb-test-weu: orders (DLQ 1), payments (DLQ 0)
func New(opts ...Option) *Backend {
	b := &Backend{
		entities: map[string][]*entity{}, topics: map[string]map[string]*topic{},
		faults: map[Op]Fault{}, lockDuration: 60 * time.Second, now: time.Now,
	}
	shipping := seedEntity("order-events/shipping", bus.KindSubscription, "shp", 3000, 0, 4, 8*time.Hour)
	shipping.filter = func(m bus.Message) bool { return m.Subject == "OrderShipped" }
	b.addNamespace("sb-prod-weu", "contoso-prod",
		seedEntity("invoices", bus.KindQueue, "inv", 1000, 3, 12, 13*time.Hour),
		seedEntity("order-events/billing", bus.KindSubscription, "bil", 2000, 12, 0, 9*time.Hour),
		shipping,
		seedEntity("orders", bus.KindQueue, "ord", 4000, 37, 120, 6*time.Hour),
	)
	b.addNamespace("sb-test-weu", "contoso-test",
		seedEntity("orders", bus.KindQueue, "tord", 5000, 1, 0, 10*time.Hour),
		seedEntity("payments", bus.KindQueue, "pay", 6000, 0, 2, 11*time.Hour),
	)
	WithDuplicateDetection("sb-prod-weu", "invoices")(b)
	for _, o := range opts {
		o(b)
	}
	b.svc = bus.NewService(driver{b}, b.clock, b.nextID)
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
	b.topics[name] = map[string]*topic{}
	for _, e := range ents {
		if t, err := bus.TargetOf(bus.Entity{Path: e.path, Kind: e.kind}); err == nil && t.Kind == bus.TargetTopic {
			if b.topics[name][t.Name] == nil {
				b.topics[name][t.Name] = &topic{name: t.Name}
			}
		}
	}
}

func (b *Backend) clock() time.Time { return b.now() }

// nextID returns deterministic "new" MessageIds, so goldens are stable.
func (b *Backend) nextID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ids++
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", b.ids)
}

// Namespaces implements bus.Discovery.
func (b *Backend) Namespaces(ctx context.Context) ([]bus.Namespace, error) {
	if err := b.before(ctx, OpNamespaces); err != nil {
		return nil, err
	}
	return append([]bus.Namespace(nil), b.namespaces...), nil
}

// ListEntities implements bus.Entities.
func (b *Backend) ListEntities(ctx context.Context, ns bus.Namespace) ([]bus.Entity, error) {
	if err := b.before(ctx, OpEntities); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	ents, ok := b.entities[ns.Name]
	if !ok {
		return nil, fmt.Errorf("namespace %q not found", ns.Name)
	}
	out := make([]bus.Entity, 0, len(ents))
	for _, e := range ents {
		ent := bus.Entity{Path: e.path, Kind: e.kind}
		if !b.unknownCounts {
			ent.CountsKnown = true
			ent.ActiveCount = int64(len(e.active))
			ent.DeadLetterCount = int64(len(e.deadLetter))
		}
		out = append(out, ent)
	}
	return out, nil
}

// entity finds an entity; the caller holds b.mu.
func (b *Backend) entity(ns string, path string, kind bus.EntityKind) *entity {
	for _, e := range b.entities[ns] {
		if e.path == path && e.kind == kind {
			return e
		}
	}
	return nil
}

// Peek implements bus.Peeker. It never locks and never changes
// DeliveryCount.
func (b *Backend) Peek(ctx context.Context, req bus.PeekRequest) ([]bus.Message, error) {
	if err := b.before(ctx, OpPeek); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	found := b.entity(req.Namespace.Name, req.Entity.Path, req.Entity.Kind)
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
	for _, s := range src {
		if s.msg.SequenceNumber < req.FromSequence {
			continue
		}
		if len(out) == limit {
			break
		}
		out = append(out, s.msg)
	}
	return out, nil
}

// seedEntity builds an entity with dlq dead-letter and active messages.
// Dead-letter messages get sequence numbers from 2, one hour apart starting
// at Epoch+start; active messages follow them.
func seedEntity(path string, kind bus.EntityKind, idPrefix string, orderBase, dlq, active int, start time.Duration) *entity {
	e := &entity{path: path, kind: kind, idPrefix: idPrefix, orderBase: orderBase, start: start}
	seq := int64(2)
	for i := range dlq {
		e.deadLetter = append(e.deadLetter, &stored{msg: deadLetterMessage(i, seq, idPrefix, orderBase+int(seq), Epoch.Add(start+time.Duration(i)*time.Hour))})
		seq++
	}
	for i := range active {
		e.active = append(e.active, &stored{msg: activeMessage(seq, idPrefix, orderBase+int(seq), Epoch.Add(start+time.Duration(dlq+i)*time.Minute))})
		seq++
	}
	e.nextSeq = seq
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

// deadLetterMessage cycles through five shapes: JSON with a schema problem,
// JSON with a long line, plain text, invalid JSON, and one long unbroken
// non-JSON line.
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
	switch i % 5 {
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
	case 4:
		m.Body = fmt.Appendf(nil, "ERR|order=%d|payload=%s|end", order, strings.Repeat("QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVo0", 9))
		m.ContentType = "application/octet-stream"
		m.Subject = "OrderBlob"
		m.Properties = baseProps(order, true)[:1]
		m.DeadLetterReason = "PoisonMessage"
		m.DeadLetterErrorDescription = "Payload could not be decoded."
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
