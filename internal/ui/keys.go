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

	Open        key.Binding // Namespaces, Entities
	FocusMainOp key.Binding // Messages: enter
	SubQueue    key.Binding // Messages: tab

	HelpUp    key.Binding
	HelpDown  key.Binding
	HelpClose key.Binding
	HelpErase key.Binding
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

	Open:        key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "open")),
	FocusMainOp: key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "focus main pane")),
	SubQueue:    key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "DLQ / Active")),

	HelpUp:    key.NewBinding(key.WithKeys("up", "ctrl+p"), key.WithHelp("↑", "scroll up")),
	HelpDown:  key.NewBinding(key.WithKeys("down", "ctrl+n"), key.WithHelp("↓", "scroll down")),
	HelpClose: key.NewBinding(key.WithKeys("esc", "enter"), key.WithHelp("esc", "close")),
	HelpErase: key.NewBinding(key.WithKeys("backspace"), key.WithHelp("backspace", "erase")),
}

// contextBindings are the keys specific to one context.
func contextBindings(c ContextID) []key.Binding {
	switch c {
	case CtxNamespaces, CtxEntities:
		return []key.Binding{keys.Open}
	case CtxMessages:
		return []key.Binding{keys.FocusMainOp, keys.SubQueue}
	case CtxMain:
		return []key.Binding{keys.Back}
	case CtxHelp:
		return []key.Binding{keys.HelpUp, keys.HelpDown, keys.HelpErase, keys.HelpClose}
	}
	return nil
}

// globalBindings are the keys every panel context accepts.
func globalBindings() []key.Binding {
	return []key.Binding{
		keys.FocusNamespaces, keys.FocusEntities, keys.FocusMessages, keys.FocusMain,
		keys.PrevPanel, keys.NextPanel, keys.Down, keys.Up, keys.Top, keys.Bottom,
		keys.HalfDown, keys.HalfUp, keys.PrevTab, keys.NextTab, keys.Help, keys.Quit,
	}
}

// optionsBindings are the keys shown in the options bar for context c.
func optionsBindings(c ContextID) []key.Binding {
	if c == CtxHelp {
		return []key.Binding{keys.HelpClose, keys.HelpUp, keys.HelpDown}
	}
	tab := key.NewBinding(key.WithKeys("[", "]"), key.WithHelp("[ ]", "tab"))
	return append(contextBindings(c), tab, keys.Help, keys.Quit)
}
