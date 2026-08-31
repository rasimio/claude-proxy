package main

import (
	"encoding/json"
	"testing"
)

func TestEffortConfig(t *testing.T) {
	tests := []struct {
		in           string
		wantEffort   string
		wantThinking string
	}{
		{"max", "max", "adaptive"},
		{"xhigh", "xhigh", "adaptive"},
		{"high", "high", "adaptive"},
		{"medium", "medium", "adaptive"},
		{"low", "low", "adaptive"},
		{"MAX", "max", "adaptive"}, // clients are not careful about case
		{"  high ", "high", "adaptive"},
		{"minimal", "low", "adaptive"}, // OpenAI's floor has no Anthropic peer
		// Unrecognized values are dropped, not forwarded: Anthropic rejects a
		// bad effort outright, and a typo shouldn't sink a valid request.
		{"ultra", "", ""},
		{"", "", ""},
	}
	for _, tc := range tests {
		effort, thinking := effortConfig(tc.in)
		if effort != tc.wantEffort || thinking != tc.wantThinking {
			t.Errorf("effortConfig(%q) = (%q, %q), want (%q, %q)",
				tc.in, effort, thinking, tc.wantEffort, tc.wantThinking)
		}
	}
}

func TestChatCompletionsCarriesEffort(t *testing.T) {
	req := openaiChatRequest{
		Model:           "claude-opus-5",
		ReasoningEffort: "max",
		Messages: []openaiChatMessage{
			{Role: "user", Content: json.RawMessage(`"hi"`)},
		},
	}
	got, err := openaiToBlueship(req, "claude-opus-5")
	if err != nil {
		t.Fatalf("openaiToBlueship: %v", err)
	}
	if got.Effort != "max" {
		t.Errorf("Effort = %q, want max", got.Effort)
	}
	if got.ThinkingMode != "adaptive" {
		t.Errorf("ThinkingMode = %q, want adaptive", got.ThinkingMode)
	}
}

func TestChatCompletionsWithoutEffortLeavesProviderDefault(t *testing.T) {
	req := openaiChatRequest{
		Model:    "claude-opus-5",
		Messages: []openaiChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
	got, err := openaiToBlueship(req, "claude-opus-5")
	if err != nil {
		t.Fatalf("openaiToBlueship: %v", err)
	}
	// Both empty means blueship sends neither field, so the model keeps
	// whatever it does by default. Defaulting to adaptive here would silently
	// change cost and latency for every existing caller.
	if got.Effort != "" || got.ThinkingMode != "" {
		t.Errorf("got Effort=%q ThinkingMode=%q, want both empty", got.Effort, got.ThinkingMode)
	}
}

func TestResponsesAcceptsBothEffortSpellings(t *testing.T) {
	nested := responsesRequest{Model: "claude-opus-5", Input: json.RawMessage(`"hi"`)}
	nested.Reasoning = &struct {
		Effort string `json:"effort,omitempty"`
	}{Effort: "xhigh"}

	flat := responsesRequest{
		Model:           "claude-opus-5",
		Input:           json.RawMessage(`"hi"`),
		ReasoningEffort: "xhigh",
	}

	for name, req := range map[string]responsesRequest{"nested": nested, "flat": flat} {
		got, _, err := responsesToBlueship(req, "claude-opus-5")
		if err != nil {
			t.Fatalf("%s: responsesToBlueship: %v", name, err)
		}
		if got.Effort != "xhigh" {
			t.Errorf("%s: Effort = %q, want xhigh", name, got.Effort)
		}
		if got.ThinkingMode != "adaptive" {
			t.Errorf("%s: ThinkingMode = %q, want adaptive", name, got.ThinkingMode)
		}
	}
}

func TestResponsesNestedEffortWins(t *testing.T) {
	// A client that sends both is telling us the Responses-shaped one is the
	// considered value; the flat field is the carried-over habit.
	req := responsesRequest{
		Model:           "claude-opus-5",
		Input:           json.RawMessage(`"hi"`),
		ReasoningEffort: "low",
	}
	req.Reasoning = &struct {
		Effort string `json:"effort,omitempty"`
	}{Effort: "max"}

	got, _, err := responsesToBlueship(req, "claude-opus-5")
	if err != nil {
		t.Fatalf("responsesToBlueship: %v", err)
	}
	if got.Effort != "max" {
		t.Errorf("Effort = %q, want max", got.Effort)
	}
}
