package ui

import (
	"context"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/samuelstrom93/lazybus/internal/bus"
)

// The Namespaces panel: the configured namespaces first, then the ones ARM
// discovery finds, subscription by subscription (spec §4 panel 1, §7). A
// subscription whose namespaces are still being listed, or failed to list,
// shows as one placeholder row; one without namespaces shows nothing.

// nsRowKind tells namespace rows from subscription placeholders.
type nsRowKind int

const (
	rowNamespace  nsRowKind = iota
	rowSubLoading           // the subscription's namespaces are being listed
	rowSubFailed            // listing them failed (the error is in the log)
)

// nsRow is one row of the Namespaces panel.
type nsRow struct {
	kind nsRowKind
	ns   bus.Namespace    // rowNamespace
	sub  bus.Subscription // placeholders
	err  error            // rowSubFailed
}

func (r nsRow) key() string {
	if r.kind == rowNamespace {
		return "ns:" + r.ns.FQDN
	}
	return "sub:" + r.sub.ID
}

// discoveryState is what the Namespaces panel is built from.
type discoveryState struct {
	gen               int // id of the newest discovery; older responses are dropped
	configured        []bus.Namespace
	configuredLoading bool
	configuredErr     error
	subsLoading       bool
	subsErr           error
	subs              []subState // in the backend's order (by name)
}

type subState struct {
	sub     bus.Subscription
	loading bool
	nss     []bus.Namespace // from the last successful listing
	err     error
}

// Messages returned by discovery commands.
type (
	configuredLoadedMsg struct {
		gen   int
		items []bus.Namespace
		err   error
	}
	subscriptionsLoadedMsg struct {
		gen  int
		subs []bus.Subscription
		err  error
	}
	subNamespacesLoadedMsg struct {
		gen   int
		sub   bus.Subscription
		items []bus.Namespace
		err   error
	}
)

// discover starts a discovery round: the configured namespaces and the
// subscription list, at once. Each subscription's namespaces follow when
// the list arrives.
func (m Model) discover() tea.Cmd {
	be, timeout, gen := m.be, m.opts.CallTimeout, m.disc.gen
	configured := func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		items, err := call(func() ([]bus.Namespace, error) { return be.Namespaces(ctx) })
		return configuredLoadedMsg{gen: gen, items: items, err: err}
	}
	subs := func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		items, err := call(func() ([]bus.Subscription, error) { return be.Subscriptions(ctx) })
		return subscriptionsLoadedMsg{gen: gen, subs: items, err: err}
	}
	return tea.Batch(configured, subs)
}

// rediscover is R on Namespaces: a new round. The rows stay until their
// replacements arrive.
func (m *Model) rediscover() tea.Cmd {
	m.disc.gen++
	m.disc.configuredLoading, m.disc.subsLoading = true, true
	m.rebuildNamespaces()
	return m.discover()
}

func (m Model) configuredLoaded(msg configuredLoadedMsg) (Model, tea.Cmd) {
	if msg.gen != m.disc.gen {
		return m, nil
	}
	m.disc.configuredLoading = false
	m.disc.configuredErr = msg.err
	if msg.err != nil {
		m.logf(true, "%s", errText("list namespaces", msg.err))
	} else {
		m.disc.configured = msg.items
		if len(msg.items) > 0 {
			m.logf(false, "list namespaces → %d", len(msg.items))
		}
	}
	m.rebuildNamespaces()
	// The first configured namespace opens by itself. Discovered ones
	// never do: discovery spans every subscription, and opening one the
	// user did not pick would list and peek it unasked.
	if m.openNS == nil && msg.err == nil && len(msg.items) > 0 {
		ns := msg.items[0]
		m.openNS = &ns
		m.selectNamespaceRow("ns:" + ns.FQDN)
		return m, m.loadEntities(ns, true, false)
	}
	return m, nil
}

func (m Model) subscriptionsLoaded(msg subscriptionsLoadedMsg) (Model, tea.Cmd) {
	if msg.gen != m.disc.gen {
		return m, nil
	}
	m.disc.subsLoading = false
	m.disc.subsErr = msg.err
	if msg.err != nil {
		m.logf(true, "%s", errText("discover subscriptions", msg.err))
		m.disc.subs = nil
		m.rebuildNamespaces()
		return m, nil
	}
	if len(msg.subs) > 0 {
		m.logf(false, "discover subscriptions → %d", len(msg.subs))
	}
	prev := map[string]subState{}
	for _, s := range m.disc.subs {
		prev[s.sub.ID] = s
	}
	subs := make([]subState, 0, len(msg.subs))
	cmds := make([]tea.Cmd, 0, len(msg.subs))
	for _, sub := range msg.subs {
		// A refresh keeps the namespaces found last time until the new
		// listing arrives.
		subs = append(subs, subState{sub: sub, loading: true, nss: prev[sub.ID].nss})
		cmds = append(cmds, m.discoverSub(sub))
	}
	m.disc.subs = subs
	m.rebuildNamespaces()
	return m, tea.Batch(cmds...)
}

// discoverSub lists the namespaces of one subscription.
func (m Model) discoverSub(sub bus.Subscription) tea.Cmd {
	be, timeout, gen := m.be, m.opts.CallTimeout, m.disc.gen
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		items, err := call(func() ([]bus.Namespace, error) { return be.SubscriptionNamespaces(ctx, sub) })
		return subNamespacesLoadedMsg{gen: gen, sub: sub, items: items, err: err}
	}
}

func (m Model) subNamespacesLoaded(msg subNamespacesLoadedMsg) Model {
	if msg.gen != m.disc.gen {
		return m
	}
	subs := append([]subState(nil), m.disc.subs...)
	for i := range subs {
		if subs[i].sub.ID != msg.sub.ID {
			continue
		}
		subs[i].loading = false
		subs[i].err = msg.err
		if msg.err != nil {
			subs[i].nss = nil
			m.logf(true, "%s", errText("discover namespaces "+msg.sub.Name, msg.err))
		} else {
			subs[i].nss = msg.items
			m.logf(false, "discover %s → %d namespace%s", msg.sub.Name, len(msg.items), plural(len(msg.items)))
		}
	}
	m.disc.subs = subs
	m.rebuildNamespaces()
	return m
}

// rebuildNamespaces rebuilds the panel rows from the discovery state,
// keeping the cursor on the same row when it is still there.
func (m *Model) rebuildNamespaces() {
	prev, hadPrev := m.namespaces.selected()
	var rows []nsRow
	seen := map[string]bool{}
	for _, ns := range m.disc.configured {
		rows = append(rows, nsRow{kind: rowNamespace, ns: ns})
		seen[ns.FQDN] = true
	}
	loading := m.disc.configuredLoading || m.disc.subsLoading
	for _, s := range m.disc.subs {
		loading = loading || s.loading
		switch {
		case len(s.nss) > 0:
			for _, ns := range s.nss {
				// A namespace also given on the command line stays
				// there, with the auth it was given.
				if !seen[ns.FQDN] {
					rows = append(rows, nsRow{kind: rowNamespace, ns: ns})
					seen[ns.FQDN] = true
				}
			}
		case s.loading:
			rows = append(rows, nsRow{kind: rowSubLoading, sub: s.sub})
		case s.err != nil:
			rows = append(rows, nsRow{kind: rowSubFailed, sub: s.sub, err: s.err})
		}
	}
	m.namespaces.loading = loading
	m.namespaces.err = m.disc.configuredErr
	if m.namespaces.err == nil {
		m.namespaces.err = m.disc.subsErr
	}
	m.namespaces.setAll(rows, nsText)
	if hadPrev {
		m.selectNamespaceRow(prev.key())
	}
}

// selectNamespaceRow puts the cursor on the row with key, when visible.
func (m *Model) selectNamespaceRow(key string) {
	for i, r := range m.namespaces.items {
		if r.key() == key {
			m.namespaces.cursor = i
			return
		}
	}
}

// nsText is the visible text of a row, for the filter.
func nsText(r nsRow) string {
	if r.kind == rowNamespace {
		return r.ns.Name
	}
	return r.sub.Name
}

// nsRowSegs renders one Namespaces row in width cells (cursor mark
// excluded).
func nsRowSegs(r nsRow, width int) []seg {
	switch r.kind {
	case rowSubLoading:
		return []seg{{"loading " + r.sub.Name + "…", stDim}}
	case rowSubFailed:
		const suffix = ": error"
		name := sanitize(r.sub.Name)
		if ansi.StringWidth(name) > width-len(suffix) {
			name = fitPlain(name, max(1, width-len(suffix)))
		}
		return []seg{{name + suffix, stErr}}
	}
	return []seg{{r.ns.Name, stPlain}}
}

// namespaceDetails is the main pane while Namespaces is focused: the
// selected namespace, or the subscription of a placeholder row.
func (m Model) namespaceDetails(width int) (title string, lines []string) {
	r, ok := m.namespaces.selected()
	if !ok {
		var status []seg
		switch {
		case m.namespaces.err != nil:
			var out []string
			for _, l := range wrapLines("error: "+sanitize(m.namespaces.err.Error()), width-2) {
				out = append(out, row([]seg{{" " + l, stErr}}, width, false))
			}
			return "Namespace", out
		case m.namespaces.loading:
			status = []seg{{"discovering namespaces…", stDim}}
		case m.namespaces.filter != "":
			status = []seg{{"no namespace matches the filter", stDim}}
		default:
			status = []seg{{"no namespaces: log in with az login, or use --namespace, --connection-string or --emulator", stDim}}
		}
		return "Namespace", []string{row(append([]seg{{" ", stPlain}}, status...), width, false)}
	}
	const labelW = 16
	field := func(label, value string, st lipgloss.Style) []string {
		if value == "" {
			return nil
		}
		var out []string
		for i, l := range wrapLines(sanitize(value), max(1, width-2-labelW-1)) {
			lab := ""
			if i == 0 {
				lab = label
			}
			out = append(out, row([]seg{{" " + fitPlain(lab, labelW) + " ", stDim}, {l, st}}, width, false))
		}
		return out
	}
	if r.kind != rowNamespace {
		lines = append(lines, field("Subscription", r.sub.Name, stPlain)...)
		lines = append(lines, field("Subscription ID", r.sub.ID, stPlain)...)
		if r.kind == rowSubLoading {
			lines = append(lines, field("Status", "listing namespaces…", stDim)...)
		} else {
			lines = append(lines, field("Status", "listing namespaces failed: "+r.err.Error(), stErr)...)
		}
		return "Subscription", lines
	}
	ns := r.ns
	lines = append(lines, field("Namespace", ns.Name, stPlain)...)
	lines = append(lines, field("FQDN", ns.FQDN, stPlain)...)
	lines = append(lines, field("Subscription", ns.Subscription, stPlain)...)
	lines = append(lines, field("Subscription ID", ns.SubscriptionID, stPlain)...)
	lines = append(lines, field("Resource group", ns.ResourceGroup, stPlain)...)
	sku := ns.SKU
	if strings.EqualFold(sku, "Basic") {
		sku += " (queues only: Basic has no topics)"
	}
	lines = append(lines, field("SKU", sku, stPlain)...)
	lines = append(lines, field("Location", ns.Location, stPlain)...)
	lines = append(lines, field("Auth", ns.Auth, stPlain)...)
	if ns.SubscriptionID == "" {
		lines = append(lines, field("Source", "command line (not discovered)", stDim)...)
	}
	if m.openNS != nil && m.openNS.FQDN == ns.FQDN {
		lines = append(lines, "", row([]seg{{" open in Entities", stDim}}, width, false))
		// A failed load of the open namespace shows here in full: the
		// panel rows and the log cut it.
		if m.entities.err != nil || m.messages.err != nil {
			lines = append(lines, "")
			lines = append(lines, m.statusLines(width)...)
		}
	} else {
		lines = append(lines, "", row([]seg{{" enter opens it in Entities", stDim}}, width, false))
	}
	return "Namespace", lines
}
