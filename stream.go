package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	bs "github.com/rasimio/blueship/core"
)

// streamCompletion runs a streaming Anthropic call and emits OpenAI-format
// SSE chunks: an initial delta with {"role":"assistant"}, then one chunk per
// text delta, then one chunk per fully-assembled tool_use (with index, id,
// name and full arguments), then a terminal chunk with finish_reason set,
// then the literal `data: [DONE]` line OpenAI clients expect.
//
// Anthropic streams partial JSON for tool_use arguments but blueship only
// surfaces tool_use once the block is complete (content_block_stop). We
// forward that as a single OpenAI tool_calls delta carrying the full
// arguments string — n8n and the OpenAI SDK accept this shape.
func (s *server) streamCompletion(w http.ResponseWriter, r *http.Request, req bs.CompletionRequest) {
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
	id := "chatcmpl-" + randomID()
	created := time.Now().Unix()

	writeSSEChunk(w, flusher, id, created, req.Model, map[string]any{"role": "assistant"}, nil)

	var toolCallIdx int64 = -1
	var sawToolUse atomic.Bool

	cb := &bs.StreamCallbacks{
		OnText: func(delta string) {
			if delta == "" {
				return
			}
			writeSSEChunk(w, flusher, id, created, req.Model, map[string]any{"content": delta}, nil)
		},
		OnToolUse: func(useID, name string, input json.RawMessage) {
			sawToolUse.Store(true)
			args := string(input)
			if args == "" || !json.Valid(input) {
				args = "{}"
			}
			idx := atomic.AddInt64(&toolCallIdx, 1)
			delta := map[string]any{
				"tool_calls": []map[string]any{{
					"index": int(idx),
					"id":    useID,
					"type":  "function",
					"function": map[string]any{
						"name":      name,
						"arguments": args,
					},
				}},
			}
			writeSSEChunk(w, flusher, id, created, req.Model, delta, nil)
		},
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()

	resp, err := s.stream.StreamComplete(ctx, req, cb)
	if err != nil {
		s.logger.Error("upstream stream failed", "error", err)
		writeSSEChunk(w, flusher, id, created, req.Model, map[string]any{}, ptr("stop"))
		writeSSEDone(w, flusher)
		return
	}

	finish := mapStopReason(resp.StopReason)
	if sawToolUse.Load() && (finish == "stop" || finish == "") {
		finish = "tool_calls"
	}

	writeSSEChunk(w, flusher, id, created, req.Model, map[string]any{}, ptr(finish))
	writeSSEDone(w, flusher)
}

func writeSSEChunk(w http.ResponseWriter, flusher http.Flusher, id string, created int64, model string, delta map[string]any, finishReason *string) {
	chunk := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"delta":         delta,
			"finish_reason": finishReason,
		}},
	}
	raw, err := json.Marshal(chunk)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", raw)
	if flusher != nil {
		flusher.Flush()
	}
}

func writeSSEDone(w http.ResponseWriter, flusher http.Flusher) {
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func ptr(s string) *string { return &s }
