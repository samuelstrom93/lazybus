package ui

import (
	"encoding/json"
	"fmt"
	"maps"
	"mime"
	"slices"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/samuelstrom93/lazybus/internal/bus"
)

// Pending Edits (spec §6): Property Edits, Subject / ContentType changes
// and a body edit per dead-letter message, kept in memory only (lost on
// quit) and consumed by a DLQ Repair.

// pending is the Pending Edits of one message. rev changes with every
// edit, so the cached Body tab lines of the message are redrawn.
type pending struct {
	bus.Edits
	rev int
}

// editsOf returns the Pending Edits of the open list's message seq.
func (m Model) editsOf(seq int64) pending {
	k, ok := m.markKeyOf(seq)
	if !ok {
		return pending{}
	}
	return m.pending[k]
}

// setEdits stores e for message k; zero Edits remove the entry. The map
// is copied: the Model is a value, and older copies must not see the
// change.
func (m *Model) setEdits(k markKey, e bus.Edits) {
	p := maps.Clone(m.pending)
	if p == nil {
		p = map[markKey]pending{}
	}
	if e.IsZero() {
		delete(p, k)
	} else {
		m.editRev++
		p[k] = pending{Edits: e, rev: m.editRev}
	}
	m.pending = p
}

// resubmitHint ends the status of a saved edit.
func (m Model) resubmitHint() string {
	if m.opts.ReadOnly {
		return "; read-only: r is disabled, edits are lost on quit"
	}
	return "; r resubmits with it"
}

// pendingCount is the number of messages with Pending Edits.
func (m Model) pendingCount() int { return len(m.pending) }

// activeRefusal is the status text for an edit or resubmit key on the
// Active tab (spec §5).
const activeRefusal = "read-only: Active messages can't be repaired"

// editTarget returns the selected DLQ message and its key, or sets the
// status that says why there is none. Edits are local, so --read-only
// allows them (only r and c change broker state).
func (m *Model) editTarget() (bus.Message, markKey, bool) {
	if m.subQueue == bus.Active {
		m.setStatus(statusWarn, activeRefusal)
		return bus.Message{}, markKey{}, false
	}
	msg, ok := m.selectedMessage()
	if !ok || m.openNS == nil || m.openEntity == nil {
		m.setStatus(statusInfo, "no message selected: nothing to edit")
		return bus.Message{}, markKey{}, false
	}
	// A SendUncertain retry reuses the MessageId of the attempt, so it must
	// send the same content: a duplicate-detecting target would drop a
	// changed copy as a duplicate. A CleanupPending copy is already sent.
	switch mark, marked := m.markOf(msg.SequenceNumber); {
	case marked && mark.outcome == bus.SendUncertain:
		m.setStatus(statusWarn, "SendUncertain: the Pending Edits are fixed until r retries with them (a retry reuses the MessageId)")
		return bus.Message{}, markKey{}, false
	case marked && mark.outcome == bus.CleanupPending:
		m.setStatus(statusWarn, "CleanupPending: the copy is already sent; c finishes cleanup, edits no longer apply")
		return bus.Message{}, markKey{}, false
	}
	k, _ := m.markKeyOf(msg.SequenceNumber)
	return msg, k, true
}

// --- the edit popup --------------------------------------------------------

type editKind int

const (
	editProperty editKind = iota
	editSubject
	editContentType
)

func (k editKind) String() string {
	return [...]string{"property", "Subject", "ContentType"}[k]
}

type editField int

const (
	fieldKey editField = iota
	fieldType
	fieldValue
)

// editState is the open edit popup (CtxEdit).
type editState struct {
	kind   editKind
	target markKey
	msg    bus.Message
	// adding: a new property (a, or e in Messages); the key is typed.
	// Otherwise the key is fixed.
	adding bool
	key    string
	typ    bus.PropertyType
	// typeTouched: the user picked the type; until then an add whose key
	// names an existing property takes that property's type.
	typeTouched bool
	value       string
	field       editField
	err         string
	// was describes the original value ("Long 1001"); empty for a new
	// key.
	was string
}

// startAddProperty opens the popup for a new property (a on Properties,
// e on Messages).
func (m Model) startAddProperty() (Model, tea.Cmd) {
	msg, k, ok := m.editTarget()
	if !ok {
		return m, nil
	}
	m.edit = editState{kind: editProperty, target: k, msg: msg, adding: true, typ: bus.TypeString, field: fieldKey}
	m.stack.Push(CtxEdit)
	return m, nil
}

// startEditProperty handles e on the Properties tab: the selected
// property, prefilled with its pending or original type and value.
func (m Model) startEditProperty() (Model, tea.Cmd) {
	msg, k, ok := m.editTarget()
	if !ok {
		return m, nil
	}
	items := m.propertyItems(msg)
	if len(items) == 0 {
		m.setStatus(statusInfo, "no application properties: a adds one")
		return m, nil
	}
	it := items[max(0, min(m.mainCursor, len(items)-1))]
	if it.marker {
		m.setStatus(statusWarn, "%s: %v", it.rawKey, bus.ErrMarkerEdit)
		return m, nil
	}
	e := editState{kind: editProperty, target: k, msg: msg, key: it.rawKey, typ: bus.TypeString, field: fieldValue}
	if o, ok := findProp(msg.Properties, it.rawKey); ok {
		e.typ, e.value = o.Type, propertyValue(o, m.opts.Location)
		e.was = o.Type.String() + " " + e.value
	}
	if pe, ok := m.editsOf(msg.SequenceNumber).Property(it.rawKey); ok && !pe.Remove {
		e.typ, e.value = pe.Type, propertyValue(bus.Property{Key: pe.Key, Type: pe.Type, Value: pe.Value}, m.opts.Location)
	}
	m.edit = e
	m.stack.Push(CtxEdit)
	return m, nil
}

// startEditField handles e on the System tab: Subject and ContentType
// only.
func (m Model) startEditField() (Model, tea.Cmd) {
	msg, k, ok := m.editTarget()
	if !ok {
		return m, nil
	}
	fields := m.systemFields(msg)
	f := fields[max(0, min(m.mainCursor, len(fields)-1))]
	var kind editKind
	var orig string
	var cur *string
	ed := m.editsOf(msg.SequenceNumber)
	switch f.label {
	case "Subject":
		kind, orig, cur = editSubject, msg.Subject, ed.Subject
	case "ContentType":
		kind, orig, cur = editContentType, msg.ContentType, ed.ContentType
	default:
		m.setStatus(statusInfo, "only Subject and ContentType (✎) can be edited; e on their row")
		return m, nil
	}
	e := editState{kind: kind, target: k, msg: msg, key: f.label, value: orig, field: fieldValue, was: orig}
	if cur != nil {
		e.value = *cur
	}
	m.edit = e
	m.stack.Push(CtxEdit)
	return m, nil
}

func findProp(props []bus.Property, key string) (bus.Property, bool) {
	for _, p := range props {
		if p.Key == key {
			return p, true
		}
	}
	return bus.Property{}, false
}

// fields are the popup's fields in tab order.
func (e editState) fields() []editField {
	switch {
	case e.kind != editProperty:
		return []editField{fieldValue}
	case e.adding:
		return []editField{fieldKey, fieldType, fieldValue}
	}
	return []editField{fieldType, fieldValue}
}

func (e *editState) moveField(d int) {
	fs := e.fields()
	i := max(0, slices.Index(fs, e.field))
	e.field = fs[(i+d+len(fs))%len(fs)]
}

// existingType is the type a property key has on the message: its
// pending type when it has a pending change or add, else its original.
func (m Model) existingType(e editState, k string) (bus.PropertyType, bool) {
	if pe, ok := m.pending[e.target].Property(k); ok && !pe.Remove {
		return pe.Type, true
	}
	if o, ok := findProp(e.msg.Properties, k); ok {
		return o.Type, true
	}
	return 0, false
}

// handleEditKey: enter saves (an invalid value keeps the popup open with
// the error), tab / shift+tab move between fields, ← → change the type,
// esc cancels. Letters are text, so n does not cancel here.
func (m Model) handleEditKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	e := &m.edit
	switch s := msg.String(); {
	case key.Matches(msg, keys.EditCancel):
		m.stack.Pop()
		m.edit = editState{}
		m.setStatus(statusInfo, "edit canceled; Pending Edits unchanged")
	case key.Matches(msg, keys.EditSave):
		return m.saveEdit()
	case key.Matches(msg, keys.EditNext):
		e.moveField(1)
	case key.Matches(msg, keys.EditPrev):
		e.moveField(-1)
	case e.field == fieldType:
		types := bus.PropertyTypes()
		i := slices.Index(types, e.typ)
		switch s {
		case "left", "h":
			e.typ, e.typeTouched, e.err = types[(i+len(types)-1)%len(types)], true, ""
		case "right", "l", "space", " ":
			e.typ, e.typeTouched, e.err = types[(i+1)%len(types)], true, ""
		}
	case key.Matches(msg, keys.HelpErase):
		if t := e.text(); t != nil {
			if r := []rune(*t); len(r) > 0 {
				m.setText(string(r[:len(r)-1]))
			}
		}
	case s == "ctrl+u":
		m.setText("")
	case msg.Text != "" && msg.Mod&(tea.ModCtrl|tea.ModAlt) == 0:
		if t := e.text(); t != nil {
			m.setText(*t + msg.Text)
		}
	}
	return m, nil
}

// text is the field being typed into; nil on the Type field.
func (e *editState) text() *string {
	switch e.field {
	case fieldKey:
		return &e.key
	case fieldValue:
		return &e.value
	}
	return nil
}

// setText replaces the text of the focused field. Typing a key that the
// message already has selects that property's type, unless the type was
// picked by hand: an add of an existing key is a change, and a changed
// key keeps its type.
func (m *Model) setText(s string) {
	e := &m.edit
	t := e.text()
	if t == nil {
		return
	}
	*t, e.err = s, ""
	if e.field == fieldKey && e.adding && !e.typeTouched {
		e.typ = bus.TypeString
		if typ, ok := m.existingType(*e, strings.TrimSpace(s)); ok {
			e.typ = typ
		}
	}
}

// paste inserts pasted text into the focused field.
func (m Model) paste(text string) Model {
	if t := m.edit.text(); t != nil {
		m.setText(*t + text)
	}
	return m
}

// saveEdit validates the popup and stores the edit; an error stays in
// the popup.
func (m Model) saveEdit() (Model, tea.Cmd) {
	e := m.edit
	cur := m.pending[e.target].Edits
	label := fmt.Sprintf("%s seq %d", entityLabel(bus.Entity{Path: e.target.path}, bus.DeadLetter), e.target.seq)
	var next bus.Edits
	var status string
	switch e.kind {
	case editProperty:
		k := strings.TrimSpace(e.key)
		if k == "" {
			m.edit.err, m.edit.field = "key is empty", fieldKey
			return m, nil
		}
		v, err := bus.ParseValue(e.typ, e.value)
		if err != nil {
			m.edit.err, m.edit.field = e.typ.String()+": "+err.Error(), fieldValue
			return m, nil
		}
		next, err = cur.SetProperty(e.msg.Properties, bus.PropertyEdit{Key: k, Type: e.typ, Value: v})
		if err != nil {
			m.edit.err = k + ": " + err.Error()
			return m, nil
		}
		if _, ok := next.Property(k); ok {
			status = fmt.Sprintf("Pending Edit on %s: %s = %s (%s)%s", label, k,
				propertyValue(bus.Property{Type: e.typ, Value: v}, m.opts.Location), e.typ, m.resubmitHint())
		} else {
			status = fmt.Sprintf("%s is back to its original value: no Pending Edit", k)
		}
		m.tab = tabProperties
	default:
		orig := e.msg.Subject
		if e.kind == editContentType {
			orig = e.msg.ContentType
		}
		var val *string
		if e.value != orig {
			v := e.value
			val = &v
		}
		next = cur
		if e.kind == editSubject {
			next.Subject = val
		} else {
			next.ContentType = val
		}
		if val != nil {
			status = fmt.Sprintf("Pending Edit on %s: %s = %q%s", label, e.kind, e.value, m.resubmitHint())
		} else {
			status = fmt.Sprintf("%s is back to its original value: no Pending Edit", e.kind)
		}
		m.tab = tabSystem
	}
	m.setEdits(e.target, next)
	m.stack.Pop()
	m.edit = editState{}
	m.setStatus(statusOK, "%s", status)
	m.selectMainRow(e)
	return m, nil
}

// selectMainRow puts the main pane cursor on the row an edit changed.
func (m *Model) selectMainRow(e editState) {
	msg, ok := m.selectedMessage()
	if !ok {
		return
	}
	switch e.kind {
	case editProperty:
		k := strings.TrimSpace(e.key)
		for i, it := range m.propertyItems(msg) {
			if it.rawKey == k && !it.marker {
				m.mainCursor = i
			}
		}
	default:
		for i, f := range m.systemFields(msg) {
			if f.label == e.kind.String() {
				m.mainCursor = i
			}
		}
	}
}

// toggleRemove handles d on the Properties tab.
func (m Model) toggleRemove() (Model, tea.Cmd) {
	msg, k, ok := m.editTarget()
	if !ok {
		return m, nil
	}
	items := m.propertyItems(msg)
	if len(items) == 0 {
		m.setStatus(statusInfo, "no application properties to remove")
		return m, nil
	}
	it := items[max(0, min(m.mainCursor, len(items)-1))]
	cur := m.pending[k].Edits
	next, err := cur.ToggleRemove(msg.Properties, it.rawKey)
	if err != nil {
		m.setStatus(statusWarn, "%s: %v", it.rawKey, err)
		return m, nil
	}
	m.setEdits(k, next)
	switch pe, edited := next.Property(it.rawKey); {
	case edited && pe.Remove:
		m.setStatus(statusOK, "%s will be removed on resubmit (−); d again keeps it", it.rawKey)
	case it.pend == pendAdded:
		m.setStatus(statusOK, "%s: pending add dropped", it.rawKey)
	default:
		m.setStatus(statusOK, "%s is kept: removal undone", it.rawKey)
	}
	return m, nil
}

// --- x discard ----------------------------------------------------------------

// startDiscard handles x: a confirm popup when the message has Pending
// Edits.
func (m Model) startDiscard() (Model, tea.Cmd) {
	msg, _, ok := m.editTarget()
	if !ok {
		return m, nil
	}
	ed := m.editsOf(msg.SequenceNumber)
	if ed.IsZero() {
		m.setStatus(statusInfo, "no Pending Edits on this message: nothing to discard")
		return m, nil
	}
	req := bus.RepairRequest{
		Namespace: *m.openNS, Entity: *m.openEntity, SubQueue: bus.DeadLetter,
		SequenceNumber: msg.SequenceNumber, Edits: ed.Edits,
	}
	plan := bus.RepairPlan{Source: entityLabel(req.Entity, bus.DeadLetter), SequenceNumber: msg.SequenceNumber, Message: msg}
	m.confirm = confirmState{kind: confirmDiscard, req: req, plan: plan}
	m.stack.Push(CtxConfirm)
	return m, nil
}

// --- descriptions ---------------------------------------------------------------

// isJSONType reports whether a content type is JSON (application/json,
// text/json or a +json suffix).
func isJSONType(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	return mt == "application/json" || mt == "text/json" || strings.HasSuffix(mt, "+json")
}

// bodyJSONError is why an edited body is not valid JSON, when JSON is
// expected: the content type (edited or original) is JSON or the
// original body parsed as JSON. Empty when the body is fine.
func bodyJSONError(msg bus.Message, e bus.Edits) string {
	if !e.BodyEdited {
		return ""
	}
	ct := msg.ContentType
	if e.ContentType != nil {
		ct = *e.ContentType
	}
	if !isJSONType(ct) && !json.Valid(msg.Body) {
		return ""
	}
	var v any
	if err := json.Unmarshal(e.Body, &v); err != nil {
		return err.Error()
	}
	return ""
}

// diffLine is one line describing a Pending Edit.
type diffLine struct {
	label, text string
	level       statusLevel
	optional    bool // may be dropped on a short terminal (a counted property line)
}

// editDiff describes e against msg: body, each Property Edit, Subject and
// ContentType, old → new. Used by the resubmit and discard popups. With
// more than one Property Edit a count comes first and the per-property
// lines are optional, so many edits never push the warnings off a short
// terminal.
func (m Model) editDiff(msg bus.Message, e bus.Edits) []diffLine {
	var out []diffLine
	if e.BodyEdited {
		out = append(out, diffLine{"Body", bodySummary("edited", e.Body), statusWarn, false})
		if why := bodyJSONError(msg, e); why != "" {
			out = append(out, diffLine{"", "invalid JSON: " + why, statusErr, false})
		}
	}
	label, optional := "Properties", false
	if n := len(e.Properties); n > 1 {
		out = append(out, diffLine{label, fmt.Sprintf("%d property edits", n), statusWarn, false})
		label, optional = "", true
	}
	for _, pe := range e.Properties {
		o, had := findProp(msg.Properties, pe.Key)
		was := ""
		if had {
			was = o.Type.String() + " " + propertyValue(o, m.opts.Location)
		}
		var text string
		switch {
		case pe.Remove:
			text = "− " + pe.Key + " (was " + was + ")"
		case had:
			text = "* " + pe.Key + ": " + was + " → " + pe.Type.String() + " " + m.peValue(pe)
		default:
			text = "+ " + pe.Key + ": " + pe.Type.String() + " " + m.peValue(pe)
		}
		out = append(out, diffLine{label, text, statusWarn, optional})
		label = ""
	}
	field := func(name, old string, cur *string) {
		if cur != nil {
			out = append(out, diffLine{name, orDash(old) + " → " + orDash(*cur), statusWarn, false})
		}
	}
	field("Subject", msg.Subject, e.Subject)
	field("ContentType", msg.ContentType, e.ContentType)
	return out
}

func (m Model) peValue(pe bus.PropertyEdit) string {
	return propertyValue(bus.Property{Key: pe.Key, Type: pe.Type, Value: pe.Value}, m.opts.Location)
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// bodySummary is "<state>, N lines[, JSON]" or "<state>, empty".
func bodySummary(state string, body []byte) string {
	if len(body) == 0 {
		return state + ", empty"
	}
	text, isJSON := bodyText(body)
	n := strings.Count(text, "\n") + 1
	s := fmt.Sprintf("%s, %d line%s", state, n, plural(n))
	if isJSON {
		s += ", JSON"
	}
	return s
}

// --- rendering --------------------------------------------------------------------

// typeHints say what the Value field takes for each type.
var typeHints = map[bus.PropertyType]string{
	bus.TypeString:   "text, sent as typed",
	bus.TypeInt:      "whole number, -2147483648 to 2147483647",
	bus.TypeLong:     "whole number, 64-bit",
	bus.TypeDouble:   "number, e.g. 12.5 or 1e-3",
	bus.TypeBool:     "true or false",
	bus.TypeGUID:     "e.g. 6f1c2b9e-4d2a-4c1e-9b7a-000000000001",
	bus.TypeDateTime: "RFC 3339, e.g. 2026-09-18T22:15:00Z",
}

// tailFit shows the end of s in width cells: what is being typed stays
// visible.
func tailFit(s string, width int) string {
	r := []rune(sanitize(s))
	if len(r) <= width {
		return string(r)
	}
	return "…" + string(r[len(r)-(width-1):])
}

func (m Model) overlayEdit(screen string) string {
	e := m.edit
	w := min(m.width-4, 76)
	inner := w - 2
	const labelW = 8
	valW := inner - 2 - labelW - 1
	src := fmt.Sprintf("%s/$DLQ seq %d", e.target.path, e.target.seq)
	var title string
	switch {
	case e.kind != editProperty:
		title = "Edit " + e.kind.String()
	case e.adding:
		title = "Add property"
	default:
		title = "Edit property"
	}
	labelStyle := func(f editField) lipgloss.Style {
		if f == e.field {
			return stTitleFocus
		}
		return stDim
	}
	textRow := func(label string, f editField, text string, editable bool) string {
		segs := []seg{{"  " + fitPlain(label, labelW), labelStyle(f)}}
		switch {
		case f == e.field:
			segs = append(segs, seg{tailFit(text, valW-1), stPlain}, seg{"▏", stTitleFocus})
		case !editable:
			segs = append(segs, seg{text, stDim})
		default:
			segs = append(segs, seg{text, stPlain})
		}
		return row(segs, inner, false)
	}
	blank := strings.Repeat(" ", inner)
	body := []string{blank}
	if e.kind == editProperty {
		body = append(body, textRow("Key", fieldKey, e.key, e.adding))
		segs := []seg{{"  " + fitPlain("Type", labelW), labelStyle(fieldType)}}
		for i, t := range bus.PropertyTypes() {
			if i > 0 {
				segs = append(segs, seg{" ", stPlain})
			}
			st := stDim
			if t == e.typ {
				st = stBold
				if e.field == fieldType {
					st = stSelected
				}
			}
			segs = append(segs, seg{" " + t.String() + " ", st})
		}
		body = append(body, row(segs, inner, false))
	}
	body = append(body, textRow("Value", fieldValue, e.value, true))
	hint := typeHints[e.typ]
	if e.kind != editProperty {
		hint = "empty = no " + e.kind.String() + " on the copy"
	}
	body = append(body, row([]seg{{"  " + strings.Repeat(" ", labelW) + hint, stDim}}, inner, false))
	if e.was != "" || !e.adding {
		was := e.was
		if was == "" {
			was = "—"
			if e.kind == editProperty {
				was = "(not on the original message)"
			}
		}
		body = append(body, row([]seg{{"  " + fitPlain("was", labelW), stDim}, {was, stDim}}, inner, false))
	}
	if e.err != "" {
		for i, l := range wrapLines(sanitize(e.err), inner-4) {
			prefix := "   "
			if i == 0 {
				prefix = " ! "
			}
			body = append(body, row([]seg{{prefix + l, stErr}}, inner, false))
		}
	}
	body = append(body, blank)

	lines := []string{hBorder(w, "┌", "┐", []seg{{title, stTitleFocus}, {"  " + src, stDim}}, stBorderFocus)}
	for _, l := range body {
		lines = append(lines, boxRow(l, stBorderFocus))
	}
	footer := []seg{{"enter", stKey}, {" save  ", stDim}}
	if len(e.fields()) > 1 {
		footer = append(footer, seg{"tab", stKey}, seg{" next field  ", stDim})
	}
	if e.field == fieldType {
		footer = append(footer, seg{"← →", stKey}, seg{" type  ", stDim})
	}
	footer = append(footer, seg{"esc", stKey}, seg{" cancel", stDim})
	lines = append(lines, hBorder(w, "└", "┘", footer, stBorderFocus))
	return m.overlay(screen, lines, w)
}
