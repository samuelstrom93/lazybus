package ui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/samuelstrom93/lazybus/internal/bus"
	"github.com/samuelstrom93/lazybus/internal/bus/fake"
)

// UI tests never run tmux: the clipboard emits plain OSC 52.
func init() {
	newClipboard = func() clipboard { return clipboard{getenv: func(string) string { return "" }} }
}

func screenText(m Model) string { return ansi.Strip(m.View().Content) }

func messageIDs(m Model) []string {
	var ids []string
	for _, msg := range m.messages.items {
		ids = append(ids, msg.MessageID)
	}
	return ids
}

func TestFilterEachPanel(t *testing.T) {
	m := keysIn(t, loaded(t), "3", "/", "m", "a", "x")
	if m.stack.Top() != CtxFilter || m.messages.filter != "max" || strings.Join(messageIDs(m), ",") != "inv-1003" {
		t.Fatalf("messages filter: top %v, filter %q, rows %v", m.stack.Top(), m.messages.filter, messageIDs(m))
	}
	if !strings.Contains(screenText(m), "/max▏") {
		t.Fatalf("panel title lacks the filter:\n%s", screenText(m))
	}
	m = keysIn(t, m, "enter")
	if m.stack.Top() != CtxMessages || m.messages.filter != "max" || len(m.messages.items) != 1 {
		t.Fatalf("enter: top %v, filter %q, %d rows", m.stack.Top(), m.messages.filter, len(m.messages.items))
	}
	if s := screenText(m); !strings.Contains(s, "/max") || strings.Contains(s, "/max▏") {
		t.Fatalf("kept filter not shown as kept:\n%s", s)
	}
	// esc on the panel clears it; the cursor stays on its message.
	m = keysIn(t, m, "esc")
	if m.messages.filter != "" || len(m.messages.items) != 3 || selectedSeq(t, m) != 3 {
		t.Fatalf("esc: filter %q, %d rows, seq %d", m.messages.filter, len(m.messages.items), selectedSeq(t, m))
	}

	// Entities: esc while typing clears too.
	m = keysIn(t, m, "2", "/", "o", "r", "d")
	if len(m.entities.items) != 3 { // order-events/billing, order-events/shipping, orders
		t.Fatalf("entities filter: %d rows", len(m.entities.items))
	}
	m = keysIn(t, m, "esc")
	if m.stack.Top() != CtxEntities || m.entities.filter != "" || len(m.entities.items) != 4 {
		t.Fatalf("esc while typing: top %v, filter %q, %d rows", m.stack.Top(), m.entities.filter, len(m.entities.items))
	}
	// The DLQ count is visible, so it matches too.
	m = keysIn(t, m, "/", "d", "l", "q", " ", "1", "2", "enter")
	if len(m.entities.items) != 1 || m.entities.items[0].Path != "order-events/billing" {
		t.Fatalf("filter on the count: %+v", m.entities.items)
	}

	m = keysIn(t, m, "1", "/", "t", "e", "s", "t", "enter")
	if r, ok := m.namespaces.selected(); len(m.namespaces.items) != 1 || !ok || r.ns.Name != "sb-test-weu" {
		t.Fatalf("namespaces filter: %+v", m.namespaces.items)
	}
	m = keysIn(t, m, "/", "x")
	if len(m.namespaces.items) != 0 || !strings.Contains(screenText(m), "no match for /testx") {
		t.Fatalf("no match:\n%s", screenText(m))
	}
}

func TestFilterOutsideSidePanelRefused(t *testing.T) {
	m := keysIn(t, loaded(t), "0", "/")
	if m.stack.Top() != CtxMain || !strings.Contains(m.status.text, "focus 1, 2 or 3") {
		t.Fatalf("top %v, status %q", m.stack.Top(), m.status.text)
	}
}

func TestFilterClearsWhenTheListChanges(t *testing.T) {
	m := keysIn(t, loaded(t), "3", "/", "m", "a", "x", "enter")
	m = keysIn(t, m, "tab")
	if m.messages.filter != "" {
		t.Fatalf("Active tab kept the DLQ filter %q", m.messages.filter)
	}
	m = keysIn(t, m, "/", "o", "enter", "2", "j", "enter")
	if m.messages.filter != "" {
		t.Fatalf("another entity kept the filter %q", m.messages.filter)
	}
}

func TestRepairWithFilterRemovesTheVisibleRow(t *testing.T) {
	be := fake.New()
	m := onOrders(t, be)
	// DLQ messages cycle five reasons: filter one of them.
	reason := m.messages.items[1].DeadLetterReason
	m = keysIn(t, m, "/")
	for _, r := range reason {
		m = keysIn(t, m, string(r))
	}
	m = keysIn(t, m, "enter")
	n := len(m.messages.items)
	if n < 2 {
		t.Fatalf("setup: %d rows match %q", n, reason)
	}
	second := m.messages.items[1].SequenceNumber
	m = keysIn(t, m, "r", "y")
	if len(m.messages.items) != n-1 || selectedSeq(t, m) != second || len(m.messages.all) != 36 {
		t.Fatalf("after r y: %d visible (want %d), %d loaded, seq %d (want %d)", len(m.messages.items), n-1, len(m.messages.all), selectedSeq(t, m), second)
	}
}

func TestSortByDLQ(t *testing.T) {
	m := keysIn(t, loaded(t), "2", "s")
	var got []string
	for _, e := range m.entities.items {
		got = append(got, e.Path)
	}
	if strings.Join(got, ",") != "orders,order-events/billing,invoices,order-events/shipping" {
		t.Fatalf("by DLQ: %v", got)
	}
	if e, _ := m.entities.selected(); e.Path != "invoices" {
		t.Fatalf("cursor moved to %s", e.Path)
	}
	if !strings.Contains(screenText(m), "2 Entities by DLQ") {
		t.Fatalf("title lacks the sort:\n%s", screenText(m))
	}
	m = keysIn(t, m, "s")
	if m.entities.items[0].Path != "invoices" {
		t.Fatalf("by path: first %s", m.entities.items[0].Path)
	}

	ents := sortEntities([]bus.Entity{
		{Path: "a"}, {Path: "b", CountsKnown: true, DeadLetterCount: 1}, {Path: "c", CountsKnown: true, DeadLetterCount: 9},
	}, true)
	if ents[0].Path != "c" || ents[1].Path != "b" || ents[2].Path != "a" {
		t.Fatalf("unknown counts not last: %+v", ents)
	}
}

func TestJump(t *testing.T) {
	m := keysIn(t, loaded(t), ":", "b", "i", "l")
	if m.stack.Top() != CtxJump || !strings.Contains(screenText(m), "Jump to entity in sb-prod-weu") {
		t.Fatalf("no jump popup:\n%s", screenText(m))
	}
	m = keysIn(t, m, "enter")
	if m.stack.Top() != CtxMessages || m.openEntity == nil || m.openEntity.Path != "order-events/billing" || m.subQueue != bus.DeadLetter {
		t.Fatalf("enter: top %v, open %+v", m.stack.Top(), m.openEntity)
	}
	if e, _ := m.entities.selected(); e.Path != "order-events/billing" || len(m.messages.items) != 12 {
		t.Fatalf("Entities cursor on %s, %d messages", e.Path, len(m.messages.items))
	}

	// A jump past the Entities filter drops it.
	m = keysIn(t, m, "2", "/", "i", "n", "v", "enter", ":", "o", "r", "d", "e", "r", "s", "enter")
	if m.entities.filter != "" || m.openEntity.Path != "orders" {
		t.Fatalf("filter %q, open %s", m.entities.filter, m.openEntity.Path)
	}
	m = keysIn(t, m, ":", "z", "z", "enter")
	if m.stack.Top() != CtxJump {
		t.Fatal("enter without a match closed the popup")
	}
	m = keysIn(t, m, "esc")
	if m.stack.Top() != CtxMessages {
		t.Fatalf("esc: top %v", m.stack.Top())
	}
}

func TestJumpRanking(t *testing.T) {
	m := loaded(t)
	m.jump.query = "ord"
	var got []string
	for _, e := range m.jumpMatches() {
		got = append(got, e.Path)
	}
	// Same substring score at a segment start: the shorter path first.
	if strings.Join(got, ",") != "orders,order-events/billing,order-events/shipping" {
		t.Fatalf("ord: %v", got)
	}
	if _, ok := fuzzyScore("oeb", "order-events/billing"); !ok {
		t.Fatal("in-order subsequence did not match")
	}
	if _, ok := fuzzyScore("bo", "order-events/billing"); ok {
		t.Fatal("out-of-order runes matched")
	}
	a, _ := fuzzyScore("ship", "order-events/shipping")
	b, _ := fuzzyScore("ship", "s-h-i-p")
	if a <= b {
		t.Fatalf("substring %d <= scattered %d", a, b)
	}
}

func TestJumpNeedsANamespace(t *testing.T) {
	be := &noConfigured{fake.New(fake.WithDiscovery())}
	m := keysIn(t, startWith(t, be), ":")
	if m.stack.Top() == CtxJump || !strings.Contains(m.status.text, "open one first") {
		t.Fatalf("top %v, status %q", m.stack.Top(), m.status.text)
	}
}

func TestRefresh(t *testing.T) {
	be := fake.New()
	m := keysIn(t, startWith(t, be), "3", "j", "j")
	removeOutOfBand(t, be, m, 2)
	m = keysIn(t, m, "R")
	// The cursor keeps its index: the list is one shorter, so it is clamped.
	if len(m.messages.items) != 2 || m.messages.cursor != 1 || selectedSeq(t, m) != 4 {
		t.Fatalf("R on Messages: %d rows, cursor %d", len(m.messages.items), m.messages.cursor)
	}

	m = keysIn(t, m, "2", "R")
	if e, _ := m.entities.selected(); e.Path != "invoices" || e.DeadLetterCount != 2 {
		t.Fatalf("R on Entities: %+v", e)
	}
	if !hasLog(m, "list entities sb-prod-weu → 4") {
		t.Fatalf("log: %+v", m.log)
	}

	m = keysIn(t, m, "1", "j", "R")
	if r, _ := m.namespaces.selected(); r.ns.Name != "sb-test-weu" || m.namespaces.loading {
		t.Fatalf("R on Namespaces: %+v loading %v", r, m.namespaces.loading)
	}
}

// clipRecorder records what a model copies through tmux.
type clipRecorder struct{ texts []string }

func (r *clipRecorder) clipboard() clipboard {
	return clipboard{
		getenv:     func(k string) string { return map[string]string{"TMUX": "/tmp/tmux-1/default,1,0"}[k] },
		loadBuffer: func(_ context.Context, text string) error { r.texts = append(r.texts, text); return nil },
	}
}

func TestCopy(t *testing.T) {
	rec := &clipRecorder{}
	m := loaded(t)
	m.clip = rec.clipboard()
	m = keysIn(t, m, "3", "Y")
	if m.status.level != statusOK || m.status.text != "copied MessageId inv-1002" {
		t.Fatalf("Y: status %+v", m.status)
	}
	m = keysIn(t, m, "y")
	if !strings.HasPrefix(m.status.text, "copied body of invoices/$DLQ seq 2 (") {
		t.Fatalf("y on Body: status %q", m.status.text)
	}
	m = keysIn(t, m, "]", "0", "j", "y") // Properties, second row
	if m.status.text != "copied orderId = 1002" {
		t.Fatalf("y on Properties: status %q", m.status.text)
	}
	m = keysIn(t, m, "]", "y") // System: the cursor is back on the first field
	if m.status.text != "copied MessageId inv-1002" {
		t.Fatalf("y on System: status %q", m.status.text)
	}
	m = keysIn(t, m, "1", "y")
	if m.status.text != "copied FQDN sb-prod-weu.servicebus.windows.net" {
		t.Fatalf("y on Namespaces: status %q", m.status.text)
	}
	want := []string{"inv-1002", "", "1002", "inv-1002", "sb-prod-weu.servicebus.windows.net"}
	if len(rec.texts) != len(want) {
		t.Fatalf("copied %q", rec.texts)
	}
	for i, w := range want {
		if w != "" && rec.texts[i] != w {
			t.Fatalf("copy %d = %q, want %q", i, rec.texts[i], w)
		}
	}
	if !strings.Contains(rec.texts[1], `"orderId":1002`) { // the raw bytes, not the pretty view
		t.Fatalf("body copy = %q", rec.texts[1])
	}
}

func TestClipboardBytes(t *testing.T) {
	if got := osc52("hi"); got != "\x1b]52;c;aGk=\a" {
		t.Fatalf("osc52 = %q", got)
	}
	if got := tmuxPassthrough(osc52("hi")); got != "\x1bPtmux;\x1b\x1b]52;c;aGk=\a\x1b\\" {
		t.Fatalf("passthrough = %q", got)
	}

	plain := clipboard{getenv: func(string) string { return "" }}
	if msg, ok := plain.copy("hi")().(tea.RawMsg); !ok || msg.Msg != "\x1b]52;c;aGk=\a" {
		t.Fatalf("outside tmux: %#v", msg)
	}

	inTmux := func(k string) string {
		if k == "TMUX" {
			return "/tmp/tmux-1/default,1,0"
		}
		return ""
	}
	var loaded string
	ok := clipboard{getenv: inTmux, loadBuffer: func(_ context.Context, s string) error { loaded = s; return nil }}
	if msg := ok.copy("hi")(); msg != nil || loaded != "hi" {
		t.Fatalf("tmux load-buffer: msg %#v, loaded %q", msg, loaded)
	}
	failing := clipboard{getenv: inTmux, loadBuffer: func(context.Context, string) error { return errors.New("no tmux") }}
	if msg, ok := failing.copy("hi")().(tea.RawMsg); !ok || msg.Msg != "\x1bPtmux;\x1b\x1b]52;c;aGk=\a\x1b\\" {
		t.Fatalf("tmux fallback: %#v", msg)
	}
}

func TestActiveTab(t *testing.T) {
	m := keysIn(t, loaded(t), "3", "tab")
	if m.subQueue != bus.Active || len(m.messages.items) != 12 {
		t.Fatalf("Active: subqueue %v, %d rows", m.subQueue, len(m.messages.items))
	}
	if !strings.Contains(screenText(m), "OrderPlaced") {
		t.Fatalf("Active rows lack the subject:\n%s", screenText(m))
	}
	m = keysIn(t, m, "r")
	if m.stack.Top() == CtxConfirm {
		t.Fatal("r on the Active tab opened the confirm popup")
	}
}

func TestActiveTabSessionful(t *testing.T) {
	m := keysIn(t, startWith(t, fake.New(fake.WithSessions("sb-prod-weu", "invoices"))), "3", "tab")
	s := screenText(m)
	if !strings.Contains(s, "need a session lock (not in v0.1). The DLQ tab") || !strings.Contains(s, "works: press tab.") || strings.Contains(s, "error:") {
		t.Fatalf("sessionful Active:\n%s", s)
	}
	if !hasLog(m, bus.SessionfulActive) {
		t.Fatalf("log: %+v", m.log)
	}
	for _, e := range m.log {
		if e.err {
			t.Fatalf("sessionful logged as an error: %+v", e)
		}
	}
}

// noConfigured has discovery only: no namespace given on the command line.
type noConfigured struct{ *fake.Backend }

func (noConfigured) Namespaces(context.Context) ([]bus.Namespace, error) { return nil, nil }

// dupDiscovery also discovers the configured sb-prod-weu.
type dupDiscovery struct{ *fake.Backend }

func (d dupDiscovery) SubscriptionNamespaces(ctx context.Context, sub bus.Subscription) ([]bus.Namespace, error) {
	nss, err := d.Backend.SubscriptionNamespaces(ctx, sub)
	if sub.Name == "contoso-dev" {
		nss = append(nss, bus.Namespace{Name: "sb-prod-weu", FQDN: "sb-prod-weu.servicebus.windows.net", SubscriptionID: sub.ID, Auth: "Entra ID (az login)"})
	}
	return nss, err
}

// partialTopics fails listing topics: the queues still arrive.
type partialTopics struct{ *fake.Backend }

func (p partialTopics) ListEntities(ctx context.Context, ns bus.Namespace) ([]bus.Entity, error) {
	ents, err := p.Backend.ListEntities(ctx, ns)
	if err != nil {
		return nil, err
	}
	var queues []bus.Entity
	for _, e := range ents {
		if e.Kind == bus.KindQueue {
			queues = append(queues, e)
		}
	}
	return queues, &bus.PartialError{Err: &bus.Error{Kind: bus.ErrUnknown, Op: "list topics " + ns.Name, Msg: "SubCode=40000. Basic tier has no topics"}}
}

func nsNames(m Model) []string {
	var out []string
	for _, r := range m.namespaces.items {
		switch r.kind {
		case rowNamespace:
			out = append(out, r.ns.Name)
		case rowSubLoading:
			out = append(out, "loading "+r.sub.Name)
		case rowSubFailed:
			out = append(out, "failed "+r.sub.Name)
		}
	}
	return out
}

func TestDiscovery(t *testing.T) {
	be := fake.New(fake.WithDiscovery(), fake.WithDiscoveryError("contoso-broken", errors.New("AuthorizationFailed")))
	m := New(be, testOptions())
	m = run(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
	m, _ = initHeld(t, m)
	// Configured namespaces and subscriptions are in; the per-subscription
	// listings are not.
	if got := strings.Join(nsNames(m), ","); got != "sb-prod-weu,sb-test-weu,loading contoso-broken,loading contoso-dev,loading contoso-sandbox" {
		t.Fatalf("while listing: %s", got)
	}
	m = keysIn(t, m, "1", "j", "j", "enter")
	if !strings.Contains(m.status.text, "still listing the namespaces of contoso-broken") {
		t.Fatalf("enter on a loading row: %q", m.status.text)
	}

	m = startWith(t, be)
	if got := strings.Join(nsNames(m), ","); got != "sb-prod-weu,sb-test-weu,failed contoso-broken,sb-dev-neu" {
		t.Fatalf("after listing: %s", got)
	}
	if !hasLog(m, "discover namespaces contoso-broken") || !hasLog(m, "discover contoso-dev → 1 namespace") {
		t.Fatalf("log: %+v", m.log)
	}
	// The first configured namespace opens; discovered ones never do.
	if m.openNS == nil || m.openNS.Name != "sb-prod-weu" {
		t.Fatalf("open namespace %+v", m.openNS)
	}
	m = keysIn(t, m, "1", "G")
	if s := screenText(m); !strings.Contains(s, "Resource group   rg-messaging-dev") || !strings.Contains(s, "Subscription     contoso-dev") {
		t.Fatalf("namespace details:\n%s", s)
	}
	m = keysIn(t, m, "enter")
	if m.openNS.Name != "sb-dev-neu" || len(m.entities.items) != 3 {
		t.Fatalf("enter on sb-dev-neu: open %s, %d entities", m.openNS.Name, len(m.entities.items))
	}

	m = startWith(t, &noConfigured{fake.New(fake.WithDiscovery())})
	if m.openNS != nil || strings.Join(nsNames(m), ",") != "sb-dev-neu" {
		t.Fatalf("discovery only: open %+v, rows %v", m.openNS, nsNames(m))
	}

	m = startWith(t, dupDiscovery{fake.New(fake.WithDiscovery())})
	if got := strings.Join(nsNames(m), ","); got != "sb-prod-weu,sb-test-weu,sb-dev-neu" {
		t.Fatalf("dedup: %s", got)
	}
	if r := m.namespaces.items[0]; r.ns.Auth != "demo data" {
		t.Fatalf("configured entry replaced: %+v", r.ns)
	}
}

func TestDiscoverySubscriptionsError(t *testing.T) {
	be := fake.New()
	be.SetFault(fake.OpDiscover, fake.Fault{Err: errors.New("az login required")})
	m := startWith(t, be)
	if m.openNS == nil || len(m.namespaces.items) != 2 || !hasLog(m, "discover subscriptions") {
		t.Fatalf("open %+v, rows %v, log %+v", m.openNS, nsNames(m), m.log)
	}
	// It stays in the log: an empty DLQ still says so in the main pane.
	m = keysIn(t, m, "2", "j", "j", "enter") // order-events/shipping: DLQ 0
	if s := screenText(m); m.namespaces.err != nil || strings.Contains(s, "error:") || !strings.Contains(s, "no message selected") {
		t.Fatalf("namespaces err %v:\n%s", m.namespaces.err, s)
	}
}

func TestPartialEntities(t *testing.T) {
	m := startWith(t, partialTopics{fake.New()})
	if len(m.entities.items) != 2 || m.entities.err != nil || !hasLog(m, "Basic tier has no topics") {
		t.Fatalf("%d entities, err %v, log %+v", len(m.entities.items), m.entities.err, m.log)
	}
}

func TestFilterTakesNavigationLetters(t *testing.T) {
	m := keysIn(t, loaded(t), "3", "/", "j", "k", "g", "G", "q")
	if m.stack.Top() != CtxFilter || m.messages.filter != "jkgGq" {
		t.Fatalf("top %v, filter %q", m.stack.Top(), m.messages.filter)
	}
}

func TestRefreshCancelsThePreviousDiscovery(t *testing.T) {
	m := keysIn(t, startWith(t, fake.New(fake.WithDiscovery())), "1")
	old := m.disc.round
	next, _ := m.Update(press("R")) // the new round's calls are not run
	m = next.(Model)
	if old.Err() == nil {
		t.Fatal("R left the previous round's calls running")
	}
	next, _ = m.Update(subscriptionsLoadedMsg{gen: m.disc.gen - 1, err: errors.New("stale round")})
	if m = next.(Model); m.disc.subsErr != nil || hasLog(m, "stale round") {
		t.Fatalf("a response of the previous round was applied: %v", m.disc.subsErr)
	}
}

func TestDiscoveryBoundWaitsOutsideTheDeadline(t *testing.T) {
	m := New(fake.New(fake.WithDiscovery()), testOptions())
	for range maxParallelDiscovery {
		m.discSem <- struct{}{}
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- m.discoverSub(bus.Subscription{ID: "x", Name: "x"})() }()
	select {
	case <-done:
		t.Fatal("a listing ran past the bound")
	case <-time.After(20 * time.Millisecond):
	}
	m.disc.cancel()
	if msg := (<-done).(subNamespacesLoadedMsg); !errors.Is(msg.err, context.Canceled) {
		t.Fatalf("waiting listing after cancel: %v", msg.err)
	}
}

func TestFailedRefreshKeepsDiscoveredNamespaces(t *testing.T) {
	be := fake.New(fake.WithDiscovery())
	m := startWith(t, be)
	be.SetFault(fake.OpDiscover, fake.Fault{Err: errors.New("az token expired")})
	m = keysIn(t, m, "1", "R")
	if got := strings.Join(nsNames(m), ","); got != "sb-prod-weu,sb-test-weu,sb-dev-neu" || m.namespaces.loading {
		t.Fatalf("after a failed refresh: %s (loading %v)", got, m.namespaces.loading)
	}
}

func TestFirstPageDropsPendingCursorMove(t *testing.T) {
	m := keysIn(t, loaded(t), "3")
	m.afterPage, m.hasAfterPage = 2, true
	if m = keysIn(t, m, "R"); m.hasAfterPage {
		t.Fatal("a refresh kept the cursor move meant for a next page")
	}
}

func TestRepairOfTheOnlyFilteredRowLoadsTheNextPage(t *testing.T) {
	m := onOrders(t, fake.New(fake.WithDeadLetters("sb-prod-weu", "orders", 120)))
	m = keysIn(t, m, "/", "o", "r", "d", "-", "4", "0", "0", "2", "enter")
	if len(m.messages.items) != 1 || len(m.messages.all) != 50 {
		t.Fatalf("setup: %d visible of %d", len(m.messages.items), len(m.messages.all))
	}
	m = keysIn(t, m, "r", "y")
	if len(m.messages.all) != 99 {
		t.Fatalf("after the repair: %d loaded, want 49 + the next 50", len(m.messages.all))
	}
}
