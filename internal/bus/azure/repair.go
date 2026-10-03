package azure

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"github.com/Azure/go-amqp"
	"github.com/google/uuid"

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

func (d driver) DeadLetterReceiver(ctx context.Context, ns bus.Namespace, e bus.Entity) (bus.DeadLetterReceiver, error) {
	c, err := d.b.conn(ns)
	if err != nil {
		return nil, err
	}
	label := e.Path + "/$DLQ"
	var r *azservicebus.Receiver
	// Peek-lock (the default receive mode). The Go SDK has no prefetch:
	// each ReceiveMessages issues credits for exactly its batch (S−1).
	err = Safe("receive "+label, func() error {
		var err error
		r, err = c.openReceiver(receiverKey{e.Path, e.Kind, bus.DeadLetter})
		return err
	})
	if err != nil {
		return nil, err
	}
	return &dlqReceiver{r: r, label: label}, nil
}

// dlqReceiver is the peek-lock receiver of one repair call. r never
// changes for its lifetime: every settle runs on the receiver that
// received the message, even after a receive error.
type dlqReceiver struct {
	r         *azservicebus.Receiver
	label     string
	abandoned atomic.Bool // an abandon was sent: Close waits releaseGrace
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

func (h *dlqReceiver) Receive(ctx context.Context, max int) ([]bus.Locked, error) {
	var msgs []*azservicebus.ReceivedMessage
	err := Safe("receive "+h.label, func() error {
		var err error
		msgs, err = h.r.ReceiveMessages(ctx, max, nil)
		return err
	})
	if err != nil {
		return nil, err
	}
	out := make([]bus.Locked, len(msgs))
	for i, m := range msgs {
		out[i] = &received{m}
	}
	return out, nil
}

func (h *dlqReceiver) Abandon(ctx context.Context, m bus.Locked) error {
	h.abandoned.Store(true)
	return Safe(fmt.Sprintf("abandon %s seq %d", h.label, m.SequenceNumber()), func() error {
		return h.r.AbandonMessage(ctx, m.(*received).m, nil)
	})
}

func (h *dlqReceiver) Complete(ctx context.Context, m bus.Locked) error {
	return Safe(fmt.Sprintf("complete %s seq %d", h.label, m.SequenceNumber()), func() error {
		return h.r.CompleteMessage(ctx, m.(*received).m, nil)
	})
}

// releaseGrace is how long Close waits before closing the link. An
// abandoned message comes straight back over the link's leftover credits;
// the SDK's releaser releases it, but Close stops the releaser, and a
// message caught in between stays locked until its lock expires. Measured
// on Azure (docs/spike-s-1.md, S4; 6 messages, repair of the third):
// closing at once stranded 3 of 15 abandoned siblings over 3 runs, 100 ms
// stranded 4 of 15, 500 ms none of 65 over 13 runs (the emulator needed
// 100 ms). Only paid by a call that abandoned something.
const releaseGrace = 500 * time.Millisecond

// Close closes the link, so no leftover credit can receive (and lock) a
// message after the call (spec §6 step 3.1).
func (h *dlqReceiver) Close(ctx context.Context) error {
	if h.abandoned.Load() {
		t := time.NewTimer(releaseGrace)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
		}
	}
	return Safe("close receiver "+h.label, func() error { return h.r.Close(ctx) })
}

// Send sends the copy of original to t and classifies a failure (§6 step
// 3.4).
func (d driver) Send(ctx context.Context, ns bus.Namespace, t bus.Target, original bus.Locked, messageID string, edits bus.Edits) (bool, error) {
	op := "send to " + t.Name
	c, err := d.b.conn(ns)
	if err != nil {
		return true, err
	}
	s, err := c.sender(t.Name)
	if err != nil {
		return true, mapErr(op, err)
	}
	msg, err := outgoing(original.(*received).m, messageID, edits)
	if err != nil {
		return true, mapErr(op, err)
	}
	err = Safe(op, func() error {
		return sendDefiniteErr(s.SendMessage(ctx, msg, nil))
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
// ReplyToSessionID, TimeToLive; then the Pending Edits: Property Edits
// (typed, see amqpValue), Subject, ContentType and body. Broker-owned
// fields can't be set on a Message at all.
func outgoing(m *azservicebus.ReceivedMessage, messageID string, edits bus.Edits) (*azservicebus.Message, error) {
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
	for _, pe := range edits.Properties {
		if pe.Remove {
			delete(props, pe.Key)
			continue
		}
		v, err := amqpValue(pe)
		if err != nil {
			return nil, err
		}
		if props == nil {
			props = map[string]any{}
		}
		props[pe.Key] = v
	}
	if len(props) == 0 {
		props = nil
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
	if edits.BodyEdited {
		out.Body = edits.Body
		if out.Body == nil {
			out.Body = []byte{}
		}
	}
	if edits.Subject != nil {
		out.Subject = optional(*edits.Subject)
	}
	if edits.ContentType != nil {
		out.ContentType = optional(*edits.ContentType)
	}
	return out, nil
}

// amqpValue is the value of a Property Edit as the SDK must send it to
// get the AMQP type of its PropertyType: int32 → int, int64 → long,
// float64 → double, bool → boolean, string → string, time.Time →
// timestamp, and a Guid (a string in bus) → amqp.UUID → uuid.
func amqpValue(pe bus.PropertyEdit) (any, error) {
	if pe.Type == bus.TypeGUID {
		s, _ := pe.Value.(string)
		u, err := uuid.Parse(s)
		if err != nil {
			return nil, &bus.Error{Kind: bus.ErrRefused, Msg: fmt.Sprintf("property %s: %q is not a GUID", pe.Key, s)}
		}
		return amqp.UUID(u), nil
	}
	return pe.Value, nil
}

// optional is nil for an empty string: an edit to "" unsets the field.
func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
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
