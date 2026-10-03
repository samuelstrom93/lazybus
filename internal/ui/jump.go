package ui

import (
	"sort"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/samuelstrom93/lazybus/internal/bus"
)

// The : popup jumps to an entity of the open namespace by fuzzy name; enter
// opens its DLQ in Messages.

type jumpState struct {
	query  string
	cursor int
}

// jumpRows is the most matches the popup shows.
const jumpRows = 10

func (m Model) startJump() (Model, tea.Cmd) {
	if m.openNS == nil {
		m.setStatus(statusInfo, ": jumps to an entity of the open namespace: open one first")
		return m, nil
	}
	m.jump = jumpState{}
	m.stack.Push(CtxJump)
	return m, nil
}

func (m Model) handleJumpKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	switch {
	case key.Matches(msg, keys.JumpCancel):
		m.stack.Pop()
	case key.Matches(msg, keys.JumpOpen):
		matches := m.jumpMatches()
		if len(matches) == 0 || m.openNS == nil {
			return m, nil
		}
		ent := matches[max(0, min(m.jump.cursor, len(matches)-1))]
		m.stack.Pop()
		if !m.selectEntity(ent) {
			// The Entities filter hides it: drop the filter.
			m.entities.filter = ""
			m.entities.applyFilter(m.entityText)
			m.selectEntity(ent)
		}
		return m.openEntityMessages(ent)
	case key.Matches(msg, keys.JumpUp):
		m.jump.cursor = max(0, m.jump.cursor-1)
	case key.Matches(msg, keys.JumpDown):
		m.jump.cursor++
	case key.Matches(msg, keys.HelpErase):
		if r := []rune(m.jump.query); len(r) > 0 {
			m.jump.query, m.jump.cursor = string(r[:len(r)-1]), 0
		}
	case msg.String() == "ctrl+u":
		m.jump.query, m.jump.cursor = "", 0
	case msg.Text != "" && msg.Mod&(tea.ModCtrl|tea.ModAlt) == 0:
		m.jump.query += msg.Text
		m.jump.cursor = 0
	}
	return m, nil
}

// jumpMatches are the open namespace's entities matching the query, best
// first. An empty query lists them all in panel order.
func (m Model) jumpMatches() []bus.Entity {
	if m.jump.query == "" {
		return m.entities.all
	}
	type scored struct {
		e     bus.Entity
		score int
	}
	var out []scored
	for _, e := range m.entities.all {
		if s, ok := fuzzyScore(m.jump.query, e.Path); ok {
			out = append(out, scored{e, s})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return len(out[i].e.Path) < len(out[j].e.Path)
	})
	ents := make([]bus.Entity, len(out))
	for i, s := range out {
		ents[i] = s.e
	}
	return ents
}

// fuzzyScore matches query's runes in order inside target, ignoring case.
// Runs of consecutive runes, matches at the start of a name segment (after
// / - _ .) and a whole-substring match score higher.
func fuzzyScore(query, target string) (int, bool) {
	lq, lt := strings.ToLower(query), strings.ToLower(target)
	q, t := []rune(lq), []rune(lt)
	score, qi, prev := 0, 0, -2
	for ti := 0; ti < len(t) && qi < len(q); ti++ {
		if t[ti] != q[qi] {
			continue
		}
		score++
		if ti == prev+1 {
			score += 4
		}
		if ti == 0 || strings.ContainsRune("/-_.", t[ti-1]) {
			score += 3
		}
		prev = ti
		qi++
	}
	if qi < len(q) {
		return 0, false
	}
	if i := strings.Index(lt, lq); i >= 0 {
		score += 20
		// The byte before a match is ASCII when it is a separator.
		if i == 0 || strings.IndexByte("/-_.", lt[i-1]) >= 0 {
			score += 10
		}
	}
	return score, true
}

func (m Model) overlayJump(screen string) string {
	w := min(m.width-4, 64)
	inner := w - 2
	matches := m.jumpMatches()
	n := max(1, min(jumpRows, m.height-1-6))
	cursor := max(0, min(m.jump.cursor, len(matches)-1))
	offset := max(0, cursor-n+1)

	title := []seg{{"Jump to entity", stTitleFocus}}
	if m.openNS != nil {
		title = append(title, seg{" in " + m.openNS.Name, stDim})
	}
	lines := []string{hBorder(w, "┌", "┐", title, stBorderFocus)}
	lines = append(lines, boxRow(row([]seg{{" : ", stDim}, {m.jump.query, stPlain}, {"▏", stTitleFocus}}, inner, false), stBorderFocus))
	lines = append(lines, stBorderFocus.Render("├"+strings.Repeat("─", inner)+"┤"))
	countW := 1
	for _, e := range matches {
		if e.CountsKnown {
			countW = max(countW, len(strconv.FormatInt(e.DeadLetterCount, 10)))
		}
	}
	for i := range n {
		j := offset + i
		var r string
		switch {
		case j < len(matches):
			e := matches[j]
			count := "?"
			if e.CountsKnown {
				count = strconv.FormatInt(e.DeadLetterCount, 10)
			}
			countText := " DLQ " + padLeft(count, countW) + " "
			mark := "  "
			if j == cursor {
				mark = "▸ "
			}
			r = row([]seg{{mark + fitPlain(e.Path, inner-2-len(countText)), stPlain}, {countText, stDim}}, inner, j == cursor)
		case i == 0 && m.entities.loading && len(m.entities.all) == 0:
			r = row([]seg{{" loading entities…", stDim}}, inner, false)
		case i == 0:
			r = row([]seg{{" no matching entity", stDim}}, inner, false)
		default:
			r = strings.Repeat(" ", inner)
		}
		lines = append(lines, boxRow(r, stBorderFocus))
	}
	footer := "enter open · esc cancel"
	if len(matches) > n {
		footer = strconv.Itoa(len(matches)) + " matches · " + footer
	}
	lines = append(lines, hBorder(w, "└", "┘", []seg{{footer, stDim}}, stBorderFocus))
	return m.overlay(screen, lines, w)
}
