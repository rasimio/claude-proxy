package main

import (
	"net/http"
	"testing"
	"time"

	bs "github.com/rasimio/blueship"
)

func TestReadyState(t *testing.T) {
	now := time.Now()
	valid := now.Add(4 * time.Hour)
	expired := now.Add(-1 * time.Minute)

	tests := []struct {
		name       string
		st         bs.AnthropicTokenStatus
		wantStatus string
		wantCode   int
	}{
		{
			name:       "healthy",
			st:         bs.AnthropicTokenStatus{Configured: true, ExpiresAt: valid},
			wantStatus: "ok",
			wantCode:   http.StatusOK,
		},
		{
			name:       "no token file yet",
			st:         bs.AnthropicTokenStatus{},
			wantStatus: "no_tokens",
			wantCode:   http.StatusServiceUnavailable,
		},
		{
			name:       "refresh token rejected is terminal",
			st:         bs.AnthropicTokenStatus{Configured: true, ExpiresAt: valid, LastError: "invalid_grant", Rejected: true},
			wantStatus: "refresh_rejected",
			wantCode:   http.StatusServiceUnavailable,
		},
		{
			// The whole point of the degraded rung: a blip at the OAuth
			// endpoint must not take the proxy out of rotation while the
			// access token it already holds is good for hours.
			name:       "transient refresh failure on a live token still serves",
			st:         bs.AnthropicTokenStatus{Configured: true, ExpiresAt: valid, LastError: "refresh request: EOF"},
			wantStatus: "degraded",
			wantCode:   http.StatusOK,
		},
		{
			name:       "refresh failing and token already expired",
			st:         bs.AnthropicTokenStatus{Configured: true, ExpiresAt: expired, LastError: "refresh request: EOF"},
			wantStatus: "stale",
			wantCode:   http.StatusServiceUnavailable,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, hint, code := readyState(tc.st, now)
			if status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", status, tc.wantStatus)
			}
			if code != tc.wantCode {
				t.Fatalf("code = %d, want %d", code, tc.wantCode)
			}
			if status != "ok" && hint == "" {
				t.Fatalf("status %q has no hint — the probe should say what to do", status)
			}
		})
	}
}
