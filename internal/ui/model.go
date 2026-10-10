// Package ui is the Bubble Tea front end of lazybus. It talks to the broker
// only through the interfaces in internal/bus, and every broker call runs as
// a tea.Cmd that returns a message; Update never blocks.
package ui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
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
	// CallTimeout bounds every broker call; zero means DefaultCallTimeout.
	CallTimeout time.Duration
}

// DefaultCallTimeout is the per-call deadline for broker calls.
const DefaultCallTimeout = 30 * time.Second

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
	// all holds every loaded item in display order; items the visible
	// ones: all, or those matching filter. items[i] is all[idx[i]]; idx
	// is nil when no filter is set. The cursor indexes items. Replace all
	// only through setAll, which re-applies the filter.
	all     []T
	items   []T
	idx     []int
	filter  string
	cursor  int
	offset  int
	loading bool
	err     error
	req     int  // id of the newest load; older responses are dropped
	more    bool // the last page was not empty: another page may follow

	cancel context.CancelFunc // cancels the newest load
}

// setAll replaces the items and applies the filter, matching each item's
// visible text (text). The cursor is left to the caller; it is clamped.
func (l *list[T]) setAll(all []T, text func(T) string) {
	l.all = all
	l.applyFilter(text)
}

// applyFilter recomputes the visible items: a case-insensitive substring
// match of the filter on each item's text.
func (l *list[T]) applyFilter(text func(T) string) {
	if l.filter == "" {
		l.items, l.idx = l.all, nil
	} else {
		f := strings.ToLower(l.filter)
		var items []T
		var idx []int
		for i, it := range l.all {
			if strings.Contains(strings.ToLower(text(it)), f) {
				items = append(items, it)
				idx = append(idx, i)
			}
		}
		l.items, l.idx = items, idx
	}
	l.move(l.cursor)
}

// allIndex maps a visible index to its index in all; -1 when out of range.
func (l list[T]) allIndex(i int) int {
	if i < 0 || i >= len(l.items) {
		return -1
	}
	if l.idx == nil {
		return i
	}
	return l.idx[i]
}

// visibleIndex maps an index in all to its visible index; -1 when the
// filter hides it.
func (l list[T]) visibleIndex(i int) int {
	if l.idx == nil {
		if i < 0 || i >= len(l.items) {
			return -1
		}
		return i
	}
	for v, a := range l.idx {
		if a == i {
			return v
		}
	}
	return -1
}

// begin starts a new load: it cancels the one in flight (its response
// will be dropped by req anyway) and returns a context with the per-call
// deadline. The caller's command must call the returned cancel when done.
func (l *list[T]) begin(timeout time.Duration) (context.Context, context.CancelFunc, int) {
	if l.cancel != nil {
		l.cancel()
	}
	l.req++
	l.loading = true
	l.err = nil
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	l.cancel = cancel
	return ctx, cancel, l.req
}

// reset cancels any load in flight and empties the list, filter included.
// Responses to earlier loads are dropped.
func (l *list[T]) reset() {
	if l.cancel != nil {
		l.cancel()
	}
	*l = list[T]{req: l.req + 1}
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
	be   bus.Backend
	opts Options

	width, height int

	stack    ContextStack
	lastSide ContextID // side panel that stays expanded while main is focused

	namespaces list[nsRow]
	entities   list[bus.Entity]
	messages   list[bus.Message]
	// msgCols are the column widths of messages.all, kept in step with it
	// so a frame does not measure every loaded row.
	msgCols msgCols

	disc    discoveryState
	discSem chan struct{} // bounds the discovery calls in flight
	sortDLQ bool          // s: Entities sorted by DLQ count, not path

	openNS     *bus.Namespace
	openEntity *bus.Entity
	subQueue   bus.SubQueue

	tab        mainTab
	mainScroll int
	// mainCursor is the selected row on the Properties and System tabs:
	// what y copies.
	mainCursor int

	// afterPage is the cursor index to apply when the next page arrives:
	// a repair removed the last loaded row, and the next message is on
	// that page.
	afterPage    int
	hasAfterPage bool
	// keepIndex is the cursor index to restore when the first page of a
	// refresh (R) arrives.
	keepIndex    int
	hasKeepIndex bool

	jump jumpState // the : popup (CtxJump)
	clip clipboard

	log  []logEntry
	help helpState

	// status is the status bar text (an outcome or a refusal); the next
	// key clears it.
	status statusLine
	// marks are the rows that keep an outcome (SendUncertain,
	// CleanupPending). Copied on write.
	marks   map[markKey]rowMark
	confirm confirmState // the open confirm popup (CtxConfirm)
	edit    editState    // the open edit popup (CtxEdit)
	// pending holds the Pending Edits per DLQ message, in memory only.
	// Copied on write. editRev numbers the edits (pending.rev).
	pending map[markKey]pending
	editRev int
	busy    busyState // the busy popup (CtxBusy)
	// quitAfterCall is the quit (tea.Quit or tea.Interrupt) a signal asked
	// for during a state-changing call; run once the outcome is in.
	quitAfterCall tea.Cmd
	exit          exitReport // the outcome of that call, for ExitReport
	spin          spinner.Model

	// lines caches the main pane content of the selected message, so a
	// large body is not re-formatted on every keystroke. A pointer: the
	// Model is copied by value on every Update.
	lines *linesCache
}

// New returns the root model for backend be.
func New(be bus.Backend, opts Options) Model {
	if opts.Location == nil {
		opts.Location = time.Local
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.CallTimeout <= 0 {
		opts.CallTimeout = DefaultCallTimeout
	}
	return Model{
		lines:      &linesCache{},
		be:         be,
		opts:       opts,
		stack:      NewContextStack(CtxNamespaces),
		lastSide:   CtxNamespaces,
		namespaces: list[nsRow]{loading: true},
		disc:       newDiscovery(),
		discSem:    make(chan struct{}, maxParallelDiscovery),
		spin:       spinner.New(spinner.WithSpinner(spinner.MiniDot)),
		clip:       newClipboard(),
	}
}

// Messages returned by broker commands.
type (
	entitiesLoadedMsg struct {
		req     int
		ns      bus.Namespace
		items   []bus.Entity
		err     error
		cascade bool // open the first entity (startup)
		keep    bool // a refresh: keep the cursor on the same entity
	}
	messagesLoadedMsg struct {
		req   int
		ns    bus.Namespace
		ent   bus.Entity
		sub   bus.SubQueue
		from  int64 // first sequence number asked for; > 0 for a next page
		items []bus.Message
		err   error
	}
)

// call runs one broker call. A panic in the backend becomes an error, so a
// broken backend never takes the UI down.
func call[T any](f func() (T, error)) (v T, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("internal error: %v", r)
		}
	}()
	return f()
}

// Init starts namespace discovery.
func (m Model) Init() tea.Cmd {
	return m.discover()
}

func (m *Model) loadEntities(ns bus.Namespace, cascade, keep bool) tea.Cmd {
	ctx, cancel, req := m.entities.begin(m.opts.CallTimeout)
	be := m.be
	return func() tea.Msg {
		defer cancel()
		items, err := call(func() ([]bus.Entity, error) { return be.ListEntities(ctx, ns) })
		return entitiesLoadedMsg{req: req, ns: ns, items: items, err: err, cascade: cascade, keep: keep}
	}
}

// peek loads the first page of ent's sub-queue, replacing the list.
func (m *Model) peek(ns bus.Namespace, ent bus.Entity, sub bus.SubQueue) tea.Cmd {
	m.resetMessages()
	return m.peekFrom(ns, ent, sub, 0)
}

// resetMessages empties the message list and drops the cached main pane
// lines, which belong to the old list.
func (m *Model) resetMessages() {
	m.messages.reset()
	m.msgCols = msgCols{}
	m.hasAfterPage = false
	m.hasKeepIndex = false
	m.clearLines()
}

// resetMain puts the main pane back at its top: another message or tab.
func (m *Model) resetMain() {
	m.mainScroll = 0
	m.mainCursor = 0
}

func (m *Model) clearLines() {
	if m.lines != nil {
		*m.lines = linesCache{}
	}
}

// loadMore loads the page after the last message when the cursor is on
// the last row and the last page was not empty. Only an empty page ends
// paging: a short page can be followed by messages enqueued since.
func (m *Model) loadMore() tea.Cmd {
	l := &m.messages
	atEnd := len(l.items) > 0 && l.cursor == len(l.items)-1
	// A filter that hides every loaded row: the next page may hold matches.
	if len(l.items) == 0 && l.filter != "" && len(l.all) > 0 {
		atEnd = true
	}
	if !l.more || l.loading || !atEnd || m.openNS == nil || m.openEntity == nil {
		return nil
	}
	return m.peekFrom(*m.openNS, *m.openEntity, m.subQueue, l.all[len(l.all)-1].SequenceNumber+1)
}

func (m *Model) peekFrom(ns bus.Namespace, ent bus.Entity, sub bus.SubQueue, from int64) tea.Cmd {
	ctx, cancel, req := m.messages.begin(m.opts.CallTimeout)
	be := m.be
	return func() tea.Msg {
		defer cancel()
		items, err := call(func() ([]bus.Message, error) {
			return be.Peek(ctx, bus.PeekRequest{
				Namespace: ns, Entity: ent, SubQueue: sub, FromSequence: from, Max: bus.PageSize,
			})
		})
		return messagesLoadedMsg{req: req, ns: ns, ent: ent, sub: sub, from: from, items: items, err: err}
	}
}

func (m *Model) logf(isErr bool, format string, args ...any) {
	m.log = append(m.log, logEntry{at: m.opts.Now(), text: fmt.Sprintf(format, args...), err: isErr})
}

// errText is the log line for a failed call: a *bus.Error already names
// its operation.
func errText(op string, err error) string {
	var be *bus.Error
	if errors.As(err, &be) && be.Op != "" {
		return be.Error()
	}
	return op + " failed: " + err.Error()
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

	case configuredLoadedMsg:
		m, cmd = m.configuredLoaded(msg)

	case subscriptionsLoadedMsg:
		m, cmd = m.subscriptionsLoaded(msg)

	case subNamespacesLoadedMsg:
		m = m.subNamespacesLoaded(msg)

	case entitiesLoadedMsg:
		if msg.req != m.entities.req {
			break
		}
		m.entities.loading = false
		m.entities.cancel = nil
		var partial *bus.PartialError
		if errors.As(msg.err, &partial) {
			// A usable list with a part missing (Basic tier: no topics).
			m.logf(true, "%s", errText("list topics "+msg.ns.Name, partial.Err))
			msg.err = nil
		}
		m.entities.err = msg.err
		if msg.err != nil {
			m.logf(true, "%s", errText("list entities "+msg.ns.Name, msg.err))
			break
		}
		prev, hadPrev := m.entities.selected()
		m.entities.setAll(sortEntities(msg.items, m.sortDLQ), m.entityText)
		if msg.keep && hadPrev {
			m.selectEntity(prev)
		} else {
			m.entities.offset = 0
			m.entities.move(0)
		}
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
		m.messages.cancel = nil
		m.messages.err = msg.err
		label := entityLabel(msg.ent, msg.sub)
		if msg.err != nil {
			// A failed next page keeps the rows and `more`, so moving onto
			// the last row again retries.
			if m.isSessionful(msg.err) {
				m.logf(false, "peek %s: %s", label, shortErr(msg.err))
			} else {
				m.logf(true, "%s", errText("peek "+label, msg.err))
			}
			m.hasAfterPage, m.hasKeepIndex = false, false
			break
		}
		m.messages.more = len(msg.items) > 0
		if msg.from > 0 {
			// Appended in place, amortized: older Model copies only see
			// their own length, and only the current Model pages on.
			m.messages.setAll(append(m.messages.all, msg.items...), m.messageText)
			m.msgCols = m.msgCols.add(msg.items)
			m.logf(false, "peek %s from %d → %d", label, msg.from, len(msg.items))
			if m.hasAfterPage {
				m.hasAfterPage = false
				m.messages.move(m.afterPage)
				m.resetMain()
			}
			break
		}
		m.clearLines()
		// Clipped: the first append copies, so paging never writes into
		// spare capacity of the backend's slice.
		m.messages.setAll(msg.items[:len(msg.items):len(msg.items)], m.messageText)
		m.msgCols = msgCols{}.add(msg.items)
		// A first page replaces the list: a cursor move pending for a next
		// page no longer applies.
		m.hasAfterPage = false
		if m.hasKeepIndex {
			// A refresh: same index, now on whatever message is there.
			m.hasKeepIndex = false
			m.messages.move(m.keepIndex)
		} else {
			m.messages.offset = 0
			m.messages.move(0)
		}
		m.resetMain()
		m.logf(false, "peek %s → %d", label, len(msg.items))

	case planDoneMsg:
		m, cmd = m.planDone(msg)

	case repairDoneMsg:
		start := len(m.log)
		m, cmd = m.repairDone(msg)
		if m.quitAfterCall != nil {
			m.exit = m.exitReport(msg, start)
			cmd = m.quitAfterCall
		}

	case deferredQuitMsg:
		m.quitAfterCall = msg.quit
		m.setStatus(statusWarn, "quit after the call finishes")

	case editorDoneMsg:
		m = m.editorDone(msg)

	case tea.PasteMsg:
		if m.stack.Top() == CtxEdit {
			m = m.paste(msg.Content)
		}

	case spinner.TickMsg:
		// A tick after the busy popup closed ends the tick chain.
		if m.stack.Top() == CtxBusy {
			m.spin, cmd = m.spin.Update(msg)
		}

	case tea.KeyPressMsg:
		m, cmd = m.handleKey(msg)
	}
	m.clampScroll()
	return m, cmd
}

// handleKey routes a key to the top context only.
func (m Model) handleKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	if key.Matches(msg, keys.ForceQuit) && !m.callRunning() {
		return m, tea.Quit
	}
	switch m.stack.Top() {
	case CtxBusy:
		// A call is running. Keys wait; a Repair or Finish Cleanup must
		// finish even on ctrl-c, so its outcome (CleanupPending included)
		// is never lost.
		return m, nil
	case CtxConfirm:
		return m.handleConfirmKey(msg)
	}
	m.status = statusLine{}
	switch m.stack.Top() {
	case CtxHelp:
		return m.handleHelpKey(msg)
	case CtxFilter:
		return m.handleFilterKey(msg)
	case CtxJump:
		return m.handleJumpKey(msg)
	case CtxEdit:
		return m.handleEditKey(msg)
	}
	return m.handlePanelKey(msg)
}

func (m Model) handleHelpKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	switch {
	case key.Matches(msg, keys.HelpClose):
		// esc clears a non-empty filter first; esc on an empty one (or
		// enter) closes the menu.
		if msg.String() == "esc" && m.help.filter != "" {
			m.help = helpState{}
			break
		}
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
	if m.isMove(msg) {
		m.move(cur, msg)
		if cur == CtxMessages {
			cmd := m.loadMore()
			return m, cmd
		}
		return m, nil
	}
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
		m.resetMain()
	case key.Matches(msg, keys.NextTab):
		m.tab = (m.tab + 1) % tabCount
		m.resetMain()
	case key.Matches(msg, keys.Back):
		if cur == CtxMain {
			m.focus(m.lastSide)
		} else if m.clearFilter(cur) {
			m.setStatus(statusInfo, "filter cleared")
		}
	case key.Matches(msg, keys.Filter):
		return m.startFilter(cur)
	case key.Matches(msg, keys.Jump):
		return m.startJump()
	case key.Matches(msg, keys.Refresh):
		return m.refresh(cur)
	case key.Matches(msg, keys.Copy):
		return m.copySelected(cur)
	case key.Matches(msg, keys.CopyID):
		return m.copyMessageID()
	case cur == CtxEntities && key.Matches(msg, keys.Sort):
		m.toggleSort()
	case cur == CtxMessages && key.Matches(msg, keys.SubQueue):
		if m.openNS != nil && m.openEntity != nil {
			if m.subQueue == bus.DeadLetter {
				m.subQueue = bus.Active
			} else {
				m.subQueue = bus.DeadLetter
			}
			cmd := m.peek(*m.openNS, *m.openEntity, m.subQueue)
			return m, cmd
		}
	case (cur == CtxMessages || cur == CtxMain) && key.Matches(msg, keys.Resubmit):
		return m.startRepair(confirmRepair)
	case (cur == CtxMessages || cur == CtxMain) && key.Matches(msg, keys.FinishCleanup):
		return m.startRepair(confirmCleanup)
	case (cur == CtxMessages || cur == CtxMain) && key.Matches(msg, keys.Discard):
		return m.startDiscard()
	case cur == CtxMessages && key.Matches(msg, keys.EditBody),
		cur == CtxMain && m.tab == tabBody && key.Matches(msg, keys.EditBody):
		return m.startBodyEdit()
	case cur == CtxMessages && key.Matches(msg, keys.AddPropMsg):
		return m.startAddProperty()
	case cur == CtxMain && m.tab == tabProperties && key.Matches(msg, keys.EditProp):
		return m.startEditProperty()
	case cur == CtxMain && m.tab == tabProperties && key.Matches(msg, keys.AddProp):
		return m.startAddProperty()
	case cur == CtxMain && m.tab == tabProperties && key.Matches(msg, keys.RemoveProp):
		return m.toggleRemove()
	case cur == CtxMain && m.tab == tabSystem && key.Matches(msg, keys.EditField):
		return m.startEditField()
	case cur == CtxMain && key.Matches(msg, keys.EditBody, keys.EditProp, keys.AddProp, keys.RemoveProp):
		m.setStatus(statusInfo, "E edits the body (Body tab); e edits a property (Properties) or Subject / ContentType (System); a adds, d removes a property")
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
		r, ok := m.namespaces.selected()
		if !ok {
			return m, nil
		}
		switch r.kind {
		case rowSubLoading:
			m.setStatus(statusInfo, "still listing the namespaces of %s", r.sub.Name)
			return m, nil
		case rowSubFailed:
			m.setStatus(statusWarn, "listing the namespaces of %s failed (see log); R retries", r.sub.Name)
			return m, nil
		}
		ns := r.ns
		m.openNS = &ns
		m.openEntity = nil
		m.entities.reset()
		m.resetMessages()
		m.focus(CtxEntities)
		cmd := m.loadEntities(ns, false, false)
		return m, cmd
	case CtxEntities:
		ent, ok := m.entities.selected()
		if !ok || m.openNS == nil {
			return m, nil
		}
		return m.openEntityMessages(ent)
	case CtxMessages:
		m.focus(CtxMain)
	}
	return m, nil
}

// isMove reports whether msg is a cursor movement key.
func (m Model) isMove(msg tea.KeyPressMsg) bool {
	return key.Matches(msg, keys.Down, keys.Up, keys.HalfDown, keys.HalfUp, keys.Top, keys.Bottom)
}

// move applies a movement key to context c.
func (m *Model) move(c ContextID, msg tea.KeyPressMsg) {
	switch {
	case key.Matches(msg, keys.Down):
		m.moveBy(c, 1)
	case key.Matches(msg, keys.Up):
		m.moveBy(c, -1)
	case key.Matches(msg, keys.HalfDown):
		m.moveBy(c, max(1, m.pageRows(c)/2))
	case key.Matches(msg, keys.HalfUp):
		m.moveBy(c, -max(1, m.pageRows(c)/2))
	case key.Matches(msg, keys.Top):
		m.moveTo(c, 0)
	case key.Matches(msg, keys.Bottom):
		m.moveTo(c, 1<<30)
	}
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
			m.resetMain()
		}
	case CtxMain:
		if m.tab == tabBody {
			m.mainScroll += d
		} else {
			m.mainCursor += d
		}
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
			m.resetMain()
		}
	case CtxMain:
		if m.tab == tabBody {
			m.mainScroll = i
		} else {
			m.mainCursor = i
		}
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
	lines, rows := m.mainContent(l.mainW - 2)
	if len(rows) > 0 {
		// Properties and System: the scroll follows the cursor.
		m.mainCursor = max(0, min(m.mainCursor, len(rows)-1))
		at := rows[m.mainCursor]
		if at < m.mainScroll {
			m.mainScroll = at
		}
		if at >= m.mainScroll+l.mainRows {
			m.mainScroll = at - l.mainRows + 1
		}
	} else {
		m.mainCursor = 0
	}
	maxScroll := max(0, len(lines)-l.mainRows)
	m.mainScroll = max(0, min(m.mainScroll, maxScroll))
	switch m.stack.Top() {
	case CtxHelp:
		_, _, n := m.helpBox()
		m.help.offset = max(0, min(m.help.offset, len(m.helpEntries())-n))
	case CtxJump:
		m.jump.cursor = max(0, min(m.jump.cursor, len(m.jumpMatches())-1))
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

// selectedMessage is the message under the cursor. While a next page
// loads, the rows (and the selection) stay.
func (m Model) selectedMessage() (bus.Message, bool) {
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
	if root.isSide() {
		add("Filter (/)", contextBindings(CtxFilter))
	}
	add("Jump popup (:)", contextBindings(CtxJump))
	if root == CtxMessages || root == CtxMain {
		add("Confirm popup", confirmBindings())
		add("Edit popup", contextBindings(CtxEdit))
		if note := "kept in memory only: lost on quit"; m.help.filter == "" ||
			strings.Contains("pending edits "+note, strings.ToLower(m.help.filter)) {
			out = append(out, helpLine{section: "Pending Edits"}, helpLine{desc: note})
		}
	}
	add("Global", globalBindings())
	return out
}

type helpLine struct {
	section   string
	key, desc string
}
