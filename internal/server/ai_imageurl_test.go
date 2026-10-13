package server

import (
	"encoding/json"
	"testing"
)

// TestParseImagePartOpenAIShape covers the OpenAI-style image part that
// agents commonly produce, which the Cloudflare edition also accepts.
func TestParseImagePartOpenAIShape(t *testing.T) {
	cases := []struct {
		name, body, wantURL string
		wantData            bool
		wantErr             bool
	}{
		{"object", `{"type":"image_url","image_url":{"url":"/__backyard/files/abc/dot.png","detail":"low"}}`, "/__backyard/files/abc/dot.png", false, false},
		{"string", `{"type":"image_url","image_url":"https://example.com/a.png"}`, "https://example.com/a.png", false, false},
		{"data url", `{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}`, "", true, false},
		{"url wins", `{"type":"image","url":"https://example.com/b.png","image_url":{"url":"https://example.com/c.png"}}`, "https://example.com/b.png", false, false},
		{"empty", `{"type":"image_url","image_url":{}}`, "", false, true},
		{"wrong type", `{"type":"image_url","image_url":42}`, "", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var p chatPart
			if err := json.Unmarshal([]byte(tc.body), &p); err != nil {
				t.Fatal(err)
			}
			part, apiErr := parseImagePart(0, 1, p)
			if tc.wantErr {
				if apiErr == nil {
					t.Fatalf("expected an error, got %+v", part)
				}
				return
			}
			if apiErr != nil {
				t.Fatalf("unexpected error: %v", apiErr)
			}
			if part.URL != tc.wantURL || (len(part.Data) > 0) != tc.wantData {
				t.Fatalf("part = url %q data %d bytes", part.URL, len(part.Data))
			}
		})
	}
}
