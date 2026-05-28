package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PKCE constants — public Claude Code OAuth client. Same values used by the
// Anthropic CLI; mirrored here so the login flow has no internal-package
// dependency on blueship.
const (
	loginAuthURL     = "https://claude.ai/oauth/authorize"
	loginTokenURL    = "https://console.anthropic.com/v1/oauth/token"
	loginClientID    = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	loginRedirectURI = "https://console.anthropic.com/oauth/code/callback"
	loginScopes      = "org:create_api_key user:profile user:inference"
)

// persistedTokens matches blueship.anthropicoauth.TokenData on the wire so the
// runtime TokenStore can load the file we write without translation.
type persistedTokens struct {
	Access    string `json:"access_token"`
	Refresh   string `json:"refresh_token"`
	ExpiresAt int64  `json:"expires_at"`
}

func runLogin() {
	tokenFile := envOr("TOKEN_FILE", "./data/anthropic-tokens.json")

	verifier, challenge := mustPKCE()
	state := mustState()

	authURL := buildAuthURL(challenge, state)

	fmt.Fprintln(os.Stderr, "Open this URL in your browser and sign in with the Claude account that has the Claude Code subscription:")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, authURL)
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "After signing in you'll land on a page showing a code (or be redirected to console.anthropic.com/oauth/code/callback).")
	fmt.Fprintln(os.Stderr, "Paste here either:")
	fmt.Fprintln(os.Stderr, "  - the bare code (looks like `<code>#<state>`),")
	fmt.Fprintln(os.Stderr, "  - or the full callback URL.")
	fmt.Fprintln(os.Stderr)
	fmt.Fprint(os.Stderr, "code: ")

	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		fmt.Fprintln(os.Stderr, "no input")
		os.Exit(1)
	}
	code, gotState := parseCallbackInput(strings.TrimSpace(scanner.Text()))
	if code == "" {
		fmt.Fprintln(os.Stderr, "could not parse code from input")
		os.Exit(1)
	}
	if gotState != "" && gotState != state {
		fmt.Fprintln(os.Stderr, "state mismatch — abort")
		os.Exit(1)
	}

	tok, err := exchangeCode(code, verifier, state)
	if err != nil {
		fmt.Fprintf(os.Stderr, "token exchange: %v\n", err)
		os.Exit(1)
	}

	if err := os.MkdirAll(filepath.Dir(tokenFile), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "create token dir: %v\n", err)
		os.Exit(1)
	}

	data := persistedTokens{
		Access:    tok.AccessToken,
		Refresh:   tok.RefreshToken,
		ExpiresAt: time.Now().Unix() + int64(tok.ExpiresIn),
	}
	raw, _ := json.MarshalIndent(data, "", "  ")
	tmp := tokenFile + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "write token file: %v\n", err)
		os.Exit(1)
	}
	if err := os.Rename(tmp, tokenFile); err != nil {
		os.Remove(tmp)
		fmt.Fprintf(os.Stderr, "rename token file: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintln(os.Stderr)
	fmt.Fprintf(os.Stderr, "Tokens written to %s\n", tokenFile)
	fmt.Fprintln(os.Stderr, "You can now run: claude-proxy serve")
}

func buildAuthURL(challenge, state string) string {
	params := url.Values{
		"code":                  {"true"},
		"client_id":             {loginClientID},
		"response_type":         {"code"},
		"redirect_uri":          {loginRedirectURI},
		"scope":                 {loginScopes},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
	}
	return loginAuthURL + "?" + params.Encode()
}

type tokenExchangeResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
	Description  string `json:"error_description"`
}

func exchangeCode(code, verifier, state string) (*tokenExchangeResponse, error) {
	reqBody, _ := json.Marshal(map[string]string{
		"grant_type":    "authorization_code",
		"code":          code,
		"redirect_uri":  loginRedirectURI,
		"client_id":     loginClientID,
		"code_verifier": verifier,
		"state":         state,
	})
	req, _ := http.NewRequest("POST", loginTokenURL, bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var tok tokenExchangeResponse
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return nil, fmt.Errorf("decode response (HTTP %d): %w", resp.StatusCode, err)
	}
	if tok.RefreshToken == "" {
		return nil, fmt.Errorf("HTTP %d: %s — %s", resp.StatusCode, tok.Error, tok.Description)
	}
	return &tok, nil
}

func mustPKCE() (string, string) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	v := base64.RawURLEncoding.EncodeToString(b)
	h := sha256.Sum256([]byte(v))
	return v, base64.RawURLEncoding.EncodeToString(h[:])
}

func mustState() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", b)
}

func parseCallbackInput(s string) (code, state string) {
	if u, err := url.Parse(s); err == nil && (u.Scheme != "" || u.Host != "") {
		if c := u.Query().Get("code"); c != "" {
			return c, u.Query().Get("state")
		}
		if frag := u.Fragment; frag != "" {
			c, st, _ := strings.Cut(frag, "&state=")
			if c != "" {
				return c, st
			}
		}
	}
	if strings.Contains(s, "#") {
		c, st, _ := strings.Cut(s, "#")
		return c, st
	}
	if i := strings.Index(s, "?"); i > 0 {
		c := s[:i]
		v, _ := url.ParseQuery(s[i+1:])
		return c, v.Get("state")
	}
	return s, ""
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
