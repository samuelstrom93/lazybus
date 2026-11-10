package main

import (
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const (
	tierBasic    = "Basic"
	tierStandard = "Standard"

	// Entities with this prefix belong to the real-namespace E2E test
	// (internal/bus/azure/azure_e2e_test.go). It creates them itself, per
	// tier; testns never creates, backs up or restores them.
	e2ePrefix = "lazybus-e2e-"

	// The demo entities that need Standard; up recreates them.
	demoTopic        = "lazybus-events"
	demoSubscription = "lazybus-billing"
	demoSessionQueue = "lazybus-sessions" // RequiresSession

	basicMaxTTL = "P14D"
)

// The Standard-only entities down deletes, in this order (the forwarding
// queue before its target). Nothing else is ever deleted.
var (
	standardOnlyTopics = []string{"lazybus-e2e-topic", demoTopic}
	standardOnlyQueues = []string{demoSessionQueue, "lazybus-e2e-dedup", "lazybus-e2e-dlfwd", "lazybus-e2e-dlsink"}
)

// queue, subscription and topic are what the tool reads from the namespace.
type queue struct {
	Name               string
	Active, DeadLetter int64 // Active includes scheduled messages
	RequiresSession    bool
	DuplicateDetection bool
	ForwardTo          string
	ForwardDeadLetter  string
	TTL                string // ISO 8601 duration
}

type subscription struct {
	Name               string
	Active, DeadLetter int64
	RequiresSession    bool
}

type topic struct {
	Name          string
	Subscriptions []subscription
}

type namespaceState struct {
	Tier   string
	Queues []queue
	Topics []topic
}

// entityRef names a queue or a topic subscription.
type entityRef struct{ Queue, Topic, Subscription string }

func (e entityRef) path() string {
	if e.Queue != "" {
		return e.Queue
	}
	return e.Topic + "/" + e.Subscription
}

func checkTier(tier string) error {
	if tier != tierBasic && tier != tierStandard {
		return fmt.Errorf("namespace tier is %q: testns only switches between Basic and Standard", tier)
	}
	return nil
}

// source is an entity down backs up before it deletes anything.
type source struct {
	entityRef
	RequiresSession    bool
	Active, DeadLetter int64
}

// downPlan is what down does, in this order.
type downPlan struct {
	Backup       []source
	DeleteTopics []string
	DeleteQueues []string
	SetTTL       []string // queues whose default TTL is above the Basic maximum
	ChangeSKU    bool
}

func (p downPlan) empty() bool {
	return len(p.DeleteTopics)+len(p.DeleteQueues)+len(p.SetTTL) == 0 && !p.ChangeSKU
}

// planDown decides what a downgrade to Basic has to do from st. It fails
// when the namespace holds a Standard-only entity that is not on the
// delete list: Azure would refuse the downgrade, and the tool does not
// delete entities it does not know.
func planDown(st namespaceState) (downPlan, error) {
	if err := checkTier(st.Tier); err != nil {
		return downPlan{}, err
	}
	var p downPlan
	var unknown []string
	for _, t := range st.Topics {
		if slices.Contains(standardOnlyTopics, t.Name) {
			continue
		}
		unknown = append(unknown, "topic "+t.Name)
	}
	for _, name := range standardOnlyTopics {
		if slices.ContainsFunc(st.Topics, func(t topic) bool { return t.Name == name }) {
			p.DeleteTopics = append(p.DeleteTopics, name)
		}
	}
	for _, name := range standardOnlyQueues {
		if slices.ContainsFunc(st.Queues, func(q queue) bool { return q.Name == name }) {
			p.DeleteQueues = append(p.DeleteQueues, name)
		}
	}
	for _, q := range st.Queues {
		if slices.Contains(standardOnlyQueues, q.Name) {
			continue
		}
		if why := standardOnlyFeature(q); why != "" {
			unknown = append(unknown, fmt.Sprintf("queue %s (%s)", q.Name, why))
		}
		if ttlAboveBasicMax(q.TTL) {
			p.SetTTL = append(p.SetTTL, q.Name)
		}
	}
	if len(unknown) > 0 {
		return downPlan{}, fmt.Errorf("the namespace holds Standard-only entities testns does not manage: %s. "+
			"Basic cannot keep them; delete them by hand first", strings.Join(unknown, ", "))
	}
	p.ChangeSKU = st.Tier != tierBasic
	// A backup is taken when a demo entity goes away. It then covers every
	// demo entity, so the file is a full snapshot like the first one.
	deletesDemo := slices.ContainsFunc(slices.Concat(p.DeleteTopics, p.DeleteQueues), func(n string) bool {
		return !strings.HasPrefix(n, e2ePrefix)
	})
	if deletesDemo {
		p.Backup = demoSources(st)
	}
	return p, nil
}

// standardOnlyFeature names the first property of q that Basic lacks.
func standardOnlyFeature(q queue) string {
	switch {
	case q.RequiresSession:
		return "sessions"
	case q.DuplicateDetection:
		return "duplicate detection"
	case q.ForwardTo != "" || q.ForwardDeadLetter != "":
		return "forwarding"
	}
	return ""
}

// demoSources lists every queue and subscription that is not the E2E test's.
func demoSources(st namespaceState) []source {
	var out []source
	for _, q := range st.Queues {
		if !strings.HasPrefix(q.Name, e2ePrefix) {
			out = append(out, source{entityRef{Queue: q.Name}, q.RequiresSession, q.Active, q.DeadLetter})
		}
	}
	for _, t := range st.Topics {
		if strings.HasPrefix(t.Name, e2ePrefix) {
			continue
		}
		for _, s := range t.Subscriptions {
			out = append(out, source{entityRef{Topic: t.Name, Subscription: s.Name}, s.RequiresSession, s.Active, s.DeadLetter})
		}
	}
	return out
}

var isoDuration = regexp.MustCompile(`^P(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?(?:(\d+(?:\.\d+)?)S)?)?$`)

// ttlAboveBasicMax reports whether the ISO 8601 duration ttl is longer
// than 14 days. A duration it cannot read counts as too long: setting P14D
// once too often is harmless, a refused downgrade is not.
func ttlAboveBasicMax(ttl string) bool {
	m := isoDuration.FindStringSubmatch(ttl)
	if m == nil || ttl == "P" {
		return true
	}
	var seconds float64
	for i, unit := range []float64{86400, 3600, 60, 1} {
		if m[i+1] == "" {
			continue
		}
		v, err := strconv.ParseFloat(m[i+1], 64)
		if err != nil {
			return true
		}
		seconds += v * unit
	}
	return seconds > 14*86400
}

// restoreTarget is an entity up restores messages into: first the
// dead-lettered ones (send, receive, dead-letter), then the active ones.
type restoreTarget struct {
	entityRef
	RequiresSession    bool
	Active, DeadLetter []Message
}

type skipped struct{ Entity, Reason string }

// upPlan is what up does, in this order.
type upPlan struct {
	ChangeSKU          bool
	CreateTopic        bool
	CreateSubscription bool
	CreateSessionQueue bool
	Restore            []restoreTarget
	Skipped            []skipped // backup entities that are not restored, and why
}

func (p upPlan) empty() bool {
	return !p.ChangeSKU && !p.CreateTopic && !p.CreateSubscription && !p.CreateSessionQueue && len(p.Restore) == 0
}

// destination is an entity as it will be once up has created what is missing.
type destination struct {
	ref             entityRef
	requiresSession bool
	created         bool
	messages        int64 // active + dead-lettered, 0 for a created entity
	siblings        int   // subscriptions of the same topic, itself included
}

// planUp decides what an upgrade to Standard has to do from st and the
// backup. Messages are restored only into entities that up creates or that
// hold no message at all, so a rerun never duplicates.
func planUp(st namespaceState, backup []Message) (upPlan, error) {
	if err := checkTier(st.Tier); err != nil {
		return upPlan{}, err
	}
	p := upPlan{ChangeSKU: st.Tier != tierStandard}
	dests := map[string]destination{}
	for _, q := range st.Queues {
		dests[q.Name] = destination{ref: entityRef{Queue: q.Name}, requiresSession: q.RequiresSession, messages: q.Active + q.DeadLetter}
	}
	if _, ok := dests[demoSessionQueue]; !ok {
		p.CreateSessionQueue = true
		dests[demoSessionQueue] = destination{ref: entityRef{Queue: demoSessionQueue}, requiresSession: true, created: true}
	}
	p.CreateTopic = true
	p.CreateSubscription = true
	for _, t := range st.Topics {
		siblings := len(t.Subscriptions)
		if t.Name == demoTopic {
			p.CreateTopic = false
			if slices.ContainsFunc(t.Subscriptions, func(s subscription) bool { return s.Name == demoSubscription }) {
				p.CreateSubscription = false
			} else {
				siblings++
			}
		}
		for _, s := range t.Subscriptions {
			ref := entityRef{Topic: t.Name, Subscription: s.Name}
			dests[ref.path()] = destination{ref: ref, requiresSession: s.RequiresSession, messages: s.Active + s.DeadLetter, siblings: siblings}
		}
	}
	if p.CreateSubscription {
		ref := entityRef{Topic: demoTopic, Subscription: demoSubscription}
		siblings := 1
		for _, t := range st.Topics {
			if t.Name == demoTopic {
				siblings = len(t.Subscriptions) + 1
			}
		}
		dests[ref.path()] = destination{ref: ref, created: true, siblings: siblings}
	}

	var order []string
	byEntity := map[string]*restoreTarget{}
	for _, m := range backup {
		base, dlq := splitEntity(m.Entity)
		t, ok := byEntity[base]
		if !ok {
			t = &restoreTarget{}
			byEntity[base] = t
			order = append(order, base)
		}
		if dlq {
			t.DeadLetter = append(t.DeadLetter, m)
		} else {
			t.Active = append(t.Active, m)
		}
	}
	for _, base := range order {
		t := byEntity[base]
		d, exists := dests[base]
		if reason := skipReason(base, d, exists, *t); reason != "" {
			p.Skipped = append(p.Skipped, skipped{base, reason})
			continue
		}
		t.entityRef, t.RequiresSession = d.ref, d.requiresSession
		p.Restore = append(p.Restore, *t)
	}
	return p, nil
}

// skipReason says why the backup messages of entity base are not restored
// into d, or "" when they are.
func skipReason(base string, d destination, exists bool, t restoreTarget) string {
	switch {
	case strings.HasPrefix(base, e2ePrefix):
		return "owned by the E2E test"
	case !exists:
		return "the entity does not exist and testns does not create it"
	case !d.created && d.messages > 0:
		return fmt.Sprintf("already holds %d message(s)", d.messages)
	case d.ref.Subscription != "" && d.siblings > 1:
		return fmt.Sprintf("topic %s has %d subscriptions; a restore through the topic would reach all of them", d.ref.Topic, d.siblings)
	case d.ref.Subscription != "" && d.requiresSession:
		return "session subscriptions are not supported"
	case d.requiresSession && slices.ContainsFunc(slices.Concat(t.Active, t.DeadLetter), func(m Message) bool { return m.SessionID == "" }):
		return "the entity requires sessions and a backed-up message has no SessionID"
	}
	return ""
}

func (p downPlan) print(w io.Writer, namespace, backupFile string) {
	fmt.Fprintf(w, "Plan: downgrade %s to Basic\n", namespace)
	if p.empty() {
		fmt.Fprintln(w, "  nothing to do")
		return
	}
	if len(p.Backup) > 0 {
		fmt.Fprintf(w, "  back up messages to %s (nothing is deleted if this fails):\n", backupFile)
		for _, s := range p.Backup {
			fmt.Fprintf(w, "    %s: %d active, %d dead-lettered\n", s.path(), s.Active, s.DeadLetter)
		}
	}
	for _, t := range p.DeleteTopics {
		fmt.Fprintf(w, "  delete topic %s and its subscriptions\n", t)
	}
	for _, q := range p.DeleteQueues {
		fmt.Fprintf(w, "  delete queue %s\n", q)
	}
	for _, q := range p.SetTTL {
		fmt.Fprintf(w, "  set DefaultMessageTimeToLive=%s on queue %s\n", basicMaxTTL, q)
	}
	if p.ChangeSKU {
		fmt.Fprintln(w, "  change the SKU to Basic and wait until the namespace reports it")
	}
}

func (p upPlan) print(w io.Writer, namespace, backupFile string) {
	fmt.Fprintf(w, "Plan: upgrade %s to Standard\n", namespace)
	if p.empty() {
		fmt.Fprintln(w, "  nothing to do")
	}
	if p.ChangeSKU {
		fmt.Fprintln(w, "  change the SKU to Standard (billed per hour until `down`) and wait until the namespace reports it")
	}
	if p.CreateTopic {
		fmt.Fprintf(w, "  create topic %s\n", demoTopic)
	}
	if p.CreateSubscription {
		fmt.Fprintf(w, "  create subscription %s/%s\n", demoTopic, demoSubscription)
	}
	if p.CreateSessionQueue {
		fmt.Fprintf(w, "  create queue %s (RequiresSession)\n", demoSessionQueue)
	}
	if backupFile == "" {
		fmt.Fprintln(w, "  no backup file: no messages are restored")
		return
	}
	for _, t := range p.Restore {
		fmt.Fprintf(w, "  restore into %s: %d active, %d dead-lettered (from %s)\n", t.path(), len(t.Active), len(t.DeadLetter), backupFile)
	}
	for _, s := range p.Skipped {
		fmt.Fprintf(w, "  skip restore into %s: %s\n", s.Entity, s.Reason)
	}
}
