package ui

import (
	"strconv"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// The main pane search: / while the main pane is focused, n and N to the
// next and previous matching line. The term stays across messages and tabs,
// so the same field can be found in message after message; esc clears it.
type searchState struct {
	term string
	line int // the line of the current match; -1 for none
}

var (
	stMatch    = lipgloss.NewStyle().Reverse(true)
	stMatchCur = lipgloss.NewStyle().Background(colYellow).Foreground(lipgloss.Color("0"))
)

func (m Model) startSearch() (Model, tea.Cmd) {
	m.search = searchState{line: -1}
	m.stack.Push(CtxSearch)
	return m, nil
}

// handleSearchKey: text edits the term and jumps to the first match from
// the current position, enter keeps it, esc clears it.
func (m Model) handleSearchKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	switch {
	case key.Matches(msg, keys.SearchKeep):
		m.stack.Pop()
		if m.search.term != "" && m.search.line < 0 {
			m.setStatus(statusInfo, "no match for /%s", m.search.term)
		}
		return m, nil
	case key.Matches(msg, keys.SearchClear):
		m.stack.Pop()
		m.search = searchState{line: -1}
		return m, nil
	case key.Matches(msg, keys.HelpErase):
		r := []rune(m.search.term)
		if len(r) == 0 {
			return m, nil
		}
		m.search.term = string(r[:len(r)-1])
	case msg.String() == "ctrl+u":
		m.search.term = ""
	case msg.Text != "" && msg.Mod&(tea.ModCtrl|tea.ModAlt) == 0:
		m.search.term += msg.Text
	default:
		return m, nil
	}
	m.search.line = -1
	if m.search.term != "" {
		m.jumpMatch(m.mainPos(), 1, true)
	}
	return m, nil
}

// nextMatch moves to the next (dir 1) or previous (dir -1) matching line,
// wrapping around the content.
func (m Model) nextMatch(dir int) (Model, tea.Cmd) {
	if m.search.term == "" {
		m.setStatus(statusInfo, "/ searches the main pane first")
		return m, nil
	}
	from := m.search.line
	if from < 0 {
		from = m.mainPos()
		if dir < 0 {
			from++
		} else {
			from--
		}
	}
	if !m.jumpMatch(from+dir, dir, true) {
		m.setStatus(statusInfo, "no match for /%s", m.search.term)
	}
	return m, nil
}

// jumpMatch moves to the first matching line from line from in direction
// dir (inclusive), wrapping around. It reports whether there was one.
func (m *Model) jumpMatch(from, dir int, wrap bool) bool {
	lines, rows := m.mainContent(m.layout().mainW - 2)
	matches := searchLines(lines, m.search.term)
	if len(matches) == 0 {
		m.search.line = -1
		return false
	}
	at := -1
	if dir > 0 {
		for _, l := range matches {
			if l >= from {
				at = l
				break
			}
		}
		if at < 0 && wrap {
			at = matches[0]
		}
	} else {
		for i := len(matches) - 1; i >= 0; i-- {
			if matches[i] <= from {
				at = matches[i]
				break
			}
		}
		if at < 0 && wrap {
			at = matches[len(matches)-1]
		}
	}
	if at < 0 {
		return false
	}
	m.search.line = at
	if len(rows) == 0 {
		// Body: the match goes to the top of the pane.
		m.mainScroll = at
		return true
	}
	// Properties and System: the cursor goes to the match's row (a header
	// line selects the row after it) and the scroll follows.
	m.mainCursor = len(rows) - 1
	for i, r := range rows {
		if r >= at {
			m.mainCursor = i
			break
		}
	}
	return true
}

// mainPos is the line the main pane is at: the top line on the Body tab,
// the selected row's line on the others.
func (m Model) mainPos() int {
	_, rows := m.mainContent(m.layout().mainW - 2)
	if len(rows) > 0 && m.mainCursor < len(rows) {
		return rows[m.mainCursor]
	}
	return m.mainScroll
}

// searchLines are the indices of the lines containing term, ignoring case
// and styling.
func searchLines(lines []string, term string) []int {
	if term == "" {
		return nil
	}
	t := strings.ToLower(term)
	var out []int
	for i, l := range lines {
		if strings.Contains(strings.ToLower(ansi.Strip(l)), t) {
			out = append(out, i)
		}
	}
	return out
}

// highlight redraws a matching line with each match marked; current marks
// the line of the current match. The line's own styling gives way to the
// marks. A line whose lower case changes its byte length is left as is.
func highlight(line, term string, current bool, width int) string {
	plain := ansi.Strip(line)
	lower, t := strings.ToLower(plain), strings.ToLower(term)
	if t == "" || len(lower) != len(plain) || !strings.Contains(lower, t) {
		return line
	}
	st := stMatch
	if current {
		st = stMatchCur
	}
	var segs []seg
	for {
		i := strings.Index(lower, t)
		if i < 0 {
			break
		}
		segs = append(segs, seg{plain[:i], stPlain}, seg{plain[i : i+len(t)], st})
		plain, lower = plain[i+len(t):], lower[i+len(t):]
	}
	segs = append(segs, seg{plain, stPlain})
	return row(segs, width, false)
}

// searchTitle is the search part of the main pane title: " /term 2/5"
// while a term is set, with a cursor while it is typed.
func (m Model) searchTitle(lines []string) []seg {
	typing := m.stack.Top() == CtxSearch
	if m.search.term == "" && !typing {
		return nil
	}
	t := []seg{{"  /" + m.search.term, stKey}}
	if typing {
		t = append(t, seg{"▏", stTitleFocus})
	}
	if m.search.term == "" {
		return t
	}
	matches := searchLines(lines, m.search.term)
	cur := 0
	for i, l := range matches {
		if l == m.search.line {
			cur = i + 1
		}
	}
	if len(matches) == 0 {
		return append(t, seg{" no match", stDim})
	}
	return append(t, seg{" " + strconv.Itoa(cur) + "/" + strconv.Itoa(len(matches)), stDim})
}
