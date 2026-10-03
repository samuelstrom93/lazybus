//go:build emulator

// Emulator E2E: needs the lazybus emulator from emulator/ running.
//
//	go test -tags emulator ./...
//
// Ports default to lazybus' compose (AMQP 5682, admin 5310); override with
// LAZYBUS_EMULATOR_AMQP_PORT and LAZYBUS_EMULATOR_ADMIN_PORT.
package azure_test

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/Azure/go-amqp"
	"github.com/charmbracelet/x/ansi"

	"github.com/samuelstrom93/lazybus/internal/bus"
	"github.com/samuelstrom93/lazybus/internal/bus/azure"
	"github.com/samuelstrom93/lazybus/internal/seed"
	"github.com/samuelstrom93/lazybus/internal/ui"
)

// opTimeout bounds each broker operation; the emulator can stall for
// seconds when it throttles (S−1).
const opTimeout = 30 * time.Second

func port(t *testing.T, env string, def int) int {
	t.Helper()
	v := os.Getenv(env)
	if v == "" {
		return def
	}
	p, err := strconv.Atoi(v)
	if err != nil {
		t.Fatalf("%s=%q: %v", env, v, err)
	}
	return p
}

func TestEmulatorSeedListPeek(t *testing.T) {
	amqpPort := port(t, "LAZYBUS_EMULATOR_AMQP_PORT", 5682)
	adminPort := port(t, "LAZYBUS_EMULATOR_ADMIN_PORT", 5310)
	cs := azure.EmulatorConnectionString("localhost", amqpPort)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := seed.Run(ctx, seed.Config{ConnectionString: cs, AdminPort: adminPort, Reset: true}); err != nil {
		t.Fatal(err)
	}

	b := azure.New()
	defer b.Close(context.Background())
	ns, err := b.AddConnectionString(cs, adminPort)
	if err != nil {
		t.Fatal(err)
	}

	// Entities: queues and subscriptions, counts unknown, topics hidden.
	lctx, lcancel := context.WithTimeout(context.Background(), opTimeout)
	ents, err := b.ListEntities(lctx, ns)
	lcancel()
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]bus.Entity{}
	for _, e := range ents {
		byPath[e.Path] = e
		if e.CountsKnown {
			t.Errorf("%s: counts known on the emulator", e.Path)
		}
		if strings.HasPrefix(e.Path, seed.EmptyTopic) {
			t.Errorf("topic listed as entity: %s", e.Path)
		}
	}
	queue, ok := byPath[seed.Queue]
	if !ok || queue.Kind != bus.KindQueue {
		t.Fatalf("queue %s missing: %+v", seed.Queue, ents)
	}
	sub, ok := byPath[seed.Topic+"/"+seed.Subscription]
	if !ok || sub.Kind != bus.KindSubscription {
		t.Fatalf("subscription missing: %+v", ents)
	}

	checkDLQ(t, b, ns, queue, seed.QueueMessages())
	checkDLQ(t, b, ns, sub, seed.SubscriptionMessages())

	// The UI shows the same: ? counts, typed properties, markers, body.
	checkUI(t, b, ents, queue)
}

// checkDLQ peeks ent's DLQ and compares it with the seeded messages.
func checkDLQ(t *testing.T, b *azure.Backend, ns bus.Namespace, ent bus.Entity, want []seed.Message) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	got, err := b.Peek(ctx, bus.PeekRequest{Namespace: ns, Entity: ent, SubQueue: bus.DeadLetter})
	if err != nil {
		t.Fatalf("peek %s: %v", ent.Path, err)
	}
	if len(got) != len(want) {
		t.Fatalf("%s/$DLQ: %d messages, want %d", ent.Path, len(got), len(want))
	}
	for i, w := range want {
		m := got[i] // peek order = send order
		if m.MessageID != w.MessageID || string(m.Body) != string(w.Body) || m.Subject != w.Subject ||
			m.ContentType != w.ContentType || m.CorrelationID != w.CorrelationID {
			t.Errorf("%s message %d = %s %q %q %q, want %s %q %q %q", ent.Path, i,
				m.MessageID, m.Subject, m.ContentType, m.Body, w.MessageID, w.Subject, w.ContentType, w.Body)
		}
		if m.DeadLetterReason != w.Reason || m.DeadLetterErrorDescription != w.Description {
			t.Errorf("%s: markers %q / %q, want %q / %q", w.MessageID, m.DeadLetterReason, m.DeadLetterErrorDescription, w.Reason, w.Description)
		}
		if m.SequenceNumber == 0 || m.EnqueuedTime.IsZero() {
			t.Errorf("%s: no sequence number or enqueued time", w.MessageID)
		}
		if len(m.Properties) != len(w.Properties) {
			t.Errorf("%s: %d properties, want %d (markers must not be among them): %+v", w.MessageID, len(m.Properties), len(w.Properties), m.Properties)
		}
		for _, p := range m.Properties {
			wv, ok := w.Properties[p.Key]
			if !ok {
				t.Errorf("%s: unexpected property %s", w.MessageID, p.Key)
				continue
			}
			wantType, wantVal := expect(wv)
			equal := p.Value == wantVal
			if tv, ok := p.Value.(time.Time); ok {
				equal = tv.Equal(wantVal.(time.Time))
			}
			if p.Type != wantType || !equal {
				t.Errorf("%s: %s = %v %v, want %v %v", w.MessageID, p.Key, p.Type, p.Value, wantType, wantVal)
			}
		}
	}
}

// expect is the bus type and value a seeded property value reads back as.
func expect(v any) (bus.PropertyType, any) {
	switch v := v.(type) {
	case string:
		return bus.TypeString, v
	case int32:
		return bus.TypeInt, v
	case int64:
		return bus.TypeLong, v
	case float64:
		return bus.TypeDouble, v
	case bool:
		return bus.TypeBool, v
	case amqp.UUID:
		return bus.TypeGUID, v.String()
	case time.Time:
		return bus.TypeDateTime, v
	}
	panic("unexpected seed property type")
}

func drive(t *testing.T, m tea.Model, msg tea.Msg) tea.Model {
	t.Helper()
	m, cmd := m.Update(msg)
	return runCmds(m, cmd)
}

// runCmds runs cmd and what follows inline; a batch runs depth first, in
// order (the broker call before the spinner tick).
func runCmds(m tea.Model, cmd tea.Cmd) tea.Model {
	for cmd != nil {
		msg := cmd()
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, c := range batch {
				m = runCmds(m, c)
			}
			return m
		}
		m, cmd = m.Update(msg)
	}
	return m
}

func key(s string) tea.KeyPressMsg {
	switch s {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "right":
		return tea.KeyPressMsg{Code: tea.KeyRight}
	}
	return tea.KeyPressMsg{Code: []rune(s)[0], Text: s}
}

func screen(m tea.Model) string { return ansi.Strip(m.View().Content) }

func checkUI(t *testing.T, b *azure.Backend, ents []bus.Entity, queue bus.Entity) {
	t.Helper()
	var m tea.Model = ui.New(b, ui.Options{Location: time.UTC, CallTimeout: opTimeout})
	m = drive(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
	m = runCmds(m, m.Init())
	if s := screen(m); !strings.Contains(s, "DLQ ?") || !strings.Contains(s, "emulator") {
		t.Fatalf("entity list without ? counts:\n%s", s)
	}

	m = drive(t, m, key("2"))
	for _, e := range ents {
		if e == queue {
			break
		}
		m = drive(t, m, key("j"))
	}
	m = drive(t, m, key("enter"))
	body := screen(m)
	// No row text: the Messages columns truncate by sequence-number width,
	// which grows with every emulator run. Row data is checked untruncated
	// in the main pane below.
	for _, want := range []string{"peek orders/$DLQ → 4", `"orderId": 1001,`} {
		if !strings.Contains(body, want) {
			t.Fatalf("Body tab lacks %q:\n%s", want, body)
		}
	}

	m = drive(t, m, key("]"))
	props := screen(m)
	for _, want := range []string{
		"traceId", "Guid", "6f1c2b9e-4d2a-4c1e-9b7a-0000000003e9",
		// The SDK returns DateTime values in time.Local (S−1); the UI
		// prints them in Options.Location (UTC here), with offset.
		"createdAt", "DateTime", "2026-09-18T22:15:00Z",
		"orderId", "Long", "1001", "attempt", "Int", "amount", "Double", "isRetry", "Bool", "tenant", "String",
		"Dead-letter markers  ✕ removed on resubmit", "DeadLetterReason", "SchemaMismatch", "Required property 'customer.id' is missing.",
	} {
		if !strings.Contains(props, want) {
			t.Fatalf("Properties tab lacks %q:\n%s", want, props)
		}
	}

	m = drive(t, m, key("]"))
	if sys := screen(m); !strings.Contains(sys, "MessageId         seed-orders-1001") {
		t.Fatalf("System tab lacks the first message's MessageId:\n%s", sys)
	}
	m = drive(t, m, key("["))

	m = drive(t, m, key("3"))
	m = drive(t, m, key("j"))
	m = drive(t, m, key("j"))
	m = drive(t, m, key("["))
	if raw := screen(m); !strings.Contains(raw, "ORDER;1003;SE;299.00") || !strings.Contains(raw, "LINE;B-220;1") {
		t.Fatalf("Body tab of the text/plain message:\n%s", raw)
	}
}
