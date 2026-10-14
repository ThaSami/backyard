package claudemodel

import "testing"

func TestBase(t *testing.T) {
	tests := map[string]string{
		"claude-opus-5-5":                                 "claude-opus-5-5",
		"Claude-Opus-5-5":                                 "claude-opus-5-5",
		"claude-sonnet-4-20250514":                        "claude-sonnet-4",
		"claude-3-5-sonnet-20241022":                      "claude-3-5-sonnet",
		"claude-opus-4-5@20251101":                        "claude-opus-4-5",
		"anthropic.claude-opus-5-5":                       "claude-opus-5-5",
		"us.anthropic.claude-sonnet-4-5-20250929-v1:0":    "claude-sonnet-4-5",
		"anthropic.claude-3-haiku-20240307-v1:0":          "claude-3-haiku",
		"global.anthropic.claude-haiku-4-5-20251001-v1:0": "claude-haiku-4-5",
		"gpt-4o":           "",
		"amazon.nova-pro":  "",
		"claude-fable-5-1": "claude-fable-5-1",
	}
	for in, want := range tests {
		if got := Base(in); got != want {
			t.Errorf("Base(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAcceptsSampling(t *testing.T) {
	tests := map[string]bool{
		"claude-3-5-sonnet-20241022":                   true,
		"claude-3-7-sonnet-latest":                     true,
		"claude-3-opus-20240229":                       true,
		"claude-3-haiku-20240307":                      true,
		"claude-sonnet-4-20250514":                     true,
		"claude-opus-4-0":                              true,
		"claude-opus-4-1":                              true,
		"claude-opus-4-1-20250805":                     true,
		"claude-sonnet-4-5":                            true,
		"claude-haiku-4-5":                             true,
		"claude-opus-4-5@20251101":                     true,
		"claude-opus-4-6":                              true,
		"claude-sonnet-4-6":                            true,
		"us.anthropic.claude-sonnet-4-5-20250929-v1:0": true,
		"claude-opus-4-7":                              false,
		"claude-opus-4-8":                              false,
		"claude-opus-5":                                false,
		"claude-opus-5-5":                              false,
		"claude-sonnet-5":                              false,
		"claude-sonnet-5-5":                            false,
		"claude-haiku-5-5":                             false,
		"claude-fable-5":                               false,
		"claude-fable-5-1":                             false,
		"claude-mythos-5-1":                            false,
		"anthropic.claude-opus-5-5":                    false,
		"some-future-model":                            false,
	}
	for in, want := range tests {
		if got := AcceptsSampling(in); got != want {
			t.Errorf("AcceptsSampling(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestEffortFor(t *testing.T) {
	tests := []struct {
		model, effort, want string
	}{
		{"claude-opus-5-5", "", ""},
		{"claude-opus-5-5", "max", "max"},
		{"claude-opus-5-5", "low", "low"},
		{"claude-fable-5-1", "xhigh", "xhigh"},
		{"claude-haiku-5-5", "medium", "medium"},
		{"claude-opus-4-8", "xhigh", "xhigh"},
		{"claude-opus-4-6", "xhigh", "high"},
		{"claude-opus-4-6", "max", "max"},
		{"claude-sonnet-4-6", "low", "low"},
		{"claude-opus-4-5", "max", "high"},
		{"claude-opus-4-5", "medium", "medium"},
		{"claude-sonnet-4-5", "high", ""},
		{"claude-haiku-4-5", "high", ""},
		{"claude-opus-4-1", "high", ""},
		{"claude-sonnet-4-20250514", "high", ""},
		{"claude-3-7-sonnet-latest", "high", ""},
		{"some-proxy-model", "high", "high"},
	}
	for _, tt := range tests {
		if got := EffortFor(tt.model, tt.effort); got != tt.want {
			t.Errorf("EffortFor(%q, %q) = %q, want %q", tt.model, tt.effort, got, tt.want)
		}
	}
}

func TestSupportsDefaultFallbacks(t *testing.T) {
	tests := map[string]bool{
		"claude-opus-5-5":   true,
		"claude-opus-5":     true,
		"claude-fable-5-1":  true,
		"claude-sonnet-5-5": true,
		"claude-haiku-5-5":  false,
		"claude-opus-4-8":   false,
		"claude-fable-5":    false,
		"claude-sonnet-5":   false,
		"claude-mythos-5-1": false,
	}
	for in, want := range tests {
		if got := SupportsDefaultFallbacks(in); got != want {
			t.Errorf("SupportsDefaultFallbacks(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestNeedsEffortBeta(t *testing.T) {
	tests := map[string]bool{
		"claude-opus-4-5":                            true,
		"claude-opus-4-5-20251101":                   true,
		"claude-opus-4-5@20251101":                   true,
		"anthropic.claude-opus-4-5":                  true,
		"us.anthropic.claude-opus-4-5-20251101-v1:0": true,
		"claude-opus-4-6":                            false,
		"claude-sonnet-4-5":                          false,
		"claude-opus-5-5":                            false,
		"gpt-5":                                      false,
	}
	for in, want := range tests {
		if got := NeedsEffortBeta(in); got != want {
			t.Errorf("NeedsEffortBeta(%q) = %v, want %v", in, got, want)
		}
	}
}
