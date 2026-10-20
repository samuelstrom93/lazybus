package bus

import (
	"context"
	"fmt"
)

// MaxBatch is the most messages one bulk DLQ Repair moves. The scan holds
// the locks of every message up to the last one, and the sends run while
// they last (LockMargin before the first expires), so a batch stays small.
const MaxBatch = 25

// BatchItem is one message of a bulk DLQ Repair.
type BatchItem struct {
	SequenceNumber int64
	// Index is the message's row in the peeked list; the largest sets the
	// batch's scan limit.
	Index int
	// Edits are the message's Pending Edits; its copy carries them.
	Edits Edits
}

// BatchRequest asks to repair several messages of one DLQ in one call: one
// receiver, one scan for all of them, then send and complete each in turn.
type BatchRequest struct {
	Namespace Namespace
	Entity    Entity
	// SubQueue must be DeadLetter; anything else is refused.
	SubQueue SubQueue
	Items    []BatchItem
	// MessageID applies to every copy; a new id is made per message.
	MessageID MessageIDMode
}

// BatchPlan is what the bulk confirm popup shows: the target guards once,
// and the pre-check of each item. Its RepairPlan has no SequenceNumber,
// Message or NewMessageID.
type BatchPlan struct {
	RepairPlan
	Items []BatchPlanItem
}

// BatchPlanItem is the pre-check of one item, in request order.
type BatchPlanItem struct {
	SequenceNumber int64
	Found          bool
	Message        Message
	// Refused is why the item will be skipped (an unsupported body or id,
	// invalid edits); empty when it can go.
	Refused string
}

// BatchItemResult is the outcome of one item. Skipped, when set, says why
// the item was not attempted (refused, or the batch stopped before it);
// nothing changed for it and Outcome is not set.
type BatchItemResult struct {
	RepairResult
	Skipped string
}

// BatchResult reports a bulk DLQ Repair.
type BatchResult struct {
	Items []BatchItemResult // one per request item, in request order
	// Stopped is the sequence number whose SendUncertain or CleanupPending
	// stopped the batch; zero when it ran to the end.
	Stopped int64
	// Locked is the number of messages the scan locked, the matches
	// included.
	Locked        int
	AbandonErrors []AbandonError
	// Log holds one line per state-changing broker call, in order.
	Log []LogLine
}

// BatchRepairer is a Repairer that also repairs several messages in one
// call.
type BatchRepairer interface {
	PlanBatch(ctx context.Context, req BatchRequest) (BatchPlan, error)
	RepairBatch(ctx context.Context, req BatchRequest) (BatchResult, error)
}

// one is req as a RepairRequest for item it.
func (req BatchRequest) one(it BatchItem) RepairRequest {
	return RepairRequest{
		Namespace: req.Namespace, Entity: req.Entity, SubQueue: req.SubQueue,
		SequenceNumber: it.SequenceNumber, Index: it.Index, MessageID: req.MessageID, Edits: it.Edits,
	}
}

func (req BatchRequest) op() string {
	return fmt.Sprintf("resubmit %s, %d message%s", sourceLabel(req.Entity), len(req.Items), plural(len(req.Items)))
}

// check refuses a batch that is empty, too big or names a message twice.
func (req BatchRequest) check() error {
	switch n := len(req.Items); {
	case n == 0:
		return refused(req.op(), "no messages selected")
	case n > MaxBatch:
		return refused(req.op(), "at most %d messages per resubmit", MaxBatch)
	}
	seen := map[int64]bool{}
	for _, it := range req.Items {
		if seen[it.SequenceNumber] {
			return refused(req.op(), "seq %d is listed twice", it.SequenceNumber)
		}
		seen[it.SequenceNumber] = true
	}
	return nil
}

// precheckItem is the pre-check of one item: found, and refused when the
// copy could not carry it unchanged or its edits are invalid.
func (s *Service) precheckItem(ctx context.Context, req BatchRequest, it BatchItem) (Message, bool, string, error) {
	r := req.one(it)
	msg, found, err := s.precheck(ctx, r)
	if err != nil || !found {
		return msg, found, "", err
	}
	if err := it.Edits.Validate(); err != nil {
		return msg, true, "refused: " + err.Error(), nil
	}
	if err := bodyGuard(r.String(), msg); err != nil {
		return msg, true, "refused: " + errMsg(err), nil
	}
	return msg, true, "", nil
}

// String is the operation name of r in the log.
func (r RepairRequest) String() string {
	return fmt.Sprintf("resubmit %s seq %d", sourceLabel(r.Entity), r.SequenceNumber)
}

// PlanBatch implements BatchRepairer: the target guards once, then a
// pre-check of each item, without taking a lock.
func (s *Service) PlanBatch(ctx context.Context, req BatchRequest) (BatchPlan, error) {
	if err := req.check(); err != nil {
		return BatchPlan{}, err
	}
	maxIndex := 0
	for _, it := range req.Items {
		maxIndex = max(maxIndex, it.Index)
	}
	first, err := s.PlanRepair(ctx, RepairRequest{Namespace: req.Namespace, Entity: req.Entity, SubQueue: req.SubQueue, Index: maxIndex})
	p := BatchPlan{RepairPlan: first}
	p.SequenceNumber, p.Message, p.Found, p.NewMessageID = 0, Message{}, false, ""
	if err != nil {
		return p, err
	}
	for _, it := range req.Items {
		msg, found, why, err := s.precheckItem(ctx, req, it)
		if err != nil {
			return p, err
		}
		p.Items = append(p.Items, BatchPlanItem{SequenceNumber: it.SequenceNumber, Found: found, Message: msg, Refused: why})
	}
	return p, nil
}

// RepairBatch implements BatchRepairer: the pre-checks, one scan that finds
// every item still there, then send and complete each in request order. A
// SendUncertain or CleanupPending stops the batch: the items after it are
// abandoned unsent (Skipped). NotFound, LockLost and SendFailed leave
// nothing half done, so the batch goes on. A non-nil error means nothing
// was sent.
func (s *Service) RepairBatch(ctx context.Context, req BatchRequest) (res BatchResult, err error) {
	res.Items = make([]BatchItemResult, len(req.Items))
	if err := req.check(); err != nil {
		return res, err
	}
	src := sourceLabel(req.Entity)
	sink := &RepairResult{Source: src} // the batch's own calls: scan, abandons, close
	defer func() {
		res.Log, res.AbandonErrors, res.Locked = sink.Log, sink.AbandonErrors, sink.Locked
	}()
	t, info, gerr := s.guard(ctx, req.op(), RepairRequest{Namespace: req.Namespace, Entity: req.Entity, SubQueue: req.SubQueue})
	for i, it := range req.Items {
		res.Items[i].RepairResult = RepairResult{Source: src, SequenceNumber: it.SequenceNumber, Target: t}
	}
	if gerr != nil {
		return res, gerr
	}

	want := map[int64]bool{}
	maxIndex := 0
	for i, it := range req.Items {
		ir := &res.Items[i]
		_, found, why, err := s.precheckItem(ctx, req, it)
		switch {
		case err != nil:
			return res, err
		case !found:
			ir.Outcome, ir.NotFound, ir.Detail = NotFound, NotFoundGone, "already gone (nothing locked)"
		case why != "":
			ir.Skipped = why
		default:
			want[it.SequenceNumber] = true
			maxIndex = max(maxIndex, it.Index)
		}
	}
	if len(want) == 0 {
		return res, nil
	}

	r0 := RepairRequest{Namespace: req.Namespace, Entity: req.Entity, SubQueue: req.SubQueue}
	err = s.withReceiver(ctx, r0, sink, func(rcv DeadLetterReceiver) error {
		matches, stop, err := s.scan(ctx, rcv, want, ScanLimit(maxIndex), sink)
		if err != nil {
			return err
		}
		var leftover []Locked
		var stoppedBy Outcome
		for i, it := range req.Items {
			ir := &res.Items[i]
			if !want[it.SequenceNumber] {
				continue
			}
			m := matches[it.SequenceNumber]
			switch {
			case m == nil:
				ir.Outcome, ir.NotFound, ir.Detail = NotFound, NotFoundScan, stop
				continue
			case res.Stopped != 0:
				ir.Skipped = fmt.Sprintf("not attempted: the batch stopped after the %s of seq %d", stoppedBy, res.Stopped)
				leftover = append(leftover, m)
				continue
			}
			s.resubmit(ctx, rcv, req.one(it), t, info, m, &ir.RepairResult)
			// The batch log stays in call order.
			sink.Log = append(sink.Log, ir.Log...)
			ir.Log = nil
			sink.AbandonErrors = append(sink.AbandonErrors, ir.AbandonErrors...)
			if ir.Outcome == SendUncertain || ir.Outcome == CleanupPending {
				res.Stopped, stoppedBy = it.SequenceNumber, ir.Outcome
			}
		}
		s.abandon(ctx, rcv, sink, leftover)
		return nil
	})
	return res, err
}
