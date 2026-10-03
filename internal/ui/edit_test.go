package ui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/samuelstrom93/lazybus/internal/bus"
	"github.com/samuelstrom93/lazybus/internal/bus/fake"
)

// typeText types s into the focused field, one key per rune.
func typeText(t *testing.T, m Model, s string) Model {
	t.Helper()
	for _, r := range s {
		m = run(t, m, press(string(r)))
	}
	return m
}

// onProperties opens orders/$DLQ seq 2 (all seven property types) on the
// Properties tab with the main pane focused.
func onProperties(t *testing.T, be *fake.Backend) Model {
	t.Helper()
	return keysIn(t, onOrders(t, be), "0", "]")
}

func editsOfSelected(t *testing.T, m Model) bus.Edits {
	t.Helper()
	return m.editsOf(selectedSeq(t, m)).Edits
}

func TestPropertyEditPopupChangeKeepsType(t *testing.T) {
	m := keysIn(t, onProperties(t, fake.New()), "j", "e") // orderId, Long 4002
	if m.stack.Top() != CtxEdit || m.edit.key != "orderId" || m.edit.typ != bus.TypeLong || m.edit.value != "4002" || m.edit.field != fieldValue {
		t.Fatalf("popup not prefilled: top %v %+v", m.stack.Top(), m.edit)
	}
	// An invalid value keeps the popup open with the error.
	m = typeText(t, keysIn(t, m, "ctrl+u"), "12x")
	m = keysIn(t, m, "enter")
	if m.stack.Top() != CtxEdit || !strings.Contains(m.edit.err, `Long: "12x" is not a whole number`) {
		t.Fatalf("invalid value: top %v err %q", m.stack.Top(), m.edit.err)
	}
	if !strings.Contains(screenText(m), `! Long: "12x" is not a whole number`) {
		t.Fatalf("error not shown:\n%s", screenText(m))
	}
	m = keysIn(t, m, "backspace", "enter")
	if !m.stack.AtRoot() {
		t.Fatalf("valid value did not close the popup: %q", m.edit.err)
	}
	pe, ok := editsOfSelected(t, m).Property("orderId")
	if !ok || pe.Type != bus.TypeLong || pe.Value != int64(12) {
		t.Fatalf("edit = %+v", pe)
	}
	s := screenText(m)
	if !strings.Contains(s, "*orderId") || !strings.Contains(s, "12  (was 4002)") || !strings.Contains(s, "✎ 1 pending") {
		t.Fatalf("Properties tab lacks the * marker or pending flag:\n%s", s)
	}

	// The type selector: ← → pick another type; the edit names it.
	m = keysIn(t, m, "e", "shift+tab", "left")
	if m.edit.field != fieldType || m.edit.typ != bus.TypeInt {
		t.Fatalf("type selector: field %v type %v", m.edit.field, m.edit.typ)
	}
	m = keysIn(t, m, "enter")
	if pe, _ := editsOfSelected(t, m).Property("orderId"); pe.Type != bus.TypeInt || pe.Value != int32(12) {
		t.Fatalf("type change: %+v", pe)
	}
	// Back to the original type and value: no Pending Edit.
	m = keysIn(t, m, "e", "shift+tab", "right", "tab", "ctrl+u")
	m = typeText(t, m, "4002")
	m = keysIn(t, m, "enter")
	if !editsOfSelected(t, m).IsZero() || m.pendingCount() != 0 {
		t.Fatalf("original value kept an edit: %+v", editsOfSelected(t, m))
	}
	// esc cancels without a change.
	m = keysIn(t, m, "e", "ctrl+u", "esc")
	if !m.stack.AtRoot() || !editsOfSelected(t, m).IsZero() {
		t.Fatal("esc changed something")
	}
}

func TestPropertyAddAndRemove(t *testing.T) {
	m := onProperties(t, fake.New())
	m = typeText(t, keysIn(t, m, "a"), "region")
	if m.edit.typ != bus.TypeString || m.edit.field != fieldKey {
		t.Fatalf("new key: %+v", m.edit)
	}
	m = typeText(t, keysIn(t, m, "tab", "tab"), "eu-north")
	m = keysIn(t, m, "enter")
	if pe, ok := editsOfSelected(t, m).Property("region"); !ok || pe.Type != bus.TypeString || pe.Value != "eu-north" {
		t.Fatalf("add: %+v", pe)
	}

	// An add of an existing key takes its type and becomes a change.
	m = typeText(t, keysIn(t, m, "a"), "attempt")
	if m.edit.typ != bus.TypeInt {
		t.Fatalf("existing key did not keep its type: %v", m.edit.typ)
	}
	m = typeText(t, keysIn(t, m, "tab", "tab"), "9")
	m = keysIn(t, m, "enter")
	if pe, _ := editsOfSelected(t, m).Property("attempt"); pe.Type != bus.TypeInt || pe.Value != int32(9) {
		t.Fatalf("duplicate-key add: %+v", pe)
	}

	// d toggles removal: tenant is the first row.
	m = keysIn(t, m, "g", "d")
	if pe, _ := editsOfSelected(t, m).Property("tenant"); !pe.Remove {
		t.Fatalf("d: %+v", pe)
	}
	s := screenText(m)
	for _, want := range []string{"−tenant", "+region", "*attempt", "✕ removed on resubmit"} {
		if !strings.Contains(s, want) {
			t.Fatalf("Properties tab lacks %q:\n%s", want, s)
		}
	}
	m = keysIn(t, m, "d")
	if _, ok := editsOfSelected(t, m).Property("tenant"); ok {
		t.Fatal("second d did not undo the removal")
	}
	// d on a pending add drops it.
	m = keysIn(t, m, "G", "k", "k", "d") // region: after the 7 properties, before the 2 markers
	if _, ok := editsOfSelected(t, m).Property("region"); ok {
		t.Fatalf("d on +region did not drop it: %+v (status %q)", editsOfSelected(t, m), m.status.text)
	}
}

func TestMarkersAreNeverEdited(t *testing.T) {
	m := keysIn(t, onProperties(t, fake.New()), "G") // DeadLetterErrorDescription
	for _, k := range []string{"e", "d"} {
		m = keysIn(t, m, k)
		if !m.stack.AtRoot() || !strings.Contains(m.status.text, "dead-letter markers are removed on resubmit") {
			t.Fatalf("%s on a marker: top %v status %q", k, m.stack.Top(), m.status.text)
		}
	}
	m = typeText(t, keysIn(t, m, "a"), bus.MarkerDeadLetterReason)
	m = keysIn(t, m, "enter")
	if m.stack.Top() != CtxEdit || !strings.Contains(m.edit.err, "can't be edited") {
		t.Fatalf("add of a marker key: top %v err %q", m.stack.Top(), m.edit.err)
	}
	if !editsOfSelected(t, m).IsZero() {
		t.Fatal("marker edit stored")
	}
}

func TestSystemEditSubjectOnly(t *testing.T) {
	m := keysIn(t, onOrders(t, fake.New()), "0", "]", "]", "e") // MessageId row
	if !m.stack.AtRoot() || !strings.Contains(m.status.text, "only Subject and ContentType") {
		t.Fatalf("e on MessageId: top %v status %q", m.stack.Top(), m.status.text)
	}
	m = keysIn(t, m, "j", "j", "e")
	if m.stack.Top() != CtxEdit || m.edit.kind != editSubject || m.edit.value != "OrderPlaced" {
		t.Fatalf("e on Subject: %+v", m.edit)
	}
	m = typeText(t, keysIn(t, m, "ctrl+u"), "OrderFixed")
	m = keysIn(t, m, "enter")
	if ed := editsOfSelected(t, m); ed.Subject == nil || *ed.Subject != "OrderFixed" {
		t.Fatalf("subject edit: %+v", ed)
	}
	if s := screenText(m); !strings.Contains(s, "*Subject ✎") || !strings.Contains(s, "OrderFixed  (was OrderPlaced)") {
		t.Fatalf("System tab:\n%s", s)
	}
}

func TestMessagesEAddsWithEmptyKey(t *testing.T) {
	m := keysIn(t, onOrders(t, fake.New()), "e")
	if m.stack.Top() != CtxEdit || !m.edit.adding || m.edit.key != "" || m.edit.field != fieldKey {
		t.Fatalf("e on Messages: %+v", m.edit)
	}
	m = keysIn(t, m, "enter")
	if m.edit.err != "key is empty" {
		t.Fatalf("empty key: %q", m.edit.err)
	}
	m = typeText(t, m, "k")
	m = typeText(t, keysIn(t, m, "tab", "tab"), "v")
	m = keysIn(t, m, "enter")
	if !m.stack.AtRoot() || m.tab != tabProperties || m.stack.Root() != CtxMessages {
		t.Fatalf("after save: top %v tab %v root %v", m.stack.Top(), m.tab, m.stack.Root())
	}
}

func TestEditKeysRefusedOnActive(t *testing.T) {
	m := keysIn(t, onOrders(t, fake.New()), "tab")
	for _, k := range []string{"e", "E", "x"} {
		m = keysIn(t, m, k)
		if !m.stack.AtRoot() || m.status.text != activeRefusal {
			t.Fatalf("%s on Active: top %v status %q", k, m.stack.Top(), m.status.text)
		}
	}
}

func TestReadOnlyAllowsLocalEdits(t *testing.T) {
	opts := testOptions()
	opts.ReadOnly = true
	m := New(fake.New(), opts)
	m = run(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
	m = runCmd(t, m, m.Init())
	m = keysIn(t, m, "2", "j", "j", "j", "enter", "e", "k", "tab", "tab", "v", "enter")
	if editsOfSelected(t, m).IsZero() || !strings.Contains(m.status.text, "read-only: r is disabled") {
		t.Fatalf("read-only edit: %+v status %q", editsOfSelected(t, m), m.status.text)
	}
	m = keysIn(t, m, "r")
	if !m.stack.AtRoot() || !strings.Contains(m.status.text, "r is disabled") {
		t.Fatalf("r in read-only: %q", m.status.text)
	}
}

func TestDiscardPendingEdits(t *testing.T) {
	m := onOrders(t, fake.New())
	m = keysIn(t, m, "x")
	if !m.stack.AtRoot() || !strings.Contains(m.status.text, "nothing to discard") {
		t.Fatalf("x without edits: %q", m.status.text)
	}
	m = keysIn(t, m, "e", "k", "tab", "tab", "v", "enter", "x")
	if m.stack.Top() != CtxConfirm || m.confirm.kind != confirmDiscard {
		t.Fatalf("x did not open the confirm popup: %v", m.stack.Top())
	}
	if s := screenText(m); !strings.Contains(s, "Discard Pending Edits") || !strings.Contains(s, "+ k: String v") {
		t.Fatalf("discard popup:\n%s", s)
	}
	m = keysIn(t, m, "enter") // does nothing in a destructive popup
	m = keysIn(t, m, "n")
	if editsOfSelected(t, m).IsZero() || !m.stack.AtRoot() {
		t.Fatal("n discarded the edits")
	}
	m = keysIn(t, m, "x", "y")
	if !editsOfSelected(t, m).IsZero() || m.pendingCount() != 0 || !strings.Contains(m.status.text, "discarded 1 Pending Edit of orders/$DLQ seq 2") {
		t.Fatalf("y kept the edits: %q", m.status.text)
	}
}

func TestRepairConsumesPendingEdits(t *testing.T) {
	be := fake.New()
	m := onProperties(t, be)
	m = keysIn(t, m, "j", "e", "ctrl+u", "7", "enter") // orderId Long 7
	m = keysIn(t, m, "g", "d")                         // remove tenant
	m = keysIn(t, m, "]", "j", "j", "e", "ctrl+u")     // Subject
	m = typeText(t, m, "OrderFixed")
	m = keysIn(t, m, "enter")
	seq := selectedSeq(t, m)

	// LockLost keeps the row and its Pending Edits.
	be.SetLockDuration(5e9)
	m = keysIn(t, m, "r")
	s := screenText(m)
	for _, want := range []string{"* orderId: Long 4002 → Long 7", "− tenant (was String contoso)", "Subject     OrderPlaced → OrderFixed"} {
		if !strings.Contains(s, want) {
			t.Fatalf("confirm popup lacks %q:\n%s", want, s)
		}
	}
	m = keysIn(t, m, "y")
	if m.messages.items[0].SequenceNumber != seq || m.editsOf(seq).Count() != 3 {
		t.Fatalf("LockLost: row %d, edits %d", m.messages.items[0].SequenceNumber, m.editsOf(seq).Count())
	}

	be.SetLockDuration(60e9)
	before := len(be.Messages("sb-prod-weu", "orders", bus.KindQueue, bus.Active))
	m = keysIn(t, m, "r", "y")
	if !strings.Contains(m.status.text, "(3 edits applied)") || m.pendingCount() != 0 {
		t.Fatalf("after resubmit: status %q pending %d", m.status.text, m.pendingCount())
	}
	got := be.Messages("sb-prod-weu", "orders", bus.KindQueue, bus.Active)
	if len(got) != before+1 {
		t.Fatalf("target active %d → %d", before, len(got))
	}
	cp := got[len(got)-1]
	if cp.Subject != "OrderFixed" || len(cp.Properties) != 6 || cp.Properties[0].Key != "orderId" || cp.Properties[0].Value != int64(7) {
		t.Fatalf("copy = %+v", cp)
	}
}

func TestRepairNotFoundDropsPendingEdits(t *testing.T) {
	be := fake.New()
	m := keysIn(t, onOrders(t, be), "e", "k", "tab", "tab", "v", "enter")
	removeOutOfBand(t, be, m, selectedSeq(t, m))
	m = keysIn(t, m, "r")
	if m.pendingCount() != 0 {
		t.Fatalf("NotFound (pre-check) kept %d pending", m.pendingCount())
	}
}

// fakeEditor makes the editor write content into the file it is given,
// recording what lazybus wrote first.
func fakeEditor(t *testing.T, content string, written *string) {
	t.Helper()
	prev := execProcess
	t.Cleanup(func() { execProcess = prev })
	execProcess = func(c *exec.Cmd, fn tea.ExecCallback) tea.Cmd {
		return func() tea.Msg {
			path := c.Args[len(c.Args)-1]
			b, err := os.ReadFile(path)
			if err != nil {
				return fn(err)
			}
			if written != nil {
				*written = string(b)
			}
			return fn(os.WriteFile(path, []byte(content), 0o600))
		}
	}
}

func TestBodyEditor(t *testing.T) {
	var written string
	fakeEditor(t, "{\n  \"orderId\": 1\n}\n", &written)
	m := keysIn(t, onOrders(t, fake.New()), "E")
	if !strings.HasPrefix(written, "{\n  \"orderId\": 4002,\n  \"status\": \"pending\"") || strings.HasSuffix(written, "\n") {
		t.Fatalf("the editor got %q, want pretty JSON without a final newline", written)
	}
	ed := editsOfSelected(t, m)
	if !ed.BodyEdited || string(ed.Body) != "{\n  \"orderId\": 1\n}" {
		t.Fatalf("pending body = %q (the editor's final newline is dropped)", ed.Body)
	}
	if s := screenText(m); !strings.Contains(s, "(edited) pending body") || strings.Contains(s, "invalid JSON") {
		t.Fatalf("Body tab:\n%s", s)
	}

	// E re-opens the pending body as is; saving it unchanged keeps it.
	fakeEditor(t, "{\n  \"orderId\": 1\n}", &written)
	m = keysIn(t, m, "E")
	if written != "{\n  \"orderId\": 1\n}" || !strings.Contains(m.status.text, "body unchanged") || !editsOfSelected(t, m).BodyEdited {
		t.Fatalf("re-open: wrote %q, status %q", written, m.status.text)
	}

	// Invalid JSON for a JSON message is kept, and flagged.
	fakeEditor(t, `{"orderId": 1`, nil)
	m = keysIn(t, m, "E")
	if ed := editsOfSelected(t, m); string(ed.Body) != `{"orderId": 1` || m.status.level != statusWarn {
		t.Fatalf("invalid JSON: %q status %q", ed.Body, m.status.text)
	}
	if s := screenText(m); !strings.Contains(s, "! invalid JSON: unexpected end of JSON input") {
		t.Fatalf("no invalid-JSON flag:\n%s", s)
	}
	m = keysIn(t, m, "r")
	if s := screenText(m); !strings.Contains(s, "edited, 1 line") || !strings.Contains(s, "invalid JSON: unexpected end of JSON input") {
		t.Fatalf("confirm popup lacks the body flag:\n%s", s)
	}
	m = keysIn(t, m, "n")

	// Typing the original body back drops the body edit, compact or as
	// first shown (pretty-printed).
	fakeEditor(t, string(m.messages.items[0].Body)+"\n", nil)
	m = keysIn(t, m, "E")
	if editsOfSelected(t, m).BodyEdited {
		t.Fatal("original body kept a body edit")
	}
	fakeEditor(t, `{"orderId": 2}`, nil)
	m = keysIn(t, m, "E")
	fakeEditor(t, string(bodyForEditor(m.messages.items[0], bus.Edits{}))+"\n", nil)
	m = keysIn(t, m, "E")
	if editsOfSelected(t, m).BodyEdited {
		t.Fatal("pretty-printed original kept a body edit")
	}
}

func TestBodyEditorUnchangedTextBody(t *testing.T) {
	// orders seq 6 is the long non-JSON line without a final newline; an
	// editor that adds one is not an edit.
	m := keysIn(t, onOrders(t, fake.New()), "j", "j", "j", "j")
	var written string
	fakeEditor(t, string(m.messages.items[4].Body)+"\n", &written)
	m = keysIn(t, m, "E")
	if written != string(m.messages.items[4].Body) || editsOfSelected(t, m).BodyEdited || m.pendingCount() != 0 {
		t.Fatalf("unchanged text body: wrote %q, edits %+v", written, editsOfSelected(t, m))
	}
}

// TestBodyEditorRunsVisual runs the real command line: $VISUAL wins over
// $EDITOR, the file is 0600 in the temp dir, and it is gone afterwards.
func TestBodyEditorRunsVisual(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "edit.sh")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
echo "$1" > "$(dirname "$0")/path"
ls -l "$1" | cut -c1-10 > "$(dirname "$0")/mode"
printf 'edited by %s' "$2" > "$1"
`), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", dir)
	t.Setenv("VISUAL", "sh "+script)
	t.Setenv("EDITOR", "false")
	prev := execProcess
	t.Cleanup(func() { execProcess = prev })
	execProcess = func(c *exec.Cmd, fn tea.ExecCallback) tea.Cmd {
		return func() tea.Msg { return fn(c.Run()) }
	}
	m := keysIn(t, onOrders(t, fake.New()), "E")
	if got := string(editsOfSelected(t, m).Body); got != "edited by " {
		t.Fatalf("body = %q (status %q)", got, m.status.text)
	}
	path, _ := os.ReadFile(filepath.Join(dir, "path"))
	mode, _ := os.ReadFile(filepath.Join(dir, "mode"))
	p := strings.TrimSpace(string(path))
	if filepath.Dir(p) != dir || !strings.HasSuffix(p, ".json") || strings.TrimSpace(string(mode)) != "-rw-------" {
		t.Fatalf("temp file %q mode %q", p, mode)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("temp file not removed: %v", err)
	}

	// A failing editor changes nothing.
	t.Setenv("VISUAL", "false")
	m = keysIn(t, m, "E")
	if got := string(editsOfSelected(t, m).Body); got != "edited by " || m.status.level != statusErr {
		t.Fatalf("failed editor: body %q status %q", got, m.status.text)
	}
}

func TestEditorCommand(t *testing.T) {
	env := func(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }
	for _, tc := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"VISUAL": "code -w", "EDITOR": "nano"}, "code -w"},
		{map[string]string{"VISUAL": " ", "EDITOR": "nano"}, "nano"},
		{nil, "vi"},
	} {
		if got := editorCommand(env(tc.env)); got != tc.want {
			t.Errorf("editorCommand(%v) = %q, want %q", tc.env, got, tc.want)
		}
	}
}

// TestEditGoldens renders the edit popups and Pending Edit markers at
// 120×30.
func TestEditGoldens(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T) Model
	}{
		{"prop-add", func(t *testing.T) Model {
			return typeText(t, keysIn(t, onProperties(t, fake.New()), "a"), "region")
		}},
		{"prop-edit", func(t *testing.T) Model { return keysIn(t, onProperties(t, fake.New()), "j", "e") }},
		{"prop-invalid", func(t *testing.T) Model {
			m := keysIn(t, onProperties(t, fake.New()), "j", "j", "e", "ctrl+u")
			return keysIn(t, typeText(t, m, "2147483648"), "enter")
		}},
		{"prop-markers", func(t *testing.T) Model {
			m := keysIn(t, onProperties(t, fake.New()), "j", "e", "ctrl+u", "7", "enter")
			m = typeText(t, keysIn(t, m, "a"), "region")
			m = typeText(t, keysIn(t, m, "tab", "tab"), "eu-north")
			return keysIn(t, m, "enter", "g", "d")
		}},
		{"system-pending", func(t *testing.T) Model {
			m := keysIn(t, onOrders(t, fake.New()), "0", "]", "]", "j", "j", "e", "ctrl+u")
			return keysIn(t, typeText(t, m, "OrderFixed"), "enter", "3")
		}},
		{"body-edited", func(t *testing.T) Model {
			fakeEditor(t, "{\n  \"orderId\": 4002,\n  \"status\": \"fixed\"\n}", nil)
			return keysIn(t, onOrders(t, fake.New()), "E")
		}},
		{"body-invalid-json", func(t *testing.T) Model {
			fakeEditor(t, "{\n  \"orderId\": 4002,\n  \"status\": \"fixed\"\n", nil)
			return keysIn(t, onOrders(t, fake.New()), "E", "0")
		}},
		{"confirm-edits", func(t *testing.T) Model {
			fakeEditor(t, "{\n  \"orderId\": 4002,\n  \"status\": \"fixed\"\n", nil)
			m := keysIn(t, onProperties(t, fake.New()), "j", "e", "ctrl+u", "7", "enter", "g", "d")
			m = typeText(t, keysIn(t, m, "a"), "region")
			m = typeText(t, keysIn(t, m, "tab", "tab"), "eu-north")
			m = keysIn(t, m, "enter", "]", "j", "j", "e", "ctrl+u")
			m = typeText(t, m, "OrderFixed")
			return keysIn(t, m, "enter", "[", "[", "E", "r")
		}},
		{"confirm-discard", func(t *testing.T) Model {
			m := keysIn(t, onProperties(t, fake.New()), "j", "e", "ctrl+u", "7", "enter", "g", "d")
			return keysIn(t, m, "x")
		}},
		{"pending-count", func(t *testing.T) Model {
			// Two messages with Pending Edits; the second is selected.
			m := keysIn(t, onOrders(t, fake.New()), "e", "k", "tab", "tab", "v", "enter", "j", "e", "k", "tab", "tab", "v", "enter", "esc")
			return m
		}},
	}
	for _, tc := range cases {
		name := tc.name + "-120x30"
		t.Run(name, func(t *testing.T) {
			screen := tc.setup(t).View().Content
			requireScreenSize(t, screen, 120, 30)
			requireGolden(t, name, screen)
		})
	}
}

func TestHelpSaysPendingEditsAreLostOnQuit(t *testing.T) {
	m := typeText(t, keysIn(t, onOrders(t, fake.New()), "?"), "quit")
	if s := screenText(m); !strings.Contains(s, "kept in memory only: lost on quit") {
		t.Fatalf("help:\n%s", s)
	}
}

// addProp adds key=value (String) to the selected message from Messages.
func addProp(t *testing.T, m Model, key, value string) Model {
	t.Helper()
	m = typeText(t, keysIn(t, m, "e"), key)
	m = typeText(t, keysIn(t, m, "tab", "tab"), value)
	return keysIn(t, m, "enter")
}

func TestPendingEditsAfterFailedResubmit(t *testing.T) {
	for name, fault := range map[string]struct {
		op fake.Op
		f  fake.Fault
	}{
		"SendFailed":     {fake.OpSend, fake.Fault{Err: &bus.Error{Kind: bus.ErrNotAllowed, Msg: "not allowed"}, Definite: true}},
		"SendUncertain":  {fake.OpSend, fake.Fault{Err: context.DeadlineExceeded}},
		"CleanupPending": {fake.OpComplete, fake.Fault{Err: errors.New("link detached")}},
	} {
		t.Run(name, func(t *testing.T) {
			be := fake.New()
			m := addProp(t, onOrders(t, be), "k", "v")
			seq := selectedSeq(t, m)
			be.SetFault(fault.op, fault.f)
			m = keysIn(t, m, "r", "y")
			if !strings.HasPrefix(m.status.text, name+" (") || m.editsOf(seq).Count() != 1 {
				t.Fatalf("status %q, edits %d", m.status.text, m.editsOf(seq).Count())
			}
			if name == "SendFailed" {
				return
			}
			// The retry (SendUncertain) reuses the MessageId, so it must
			// send what the attempt sent; after CleanupPending the copy is
			// sent. Edits are fixed.
			for _, k := range []string{"e", "x", "E"} {
				m = keysIn(t, m, k)
				if !m.stack.AtRoot() || !strings.HasPrefix(m.status.text, name+":") {
					t.Fatalf("%s: top %v status %q", k, m.stack.Top(), m.status.text)
				}
			}
			if name == "CleanupPending" {
				return
			}
			be.SetFault(fault.op, fake.Fault{})
			m = keysIn(t, m, "r")
			if s := screenText(m); !strings.Contains(s, "+ k: String v") {
				t.Fatalf("retry confirm lacks the edit:\n%s", s)
			}
			m = keysIn(t, m, "y")
			got := be.Messages("sb-prod-weu", "orders", bus.KindQueue, bus.Active)
			if m.pendingCount() != 0 || len(got) == 0 || got[len(got)-1].Properties[len(got[len(got)-1].Properties)-1].Key != "k" {
				t.Fatalf("retry: pending %d, status %q", m.pendingCount(), m.status.text)
			}
		})
	}
}

// TestConfirmManyEditsKeepsWarnings: many Property Edits give way to the
// warnings at the minimum size; their count stays.
func TestConfirmManyEditsKeepsWarnings(t *testing.T) {
	be := fake.New()
	m := New(be, testOptions())
	m = run(t, m, tea.WindowSizeMsg{Width: minWidth, Height: minHeight})
	m = runCmd(t, m, m.Init())
	m = keysIn(t, m, "2", "j", "enter")
	for i := range 12 {
		m = addProp(t, m, fmt.Sprintf("k%d", i), "v")
	}
	be.SetFault(fake.OpSend, fake.Fault{Err: context.DeadlineExceeded})
	m = keysIn(t, m, "r", "y")
	be.SetFault(fake.OpSend, fake.Fault{})
	m = keysIn(t, m, "r")
	screen := m.View().Content
	requireScreenSize(t, screen, minWidth, minHeight)
	text := stripBoxes(screen)
	for _, want := range []string{"Source", "Target", "12 property edits", "MessageId", "no match = dropped", "previous attempt may have delivered"} {
		if !strings.Contains(text, want) {
			t.Errorf("confirm at %dx%d lacks %q:\n%s", minWidth, minHeight, want, ansi.Strip(screen))
		}
	}
}
