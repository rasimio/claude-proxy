# claude-proxy — Claude Code Instructions

## What this is

OpenAI-compatible HTTP proxy that authenticates against Anthropic via the
Claude Code OAuth subscription flow. Anything that speaks OpenAI Chat
Completions or Responses API (n8n, LangChain, Vercel AI SDK, …) can hit this
and get Claude responses billed against the user's Claude Pro/Max
subscription instead of per-token API credits.

Built on top of `github.com/rasimio/blueship` — specifically its
`internal/anthropic` provider and `internal/anthropicoauth` token store.
We do not import those packages directly (they're internal); we call
the public factory `blueship.AnthropicOAuth(...)` which returns a
`bs.CompletionProvider` that also satisfies `bs.StreamCompletionProvider`.

## Layout — flat by design

```
main.go         entry, subcommand dispatch
login.go        interactive PKCE OAuth flow (writes tokens.json)
server.go       HTTP server, routes, auth middleware, access log
translate.go    OpenAI Chat Completions ↔ blueship request/response
stream.go      /v1/chat/completions SSE streaming
responses.go    /v1/responses (OpenAI Responses API) request/response/stream
Dockerfile      multi-stage; clones blueship inside builder via git
docker-compose.yml   service + healthcheck + ./data volume
.env.example    config template
```

One package, one binary, two subcommands. No internal/ tree, no factories,
no DI framework. Add a new file when a concern grows past ~300 lines, not
because layering feels nicer.

## Style — Karpathy mode

We are not building a platform. We are building one HTTP proxy whose job
is small and well-defined. Code should reflect that.

- **Single file per concern.** Don't split `translate.go` into a package
  because the file is "long" — split when there are genuinely two unrelated
  axes of variation.
- **No interfaces until two implementations exist.** `*server` is a
  concrete struct, not behind an interface. We have one HTTP server.
- **No constructors that take 12 args.** If wiring is gnarly, the wiring
  itself is a smell. Re-shape the dependencies, don't paper with a builder.
- **Plain structs.** `serveConfig` is a struct with public fields read
  directly. No getters, no validation builders, no `Option` pattern.
- **Comments explain *why*, not *what*.** The code shows what. Add a
  comment only when the next reader will ask "why is this here / why this
  way" — past incidents, non-obvious constraints, links to spec quirks.
- **Three similar lines beat one wrong abstraction.** Don't dedupe
  speculatively. If `openaiToBlueship` and `responsesToBlueship` share
  shape, that's fine — they will diverge as both APIs evolve, and a
  premature shared helper will fight that drift.
- **Validate at boundaries, trust the inside.** HTTP request gets parsed
  and bounds-checked; downstream code assumes the parse succeeded. No
  defensive `if x == nil` in every internal function.
- **Errors carry context but don't get wrapped to death.** One layer of
  `fmt.Errorf("…: %w", err)` per logical boundary. Not five.

## Dependencies

- Go 1.26
- `github.com/rasimio/blueship` — pinned in Dockerfile via `BLUESHIP_REV`
  build arg
- Pulls a lot of transitive deps from blueship (postgres, redis, otel,
  chromedp, …) that we don't use at runtime. They don't hurt — binary is
  ~10 MB after `-ldflags="-s -w"`. Don't bother pruning unless we hit a
  CVE or build time becomes painful.

## How OAuth actually flows

1. `claude-proxy login` generates PKCE verifier + challenge + state.
2. Prints an authorize URL pointing at `claude.ai/oauth/authorize` with the
   public Claude Code OAuth client_id (`9d1c250a-…`) — same client_id
   the real Claude Code CLI uses.
3. User opens URL in a browser, signs in with their Claude Pro/Max
   account, lands on `console.anthropic.com/oauth/code/callback#<code>#<state>`.
4. User pastes the code (or the whole callback URL) back into the CLI.
5. CLI exchanges `code + code_verifier` → `{access_token, refresh_token,
   expires_at}`, writes JSON to `./data/anthropic-tokens.json` (matches the
   on-disk format `blueship.anthropicoauth.TokenData` expects).
6. At `serve` time, `blueship.AnthropicOAuth(refreshToken="", tokenFile,
   …)` constructs a `TokenStore` that loads the file, auto-refreshes the
   access token before it expires, and rotates the refresh token when
   Anthropic returns a new one.
7. Each request to `api.anthropic.com/v1/messages` carries
   `Authorization: Bearer <access_token>` plus
   `anthropic-beta: oauth-2025-04-20`. blueship's `anthropic.Provider`
   also prepends a `system` block `You are Claude Code, Anthropic's
   official CLI for Claude.` — Anthropic rejects OAuth-authed inference
   requests without it. **Do not strip this** even if it bleeds into
   responses.

## Routes

| Path | Auth | Notes |
|---|---|---|
| `POST /v1/chat/completions` | Bearer | OpenAI Chat Completions (legacy SDKs, LangChain JS v0.x, Vercel AI SDK) |
| `POST /chat/completions` | Bearer | alias for clients that don't prepend `/v1` |
| `POST /v1/responses` | Bearer | OpenAI Responses API (LangChain JS v1+, n8n) |
| `POST /responses` | Bearer | alias |
| `GET /v1/models` | Bearer | static list of four short-name models |
| `GET /models` | Bearer | alias |
| `GET /healthz` | none | `200 ok` |
| `*` | — | 404 with a helpful message listing supported paths |

`accessLog` middleware logs `method`, `path`, `status`, `dur_ms`, `ua` on
every request. **Trust this log** — when a client gets a 404, it tells you
exactly which path the client built.

## Deploy

Production: `ssh root@188.166.99.177`, code in `/opt/claude-proxy/`,
managed by `docker compose`.

```bash
# update
ssh root@188.166.99.177 'cd /opt/claude-proxy && git pull && \
  docker compose build && docker compose up -d'

# logs
ssh root@188.166.99.177 'cd /opt/claude-proxy && docker compose logs -f claude-proxy'

# health from outside
curl -s http://188.166.99.177:8080/healthz
```

CI is not set up. Push to main + ssh + rebuild is the workflow. Set up GH
Actions only if deploys start being frequent enough to matter.

## Things that have bitten us

- **`git commit -am` does NOT include new untracked files.** When you add
  a new `.go` file, `git add` it explicitly before committing. Lost one
  redeploy cycle to this.
- **gopls shows phantom errors** because the working dir is not in any
  `go.work` file. `go build` is the source of truth.
- **LangChain JS v1+ uses `/v1/responses`** by default, not
  `chat/completions`. The Responses API shape is different — top-level
  `input` (string or array), `instructions` instead of system messages,
  flat tool definitions (`{"type":"function","name","parameters"}`
  without nested `function:{…}`), streaming via typed SSE events
  (`response.output_text.delta` etc).
- **LangChain JS calls `.map()` on `output[i].content` and
  `content[j].annotations` unconditionally.** When those are missing or
  `null` it crashes with "Cannot read properties of undefined (reading
  'map')". `responsesItem` and `responsesOutputPart` implement custom
  `MarshalJSON` to emit empty arrays, not `null`. Don't drop those.
- **Claude wraps JSON in ` ```json … ``` ` fences** even when system
  prompt forbids it. `response_format` non-streaming path runs the output
  through `stripJSONFences` as a safety net. Streaming JSON mode does
  not strip (would require buffering the whole stream); document that.
- **`tool_choice: "required"` and force-specific tool** are not
  first-class. blueship's `CompletionRequest` doesn't expose
  `tool_choice`. We nudge via an injected system instruction — works
  most of the time but isn't a hard force. Patching blueship core is
  the right fix when this becomes a problem.
- **Token rotation is concurrency-safe** because blueship's `TokenStore`
  serializes refresh with a mutex and reloads from disk on a rotation
  race. We don't need our own locking around it.

## Env vars (serve)

| Var | Default | Meaning |
|---|---|---|
| `PROXY_API_KEY` | — required — | Bearer token clients must send |
| `TOKEN_FILE` | `./data/anthropic-tokens.json` | path to OAuth tokens |
| `DEFAULT_MODEL` | `claude-opus-4-7` | used when client omits `model` |
| `REQUEST_TIMEOUT` | `300s` | upstream timeout for non-streaming |
| `PORT` | `8080` | listen port |
| `BIND` | `0.0.0.0` | listen interface |

In Docker `TOKEN_FILE` is overridden to `/data/anthropic-tokens.json`
because the `./data` host directory is mounted at `/data`.

## When in doubt

- Test the change with `curl` before assuming the bug is in the model.
- Read the access log first — most n8n/LangChain integration failures
  are URL mismatch (wrong base URL), not protocol bugs.
- `docker compose logs -f claude-proxy` shows everything: requests,
  upstream errors, token refreshes.
