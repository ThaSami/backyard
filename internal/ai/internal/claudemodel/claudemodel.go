// Package claudemodel encodes per-model request rules for Claude models that
// are shared by the anthropic provider (first-party, Vertex AI, Bedrock
// Mantle) and the bedrock provider (Converse API).
//
// Model ids arrive in several spellings: "claude-opus-5-5",
// "claude-sonnet-4-20250514", "claude-3-5-sonnet-20241022",
// "claude-opus-4-5@20251101" (Vertex), "anthropic.claude-opus-5-5" (Bedrock
// Mantle) and "us.anthropic.claude-sonnet-4-5-20250929-v1:0" (Bedrock
// runtime inference profiles). Base normalises them.
package claudemodel

import (
	"strconv"
	"strings"
)

// Version is a parsed Claude model id.
type Version struct {
	Family string // opus | sonnet | haiku | fable | mythos | instant | "" (claude-2)
	Major  int
	Minor  int // 0 when absent
}

// Base returns the canonical Claude id without platform prefixes, dates,
// "@version" or Bedrock "-v1:0" suffixes, e.g. "claude-sonnet-4-5". It
// returns "" when model is not a Claude model.
func Base(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	i := strings.Index(m, "claude")
	if i < 0 {
		return ""
	}
	m = m[i:]
	if j := strings.IndexAny(m, "@:"); j >= 0 {
		m = m[:j]
	}
	parts := strings.Split(m, "-")
	out := parts[:0]
	for _, p := range parts {
		if p == "" {
			continue
		}
		// Dates (20250514) and Bedrock revision tags (v1, v2) end the id.
		if len(p) == 8 && isDigits(p) {
			break
		}
		if len(p) >= 2 && p[0] == 'v' && isDigits(p[1:]) {
			break
		}
		out = append(out, p)
	}
	return strings.Join(out, "-")
}

// Parse parses a Claude model id. ok is false for non-Claude or unparsable ids.
func Parse(model string) (v Version, ok bool) {
	b := Base(model)
	if b == "" {
		return Version{}, false
	}
	parts := strings.Split(b, "-")[1:] // drop "claude"
	var nums []int
	for _, p := range parts {
		if n, err := strconv.Atoi(p); err == nil {
			nums = append(nums, n)
			continue
		}
		if v.Family == "" {
			v.Family = p
		}
	}
	if len(nums) == 0 {
		return Version{}, false
	}
	v.Major = nums[0]
	if len(nums) > 1 {
		v.Minor = nums[1]
	}
	return v, true
}

// AcceptsSampling reports whether the model accepts temperature (and other
// sampling parameters). Claude 3.x and the 4.0–4.6 generations do; Opus 4.7+,
// all 5.x models (including Haiku 5.5), Fable and Mythos reject non-default
// values with a 400. Unknown ids are treated as current models (false), so
// temperature is silently dropped rather than causing an error.
func AcceptsSampling(model string) bool {
	v, ok := Parse(model)
	if !ok {
		return false
	}
	switch v.Family {
	case "fable", "mythos":
		return false
	}
	if v.Major < 4 {
		return true
	}
	return v.Major == 4 && v.Minor <= 6
}

// EffortFor maps a requested effort level to one the model accepts, or ""
// when the model does not support output_config.effort at all (Claude 3.x,
// Opus/Sonnet 4.0/4.1, Sonnet 4.5 and Haiku 4.5). Opus 4.5 supports only
// low/medium/high and the 4.6 generation lacks xhigh; higher levels are
// clamped down. Unknown future models get the level unchanged.
func EffortFor(model, effort string) string {
	if effort == "" {
		return ""
	}
	v, ok := Parse(model)
	if !ok {
		return effort
	}
	switch v.Family {
	case "fable", "mythos":
		return effort
	}
	if v.Major < 4 {
		return ""
	}
	if v.Major == 4 {
		switch {
		case v.Minor <= 1:
			return ""
		case v.Minor == 5:
			if v.Family != "opus" {
				return ""
			}
			if effort == "xhigh" || effort == "max" {
				return "high"
			}
		case v.Minor == 6:
			if effort == "xhigh" {
				return "high"
			}
		case v.Minor < 5:
			return ""
		}
	}
	return effort
}

// EffortBeta is the beta flag that gates output_config.effort on Claude Opus
// 4.5 (effort became GA with the 4.6 generation).
const EffortBeta = "effort-2025-11-24"

// NeedsEffortBeta reports whether sending output_config.effort to model
// requires the EffortBeta flag (Claude Opus 4.5 only).
func NeedsEffortBeta(model string) bool {
	v, ok := Parse(model)
	return ok && v.Family == "opus" && v.Major == 4 && v.Minor == 5
}

// fallbackModels are the models for which server-side refusal fallbacks
// ("fallbacks": "default") are enabled on the first-party API.
var fallbackModels = map[string]bool{
	"claude-opus-5-5":   true,
	"claude-opus-5":     true,
	"claude-fable-5-1":  true,
	"claude-sonnet-5-5": true,
}

// SupportsDefaultFallbacks reports whether the model accepts
// "fallbacks": "default" (beta server-side-fallback-2026-07-01).
func SupportsDefaultFallbacks(model string) bool {
	return fallbackModels[Base(model)]
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
