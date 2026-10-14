package templates

import (
	"fmt"
	"maps"
	"math"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestColorContrast checks the colors of everything `backyard init` writes —
// the starter pages and, through the copied skill, every recipe and snippet
// agents copy into new sites — against WCAG AA: text (including text dimmed
// with opacity and SVG labels) must reach 4.5:1 against its background in
// both light and dark mode, and the per-person palettes used behind white
// text must too.
func TestColorContrast(t *testing.T) {
	docs := map[string]string{} // name → HTML or CSS
	for _, tmpl := range Templates() {
		files := mustRender(t, InitOptions{Site: "contrast", Template: tmpl, Agents: KnownAgents()})
		for name, b := range files {
			label := tmpl + ":" + name
			for _, bad := range lowContrast {
				if strings.Contains(strings.ToLower(string(b)), bad) {
					t.Errorf("%s uses %s, a color that fails WCAG AA contrast in its role", label, bad)
				}
			}
			switch path.Ext(name) {
			case ".html", ".css":
				docs[label] = string(b)
			case ".md", ".mdc":
				for i, m := range fenceRe.FindAllStringSubmatch(string(b), -1) {
					docs[fmt.Sprintf("%s#%s-%d", label, m[1], i+1)] = m[2]
				}
			}
		}
	}
	checked := 0
	for _, name := range slices.Sorted(maps.Keys(docs)) {
		doc := docs[name]
		css := doc
		if strings.HasSuffix(name, ".html") || strings.Contains(name, "#html-") {
			css = ""
			for _, m := range styleRe.FindAllStringSubmatch(doc, -1) {
				css += m[1] + "\n"
			}
		}
		for _, p := range checkContrast(css, doc) {
			t.Errorf("%s: %s", name, p)
		}
		checked++
	}
	if checked < 20 {
		t.Fatalf("only %d documents checked", checked)
	}
}

// lowContrast are colors once used by the recipes that fail WCAG AA in
// their roles (accent behind white text, palettes behind white names).
var lowContrast = []string{"#2a78d6", "#1baf7a", "#e87ba4", "#eb6834", "#eda100"}

var (
	fenceRe   = regexp.MustCompile("(?s)```(html|css)\n(.*?)```")
	styleRe   = regexp.MustCompile(`(?s)<style>(.*?)</style>`)
	commentRe = regexp.MustCompile(`(?s)/\*.*?\*/`)
	varRe     = regexp.MustCompile(`^var\(\s*(--[\w-]+)\s*(?:,\s*(.+))?\)$`)
	hexRe     = regexp.MustCompile(`^#([0-9a-fA-F]{3}|[0-9a-fA-F]{6})$`)
	svgFillRe = regexp.MustCompile(`<text\b[^>]*\sfill="([^"]+)"`)
	paletteRe = regexp.MustCompile(`\[\s*"#[0-9a-fA-F]{6}"(?:\s*,\s*"#[0-9a-fA-F]{6}"){2,}\s*\]`)
	colorRe   = regexp.MustCompile(`#[0-9a-fA-F]{6}`)
)

// scriptStyled lists selectors whose background is set by the page's
// script (from a palette checked separately), not by the stylesheet.
var scriptStyled = []string{".cursor span"}

type cssRule struct {
	media, selector string
	decls           map[string]string
}

// parseCSS returns the style rules of css with their enclosing @media
// prelude; other at-rules (@keyframes…) are skipped.
func parseCSS(css, media string) []cssRule {
	var rules []cssRule
	css = commentRe.ReplaceAllString(css, "")
	for {
		open := strings.IndexByte(css, '{')
		if open < 0 {
			return rules
		}
		depth, end := 0, -1
		for i := open; i < len(css) && end < 0; i++ {
			switch css[i] {
			case '{':
				depth++
			case '}':
				if depth--; depth == 0 {
					end = i
				}
			}
		}
		if end < 0 {
			return rules
		}
		prelude, body := strings.TrimSpace(css[:open]), css[open+1:end]
		css = css[end+1:]
		switch {
		case strings.HasPrefix(prelude, "@media"):
			rules = append(rules, parseCSS(body, prelude)...)
		case strings.HasPrefix(prelude, "@"):
		default:
			decls := map[string]string{}
			for d := range strings.SplitSeq(body, ";") {
				if k, v, ok := strings.Cut(d, ":"); ok {
					decls[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "!important"))
				}
			}
			rules = append(rules, cssRule{media, prelude, decls})
		}
	}
}

func (r cssRule) applies(dark bool) bool {
	if !strings.Contains(r.media, "prefers-color-scheme") {
		return true
	}
	return strings.Contains(r.media, "dark") == dark
}

// checkContrast returns the contrast problems of a stylesheet and the
// document it styles (SVG labels, palettes).
func checkContrast(css, doc string) []string {
	var problems []string
	rules := parseCSS(css, "")
	darkToo := false
	for _, r := range rules {
		if r.selector == ":root" && strings.Contains(r.decls["color-scheme"], "dark") {
			darkToo = true
		}
	}
	modes := []bool{false}
	if darkToo {
		modes = append(modes, true)
	}
	for _, dark := range modes {
		mode := "light"
		// The darkest text and lightest background browsers use by default.
		canvas, canvasText := "ffffff", "000000"
		if dark {
			mode, canvas, canvasText = "dark", "1e1e1e", "ffffff"
		}
		vars := map[string]string{}
		for _, r := range rules {
			if r.selector == ":root" && r.applies(dark) {
				for k, v := range r.decls {
					if strings.HasPrefix(k, "--") {
						vars[k] = v
					}
				}
			}
		}
		pageBg, pageText := canvas, canvasText
		for _, r := range rules {
			if r.selector == "body" && r.applies(dark) {
				if c := resolveColor(firstOf(r.decls, "background", "background-color"), vars); c != "" {
					pageBg = c
				}
				if c := resolveColor(r.decls["color"], vars); c != "" {
					pageText = c
				}
			}
		}
		backdrops := []string{pageBg}
		if c := resolveColor("var(--surface)", vars); c != "" && c != pageBg {
			backdrops = append(backdrops, c)
		}
		for _, r := range rules {
			if !r.applies(dark) || r.selector == ":root" || slices.Contains(scriptStyled, r.selector) ||
				strings.Contains(r.selector, ":disabled") { // inactive controls are exempt (WCAG 1.4.3)
				continue
			}
			opacity := 1.0
			if o, err := strconv.ParseFloat(r.decls["opacity"], 64); err == nil {
				opacity = o
			}
			raw, hasColor := r.decls["color"]
			if !hasColor && opacity == 1 {
				continue
			}
			text := pageText
			if hasColor {
				if text = resolveColor(raw, vars); text == "" {
					continue // inherit, currentColor…
				}
			}
			bgs := backdrops
			if c := resolveColor(firstOf(r.decls, "background", "background-color"), vars); c != "" {
				bgs = []string{c}
			}
			for _, bg := range bgs {
				if ratio := contrast(blend(text, bg, opacity), bg); ratio < 4.5 {
					problems = append(problems, fmt.Sprintf("%s (%s): text #%s (opacity %.2g) on #%s has contrast %.2f:1, want ≥ 4.5:1",
						r.selector, mode, text, opacity, bg, ratio))
				}
			}
		}
		for _, m := range svgFillRe.FindAllStringSubmatch(doc, -1) {
			fill := resolveColor(m[1], vars)
			for _, bg := range backdrops {
				if fill != "" && contrast(fill, bg) < 4.5 {
					problems = append(problems, fmt.Sprintf("SVG text fill %s (%s): #%s on #%s has contrast %.2f:1, want ≥ 4.5:1",
						m[1], mode, fill, bg, contrast(fill, bg)))
				}
			}
		}
	}
	for _, p := range paletteRe.FindAllString(doc, -1) {
		for _, c := range colorRe.FindAllString(p, -1) {
			if r := contrast("ffffff", strings.ToLower(c[1:])); r < 4.5 {
				problems = append(problems, fmt.Sprintf("palette color %s: white text has contrast %.2f:1, want ≥ 4.5:1", c, r))
			}
		}
	}
	return problems
}

func firstOf(m map[string]string, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			return v
		}
	}
	return ""
}

// resolveColor returns an opaque color as "rrggbb", following var()
// references; "" for anything else (translucent, keywords, functions).
func resolveColor(v string, vars map[string]string) string {
	for range 10 {
		v = strings.TrimSpace(v)
		m := varRe.FindStringSubmatch(v)
		if m == nil {
			break
		}
		if val, ok := vars[m[1]]; ok {
			v = val
		} else {
			v = m[2]
		}
	}
	switch strings.ToLower(v) {
	case "white":
		return "ffffff"
	case "black":
		return "000000"
	}
	m := hexRe.FindStringSubmatch(v)
	if m == nil {
		return ""
	}
	h := strings.ToLower(m[1])
	if len(h) == 3 {
		h = string([]byte{h[0], h[0], h[1], h[1], h[2], h[2]})
	}
	return h
}

func rgb(hex string) [3]float64 {
	var c [3]float64
	for i := range c {
		v, err := strconv.ParseUint(hex[2*i:2*i+2], 16, 8)
		if err != nil {
			panic(err)
		}
		c[i] = float64(v)
	}
	return c
}

// blend composites fg at opacity over bg.
func blend(fg, bg string, opacity float64) string {
	if opacity >= 1 {
		return fg
	}
	f, b := rgb(fg), rgb(bg)
	var out strings.Builder
	for i := range f {
		fmt.Fprintf(&out, "%02x", int(math.Round(f[i]*opacity+b[i]*(1-opacity))))
	}
	return out.String()
}

// contrast is the WCAG 2 contrast ratio of two sRGB colors ("rrggbb").
func contrast(a, b string) float64 {
	la, lb := luminance(a), luminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

func luminance(hex string) float64 {
	c := rgb(hex)
	for i, v := range c {
		v /= 255
		if v <= 0.03928 {
			c[i] = v / 12.92
		} else {
			c[i] = math.Pow((v+0.055)/1.055, 2.4)
		}
	}
	return 0.2126*c[0] + 0.7152*c[1] + 0.0722*c[2]
}

// TestCheckContrast makes sure the checker catches what it should.
func TestCheckContrast(t *testing.T) {
	bad := []struct{ css, doc, want string }{
		{`:root { --accent: #2a78d6; } .btn { background: var(--accent); color: #fff; }`, "", ".btn (light)"},
		{`:root { color-scheme: light dark; } #error { color: #d03b3b; }`, "", "#error (dark)"},
		{`:root { color-scheme: light dark; --accent: #2266c8; } a { color: var(--accent); }`, "", "a (dark)"},
		{`.card button { opacity: .5; }`, "", ".card button (light)"},
		{`:root { --muted: #898781; } body { background: #fcfcfb; }`, `<text fill="var(--muted)">1</text>`, "SVG text fill"},
		{``, `const palette = ["#2266c8", "#1baf7a", "#6250d6"];`, "palette color #1baf7a"},
		{`@media (prefers-color-scheme: dark) { :root { --ink: #333; } } :root { color-scheme: light dark; --ink: #222; } p { color: var(--ink); }`, "", "p (dark)"},
	}
	for _, tt := range bad {
		if got := strings.Join(checkContrast(tt.css, tt.doc), "\n"); !strings.Contains(got, tt.want) {
			t.Errorf("checkContrast(%q, %q) = %q, want a problem with %q", tt.css, tt.doc, got, tt.want)
		}
	}
	good := `:root { color-scheme: light dark; --accent: #2266c8; --accent-ink: #fff; }
		@media (prefers-color-scheme: dark) { :root { --accent: #4b93ea; --accent-ink: #0b1b30; } }
		.btn { background: var(--accent); color: var(--accent-ink); } .muted { opacity: .7; } .fill { background: var(--accent); }
		@keyframes in { from { opacity: 0; } }`
	if got := checkContrast(good, `const palette = ["#2266c8", "#c4501d", "#0f7f57"];`); len(got) != 0 {
		t.Errorf("false positives: %q", got)
	}
}
