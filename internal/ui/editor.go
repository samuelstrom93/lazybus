package ui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/samuelstrom93/lazybus/internal/bus"
)

// Body edits in the user's editor (spec §5 E, §10 S3): the body goes to a
// temp file (0600, in os.TempDir, removed afterwards), the editor runs
// with the terminal handed over, and what comes back becomes the pending
// body. A JSON body is written pretty-printed; the copy is sent with the
// bytes as edited, not re-compacted.

// execProcess runs an editor with the terminal; tests replace it.
var execProcess = tea.ExecProcess

// editorDoneMsg reports that the editor exited.
type editorDoneMsg struct {
	target  markKey
	msg     bus.Message
	path    string
	written []byte // what lazybus wrote to the file
	err     error
}

// editorCommand is the editor to run: $VISUAL, then $EDITOR, then vi. It
// is run by sh, so it may carry arguments ("code -w").
func editorCommand(getenv func(string) string) string {
	for _, v := range []string{"VISUAL", "EDITOR"} {
		if e := strings.TrimSpace(getenv(v)); e != "" {
			return e
		}
	}
	return "vi"
}

// bodyForEditor is what the editor opens: the pending body as it is, or
// the original body, pretty-printed when it is JSON.
func bodyForEditor(msg bus.Message, ed bus.Edits) []byte {
	if ed.BodyEdited {
		return ed.Body
	}
	if json.Valid(msg.Body) {
		var b bytes.Buffer
		if json.Indent(&b, msg.Body, "", "  ") == nil {
			return b.Bytes()
		}
	}
	return msg.Body
}

// writeTempBody writes body to a new 0600 file in os.TempDir and returns
// its path.
func writeTempBody(body []byte, jsonish bool) (string, error) {
	ext := ".txt"
	if jsonish {
		ext = ".json"
	}
	f, err := os.CreateTemp("", "lazybus-body-*"+ext)
	if err != nil {
		return "", err
	}
	path := f.Name()
	err = f.Chmod(0o600)
	if err == nil {
		_, err = f.Write(body)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

// startBodyEdit handles E: open the body of the selected DLQ message in
// the editor.
func (m Model) startBodyEdit() (Model, tea.Cmd) {
	msg, k, ok := m.editTarget()
	if !ok {
		return m, nil
	}
	ed := m.editsOf(msg.SequenceNumber).Edits
	body := bodyForEditor(msg, ed)
	jsonish := isJSONType(msg.ContentType) || json.Valid(msg.Body)
	path, err := writeTempBody(body, jsonish)
	if err != nil {
		m.setStatus(statusErr, "edit body: %v", err)
		return m, nil
	}
	editor := editorCommand(os.Getenv)
	c := exec.Command("sh", "-c", editor+` "$1"`, "lazybus-editor", path)
	done := editorDoneMsg{target: k, msg: msg, path: path, written: body}
	return m, execProcess(c, func(err error) tea.Msg {
		done.err = err
		return done
	})
}

// editorDone reads the edited file back, removes it, and stores the body
// as a Pending Edit when it changed.
func (m Model) editorDone(d editorDoneMsg) Model {
	edited, rerr := os.ReadFile(d.path)
	_ = os.Remove(d.path)
	switch {
	case d.err != nil:
		m.setStatus(statusErr, "editor failed (%v): body unchanged", d.err)
		return m
	case rerr != nil:
		m.setStatus(statusErr, "reading the edited body: %v; body unchanged", rerr)
		return m
	}
	// Editors end the file with a newline; lazybus did not write one.
	if !bytes.HasSuffix(d.written, []byte("\n")) {
		edited = bytes.TrimSuffix(edited, []byte("\n"))
	}
	label := fmt.Sprintf("%s seq %d", entityLabel(bus.Entity{Path: d.target.path}, bus.DeadLetter), d.target.seq)
	cur := m.pending[d.target].Edits
	m.tab = tabBody
	m.mainScroll = 0
	switch {
	case bytes.Equal(edited, d.written):
		m.setStatus(statusInfo, "body unchanged: no new Pending Edit")
		return m
	case bytes.Equal(edited, d.msg.Body), bytes.Equal(edited, bodyForEditor(d.msg, bus.Edits{})):
		cur.Body, cur.BodyEdited = nil, false
		m.setEdits(d.target, cur)
		m.setStatus(statusOK, "body is back to the original: no body edit")
		return m
	}
	cur.Body, cur.BodyEdited = edited, true
	m.setEdits(d.target, cur)
	if why := bodyJSONError(d.msg, cur); why != "" {
		m.setStatus(statusWarn, "body of %s edited but it is not valid JSON (%s); kept as the pending edit", label, why)
		return m
	}
	m.setStatus(statusOK, "body of %s edited (%s)%s", label, strings.TrimPrefix(bodySummary("", edited), ", "), m.resubmitHint())
	return m
}
