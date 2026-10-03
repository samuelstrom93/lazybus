package azure

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/servicebus/armservicebus"

	"github.com/samuelstrom93/lazybus/internal/bus"
)

func ptr[T any](v T) *T { return &v }

func TestArmNamespace(t *testing.T) {
	sub := bus.Subscription{ID: "dd5f406d-6c25-497b-95eb-7e29f8840aa6", Name: "lazybus-dev"}
	ns, ok := armNamespace(&armservicebus.SBNamespace{
		Name:     ptr("sb-x"),
		ID:       ptr("/subscriptions/dd5f406d-6c25-497b-95eb-7e29f8840aa6/resourceGroups/rg-lazybus-test/providers/Microsoft.ServiceBus/namespaces/sb-x"),
		Location: ptr("swedencentral"),
		SKU:      &armservicebus.SBSKU{Name: ptr(armservicebus.SKUNameBasic)},
		Properties: &armservicebus.SBNamespaceProperties{
			ServiceBusEndpoint: ptr("https://sb-x.servicebus.usgovcloudapi.net:443/"),
		},
	}, sub)
	want := bus.Namespace{
		Name: "sb-x", FQDN: "sb-x.servicebus.usgovcloudapi.net", Auth: authEntra,
		Subscription: "lazybus-dev", SubscriptionID: sub.ID,
		ResourceGroup: "rg-lazybus-test", SKU: "Basic", Location: "swedencentral",
	}
	if !ok || ns != want {
		t.Fatalf("armNamespace = %+v, want %+v", ns, want)
	}

	ns, ok = armNamespace(&armservicebus.SBNamespace{Name: ptr("sb-y")}, sub)
	if !ok || ns.FQDN != "sb-y.servicebus.windows.net" || ns.ResourceGroup != "" {
		t.Fatalf("without endpoint: %+v", ns)
	}
	if _, ok := armNamespace(&armservicebus.SBNamespace{}, sub); ok {
		t.Fatal("namespace without a name accepted")
	}
}

func TestResourceGroup(t *testing.T) {
	for id, want := range map[string]string{
		"/subscriptions/s/resourceGroups/rg-a/providers/Microsoft.ServiceBus/namespaces/n": "rg-a",
		"/subscriptions/s/resourcegroups/RG-B/providers/x":                                 "RG-B",
		"/subscriptions/s": "",
		"":                 "",
	} {
		if got := resourceGroup(id); got != want {
			t.Errorf("resourceGroup(%q) = %q, want %q", id, got, want)
		}
	}
}

// countingCred hands out tokens that expire after ttl.
type countingCred struct {
	calls atomic.Int32
	ttl   time.Duration
	err   error
}

func (c *countingCred) GetToken(_ context.Context, opts policy.TokenRequestOptions) (azcore.AccessToken, error) {
	n := c.calls.Add(1)
	if c.err != nil {
		return azcore.AccessToken{}, c.err
	}
	return azcore.AccessToken{Token: opts.Scopes[0] + "#" + string(rune('0'+n)), ExpiresOn: time.Now().Add(c.ttl)}, nil
}

func TestCachedCredential(t *testing.T) {
	ctx := context.Background()
	arm := policy.TokenRequestOptions{Scopes: []string{"https://management.azure.com//.default"}}
	sb := policy.TokenRequestOptions{Scopes: []string{"https://servicebus.azure.net//.default"}}

	inner := &countingCred{ttl: time.Hour}
	cred := CachedCredential(inner)
	a, _ := cred.GetToken(ctx, arm)
	b, _ := cred.GetToken(ctx, arm)
	c, _ := cred.GetToken(ctx, sb)
	if inner.calls.Load() != 2 || a.Token != b.Token || c.Token == a.Token {
		t.Fatalf("calls %d, tokens %q %q %q", inner.calls.Load(), a.Token, b.Token, c.Token)
	}
	// A claims challenge always gets a new token.
	if _, err := cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: arm.Scopes, Claims: "x"}); err != nil || inner.calls.Load() != 3 {
		t.Fatalf("claims: calls %d, err %v", inner.calls.Load(), err)
	}

	// A token inside the margin is fetched again.
	short := &countingCred{ttl: tokenMargin - time.Minute}
	cred = CachedCredential(short)
	_, _ = cred.GetToken(ctx, arm)
	_, _ = cred.GetToken(ctx, arm)
	if short.calls.Load() != 2 {
		t.Fatalf("near-expiry token reused: %d calls", short.calls.Load())
	}

	// Errors are not cached.
	boom := &countingCred{err: errors.New("az login required")}
	cred = CachedCredential(boom)
	for range 2 {
		if _, err := cred.GetToken(ctx, arm); err == nil {
			t.Fatal("error swallowed")
		}
	}
	if boom.calls.Load() != 2 {
		t.Fatalf("error cached: %d calls", boom.calls.Load())
	}
}

func TestDiscoveryOff(t *testing.T) {
	b := New()
	defer b.Close(context.Background())
	if subs, err := b.Subscriptions(context.Background()); subs != nil || err != nil {
		t.Fatalf("Subscriptions with discovery off = %v, %v", subs, err)
	}
	if _, err := b.SubscriptionNamespaces(context.Background(), bus.Subscription{ID: "s"}); err == nil {
		t.Fatal("SubscriptionNamespaces with discovery off succeeded")
	}
}

// TestDiscoveredNamespacesAreNotConfigured: Namespaces lists only the
// command-line ones; a discovered one still opens (conn finds it).
func TestDiscoveredNamespacesAreNotConfigured(t *testing.T) {
	b := New()
	defer b.Close(context.Background())
	cred := &countingCred{ttl: time.Hour}
	if _, err := b.AddNamespace("sb-given", cred); err != nil {
		t.Fatal(err)
	}
	found := bus.Namespace{Name: "sb-found", FQDN: "sb-found.servicebus.windows.net", SubscriptionID: "s", Auth: authEntra}
	if _, err := b.addTokenNamespace(found, cred); err != nil {
		t.Fatal(err)
	}
	nss, _ := b.Namespaces(context.Background())
	if len(nss) != 1 || nss[0].Name != "sb-given" || nss[0].Auth != authEntra {
		t.Fatalf("Namespaces = %+v", nss)
	}
	if _, err := b.conn(found); err != nil {
		t.Fatalf("discovered namespace not registered: %v", err)
	}
}
