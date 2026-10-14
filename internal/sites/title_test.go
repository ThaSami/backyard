package sites

import (
	"strings"
	"testing"
)

func TestExtractTitle(t *testing.T) {
	tests := []struct {
		name      string
		html      string
		wantTitle string
		wantDesc  string
	}{
		{"empty", "", "", ""},
		{"plain", "<html><head><title>Lunch vote</title></head></html>", "Lunch vote", ""},
		{"uppercase tags", "<HTML><HEAD><TITLE>Shout</TITLE><META NAME=\"Description\" CONTENT=\"Loud\"></HEAD>", "Shout", "Loud"},
		{"entities", "<title>Tom &amp; Jerry &#8212; &lt;3 &quot;x&quot;</title>", `Tom & Jerry — <3 "x"`, ""},
		{"whitespace collapsed", "<title>\n   Multi\n\tline   title \n</title>", "Multi line title", ""},
		{"attributes on title", `<title id="t" data-x='>'>Attr</title>`, "Attr", ""},
		{"description double quotes", `<meta name="description" content="A tool for voting on lunch.">`, "", "A tool for voting on lunch."},
		{"description single quotes", `<meta content='Single &amp; quoted' name='description' />`, "", "Single & quoted"},
		{"description unquoted", `<meta name=description content=Unquoted>`, "", "Unquoted"},
		{"unquoted value with slashes", `<meta name=description content=https://example.com/a/b>`, "", "https://example.com/a/b"},
		{"self-closing meta", `<meta name="description" content="Closed"/>`, "", "Closed"},
		{"other meta ignored", `<meta charset="utf-8"><meta name="viewport" content="width=device-width"><meta name="description" content="Real">`, "", "Real"},
		{"first wins", "<title>One</title><title>Two</title><meta name=description content=a><meta name=description content=b>", "One", "a"},
		{"comment skipped", "<!-- <title>Hidden</title> --><title>Shown</title>", "Shown", ""},
		{"script skipped", `<script>document.write("<title>Nope</title>")</script><title>Yes</title>`, "Yes", ""},
		{"svg title skipped", `<body><svg><title>Icon</title></svg><title>Page</title>`, "Page", ""},
		{"self-closing svg", `<svg/><title>After svg</title>`, "After svg", ""},
		{"titlebar is not title", "<titlebar>x</titlebar><title>Real</title>", "Real", ""},
		{"unterminated title", "<title>Partial", "Partial", ""},
		{"unterminated comment", "<!-- <title>x</title>", "", ""},
		{"invalid utf8", "<title>bad\xffbyte</title>", "bad�byte", ""},
		{"no title", "<html><body><h1>Hello</h1></body></html>", "", ""},
		{"meta before title", `<meta name="description" content="d"><title>t</title>`, "t", "d"},
		{"empty title", "<title></title><meta name=description content=''>", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			title, desc := ExtractTitle([]byte(tc.html))
			if title != tc.wantTitle || desc != tc.wantDesc {
				t.Errorf("ExtractTitle(%q) = (%q, %q), want (%q, %q)", tc.html, title, desc, tc.wantTitle, tc.wantDesc)
			}
		})
	}
}

func TestExtractTitleOnlyFirst64KiB(t *testing.T) {
	doc := strings.Repeat(" ", TitleScanBytes) + "<title>Too late</title>"
	if title, _ := ExtractTitle([]byte(doc)); title != "" {
		t.Errorf("title beyond 64 KiB was extracted: %q", title)
	}
}

func TestExtractTitleLengthCapped(t *testing.T) {
	title, desc := ExtractTitle([]byte("<title>" + strings.Repeat("a", 5000) + "</title><meta name=description content=" + strings.Repeat("b", 5000) + ">"))
	if n := len([]rune(title)); n != maxTitleRunes {
		t.Errorf("title length = %d, want %d", n, maxTitleRunes)
	}
	if n := len([]rune(desc)); n != maxDescriptionRunes {
		t.Errorf("description length = %d, want %d", n, maxDescriptionRunes)
	}
}

func FuzzExtractTitle(f *testing.F) {
	for _, s := range []string{"<title>x</title>", "<meta name=description content='y'>", "<!--", "<script>", "<title", "<meta name=\"", "<svg/>"} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = ExtractTitle(b) // must not panic
	})
}
