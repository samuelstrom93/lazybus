// Package azure implements the bus interfaces on Azure Service Bus with the
// official Go SDK (azservicebus + its admin package). SDK types stay inside
// this package: callers see only internal/bus types, and every SDK error is
// mapped to a *bus.Error.
package azure

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus/admin"
	"golang.org/x/sync/errgroup"

	"github.com/samuelstrom93/lazybus/internal/bus"
)

// Default emulator ports (the emulator's own defaults; lazybus' compose in
// emulator/ maps them to 5682 and 5310).
const (
	DefaultEmulatorAMQPPort  = 5672
	DefaultEmulatorAdminPort = 5300
)

// emulatorKey is the emulator's fixed development SAS key.
const emulatorKey = "SAS_KEY_VALUE"

// EmulatorConnectionString returns the AMQP connection string of a local
// Service Bus emulator on host:port.
func EmulatorConnectionString(host string, port int) string {
	return fmt.Sprintf("Endpoint=sb://%s:%d;SharedAccessKeyName=RootManageSharedAccessKey;SharedAccessKey=%s;UseDevelopmentEmulator=true;",
		host, port, emulatorKey)
}

// Backend serves one or more namespaces. It implements bus.Backend; DLQ
// Repair runs bus.Service on the SDK (repair.go).
type Backend struct {
	mu    sync.Mutex
	conns []*conn
	svc   *bus.Service

	// ARM discovery (discovery.go); nil when off.
	discoveryCred azcore.TokenCredential
}

var _ bus.Backend = (*Backend)(nil)

// New returns a Backend with no namespaces; add them with
// AddConnectionString or AddNamespace.
func New() *Backend {
	b := &Backend{}
	b.svc = bus.NewService(driver{b}, nil, nil)
	return b
}

// conn is one namespace: a data-plane client, an admin client, the
// receivers opened for peeking, and the senders of DLQ Repair. A repair's
// peek-lock receiver lives only for its call.
type conn struct {
	ns       bus.Namespace
	client   *azservicebus.Client
	admin    *admin.Client
	emulator bool // never call runtime-properties APIs (S−1: they fail or panic)

	mu        sync.Mutex
	receivers map[receiverKey]*azservicebus.Receiver
	senders   map[string]*azservicebus.Sender
}

func newConn(ns bus.Namespace, emulator bool) *conn {
	return &conn{
		ns: ns, emulator: emulator,
		receivers: map[receiverKey]*azservicebus.Receiver{},
		senders:   map[string]*azservicebus.Sender{},
	}
}

type receiverKey struct {
	path string
	kind bus.EntityKind
	sub  bus.SubQueue
}

// AddConnectionString adds the namespace of a SAS connection string. A
// connection string with UseDevelopmentEmulator=true is the local emulator:
// its admin client talks plain HTTP to the same host on emulatorAdminPort
// (ignored otherwise), and runtime counts are never requested, so entities
// report unknown counts.
func (b *Backend) AddConnectionString(cs string, emulatorAdminPort int) (bus.Namespace, error) {
	cfg, err := parseConnectionString(cs)
	if err != nil {
		return bus.Namespace{}, err
	}
	c := newConn(bus.Namespace{}, cfg.emulator)
	if c.client, err = azservicebus.NewClientFromConnectionString(cs, nil); err != nil {
		return bus.Namespace{}, fmt.Errorf("connection string: %w", err)
	}
	if cfg.emulator {
		c.ns = bus.Namespace{Name: "emulator", FQDN: cfg.host, Auth: "emulator development key"}
		c.admin, err = EmulatorAdminClient(cs, emulatorAdminPort)
	} else {
		c.ns = bus.Namespace{Name: shortName(cfg.host), FQDN: cfg.host, Auth: "SAS connection string"}
		c.admin, err = admin.NewClientFromConnectionString(cs, nil)
	}
	if err != nil {
		return bus.Namespace{}, fmt.Errorf("connection string: %w", err)
	}
	return b.add(c), nil
}

// EmulatorAdminClient returns an admin client for the emulator behind the
// AMQP connection string cs: same host and key, port adminPort, plain HTTP.
// The SDK always builds https:// URLs and ignores UseDevelopmentEmulator for
// the admin client (S−1). Only for the emulator; tools/seed uses it too.
func EmulatorAdminClient(cs string, adminPort int) (*admin.Client, error) {
	cfg, err := parseConnectionString(cs)
	if err != nil {
		return nil, err
	}
	if !cfg.emulator {
		return nil, errors.New("connection string: not an emulator (UseDevelopmentEmulator=true missing)")
	}
	adminCS := strings.Replace(cs, "sb://"+cfg.host, "sb://"+cfg.hostname+":"+strconv.Itoa(adminPort), 1)
	return admin.NewClientFromConnectionString(adminCS, &admin.ClientOptions{
		ClientOptions: azcore.ClientOptions{Transport: httpOnly{http.DefaultTransport}},
	})
}

// IsEmulatorConnectionString reports whether cs parses and has
// UseDevelopmentEmulator=true.
func IsEmulatorConnectionString(cs string) bool {
	cfg, err := parseConnectionString(cs)
	return err == nil && cfg.emulator
}

// ConnectionStringHost returns the host name (without port) of cs's
// Endpoint.
func ConnectionStringHost(cs string) (string, error) {
	cfg, err := parseConnectionString(cs)
	return cfg.hostname, err
}

// AddNamespace adds a namespace by its fully qualified name, authenticated
// with cred (lazybus uses the az CLI credential). A bare name without dots
// gets ".servicebus.windows.net" appended.
func (b *Backend) AddNamespace(fqdn string, cred azcore.TokenCredential) (bus.Namespace, error) {
	fqdn = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(fqdn), "sb://"), "/")
	if fqdn == "" {
		return bus.Namespace{}, errors.New("namespace: empty name")
	}
	if !strings.Contains(fqdn, ".") {
		fqdn += ".servicebus.windows.net"
	}
	return b.addTokenNamespace(bus.Namespace{Name: shortName(fqdn), FQDN: fqdn, Auth: authEntra}, cred)
}

// addTokenNamespace adds ns (FQDN set) with clients authenticated by cred.
// Clients connect lazily, so this does no network I/O.
func (b *Backend) addTokenNamespace(ns bus.Namespace, cred azcore.TokenCredential) (bus.Namespace, error) {
	c := newConn(ns, false)
	var err error
	if c.client, err = azservicebus.NewClient(ns.FQDN, cred, nil); err != nil {
		return bus.Namespace{}, fmt.Errorf("namespace %s: %w", ns.FQDN, err)
	}
	if c.admin, err = admin.NewClient(ns.FQDN, cred, nil); err != nil {
		return bus.Namespace{}, fmt.Errorf("namespace %s: %w", ns.FQDN, err)
	}
	return b.add(c), nil
}

// add registers c unless a namespace with the same FQDN is already there.
func (b *Backend) add(c *conn) bus.Namespace {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, have := range b.conns {
		if have.ns.FQDN == c.ns.FQDN {
			_ = c.client.Close(context.Background())
			return have.ns
		}
	}
	b.conns = append(b.conns, c)
	return c.ns
}

// Close closes every receiver and client.
func (b *Backend) Close(ctx context.Context) error {
	b.mu.Lock()
	conns := b.conns
	b.conns = nil
	b.mu.Unlock()
	var errs []error
	for _, c := range conns {
		c.mu.Lock()
		for k, r := range c.receivers {
			errs = append(errs, r.Close(ctx))
			delete(c.receivers, k)
		}
		for k, s := range c.senders {
			errs = append(errs, s.Close(ctx))
			delete(c.senders, k)
		}
		c.mu.Unlock()
		errs = append(errs, c.client.Close(ctx))
	}
	return errors.Join(errs...)
}

func (b *Backend) conn(ns bus.Namespace) (*conn, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range b.conns {
		if c.ns.FQDN == ns.FQDN {
			return c, nil
		}
	}
	return nil, &bus.Error{Kind: bus.ErrNotFound, Op: "open namespace", Msg: fmt.Sprintf("namespace %q is not configured", ns.Name)}
}

// Namespaces implements bus.Discovery: the configured namespaces, not the
// ones discovery added.
func (b *Backend) Namespaces(ctx context.Context) ([]bus.Namespace, error) {
	if err := ctx.Err(); err != nil {
		return nil, mapErr("list namespaces", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]bus.Namespace, 0, len(b.conns))
	for _, c := range b.conns {
		if c.ns.SubscriptionID == "" {
			out = append(out, c.ns)
		}
	}
	return out, nil
}

// ListEntities implements bus.Entities: queues and topic subscriptions,
// sorted by path. Topics are not listed (they have no dead-letter queue).
// When the queues list but the topics or their subscriptions don't (a
// Basic-tier namespace has no topics), the queues come back with a
// *bus.PartialError.
func (b *Backend) ListEntities(ctx context.Context, ns bus.Namespace) ([]bus.Entity, error) {
	c, err := b.conn(ns)
	if err != nil {
		return nil, err
	}
	var out []bus.Entity
	err = Safe("list entities "+ns.Name, func() error {
		var err error
		out, err = c.listQueues(ctx)
		return err
	})
	if err != nil {
		return nil, err
	}
	var subs []bus.Entity
	var partial error
	err = Safe("list topics "+ns.Name, func() error {
		var err error
		subs, err = c.listSubscriptions(ctx)
		return err
	})
	switch {
	case bus.KindOf(err) == bus.ErrCanceled:
		return nil, err // superseded; nobody shows it
	case err != nil:
		partial = &bus.PartialError{Err: err}
	}
	out = append(out, subs...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Kind < out[j].Kind
	})
	return out, partial
}

// maxParallelTopics bounds the concurrent per-topic subscription listings.
const maxParallelTopics = 8

// listQueues lists the queues: through the runtime-properties pager, which
// carries the counts, except on the emulator, where runtime calls fail
// (S−1) and counts stay unknown.
func (c *conn) listQueues(ctx context.Context) ([]bus.Entity, error) {
	if c.emulator {
		var out []bus.Entity
		qp := c.admin.NewListQueuesPager(nil)
		for qp.More() {
			page, err := qp.NextPage(ctx)
			if err != nil {
				return nil, err
			}
			for _, q := range page.Queues {
				out = append(out, bus.Entity{Path: q.QueueName, Kind: bus.KindQueue})
			}
		}
		return out, nil
	}
	var out []bus.Entity
	qp := c.admin.NewListQueuesRuntimePropertiesPager(nil)
	for qp.More() {
		page, err := qp.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, q := range page.QueueRuntimeProperties {
			out = append(out, bus.Entity{
				Path: q.QueueName, Kind: bus.KindQueue, CountsKnown: true,
				ActiveCount: int64(q.ActiveMessageCount), DeadLetterCount: int64(q.DeadLetterMessageCount),
			})
		}
	}
	return out, nil
}

// listSubscriptions lists the subscriptions of every topic, with counts
// except on the emulator.
func (c *conn) listSubscriptions(ctx context.Context) ([]bus.Entity, error) {
	if c.emulator {
		return c.eachTopic(ctx, func(ctx context.Context, topic string) ([]bus.Entity, error) {
			var subs []bus.Entity
			sp := c.admin.NewListSubscriptionsPager(topic, nil)
			for sp.More() {
				page, err := sp.NextPage(ctx)
				if err != nil {
					return nil, err
				}
				for _, s := range page.Subscriptions {
					subs = append(subs, bus.Entity{Path: topic + "/" + s.SubscriptionName, Kind: bus.KindSubscription})
				}
			}
			return subs, nil
		})
	}
	return c.eachTopic(ctx, func(ctx context.Context, topic string) ([]bus.Entity, error) {
		var subs []bus.Entity
		sp := c.admin.NewListSubscriptionsRuntimePropertiesPager(topic, nil)
		for sp.More() {
			page, err := sp.NextPage(ctx)
			if err != nil {
				return nil, err
			}
			for _, s := range page.SubscriptionRuntimeProperties {
				subs = append(subs, bus.Entity{
					Path: topic + "/" + s.SubscriptionName, Kind: bus.KindSubscription, CountsKnown: true,
					ActiveCount: int64(s.ActiveMessageCount), DeadLetterCount: int64(s.DeadLetterMessageCount),
				})
			}
		}
		return subs, nil
	})
}

// eachTopic lists the topics and runs list for each, a few at a time.
func (c *conn) eachTopic(ctx context.Context, list func(context.Context, string) ([]bus.Entity, error)) ([]bus.Entity, error) {
	var topics []string
	tp := c.admin.NewListTopicsPager(nil)
	for tp.More() {
		page, err := tp.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, t := range page.Topics {
			topics = append(topics, t.TopicName)
		}
	}
	results := make([][]bus.Entity, len(topics))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(maxParallelTopics)
	for i, t := range topics {
		g.Go(func() error {
			return Safe("list subscriptions "+t, func() error {
				var err error
				results[i], err = list(gctx, t)
				return err
			})
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	var out []bus.Entity
	for _, r := range results {
		out = append(out, r...)
	}
	return out, nil
}

// Peek implements bus.Peeker with the SDK's PeekMessages, which takes no
// lock and leaves DeliveryCount alone.
func (b *Backend) Peek(ctx context.Context, req bus.PeekRequest) ([]bus.Message, error) {
	c, err := b.conn(req.Namespace)
	if err != nil {
		return nil, err
	}
	label := req.Entity.Path
	if req.SubQueue == bus.DeadLetter {
		label += "/$DLQ"
	}
	op := "peek " + label
	limit := req.Max
	if limit <= 0 {
		limit = bus.PageSize
	}
	key := receiverKey{req.Entity.Path, req.Entity.Kind, req.SubQueue}
	var out []bus.Message
	err = Safe(op, func() error {
		r, err := c.receiver(key)
		if err != nil {
			return err
		}
		from := req.FromSequence
		msgs, err := r.PeekMessages(ctx, limit, &azservicebus.PeekMessagesOptions{FromSequenceNumber: &from})
		if err != nil {
			// A canceled (superseded) or timed-out call says nothing about
			// the link; another call may be using the receiver right now.
			if k := bus.KindOf(mapErr(op, err)); k != bus.ErrCanceled && k != bus.ErrTimeout {
				c.dropReceiver(key, r)
			}
			return err
		}
		out = make([]bus.Message, 0, len(msgs))
		for _, m := range msgs {
			out = append(out, toMessage(m))
		}
		return nil
	})
	var be *bus.Error
	if req.SubQueue == bus.Active && errors.As(err, &be) && be.Kind == bus.ErrNotAllowed {
		// S−1: a plain receiver can't peek the active side of a sessionful
		// entity; that needs a session lock, which v0.1 never takes.
		be.Msg = bus.SessionfulActive
	}
	return out, err
}

// receiver returns the cached receiver for key, opening it on first use.
func (c *conn) receiver(key receiverKey) (*azservicebus.Receiver, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r, ok := c.receivers[key]; ok {
		return r, nil
	}
	r, err := c.openReceiver(key)
	if err != nil {
		return nil, err
	}
	c.receivers[key] = r
	return r, nil
}

// openReceiver opens a peek-lock receiver on key's entity and sub-queue.
func (c *conn) openReceiver(key receiverKey) (*azservicebus.Receiver, error) {
	opts := &azservicebus.ReceiverOptions{}
	if key.sub == bus.DeadLetter {
		opts.SubQueue = azservicebus.SubQueueDeadLetter
	}
	var r *azservicebus.Receiver
	var err error
	if key.kind == bus.KindSubscription {
		// Subscription names can't contain '/', topic names can.
		i := strings.LastIndex(key.path, "/")
		if i <= 0 {
			return nil, &bus.Error{Kind: bus.ErrNotFound, Msg: fmt.Sprintf("%q is not topic/subscription", key.path)}
		}
		r, err = c.client.NewReceiverForSubscription(key.path[:i], key.path[i+1:], opts)
	} else {
		r, err = c.client.NewReceiverForQueue(key.path, opts)
	}
	return r, err
}

// dropReceiver closes and forgets receiver r after a failed call, so the
// next call starts on a fresh link. If another call already replaced r in
// the cache, the replacement stays.
func (c *conn) dropReceiver(key receiverKey, r *azservicebus.Receiver) {
	c.mu.Lock()
	if c.receivers[key] == r {
		delete(c.receivers, key)
	}
	c.mu.Unlock()
	go func() { _ = r.Close(context.Background()) }()
}

// --- connection strings and transport --------------------------------------

type csConfig struct {
	host     string // host[:port] from Endpoint
	hostname string // host without port
	emulator bool
}

func parseConnectionString(cs string) (csConfig, error) {
	var cfg csConfig
	var endpoint string
	for part := range strings.SplitSeq(cs, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch strings.ToLower(k) {
		case "endpoint":
			endpoint = v
		case "usedevelopmentemulator":
			// Same parsing as the SDK's connection-string parser.
			b, err := strconv.ParseBool(v)
			if err != nil {
				return cfg, fmt.Errorf("connection string: bad UseDevelopmentEmulator %q", v)
			}
			cfg.emulator = b
		}
	}
	if endpoint == "" {
		return cfg, errors.New("connection string: no Endpoint")
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return cfg, fmt.Errorf("connection string: bad Endpoint %q", endpoint)
	}
	cfg.host, cfg.hostname = u.Host, u.Hostname()
	return cfg, nil
}

// shortName is the first DNS label: "sb-prod-weu" for
// "sb-prod-weu.servicebus.windows.net".
func shortName(host string) string {
	name, _, _ := strings.Cut(host, ".")
	return name
}

// httpOnly sends admin requests over plain HTTP. The SDK always builds
// https:// URLs, but the emulator's admin endpoint speaks HTTP only (S−1).
type httpOnly struct{ base http.RoundTripper }

func (t httpOnly) Do(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	u := *r.URL
	u.Scheme = "http"
	r.URL = &u
	return t.base.RoundTrip(r)
}
