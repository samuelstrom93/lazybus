// Command gallery renders every golden screen into one self-contained HTML
// page, converting ANSI styling to HTML spans.
//
//	go run ./tools/gallery -out <dir>
package main

import (
	"flag"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

func main() {
	in := flag.String("in", "internal/ui/testdata", "directory with *.golden files")
	out := flag.String("out", "", "output directory for index.html (required)")
	title := flag.String("title", "lazybus golden screens", "page title")
	flag.Parse()
	if *out == "" {
		fmt.Fprintln(os.Stderr, "gallery: -out is required")
		os.Exit(2)
	}
	if err := run(*in, *out, *title); err != nil {
		fmt.Fprintln(os.Stderr, "gallery:", err)
		os.Exit(1)
	}
}

type screen struct {
	name          string
	width, height int
	html          string
}

func run(in, out, title string) error {
	paths, err := filepath.Glob(filepath.Join(in, "*.golden"))
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return fmt.Errorf("no *.golden files in %s", in)
	}
	sort.Strings(paths)
	var screens []screen
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		s := string(b)
		lines := strings.Split(s, "\n")
		w := 0
		for _, l := range lines {
			w = max(w, ansi.StringWidth(l))
		}
		screens = append(screens, screen{
			name:   strings.TrimSuffix(filepath.Base(p), ".golden"),
			width:  w,
			height: len(lines),
			html:   ansiToHTML(s),
		})
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	dst := filepath.Join(out, "index.html")
	if err := os.WriteFile(dst, []byte(page(title, in, screens)), 0o644); err != nil {
		return err
	}
	fmt.Println(dst)
	return nil
}

const css = `
body{background:#0d1117;color:#c9d1d9;font:14px system-ui;margin:0;padding:16px}
h1{font-size:18px} h2{font-size:14px;color:#8b949e;margin:28px 0 6px}
h2 .size{color:#6e7681;font-weight:400;margin-left:8px}
nav{display:flex;flex-wrap:wrap;gap:6px 12px;font-size:12px} a{color:#58a6ff}
pre{font:13px/1.15 "JetBrains Mono","DejaVu Sans Mono",Menlo,monospace;background:#010409;color:#c9d1d9;border:1px solid #30363d;padding:8px;overflow-x:auto;margin:0;width:max-content;max-width:100%;box-sizing:border-box}
.b{font-weight:700}.f{opacity:.6}.i{font-style:italic}.u{text-decoration:underline}
`

func page(title, in string, screens []screen) string {
	var b strings.Builder
	b.WriteString(`<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">`)
	fmt.Fprintf(&b, "\n<title>%s</title><style>%s</style>", html.EscapeString(title), css)
	fmt.Fprintf(&b, "<h1>%s</h1><p>%d golden screens from <code>%s</code>. Fake data, fixed clock, UTC.</p><nav>",
		html.EscapeString(title), len(screens), html.EscapeString(in))
	for _, s := range screens {
		fmt.Fprintf(&b, `<a href="#%s">%s</a>`, s.name, html.EscapeString(s.name))
	}
	b.WriteString("</nav>")
	for _, s := range screens {
		fmt.Fprintf(&b, `<section id="%s"><h2>%s<span class="size">%d×%d</span></h2><pre>%s</pre></section>`,
			s.name, html.EscapeString(s.name), s.width, s.height, s.html)
	}
	b.WriteString("\n</html>\n")
	return b.String()
}

// --- ANSI → HTML -------------------------------------------------------------

// palette is the 16-colour terminal palette (GitHub dark).
var palette = [16]string{
	"#484f58", "#ff7b72", "#3fb950", "#d29922", "#1f6feb", "#bc8cff", "#39c5cf", "#b1bac4",
	"#6e7681", "#ffa198", "#56d364", "#e3b341", "#79c0ff", "#d2a8ff", "#56d4dd", "#f0f6fc",
}

const (
	defaultFG = "#c9d1d9"
	defaultBG = "#010409"
)

type sgr struct {
	fg, bg                                 string
	bold, faint, italic, underline, invert bool
}

func (s sgr) open() string {
	fg, bg := s.fg, s.bg
	if s.invert {
		fg, bg = or(bg, defaultBG), or(fg, defaultFG)
	}
	var cls, style []string
	if s.bold {
		cls = append(cls, "b")
	}
	if s.faint {
		cls = append(cls, "f")
	}
	if s.italic {
		cls = append(cls, "i")
	}
	if s.underline {
		cls = append(cls, "u")
	}
	if fg != "" {
		style = append(style, "color:"+fg)
	}
	if bg != "" {
		style = append(style, "background:"+bg)
	}
	if len(cls) == 0 && len(style) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<span")
	if len(cls) > 0 {
		fmt.Fprintf(&b, ` class="%s"`, strings.Join(cls, " "))
	}
	if len(style) > 0 {
		fmt.Fprintf(&b, ` style="%s"`, strings.Join(style, ";"))
	}
	b.WriteString(">")
	return b.String()
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func ansiToHTML(s string) string {
	var b strings.Builder
	var cur sgr
	open := ""
	setStyle := func(next sgr) {
		if next == cur {
			return
		}
		if open != "" {
			b.WriteString("</span>")
		}
		cur = next
		open = cur.open()
		b.WriteString(open)
	}
	for i := 0; i < len(s); {
		if s[i] != 0x1b {
			j := strings.IndexByte(s[i:], 0x1b)
			if j < 0 {
				j = len(s) - i
			}
			b.WriteString(html.EscapeString(s[i : i+j]))
			i += j
			continue
		}
		// ESC sequence
		if i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
				j++
			}
			if j < len(s) && s[j] == 'm' {
				setStyle(applySGR(cur, s[i+2:j]))
			}
			i = j + 1
			continue
		}
		if i+1 < len(s) && s[i+1] == ']' { // OSC: skip to BEL or ST
			j := i + 2
			for j < len(s) && s[j] != 0x07 && !(s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\') {
				j++
			}
			if j < len(s) && s[j] == 0x1b {
				j++
			}
			i = j + 1
			continue
		}
		i += 2
	}
	if open != "" {
		b.WriteString("</span>")
	}
	return b.String()
}

func applySGR(s sgr, params string) sgr {
	if params == "" {
		return sgr{}
	}
	ps := strings.FieldsFunc(params, func(r rune) bool { return r == ';' || r == ':' })
	n := func(i int) int {
		if i >= len(ps) {
			return 0
		}
		v, _ := strconv.Atoi(ps[i])
		return v
	}
	for i := 0; i < len(ps); i++ {
		switch p := n(i); {
		case p == 0:
			s = sgr{}
		case p == 1:
			s.bold = true
		case p == 2:
			s.faint = true
		case p == 3:
			s.italic = true
		case p == 4:
			s.underline = true
		case p == 7:
			s.invert = true
		case p == 22:
			s.bold, s.faint = false, false
		case p == 23:
			s.italic = false
		case p == 24:
			s.underline = false
		case p == 27:
			s.invert = false
		case p >= 30 && p <= 37:
			s.fg = palette[p-30]
		case p == 39:
			s.fg = ""
		case p >= 40 && p <= 47:
			s.bg = palette[p-40]
		case p == 49:
			s.bg = ""
		case p >= 90 && p <= 97:
			s.fg = palette[p-90+8]
		case p >= 100 && p <= 107:
			s.bg = palette[p-100+8]
		case p == 38 || p == 48:
			var c string
			switch n(i + 1) {
			case 5:
				c = color256(n(i + 2))
				i += 2
			case 2:
				c = fmt.Sprintf("#%02x%02x%02x", n(i+2), n(i+3), n(i+4))
				i += 4
			}
			if p == 38 {
				s.fg = c
			} else {
				s.bg = c
			}
		}
	}
	return s
}

func color256(i int) string {
	switch {
	case i < 16:
		return palette[max(0, i)]
	case i < 232:
		i -= 16
		lv := func(v int) int {
			if v == 0 {
				return 0
			}
			return 55 + v*40
		}
		return fmt.Sprintf("#%02x%02x%02x", lv(i/36), lv(i/6%6), lv(i%6))
	case i < 256:
		v := 8 + (i-232)*10
		return fmt.Sprintf("#%02x%02x%02x", v, v, v)
	}
	return ""
}
