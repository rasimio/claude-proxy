// claude-proxy — minimal HTTP server exposing OpenAI-compatible
// /v1/chat/completions, backed by an Anthropic Claude Code OAuth subscription
// via blueship's anthropicoauth + anthropic packages.
//
// Subcommands:
//
//	claude-proxy login   # interactive PKCE flow → writes data/anthropic-tokens.json
//	claude-proxy serve   # runs HTTP server (see env vars below)
//
// Env vars (serve):
//
//	PORT            (default "8080")
//	BIND            (default "0.0.0.0")
//	PROXY_API_KEY   (required) — n8n sends Authorization: Bearer <this>
//	TOKEN_FILE      (default "./data/anthropic-tokens.json")
//	DEFAULT_MODEL   (default "claude-sonnet-4-5-20250929")
//	REQUEST_TIMEOUT (default "300s") — upstream Anthropic call timeout
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "login":
		runLogin()
	case "serve":
		runServe()
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "claude-proxy — OpenAI-compatible proxy backed by Claude Code OAuth")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  claude-proxy login   interactive OAuth flow, writes token file")
	fmt.Fprintln(os.Stderr, "  claude-proxy serve   run HTTP server")
}
