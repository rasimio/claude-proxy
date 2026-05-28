package main

// OpenAI Responses API (POST /v1/responses) compatibility layer.
//
// The Responses API is OpenAI's newer endpoint that supersedes
// /v1/chat/completions. LangChain JS ≥ 1.0 uses it by default, so n8n's
// "OpenAI Chat Model" node hits this path. The wire format is different
// from chat/completions: a single `input` field (string or array of items)
// in, an `output` array of items out, and a SSE stream of typed events
// (response.created → response.output_text.delta → response.completed).
//
// This implementation translates a minimal-but-useful subset onto blueship's
// CompletionRequest and re-emits Responses-API-shaped events from blueship's
// stream callbacks. Tools and vision are supported; built-in tools
// (web_search, file_search, computer_use) are not.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	bs "github.com/rasimio/blueship/core"
)

// --- Request shape ---

type responsesRequest struct {
	Model           string                `json:"model"`
	Input           json.RawMessage       `json:"input"`        // string OR []responsesInputItem
	Instructions    string                `json:"instructions"` // top-level system prompt
	MaxOutputTokens int                   `json:"max_output_tokens,omitempty"`
	Temperature     float64               `json:"temperature,omitempty"`
	Stream          bool                  `json:"stream,omitempty"`
	Tools           []responsesTool       `json:"tools,omitempty"`
	ToolChoice      json.RawMessage       `json:"tool_choice,omitempty"`
	ResponseFormat  *openaiResponseFormat `json:"response_format,omitempty"`
	// "text.format" is the Responses-API location for response_format; accept
	// both spellings for forward-compat with langchain.
	Text *struct {
		Format *openaiResponseFormat `json:"format,omitempty"`
	} `json:"text,omitempty"`
}

// responsesInputItem covers the item shapes we translate:
//   - message: {role, content}
//   - function_call: {type:"function_call", call_id, name, arguments}
//   - function_call_output: {type:"function_call_output", call_id, output}
type responsesInputItem struct {
	Type      string          `json:"type,omitempty"` // "message"|"function_call"|"function_call_output"
	Role      string          `json:"role,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments string          `json:"arguments,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"`
}

type responsesContentPart struct {
	Type     string `json:"type"` // input_text|input_image|output_text
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"` // Responses API uses flat string
}

type responsesTool struct {
	// Responses API has a flat shape: {"type":"function","name":...,"parameters":...}
	// (no nested "function" object like chat/completions).
	Type        string          `json:"type"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	// Allow nested form too, just in case.
	Function *openaiFunction `json:"function,omitempty"`
}

// --- Response shape ---

type responsesResponse struct {
	ID        string           `json:"id"`
	Object    string           `json:"object"`
	CreatedAt int64            `json:"created_at"`
	Status    string           `json:"status"`
	Model     string           `json:"model"`
	Output    []responsesItem  `json:"output"`
	Usage     responsesUsage   `json:"usage"`
}

type responsesItem struct {
	ID        string                  `json:"id"`
	Type      string                  `json:"type"` // "message" | "function_call"
	Status    string                  `json:"status,omitempty"`
	Role      string                  `json:"role,omitempty"`
	Content   []responsesOutputPart   `json:"content,omitempty"`
	CallID    string                  `json:"call_id,omitempty"`
	Name      string                  `json:"name,omitempty"`
	Arguments string                  `json:"arguments,omitempty"`
}

type responsesOutputPart struct {
	Type        string                 `json:"type"` // "output_text"
	Text        string                 `json:"text"`
	Annotations []any                  `json:"annotations,omitempty"`
}

type responsesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// --- Handler ---

func (s *server) responses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 8*1024*1024))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "bad_request", "failed to read body")
		return
	}

	var req responsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON: "+err.Error())
		return
	}

	bsReq, jsonMode, err := responsesToBlueship(req, s.defaultModel)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	s.logger.Info("responses",
		"model", bsReq.Model,
		"messages", len(bsReq.Messages),
		"system_len", len(bsReq.System),
		"max_tokens", bsReq.MaxTokens,
		"tools", len(bsReq.Tools),
		"stream", req.Stream,
		"json_mode", jsonMode,
	)

	if req.Stream {
		s.streamResponses(w, r, bsReq, jsonMode)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()

	resp, err := s.provider.Complete(ctx, bsReq)
	if err != nil {
		s.logger.Error("upstream complete failed", "error", err)
		writeUpstreamError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, blueshipToResponses(resp, bsReq.Model, jsonMode))
}

// responsesToBlueship parses the Responses API request into a CompletionRequest.
// Returns (req, jsonMode, err) — jsonMode controls fence stripping on the way out.
func responsesToBlueship(req responsesRequest, defaultModel string) (bs.CompletionRequest, bool, error) {
	var systemParts []string
	if instr := strings.TrimSpace(req.Instructions); instr != "" {
		systemParts = append(systemParts, instr)
	}

	items, err := parseResponsesInput(req.Input)
	if err != nil {
		return bs.CompletionRequest{}, false, err
	}

	messages := make([]bs.Message, 0, len(items))
	for _, it := range items {
		switch {
		case it.Type == "function_call":
			args := json.RawMessage(it.Arguments)
			if !json.Valid(args) || len(args) == 0 {
				args = json.RawMessage("{}")
			}
			messages = append(messages, bs.Message{Role: "assistant", Content: []bs.ContentBlock{{
				Type:  "tool_use",
				ID:    it.CallID,
				Name:  it.Name,
				Input: args,
			}}})

		case it.Type == "function_call_output":
			content := flattenResponsesOutput(it.Output)
			if content == "" {
				content = " "
			}
			messages = append(messages, bs.Message{Role: "user", Content: []bs.ContentBlock{{
				Type:      "tool_result",
				ToolUseID: it.CallID,
				Content:   content,
			}}})

		case it.Type == "message" || it.Type == "":
			blocks := parseResponsesContent(it.Content, it.Role)
			if len(blocks) == 0 {
				continue
			}
			role := it.Role
			if role == "" {
				role = "user"
			}
			if role == "system" || role == "developer" {
				if t := flattenBlocksText(blocks); t != "" {
					systemParts = append(systemParts, t)
				}
				continue
			}
			messages = append(messages, bs.Message{Role: role, Content: blocks})
		}
	}

	if len(messages) == 0 {
		return bs.CompletionRequest{}, false, errors.New("no user/assistant messages in input")
	}

	// response_format: accept either top-level or text.format.
	rf := req.ResponseFormat
	if rf == nil && req.Text != nil {
		rf = req.Text.Format
	}
	jsonMode := false
	if rf != nil {
		switch rf.Type {
		case "json_object":
			systemParts = append(systemParts, jsonModeInstruction(""))
			jsonMode = true
		case "json_schema":
			systemParts = append(systemParts, jsonModeInstruction(string(rf.JSONSchema)))
			jsonMode = true
		}
	}

	tools := responsesToolsToBlueship(req.Tools)
	if shouldOmitTools(req.ToolChoice) {
		tools = nil
	}
	if name := forcedToolName(req.ToolChoice); name != "" {
		systemParts = append(systemParts, "You MUST call the tool named `"+name+"`.")
	}

	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = defaultModel
	}

	maxTokens := req.MaxOutputTokens
	if maxTokens <= 0 {
		maxTokens = 4096
	}

	return bs.CompletionRequest{
		Model:       model,
		System:      strings.Join(systemParts, "\n\n"),
		Messages:    messages,
		Tools:       tools,
		MaxTokens:   maxTokens,
		Temperature: req.Temperature,
	}, jsonMode, nil
}

// parseResponsesInput accepts either a bare string or an array of items.
// A bare string is treated as a single user message.
func parseResponsesInput(raw json.RawMessage) ([]responsesInputItem, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, errors.New("input is required")
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			c, _ := json.Marshal(s)
			return []responsesInputItem{{Type: "message", Role: "user", Content: c}}, nil
		}
	}
	var items []responsesInputItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("input: %w", err)
	}
	return items, nil
}

// parseResponsesContent reads a Responses-API content field. content may be:
//   - a bare string
//   - an array of {"type":"input_text"|"output_text"|"input_image", ...}
//
// role hints which output types to expect (assistant uses output_text).
func parseResponsesContent(raw json.RawMessage, _ string) []bs.ContentBlock {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil && s != "" {
			return []bs.ContentBlock{{Type: "text", Text: s}}
		}
		return nil
	}

	var parts []responsesContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return []bs.ContentBlock{{Type: "text", Text: trimmed}}
	}

	blocks := make([]bs.ContentBlock, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "input_text", "output_text", "text":
			if p.Text != "" {
				blocks = append(blocks, bs.ContentBlock{Type: "text", Text: p.Text})
			}
		case "input_image":
			if p.ImageURL == "" {
				continue
			}
			if src := parseDataURL(p.ImageURL); src != nil {
				blocks = append(blocks, bs.ContentBlock{Type: "image", Source: src})
			}
		}
	}
	return blocks
}

// flattenBlocksText collapses ContentBlocks back into plain text for system role.
func flattenBlocksText(blocks []bs.ContentBlock) string {
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// flattenResponsesOutput parses a function_call_output's `output` field
// which can be either a string or arbitrary JSON.
func flattenResponsesOutput(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
	}
	return trimmed
}

func responsesToolsToBlueship(tools []responsesTool) []bs.ToolDefinition {
	out := make([]bs.ToolDefinition, 0, len(tools))
	for _, t := range tools {
		if t.Type != "" && t.Type != "function" {
			continue // skip built-in tools (web_search, file_search, …)
		}
		// Flat form takes precedence; fall back to nested {function:{…}}.
		name := t.Name
		desc := t.Description
		schema := t.Parameters
		if name == "" && t.Function != nil {
			name = t.Function.Name
			desc = t.Function.Description
			schema = t.Function.Parameters
		}
		if name == "" {
			continue
		}
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, bs.ToolDefinition{
			Name:        name,
			Description: desc,
			InputSchema: schema,
		})
	}
	return out
}

// blueshipToResponses produces a single Responses-API response object.
func blueshipToResponses(resp *bs.CompletionResponse, model string, jsonMode bool) responsesResponse {
	out := responsesResponse{
		ID:        "resp_" + randomID(),
		Object:    "response",
		CreatedAt: time.Now().Unix(),
		Status:    "completed",
		Model:     model,
		Usage: responsesUsage{
			InputTokens:  resp.Usage.InputTokens,
			OutputTokens: resp.Usage.OutputTokens,
			TotalTokens:  resp.Usage.InputTokens + resp.Usage.OutputTokens,
		},
	}

	var textParts []string
	var items []responsesItem

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
			items = append(items, responsesItem{
				ID:        "fc_" + randomID(),
				Type:      "function_call",
				Status:    "completed",
				CallID:    b.ID,
				Name:      b.Name,
				Arguments: args,
			})
		}
	}

	if len(textParts) > 0 || (len(items) == 0) {
		text := strings.Join(textParts, "")
		if jsonMode {
			text = stripJSONFences(text)
		}
		// Prepend the message item so a consumer that only reads output[0]
		// sees the assistant text.
		msgItem := responsesItem{
			ID:     "msg_" + randomID(),
			Type:   "message",
			Status: "completed",
			Role:   "assistant",
			Content: []responsesOutputPart{{
				Type: "output_text",
				Text: text,
			}},
		}
		items = append([]responsesItem{msgItem}, items...)
	}

	out.Output = items
	return out
}

// --- Streaming ---

func (s *server) streamResponses(w http.ResponseWriter, r *http.Request, req bs.CompletionRequest, jsonMode bool) {
	if s.stream == nil {
		writeOpenAIError(w, http.StatusInternalServerError, "stream_unavailable", "provider does not support streaming")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	respID := "resp_" + randomID()
	msgID := "msg_" + randomID()
	created := time.Now().Unix()

	emit := func(event string, payload any) {
		raw, err := json.Marshal(payload)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, raw)
		if flusher != nil {
			flusher.Flush()
		}
	}

	initialResponse := responsesResponse{
		ID:        respID,
		Object:    "response",
		CreatedAt: created,
		Status:    "in_progress",
		Model:     req.Model,
		Output:    []responsesItem{},
	}
	emit("response.created", map[string]any{"type": "response.created", "response": initialResponse})
	emit("response.in_progress", map[string]any{"type": "response.in_progress", "response": initialResponse})

	textOutputIndex := 0
	emit("response.output_item.added", map[string]any{
		"type":         "response.output_item.added",
		"output_index": textOutputIndex,
		"item": responsesItem{
			ID:      msgID,
			Type:    "message",
			Status:  "in_progress",
			Role:    "assistant",
			Content: []responsesOutputPart{},
		},
	})
	emit("response.content_part.added", map[string]any{
		"type":          "response.content_part.added",
		"item_id":       msgID,
		"output_index":  textOutputIndex,
		"content_index": 0,
		"part":          responsesOutputPart{Type: "output_text", Text: ""},
	})

	var textBuf strings.Builder
	var nextOutputIndex int64 = int64(textOutputIndex) + 1
	var toolCallsEmitted atomic.Bool

	cb := &bs.StreamCallbacks{
		OnText: func(delta string) {
			if delta == "" {
				return
			}
			textBuf.WriteString(delta)
			emit("response.output_text.delta", map[string]any{
				"type":          "response.output_text.delta",
				"item_id":       msgID,
				"output_index":  textOutputIndex,
				"content_index": 0,
				"delta":         delta,
			})
		},
		OnToolUse: func(useID, name string, input json.RawMessage) {
			args := string(input)
			if args == "" || !json.Valid(input) {
				args = "{}"
			}
			fcIdx := int(atomic.AddInt64(&nextOutputIndex, 1) - 1)
			fcID := "fc_" + randomID()
			fcItem := responsesItem{
				ID:        fcID,
				Type:      "function_call",
				Status:    "completed",
				CallID:    useID,
				Name:      name,
				Arguments: args,
			}
			emit("response.output_item.added", map[string]any{
				"type":         "response.output_item.added",
				"output_index": fcIdx,
				"item":         fcItem,
			})
			emit("response.function_call_arguments.delta", map[string]any{
				"type":         "response.function_call_arguments.delta",
				"item_id":      fcID,
				"output_index": fcIdx,
				"delta":        args,
			})
			emit("response.function_call_arguments.done", map[string]any{
				"type":         "response.function_call_arguments.done",
				"item_id":      fcID,
				"output_index": fcIdx,
				"arguments":    args,
			})
			emit("response.output_item.done", map[string]any{
				"type":         "response.output_item.done",
				"output_index": fcIdx,
				"item":         fcItem,
			})
			toolCallsEmitted.Store(true)
		},
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()

	resp, err := s.stream.StreamComplete(ctx, req, cb)
	if err != nil {
		s.logger.Error("upstream stream failed", "error", err)
		emit("response.failed", map[string]any{
			"type":     "response.failed",
			"response": map[string]any{"id": respID, "status": "failed", "error": map[string]any{"message": err.Error()}},
		})
		return
	}

	finalText := textBuf.String()
	if jsonMode {
		finalText = stripJSONFences(finalText)
	}

	emit("response.output_text.done", map[string]any{
		"type":          "response.output_text.done",
		"item_id":       msgID,
		"output_index":  textOutputIndex,
		"content_index": 0,
		"text":          finalText,
	})
	emit("response.content_part.done", map[string]any{
		"type":          "response.content_part.done",
		"item_id":       msgID,
		"output_index":  textOutputIndex,
		"content_index": 0,
		"part":          responsesOutputPart{Type: "output_text", Text: finalText},
	})
	emit("response.output_item.done", map[string]any{
		"type":         "response.output_item.done",
		"output_index": textOutputIndex,
		"item": responsesItem{
			ID:      msgID,
			Type:    "message",
			Status:  "completed",
			Role:    "assistant",
			Content: []responsesOutputPart{{Type: "output_text", Text: finalText}},
		},
	})

	final := blueshipToResponses(resp, req.Model, jsonMode)
	final.ID = respID
	final.CreatedAt = created
	emit("response.completed", map[string]any{"type": "response.completed", "response": final})

	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}
