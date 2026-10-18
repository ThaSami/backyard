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
			pg := ExtractPage([]byte(tc.html))
			if pg.Title != tc.wantTitle || pg.Description != tc.wantDesc || pg.Image != "" {
				t.Errorf("ExtractPage(%q) = %+v, want title %q, description %q", tc.html, pg, tc.wantTitle, tc.wantDesc)
			}
		})
	}
}

func TestExtractTitleOnlyFirst64KiB(t *testing.T) {
	doc := strings.Repeat(" ", TitleScanBytes) + "<title>Too late</title>"
	if pg := ExtractPage([]byte(doc)); pg.Title != "" {
		t.Errorf("title beyond 64 KiB was extracted: %q", pg.Title)
	}
}

func TestExtractTitleLengthCapped(t *testing.T) {
	pg := ExtractPage([]byte("<title>" + strings.Repeat("a", 5000) + "</title><meta name=description content=" + strings.Repeat("b", 5000) + ">"))
	if n := len([]rune(pg.Title)); n != maxTitleRunes {
		t.Errorf("title length = %d, want %d", n, maxTitleRunes)
	}
	if n := len([]rune(pg.Description)); n != maxDescriptionRunes {
		t.Errorf("description length = %d, want %d", n, maxDescriptionRunes)
	}
}

func TestExtractPageImage(t *testing.T) {
	const head = `<title>T</title><meta name="description" content="D">`
	tests := []struct {
		name string
		html string
		want string
	}{
		{"property", `<meta property="og:image" content="preview.png">`, "preview.png"},
		{"name", `<meta name="og:image" content="img/cover.jpg">`, "img/cover.jpg"},
		{"after title and description", head + `<meta property="og:image" content="a.png">`, "a.png"},
		{"before title", `<meta property="og:image" content="a.png">` + head, "a.png"},
		{"far after title and description", head + strings.Repeat("<p>filler</p>", 2000) + `<meta property="og:image" content="late.png">`, "late.png"},
		{"case-insensitive", `<META PROPERTY="OG:Image" CONTENT="Up.PNG">`, "Up.PNG"},
		{"key trimmed", "<meta property=' og:image\n' content='a.png'>", "a.png"},
		{"content trimmed and decoded", "<meta property='og:image' content=' \timg/a&amp;b.png\n\f '>", "img/a&b.png"},
		{"only ASCII whitespace trimmed", "<meta property='og:image' content='&nbsp;a.png'>", "\u00a0a.png"},
		{"numeric entity", `<meta property="og:image" content="&#x2F;a.png">`, "/a.png"},
		{"unquoted", `<meta property=og:image content=/img/a.png>`, "/img/a.png"},
		{"absolute URL kept raw", `<meta property="og:image" content="https://cdn.example.com/a.png">`, "https://cdn.example.com/a.png"},
		{"first wins", `<meta property="og:image" content="one.png"><meta property="og:image" content="two.png">`, "one.png"},
		{"first wins even if empty", `<meta property="og:image" content=""><meta property="og:image" content="two.png">`, ""},
		{"property beats name", `<meta property="og:title" name="og:image" content="x.png"><meta name="og:image" content="y.png">`, "y.png"},
		{"empty property beats name", `<meta property="" name="og:image" content="x.png">`, ""},
		{"same tag as description", `<meta name="description" property="og:image" content="d.png">`, "d.png"},
		{"other og tags ignored", `<meta property="og:image:width" content="1200"><meta property="og:image:url" content="u.png"><meta property="twitter:image" content="t.png">`, ""},
		{"image_src is not og:image", `<link rel="image_src" href="a.png"><meta itemprop="image" content="b.png">`, ""},
		{"in comment", `<!-- <meta property="og:image" content="hidden.png"> --><meta property="og:image" content="shown.png">`, "shown.png"},
		{"in script", `<script>var m = '<meta property="og:image" content="s.png">'</script><meta property="og:image" content="real.png">`, "real.png"},
		{"in noscript", `<noscript><meta property="og:image" content="n.png"></noscript>`, ""},
		{"in template", `<template><meta property="og:image" content="t.png"></template><meta name="og:image" content="ok.png">`, "ok.png"},
		{"in svg", `<svg><meta property="og:image" content="s.png"></svg>`, ""},
		{"in textarea", `<textarea><meta property="og:image" content="x.png"></textarea>`, ""},
		{"none", head, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pg := ExtractPage([]byte(tc.html))
			if pg.Image != tc.want {
				t.Errorf("ExtractPage(%q).Image = %q, want %q", tc.html, pg.Image, tc.want)
			}
		})
	}
	// Title and description are still found when the og:image comes first.
	pg := ExtractPage([]byte(`<meta property="og:image" content="a.png">` + head))
	if pg.Title != "T" || pg.Description != "D" || pg.Image != "a.png" {
		t.Errorf("ExtractPage = %+v", pg)
	}
}

func TestExtractPageImageOnlyFirst64KiB(t *testing.T) {
	tag := `<meta property="og:image" content="a.png">`
	inside := strings.Repeat(" ", TitleScanBytes-len(tag)) + tag
	if pg := ExtractPage([]byte(inside)); pg.Image != "a.png" {
		t.Errorf("og:image ending at 64 KiB: %q, want a.png", pg.Image)
	}
	beyond := strings.Repeat(" ", TitleScanBytes-len(tag)+1) + tag
	if pg := ExtractPage([]byte(beyond)); pg.Image != "" {
		t.Errorf("og:image crossing 64 KiB was extracted: %q", pg.Image)
	}
}

func FuzzExtractPage(f *testing.F) {
	for _, s := range []string{"<title>x</title>", "<meta name=description content='y'>", "<meta property=og:image content=a.png>", "<!--", "<script>", "<title", "<meta name=\"", "<svg/>"} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		pg := ExtractPage(b) // must not panic
		_, _ = ResolvePreviewRef(pg.Image)
	})
}
