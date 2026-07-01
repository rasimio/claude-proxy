package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/rasimio/blueship"
	bs "github.com/rasimio/blueship/core"
)

type serveConfig struct {
	Bind           string
	Port           string
	APIKey         string
	TokenFile      string
	DefaultModel   string
	RequestTimeout time.Duration
}

func loadServeConfig() serveConfig {
	cfg := serveConfig{
		Bind:         envOr("BIND", "0.0.0.0"),
		Port:         envOr("PORT", "8080"),
		APIKey:       strings.TrimSpace(os.Getenv("PROXY_API_KEY")),
		TokenFile:    envOr("TOKEN_FILE", "./data/anthropic-tokens.json"),
		DefaultModel: envOr("DEFAULT_MODEL", "claude-opus-4-7"),
	}
	if d, err := time.ParseDuration(envOr("REQUEST_TIMEOUT", "300s")); err == nil {
		cfg.RequestTimeout = d
	} else {
		cfg.RequestTimeout = 300 * time.Second
	}
	return cfg
}

type server struct {
	provider     bs.CompletionProvider
	stream       bs.StreamCompletionProvider // may be nil
	apiKey       string
	defaultModel string
	logger       *slog.Logger
}

func runServe() {
	cfg := loadServeConfig()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if cfg.APIKey == "" {
		fmt.Fprintln(os.Stderr, "PROXY_API_KEY env var is required")
		os.Exit(2)
	}
	if _, err := os.Stat(cfg.TokenFile); err != nil {
		fmt.Fprintf(os.Stderr, "token file %s not found — run `claude-proxy login` first\n", cfg.TokenFile)
		os.Exit(2)
	}

	backoffs := []time.Duration{1 * time.Second, 2 * time.Second, 5 * time.Second}
	provider := blueship.AnthropicOAuth("", cfg.TokenFile, cfg.RequestTimeout, backoffs, logger)
	streamProvider, _ := provider.(bs.StreamCompletionProvider)

	srv := &server{
		provider:     provider,
		stream:       streamProvider,
		apiKey:       cfg.APIKey,
		defaultModel: cfg.DefaultModel,
		logger:       logger,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", srv.healthz)

	// Register both /v1/<x> and /<x> — different OpenAI clients (n8n's
	// LangChain ChatOpenAI in particular) construct URLs differently
	// depending on whether the base URL the user typed already ends in /v1.
	mux.HandleFunc("/v1/models", srv.requireAuth(srv.models))
	mux.HandleFunc("/models", srv.requireAuth(srv.models))
	mux.HandleFunc("/v1/chat/completions", srv.requireAuth(srv.chatCompletions))
	mux.HandleFunc("/chat/completions", srv.requireAuth(srv.chatCompletions))
	mux.HandleFunc("/v1/responses", srv.requireAuth(srv.responses))
	mux.HandleFunc("/responses", srv.requireAuth(srv.responses))

	// Catch-all so unknown paths surface in logs instead of bare 404.
	mux.HandleFunc("/", srv.notFound)

	addr := cfg.Bind + ":" + cfg.Port
	logger.Info("claude-proxy listening",
		"addr", addr,
		"default_model", cfg.DefaultModel,
		"token_file", cfg.TokenFile,
	)

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           accessLog(logger, mux),
		ReadHeaderTimeout: 15 * time.Second,
	}
	if err := httpSrv.ListenAndServe(); err != nil {
		logger.Error("listen", "error", err)
		os.Exit(1)
	}
}

func (s *server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *server) notFound(w http.ResponseWriter, r *http.Request) {
	writeOpenAIError(w, http.StatusNotFound, "not_found",
		"unknown path "+r.Method+" "+r.URL.Path+"; supported: /v1/chat/completions, /v1/responses, /v1/models, /healthz")
}

// accessLog wraps the handler with a per-request log line so misrouted
// clients (404s) leave a trail showing the exact path they hit.
func accessLog(logger *slog.Logger, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusRecorder{ResponseWriter: w, status: 200}
		h.ServeHTTP(sw, r)
		logger.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"dur_ms", time.Since(start).Milliseconds(),
			"ua", r.Header.Get("User-Agent"),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *server) requireAuth(h http.HandlerFunc) http.HandlerFunc {
	expected := []byte(s.apiKey)
	return func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), expected) != 1 {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "Incorrect API key provided")
			return
		}
		h(w, r)
	}
}

func (s *server) models(w http.ResponseWriter, _ *http.Request) {
	// A small, fixed set. n8n only needs *something* to list; the actual model
	// name in /v1/chat/completions is what matters at request time.
	models := []map[string]any{
		{"id": "claude-opus-4-7", "object": "model", "created": 0, "owned_by": "anthropic"},
		{"id": "claude-sonnet-5", "object": "model", "created": 0, "owned_by": "anthropic"},
		{"id": "claude-sonnet-4-6", "object": "model", "created": 0, "owned_by": "anthropic"},
		{"id": "claude-haiku-4-5", "object": "model", "created": 0, "owned_by": "anthropic"},
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   models,
	})
}

func (s *server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 8*1024*1024))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "bad_request", "failed to read body")
		return
	}

	var req openaiChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON: "+err.Error())
		return
	}

	bsReq, err := openaiToBlueship(req, s.defaultModel)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	s.logger.Info("chat completion",
		"model", bsReq.Model,
		"messages", len(bsReq.Messages),
		"system_len", len(bsReq.System),
		"max_tokens", bsReq.MaxTokens,
		"stream", req.Stream,
	)

	if req.Stream {
		s.streamCompletion(w, r, bsReq)
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

	jsonMode := req.ResponseFormat != nil && (req.ResponseFormat.Type == "json_object" || req.ResponseFormat.Type == "json_schema")
	writeJSON(w, http.StatusOK, blueshipToOpenAI(resp, bsReq.Model, jsonMode))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    code,
			"code":    code,
		},
	})
}

func writeUpstreamError(w http.ResponseWriter, err error) {
	// Treat context cancellation distinctly.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		writeOpenAIError(w, http.StatusGatewayTimeout, "timeout", err.Error())
		return
	}
	writeOpenAIError(w, http.StatusBadGateway, "upstream_error", err.Error())
}

func randomID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
