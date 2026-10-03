package ui

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// clipboard copies text with OSC 52, which works over SSH: the terminal
// on the user's machine sets its clipboard.
//
// Inside tmux, an application's OSC 52 reaches the outer terminal only with
// `set -g set-clipboard on`, and a passthrough-wrapped one only with
// `set -g allow-passthrough on`; tmux's defaults (external, off) drop both.
// `tmux load-buffer -w -` works with the defaults (tmux ≥ 3.2): tmux sets
// its paste buffer and sends OSC 52 to the outer terminal itself. Verified
// on tmux 3.7c by recording the outer terminal's bytes. So inside tmux,
// lazybus runs that, and falls back to the passthrough-wrapped sequence
// when the command fails (older tmux).
type clipboard struct {
	getenv func(string) string
	// loadBuffer runs `tmux load-buffer -w -` with text on stdin.
	loadBuffer func(ctx context.Context, text string) error
}

func systemClipboard() clipboard {
	return clipboard{getenv: os.Getenv, loadBuffer: tmuxLoadBuffer}
}

// newClipboard is the clipboard New gives a model; tests replace it so
// they never run tmux.
var newClipboard = systemClipboard

// copy returns the command that sets the clipboard to text.
func (c clipboard) copy(text string) tea.Cmd {
	if c.getenv == nil || c.getenv("TMUX") == "" {
		return tea.Raw(osc52(text))
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if c.loadBuffer != nil && c.loadBuffer(ctx, text) == nil {
			return nil
		}
		return tea.RawMsg{Msg: tmuxPassthrough(osc52(text))}
	}
}

// osc52 is the OSC 52 sequence that sets the system clipboard ("c") to
// text, BEL-terminated (the most widely supported terminator).
func osc52(text string) string {
	return "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\a"
}

// tmuxPassthrough wraps seq in tmux's DCS passthrough: every ESC inside is
// doubled.
func tmuxPassthrough(seq string) string {
	return "\x1bPtmux;" + strings.ReplaceAll(seq, "\x1b", "\x1b\x1b") + "\x1b\\"
}

func tmuxLoadBuffer(ctx context.Context, text string) error {
	cmd := exec.CommandContext(ctx, "tmux", "load-buffer", "-w", "-")
	cmd.Stdin = strings.NewReader(text)
	return cmd.Run()
}
