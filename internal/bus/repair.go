package bus

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// IsMarker reports whether key is a Dead-letter Marker.
func IsMarker(key string) bool {
	return key == MarkerDeadLetterReason || key == MarkerDeadLetterErrorDescription
}

// Markers are the application-property keys a DLQ Repair strips.
func Markers() []string {
	return []string{MarkerDeadLetterReason, MarkerDeadLetterErrorDescription}
}

// Outcome is the result of a DLQ Repair or a Finish Cleanup (spec §6).
type Outcome int

const (
	// Resubmitted: the copy is in the target and the original is gone.
	Resubmitted Outcome = iota
	// NotFound: the message was not in the DLQ (already gone, or not
	// within the scan limits); nothing was sent.
	NotFound
	// LockLost: the lock on the original ran out before the send; nothing
	// changed.
	LockLost
	// SendFailed: the target refused the copy; nothing changed.
	SendFailed
	// SendUncertain: the send ended ambiguously; the original is back in
	// the DLQ and the copy may or may not be in the target.
	SendUncertain
	// CleanupPending: the copy is in the target and the original is still
	// in the DLQ. Resolved by Finish Cleanup.
	CleanupPending
	// Cleaned: Finish Cleanup removed the original; nothing was sent.
	Cleaned
)

func (o Outcome) String() string {
	switch o {
	case Resubmitted:
		return "Resubmitted"
	case NotFound:
		return "NotFound"
	case LockLost:
		return "LockLost"
	case SendFailed:
		return "SendFailed"
	case SendUncertain:
		return "SendUncertain"
	case CleanupPending:
		return "CleanupPending"
	case Cleaned:
		return "Cleaned"
	}
	return "Unknown"
}

// NotFoundCause tells the two NotFound cases apart (spec §6 step 4).
type NotFoundCause int

const (
	// NotFoundGone: the pre-check did not find the message; it is no
	// longer in the DLQ and nothing was locked.
	NotFoundGone NotFoundCause = iota
	// NotFoundScan: the pre-check saw it, but the scan did not reach it
	// within the scan limit or the lock deadline; it may still be in the
	// DLQ.
	NotFoundScan
)

// TargetKind tells queue and topic targets apart.
type TargetKind int

const (
	TargetQueue TargetKind = iota
	TargetTopic
)

func (k TargetKind) String() string {
	if k == TargetTopic {
		return "topic"
	}
	return "queue"
}

// Target is a Resubmit Target: a queue or topic on the same namespace.
type Target struct {
	Name string
	Kind TargetKind
}

func (t Target) String() string { return t.Name }

// TargetOf returns the Resubmit Target for the dead-letter queue of e: a
// queue's DLQ goes back to that queue, a subscription's DLQ to its parent
// topic (§6, §12.1). Never derived from DeadLetterSource.
func TargetOf(e Entity) (Target, error) {
	switch e.Kind {
	case KindQueue:
		return Target{Name: e.Path, Kind: TargetQueue}, nil
	case KindSubscription:
		// Subscription names can't contain '/', topic names can.
		i := strings.LastIndex(e.Path, "/")
		if i <= 0 {
			return Target{}, fmt.Errorf("%q is not topic/subscription", e.Path)
		}
		return Target{Name: e.Path[:i], Kind: TargetTopic}, nil
	}
	return Target{}, fmt.Errorf("%q: unknown entity kind", e.Path)
}

// MessageIDMode chooses the MessageId of the outgoing copy.
type MessageIDMode int

const (
	// MessageIDDefault keeps the original MessageId, except on a target
	// with duplicate detection, where a new one is used (§6.1).
	MessageIDDefault MessageIDMode = iota
	// MessageIDKeep keeps the original MessageId.
	MessageIDKeep
	// MessageIDNew uses a new MessageId.
	MessageIDNew
)

// RepairRequest identifies one dead-letter message to repair (or, for
// Finish Cleanup, to remove).
type RepairRequest struct {
	Namespace Namespace
	Entity    Entity
	// SubQueue must be DeadLetter; anything else is refused.
	SubQueue       SubQueue
	SequenceNumber int64
	// Index is the message's row in the peeked list; it sets the scan
	// limit (ScanLimit).
	Index     int
	MessageID MessageIDMode
	// NewMessageID is the id used when a new one is chosen; empty means
	// generate one. PlanRepair proposes one so the confirm popup and the
	// log show the same id.
	NewMessageID string
	// Edits are the message's Pending Edits; the copy carries them (§6
	// step 3.2). Ignored by Finish Cleanup.
	Edits Edits
}

// Repair tuning (spec §6 step 3).
const (
	// ScanBatch is the number of messages one peek-lock receive asks for.
	ScanBatch = 10
	// LockMargin is subtracted from every LockedUntil before it is trusted.
	LockMargin = 10 * time.Second
	// ScanReceiveWait bounds one receive of the scan; a receive that gets
	// nothing in that time ends the scan (the DLQ has nothing more to give).
	ScanReceiveWait = 10 * time.Second
	// settleTimeout bounds abandons and completes. They run on a context
	// detached from the caller's: locks are released even when the caller
	// gave up.
	settleTimeout = 30 * time.Second
)

// ScanLimit is K for the message at row index: the most messages the
// by-sequence scan locks (the row's index plus one page).
func ScanLimit(index int) int { return max(0, index) + PageSize }

// RepairPlan is what the confirm popup shows. It comes from the pre-check
// (a peek, no lock) and the target guards.
type RepairPlan struct {
	Source         string // "orders/$DLQ"
	SequenceNumber int64
	// Found is false when the pre-check did not find the message (NotFound,
	// zero messages touched).
	Found   bool
	Message Message

	Target             Target
	Subscriptions      int  // topic targets: number of subscriptions
	DuplicateDetection bool // the target has RequiresDuplicateDetection
	// NewIDByDefault is true when the repair uses a new MessageId unless
	// the request keeps the original.
	NewIDByDefault bool
	// NewMessageID is the id a new-id repair uses; pass it back in the
	// request.
	NewMessageID string

	ScanLimit int
	// ScanCost describes the DeliveryCount cost of the by-sequence scan.
	ScanCost string
	// Warnings about the target (fan-out, duplicate detection).
	Warnings []string
	// MarkersRemoved are the Dead-letter Markers the copy will not carry.
	MarkersRemoved []string
}

// RepairResult reports what a DLQ Repair or Finish Cleanup did.
type RepairResult struct {
	Outcome        Outcome
	Source         string // "orders/$DLQ"
	SequenceNumber int64
	Target         Target
	OldMessageID   string
	NewMessageID   string // equal to OldMessageID when kept; empty for cleanup
	// Detail is a short reason for the outcome ("already gone", the send
	// error, …).
	Detail string
	// NotFound says which NotFound this is; only set with Outcome NotFound.
	NotFound NotFoundCause
	// Locked is the number of messages the scan locked, the match included.
	Locked int
	// AbandonErrors lists every abandon that failed; never swallowed.
	AbandonErrors []AbandonError
	// Err is the broker error behind SendFailed, SendUncertain or
	// CleanupPending.
	Err error
	// Log holds one line per state-changing broker call, for the command
	// log.
	Log []LogLine
}

// AbandonError is one failed abandon.
type AbandonError struct {
	SequenceNumber int64
	Err            error
}

// LogLine is one command-log line.
type LogLine struct {
	Text string
	Err  bool
}

func (r *RepairResult) logf(isErr bool, format string, args ...any) {
	r.Log = append(r.Log, LogLine{Text: fmt.Sprintf(format, args...), Err: isErr})
}

// Repairer performs DLQ Repairs and Finish Cleanups. Guards are enforced
// here, not in the UI.
type Repairer interface {
	// PlanRepair runs the pre-check and the target guards without taking
	// a lock.
	PlanRepair(ctx context.Context, req RepairRequest) (RepairPlan, error)
	// Repair runs one DLQ Repair. A non-nil error means nothing was sent
	// (refused by a guard, or a broker failure before the send); the
	// result's Log still lists every call made.
	Repair(ctx context.Context, req RepairRequest) (RepairResult, error)
	// PlanCleanup runs the pre-check for a Finish Cleanup.
	PlanCleanup(ctx context.Context, req RepairRequest) (RepairPlan, error)
	// FinishCleanup removes the original of a CleanupPending message
	// without sending anything: Cleaned, NotFound or LockLost.
	FinishCleanup(ctx context.Context, req RepairRequest) (RepairResult, error)
}

// Backend is everything the UI needs.
type Backend interface {
	Browser
	Repairer
}

// --- driver ------------------------------------------------------------------

// TargetInfo is what the guards need to know about a Resubmit Target.
type TargetInfo struct {
	Exists             bool
	DuplicateDetection bool
	// Subscriptions is the number of subscriptions of a topic, counted by
	// listing them (runtime properties fail on the emulator).
	Subscriptions int
}

// Locked is a message received in peek-lock by a DeadLetterReceiver.
type Locked interface {
	SequenceNumber() int64
	MessageID() string
	// LockedUntil is when the lock expires; zero when unknown.
	LockedUntil() time.Time
}

// DeadLetterReceiver is the peek-lock receiver of one repair or Finish
// Cleanup call. It is opened for the call and closed before the call
// returns: an open link's leftover credits would make the SDK receive (and
// lock) messages after the call (spec §6 step 3.1). Every settle of a
// message runs on the receiver that received it.
type DeadLetterReceiver interface {
	// Receive receives up to max messages in peek-lock without prefetch.
	// It may block until ctx ends when the DLQ has nothing to deliver.
	Receive(ctx context.Context, max int) ([]Locked, error)
	Abandon(ctx context.Context, m Locked) error
	Complete(ctx context.Context, m Locked) error
	// Close closes the link; called once, after every settle.
	Close(ctx context.Context) error
}

// Driver is the broker access a Service needs. Azure and the fake
// implement it; the repair algorithm lives in Service only.
type Driver interface {
	Peeker
	TargetInfo(ctx context.Context, ns Namespace, t Target) (TargetInfo, error)
	// DeadLetterReceiver opens a new peek-lock receiver on e's DLQ.
	DeadLetterReceiver(ctx context.Context, ns Namespace, e Entity) (DeadLetterReceiver, error)
	// Send builds the copy of original (§6 step 3.2: markers stripped,
	// property types kept, listed fields copied, messageID set, edits
	// applied) and sends it to t. definite reports a definite rejection
	// (nothing was delivered); otherwise a send error is ambiguous.
	Send(ctx context.Context, ns Namespace, t Target, original Locked, messageID string, edits Edits) (definite bool, err error)
}

// --- service -----------------------------------------------------------------

// Service implements Repairer on a Driver.
type Service struct {
	d     Driver
	now   func() time.Time
	newID func() string
}

var _ Repairer = (*Service)(nil)

// NewService returns a Service on d. now (nil: time.Now) is the clock for
// lock checks; newID (nil: random UUID) makes new MessageIds.
func NewService(d Driver, now func() time.Time, newID func() string) *Service {
	if now == nil {
		now = time.Now
	}
	if newID == nil {
		newID = uuid.NewString
	}
	return &Service{d: d, now: now, newID: newID}
}

func sourceLabel(e Entity) string { return e.Path + "/$DLQ" }

func refused(op, format string, args ...any) error {
	return &Error{Kind: ErrRefused, Op: op, Msg: fmt.Sprintf(format, args...)}
}

// guard enforces §6.1 before anything is locked: source is a DLQ, the
// edits are valid (no Dead-letter Marker among them), the target exists on
// the same namespace, a topic target has subscriptions.
func (s *Service) guard(ctx context.Context, op string, req RepairRequest) (Target, TargetInfo, error) {
	if req.SubQueue != DeadLetter {
		return Target{}, TargetInfo{}, refused(op, "repair only from a dead-letter queue")
	}
	if err := req.Edits.Validate(); err != nil {
		return Target{}, TargetInfo{}, refused(op, "%v", err)
	}
	t, err := TargetOf(req.Entity)
	if err != nil {
		return Target{}, TargetInfo{}, refused(op, "%v", err)
	}
	info, err := s.d.TargetInfo(ctx, req.Namespace, t)
	if err != nil {
		return t, info, err
	}
	if !info.Exists {
		return t, info, refused(op, "target %s %s does not exist", t.Kind, t.Name)
	}
	if t.Kind == TargetTopic && info.Subscriptions == 0 {
		return t, info, refused(op, "topic %s has no subscriptions: the copy would be dropped and the original deleted", t.Name)
	}
	return t, info, nil
}

// precheck peeks one message from sequence N (no lock): found when the
// broker returns N itself.
func (s *Service) precheck(ctx context.Context, req RepairRequest) (Message, bool, error) {
	msgs, err := s.d.Peek(ctx, PeekRequest{
		Namespace: req.Namespace, Entity: req.Entity, SubQueue: DeadLetter,
		FromSequence: req.SequenceNumber, Max: 1,
	})
	if err != nil {
		return Message{}, false, err
	}
	if len(msgs) == 0 || msgs[0].SequenceNumber != req.SequenceNumber {
		return Message{}, false, nil
	}
	return msgs[0], true, nil
}

func scanCost(k int) string {
	return fmt.Sprintf("locks up to %d messages ahead of it briefly; their DeliveryCount does not change", k)
}

// PlanRepair implements Repairer.
func (s *Service) PlanRepair(ctx context.Context, req RepairRequest) (RepairPlan, error) {
	op := fmt.Sprintf("resubmit %s seq %d", sourceLabel(req.Entity), req.SequenceNumber)
	p := RepairPlan{
		Source: sourceLabel(req.Entity), SequenceNumber: req.SequenceNumber,
		ScanLimit: ScanLimit(req.Index), MarkersRemoved: Markers(),
	}
	p.ScanCost = scanCost(p.ScanLimit)
	t, info, err := s.guard(ctx, op, req)
	if err != nil {
		return p, err
	}
	p.Target, p.Subscriptions, p.DuplicateDetection = t, info.Subscriptions, info.DuplicateDetection
	p.NewIDByDefault = info.DuplicateDetection
	p.NewMessageID = req.NewMessageID
	if p.NewMessageID == "" {
		p.NewMessageID = s.newID()
	}
	if t.Kind == TargetTopic {
		p.Warnings = append(p.Warnings, fmt.Sprintf(
			"fans out to %d subscription%s: delivered only to subscriptions whose rules match; no match = dropped",
			info.Subscriptions, plural(info.Subscriptions)))
	}
	if info.DuplicateDetection {
		p.Warnings = append(p.Warnings, fmt.Sprintf(
			"%s has duplicate detection: a copy with the original MessageId may be dropped as a duplicate", t.Name))
	}
	p.Message, p.Found, err = s.precheck(ctx, req)
	if err == nil && p.Found {
		err = bodyGuard(op, p.Message)
	}
	return p, err
}

// PlanCleanup implements Repairer.
func (s *Service) PlanCleanup(ctx context.Context, req RepairRequest) (RepairPlan, error) {
	op := fmt.Sprintf("finish cleanup %s seq %d", sourceLabel(req.Entity), req.SequenceNumber)
	p := RepairPlan{Source: sourceLabel(req.Entity), SequenceNumber: req.SequenceNumber, ScanLimit: ScanLimit(req.Index)}
	p.ScanCost = scanCost(p.ScanLimit)
	if req.SubQueue != DeadLetter {
		return p, refused(op, "finish cleanup only on a dead-letter queue")
	}
	if t, err := TargetOf(req.Entity); err == nil {
		p.Target = t
	}
	var err error
	p.Message, p.Found, err = s.precheck(ctx, req)
	return p, err
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// Repair implements Repairer: spec §6 step 3. Calls for one DLQ must not
// overlap: two scans would lock each other's messages (the UI runs one
// state-changing call at a time).
func (s *Service) Repair(ctx context.Context, req RepairRequest) (RepairResult, error) {
	src := sourceLabel(req.Entity)
	op := fmt.Sprintf("resubmit %s seq %d", src, req.SequenceNumber)
	res := RepairResult{Source: src, SequenceNumber: req.SequenceNumber}
	t, info, err := s.guard(ctx, op, req)
	res.Target = t
	if err != nil {
		return res, err
	}
	if msg, found, err := s.precheck(ctx, req); err != nil {
		return res, err
	} else if !found {
		res.Outcome, res.Detail = NotFound, "already gone (nothing locked)"
		return res, nil
	} else if err := bodyGuard(op, msg); err != nil {
		return res, err
	}
	err = s.withReceiver(ctx, req, &res, func(rcv DeadLetterReceiver) error {
		return s.repair(ctx, rcv, req, t, info, &res)
	})
	return res, err
}

// repair is §6 steps 3.1 to 3.5 on the call's receiver.
func (s *Service) repair(ctx context.Context, rcv DeadLetterReceiver, req RepairRequest, t Target, info TargetInfo, res *RepairResult) error {
	src := res.Source
	match, stop, err := s.scan(ctx, rcv, req, res)
	if err != nil {
		return err
	}
	if match == nil {
		res.Outcome, res.NotFound, res.Detail = NotFound, NotFoundScan, stop
		return nil
	}

	// §6 step 3.2: the copy's MessageId.
	res.OldMessageID = match.MessageID()
	res.NewMessageID = res.OldMessageID
	if req.MessageID == MessageIDNew || req.MessageID == MessageIDDefault && info.DuplicateDetection {
		res.NewMessageID = req.NewMessageID
		if res.NewMessageID == "" {
			res.NewMessageID = s.newID()
		}
	}

	// §6 step 3.3: local lock check on the received message.
	if !s.lockValid(match) {
		s.abandon(ctx, rcv, res, []Locked{match})
		res.Outcome, res.Detail = LockLost, "lock on the message expires too soon to send (nothing changed)"
		return nil
	}
	// §6 step 3.4: a call whose time is up sends nothing (a send on a
	// dead context would only come back ambiguous).
	if ctx.Err() != nil {
		s.abandon(ctx, rcv, res, []Locked{match})
		res.Outcome, res.Detail = LockLost, "the call ran out of time before the send (nothing sent)"
		return nil
	}

	// §6 step 3.4: send.
	ids := "MessageId " + res.OldMessageID
	if res.NewMessageID != res.OldMessageID {
		ids += " → " + res.NewMessageID
	}
	if n := req.Edits.Count(); n > 0 {
		ids += fmt.Sprintf(", %d edit%s", n, plural(n))
	}
	definite, err := s.d.Send(ctx, req.Namespace, t, match, res.NewMessageID, req.Edits)
	if err != nil {
		res.Err = err
		if definite {
			res.logf(true, "send %s seq %d → %s %s (%s) → rejected: %s", src, req.SequenceNumber, t.Kind, t.Name, ids, errMsg(err))
			res.Outcome, res.Detail = SendFailed, errMsg(err)
		} else {
			res.logf(true, "send %s seq %d → %s %s (%s) → uncertain: %s", src, req.SequenceNumber, t.Kind, t.Name, ids, errMsg(err))
			res.Outcome, res.Detail = SendUncertain, errMsg(err)
		}
		s.abandon(ctx, rcv, res, []Locked{match})
		return nil
	}
	res.logf(false, "send %s seq %d → %s %s (%s) → ok", src, req.SequenceNumber, t.Kind, t.Name, ids)

	// §6 step 3.5: complete the original.
	if err := s.complete(ctx, rcv, res, match); err != nil {
		res.Err = err
		res.Outcome, res.Detail = CleanupPending, errMsg(err)
		return nil
	}
	res.Outcome = Resubmitted
	return nil
}

// FinishCleanup implements Repairer: the same scan as Repair, then
// complete the match; nothing is sent.
func (s *Service) FinishCleanup(ctx context.Context, req RepairRequest) (RepairResult, error) {
	src := sourceLabel(req.Entity)
	op := fmt.Sprintf("finish cleanup %s seq %d", src, req.SequenceNumber)
	res := RepairResult{Source: src, SequenceNumber: req.SequenceNumber}
	if req.SubQueue != DeadLetter {
		return res, refused(op, "finish cleanup only on a dead-letter queue")
	}
	if t, err := TargetOf(req.Entity); err == nil {
		res.Target = t
	}
	if _, found, err := s.precheck(ctx, req); err != nil {
		return res, err
	} else if !found {
		res.Outcome, res.Detail = NotFound, "already gone (nothing locked)"
		return res, nil
	}
	err := s.withReceiver(ctx, req, &res, func(rcv DeadLetterReceiver) error {
		match, stop, err := s.scan(ctx, rcv, req, &res)
		if err != nil {
			return err
		}
		if match == nil {
			res.Outcome, res.NotFound, res.Detail = NotFound, NotFoundScan, stop
			return nil
		}
		res.OldMessageID = match.MessageID()
		if !s.lockValid(match) {
			s.abandon(ctx, rcv, &res, []Locked{match})
			res.Outcome, res.Detail = LockLost, "lock on the message expires too soon (nothing changed)"
			return nil
		}
		if ctx.Err() != nil {
			s.abandon(ctx, rcv, &res, []Locked{match})
			res.Outcome, res.Detail = LockLost, "the call ran out of time before the complete (nothing changed)"
			return nil
		}
		if err := s.complete(ctx, rcv, &res, match); err != nil {
			// The original may or may not be gone; the row stays
			// CleanupPending and another c finds out (NotFound if it is gone).
			res.Err = err
			return &Error{Kind: KindOf(err), Op: op, Msg: "complete failed: " + errMsg(err), Err: err}
		}
		res.Outcome = Cleaned
		return nil
	})
	return res, err
}

// withReceiver opens the call's peek-lock receiver, runs f on it and closes
// it before returning, whatever f did (spec §6 step 3.1).
func (s *Service) withReceiver(ctx context.Context, req RepairRequest, res *RepairResult, f func(DeadLetterReceiver) error) error {
	rcv, err := s.d.DeadLetterReceiver(ctx, req.Namespace, req.Entity)
	if err != nil {
		return err
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
		defer cancel()
		if err := safely(func() error { return rcv.Close(cctx) }); err != nil {
			res.logf(true, "close receiver %s → error: %s", res.Source, errMsg(err))
		}
	}()
	return f(rcv)
}

// safely runs f and turns a panic into an error, so a broken settle or
// close never takes the process down while other locks are held.
func safely(f func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("internal error: %v", r)
		}
	}()
	return f()
}

// bodyGuard refuses a message the copy can't carry unchanged (spec §6
// step 1): a body other than exactly one data section, or a message-id
// that is not a string.
func bodyGuard(op string, m Message) error {
	if m.Unsupported == "" {
		return nil
	}
	return refused(op, "unsupported AMQP body/message-id; not repaired (%s)", m.Unsupported)
}

// lockDeadline is the time a lock stops being trusted: LockedUntil minus
// LockMargin. An unknown LockedUntil is treated as already expired.
func (s *Service) lockDeadline(m Locked) time.Time {
	lu := m.LockedUntil()
	if lu.IsZero() {
		return s.now()
	}
	return lu.Add(-LockMargin)
}

func (s *Service) lockValid(m Locked) bool {
	return s.now().Before(s.lockDeadline(m))
}

// scan is §6 step 3.1: receive the DLQ in peek-lock, batches of ScanBatch,
// until sequence N turns up. Non-matching messages keep their locks until
// the scan ends (abandoning one puts it back at the head and the scan
// never advances, S−1); then all of them are abandoned at once, the
// post-match siblings included, and every abandon is awaited. The scan
// stops after ScanLimit messages, or LockMargin before the first held
// lock expires. A nil match with a nil error is NotFound; stop says why.
func (s *Service) scan(ctx context.Context, rcv DeadLetterReceiver, req RepairRequest, res *RepairResult) (match Locked, stop string, err error) {
	limit := ScanLimit(req.Index)
	var held, all []Locked
	var deadline time.Time
	defer func() {
		if len(all) > 0 {
			res.logf(false, "receive (peek-lock) %s → locked seq %s", res.Source, seqList(all))
		} else if err == nil {
			res.logf(false, "receive (peek-lock) %s → nothing", res.Source)
		}
		res.Locked = len(all)
		if err != nil && match != nil {
			held = append(held, match)
			match = nil
		}
		s.abandon(ctx, rcv, res, held)
	}()
	for len(all) < limit {
		wait := ScanReceiveWait
		if !deadline.IsZero() {
			left := deadline.Sub(s.now())
			if left <= 0 {
				return nil, fmt.Sprintf("not found before the first lock got within %s of expiring", LockMargin), nil
			}
			wait = min(wait, left)
		}
		rctx, cancel := context.WithTimeout(ctx, wait)
		got, err := rcv.Receive(rctx, min(ScanBatch, limit-len(all)))
		timedOut := rctx.Err() != nil && ctx.Err() == nil
		cancel()
		if err != nil && !timedOut {
			res.logf(true, "receive (peek-lock) %s → error: %s", res.Source, errMsg(err))
			return nil, "", err
		}
		if len(got) == 0 {
			return nil, fmt.Sprintf("not found: the DLQ delivered nothing more after %d messages", len(all)), nil
		}
		for _, m := range got {
			all = append(all, m)
			if deadline.IsZero() {
				deadline = s.lockDeadline(m)
			}
			if match == nil && m.SequenceNumber() == req.SequenceNumber {
				match = m
			} else {
				held = append(held, m)
			}
		}
		if match != nil {
			return match, "", nil
		}
	}
	return nil, fmt.Sprintf("not within the first %d messages (scan limit)", limit), nil
}

// abandon abandons ms at once and awaits every abandon, on a context the
// caller can't cancel. Failures go to res.AbandonErrors and the log.
func (s *Service) abandon(ctx context.Context, rcv DeadLetterReceiver, res *RepairResult, ms []Locked) {
	if len(ms) == 0 {
		return
	}
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
	defer cancel()
	errs := make([]error, len(ms))
	var wg sync.WaitGroup
	for i, m := range ms {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = safely(func() error { return rcv.Abandon(actx, m) })
		}()
	}
	wg.Wait()
	var ok []Locked
	for i, m := range ms {
		if errs[i] == nil {
			ok = append(ok, m)
			continue
		}
		res.AbandonErrors = append(res.AbandonErrors, AbandonError{SequenceNumber: m.SequenceNumber(), Err: errs[i]})
	}
	sort.Slice(res.AbandonErrors, func(i, j int) bool {
		return res.AbandonErrors[i].SequenceNumber < res.AbandonErrors[j].SequenceNumber
	})
	if len(ok) > 0 {
		res.logf(false, "abandon %s seq %s → ok", res.Source, seqList(ok))
	}
	for _, ae := range res.AbandonErrors {
		res.logf(true, "abandon %s seq %d → error: %s", res.Source, ae.SequenceNumber, errMsg(ae.Err))
	}
}

// complete completes m. When that fails, m is abandoned so no lock outlives
// the call (ADR 0002); if the complete did take effect, that abandon fails
// and is logged.
func (s *Service) complete(ctx context.Context, rcv DeadLetterReceiver, res *RepairResult, m Locked) error {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
	defer cancel()
	if err := safely(func() error { return rcv.Complete(cctx, m) }); err != nil {
		res.logf(true, "complete %s seq %d → error: %s", res.Source, m.SequenceNumber(), errMsg(err))
		s.abandon(ctx, rcv, res, []Locked{m})
		return err
	}
	res.logf(false, "complete %s seq %d → ok", res.Source, m.SequenceNumber())
	return nil
}

// errMsg is the short text of err: a *Error's Msg, else err.Error().
func errMsg(err error) string {
	var be *Error
	if errors.As(err, &be) && be.Msg != "" {
		return be.Msg
	}
	return err.Error()
}

// seqList formats sequence numbers in order, runs collapsed: "2-4,7".
func seqList(ms []Locked) string {
	seqs := make([]int64, len(ms))
	for i, m := range ms {
		seqs[i] = m.SequenceNumber()
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	var parts []string
	for i := 0; i < len(seqs); {
		j := i
		for j+1 < len(seqs) && seqs[j+1] == seqs[j]+1 {
			j++
		}
		p := strconv.FormatInt(seqs[i], 10)
		if j > i {
			p += "-" + strconv.FormatInt(seqs[j], 10)
		}
		parts = append(parts, p)
		i = j + 1
	}
	return strings.Join(parts, ",")
}
