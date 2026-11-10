package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// demoBackup has the shape of the first backup: orders on both tiers, the
// session queue and the subscription only on Standard.
const demoBackup = `[
 {"Entity":"lazybus-orders","MessageID":"ord-6","Body":"{}","Props":{"orderId":13,"smoke":true,"tenant":"lazybus"}},
 {"Entity":"lazybus-orders/$DLQ","MessageID":"ord-1","DeadLetterReason":"SchemaMismatch","Props":{}},
 {"Entity":"lazybus-sessions/$DLQ","MessageID":"ses-1","SessionID":"a","DeadLetterReason":"PoisonSessionStep",
  "Props":{"DeadLetterReason":"PoisonSessionStep","orderId":13}},
 {"Entity":"lazybus-events/lazybus-billing","MessageID":"bil-3","Seq":3,"Body":"{\"invoice\":3}","Props":{"orderId":13}},
 {"Entity":"lazybus-events/lazybus-billing/$DLQ","MessageID":"bil-1","DeadLetterReason":"DownstreamTimeout",
  "DeadLetterDescription":"smoke test","Props":{"DeadLetterErrorDescription":"smoke test","DeadLetterReason":"DownstreamTimeout"}},
 {"Entity":"lazybus-events/lazybus-billing/$DLQ","MessageID":"bil-2","DeadLetterReason":"DownstreamTimeout","Props":{}},
 {"Entity":"lazybus-sessions","MessageID":"ses-2","SessionID":"a","Props":{}},
 {"Entity":"lazybus-sessions","MessageID":"ses-3","SessionID":"a","Props":{}}
]`

func mustParse(t *testing.T, data string) []Message {
	t.Helper()
	msgs, err := parseBackup([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	return msgs
}

func TestParseBackupKeepsIntegersAsInt64(t *testing.T) {
	msgs := mustParse(t, `[{"Entity":"q","MessageID":"m","Seq":7,"Body":"b",
		"Props":{"i":13,"big":9007199254740993,"f":1.5,"e":1e3,"huge":99999999999999999999,"b":true,"s":"x","n":null}}]`)
	want := map[string]any{
		"i": int64(13), "big": int64(9007199254740993), "f": 1.5, "e": 1000.0,
		"huge": 1e20, "b": true, "s": "x", "n": nil,
	}
	if got := msgs[0].Props; !reflect.DeepEqual(got, want) {
		t.Errorf("Props = %#v, want %#v", got, want)
	}
	if msgs[0].Seq != 7 || msgs[0].Body != "b" {
		t.Errorf("message = %+v", msgs[0])
	}
}

func TestParseBackupRejectsBadInput(t *testing.T) {
	for name, data := range map[string]string{
		"not an array":  `{"Entity":"q"}`,
		"no entity":     `[{"MessageID":"m"}]`,
		"trailing data": `[] []`,
	} {
		if _, err := parseBackup([]byte(data)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestDeadLetteredEntryRestoresWithoutBrokerProperties(t *testing.T) {
	dead := Message{Entity: "q/$DLQ", Props: map[string]any{
		"DeadLetterReason": "R", "DeadLetterErrorDescription": "D", "tenant": "lazybus",
	}}
	if got, want := restoreProps(dead), (map[string]any{"tenant": "lazybus"}); !reflect.DeepEqual(got, want) {
		t.Errorf("dead-lettered props = %v, want %v", got, want)
	}
	// Reason and description fall back to the properties the broker set.
	if r, d := deadLetterInfo(dead); r != "R" || d != "D" {
		t.Errorf("deadLetterInfo = %q, %q", r, d)
	}
	dead.DeadLetterReason, dead.DeadLetterDescription = "R2", "D2"
	if r, d := deadLetterInfo(dead); r != "R2" || d != "D2" {
		t.Errorf("deadLetterInfo = %q, %q", r, d)
	}
	// On an active message the same names are ordinary properties.
	active := Message{Entity: "q", Props: map[string]any{"DeadLetterReason": "R"}}
	if got := restoreProps(active); !reflect.DeepEqual(got, active.Props) {
		t.Errorf("active props = %v, want %v", got, active.Props)
	}
}

func TestBackupFiles(t *testing.T) {
	dir := t.TempDir()
	if got, err := backups(filepath.Join(dir, "missing"), "ns"); got != nil || err != nil {
		t.Errorf("missing dir: %q, %v", got, err)
	}
	at := time.Date(2026, 11, 10, 22, 5, 9, 0, time.FixedZone("CET", 3600))
	name := backupName("ns", at)
	if name != "ns-2026-11-10T210509Z.json" {
		t.Errorf("backupName = %s", name)
	}
	msgs := []Message{{Entity: "q", MessageID: "m", Body: "b", Props: map[string]any{"n": int64(1)}}}
	path := filepath.Join(dir, "sub", name)
	if err := writeBackup(path, msgs); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, %v; want 0600", info.Mode().Perm(), err)
	}
	if back, err := readBackup(path); err != nil || !reflect.DeepEqual(back, msgs) {
		t.Errorf("read back %+v, %v", back, err)
	}
	if err := writeBackup(path, nil); err == nil {
		t.Error("writeBackup overwrote an existing file")
	}
	// The newest is the last by name: a timestamped file beats the
	// hand-made dated one, and other namespaces' files do not count.
	for _, n := range []string{"ns-2026-11-10.json", "ns-2026-11-09T230000Z.json", "other-2027-01-01.json", "ns-notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, "sub", n), []byte("[]"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{path, filepath.Join(dir, "sub", "ns-2026-11-10.json"), filepath.Join(dir, "sub", "ns-2026-11-09T230000Z.json")}
	if got, err := backups(filepath.Join(dir, "sub"), "ns"); !reflect.DeepEqual(got, want) || err != nil {
		t.Errorf("backups = %q, %v; want %q", got, err, want)
	}
}

func TestEntitiesOnlyInOlderBackup(t *testing.T) {
	older := mustParse(t, demoBackup)
	// A second down after the first deleted lazybus-events sees only these.
	newest := []Message{{Entity: "lazybus-orders/$DLQ"}, {Entity: "lazybus-sessions"}}
	if got, want := entitiesOnlyIn(older, newest), []string{"lazybus-events/lazybus-billing"}; !reflect.DeepEqual(got, want) {
		t.Errorf("entitiesOnlyIn = %q, want %q", got, want)
	}
	if got := entitiesOnlyIn(older, older); got != nil {
		t.Errorf("same backup: %q", got)
	}
}

func TestBackupDir(t *testing.T) {
	env := map[string]string{}
	get := func(k string) string { return env[k] }
	if got := backupDir(get, "/home/u"); got != "/home/u/.local/state/lazybus/testns" {
		t.Errorf("default = %s", got)
	}
	env["XDG_STATE_HOME"] = "/state"
	if got := backupDir(get, "/home/u"); got != "/state/lazybus/testns" {
		t.Errorf("XDG = %s", got)
	}
}

func TestTTLAboveBasicMax(t *testing.T) {
	for ttl, want := range map[string]bool{
		"P14D": false, "P13DT23H59M59S": false, "PT336H": false, "PT1H": false, "PT0.5S": false,
		"P14DT1S": true, "P14DT0.001S": true, "P15D": true, "PT337H": true,
		"P10675199DT2H48M5.4775807S": true,                                          // "never expires"
		"":                           true, "P": true, "P1M": true, "forever": true, // unreadable: cap it
	} {
		if got := ttlAboveBasicMax(ttl); got != want {
			t.Errorf("ttlAboveBasicMax(%q) = %v, want %v", ttl, got, want)
		}
	}
}

func basicState() namespaceState {
	return namespaceState{Tier: tierBasic, Queues: []queue{
		{Name: "lazybus-e2e-q", TTL: "P14D"},
		{Name: "lazybus-orders", Active: 2, DeadLetter: 5, TTL: "P14D"},
	}}
}

func standardState() namespaceState {
	return namespaceState{
		Tier: tierStandard,
		Queues: []queue{
			{Name: "lazybus-e2e-dedup", DuplicateDetection: true, TTL: "P14D"},
			{Name: "lazybus-e2e-dlfwd", ForwardDeadLetter: "lazybus-e2e-dlsink", TTL: "P14D"},
			{Name: "lazybus-e2e-dlsink", TTL: "P14D"},
			{Name: "lazybus-e2e-q", TTL: "P14D"},
			{Name: "lazybus-orders", Active: 2, DeadLetter: 5, TTL: "P10675199DT2H48M5.4775807S"},
			{Name: "lazybus-sessions", Active: 2, DeadLetter: 1, RequiresSession: true, TTL: "P10675199DT2H48M5.4775807S"},
		},
		Topics: []topic{
			{Name: "lazybus-e2e-topic", Subscriptions: []subscription{{Name: "lazybus-e2e-all"}, {Name: "lazybus-e2e-shipped"}}},
			{Name: "lazybus-events", Subscriptions: []subscription{{Name: "lazybus-billing", Active: 1, DeadLetter: 2}}},
		},
	}
}

func ids(msgs []Message) string {
	var out []string
	for _, m := range msgs {
		out = append(out, m.MessageID)
	}
	return strings.Join(out, ",")
}

func TestPlanUpFromBasic(t *testing.T) {
	p, err := planUp(basicState(), mustParse(t, demoBackup))
	if err != nil {
		t.Fatal(err)
	}
	if !p.ChangeSKU || !p.CreateTopic || !p.CreateSubscription || !p.CreateSessionQueue {
		t.Errorf("plan = %+v, want the SKU change and all three creates", p)
	}
	if len(p.Restore) != 2 {
		t.Fatalf("Restore = %+v, want sessions and billing", p.Restore)
	}
	ses, bil := p.Restore[0], p.Restore[1]
	if ses.Queue != "lazybus-sessions" || !ses.RequiresSession || ids(ses.Active) != "ses-2,ses-3" || ids(ses.DeadLetter) != "ses-1" {
		t.Errorf("sessions target = %+v", ses)
	}
	if bil.Topic != "lazybus-events" || bil.Subscription != "lazybus-billing" || bil.RequiresSession ||
		ids(bil.Active) != "bil-3" || ids(bil.DeadLetter) != "bil-1,bil-2" {
		t.Errorf("billing target = %+v", bil)
	}
	// lazybus-orders exists on both tiers and still holds its messages.
	want := []skipped{{"lazybus-orders", "already holds 7 message(s)"}}
	if !reflect.DeepEqual(p.Skipped, want) {
		t.Errorf("Skipped = %+v, want %+v", p.Skipped, want)
	}
}

func TestPlanUpRerunIsEmpty(t *testing.T) {
	p, err := planUp(standardState(), mustParse(t, demoBackup))
	if err != nil {
		t.Fatal(err)
	}
	if !p.empty() || len(p.Skipped) != 3 {
		t.Errorf("plan = %+v, want nothing to do and three skipped entities", p)
	}
}

func TestPlanUpRestoreDecisions(t *testing.T) {
	st := standardState()
	st.Queues[4].Active, st.Queues[4].DeadLetter = 0, 0 // lazybus-orders emptied
	st.Queues[5].RequiresSession = true
	st.Queues[5].Active, st.Queues[5].DeadLetter = 0, 0 // lazybus-sessions emptied
	st.Topics[1].Subscriptions = append(st.Topics[1].Subscriptions, subscription{Name: "audit"})
	st.Topics[1].Subscriptions[0].Active, st.Topics[1].Subscriptions[0].DeadLetter = 0, 0
	backup := mustParse(t, demoBackup+"")
	backup = append(backup,
		Message{Entity: "lazybus-e2e-q", MessageID: "e2e"},
		Message{Entity: "gone/$DLQ", MessageID: "gone"},
		Message{Entity: "lazybus-sessions", MessageID: "no-session"},
	)
	p, err := planUp(st, backup)
	if err != nil {
		t.Fatal(err)
	}
	if p.ChangeSKU || p.CreateTopic || p.CreateSubscription || p.CreateSessionQueue {
		t.Errorf("plan = %+v, want no SKU change and no creates", p)
	}
	// An existing entity is restored only when it holds nothing.
	if len(p.Restore) != 1 || p.Restore[0].Queue != "lazybus-orders" ||
		ids(p.Restore[0].Active) != "ord-6" || ids(p.Restore[0].DeadLetter) != "ord-1" {
		t.Errorf("Restore = %+v, want only lazybus-orders", p.Restore)
	}
	reasons := map[string]string{}
	for _, s := range p.Skipped {
		reasons[s.Entity] = s.Reason
	}
	for entity, want := range map[string]string{
		"lazybus-sessions":               "no SessionID",
		"lazybus-events/lazybus-billing": "has 2 subscriptions",
		"lazybus-e2e-q":                  "E2E test",
		"gone":                           "does not exist",
	} {
		if !strings.Contains(reasons[entity], want) {
			t.Errorf("skip reason for %s = %q, want it to mention %q", entity, reasons[entity], want)
		}
	}
}

func TestPlanUpCreatesOnlyWhatIsMissing(t *testing.T) {
	st := standardState()
	st.Topics[1].Subscriptions = nil // lazybus-events without lazybus-billing
	p, err := planUp(st, mustParse(t, demoBackup))
	if err != nil {
		t.Fatal(err)
	}
	if p.CreateTopic || !p.CreateSubscription || p.CreateSessionQueue || p.ChangeSKU {
		t.Errorf("plan = %+v, want only the subscription created", p)
	}
	if len(p.Restore) != 1 || p.Restore[0].Subscription != "lazybus-billing" {
		t.Errorf("Restore = %+v, want only the created subscription", p.Restore)
	}
}

func TestPlanDownFromStandard(t *testing.T) {
	p, err := planDown(standardState())
	if err != nil {
		t.Fatal(err)
	}
	want := downPlan{
		Backup: []source{
			{entityRef{Queue: "lazybus-orders"}, false, 2, 5},
			{entityRef{Queue: "lazybus-sessions"}, true, 2, 1},
			{entityRef{Topic: "lazybus-events", Subscription: "lazybus-billing"}, false, 1, 2},
		},
		DeleteTopics: []string{"lazybus-e2e-topic", "lazybus-events"},
		DeleteQueues: []string{"lazybus-sessions", "lazybus-e2e-dedup", "lazybus-e2e-dlfwd", "lazybus-e2e-dlsink"},
		SetTTL:       []string{"lazybus-orders"},
		ChangeSKU:    true,
	}
	if !reflect.DeepEqual(p, want) {
		t.Errorf("plan =\n%+v\nwant\n%+v", p, want)
	}
}

func TestPlanDownIsIdempotent(t *testing.T) {
	p, err := planDown(basicState())
	if err != nil {
		t.Fatal(err)
	}
	if !p.empty() || len(p.Backup) != 0 {
		t.Errorf("plan on Basic = %+v, want nothing to do", p)
	}

	// Entities already gone, SKU still Standard (a run that failed at the
	// SKU change): only the SKU is left, and no second, emptier backup.
	st := basicState()
	st.Tier = tierStandard
	st.Queues = append(st.Queues, queue{Name: "lazybus-e2e-dedup", DuplicateDetection: true, TTL: "P14D"})
	p, err = planDown(st)
	if err != nil {
		t.Fatal(err)
	}
	want := downPlan{DeleteQueues: []string{"lazybus-e2e-dedup"}, ChangeSKU: true}
	if !reflect.DeepEqual(p, want) {
		t.Errorf("plan = %+v, want %+v", p, want)
	}
}

func TestPlanDownRefusesUnknownStandardOnlyEntities(t *testing.T) {
	for name, add := range map[string]func(*namespaceState){
		"topic other": func(st *namespaceState) { st.Topics = append(st.Topics, topic{Name: "other"}) },
		"queue q (sessions)": func(st *namespaceState) {
			st.Queues = append(st.Queues, queue{Name: "q", RequiresSession: true, TTL: "P1D"})
		},
		"queue q (duplicate detection)": func(st *namespaceState) {
			st.Queues = append(st.Queues, queue{Name: "q", DuplicateDetection: true, TTL: "P1D"})
		},
		"queue q (forwarding)": func(st *namespaceState) {
			st.Queues = append(st.Queues, queue{Name: "q", ForwardTo: "lazybus-orders", TTL: "P1D"})
		},
	} {
		st := standardState()
		add(&st)
		if _, err := planDown(st); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: err = %v, want a refusal naming it", name, err)
		}
	}
}

func TestPlansRefuseOtherTiers(t *testing.T) {
	st := namespaceState{Tier: "Premium"}
	if _, err := planDown(st); err == nil {
		t.Error("planDown accepted Premium")
	}
	if _, err := planUp(st, nil); err == nil {
		t.Error("planUp accepted Premium")
	}
}
