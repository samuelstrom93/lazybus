// Package ui is the Bubble Tea front end of lazybus. It talks to the broker
// only through the interfaces in internal/bus, and every broker call runs as
// a tea.Cmd that returns a message; Update never blocks.
package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/samuelstrom93/lazybus/internal/bus"
)

// Options configure the UI.
type Options struct {
	// ReadOnly disables every state-changing key and shows READ-ONLY.
	ReadOnly bool
	// Location is the time zone times are shown in; nil means time.Local.
	Location *time.Location
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// mainTab is a tab of the main pane.
type mainTab int

const (
	tabBody mainTab = iota
	tabProperties
	tabSystem
	tabCount
)

func (t mainTab) String() string {
	return [...]string{"Body", "Properties", "System"}[t]
}

// list is the state of one side panel.
type list[T any] struct {
	items   []T
	cursor  int
	offset  int
	loading bool
	err     error
	req     int // id of the newest load; older responses are dropped
}

func (l *list[T]) selected() (T, bool) {
	var zero T
	if l.cursor < 0 || l.cursor >= len(l.items) {
		return zero, false
	}
	return l.items[l.cursor], true
}

// move puts the cursor at i, clamped to the list.
func (l *list[T]) move(i int) {
	l.cursor = max(0, min(i, len(l.items)-1))
}

// scrollTo keeps the cursor inside a window of n rows.
func (l *list[T]) scrollTo(n int) {
	if n <= 0 {
		return
	}
	if l.cursor < l.offset {
		l.offset = l.cursor
	}
	if l.cursor >= l.offset+n {
		l.offset = l.cursor - n + 1
	}
	l.offset = max(0, min(l.offset, len(l.items)-n))
}

type logEntry struct {
	at   time.Time
	text string
	err  bool
}

type helpState struct {
	filter string
	offset int
}

// Model is the root Bubble Tea model.
type Model struct {
	be   bus.Browser
	opts Options

	width, height int

	stack    ContextStack
	lastSide ContextID // side panel that stays expanded while main is focused

	namespaces list[bus.Namespace]
	entities   list[bus.Entity]
	messages   list[bus.Message]

	openNS     *bus.Namespace
	openEntity *bus.Entity
	subQueue   bus.SubQueue

	tab        mainTab
	mainScroll int

	log    []logEntry
	help   helpState
	status string
}

// New returns the root model for backend be.
func New(be bus.Browser, opts Options) Model {
	if opts.Location == nil {
		opts.Location = time.Local
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return Model{
		be:         be,
		opts:       opts,
		stack:      NewContextStack(CtxNamespaces),
		lastSide:   CtxNamespaces,
		namespaces: list[bus.Namespace]{loading: true},
	}
}

// Messages returned by broker commands.
type (
	namespacesLoadedMsg struct {
		items []bus.Namespace
		err   error
	}
	entitiesLoadedMsg struct {
		req     int
		ns      bus.Namespace
		items   []bus.Entity
		err     error
		cascade bool
	}
	messagesLoadedMsg struct {
		req   int
		ns    bus.Namespace
		ent   bus.Entity
		sub   bus.SubQueue
		items []bus.Message
		err   error
	}
)

// Init starts namespace discovery.
func (m Model) Init() tea.Cmd {
	be := m.be
	return func() tea.Msg {
		items, err := be.Namespaces(context.Background())
		return namespacesLoadedMsg{items: items, err: err}
	}
}

func (m *Model) loadEntities(ns bus.Namespace, cascade bool) tea.Cmd {
	m.entities.req++
	m.entities.loading = true
	m.entities.err = nil
	req, be := m.entities.req, m.be
	return func() tea.Msg {
		items, err := be.ListEntities(context.Background(), ns)
		return entitiesLoadedMsg{req: req, ns: ns, items: items, err: err, cascade: cascade}
	}
}

func (m *Model) peek(ns bus.Namespace, ent bus.Entity, sub bus.SubQueue) tea.Cmd {
	m.messages.req++
	m.messages.loading = true
	m.messages.err = nil
	req, be := m.messages.req, m.be
	return func() tea.Msg {
		items, err := be.Peek(context.Background(), bus.PeekRequest{
			Namespace: ns, Entity: ent, SubQueue: sub, Max: bus.PageSize,
		})
		return messagesLoadedMsg{req: req, ns: ns, ent: ent, sub: sub, items: items, err: err}
	}
}

func (m *Model) logf(isErr bool, format string, args ...any) {
	m.log = append(m.log, logEntry{at: m.opts.Now(), text: fmt.Sprintf(format, args...), err: isErr})
}

func entityLabel(e bus.Entity, sub bus.SubQueue) string {
	if sub == bus.DeadLetter {
		return e.Path + "/$DLQ"
	}
	return e.Path
}

// Update handles messages.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height

	case namespacesLoadedMsg:
		m.namespaces.loading = false
		m.namespaces.err = msg.err
		if msg.err != nil {
			m.logf(true, "list namespaces failed: %v", msg.err)
			break
		}
		m.namespaces.items = msg.items
		m.namespaces.move(0)
		m.logf(false, "list namespaces → %d", len(msg.items))
		if ns, ok := m.namespaces.selected(); ok {
			m.openNS = &ns
			cmd = m.loadEntities(ns, true)
		}

	case entitiesLoadedMsg:
		if msg.req != m.entities.req {
			break
		}
		m.entities.loading = false
		m.entities.err = msg.err
		if msg.err != nil {
			m.logf(true, "list entities %s failed: %v", msg.ns.Name, msg.err)
			break
		}
		m.entities.items = msg.items
		m.entities.offset = 0
		m.entities.move(0)
		m.logf(false, "list entities %s → %d", msg.ns.Name, len(msg.items))
		if ent, ok := m.entities.selected(); ok && msg.cascade {
			m.openEntity = &ent
			cmd = m.peek(msg.ns, ent, m.subQueue)
		}

	case messagesLoadedMsg:
		if msg.req != m.messages.req {
			break
		}
		m.messages.loading = false
		m.messages.err = msg.err
		label := entityLabel(msg.ent, msg.sub)
		if msg.err != nil {
			m.logf(true, "peek %s failed: %v", label, msg.err)
			break
		}
		m.messages.items = msg.items
		m.messages.offset = 0
		m.messages.move(0)
		m.mainScroll = 0
		m.logf(false, "peek %s → %d", label, len(msg.items))

	case tea.KeyPressMsg:
		m, cmd = m.handleKey(msg)
	}
	m.clampScroll()
	return m, cmd
}

// handleKey routes a key to the top context only.
func (m Model) handleKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	if key.Matches(msg, keys.ForceQuit) {
		return m, tea.Quit
	}
	m.status = ""
	if m.stack.Top() == CtxHelp {
		return m.handleHelpKey(msg)
	}
	return m.handlePanelKey(msg)
}

func (m Model) handleHelpKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	switch {
	case key.Matches(msg, keys.HelpClose):
		m.stack.Pop()
	case key.Matches(msg, keys.HelpUp):
		m.help.offset = max(0, m.help.offset-1)
	case key.Matches(msg, keys.HelpDown):
		m.help.offset++
	case key.Matches(msg, keys.HelpErase):
		if r := []rune(m.help.filter); len(r) > 0 {
			m.help.filter = string(r[:len(r)-1])
			m.help.offset = 0
		}
	case msg.Text != "" && msg.Mod&(tea.ModCtrl|tea.ModAlt) == 0:
		m.help.filter += msg.Text
		m.help.offset = 0
	}
	return m, nil
}

var panelOrder = []ContextID{CtxNamespaces, CtxEntities, CtxMessages, CtxMain}

func (m *Model) focus(c ContextID) {
	m.stack.Focus(c)
	if c.isSide() {
		m.lastSide = c
	}
}

func (m Model) handlePanelKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	cur := m.stack.Top()
	switch {
	case key.Matches(msg, keys.Quit):
		if m.stack.AtRoot() {
			return m, tea.Quit
		}
	case key.Matches(msg, keys.Help):
		m.help = helpState{}
		m.stack.Push(CtxHelp)
	case key.Matches(msg, keys.FocusNamespaces):
		m.focus(CtxNamespaces)
	case key.Matches(msg, keys.FocusEntities):
		m.focus(CtxEntities)
	case key.Matches(msg, keys.FocusMessages):
		m.focus(CtxMessages)
	case key.Matches(msg, keys.FocusMain):
		m.focus(CtxMain)
	case key.Matches(msg, keys.PrevPanel), key.Matches(msg, keys.NextPanel):
		i := 0
		for j, c := range panelOrder {
			if c == cur {
				i = j
			}
		}
		if key.Matches(msg, keys.PrevPanel) {
			i--
		} else {
			i++
		}
		m.focus(panelOrder[max(0, min(i, len(panelOrder)-1))])
	case key.Matches(msg, keys.PrevTab):
		m.tab = (m.tab + tabCount - 1) % tabCount
		m.mainScroll = 0
	case key.Matches(msg, keys.NextTab):
		m.tab = (m.tab + 1) % tabCount
		m.mainScroll = 0
	case key.Matches(msg, keys.Down):
		m.moveBy(cur, 1)
	case key.Matches(msg, keys.Up):
		m.moveBy(cur, -1)
	case key.Matches(msg, keys.HalfDown):
		m.moveBy(cur, max(1, m.pageRows(cur)/2))
	case key.Matches(msg, keys.HalfUp):
		m.moveBy(cur, -max(1, m.pageRows(cur)/2))
	case key.Matches(msg, keys.Top):
		m.moveTo(cur, 0)
	case key.Matches(msg, keys.Bottom):
		m.moveTo(cur, 1<<30)
	case key.Matches(msg, keys.Back):
		if cur == CtxMain {
			m.focus(m.lastSide)
		}
	case cur == CtxMessages && key.Matches(msg, keys.SubQueue):
		if m.openNS != nil && m.openEntity != nil {
			if m.subQueue == bus.DeadLetter {
				m.subQueue = bus.Active
			} else {
				m.subQueue = bus.DeadLetter
			}
			m.messages.items = nil
			return m, m.peek(*m.openNS, *m.openEntity, m.subQueue)
		}
	case key.Matches(msg, keys.Open):
		return m.open(cur)
	}
	return m, nil
}

// open handles enter on a panel: Namespaces and Entities open the next
// panel, Messages focuses the main pane.
func (m Model) open(cur ContextID) (Model, tea.Cmd) {
	switch cur {
	case CtxNamespaces:
		ns, ok := m.namespaces.selected()
		if !ok {
			return m, nil
		}
		m.openNS = &ns
		m.openEntity = nil
		m.entities.items = nil
		m.messages = list[bus.Message]{req: m.messages.req + 1}
		m.focus(CtxEntities)
		return m, m.loadEntities(ns, false)
	case CtxEntities:
		ent, ok := m.entities.selected()
		if !ok || m.openNS == nil {
			return m, nil
		}
		m.openEntity = &ent
		m.subQueue = bus.DeadLetter
		m.messages.items = nil
		m.focus(CtxMessages)
		return m, m.peek(*m.openNS, ent, m.subQueue)
	case CtxMessages:
		m.focus(CtxMain)
	}
	return m, nil
}

func (m *Model) moveBy(c ContextID, d int) {
	switch c {
	case CtxNamespaces:
		m.namespaces.move(m.namespaces.cursor + d)
	case CtxEntities:
		m.entities.move(m.entities.cursor + d)
	case CtxMessages:
		before := m.messages.cursor
		m.messages.move(m.messages.cursor + d)
		if m.messages.cursor != before {
			m.mainScroll = 0
		}
	case CtxMain:
		m.mainScroll += d
	}
}

func (m *Model) moveTo(c ContextID, i int) {
	switch c {
	case CtxNamespaces:
		m.namespaces.move(i)
	case CtxEntities:
		m.entities.move(i)
	case CtxMessages:
		before := m.messages.cursor
		m.messages.move(i)
		if m.messages.cursor != before {
			m.mainScroll = 0
		}
	case CtxMain:
		m.mainScroll = i
	}
}

// pageRows is the number of visible rows of context c.
func (m Model) pageRows(c ContextID) int {
	l := m.layout()
	switch c {
	case CtxNamespaces:
		return l.side[0]
	case CtxEntities:
		return l.side[1]
	case CtxMessages:
		return l.side[2]
	}
	return l.mainRows
}

// clampScroll keeps list cursors visible and the main pane inside its content.
func (m *Model) clampScroll() {
	if m.width == 0 || m.height == 0 {
		return
	}
	l := m.layout()
	m.namespaces.scrollTo(l.side[0])
	m.entities.scrollTo(l.side[1])
	m.messages.scrollTo(l.side[2])
	maxScroll := max(0, len(m.mainLines(l.mainW-2))-l.mainRows)
	m.mainScroll = max(0, min(m.mainScroll, maxScroll))
	if m.stack.Top() == CtxHelp {
		_, _, n := m.helpBox()
		m.help.offset = max(0, min(m.help.offset, len(m.helpEntries())-n))
	}
}

// View renders the screen.
func (m Model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	v.WindowTitle = "lazybus"
	return v
}

// focusedSide is the side panel that is expanded: the focused one, or the
// last focused one while the main pane or a popup has focus.
func (m Model) focusedSide() ContextID {
	if r := m.stack.Root(); r.isSide() {
		return r
	}
	return m.lastSide
}

func (m Model) selectedMessage() (bus.Message, bool) {
	if m.messages.loading {
		return bus.Message{}, false
	}
	return m.messages.selected()
}

// helpEntries returns the `?` menu lines for the root context, filtered.
func (m Model) helpEntries() []helpLine {
	root := m.stack.Root()
	var out []helpLine
	add := func(section string, bs []key.Binding) {
		var rows []helpLine
		f := strings.ToLower(m.help.filter)
		for _, b := range bs {
			h := b.Help()
			if f != "" && !strings.Contains(strings.ToLower(h.Key+" "+h.Desc), f) {
				continue
			}
			rows = append(rows, helpLine{key: h.Key, desc: h.Desc})
		}
		if len(rows) > 0 {
			out = append(out, helpLine{section: section})
			out = append(out, rows...)
		}
	}
	name := root.String()
	if root == CtxMain {
		name = "Main pane"
	}
	add(name, contextBindings(root))
	add("Global", globalBindings())
	return out
}

type helpLine struct {
	section   string
	key, desc string
}
