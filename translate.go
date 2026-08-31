package main

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	bs "github.com/rasimio/blueship"
)

// openaiChatRequest covers the OpenAI Chat Completions fields we translate:
// system/user/assistant/tool messages, tools + tool_choice, response_format,
// vision (image_url content parts), max_tokens, temperature, stream. Unknown
// fields are accepted and ignored.
type openaiChatRequest struct {
	Model          string                `json:"model"`
	Messages       []openaiChatMessage   `json:"messages"`
	MaxTokens      int                   `json:"max_tokens,omitempty"`
	Temperature    float64               `json:"temperature,omitempty"`
	Stream         bool                  `json:"stream,omitempty"`
	Tools          []openaiTool          `json:"tools,omitempty"`
	ToolChoice     json.RawMessage       `json:"tool_choice,omitempty"`
	ResponseFormat *openaiResponseFormat `json:"response_format,omitempty"`

	// OpenAI's own name for the knob, so a client that already speaks
	// reasoning_effort needs no special-casing to drive Claude's.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

type openaiChatMessage struct {
	Role       string           `json:"role"`
	Content    json.RawMessage  `json:"content,omitempty"` // string OR array of parts OR null (assistant with tool_calls)
	Name       string           `json:"name,omitempty"`
	ToolCalls  []openaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openaiTool struct {
	Type     string         `json:"type"`
	Function openaiFunction `json:"function"`
}

type openaiFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type openaiToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function openaiFunctionCall `json:"function"`
}

type openaiFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openaiResponseFormat struct {
	Type       string          `json:"type"`
	JSONSchema json.RawMessage `json:"json_schema,omitempty"`
}

type openaiContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL *struct {
		URL    string `json:"url"`
		Detail string `json:"detail,omitempty"`
	} `json:"image_url,omitempty"`
}

// openaiToBlueship converts an OpenAI request into blueship's CompletionRequest.
// system messages concat into the System field. user/assistant turns produce
// ContentBlock arrays so text + images + tool_use / tool_result coexist.
// tool-role messages fold back into the prior assistant's tool_use as a user
// turn carrying a tool_result block keyed by tool_call_id. response_format
// is steered via an appended system instruction (Anthropic has no first-class
// json_object mode but the model honours an explicit prompt).
// effortConfig maps a client's reasoning-effort string onto the two fields
// Anthropic needs: output_config.effort, and a thinking mode to go with it.
//
// OpenAI's vocabulary starts at "minimal", which has no Anthropic equivalent —
// it folds into "low" rather than erroring, so a client tuned for OpenAI still
// lands on the cheap end of the range. Anything unrecognized yields "" and both
// fields are omitted: the API rejects a bad effort value outright, and a typo
// should not sink an otherwise valid request.
//
// Effort alone is not enough to mean the same thing on every model. With no
// thinking block at all, Opus 5 still reasons (it is on by default there) while
// Opus 4.7 and 4.8 do not — so the identical request quietly changes meaning
// with the model name. Asking for effort *is* a statement about reasoning
// depth, so it comes paired with adaptive thinking and the model picks the
// depth. Send no effort and the provider default is left alone.
func effortConfig(s string) (effort, thinkingMode string) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "minimal", "low":
		effort = "low"
	case "medium":
		effort = "medium"
	case "high":
		effort = "high"
	case "xhigh":
		effort = "xhigh"
	case "max":
		effort = "max"
	default:
		return "", ""
	}
	return effort, "adaptive"
}

func openaiToBlueship(req openaiChatRequest, defaultModel string) (bs.CompletionRequest, error) {
	var systemParts []string
	messages := make([]bs.Message, 0, len(req.Messages))

	for _, m := range req.Messages {
		switch m.Role {
		case "system", "developer":
			if text := flattenTextContent(m.Content); text != "" {
				systemParts = append(systemParts, text)
			}

		case "user":
			blocks := openaiContentToBlocks(m.Content)
			if len(blocks) == 0 {
				continue
			}
			messages = append(messages, bs.Message{Role: "user", Content: blocks})

		case "assistant":
			blocks := openaiContentToBlocks(m.Content)
			for _, tc := range m.ToolCalls {
				args := json.RawMessage(tc.Function.Arguments)
				if !json.Valid(args) || len(args) == 0 {
					args = json.RawMessage("{}")
				}
				blocks = append(blocks, bs.ContentBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Function.Name,
					Input: args,
				})
			}
			if len(blocks) == 0 {
				continue
			}
			messages = append(messages, bs.Message{Role: "assistant", Content: blocks})

		case "tool", "function":
			content := flattenTextContent(m.Content)
			if content == "" {
				content = " " // Anthropic rejects empty tool_result
			}
			toolUseID := m.ToolCallID
			if toolUseID == "" {
				toolUseID = m.Name // legacy function-role fallback
			}
			messages = append(messages, bs.Message{Role: "user", Content: []bs.ContentBlock{{
				Type:      "tool_result",
				ToolUseID: toolUseID,
				Content:   content,
			}}})

		default:
			// Unknown role — treat as user text.
			if text := flattenTextContent(m.Content); text != "" {
				messages = append(messages, bs.Message{Role: "user", Content: text})
			}
		}
	}

	messages = normalizeToolPairing(messages)

	if len(messages) == 0 {
		return bs.CompletionRequest{}, errors.New("no user/assistant messages provided")
	}

	if rf := req.ResponseFormat; rf != nil {
		switch rf.Type {
		case "json_object":
			systemParts = append(systemParts, jsonModeInstruction(""))
		case "json_schema":
			systemParts = append(systemParts, jsonModeInstruction(string(rf.JSONSchema)))
		}
	}

	tools := openaiToolsToBlueship(req.Tools)
	if shouldOmitTools(req.ToolChoice) {
		tools = nil
	}
	if name := forcedToolName(req.ToolChoice); name != "" {
		// Anthropic accepts force-tool only through its own tool_choice field which
		// blueship's CompletionRequest doesn't expose. Best-effort: nudge via system.
		systemParts = append(systemParts, "You MUST call the tool named `"+name+"`.")
	}

	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = defaultModel
	}

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 4096
	}

	effort, thinkingMode := effortConfig(req.ReasoningEffort)

	return bs.CompletionRequest{
		Model:        model,
		System:       strings.Join(systemParts, "\n\n"),
		Messages:     messages,
		Tools:        tools,
		MaxTokens:    maxTokens,
		Temperature:  req.Temperature,
		Effort:       effort,
		ThinkingMode: thinkingMode,
	}, nil
}

// normalizeToolPairing repairs the tool_use / tool_result contract the
// Anthropic API enforces but the OpenAI wire format cannot express:
//
//  1. OpenAI allows only one tool_call_id per tool-role message, so a
//     parallel tool_use turn arrives as SEVERAL consecutive tool-role
//     messages. Each became its own user turn here, and Anthropic then
//     rejects the request ("tool_use ids were found without tool_result
//     blocks immediately after") because it requires ALL results for a
//     turn in the single next user message. → merge consecutive user
//     turns, tool_result blocks first.
//  2. Клиентская обрезка истории (dialog budget) может оставить tool_use
//     без результата или tool_result без вызова. → inject a synthetic
//     "(tool result unavailable)" for missing ids; demote orphan
//     tool_results to plain text so the info survives without a 400.
func normalizeToolPairing(messages []bs.Message) []bs.Message {
	blocksOf := func(m bs.Message) []bs.ContentBlock {
		switch c := m.Content.(type) {
		case []bs.ContentBlock:
			return c
		case string:
			if c == "" {
				return nil
			}
			return []bs.ContentBlock{{Type: "text", Text: c}}
		default:
			return nil
		}
	}

	// Pass 1: merge consecutive same-role messages (user+user comes from
	// split tool results; assistant+assistant is harmless to merge too).
	merged := make([]bs.Message, 0, len(messages))
	for _, m := range messages {
		if n := len(merged); n > 0 && merged[n-1].Role == m.Role {
			merged[n-1].Content = append(blocksOf(merged[n-1]), blocksOf(m)...)
			continue
		}
		mm := m
		mm.Content = blocksOf(m)
		merged = append(merged, mm)
	}

	// Pass 2: within each user turn put tool_result blocks first (the API
	// requires results before any other content in the message).
	for i := range merged {
		if merged[i].Role != "user" {
			continue
		}
		blocks := merged[i].Content.([]bs.ContentBlock)
		results := make([]bs.ContentBlock, 0, len(blocks))
		rest := make([]bs.ContentBlock, 0, len(blocks))
		for _, b := range blocks {
			if b.Type == "tool_result" {
				results = append(results, b)
			} else {
				rest = append(rest, b)
			}
		}
		merged[i].Content = append(results, rest...)
	}

	// Pass 3: pair every assistant tool_use with a result in the next
	// message; synthesize missing results, demote orphan results to text.
	// Manual bound: the loop inserts synthetic turns into merged.
	for i := 0; i < len(merged); i++ {
		blocks := merged[i].Content.([]bs.ContentBlock)
		if merged[i].Role == "assistant" {
			pending := map[string]bool{}
			for _, b := range blocks {
				if b.Type == "tool_use" && b.ID != "" {
					pending[b.ID] = true
				}
			}
			if len(pending) == 0 {
				continue
			}
			if i+1 < len(merged) && merged[i+1].Role == "user" {
				next := merged[i+1].Content.([]bs.ContentBlock)
				for _, b := range next {
					if b.Type == "tool_result" {
						delete(pending, b.ToolUseID)
					}
				}
				if len(pending) > 0 {
					synth := make([]bs.ContentBlock, 0, len(pending))
					for id := range pending {
						synth = append(synth, bs.ContentBlock{
							Type: "tool_result", ToolUseID: id,
							Content: "(tool result unavailable)",
						})
					}
					merged[i+1].Content = append(synth, next...)
				}
			} else {
				// tool_use is the last message or followed by assistant:
				// insert a synthetic result turn to satisfy the contract.
				synth := make([]bs.ContentBlock, 0, len(pending))
				for id := range pending {
					synth = append(synth, bs.ContentBlock{
						Type: "tool_result", ToolUseID: id,
						Content: "(tool result unavailable)",
					})
				}
				merged = append(merged[:i+1], append([]bs.Message{{Role: "user", Content: synth}}, merged[i+1:]...)...)
			}
		}
	}

	// Pass 4: demote tool_results whose id has no matching tool_use in the
	// immediately preceding assistant turn (budget-clipped history).
	for i := range merged {
		if merged[i].Role != "user" {
			continue
		}
		valid := map[string]bool{}
		if i > 0 && merged[i-1].Role == "assistant" {
			for _, b := range merged[i-1].Content.([]bs.ContentBlock) {
				if b.Type == "tool_use" && b.ID != "" {
					valid[b.ID] = true
				}
			}
		}
		blocks := merged[i].Content.([]bs.ContentBlock)
		out := make([]bs.ContentBlock, 0, len(blocks))
		for _, b := range blocks {
			if b.Type == "tool_result" && !valid[b.ToolUseID] {
				text, _ := b.Content.(string)
				out = append(out, bs.ContentBlock{Type: "text", Text: "[tool result] " + text})
				continue
			}
			out = append(out, b)
		}
		merged[i].Content = out
	}

	return merged
}

func openaiToolsToBlueship(tools []openaiTool) []bs.ToolDefinition {
	out := make([]bs.ToolDefinition, 0, len(tools))
	for _, t := range tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		schema := t.Function.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, bs.ToolDefinition{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: schema,
		})
	}
	return out
}

// shouldOmitTools returns true when tool_choice is the literal string "none".
func shouldOmitTools(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s == "none"
	}
	return false
}

// forcedToolName extracts the function.name from a structured tool_choice
// like {"type":"function","function":{"name":"x"}}. Empty when not forced.
func forcedToolName(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.Type == "function" {
		return obj.Function.Name
	}
	return ""
}

// openaiContentToBlocks parses an OpenAI content field into ContentBlocks.
// content can be a JSON string (the simple case), a JSON array of parts
// ({"type":"text",…} or {"type":"image_url",…}), or null.
func openaiContentToBlocks(raw json.RawMessage) []bs.ContentBlock {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}

	// Fast path: bare JSON string.
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			if s == "" {
				return nil
			}
			return []bs.ContentBlock{{Type: "text", Text: s}}
		}
	}

	// Array of parts.
	var parts []openaiContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		// Last resort: maybe it's some other JSON value we can stringify.
		return []bs.ContentBlock{{Type: "text", Text: string(raw)}}
	}

	blocks := make([]bs.ContentBlock, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "text":
			if p.Text != "" {
				blocks = append(blocks, bs.ContentBlock{Type: "text", Text: p.Text})
			}
		case "image_url", "image":
			if p.ImageURL == nil || p.ImageURL.URL == "" {
				continue
			}
			if src := parseDataURL(p.ImageURL.URL); src != nil {
				blocks = append(blocks, bs.ContentBlock{Type: "image", Source: src})
			}
			// http(s):// URLs silently dropped — blueship's ImageSource is
			// base64-only. Senders should inline data:image/...;base64.
		}
	}
	return blocks
}

// flattenTextContent collapses an OpenAI content field into a single text
// string. Image parts are dropped. Used for system / tool messages where
// blocks aren't meaningful.
func flattenTextContent(raw json.RawMessage) string {
	blocks := openaiContentToBlocks(raw)
	if len(blocks) == 0 {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// parseDataURL parses data:[<media>];base64,<data> into an ImageSource.
// Anthropic accepts base64 with a media_type; other data URL forms return nil.
func parseDataURL(url string) *bs.ImageSource {
	const dataPrefix = "data:"
	if !strings.HasPrefix(url, dataPrefix) {
		return nil
	}
	rest := url[len(dataPrefix):]
	comma := strings.Index(rest, ",")
	if comma == -1 {
		return nil
	}
	meta, data := rest[:comma], rest[comma+1:]
	if !strings.Contains(meta, "base64") {
		return nil
	}
	media := strings.SplitN(meta, ";", 2)[0]
	if media == "" {
		media = "image/jpeg"
	}
	return &bs.ImageSource{Type: "base64", MediaType: media, Data: data}
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
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	ToolCalls []openaiToolCall `json:"tool_calls,omitempty"`
}

type openaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func blueshipToOpenAI(resp *bs.CompletionResponse, model string, jsonMode bool) openaiChatResponse {
	var textParts []string
	var toolCalls []openaiToolCall
	for _, b := range resp.Content {
		switch b.Type {
		case "text":
			if b.Text != "" {
				textParts = append(textParts, b.Text)
			}
		case "tool_use":
			args := string(b.Input)
			if args == "" || !json.Valid(b.Input) {
				args = "{}"
			}
			toolCalls = append(toolCalls, openaiToolCall{
				ID:   b.ID,
				Type: "function",
				Function: openaiFunctionCall{
					Name:      b.Name,
					Arguments: args,
				},
			})
		}
	}

	finish := mapStopReason(resp.StopReason)
	if len(toolCalls) > 0 && (finish == "stop" || finish == "") {
		finish = "tool_calls"
	}

	content := strings.Join(textParts, "")
	if jsonMode {
		content = stripJSONFences(content)
	}

	return openaiChatResponse{
		ID:      "chatcmpl-" + randomID(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []openaiChoice{{
			Index: 0,
			Message: openaiOutputMessage{
				Role:      "assistant",
				Content:   content,
				ToolCalls: toolCalls,
			},
			FinishReason: finish,
		}},
		Usage: openaiUsage{
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
			TotalTokens:      resp.Usage.InputTokens + resp.Usage.OutputTokens,
		},
	}
}

// jsonModeInstruction builds the system suffix used to mimic OpenAI's
// response_format. Anthropic has no first-class JSON mode and Claude likes
// to wrap structured output in ```json fences even when told otherwise, so
// the instruction is explicit about the prohibition and we also strip
// fences from the response (see stripJSONFences) as a belt-and-suspenders
// guarantee for downstream consumers.
func jsonModeInstruction(schema string) string {
	b := strings.Builder{}
	b.WriteString("You MUST respond with a single raw JSON value. ")
	b.WriteString("Do NOT wrap it in markdown code fences (no ``` or ```json). ")
	b.WriteString("Do NOT add any prose, commentary, or explanation before or after the JSON. ")
	b.WriteString("Your entire response must be valid JSON starting with `{` or `[`.")
	if strings.TrimSpace(schema) != "" {
		b.WriteString("\n\nThe JSON must conform to this schema:\n")
		b.WriteString(schema)
	}
	return b.String()
}

// stripJSONFences removes a leading ```json (or ```) and a trailing ``` from
// model output. No-op if no fence is present. Applied only when the request
// asked for response_format JSON.
func stripJSONFences(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "```") {
		return s
	}
	t = strings.TrimPrefix(t, "```")
	t = strings.TrimPrefix(t, "json")
	t = strings.TrimPrefix(t, "JSON")
	t = strings.TrimLeft(t, "\r\n")
	t = strings.TrimSuffix(t, "```")
	return strings.TrimSpace(t)
}

// mapStopReason translates Anthropic stop_reason values into OpenAI
// finish_reason. tool_use → tool_calls; max_tokens → length; everything
// else collapses to stop.
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
