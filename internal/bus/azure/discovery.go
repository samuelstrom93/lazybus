package azure

import (
	"context"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armsubscriptions"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/servicebus/armservicebus"

	"github.com/samuelstrom93/lazybus/internal/bus"
)

// authEntra is the Auth label of namespaces opened with a token credential
// (lazybus uses the az CLI credential).
const authEntra = "Entra ID (az login)"

// maxParallelDiscovery bounds the concurrent per-subscription namespace
// listings.
const maxParallelDiscovery = 4

// EnableDiscovery turns on ARM discovery with cred: Subscriptions lists the
// subscriptions cred can read, and SubscriptionNamespaces their Service Bus
// namespaces, which then open with the same credential.
func (b *Backend) EnableDiscovery(cred azcore.TokenCredential) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.discoveryCred = cred
	b.discoverySem = make(chan struct{}, maxParallelDiscovery)
}

func (b *Backend) discovery() (azcore.TokenCredential, chan struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.discoveryCred, b.discoverySem
}

// Subscriptions implements bus.Discovery: the enabled subscriptions the
// credential can read, sorted by name. None when discovery is off.
func (b *Backend) Subscriptions(ctx context.Context) ([]bus.Subscription, error) {
	cred, _ := b.discovery()
	if cred == nil {
		return nil, nil
	}
	const op = "discover subscriptions"
	var out []bus.Subscription
	err := Safe(op, func() error {
		client, err := armsubscriptions.NewClient(cred, nil)
		if err != nil {
			return err
		}
		pager := client.NewListPager(nil)
		for pager.More() {
			page, err := pager.NextPage(ctx)
			if err != nil {
				return err
			}
			for _, s := range page.Value {
				if s == nil || s.SubscriptionID == nil {
					continue
				}
				// Disabled and deleted subscriptions can't list resources.
				if s.State != nil && (*s.State == armsubscriptions.SubscriptionStateDisabled || *s.State == armsubscriptions.SubscriptionStateDeleted) {
					continue
				}
				name := *s.SubscriptionID
				if s.DisplayName != nil && *s.DisplayName != "" {
					name = *s.DisplayName
				}
				out = append(out, bus.Subscription{ID: *s.SubscriptionID, Name: name})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name)
		if a != b {
			return a < b
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// SubscriptionNamespaces implements bus.Discovery: the Service Bus
// namespaces of sub, sorted by name. Each one is added to the backend with
// the discovery credential, so the data plane and admin calls use it too.
func (b *Backend) SubscriptionNamespaces(ctx context.Context, sub bus.Subscription) ([]bus.Namespace, error) {
	op := "discover namespaces " + sub.Name
	cred, sem := b.discovery()
	if cred == nil {
		return nil, &bus.Error{Kind: bus.ErrNotFound, Op: op, Msg: "discovery is off"}
	}
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	case <-ctx.Done():
		return nil, mapErr(op, ctx.Err())
	}
	var found []bus.Namespace
	err := Safe(op, func() error {
		client, err := armservicebus.NewNamespacesClient(sub.ID, cred, nil)
		if err != nil {
			return err
		}
		pager := client.NewListPager(nil)
		for pager.More() {
			page, err := pager.NextPage(ctx)
			if err != nil {
				return err
			}
			for _, n := range page.Value {
				if ns, ok := armNamespace(n, sub); ok {
					found = append(found, ns)
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Name < found[j].Name })
	out := make([]bus.Namespace, 0, len(found))
	for _, ns := range found {
		if _, err := b.addTokenNamespace(ns, cred); err != nil {
			return nil, mapErr(op, err)
		}
		out = append(out, ns)
	}
	return out, nil
}

// armNamespace converts an ARM namespace resource. The FQDN comes from its
// Service Bus endpoint (so sovereign clouds work), falling back to
// <name>.servicebus.windows.net.
func armNamespace(n *armservicebus.SBNamespace, sub bus.Subscription) (bus.Namespace, bool) {
	if n == nil || n.Name == nil || *n.Name == "" {
		return bus.Namespace{}, false
	}
	ns := bus.Namespace{
		Name: *n.Name, FQDN: *n.Name + ".servicebus.windows.net", Auth: authEntra,
		Subscription: sub.Name, SubscriptionID: sub.ID,
	}
	if n.Properties != nil && n.Properties.ServiceBusEndpoint != nil {
		if u, err := url.Parse(*n.Properties.ServiceBusEndpoint); err == nil && u.Hostname() != "" {
			ns.FQDN = u.Hostname()
		}
	}
	if n.ID != nil {
		ns.ResourceGroup = resourceGroup(*n.ID)
	}
	if n.SKU != nil && n.SKU.Name != nil {
		ns.SKU = string(*n.SKU.Name)
	}
	if n.Location != nil {
		ns.Location = *n.Location
	}
	return ns, true
}

// resourceGroup extracts the resource group from an ARM resource ID
// (/subscriptions/…/resourceGroups/<rg>/providers/…).
func resourceGroup(id string) string {
	parts := strings.Split(id, "/")
	for i := 0; i+1 < len(parts); i++ {
		if strings.EqualFold(parts[i], "resourceGroups") {
			return parts[i+1]
		}
	}
	return ""
}

// --- credential cache --------------------------------------------------------

// CachedCredential wraps cred with a token cache. The az CLI credential
// runs `az` for every token and serializes the calls, and every SDK client
// (one per discovered subscription, per namespace, per AMQP link) asks for
// its own; with the cache they share one token per scope until it nears
// expiry.
func CachedCredential(cred azcore.TokenCredential) azcore.TokenCredential {
	return &cachedCredential{cred: cred, tokens: map[string]azcore.AccessToken{}}
}

type cachedCredential struct {
	cred   azcore.TokenCredential
	mu     sync.Mutex // held while fetching, so concurrent callers wait for one fetch
	tokens map[string]azcore.AccessToken
}

// tokenMargin: a cached token is reused only while it has this long left.
// It is longer than the SDK's own early-refresh window (5 min), so a
// client refreshing its token gets a fresh one, not the same one again.
const tokenMargin = 10 * time.Minute

func (c *cachedCredential) GetToken(ctx context.Context, opts policy.TokenRequestOptions) (azcore.AccessToken, error) {
	if opts.Claims != "" {
		// A claims challenge needs a new token.
		return c.cred.GetToken(ctx, opts)
	}
	key := opts.TenantID + "|" + strings.Join(opts.Scopes, " ")
	c.mu.Lock()
	defer c.mu.Unlock()
	if tk, ok := c.tokens[key]; ok && time.Until(tk.ExpiresOn) > tokenMargin {
		return tk, nil
	}
	tk, err := c.cred.GetToken(ctx, opts)
	if err != nil {
		return tk, err
	}
	c.tokens[key] = tk
	return tk, nil
}
