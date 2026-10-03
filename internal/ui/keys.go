package ui

import "charm.land/bubbles/v2/key"

// keyMap holds every binding lazybus knows in this build. Only bindings that
// are implemented are listed, so the options bar and the `?` menu never
// advertise a key that does nothing.
type keyMap struct {
	FocusNamespaces key.Binding
	FocusEntities   key.Binding
	FocusMessages   key.Binding
	FocusMain       key.Binding
	PrevPanel       key.Binding
	NextPanel       key.Binding
	Down            key.Binding
	Up              key.Binding
	Top             key.Binding
	Bottom          key.Binding
	HalfDown        key.Binding
	HalfUp          key.Binding
	PrevTab         key.Binding
	NextTab         key.Binding
	Help            key.Binding
	Back            key.Binding
	Quit            key.Binding
	ForceQuit       key.Binding
	Filter          key.Binding
	Jump            key.Binding
	Refresh         key.Binding
	Copy            key.Binding
	CopyID          key.Binding

	Open          key.Binding // Namespaces, Entities
	FocusMainOp   key.Binding // Messages: enter
	SubQueue      key.Binding // Messages: tab
	Resubmit      key.Binding // Messages, Main: r
	FinishCleanup key.Binding // Messages, Main: c
	Sort          key.Binding // Entities: s

	Confirm  key.Binding // destructive popups: y only (spec §2)
	Cancel   key.Binding
	ToggleID key.Binding // resubmit popup: m

	HelpUp    key.Binding
	HelpDown  key.Binding
	HelpClose key.Binding
	HelpErase key.Binding

	FilterKeep  key.Binding // / input: enter
	FilterClear key.Binding // / input: esc
	FilterMove  key.Binding // / input: ↑ ↓ (help only)
	JumpOpen    key.Binding
	JumpCancel  key.Binding
	JumpUp      key.Binding
	JumpDown    key.Binding
}

var keys = keyMap{
	FocusNamespaces: key.NewBinding(key.WithKeys("1"), key.WithHelp("1", "focus Namespaces")),
	FocusEntities:   key.NewBinding(key.WithKeys("2"), key.WithHelp("2", "focus Entities")),
	FocusMessages:   key.NewBinding(key.WithKeys("3"), key.WithHelp("3", "focus Messages")),
	FocusMain:       key.NewBinding(key.WithKeys("0"), key.WithHelp("0", "focus main pane")),
	PrevPanel:       key.NewBinding(key.WithKeys("h", "left"), key.WithHelp("h ←", "previous panel")),
	NextPanel:       key.NewBinding(key.WithKeys("l", "right"), key.WithHelp("l →", "next panel")),
	Down:            key.NewBinding(key.WithKeys("j", "down"), key.WithHelp("j ↓", "move down")),
	Up:              key.NewBinding(key.WithKeys("k", "up"), key.WithHelp("k ↑", "move up")),
	Top:             key.NewBinding(key.WithKeys("g", "home"), key.WithHelp("g", "top")),
	Bottom:          key.NewBinding(key.WithKeys("G", "end"), key.WithHelp("G", "bottom")),
	HalfDown:        key.NewBinding(key.WithKeys("ctrl+d"), key.WithHelp("ctrl-d", "half page down")),
	HalfUp:          key.NewBinding(key.WithKeys("ctrl+u"), key.WithHelp("ctrl-u", "half page up")),
	PrevTab:         key.NewBinding(key.WithKeys("["), key.WithHelp("[", "previous main tab")),
	NextTab:         key.NewBinding(key.WithKeys("]"), key.WithHelp("]", "next main tab")),
	Help:            key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "keybindings")),
	Back:            key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back")),
	Quit:            key.NewBinding(key.WithKeys("q"), key.WithHelp("q", "quit")),
	ForceQuit:       key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl-c", "quit")),
	Filter:          key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter")),
	Jump:            key.NewBinding(key.WithKeys(":"), key.WithHelp(":", "jump")),
	Refresh:         key.NewBinding(key.WithKeys("R"), key.WithHelp("R", "refresh panel")),
	Copy:            key.NewBinding(key.WithKeys("y"), key.WithHelp("y", "copy body / value / field")),
	CopyID:          key.NewBinding(key.WithKeys("Y"), key.WithHelp("Y", "copy MessageId")),

	Open:        key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "open")),
	FocusMainOp: key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "focus main pane")),
	SubQueue:    key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "DLQ / Active")),

	Resubmit:      key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "resubmit")),
	FinishCleanup: key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "finish cleanup")),
	Sort:          key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "sort path / DLQ")),

	Confirm:  key.NewBinding(key.WithKeys("y"), key.WithHelp("y", "confirm (enter does nothing)")),
	Cancel:   key.NewBinding(key.WithKeys("n", "esc"), key.WithHelp("n esc", "cancel")),
	ToggleID: key.NewBinding(key.WithKeys("m"), key.WithHelp("m", "MessageId keep/new")),

	HelpUp:    key.NewBinding(key.WithKeys("up", "ctrl+p"), key.WithHelp("↑", "scroll up")),
	HelpDown:  key.NewBinding(key.WithKeys("down", "ctrl+n"), key.WithHelp("↓", "scroll down")),
	HelpClose: key.NewBinding(key.WithKeys("esc", "enter"), key.WithHelp("esc", "close")),
	HelpErase: key.NewBinding(key.WithKeys("backspace"), key.WithHelp("backspace", "erase")),

	FilterKeep:  key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "keep filter")),
	FilterClear: key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "clear filter")),
	FilterMove:  key.NewBinding(key.WithKeys("up", "down"), key.WithHelp("↑ ↓", "move")),
	JumpOpen:    key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "open")),
	JumpCancel:  key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "cancel")),
	JumpUp:      key.NewBinding(key.WithKeys("up", "ctrl+p"), key.WithHelp("↑", "previous")),
	JumpDown:    key.NewBinding(key.WithKeys("down", "ctrl+n"), key.WithHelp("↓", "next")),
}

// contextBindings are the keys specific to one context.
func contextBindings(c ContextID) []key.Binding {
	switch c {
	case CtxNamespaces:
		return []key.Binding{keys.Open}
	case CtxEntities:
		return []key.Binding{keys.Open, keys.Sort}
	case CtxMessages:
		return []key.Binding{keys.FocusMainOp, keys.SubQueue, keys.Resubmit, keys.FinishCleanup}
	case CtxMain:
		return []key.Binding{keys.Back, keys.Resubmit, keys.FinishCleanup}
	case CtxHelp:
		return []key.Binding{keys.HelpUp, keys.HelpDown, keys.HelpErase, keys.HelpClose}
	case CtxFilter:
		return []key.Binding{keys.FilterKeep, keys.FilterClear, keys.FilterMove}
	case CtxJump:
		return []key.Binding{keys.JumpOpen, keys.JumpCancel, keys.JumpUp, keys.JumpDown}
	}
	return nil
}

// confirmBindings are the keys of the Resubmit and Finish Cleanup confirm
// popups, listed in the help menu where r and c work.
func confirmBindings() []key.Binding {
	return []key.Binding{keys.Confirm, keys.Cancel, keys.ToggleID}
}

// globalBindings are the keys every panel context accepts.
func globalBindings() []key.Binding {
	return []key.Binding{
		keys.FocusNamespaces, keys.FocusEntities, keys.FocusMessages, keys.FocusMain,
		keys.PrevPanel, keys.NextPanel, keys.Down, keys.Up, keys.Top, keys.Bottom,
		keys.HalfDown, keys.HalfUp, keys.PrevTab, keys.NextTab,
		keys.Filter, keys.Jump, keys.Refresh, keys.Copy, keys.CopyID, keys.Help, keys.Quit,
	}
}

// optionsBindings are the keys shown in the options bar for context c.
// repair shows `r` (DLQ tab, not --read-only); cleanup shows `c`, which
// only acts on a CleanupPending row.
func optionsBindings(c ContextID, repair, cleanup bool) []key.Binding {
	switch c {
	case CtxHelp:
		return []key.Binding{keys.HelpClose, keys.HelpUp, keys.HelpDown}
	case CtxConfirm:
		return []key.Binding{keys.Confirm, keys.Cancel}
	case CtxBusy:
		return nil
	}
	var out []key.Binding
	for _, b := range contextBindings(c) {
		switch b.Help().Key {
		case keys.Resubmit.Help().Key:
			if !repair {
				continue
			}
		case keys.FinishCleanup.Help().Key:
			if !cleanup {
				continue
			}
		}
		out = append(out, b)
	}
	tab := key.NewBinding(key.WithKeys("[", "]"), key.WithHelp("[ ]", "tab"))
	out = append(out, tab)
	if c.isSide() {
		out = append(out, keys.Filter)
	}
	return append(out, keys.Jump, keys.Help, keys.Quit)
}
