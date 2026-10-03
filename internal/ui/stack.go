package ui

// ContextID names one UI context: a side panel, the main pane, or a popup.
type ContextID int

const (
	CtxNamespaces ContextID = iota
	CtxEntities
	CtxMessages
	CtxMain
	CtxHelp
	CtxConfirm // DLQ Repair / Finish Cleanup confirm popup
	CtxBusy    // a broker call that changes state (or its pre-check) is running
)

func (c ContextID) String() string {
	switch c {
	case CtxNamespaces:
		return "Namespaces"
	case CtxEntities:
		return "Entities"
	case CtxMessages:
		return "Messages"
	case CtxMain:
		return "Main"
	case CtxHelp:
		return "Keybindings"
	case CtxConfirm:
		return "Confirm"
	case CtxBusy:
		return "Working"
	}
	return "Unknown"
}

// isSide reports whether c is one of the three numbered side panels.
func (c ContextID) isSide() bool {
	return c == CtxNamespaces || c == CtxEntities || c == CtxMessages
}

// ContextStack is the ordered set of UI contexts. The root is the focused
// panel (side panel or main pane); popups and menus are pushed on top. Only
// the top context receives keys, and esc pops one level.
type ContextStack struct {
	items []ContextID
}

// NewContextStack returns a stack with root as its only context.
func NewContextStack(root ContextID) ContextStack {
	return ContextStack{items: []ContextID{root}}
}

// Top is the context that owns the keys.
func (s ContextStack) Top() ContextID { return s.items[len(s.items)-1] }

// Root is the focused panel underneath any popups.
func (s ContextStack) Root() ContextID { return s.items[0] }

// AtRoot reports whether no popup is open.
func (s ContextStack) AtRoot() bool { return len(s.items) == 1 }

// Len is the number of contexts on the stack.
func (s ContextStack) Len() int { return len(s.items) }

// Push opens c on top of the stack.
func (s *ContextStack) Push(c ContextID) {
	items := make([]ContextID, len(s.items), len(s.items)+1)
	copy(items, s.items)
	s.items = append(items, c)
}

// Pop closes the top context. The root is never popped; Pop reports
// whether anything was closed.
func (s *ContextStack) Pop() bool {
	if len(s.items) <= 1 {
		return false
	}
	s.items = s.items[: len(s.items)-1 : len(s.items)-1]
	return true
}

// Focus replaces the root with c and closes every popup.
func (s *ContextStack) Focus(c ContextID) {
	s.items = []ContextID{c}
}
