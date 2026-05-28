package main

import (
	"errors"
	"strings"
	"time"

	bs "github.com/rasimio/blueship/core"
)

// openaiChatRequest is a subset of the OpenAI Chat Completions schema that
// covers what n8n's OpenAI Chat Model node actually sends. Fields we don't
// translate (tools, tool_choice, response_format, …) are accepted and ignored
// so that requests with extra keys still work — proxy must be permissive.
type openaiChatRequest struct {
	Model       string              `json:"model"`
	Messages    []openaiChatMessage `json:"messages"`
	MaxTokens   int                 `json:"max_tokens,omitempty"`
	Temperature float64             `json:"temperature,omitempty"`
	Stream      bool                `json:"stream,omitempty"`
}

type openaiChatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string OR []{type,text,…}
	Name    string `json:"name,omitempty"`
}

// openaiToBlueship converts an OpenAI Chat Completions request into a
// blueship CompletionRequest. system messages are concatenated into the
// dedicated System field (Anthropic expects it separate from the message
// turns); user/assistant turns are passed through as plain text; tool-role
// messages get folded back into a user turn with a "[tool result]" prefix
// because the OpenAI tool-call format does not round-trip cleanly into
// Anthropic's tool_use/tool_result blocks without also translating the
// preceding assistant tool_calls — out of scope for v1.
func openaiToBlueship(req openaiChatRequest, defaultModel string) (bs.CompletionRequest, error) {
	var systemParts []string
	messages := make([]bs.Message, 0, len(req.Messages))

	for _, m := range req.Messages {
		text := flattenContent(m.Content)
		switch m.Role {
		case "system", "developer":
			if text != "" {
				systemParts = append(systemParts, text)
			}
		case "user":
			messages = append(messages, bs.Message{Role: "user", Content: text})
		case "assistant":
			if text == "" {
				continue // skip empty assistant turns from prior tool_call rounds
			}
			messages = append(messages, bs.Message{Role: "assistant", Content: text})
		case "tool", "function":
			messages = append(messages, bs.Message{Role: "user", Content: "[tool result] " + text})
		default:
			// unknown role — pass as user
			messages = append(messages, bs.Message{Role: "user", Content: text})
		}
	}

	if len(messages) == 0 {
		return bs.CompletionRequest{}, errors.New("no user/assistant messages provided")
	}

	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = defaultModel
	}

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 4096
	}

	return bs.CompletionRequest{
		Model:       model,
		System:      strings.Join(systemParts, "\n\n"),
		Messages:    messages,
		MaxTokens:   maxTokens,
		Temperature: req.Temperature,
	}, nil
}

// flattenContent extracts plain text from OpenAI's flexible content schema.
// A content field is either a string, or an array of parts each like
// {"type":"text","text":"…"} (vision parts are dropped for v1).
func flattenContent(c any) string {
	switch v := c.(type) {
	case nil:
		return ""
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := m["type"].(string); t == "text" {
				if s, ok := m["text"].(string); ok {
					parts = append(parts, s)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

// openaiChatResponse is the non-streaming response envelope.
type openaiChatResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []openaiChoice `json:"choices"`
	Usage   openaiUsage    `json:"usage"`
}

type openaiChoice struct {
	Index        int                 `json:"index"`
	Message      openaiOutputMessage `json:"message"`
	FinishReason string              `json:"finish_reason"`
}

type openaiOutputMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func blueshipToOpenAI(resp *bs.CompletionResponse, model string) openaiChatResponse {
	text := bs.ExtractText(resp.Content)
	return openaiChatResponse{
		ID:      "chatcmpl-" + randomID(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []openaiChoice{{
			Index:        0,
			Message:      openaiOutputMessage{Role: "assistant", Content: text},
			FinishReason: mapStopReason(resp.StopReason),
		}},
		Usage: openaiUsage{
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
			TotalTokens:      resp.Usage.InputTokens + resp.Usage.OutputTokens,
		},
	}
}

// mapStopReason translates Anthropic stop_reason values into OpenAI
// finish_reason. n8n and most clients only branch on "stop"/"length" so
// anything unexpected gets normalised to "stop".
func mapStopReason(r string) string {
	switch r {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return "stop"
	}
}
