package bus

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Tests for Service.scan against a scripted receiver: the paths the fake
// transport can't reach (a duplicate delivery, a caller cancel inside a
// receive, the exact batch sizes).

var scanT0 = time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

type scanMsg struct {
	seq   int64
	until time.Time
}

func (m *scanMsg) SequenceNumber() int64  { return m.seq }
func (m *scanMsg) MessageID() string      { return fmt.Sprint("id-", m.seq) }
func (m *scanMsg) LockedUntil() time.Time { return m.until }

// scriptRcv answers receive call i with script[i]; past the end it
// delivers nothing. It records every max asked for and every abandon.
type scriptRcv struct {
	script []func(ctx context.Context, max int) ([]Locked, error)

	maxes     []int
	mu        sync.Mutex
	abandoned []Locked
}

func (r *scriptRcv) Receive(ctx context.Context, max int) ([]Locked, error) {
	i := len(r.maxes)
	r.maxes = append(r.maxes, max)
	if i >= len(r.script) {
		return nil, nil
	}
	return r.script[i](ctx, max)
}

func (r *scriptRcv) Abandon(_ context.Context, m Locked) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.abandoned = append(r.abandoned, m)
	return nil
}

func (r *scriptRcv) Complete(context.Context, Locked) error { return errors.New("unexpected complete") }
func (r *scriptRcv) Close(context.Context) error            { return nil }

// deliver returns a script step that hands out ms.
func deliver(ms ...Locked) func(context.Context, int) ([]Locked, error) {
	return func(context.Context, int) ([]Locked, error) { return ms, nil }
}

// msgs makes messages seq from..to, locked for a minute from scanT0.
func msgs(from, to int64) []Locked {
	var out []Locked
	for seq := from; seq <= to; seq++ {
		out = append(out, &scanMsg{seq: seq, until: scanT0.Add(time.Minute)})
	}
	return out
}

func scanService(now *time.Time) *Service {
	return &Service{now: func() time.Time { return *now }}
}

func want(seqs ...int64) map[int64]bool {
	w := map[int64]bool{}
	for _, s := range seqs {
		w[s] = true
	}
	return w
}

// sameSet reports whether got and wantMs hold the same Locked values
// (identity, not sequence number).
func sameSet(got, wantMs []Locked) bool {
	if len(got) != len(wantMs) {
		return false
	}
	for _, m := range wantMs {
		if !slices.Contains(got, m) {
			return false
		}
	}
	return true
}

func logText(res *RepairResult) string {
	var b strings.Builder
	for _, l := range res.Log {
		fmt.Fprintf(&b, "%v %s\n", l.Err, l.Text)
	}
	return b.String()
}

func TestScanReturnsAsSoonAsEveryWantedIsFound(t *testing.T) {
	now := scanT0
	batch := msgs(1, 10)
	rcv := &scriptRcv{script: []func(context.Context, int) ([]Locked, error){deliver(batch...), deliver(msgs(11, 20)...)}}
	res := &RepairResult{Source: "q/$DLQ"}

	matches, stop, err := scanService(&now).scan(context.Background(), rcv, want(3, 7), 100, res)

	if err != nil || stop != "" {
		t.Fatalf("err %v, stop %q; want nil, \"\"", err, stop)
	}
	if len(rcv.maxes) != 1 {
		t.Errorf("receives = %v, want one", rcv.maxes)
	}
	if len(matches) != 2 || matches[3] != batch[2] || matches[7] != batch[6] {
		t.Errorf("matches = %v, want seq 3 and 7 of the batch", matches)
	}
	held := slices.Concat(batch[:2], batch[3:6], batch[7:])
	if !sameSet(rcv.abandoned, held) {
		t.Errorf("abandoned %v, want the 8 non-matches", rcv.abandoned)
	}
	if res.Locked != 10 {
		t.Errorf("Locked = %d, want 10", res.Locked)
	}
	if got, w := logText(res), "false receive (peek-lock) q/$DLQ → locked seq 1-10\n"; !strings.HasPrefix(got, w) {
		t.Errorf("log =\n%s\nwant prefix %q", got, w)
	}
}

func TestScanBatchSizeNearLimit(t *testing.T) {
	now := scanT0
	rcv := &scriptRcv{script: []func(context.Context, int) ([]Locked, error){
		deliver(msgs(1, 7)...), // fewer than asked
		deliver(msgs(8, 17)...),
		deliver(msgs(18, 25)...),
		deliver(msgs(26, 30)...), // must never be asked for
	}}
	res := &RepairResult{Source: "q/$DLQ"}

	matches, stop, err := scanService(&now).scan(context.Background(), rcv, want(99), 25, res)

	if err != nil || len(matches) != 0 {
		t.Fatalf("err %v, matches %v; want nil, none", err, matches)
	}
	if w := []int{ScanBatch, ScanBatch, 8}; !slices.Equal(rcv.maxes, w) {
		t.Errorf("receive max = %v, want %v (min(ScanBatch, limit-locked))", rcv.maxes, w)
	}
	if w := "not within the first 25 messages (scan limit)"; stop != w {
		t.Errorf("stop = %q, want %q", stop, w)
	}
	if len(rcv.abandoned) != 25 || res.Locked != 25 {
		t.Errorf("abandoned %d, Locked %d; want 25, 25", len(rcv.abandoned), res.Locked)
	}
}

func TestScanStopsAtFirstLockDeadline(t *testing.T) {
	now := scanT0
	// The first message's lock sets the deadline, though later ones last longer.
	first := &scanMsg{seq: 1, until: scanT0.Add(LockMargin + time.Second)}
	second := &scanMsg{seq: 2, until: scanT0.Add(time.Hour)}
	rcv := &scriptRcv{script: []func(context.Context, int) ([]Locked, error){
		func(context.Context, int) ([]Locked, error) {
			now = now.Add(time.Second) // exactly at the deadline
			return []Locked{first, second}, nil
		},
		deliver(msgs(3, 5)...),
	}}
	res := &RepairResult{Source: "q/$DLQ"}

	matches, stop, err := scanService(&now).scan(context.Background(), rcv, want(9), 100, res)

	if err != nil || len(matches) != 0 {
		t.Fatalf("err %v, matches %v; want nil, none", err, matches)
	}
	if w := "not found before the first lock got within 10s of expiring"; stop != w {
		t.Errorf("stop = %q, want %q", stop, w)
	}
	if len(rcv.maxes) != 1 {
		t.Errorf("receives = %v, want one", rcv.maxes)
	}
	if !sameSet(rcv.abandoned, []Locked{first, second}) {
		t.Errorf("abandoned %v, want both", rcv.abandoned)
	}
}

func TestScanDuplicateDeliveryIsHeld(t *testing.T) {
	now := scanT0
	five, again, seven := &scanMsg{seq: 5, until: scanT0.Add(time.Minute)}, &scanMsg{seq: 5, until: scanT0.Add(time.Minute)}, &scanMsg{seq: 7, until: scanT0.Add(time.Minute)}
	rcv := &scriptRcv{script: []func(context.Context, int) ([]Locked, error){deliver(five), deliver(again, seven)}}
	res := &RepairResult{Source: "q/$DLQ"}

	matches, stop, err := scanService(&now).scan(context.Background(), rcv, want(5, 7), 100, res)

	if err != nil || stop != "" {
		t.Fatalf("err %v, stop %q; want nil, \"\"", err, stop)
	}
	if matches[5] != five || matches[7] != seven {
		t.Errorf("matches = %v, want the first seq 5 and seq 7", matches)
	}
	if !sameSet(rcv.abandoned, []Locked{again}) {
		t.Errorf("abandoned %v, want only the second seq 5", rcv.abandoned)
	}
	if res.Locked != 3 {
		t.Errorf("Locked = %d, want 3", res.Locked)
	}
}

func TestScanCallerCancelMidScanAbandonsEverything(t *testing.T) {
	now := scanT0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	batch := msgs(1, 10)
	rcv := &scriptRcv{script: []func(context.Context, int) ([]Locked, error){
		deliver(batch...),
		func(rctx context.Context, _ int) ([]Locked, error) {
			cancel() // the user gives up while the receive waits
			<-rctx.Done()
			return nil, rctx.Err()
		},
	}}
	res := &RepairResult{Source: "q/$DLQ"}

	matches, stop, err := scanService(&now).scan(ctx, rcv, want(4, 99), 100, res)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(matches) != 0 || stop != "" {
		t.Errorf("matches %v, stop %q; want none, \"\" (the match is abandoned too)", matches, stop)
	}
	if !sameSet(rcv.abandoned, batch) {
		t.Errorf("abandoned %v, want all 10, the match included", rcv.abandoned)
	}
	if res.Locked != 10 {
		t.Errorf("Locked = %d, want 10", res.Locked)
	}
	if got := logText(res); !strings.Contains(got, "true receive (peek-lock) q/$DLQ → error: context canceled\n") {
		t.Errorf("log =\n%s\nwant the receive error", got)
	}
}

func TestScanReceiveTimeoutIsNotAnError(t *testing.T) {
	now := scanT0
	// The lock deadline bounds the second receive to 50ms of real time.
	first := &scanMsg{seq: 1, until: scanT0.Add(LockMargin + 50*time.Millisecond)}
	rcv := &scriptRcv{script: []func(context.Context, int) ([]Locked, error){
		deliver(first),
		func(rctx context.Context, _ int) ([]Locked, error) {
			<-rctx.Done() // nothing more: the receive runs out its wait
			return nil, rctx.Err()
		},
	}}
	res := &RepairResult{Source: "q/$DLQ"}

	matches, stop, err := scanService(&now).scan(context.Background(), rcv, want(9), 100, res)

	if err != nil || len(matches) != 0 {
		t.Fatalf("err %v, matches %v; want nil, none", err, matches)
	}
	if w := "not found: the DLQ delivered nothing more after 1 messages"; stop != w {
		t.Errorf("stop = %q, want %q", stop, w)
	}
	if !sameSet(rcv.abandoned, []Locked{first}) {
		t.Errorf("abandoned %v, want seq 1", rcv.abandoned)
	}
	if got := logText(res); strings.Contains(got, "error") {
		t.Errorf("log =\n%s\nwant no error line", got)
	}
}

func TestScanNothingDelivered(t *testing.T) {
	now := scanT0
	rcv := &scriptRcv{}
	res := &RepairResult{Source: "q/$DLQ"}

	matches, stop, err := scanService(&now).scan(context.Background(), rcv, want(1), 100, res)

	if err != nil || len(matches) != 0 {
		t.Fatalf("err %v, matches %v; want nil, none", err, matches)
	}
	if w := "not found: the DLQ delivered nothing more after 0 messages"; stop != w {
		t.Errorf("stop = %q, want %q", stop, w)
	}
	if got, w := logText(res), "false receive (peek-lock) q/$DLQ → nothing\n"; got != w {
		t.Errorf("log = %q, want %q", got, w)
	}
}

func TestScanReceiveErrorBeforeAnyMessage(t *testing.T) {
	now := scanT0
	boom := errors.New("boom")
	rcv := &scriptRcv{script: []func(context.Context, int) ([]Locked, error){
		func(context.Context, int) ([]Locked, error) { return nil, boom },
	}}
	res := &RepairResult{Source: "q/$DLQ"}

	_, _, err := scanService(&now).scan(context.Background(), rcv, want(1), 100, res)

	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	// No "→ nothing" line: the receive failed, it didn't come back empty.
	if got, w := logText(res), "true receive (peek-lock) q/$DLQ → error: boom\n"; got != w {
		t.Errorf("log = %q, want %q", got, w)
	}
}
