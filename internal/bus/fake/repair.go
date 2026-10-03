package fake

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/samuelstrom93/lazybus/internal/bus"
)

// PlanRepair implements bus.Repairer with the real algorithm.
func (b *Backend) PlanRepair(ctx context.Context, req bus.RepairRequest) (bus.RepairPlan, error) {
	return b.svc.PlanRepair(ctx, req)
}

// Repair implements bus.Repairer with the real algorithm.
func (b *Backend) Repair(ctx context.Context, req bus.RepairRequest) (bus.RepairResult, error) {
	return b.svc.Repair(ctx, req)
}

// PlanCleanup implements bus.Repairer with the real algorithm.
func (b *Backend) PlanCleanup(ctx context.Context, req bus.RepairRequest) (bus.RepairPlan, error) {
	return b.svc.PlanCleanup(ctx, req)
}

// FinishCleanup implements bus.Repairer with the real algorithm.
func (b *Backend) FinishCleanup(ctx context.Context, req bus.RepairRequest) (bus.RepairResult, error) {
	return b.svc.FinishCleanup(ctx, req)
}

// Event is one state-changing broker call, recorded in order for tests.
type Event struct {
	Op     Op
	Entity string // "orders/$DLQ" or the target name for a send
	Seq    int64  // sequence number in the DLQ (receive, abandon, complete, send)
}

func (e Event) String() string { return fmt.Sprintf("%s %s %d", e.Op, e.Entity, e.Seq) }

// Events returns the state-changing calls made so far.
func (b *Backend) Events() []Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.events)
}

// ResetEvents clears the event record.
func (b *Backend) ResetEvents() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = nil
}

// Messages returns a copy of the messages of an entity's active queue or
// DLQ, in sequence order, as a peek would see them (for tests).
func (b *Backend) Messages(ns, path string, kind bus.EntityKind, sub bus.SubQueue) []bus.Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.entity(ns, path, kind)
	if e == nil {
		return nil
	}
	src := e.deadLetter
	if sub == bus.Active {
		src = e.active
	}
	out := make([]bus.Message, len(src))
	for i, s := range src {
		out[i] = s.msg
	}
	return out
}

// errLockLost is what abandon and complete return for an expired or
// foreign lock (the SDK's locklost).
var errLockLost = &bus.Error{Kind: bus.ErrUnknown, Msg: "lock lost: the lock expired or was released"}

// driver implements bus.Driver on the in-memory broker.
type driver struct{ b *Backend }

func (d driver) Peek(ctx context.Context, req bus.PeekRequest) ([]bus.Message, error) {
	return d.b.Peek(ctx, req)
}

func (d driver) TargetInfo(ctx context.Context, ns bus.Namespace, t bus.Target) (bus.TargetInfo, error) {
	b := d.b
	if err := b.before(ctx, OpTargetInfo); err != nil {
		return bus.TargetInfo{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var info bus.TargetInfo
	switch t.Kind {
	case bus.TargetQueue:
		if e := b.entity(ns.Name, t.Name, bus.KindQueue); e != nil {
			info.Exists, info.DuplicateDetection = true, e.dedup != nil
		}
	case bus.TargetTopic:
		if tp := b.topics[ns.Name][t.Name]; tp != nil {
			info.Exists, info.DuplicateDetection = true, tp.dedup != nil
			info.Subscriptions = len(b.subscriptions(ns.Name, t.Name))
		}
	}
	return info, nil
}

// subscriptions returns the subscriptions of topic; the caller holds b.mu.
func (b *Backend) subscriptions(ns, topic string) []*entity {
	var out []*entity
	for _, e := range b.entities[ns] {
		if e.kind == bus.KindSubscription && strings.HasPrefix(e.path, topic+"/") && !strings.Contains(e.path[len(topic)+1:], "/") {
			out = append(out, e)
		}
	}
	return out
}

func (d driver) DeadLetterReceiver(ctx context.Context, ns bus.Namespace, ent bus.Entity) (bus.DeadLetterReceiver, error) {
	b := d.b
	b.mu.Lock()
	e := b.entity(ns.Name, ent.Path, ent.Kind)
	b.mu.Unlock()
	if e == nil {
		return nil, &bus.Error{Kind: bus.ErrNotFound, Op: "receive " + ent.Path + "/$DLQ", Msg: "entity not found"}
	}
	e.scan.Lock()
	return &receiver{b: b, e: e, label: ent.Path + "/$DLQ"}, nil
}

type receiver struct {
	b     *Backend
	e     *entity
	label string
}

type locked struct {
	seq   int64
	id    string
	token int64
	until time.Time
}

func (l *locked) SequenceNumber() int64  { return l.seq }
func (l *locked) MessageID() string      { return l.id }
func (l *locked) LockedUntil() time.Time { return l.until }

func (r *receiver) Release() { r.e.scan.Unlock() }

// Receive locks up to max available DLQ messages in sequence order. Each
// lock adds one to DeliveryCount. Unlike the SDK it returns at once with
// nothing when nothing is available, so tests don't wait.
func (r *receiver) Receive(ctx context.Context, max int) ([]bus.Locked, error) {
	b := r.b
	if err := b.before(ctx, OpReceive); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	var out []bus.Locked
	for _, s := range r.e.deadLetter {
		if len(out) == max {
			break
		}
		if now.Before(s.lockedUntil) {
			continue // locked by someone
		}
		b.token++
		s.token, s.lockedUntil = b.token, now.Add(b.lockDuration)
		s.msg.DeliveryCount++
		out = append(out, &locked{seq: s.msg.SequenceNumber, id: s.msg.MessageID, token: s.token, until: s.lockedUntil})
		b.events = append(b.events, Event{OpReceive, r.label, s.msg.SequenceNumber})
	}
	return out, nil
}

// settle runs abandon or complete on m with the fault for op.
func (r *receiver) settle(ctx context.Context, op Op, m bus.Locked) error {
	b := r.b
	l := m.(*locked)
	f := b.fault(op, l.seq)
	if err := wait(ctx, f); err != nil {
		return err
	}
	if f.Err != nil && !f.After {
		return f.Err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	i := slices.IndexFunc(r.e.deadLetter, func(s *stored) bool { return s.msg.SequenceNumber == l.seq })
	if i < 0 || r.e.deadLetter[i].token != l.token || !b.now().Before(r.e.deadLetter[i].lockedUntil) {
		return errLockLost
	}
	if op == OpComplete {
		r.e.deadLetter = slices.Delete(r.e.deadLetter, i, i+1)
	} else {
		r.e.deadLetter[i].lockedUntil = time.Time{}
	}
	b.events = append(b.events, Event{op, r.label, l.seq})
	return f.Err // nil unless After
}

func (r *receiver) Abandon(ctx context.Context, m bus.Locked) error {
	return r.settle(ctx, OpAbandon, m)
}

func (r *receiver) Complete(ctx context.Context, m bus.Locked) error {
	return r.settle(ctx, OpComplete, m)
}

// Send builds the copy per spec §6 step 3.2 and delivers it: to a queue,
// or to every subscription of a topic whose filter matches. A duplicate
// MessageId inside the detection window is accepted and dropped.
func (d driver) Send(ctx context.Context, ns bus.Namespace, t bus.Target, original bus.Locked, messageID string) (bool, error) {
	b := d.b
	l := original.(*locked)
	f := b.fault(OpSend, l.seq)
	if err := wait(ctx, f); err != nil {
		return false, err
	}
	if f.Err != nil && !f.After {
		return f.Definite, f.Err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var src *stored
	for _, e := range b.entities[ns.Name] {
		for _, s := range e.deadLetter {
			if s.token == l.token && s.msg.SequenceNumber == l.seq {
				src = s
			}
		}
	}
	if src == nil {
		return false, fmt.Errorf("fake: original seq %d not found", l.seq)
	}
	out := outgoing(src.msg, messageID)

	var dd *dedup
	var dests []*entity
	switch t.Kind {
	case bus.TargetQueue:
		e := b.entity(ns.Name, t.Name, bus.KindQueue)
		if e == nil {
			return true, &bus.Error{Kind: bus.ErrNotFound, Msg: "amqp:not-found: entity " + t.Name}
		}
		dd, dests = e.dedup, []*entity{e}
	case bus.TargetTopic:
		tp := b.topics[ns.Name][t.Name]
		if tp == nil {
			return true, &bus.Error{Kind: bus.ErrNotFound, Msg: "amqp:not-found: entity " + t.Name}
		}
		dd = tp.dedup
		for _, e := range b.subscriptions(ns.Name, t.Name) {
			if e.filter == nil || e.filter(out) {
				dests = append(dests, e)
			}
		}
	}
	b.events = append(b.events, Event{OpSend, t.Name, l.seq})
	now := b.now()
	if dd != nil {
		if at, ok := dd.seen[messageID]; ok && now.Sub(at) < dd.window {
			return false, f.Err // accepted and dropped, like the broker
		}
		dd.seen[messageID] = now
	}
	for _, e := range dests {
		m := out
		m.SequenceNumber, m.EnqueuedTime = e.nextSeq, now
		e.nextSeq++
		e.active = append(e.active, &stored{msg: m})
	}
	return f.Definite, f.Err // nil unless After
}

// outgoing is the copy of m that a repair sends: body, application
// properties (types kept, markers never among them), Subject, ContentType,
// the new MessageId and the copied fields; no broker-owned fields.
func outgoing(m bus.Message, messageID string) bus.Message {
	return bus.Message{
		Body:             slices.Clone(m.Body),
		Properties:       slices.DeleteFunc(slices.Clone(m.Properties), func(p bus.Property) bool { return bus.IsMarker(p.Key) }),
		MessageID:        messageID,
		CorrelationID:    m.CorrelationID,
		Subject:          m.Subject,
		ContentType:      m.ContentType,
		SessionID:        m.SessionID,
		PartitionKey:     m.PartitionKey,
		To:               m.To,
		ReplyTo:          m.ReplyTo,
		ReplyToSessionID: m.ReplyToSessionID,
		TimeToLive:       m.TimeToLive,
	}
}
