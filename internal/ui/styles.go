package ui

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Basic ANSI colours only, so lazybus follows the user's terminal theme
// (lazygit does the same).
var (
	colRed    = lipgloss.Color("1")
	colGreen  = lipgloss.Color("2")
	colYellow = lipgloss.Color("3")
	colBlue   = lipgloss.Color("4")
	colCyan   = lipgloss.Color("6")
	colDim    = lipgloss.Color("8")
)

var (
	stPlain       = lipgloss.NewStyle()
	stBold        = lipgloss.NewStyle().Bold(true)
	stBorder      = lipgloss.NewStyle().Foreground(colDim)
	stBorderFocus = lipgloss.NewStyle().Foreground(colGreen).Bold(true)
	stTitleFocus  = lipgloss.NewStyle().Foreground(colGreen).Bold(true)
	stDim         = lipgloss.NewStyle().Foreground(colDim)
	stSelected    = lipgloss.NewStyle().Background(colBlue).Bold(true)
	stKey         = lipgloss.NewStyle().Foreground(colCyan)
	stErr         = lipgloss.NewStyle().Foreground(colRed)
	stWarn        = lipgloss.NewStyle().Foreground(colYellow)
	stReadOnly    = lipgloss.NewStyle().Foreground(colRed).Bold(true)
)

// seg is a run of text with one style. Rows and titles are built from
// segments so they can be truncated before styling.
type seg struct {
	text  string
	style lipgloss.Style
}

func segsWidth(segs []seg) int {
	w := 0
	for _, s := range segs {
		w += ansi.StringWidth(s.text)
	}
	return w
}

// renderSegs renders segs into at most width cells; overflow is cut with
// "…". It returns the rendered string and its width. If bg is set, every
// segment gets that background. All segment text is sanitized here, so
// message data can never reach the terminal as control sequences.
func renderSegs(segs []seg, width int, bg *lipgloss.Style) (string, int) {
	if width <= 0 {
		return "", 0
	}
	clean := make([]seg, len(segs))
	for i, s := range segs {
		clean[i] = seg{sanitize(s.text), s.style}
	}
	segs = clean
	var b strings.Builder
	style := func(s seg, text string) string {
		if bg != nil {
			// Selected rows are one style: dim columns on the selection
			// background would be unreadable.
			return bg.Render(text)
		}
		return s.style.Render(text)
	}
	if segsWidth(segs) <= width {
		for _, s := range segs {
			if s.text != "" {
				b.WriteString(style(s, s.text))
			}
		}
		return b.String(), segsWidth(segs)
	}
	remaining := width
	for _, s := range segs {
		w := ansi.StringWidth(s.text)
		if w < remaining {
			b.WriteString(style(s, s.text))
			remaining -= w
			continue
		}
		// Measure after truncating: a wide rune that does not fit is
		// dropped whole, so pad what is left.
		t := ansi.Truncate(s.text, remaining-1, "") + "…"
		t += strings.Repeat(" ", max(0, remaining-ansi.StringWidth(t)))
		b.WriteString(style(s, t))
		break
	}
	return b.String(), width
}

// row renders segs into exactly width cells, padding with spaces. With
// selected, the whole row gets the selection background.
func row(segs []seg, width int, selected bool) string {
	var bg *lipgloss.Style
	if selected {
		bg = &stSelected
	}
	s, w := renderSegs(segs, width, bg)
	if pad := width - w; pad > 0 {
		p := strings.Repeat(" ", pad)
		if selected {
			p = stSelected.Render(p)
		}
		s += p
	}
	return s
}

// fitPlain sanitizes plain text and truncates or pads it to exactly width
// cells.
func fitPlain(s string, width int) string {
	if width <= 0 {
		return ""
	}
	s = sanitize(s)
	if ansi.StringWidth(s) > width {
		s = ansi.Truncate(s, width-1, "") + "…"
	}
	return s + strings.Repeat(" ", max(0, width-ansi.StringWidth(s)))
}

// padLeft right-aligns plain text in width cells.
func padLeft(s string, width int) string {
	if w := ansi.StringWidth(s); w < width {
		return strings.Repeat(" ", width-w) + s
	}
	return s
}

// hBorder draws a horizontal box edge of width w: left corner, optional
// title, fill, right corner.
func hBorder(w int, left, right string, title []seg, bs lipgloss.Style) string {
	inner := w - 2
	if inner < 0 {
		return ""
	}
	if len(title) == 0 || inner < 4 {
		return bs.Render(left + strings.Repeat("─", inner) + right)
	}
	t, tw := renderSegs(title, inner-2, nil)
	return bs.Render(left) + " " + t + " " + bs.Render(strings.Repeat("─", inner-2-tw)+right)
}

// boxRow wraps exactly-inner-wide content in side borders.
func boxRow(content string, bs lipgloss.Style) string {
	return bs.Render("│") + content + bs.Render("│")
}
