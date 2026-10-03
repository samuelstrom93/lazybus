package ui

import "testing"

func TestContextStack(t *testing.T) {
	s := NewContextStack(CtxNamespaces)
	if s.Top() != CtxNamespaces || !s.AtRoot() {
		t.Fatalf("new stack: top %v, atRoot %v", s.Top(), s.AtRoot())
	}
	if s.Pop() {
		t.Fatal("Pop at root closed something")
	}

	s.Push(CtxHelp)
	if s.Top() != CtxHelp || s.Root() != CtxNamespaces || s.AtRoot() {
		t.Fatalf("after push: top %v, root %v, atRoot %v", s.Top(), s.Root(), s.AtRoot())
	}

	// A copy taken before Pop must not see later pushes (value semantics in
	// Bubble Tea models).
	before := s
	if !s.Pop() || s.Top() != CtxNamespaces {
		t.Fatalf("pop: top %v", s.Top())
	}
	s.Push(CtxMain)
	if before.Top() != CtxHelp {
		t.Fatalf("copy changed: top %v", before.Top())
	}

	s.Focus(CtxMessages)
	if s.Len() != 1 || s.Top() != CtxMessages {
		t.Fatalf("focus: len %d, top %v", s.Len(), s.Top())
	}
}
