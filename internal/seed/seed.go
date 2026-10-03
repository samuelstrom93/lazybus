// Package seed fills the local Service Bus emulator with dead-lettered
// messages for manual testing and the emulator E2E tests. It refuses any
// connection string that is not the emulator's.
package seed

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus/admin"
	"github.com/Azure/go-amqp"

	"github.com/samuelstrom93/lazybus/internal/bus/azure"
)

// Entities the seeder writes to (see emulator/config.json).
const (
	Queue        = "orders"
	Topic        = "order-events"
	Subscription = "billing"
	// EmptyTopic is a topic without subscriptions, for the S2 guard. The
	// emulator's config can't hold one and runtime entities vanish on
	// restart, so every run creates it if it is missing.
	EmptyTopic = "empty-topic"
)

// Config for Run.
type Config struct {
	// ConnectionString of the emulator (UseDevelopmentEmulator=true).
	ConnectionString string
	// AdminPort is the emulator's admin (HTTP) port.
	AdminPort int
	// Reset drains the seeded DLQs first, so a rerun ends in the same
	// state. Active messages of the seeded entities are always drained.
	Reset bool
	// Log receives progress lines; nil discards them.
	Log io.Writer
}

// Message is one seeded message as it should read back from the DLQ.
type Message struct {
	MessageID     string
	Subject       string
	ContentType   string
	CorrelationID string
	Body          []byte
	Properties    map[string]any
	Reason        string
	Description   string
}

// QueueMessages are the messages seeded into orders/$DLQ.
func QueueMessages() []Message { return messages("orders", nil) }

// SubscriptionMessages are the messages seeded into
// order-events/billing/$DLQ. They carry eventType=OrderCreated, so the
// shipping subscription's rule does not match them.
func SubscriptionMessages() []Message {
	return messages("billing", map[string]any{"eventType": "OrderCreated"})
}

// CreatedAt is the DateTime property of every seeded message.
var CreatedAt = time.Date(2026, 9, 18, 22, 15, 0, 0, time.UTC)

// messages builds four messages: valid JSON, JSON with a long line, plain
// text, and invalid JSON, each with all seven property types.
func messages(prefix string, extra map[string]any) []Message {
	base := 1000
	if prefix == "billing" {
		base = 2000
	}
	shapes := []struct {
		subject, contentType, body, reason, description string
	}{
		{"OrderPlaced", "application/json",
			`{"orderId":%d,"status":"pending","customer":null,"items":[{"sku":"A-100","qty":2},{"sku":"B-220","qty":1}]}`,
			"SchemaMismatch", "Required property 'customer.id' is missing."},
		{"OrderPlaced", "application/json",
			`{"orderId":%d,"status":"failed","note":"` + strings.Repeat("Downstream warehouse API returned 409 Conflict for reservation; ", 4) + `"}`,
			"MaxDeliveryCountExceeded", "Message could not be consumed after 10 delivery attempts."},
		{"OrderImported", "text/plain",
			"ORDER;%d;SE;299.00\nLINE;A-100;2\nLINE;B-220;1\n",
			"DownstreamTimeout", "POST https://billing.internal/api/v2/invoices timed out after 30s."},
		{"OrderPlaced", "application/json",
			`{"orderId":%d,"status":"pending","items":[{"sku":"A-100",`,
			"SchemaMismatch", "Unexpected end of JSON input."},
	}
	out := make([]Message, 0, len(shapes))
	for i, s := range shapes {
		order := base + i + 1
		props := map[string]any{
			"tenant":    "contoso",
			"attempt":   int32(i + 1),
			"orderId":   int64(order),
			"amount":    float64(order%500) + 0.5,
			"isRetry":   i%2 == 1,
			"traceId":   amqp.UUID{0x6f, 0x1c, 0x2b, 0x9e, 0x4d, 0x2a, 0x4c, 0x1e, 0x9b, 0x7a, 0, 0, 0, 0, byte(order >> 8), byte(order)},
			"createdAt": CreatedAt,
		}
		for k, v := range extra {
			props[k] = v
		}
		out = append(out, Message{
			MessageID:     fmt.Sprintf("seed-%s-%d", prefix, order),
			Subject:       s.subject,
			ContentType:   s.contentType,
			CorrelationID: fmt.Sprintf("corr-%d", order),
			Body:          fmt.Appendf(nil, s.body, order),
			Properties:    props,
			Reason:        s.reason,
			Description:   s.description,
		})
	}
	return out
}

// target is a queue or a topic subscription.
type target struct {
	name        string // queue or topic
	sub         string // subscription; empty for a queue
	sendTo      string
	description string
}

func (t target) receiver(c *azservicebus.Client, opts *azservicebus.ReceiverOptions) (*azservicebus.Receiver, error) {
	if t.sub != "" {
		return c.NewReceiverForSubscription(t.name, t.sub, opts)
	}
	return c.NewReceiverForQueue(t.name, opts)
}

// Run seeds the emulator: it (re)creates EmptyTopic, drains the seeded
// entities, sends QueueMessages to orders and SubscriptionMessages to
// order-events, and dead-letters them from orders and billing with their
// reason and description.
func Run(ctx context.Context, cfg Config) error {
	logw := cfg.Log
	if logw == nil {
		logw = io.Discard
	}
	logf := func(format string, args ...any) { fmt.Fprintf(logw, format+"\n", args...) }
	if err := checkEmulator(cfg.ConnectionString); err != nil {
		return err
	}

	ac, err := azure.EmulatorAdminClient(cfg.ConnectionString, cfg.AdminPort)
	if err != nil {
		return fmt.Errorf("seed: admin client: %w", err)
	}
	var topic *admin.GetTopicResponse
	if err := azure.Safe("seed: get topic "+EmptyTopic, func() (err error) {
		topic, err = ac.GetTopic(ctx, EmptyTopic, nil)
		return err
	}); err != nil {
		return err
	}
	if topic == nil { // Get* returns (nil, nil) for a missing entity (S−1)
		if err := azure.Safe("seed: create topic "+EmptyTopic, func() error {
			_, err := ac.CreateTopic(ctx, EmptyTopic, nil)
			return err
		}); err != nil {
			return err
		}
		logf("created topic %s", EmptyTopic)
	} else {
		logf("topic %s exists", EmptyTopic)
	}

	client, err := azservicebus.NewClientFromConnectionString(cfg.ConnectionString, nil)
	if err != nil {
		return fmt.Errorf("seed: client: %w", err)
	}
	defer client.Close(context.Background())

	targets := []struct {
		t    target
		msgs []Message
	}{
		{target{name: Queue, sendTo: Queue, description: Queue}, QueueMessages()},
		{target{name: Topic, sub: Subscription, sendTo: Topic, description: Topic + "/" + Subscription}, SubscriptionMessages()},
	}
	for _, tg := range targets {
		n, err := drain(ctx, client, tg.t, false)
		if err != nil {
			return err
		}
		if n > 0 {
			logf("drained %d active from %s", n, tg.t.description)
		}
		if cfg.Reset {
			n, err := drain(ctx, client, tg.t, true)
			if err != nil {
				return err
			}
			logf("drained %d from %s/$DLQ", n, tg.t.description)
		}
		if err := send(ctx, client, tg.t.sendTo, tg.msgs); err != nil {
			return err
		}
		if err := deadLetter(ctx, client, tg.t, tg.msgs); err != nil {
			return err
		}
		logf("dead-lettered %d into %s/$DLQ", len(tg.msgs), tg.t.description)
	}
	return nil
}

// checkEmulator refuses anything but the local emulator: the connection
// string must say UseDevelopmentEmulator=true and point at a loopback host.
// The seeder drains queues; it must never reach a real namespace.
func checkEmulator(cs string) error {
	if !azure.IsEmulatorConnectionString(cs) {
		return errors.New("seed: refusing a connection string without UseDevelopmentEmulator=true (emulator only)")
	}
	host, err := azure.ConnectionStringHost(cs)
	if err != nil {
		return fmt.Errorf("seed: %w", err)
	}
	if ip := net.ParseIP(host); !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("seed: refusing host %q: only localhost or a loopback address (emulator only)", host)
	}
	return nil
}

func send(ctx context.Context, c *azservicebus.Client, entity string, msgs []Message) error {
	s, err := c.NewSender(entity, nil)
	if err != nil {
		return fmt.Errorf("seed: sender %s: %w", entity, err)
	}
	defer s.Close(context.Background())
	for _, m := range msgs {
		out := &azservicebus.Message{
			MessageID:             &m.MessageID,
			Subject:               &m.Subject,
			ContentType:           &m.ContentType,
			CorrelationID:         &m.CorrelationID,
			Body:                  m.Body,
			ApplicationProperties: m.Properties,
		}
		if err := s.SendMessage(ctx, out, nil); err != nil {
			return fmt.Errorf("seed: send %s to %s: %w", m.MessageID, entity, err)
		}
	}
	return nil
}

// deadLetter receives the sent messages in peek-lock and dead-letters each
// with its reason and description. Anything else that turns up is
// completed (the active side was drained, so that is leftover junk); no
// abandon loops, which make the emulator throttle (S−1).
func deadLetter(ctx context.Context, c *azservicebus.Client, t target, msgs []Message) error {
	r, err := t.receiver(c, nil)
	if err != nil {
		return fmt.Errorf("seed: receiver %s: %w", t.description, err)
	}
	defer r.Close(context.Background())
	want := map[string]Message{}
	for _, m := range msgs {
		want[m.MessageID] = m
	}
	for len(want) > 0 {
		got, err := r.ReceiveMessages(ctx, len(want), nil)
		if err != nil {
			return fmt.Errorf("seed: receive from %s (%d still missing): %w", t.description, len(want), err)
		}
		for _, rm := range got {
			m, ok := want[rm.MessageID]
			if !ok {
				if err := r.CompleteMessage(ctx, rm, nil); err != nil {
					return fmt.Errorf("seed: complete stray %s: %w", rm.MessageID, err)
				}
				continue
			}
			if err := r.DeadLetterMessage(ctx, rm, &azservicebus.DeadLetterOptions{
				Reason:           &m.Reason,
				ErrorDescription: &m.Description,
			}); err != nil {
				return fmt.Errorf("seed: dead-letter %s: %w", m.MessageID, err)
			}
			delete(want, rm.MessageID)
		}
	}
	return nil
}

// maxEmptyReceives is how many receives in a row may come back empty while
// a peek still sees messages before drain gives up, instead of looping
// until the overall deadline.
const maxEmptyReceives = 2

// drain deletes every message of t (or its DLQ) with receive-and-delete
// until a peek finds it empty.
func drain(ctx context.Context, c *azservicebus.Client, t target, dlq bool) (int, error) {
	opts := &azservicebus.ReceiverOptions{ReceiveMode: azservicebus.ReceiveModeReceiveAndDelete}
	name := t.description
	if dlq {
		opts.SubQueue = azservicebus.SubQueueDeadLetter
		name += "/$DLQ"
	}
	r, err := t.receiver(c, opts)
	if err != nil {
		return 0, fmt.Errorf("seed: drain receiver %s: %w", name, err)
	}
	defer r.Close(context.Background())
	n, empty := 0, 0
	for {
		from := int64(0)
		left, err := r.PeekMessages(ctx, 1, &azservicebus.PeekMessagesOptions{FromSequenceNumber: &from})
		if err != nil {
			return n, fmt.Errorf("seed: peek %s: %w", name, err)
		}
		if len(left) == 0 {
			return n, nil
		}
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		got, err := r.ReceiveMessages(rctx, 100, nil)
		cancel()
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			return n, fmt.Errorf("seed: drain %s: %w", name, err)
		}
		if err := ctx.Err(); err != nil {
			return n, fmt.Errorf("seed: drain %s: %w", name, err)
		}
		if len(got) == 0 {
			empty++
			if empty >= maxEmptyReceives {
				return n, fmt.Errorf("seed: drain %s: peek still sees messages but %d receives in a row got none", name, empty)
			}
			continue
		}
		empty = 0
		n += len(got)
	}
}
