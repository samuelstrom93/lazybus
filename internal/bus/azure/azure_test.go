package azure

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"github.com/Azure/go-amqp"

	"github.com/samuelstrom93/lazybus/internal/bus"
)

func TestAddConnectionStrings(t *testing.T) {
	b := New()
	defer b.Close(context.Background())

	ns, err := b.AddConnectionString("Endpoint=sb://sb-prod-weu.servicebus.windows.net/;SharedAccessKeyName=k;SharedAccessKey=dGVzdA==", 0)
	if err != nil {
		t.Fatal(err)
	}
	if ns.Name != "sb-prod-weu" || ns.FQDN != "sb-prod-weu.servicebus.windows.net" {
		t.Fatalf("SAS namespace = %+v", ns)
	}

	em, err := b.AddConnectionString(EmulatorConnectionString("localhost", 5682), 5310)
	if err != nil {
		t.Fatal(err)
	}
	if em.Name != "emulator" || em.FQDN != "localhost:5682" {
		t.Fatalf("emulator namespace = %+v", em)
	}

	// The same namespace twice is listed once.
	if _, err := b.AddConnectionString(EmulatorConnectionString("localhost", 5682), 5310); err != nil {
		t.Fatal(err)
	}
	nss, _ := b.Namespaces(context.Background())
	if len(nss) != 2 {
		t.Fatalf("namespaces = %+v", nss)
	}
	if !b.conns[1].emulator || b.conns[0].emulator {
		t.Fatal("emulator flag not set from UseDevelopmentEmulator")
	}

	if _, err := b.AddConnectionString("SharedAccessKey=x", 0); err == nil {
		t.Fatal("connection string without Endpoint accepted")
	}
	if _, err := b.AddConnectionString("Endpoint=sb://localhost:5682;SharedAccessKeyName=k;SharedAccessKey=x;UseDevelopmentEmulator=yes", 5310); err == nil {
		t.Fatal("UseDevelopmentEmulator=yes accepted (strconv.ParseBool rejects it, like the SDK)")
	}
	if !IsEmulatorConnectionString("Endpoint=sb://localhost:5682;SharedAccessKeyName=k;SharedAccessKey=x;UseDevelopmentEmulator=TRUE") {
		t.Fatal("UseDevelopmentEmulator=TRUE not recognised")
	}
}

// TestDropReceiverKeepsReplacement: a late failure of an old receiver must
// not evict the receiver another call has cached since.
func TestDropReceiverKeepsReplacement(t *testing.T) {
	b := New()
	defer b.Close(context.Background())
	if _, err := b.AddConnectionString(EmulatorConnectionString("localhost", 5682), 5310); err != nil {
		t.Fatal(err)
	}
	c := b.conns[0]
	key := receiverKey{"orders", bus.KindQueue, bus.DeadLetter}
	old, err := c.receiver(key) // receivers attach lazily: no network here
	if err != nil {
		t.Fatal(err)
	}
	c.dropReceiver(key, old)
	fresh, err := c.receiver(key)
	if err != nil || fresh == old {
		t.Fatalf("receiver not replaced after drop: %v", err)
	}
	c.dropReceiver(key, old) // stale failure arrives late
	if got, _ := c.receiver(key); got != fresh {
		t.Fatal("stale drop evicted the fresh receiver")
	}
}

// TestAddNamespaceWithCLICredential checks the --namespace wiring without
// calling az: clients are lazy, so nothing touches the network here.
func TestAddNamespaceWithCLICredential(t *testing.T) {
	cred, err := azidentity.NewAzureCLICredential(nil)
	if err != nil {
		t.Fatal(err)
	}
	b := New()
	defer b.Close(context.Background())
	ns, err := b.AddNamespace("sb-test-weu", cred)
	if err != nil {
		t.Fatal(err)
	}
	if ns.Name != "sb-test-weu" || ns.FQDN != "sb-test-weu.servicebus.windows.net" {
		t.Fatalf("namespace = %+v", ns)
	}
	if b.conns[0].emulator {
		t.Fatal("--namespace marked as emulator")
	}
}

func TestUnknownNamespace(t *testing.T) {
	_, err := New().ListEntities(context.Background(), bus.Namespace{Name: "x", FQDN: "x.servicebus.windows.net"})
	if bus.KindOf(err) != bus.ErrNotFound {
		t.Fatalf("err = %v", err)
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		err  error
		kind bus.ErrorKind
		msg  string
	}{
		{context.DeadlineExceeded, bus.ErrTimeout, "timed out"},
		{fmt.Errorf("wrapped: %w", context.Canceled), bus.ErrCanceled, "canceled"},
		{&amqp.Error{Condition: amqp.ErrCondNotFound, Description: "The messaging entity 'x' could not be found. TrackingId:abc, SystemTracker:x, Timestamp:now"},
			bus.ErrNotFound, "amqp:not-found: The messaging entity 'x' could not be found"},
		{&amqp.Error{Condition: amqp.ErrCondNotAllowed}, bus.ErrNotAllowed, "amqp:not-allowed"},
		{&amqp.Error{Condition: "com.microsoft:server-busy", Description: "busy"}, bus.ErrThrottled, "com.microsoft:server-busy: busy"},
		{&azcore.ResponseError{StatusCode: 401, ErrorCode: "Unauthorized"}, bus.ErrUnauthorized, "HTTP 401 Unauthorized"},
		{&azcore.ResponseError{StatusCode: 404}, bus.ErrNotFound, "HTTP 404"},
		{errors.New("AzureCLICredential: Azure CLI not found on path\nmore"), bus.ErrUnauthorized, "az CLI credential: AzureCLICredential: Azure CLI not found on path"},
		{errors.New("\n first line \nsecond"), bus.ErrUnknown, "first line"},
	}
	for _, tc := range cases {
		err := mapErr("op", tc.err)
		var be *bus.Error
		if !errors.As(err, &be) {
			t.Fatalf("%v: not a *bus.Error", tc.err)
		}
		if be.Kind != tc.kind || be.Msg != tc.msg || be.Op != "op" {
			t.Errorf("%v: got %v %q, want %v %q", tc.err, be.Kind, be.Msg, tc.kind, tc.msg)
		}
		if !errors.Is(err, tc.err) {
			t.Errorf("%v: original error not unwrapped", tc.err)
		}
	}
}

func TestSafeRecoversPanic(t *testing.T) {
	err := Safe("list entities", func() error { panic("nil pointer") })
	if bus.KindOf(err) != bus.ErrUnknown || err.Error() != "list entities: SDK panic: nil pointer" {
		t.Fatalf("err = %v", err)
	}
}

func TestToMessage(t *testing.T) {
	seq, at := int64(7), time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	reason := "SchemaMismatch"
	uuid := amqp.UUID{0x6f, 0x1c, 0x2b, 0x9e, 0x4d, 0x2a, 0x4c, 0x1e, 0x9b, 0x7a, 0, 0, 0, 0, 0, 1}
	m := toMessage(&azservicebus.ReceivedMessage{
		SequenceNumber:   &seq,
		EnqueuedTime:     &at,
		DeadLetterReason: &reason,
		ApplicationProperties: map[string]any{
			"s": "x", "i": int32(1), "l": int64(2), "d": 0.5, "b": true, "g": uuid, "t": at,
			bus.MarkerDeadLetterReason: reason, bus.MarkerDeadLetterErrorDescription: "desc",
		},
	})
	if m.SequenceNumber != 7 || !m.EnqueuedTime.Equal(at) || m.DeadLetterReason != reason {
		t.Fatalf("message = %+v", m)
	}
	want := []struct {
		key string
		typ bus.PropertyType
		val any
	}{
		{"b", bus.TypeBool, true}, {"d", bus.TypeDouble, 0.5}, {"g", bus.TypeGUID, "6f1c2b9e-4d2a-4c1e-9b7a-000000000001"},
		{"i", bus.TypeInt, int32(1)}, {"l", bus.TypeLong, int64(2)}, {"s", bus.TypeString, "x"}, {"t", bus.TypeDateTime, at},
	}
	if len(m.Properties) != len(want) {
		t.Fatalf("properties = %+v (markers must be removed)", m.Properties)
	}
	for i, w := range want {
		p := m.Properties[i]
		if p.Key != w.key || p.Type != w.typ || p.Value != w.val {
			t.Errorf("property %d = %+v, want %+v", i, p, w)
		}
	}
}
