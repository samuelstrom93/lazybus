package azure

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"github.com/Azure/go-amqp"

	"github.com/samuelstrom93/lazybus/internal/bus"
)

// PlanRepair implements bus.Repairer.
func (b *Backend) PlanRepair(ctx context.Context, req bus.RepairRequest) (bus.RepairPlan, error) {
	return b.service().PlanRepair(ctx, req)
}

// Repair implements bus.Repairer.
func (b *Backend) Repair(ctx context.Context, req bus.RepairRequest) (bus.RepairResult, error) {
	return b.service().Repair(ctx, req)
}

// PlanCleanup implements bus.Repairer.
func (b *Backend) PlanCleanup(ctx context.Context, req bus.RepairRequest) (bus.RepairPlan, error) {
	return b.service().PlanCleanup(ctx, req)
}

// FinishCleanup implements bus.Repairer.
func (b *Backend) FinishCleanup(ctx context.Context, req bus.RepairRequest) (bus.RepairResult, error) {
	return b.service().FinishCleanup(ctx, req)
}

func (b *Backend) service() *bus.Service { return b.svc }

// driver implements bus.Driver on the SDK.
type driver struct{ b *Backend }

func (d driver) Peek(ctx context.Context, req bus.PeekRequest) ([]bus.Message, error) {
	return d.b.Peek(ctx, req)
}

// TargetInfo reads RequiresDuplicateDetection with GetQueue/GetTopic and
// counts a topic's subscriptions with NewListSubscriptionsPager (runtime
// properties fail on the emulator, S−1).
func (d driver) TargetInfo(ctx context.Context, ns bus.Namespace, t bus.Target) (bus.TargetInfo, error) {
	c, err := d.b.conn(ns)
	if err != nil {
		return bus.TargetInfo{}, err
	}
	var info bus.TargetInfo
	err = Safe("get "+t.Kind.String()+" "+t.Name, func() error {
		switch t.Kind {
		case bus.TargetQueue:
			q, err := c.admin.GetQueue(ctx, t.Name, nil)
			if err != nil || q == nil { // (nil, nil) for a missing entity
				return err
			}
			info.Exists = true
			info.DuplicateDetection = q.RequiresDuplicateDetection != nil && *q.RequiresDuplicateDetection
		case bus.TargetTopic:
			tp, err := c.admin.GetTopic(ctx, t.Name, nil)
			if err != nil || tp == nil {
				return err
			}
			info.Exists = true
			info.DuplicateDetection = tp.RequiresDuplicateDetection != nil && *tp.RequiresDuplicateDetection
			pager := c.admin.NewListSubscriptionsPager(t.Name, nil)
			for pager.More() {
				page, err := pager.NextPage(ctx)
				if err != nil {
					return err
				}
				info.Subscriptions += len(page.Subscriptions)
			}
		}
		return nil
	})
	return info, err
}

// scanner is the one peek-lock receiver of a DLQ, kept open across repairs
// (a link holds no lock). use gives one repair at a time exclusive use:
// the SDK refuses concurrent ReceiveMessages calls.
type scanner struct {
	use sync.Mutex
	r   *azservicebus.Receiver // nil until opened, and after a failure
}

func (d driver) DeadLetterReceiver(ctx context.Context, ns bus.Namespace, e bus.Entity) (bus.DeadLetterReceiver, error) {
	c, err := d.b.conn(ns)
	if err != nil {
		return nil, err
	}
	key := receiverKey{e.Path, e.Kind, bus.DeadLetter}
	c.mu.Lock()
	s := c.scanners[key]
	if s == nil {
		s = &scanner{}
		c.scanners[key] = s
	}
	c.mu.Unlock()
	s.use.Lock()
	if s.r == nil {
		// Peek-lock (the default receive mode). The Go SDK has no prefetch:
		// each ReceiveMessages issues credits for exactly its batch and
		// releases leftovers (S−1).
		if s.r, err = c.openReceiver(key); err != nil {
			s.use.Unlock()
			return nil, mapErr("receive "+e.Path+"/$DLQ", err)
		}
	}
	return &scanHandle{s: s, label: e.Path + "/$DLQ"}, nil
}

type scanHandle struct {
	s     *scanner
	label string
}

// received is a message received in peek-lock.
type received struct{ m *azservicebus.ReceivedMessage }

func (r *received) SequenceNumber() int64 {
	if r.m.SequenceNumber == nil {
		return -1
	}
	return *r.m.SequenceNumber
}

func (r *received) MessageID() string { return r.m.MessageID }

// LockedUntil is zero when the broker did not send it or sent a sentinel
// (S−1 saw 10000-01-01 on a peeked message); the service then treats the
// lock as expired.
func (r *received) LockedUntil() time.Time {
	if r.m.LockedUntil == nil || r.m.LockedUntil.After(time.Now().Add(24*time.Hour)) {
		return time.Time{}
	}
	return *r.m.LockedUntil
}

func (h *scanHandle) Release() { h.s.use.Unlock() }

func (h *scanHandle) Receive(ctx context.Context, max int) ([]bus.Locked, error) {
	op := "receive " + h.label
	msgs, err := h.s.r.ReceiveMessages(ctx, max, nil)
	if err != nil {
		mapped := mapErr(op, err)
		if k := bus.KindOf(mapped); k != bus.ErrCanceled && k != bus.ErrTimeout {
			h.drop()
		}
		return nil, mapped
	}
	out := make([]bus.Locked, len(msgs))
	for i, m := range msgs {
		out[i] = &received{m}
	}
	return out, nil
}

// drop closes the receiver after a failure; the next repair opens a fresh
// link. The caller holds s.use.
func (h *scanHandle) drop() {
	r := h.s.r
	h.s.r = nil
	go func() { _ = r.Close(context.Background()) }()
}

func (h *scanHandle) Abandon(ctx context.Context, m bus.Locked) error {
	return mapErr(fmt.Sprintf("abandon %s seq %d", h.label, m.SequenceNumber()), h.s.r.AbandonMessage(ctx, m.(*received).m, nil))
}

func (h *scanHandle) Complete(ctx context.Context, m bus.Locked) error {
	return mapErr(fmt.Sprintf("complete %s seq %d", h.label, m.SequenceNumber()), h.s.r.CompleteMessage(ctx, m.(*received).m, nil))
}

// Send sends the copy of original to t and classifies a failure (§6 step
// 3.4).
func (d driver) Send(ctx context.Context, ns bus.Namespace, t bus.Target, original bus.Locked, messageID string) (bool, error) {
	op := "send to " + t.Name
	c, err := d.b.conn(ns)
	if err != nil {
		return true, err
	}
	s, err := c.sender(t.Name)
	if err != nil {
		return true, mapErr(op, err)
	}
	err = Safe(op, func() error {
		return sendDefiniteErr(s.SendMessage(ctx, outgoing(original.(*received).m, messageID), nil))
	})
	if err == nil {
		return false, nil
	}
	c.dropSender(t.Name, s)
	var de *definiteErr
	return errors.As(err, &de), err
}

// definiteErr marks a send error as a definite rejection, so the
// classification survives mapErr.
type definiteErr struct{ error }

func (e *definiteErr) Unwrap() error { return e.error }

func sendDefiniteErr(err error) error {
	if err != nil && sendDefinite(err) {
		return &definiteErr{err}
	}
	return err
}

// sendDefinite reports whether a send error is a definite rejection
// (nothing was delivered). Everything else is ambiguous (S−1 §7).
func sendDefinite(err error) bool {
	var ae *amqp.Error
	if errors.As(err, &ae) {
		switch ae.Condition {
		case amqp.ErrCondNotFound, amqp.ErrCondNotAllowed, amqp.ErrCondUnauthorizedAccess,
			amqp.ErrCondMessageSizeExceeded, amqp.ErrCondResourceLimitExceeded,
			"com.microsoft:entity-disabled", "com.microsoft:server-busy":
			return true
		}
		return false
	}
	var se *azservicebus.Error
	if errors.As(err, &se) {
		return se.Code == azservicebus.CodeUnauthorizedAccess || se.Code == azservicebus.CodeNotFound
	}
	return errors.Is(err, azservicebus.ErrMessageTooLarge)
}

// outgoing builds the copy a DLQ Repair sends (§6 step 3.2): the original
// body, application properties without the Dead-letter Markers (values
// copied as-is, so their types stay), Subject and ContentType, messageID,
// and CorrelationID, SessionID, PartitionKey, To, ReplyTo,
// ReplyToSessionID, TimeToLive. Broker-owned fields can't be set on a
// Message at all.
func outgoing(m *azservicebus.ReceivedMessage, messageID string) *azservicebus.Message {
	var props map[string]any
	for k, v := range m.ApplicationProperties {
		if bus.IsMarker(k) {
			continue
		}
		if props == nil {
			props = make(map[string]any, len(m.ApplicationProperties))
		}
		props[k] = v
	}
	out := &azservicebus.Message{
		ApplicationProperties: props,
		Body:                  m.Body,
		ContentType:           m.ContentType,
		CorrelationID:         m.CorrelationID,
		MessageID:             &messageID,
		PartitionKey:          m.PartitionKey,
		ReplyTo:               m.ReplyTo,
		ReplyToSessionID:      m.ReplyToSessionID,
		SessionID:             m.SessionID,
		Subject:               m.Subject,
		To:                    m.To,
	}
	if m.TimeToLive != nil {
		ttl := *m.TimeToLive
		out.TimeToLive = &ttl
	}
	return out
}

// sender returns the cached sender for a queue or topic.
func (c *conn) sender(name string) (*azservicebus.Sender, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.senders[name]; ok {
		return s, nil
	}
	s, err := c.client.NewSender(name, nil)
	if err != nil {
		return nil, err
	}
	c.senders[name] = s
	return s, nil
}

// dropSender closes and forgets s after a failed send.
func (c *conn) dropSender(name string, s *azservicebus.Sender) {
	c.mu.Lock()
	if c.senders[name] == s {
		delete(c.senders, name)
	}
	c.mu.Unlock()
	go func() { _ = s.Close(context.Background()) }()
}
