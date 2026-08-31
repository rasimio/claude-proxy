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

	bs "github.com/rasimio/blueship"
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
		DefaultModel: envOr("DEFAULT_MODEL", "claude-opus-5"),
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
	tokens       *bs.AnthropicTokenStore
	apiKey       string
	defaultModel string
	logger       *slog.Logger
}

// Token refresh cadence. The OAuth access token lives ~8 h. Renewing 15 min
// ahead of expiry on a 5-min tick keeps rotation off the request path
// entirely: a refresh that fails transiently gets two more ticks before the
// token the request path would settle for (60 s of slack) even goes stale.
const (
	tokenRefreshInterval = 5 * time.Minute
	tokenRefreshLead     = 15 * time.Minute
)

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
	provider, tokens := bs.AnthropicOAuthWithTokens("", cfg.TokenFile, cfg.RequestTimeout, backoffs, logger)
	streamProvider, _ := provider.(bs.StreamCompletionProvider)

	srv := &server{
		provider:     provider,
		stream:       streamProvider,
		tokens:       tokens,
		apiKey:       cfg.APIKey,
		defaultModel: cfg.DefaultModel,
		logger:       logger,
	}
	go srv.refreshTokens(context.Background())

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", srv.healthz)
	mux.HandleFunc("/readyz", srv.readyz)

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

// refreshTokens renews the OAuth pair ahead of expiry, forever.
//
// Without it the token only ever rotates on the first request that finds it
// stale — so that request pays the refresh latency, and if the OAuth endpoint
// is having a bad minute it eats the failure too, surfacing in the access log
// as a 502 that has nothing to do with what the client asked for. On a tick
// the same failure is just retried 5 minutes later, with hours of slack left.
func (s *server) refreshTokens(ctx context.Context) {
	tick := time.NewTicker(tokenRefreshInterval)
	defer tick.Stop()

	for {
		// Run once up front: a token file left stale by a long shutdown should
		// be caught at boot, not by the first client through the door.
		if err := s.tokens.EnsureFresh(tokenRefreshLead); err != nil {
			st := s.tokens.Status()
			if st.Rejected {
				// Terminal — the refresh chain is broken and no amount of
				// ticking re-mints it. Say the fix out loud; /readyz goes red.
				s.logger.Error("oauth refresh token rejected — run `claude-proxy login` to re-authorize",
					"error", err)
			} else {
				s.logger.Error("oauth token refresh failed, will retry", "error", err)
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// readyz reports whether the proxy can actually reach Anthropic — that is,
// whether the OAuth pair is alive. /healthz stays a plain liveness probe (the
// process is up and serving); this is the one that goes red when the refresh
// chain breaks, the failure that used to stay invisible until a client
// complained.
//
// Deliberately terse and unauthenticated, like /healthz: it carries no token
// material and no upstream error text, only a status and a fixed hint. The
// full error is in the log, which is where you were going to look anyway.
func (s *server) readyz(w http.ResponseWriter, _ *http.Request) {
	st := s.tokens.Status()
	status, hint, code := readyState(st, time.Now())

	body := map[string]any{"status": status}
	if hint != "" {
		body["hint"] = hint
	}
	if !st.ExpiresAt.IsZero() {
		body["access_token_expires_in_s"] = int64(st.ExpiresAt.Sub(time.Now()).Seconds())
	}
	if !st.LastRefresh.IsZero() {
		body["last_refresh"] = st.LastRefresh.UTC().Format(time.RFC3339)
	}
	writeJSON(w, code, body)
}

// readyState maps token health onto a probe verdict. Split out from the
// handler because the interesting part is which failures are worth going red
// over — a failing refresh on a token that is still valid is not one of them.
func readyState(st bs.AnthropicTokenStatus, now time.Time) (status, hint string, code int) {
	switch {
	case !st.Configured:
		return "no_tokens", "run `claude-proxy login`", http.StatusServiceUnavailable
	case st.Rejected:
		return "refresh_rejected", "refresh token is dead — run `claude-proxy login`", http.StatusServiceUnavailable
	case st.LastError != "" && !st.ExpiresAt.After(now):
		return "stale", "refresh failing and the access token has expired — check the log", http.StatusServiceUnavailable
	case st.LastError != "":
		// Refresh is failing but the current access token still works, so the
		// proxy is serving fine and a red probe here would be a false alarm.
		return "degraded", "last refresh failed; still serving on the current token", http.StatusOK
	default:
		return "ok", "", http.StatusOK
	}
}

func (s *server) notFound(w http.ResponseWriter, r *http.Request) {
	writeOpenAIError(w, http.StatusNotFound, "not_found",
		"unknown path "+r.Method+" "+r.URL.Path+"; supported: /v1/chat/completions, /v1/responses, /v1/models, /healthz, /readyz")
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
	// name in /v1/chat/completions is what matters at request time and is
	// passed straight through — a model missing from this list still works.
	// Keep it current anyway: a stale list is how a new model stays invisible
	// in every client dropdown long after it works fine.
	models := []map[string]any{
		{"id": "claude-opus-5", "object": "model", "created": 0, "owned_by": "anthropic"},
		{"id": "claude-opus-4-8", "object": "model", "created": 0, "owned_by": "anthropic"},
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
