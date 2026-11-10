package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"
	"unicode/utf8"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus/admin"
)

// config names the namespace; see the environment variables in main.go.
type config struct {
	Namespace     string // short name, without .servicebus.windows.net
	Subscription  string
	ResourceGroup string
}

func (c config) fqdn() string { return c.Namespace + ".servicebus.windows.net" }

// ops talks to the namespace: the az CLI for the SKU (ARM), the SDK with
// the az CLI credential for entities and messages.
type ops struct {
	cfg   config
	admin *admin.Client
	sb    *azservicebus.Client
	log   io.Writer
}

func newOps(cfg config, log io.Writer) (*ops, error) {
	cred, err := azidentity.NewAzureCLICredential(&azidentity.AzureCLICredentialOptions{Subscription: cfg.Subscription})
	if err != nil {
		return nil, err
	}
	o := &ops{cfg: cfg, log: log}
	if o.admin, err = admin.NewClient(cfg.fqdn(), cred, nil); err != nil {
		return nil, err
	}
	if o.sb, err = azservicebus.NewClient(cfg.fqdn(), cred, nil); err != nil {
		return nil, err
	}
	return o, nil
}

func (o *ops) close() { _ = o.sb.Close(context.Background()) }

func (o *ops) logf(format string, args ...any) { fmt.Fprintf(o.log, format+"\n", args...) }

// az runs `az resource <verb>` on the namespace and returns its stdout.
func (o *ops) az(ctx context.Context, verb string, args ...string) ([]byte, error) {
	full := append([]string{
		"resource", verb,
		"--subscription", o.cfg.Subscription,
		"--resource-group", o.cfg.ResourceGroup,
		"--resource-type", "Microsoft.ServiceBus/namespaces",
		"--name", o.cfg.Namespace,
	}, args...)
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "az", full...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("az resource %s: %w: %s", verb, err, bytes.TrimSpace(stderr.Bytes()))
	}
	return out, nil
}

// armState is the namespace as ARM reports it.
type armState struct {
	SKU          string `json:"sku"`
	Provisioning string `json:"provisioning"`
	Status       string `json:"status"`
}

func (o *ops) arm(ctx context.Context) (armState, error) {
	out, err := o.az(ctx, "show", "--output", "json",
		"--query", "{sku:sku.name,provisioning:properties.provisioningState,status:properties.status}")
	if err != nil {
		return armState{}, err
	}
	var s armState
	if err := json.Unmarshal(out, &s); err != nil {
		return armState{}, fmt.Errorf("az resource show: %w", err)
	}
	return s, nil
}

// setSKU changes the tier and waits until both ARM (Succeeded, Active) and
// the data plane report it. `az servicebus namespace update --sku` refuses
// the change ("Tier 'Standard' is invalid"); the generic update works.
func (o *ops) setSKU(ctx context.Context, tier string) error {
	o.logf("changing the SKU to %s", tier)
	if _, err := o.az(ctx, "update", "--set", "sku.name="+tier, "sku.tier="+tier, "--output", "none"); err != nil {
		return err
	}
	for {
		s, err := o.arm(ctx)
		if err != nil {
			return err
		}
		props, err := o.admin.GetNamespaceProperties(ctx, nil)
		if err != nil {
			return fmt.Errorf("namespace properties: %w", err)
		}
		if s.SKU == tier && s.Provisioning == "Succeeded" && s.Status == "Active" && props.SKU == tier {
			o.logf("the namespace is %s", tier)
			return nil
		}
		o.logf("waiting: ARM %s/%s/%s, data plane %s", s.SKU, s.Provisioning, s.Status, props.SKU)
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for tier %s: %w", tier, ctx.Err())
		case <-time.After(10 * time.Second):
		}
	}
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

// state reads the tier and every queue, topic and subscription with its
// message counts. Basic has no topics, so they are not listed there.
func (o *ops) state(ctx context.Context) (namespaceState, armState, error) {
	a, err := o.arm(ctx)
	if err != nil {
		return namespaceState{}, a, err
	}
	st := namespaceState{Tier: a.SKU}
	if st.Queues, err = o.queues(ctx); err != nil {
		return st, a, err
	}
	if st.Tier != tierBasic {
		if st.Topics, err = o.topics(ctx); err != nil {
			return st, a, err
		}
	}
	return st, a, nil
}

func (o *ops) queues(ctx context.Context) ([]queue, error) {
	var out []queue
	index := map[string]int{}
	for pager := o.admin.NewListQueuesPager(nil); pager.More(); {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list queues: %w", err)
		}
		for _, q := range page.Queues {
			index[q.QueueName] = len(out)
			out = append(out, queue{
				Name:               q.QueueName,
				RequiresSession:    deref(q.RequiresSession),
				DuplicateDetection: deref(q.RequiresDuplicateDetection),
				ForwardTo:          deref(q.ForwardTo),
				ForwardDeadLetter:  deref(q.ForwardDeadLetteredMessagesTo),
				TTL:                deref(q.DefaultMessageTimeToLive),
			})
		}
	}
	for pager := o.admin.NewListQueuesRuntimePropertiesPager(nil); pager.More(); {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list queue counts: %w", err)
		}
		for _, r := range page.QueueRuntimeProperties {
			if i, ok := index[r.QueueName]; ok {
				out[i].Active = int64(r.ActiveMessageCount) + int64(r.ScheduledMessageCount)
				out[i].DeadLetter = int64(r.DeadLetterMessageCount)
			}
		}
	}
	return out, nil
}

func (o *ops) topics(ctx context.Context) ([]topic, error) {
	var out []topic
	for pager := o.admin.NewListTopicsPager(nil); pager.More(); {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list topics: %w", err)
		}
		for _, t := range page.Topics {
			out = append(out, topic{Name: t.TopicName})
		}
	}
	for i := range out {
		subs, err := o.subscriptions(ctx, out[i].Name)
		if err != nil {
			return nil, err
		}
		out[i].Subscriptions = subs
	}
	return out, nil
}

func (o *ops) subscriptions(ctx context.Context, topicName string) ([]subscription, error) {
	var out []subscription
	index := map[string]int{}
	for pager := o.admin.NewListSubscriptionsPager(topicName, nil); pager.More(); {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list subscriptions of %s: %w", topicName, err)
		}
		for _, s := range page.Subscriptions {
			index[s.SubscriptionName] = len(out)
			out = append(out, subscription{Name: s.SubscriptionName, RequiresSession: deref(s.RequiresSession)})
		}
	}
	for pager := o.admin.NewListSubscriptionsRuntimePropertiesPager(topicName, nil); pager.More(); {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list subscription counts of %s: %w", topicName, err)
		}
		for _, r := range page.SubscriptionRuntimeProperties {
			if i, ok := index[r.SubscriptionName]; ok {
				out[i].Active = int64(r.ActiveMessageCount)
				out[i].DeadLetter = int64(r.DeadLetterMessageCount)
			}
		}
	}
	return out, nil
}

// --- backup -----------------------------------------------------------

type peekFunc func(context.Context, int, *azservicebus.PeekMessagesOptions) ([]*azservicebus.ReceivedMessage, error)

func (o *ops) receiver(ref entityRef, opts *azservicebus.ReceiverOptions) (*azservicebus.Receiver, error) {
	if ref.Queue != "" {
		return o.sb.NewReceiverForQueue(ref.Queue, opts)
	}
	return o.sb.NewReceiverForSubscription(ref.Topic, ref.Subscription, opts)
}

// backup peeks every message of the sources, active and dead-lettered. It
// fails when it cannot account for every message the entity reports, so a
// partial backup never leads to a delete.
func (o *ops) backup(ctx context.Context, sources []source) ([]Message, error) {
	var all []Message
	for _, s := range sources {
		var active []Message
		var err error
		if s.RequiresSession {
			active, err = o.peekSessions(ctx, s)
		} else {
			active, err = o.peekPlain(ctx, s.entityRef, s.path(), nil)
		}
		if err != nil {
			return nil, err
		}
		// The dead-letter queue of a session entity has no sessions.
		dead, err := o.peekPlain(ctx, s.entityRef, s.path()+dlqSuffix,
			&azservicebus.ReceiverOptions{SubQueue: azservicebus.SubQueueDeadLetter})
		if err != nil {
			return nil, err
		}
		if int64(len(active)) != s.Active || int64(len(dead)) != s.DeadLetter {
			return nil, fmt.Errorf("backup %s: peeked %d active and %d dead-lettered, the entity reports %d and %d",
				s.path(), len(active), len(dead), s.Active, s.DeadLetter)
		}
		o.logf("backed up %s: %d active, %d dead-lettered", s.path(), len(active), len(dead))
		all = append(all, label(active, s.path())...)
		all = append(all, label(dead, s.path()+dlqSuffix)...)
	}
	return all, nil
}

func (o *ops) peekPlain(ctx context.Context, ref entityRef, label string, opts *azservicebus.ReceiverOptions) ([]Message, error) {
	r, err := o.receiver(ref, opts)
	if err != nil {
		return nil, fmt.Errorf("receiver %s: %w", label, err)
	}
	defer r.Close(context.Background())
	return peekAll(ctx, r.PeekMessages, label)
}

// peekSessions peeks the active side of a session entity, which only a
// session receiver can do: it accepts session after session, holding each
// one so the next accept yields another, until it has every message the
// entity reports or no session is left.
func (o *ops) peekSessions(ctx context.Context, s source) ([]Message, error) {
	var out []Message
	var held []*azservicebus.SessionReceiver
	defer func() {
		for _, r := range held {
			_ = r.Close(context.Background())
		}
	}()
	for int64(len(out)) < s.Active {
		var r *azservicebus.SessionReceiver
		var err error
		if s.Queue != "" {
			r, err = o.sb.AcceptNextSessionForQueue(ctx, s.Queue, nil)
		} else {
			r, err = o.sb.AcceptNextSessionForSubscription(ctx, s.Topic, s.Subscription, nil)
		}
		var sbErr *azservicebus.Error
		if errors.As(err, &sbErr) && sbErr.Code == azservicebus.CodeTimeout {
			break // no session left; backup compares the count
		} else if err != nil {
			return nil, fmt.Errorf("accept next session of %s: %w", s.path(), err)
		}
		held = append(held, r)
		msgs, err := peekAll(ctx, r.PeekMessages, s.path()+" session "+r.SessionID())
		if err != nil {
			return nil, err
		}
		out = append(out, msgs...)
	}
	return out, nil
}

func peekAll(ctx context.Context, peek peekFunc, label string) ([]Message, error) {
	var out []Message
	for {
		got, err := peek(ctx, 100, nil)
		if err != nil {
			return nil, fmt.Errorf("peek %s: %w", label, err)
		}
		if len(got) == 0 {
			return out, nil
		}
		for _, rm := range got {
			if !utf8.Valid(rm.Body) {
				return nil, fmt.Errorf("peek %s: the body of message %q is not UTF-8 text; the backup format cannot hold it", label, rm.MessageID)
			}
			out = append(out, Message{
				MessageID:             rm.MessageID,
				ContentType:           deref(rm.ContentType),
				Subject:               deref(rm.Subject),
				SessionID:             deref(rm.SessionID),
				DeadLetterReason:      deref(rm.DeadLetterReason),
				DeadLetterDescription: deref(rm.DeadLetterErrorDescription),
				Seq:                   deref(rm.SequenceNumber),
				Body:                  string(rm.Body),
				Props:                 rm.ApplicationProperties,
			})
		}
	}
}

// label sets Entity on msgs (peekAll leaves it empty).
func label(msgs []Message, entity string) []Message {
	for i := range msgs {
		msgs[i].Entity = entity
	}
	return msgs
}

// --- entities ---------------------------------------------------------

func (o *ops) createDemoEntities(ctx context.Context, p upPlan) error {
	if p.CreateTopic {
		o.logf("creating topic %s", demoTopic)
		if _, err := o.admin.CreateTopic(ctx, demoTopic, nil); err != nil {
			return fmt.Errorf("create topic %s: %w", demoTopic, err)
		}
	}
	if p.CreateSubscription {
		o.logf("creating subscription %s/%s", demoTopic, demoSubscription)
		if _, err := o.admin.CreateSubscription(ctx, demoTopic, demoSubscription, nil); err != nil {
			return fmt.Errorf("create subscription %s/%s: %w", demoTopic, demoSubscription, err)
		}
	}
	if p.CreateSessionQueue {
		o.logf("creating queue %s (RequiresSession)", demoSessionQueue)
		yes := true
		_, err := o.admin.CreateQueue(ctx, demoSessionQueue, &admin.CreateQueueOptions{
			Properties: &admin.QueueProperties{RequiresSession: &yes},
		})
		if err != nil {
			return fmt.Errorf("create queue %s: %w", demoSessionQueue, err)
		}
	}
	return nil
}

func (o *ops) deleteEntities(ctx context.Context, p downPlan) error {
	for _, t := range p.DeleteTopics {
		o.logf("deleting topic %s", t)
		if _, err := o.admin.DeleteTopic(ctx, t, nil); err != nil {
			return fmt.Errorf("delete topic %s: %w", t, err)
		}
	}
	for _, q := range p.DeleteQueues {
		o.logf("deleting queue %s", q)
		if _, err := o.admin.DeleteQueue(ctx, q, nil); err != nil {
			return fmt.Errorf("delete queue %s: %w", q, err)
		}
	}
	return nil
}

func (o *ops) capTTL(ctx context.Context, queues []string) error {
	for _, name := range queues {
		o.logf("setting DefaultMessageTimeToLive=%s on %s", basicMaxTTL, name)
		q, err := o.admin.GetQueue(ctx, name, nil)
		if err != nil {
			return fmt.Errorf("get queue %s: %w", name, err)
		}
		if q == nil {
			return fmt.Errorf("get queue %s: not found", name)
		}
		ttl := basicMaxTTL
		q.DefaultMessageTimeToLive = &ttl
		if _, err := o.admin.UpdateQueue(ctx, name, q.QueueProperties, nil); err != nil {
			return fmt.Errorf("update queue %s: %w", name, err)
		}
	}
	return nil
}

// --- restore ----------------------------------------------------------

// receiveTimeout bounds the wait for a message that was just sent.
const receiveTimeout = time.Minute

func outgoing(m Message) *azservicebus.Message {
	out := &azservicebus.Message{Body: []byte(m.Body), ApplicationProperties: restoreProps(m)}
	for _, f := range []struct {
		v   string
		dst **string
	}{{m.MessageID, &out.MessageID}, {m.ContentType, &out.ContentType}, {m.Subject, &out.Subject}, {m.SessionID, &out.SessionID}} {
		if f.v != "" {
			*f.dst = &f.v
		}
	}
	return out
}

// restore sends the backed-up messages of one entity. The dead-lettered
// ones go first, one at a time: send, receive, dead-letter with the
// original reason and description. The entity is empty when this starts,
// so the message received is the one just sent. The active ones follow.
func (o *ops) restore(ctx context.Context, t restoreTarget) error {
	dest := t.Queue
	if dest == "" {
		dest = t.Topic
	}
	sender, err := o.sb.NewSender(dest, nil)
	if err != nil {
		return fmt.Errorf("sender %s: %w", dest, err)
	}
	defer sender.Close(context.Background())
	for _, m := range t.DeadLetter {
		if err := sender.SendMessage(ctx, outgoing(m), nil); err != nil {
			return fmt.Errorf("send %s to %s: %w", m.MessageID, dest, err)
		}
		if err := o.deadLetter(ctx, t, m); err != nil {
			return err
		}
	}
	for _, m := range t.Active {
		if err := sender.SendMessage(ctx, outgoing(m), nil); err != nil {
			return fmt.Errorf("send %s to %s: %w", m.MessageID, dest, err)
		}
	}
	o.logf("restored %s: %d active, %d dead-lettered", t.path(), len(t.Active), len(t.DeadLetter))
	return nil
}

// settler is the part of Receiver and SessionReceiver deadLetter needs.
type settler interface {
	ReceiveMessages(context.Context, int, *azservicebus.ReceiveMessagesOptions) ([]*azservicebus.ReceivedMessage, error)
	DeadLetterMessage(context.Context, *azservicebus.ReceivedMessage, *azservicebus.DeadLetterOptions) error
	AbandonMessage(context.Context, *azservicebus.ReceivedMessage, *azservicebus.AbandonMessageOptions) error
	Close(context.Context) error
}

func (o *ops) deadLetter(ctx context.Context, t restoreTarget, m Message) error {
	ctx, cancel := context.WithTimeout(ctx, receiveTimeout)
	defer cancel()
	var r settler
	var err error
	if t.RequiresSession {
		r, err = o.sb.AcceptSessionForQueue(ctx, t.Queue, m.SessionID, nil)
	} else {
		r, err = o.receiver(t.entityRef, nil)
	}
	if err != nil {
		return fmt.Errorf("receiver %s: %w", t.path(), err)
	}
	defer r.Close(context.Background())
	got, err := r.ReceiveMessages(ctx, 1, nil)
	if err != nil {
		return fmt.Errorf("receive %s from %s: %w", m.MessageID, t.path(), err)
	}
	if len(got) != 1 || got[0].MessageID != m.MessageID {
		for _, rm := range got {
			_ = r.AbandonMessage(ctx, rm, nil)
		}
		return fmt.Errorf("receive from %s: expected message %q, got %d other message(s)", t.path(), m.MessageID, len(got))
	}
	reason, description := deadLetterInfo(m)
	opts := &azservicebus.DeadLetterOptions{Reason: &reason}
	if description != "" {
		opts.ErrorDescription = &description
	}
	if err := r.DeadLetterMessage(ctx, got[0], opts); err != nil {
		return fmt.Errorf("dead-letter %s in %s: %w", m.MessageID, t.path(), err)
	}
	return nil
}
