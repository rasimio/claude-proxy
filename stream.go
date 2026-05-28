package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	bs "github.com/rasimio/blueship/core"
)

// streamCompletion runs a streaming Anthropic call and emits OpenAI-format
// SSE chunks: first an initial delta carrying {"role":"assistant"}, then one
// chunk per text delta, then a terminal chunk with finish_reason set, then
// the literal `data: [DONE]` line OpenAI clients expect.
func (s *server) streamCompletion(w http.ResponseWriter, r *http.Request, req bs.CompletionRequest) {
	if s.stream == nil {
		writeOpenAIError(w, http.StatusInternalServerError, "stream_unavailable", "provider does not support streaming")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // tell nginx/caddy not to buffer
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	id := "chatcmpl-" + randomID()
	created := time.Now().Unix()

	writeSSEChunk(w, flusher, id, created, req.Model, map[string]any{"role": "assistant"}, nil)

	cb := &bs.StreamCallbacks{
		OnText: func(delta string) {
			if delta == "" {
				return
			}
			writeSSEChunk(w, flusher, id, created, req.Model, map[string]any{"content": delta}, nil)
		},
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()

	resp, err := s.stream.StreamComplete(ctx, req, cb)
	if err != nil {
		s.logger.Error("upstream stream failed", "error", err)
		// Best-effort: emit a final chunk carrying an error finish_reason. We
		// can't switch back to JSON error mid-stream — the client already saw
		// 200 + text/event-stream headers.
		writeSSEChunk(w, flusher, id, created, req.Model, map[string]any{}, ptr("stop"))
		writeSSEDone(w, flusher)
		return
	}

	writeSSEChunk(w, flusher, id, created, req.Model, map[string]any{}, ptr(mapStopReason(resp.StopReason)))
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
