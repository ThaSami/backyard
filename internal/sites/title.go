package sites

import (
	"bytes"
	"html"
	"strings"
	"unicode/utf8"
)

const (
	maxTitleRunes       = 256
	maxDescriptionRunes = 1024
)

// rawTextElements hold content that is not markup; a <title> or <meta>
// inside them (e.g. in a script string or an inline SVG) is not the page's.
var rawTextElements = []string{"script", "style", "svg", "template", "textarea", "noscript", "math"}

// ExtractTitle returns the <title> text and <meta name="description"> content
// of an HTML document (best effort; only the first 64 KiB are considered).
//
// The scan is tolerant of malformed markup: comments and the contents of
// script/style/svg/template elements are skipped, entities are decoded and
// whitespace is collapsed. Results are capped at a sensible length.
func ExtractTitle(doc []byte) (title, description string) {
	if len(doc) > TitleScanBytes {
		doc = doc[:TitleScanBytes]
	}
	lower := asciiLower(doc)
	var haveTitle, haveDesc bool
	i := 0
	for i < len(doc) && !(haveTitle && haveDesc) {
		lt := bytes.IndexByte(lower[i:], '<')
		if lt < 0 {
			break
		}
		i += lt
		rest := lower[i:]
		switch {
		case bytes.HasPrefix(rest, []byte("<!--")):
			end := bytes.Index(rest[4:], []byte("-->"))
			if end < 0 {
				return title, description
			}
			i += 4 + end + 3
			continue
		case tagIs(rest, "title"):
			start := tagEnd(lower, i)
			if start < 0 {
				return title, description
			}
			end := bytes.Index(lower[start:], []byte("</title"))
			if end < 0 {
				end = len(doc) - start // unterminated: take what we have
			}
			if !haveTitle {
				title = cleanText(doc[start:start+end], maxTitleRunes)
				haveTitle = true
			}
			i = start + end
			continue
		case tagIs(rest, "meta"):
			end := tagEnd(lower, i)
			if end < 0 {
				return title, description
			}
			if !haveDesc {
				attrs := parseAttrs(doc[i+len("<meta") : end-1])
				if strings.EqualFold(strings.TrimSpace(attrs["name"]), "description") {
					description = cleanText([]byte(attrs["content"]), maxDescriptionRunes)
					haveDesc = true
				}
			}
			i = end
			continue
		}
		skipped := false
		for _, el := range rawTextElements {
			if tagIs(rest, el) {
				start := tagEnd(lower, i)
				if start < 0 {
					return title, description
				}
				if (el == "svg" || el == "math") && start >= 2 && lower[start-2] == '/' {
					i = start // self-closing foreign element
					skipped = true
					break
				}
				end := bytes.Index(lower[start:], []byte("</"+el))
				if end < 0 {
					return title, description
				}
				i = start + end + 2 + len(el)
				skipped = true
				break
			}
		}
		if !skipped {
			i++
		}
	}
	return title, description
}

// tagIs reports whether s (lowercased, starting at '<') opens element name.
func tagIs(s []byte, name string) bool {
	if len(s) < 1+len(name) || string(s[1:1+len(name)]) != name {
		return false
	}
	if len(s) == 1+len(name) {
		return true
	}
	switch s[1+len(name)] {
	case '>', '/', ' ', '\t', '\n', '\r', '\f':
		return true
	}
	return false
}

// tagEnd returns the index just past the '>' closing the tag starting at i,
// honouring quoted attribute values, or -1 when the tag is unterminated.
func tagEnd(s []byte, i int) int {
	var quote byte
	for j := i + 1; j < len(s); j++ {
		c := s[j]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '>':
			return j + 1
		}
	}
	return -1
}

// parseAttrs parses the attribute list of a start tag (names lowercased;
// values raw, entity-encoded).
func parseAttrs(s []byte) map[string]string {
	attrs := map[string]string{}
	i := 0
	isSpace := func(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '/' }
	for i < len(s) {
		for i < len(s) && isSpace(s[i]) {
			i++
		}
		start := i
		for i < len(s) && !isSpace(s[i]) && s[i] != '=' {
			i++
		}
		name := strings.ToLower(string(s[start:i]))
		for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
			i++
		}
		val := ""
		if i < len(s) && s[i] == '=' {
			i++
			for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
				i++
			}
			if i < len(s) && (s[i] == '"' || s[i] == '\'') {
				q := s[i]
				i++
				vs := i
				for i < len(s) && s[i] != q {
					i++
				}
				val = string(s[vs:i])
				if i < len(s) {
					i++
				}
			} else {
				// Unquoted values end at whitespace only: "/" is part of
				// the value (content=https://example.com/x).
				vs := i
				for i < len(s) && !isWhitespace(s[i]) {
					i++
				}
				val = string(s[vs:i])
			}
		}
		if name == "" {
			if i == start {
				i++ // stray character
			}
			continue
		}
		if _, dup := attrs[name]; !dup {
			attrs[name] = val
		}
	}
	return attrs
}

// cleanText decodes entities, makes the text valid UTF-8, collapses
// whitespace and caps the length at maxRunes.
func cleanText(b []byte, maxRunes int) string {
	s := strings.ToValidUTF8(string(b), "�")
	s = html.UnescapeString(s)
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > maxRunes {
		r := []rune(s)
		s = strings.TrimSpace(string(r[:maxRunes-1])) + "…"
	}
	return s
}

// asciiLower lowercases ASCII letters only, so byte offsets are preserved.
func asciiLower(b []byte) []byte {
	out := make([]byte, len(b))
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return out
}

// isWhitespace reports HTML ASCII whitespace.
func isWhitespace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f'
}
