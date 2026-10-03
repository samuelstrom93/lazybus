package ui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/samuelstrom93/lazybus/internal/bus"
)

// bodyText returns the body as display text: pretty JSON when it parses,
// raw text otherwise. Tabs become spaces and control characters are dropped
// so nothing in a message can move the terminal cursor.
func bodyText(body []byte) (text string, isJSON bool) {
	if json.Valid(body) {
		var b bytes.Buffer
		if err := json.Indent(&b, body, "", "  "); err == nil {
			return b.String(), true
		}
	}
	s := strings.ToValidUTF8(string(body), "�")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\t", "    ")
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r >= 0x20 && r != 0x7f && (r < 0x80 || r > 0x9f) {
			return r
		}
		return -1
	}, s)
	return strings.TrimRight(s, "\n"), false
}

// wrapLines splits text into lines and wraps each to width cells.
// Continuation lines keep the line's indentation (up to half the width), so
// wrapped JSON stays readable.
func wrapLines(text string, width int) []string {
	if width < 1 {
		width = 1
	}
	var out []string
	for line := range strings.SplitSeq(text, "\n") {
		if ansi.StringWidth(line) <= width {
			out = append(out, line)
			continue
		}
		rest := strings.TrimLeft(line, " ")
		indent := min(len(line)-len(rest), width/2)
		pad := strings.Repeat(" ", indent)
		for _, w := range strings.Split(ansi.Wrap(rest, width-indent, ""), "\n") {
			out = append(out, pad+strings.TrimRight(w, " "))
		}
	}
	return out
}

// propertyValue formats an application property value.
func propertyValue(p bus.Property) string {
	switch v := p.Value.(type) {
	case nil:
		return "null"
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64)
	case time.Time:
		return v.Format(time.RFC3339Nano)
	default:
		return fmt.Sprint(v)
	}
}

// listTime formats an enqueued time for a list row: HH:MM today, MM-DD
// otherwise. Always 5 cells.
func listTime(t, now time.Time, loc *time.Location) string {
	t, now = t.In(loc), now.In(loc)
	if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
		return t.Format("15:04")
	}
	return t.Format("01-02")
}

// fullTime formats a time for the System tab.
func fullTime(t time.Time, loc *time.Location) string {
	if t.IsZero() {
		return ""
	}
	return t.In(loc).Format("2006-01-02 15:04:05 MST")
}

// ttlText formats a time-to-live; whole days are shown as days.
func ttlText(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	day := 24 * time.Hour
	if d%day == 0 {
		return fmt.Sprintf("%dd", d/day)
	}
	return d.String()
}
