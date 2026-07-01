# claude-proxy

An OpenAI-compatible HTTP proxy that authenticates against Anthropic using a
**Claude Pro/Max subscription** (via the Claude Code OAuth flow) instead of
a per-token API key.

If you already pay for Claude Pro or Max, this lets you point any tool that
speaks OpenAI's Chat Completions or Responses API — **n8n, LangChain,
Vercel AI SDK, your own scripts** — at Claude without spinning up a
metered Anthropic API account.

```
n8n / LangChain / cURL
        │
        │  POST /v1/chat/completions     (OpenAI shape)
        │  POST /v1/responses
        ▼
   claude-proxy  ──── Bearer PROXY_API_KEY check
        │
        │  OAuth access_token (auto-refreshed)
        ▼
  api.anthropic.com/v1/messages          (real Anthropic API)
        │
        ▼
  bills your Claude Pro/Max subscription
```

---

## Features

- **OpenAI Chat Completions** (`/v1/chat/completions`) — works with most
  classic OpenAI SDKs and LangChain JS v0.x
- **OpenAI Responses API** (`/v1/responses`) — works with LangChain JS v1+,
  n8n's OpenAI Chat Model node, Vercel AI SDK newer versions
- **Streaming** (SSE) on both endpoints
- **Function / tool calling** — both `tools` definitions and `tool_calls`
  in the message history are translated round-trip
- **Vision** — `image_url` content parts (data: URLs) get forwarded as
  Anthropic image blocks
- **JSON mode** — `response_format: {type: json_object}` and
  `{type: json_schema}` translate into system instructions plus
  best-effort fence stripping on output
- **Models** — `claude-opus-4-7`, `claude-sonnet-5`,
  `claude-sonnet-4-6`, `claude-haiku-4-5` (whichever the client requests;
  falls back to `DEFAULT_MODEL`)
- **Token rotation** — handled by the underlying blueship token store;
  the proxy survives Anthropic rotating refresh tokens
- **Access log** — every request is logged with method, path, status,
  duration, and User-Agent, which makes diagnosing integration issues
  trivial

---

## Quickstart (on the server)

Prerequisites:
- A Linux host with Docker + Docker Compose v2
- A Claude account with an active **Pro** or **Max** subscription
- A browser somewhere you can sign in to claude.ai

```bash
# 1) Clone
git clone https://github.com/rasimio/claude-proxy.git
cd claude-proxy

# 2) Configure
cp .env.example .env
$EDITOR .env
#   set PROXY_API_KEY to a strong random string, e.g.
#   PROXY_API_KEY=$(openssl rand -hex 32)

# 3) Build the image
docker compose build

# 4) Authenticate (interactive — needs your browser)
docker compose run --rm claude-proxy login
#   - the CLI prints a URL
#   - open it in a browser, sign in with your Claude Pro/Max account
#   - you'll land on console.anthropic.com/oauth/code/callback with a
#     code in the URL fragment (looks like #<code>#<state>)
#   - copy the WHOLE callback URL (or just the `<code>#<state>` portion)
#     and paste it back into the terminal
#   - tokens are written to ./data/anthropic-tokens.json

# 5) Start the server
docker compose up -d
docker compose logs -f claude-proxy
#   you should see: "claude-proxy listening" on :8080

# 6) Smoke test
curl -s http://localhost:8080/healthz
# → ok

curl -s -H "Authorization: Bearer $PROXY_API_KEY" \
  http://localhost:8080/v1/models | jq .
```

---

## The OAuth login, step by step

The login command runs an OAuth 2.0 Authorization Code flow with PKCE,
using the same public client ID the real Claude Code CLI uses
(`9d1c250a-e61b-44d9-88ed-5944d1962f5e`). Anthropic doesn't allow a
`localhost` redirect, so the flow has to be interactive: you paste the
callback code back into the CLI.

### Step 1 — start the flow

```bash
docker compose run --rm claude-proxy login
```

You'll see something like:

```
Open this URL in your browser and sign in with the Claude account that has
the Claude Code subscription:

https://claude.ai/oauth/authorize?client_id=9d1c250a-…&code=true&code_challenge=…&code_challenge_method=S256&redirect_uri=https%3A%2F%2Fconsole.anthropic.com%2Foauth%2Fcode%2Fcallback&response_type=code&scope=org%3Acreate_api_key+user%3Aprofile+user%3Ainference&state=…

After signing in you'll land on a page showing a code (or be redirected to
console.anthropic.com/oauth/code/callback).
Paste here either:
  - the bare code (looks like `<code>#<state>`),
  - or the full callback URL.

code:
```

### Step 2 — open the URL

Copy that long URL, paste into your browser. Sign in with the Google /
email tied to your Claude subscription. Approve the consent screen if it
appears.

### Step 3 — grab the code

After approval Anthropic redirects you to
`https://console.anthropic.com/oauth/code/callback#<long-code>#<state>`.

That URL fragment after the `#` is what you need. Either:
- Copy the whole address bar URL, or
- Just the `<long-code>#<state>` portion

### Step 4 — paste it back

Paste it into the terminal where the login CLI is waiting. It will
exchange the code for tokens and write them to
`./data/anthropic-tokens.json`. You'll see:

```
Tokens written to /data/anthropic-tokens.json
You can now run: claude-proxy serve
```

### Step 5 — bring up the server

```bash
docker compose up -d
```

The token file persists in the mounted `./data` volume — you don't have
to log in again unless you delete it or Anthropic invalidates your
subscription session.

---

## Using it from n8n

In n8n's **OpenAI Chat Model** (the LangChain one — found inside agent
workflows) or any other "OpenAI compatible" node:

1. **Create a new OpenAI credential**
   - **API Key**: your `PROXY_API_KEY` from `.env`
   - **Base URL**: `http://<host>:8080/v1` — both with and without `/v1`
     work, the proxy aliases the routes. Use whatever the node accepts.

2. **In the node parameters**
   - **Model name**: `claude-opus-4-7`, `claude-sonnet-5`,
     `claude-sonnet-4-6`, or `claude-haiku-4-5`
   - **Temperature**, **Max tokens**, etc. work as usual

3. **HTTPS** — out of the box claude-proxy serves plain HTTP. If n8n is
   on a different host and you want TLS, terminate it with a reverse
   proxy in front (Caddy is two lines of config). See "Adding HTTPS"
   below.

---

## Using it from cURL

```bash
export KEY=<your-proxy-api-key>
export URL=http://localhost:8080
```

### List models

```bash
curl -s -H "Authorization: Bearer $KEY" $URL/v1/models | jq .
```

### Simple chat completion (non-streaming)

```bash
curl -s -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  $URL/v1/chat/completions -d '{
    "model": "claude-sonnet-4-6",
    "max_tokens": 200,
    "messages": [{"role":"user","content":"объясни рекурсию одним предложением"}]
  }' | jq .
```

### With a system prompt

```bash
curl -s -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  $URL/v1/chat/completions -d '{
    "model": "claude-opus-4-7",
    "max_tokens": 300,
    "temperature": 0.3,
    "messages": [
      {"role":"system","content":"Ты sql-эксперт. Отвечай только SQL без объяснений."},
      {"role":"user","content":"топ-10 пользователей по числу заказов"}
    ]
  }' | jq .
```

### Streaming (Server-Sent Events)

```bash
curl -sN -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  $URL/v1/chat/completions -d '{
    "model": "claude-haiku-4-5",
    "stream": true,
    "max_tokens": 200,
    "messages": [{"role":"user","content":"посчитай от 1 до 5"}]
  }'
```

### Function calling — round trip

Step 1: send a tool definition along with the user request.

```bash
curl -s -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  $URL/v1/chat/completions -d '{
    "model":"claude-sonnet-4-6",
    "messages":[{"role":"user","content":"weather in Tokyo?"}],
    "tools":[{
      "type":"function",
      "function":{
        "name":"get_weather",
        "description":"Get weather for a city",
        "parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}
      }
    }]
  }'
```

The response will carry a `tool_calls` array and `finish_reason:"tool_calls"`.

Step 2: execute the tool yourself (or let your agent framework do it),
then send the entire conversation back including a tool-role message
with the result:

```bash
curl -s -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  $URL/v1/chat/completions -d '{
    "model":"claude-sonnet-4-6",
    "messages":[
      {"role":"user","content":"weather in Tokyo?"},
      {"role":"assistant","content":"","tool_calls":[
        {"id":"toolu_abc","type":"function",
         "function":{"name":"get_weather","arguments":"{\"city\":\"Tokyo\"}"}}
      ]},
      {"role":"tool","tool_call_id":"toolu_abc","content":"{\"temp\":22,\"sky\":\"clear\"}"}
    ],
    "tools":[{
      "type":"function",
      "function":{"name":"get_weather","description":"x","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}
    }]
  }'
```

In a framework like n8n's Tool Agent / LangChain agent, you don't write
this loop manually — the framework handles it.

### Vision

```bash
curl -s -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  $URL/v1/chat/completions -d '{
    "model":"claude-sonnet-4-6",
    "max_tokens":200,
    "messages":[{
      "role":"user",
      "content":[
        {"type":"text","text":"Describe this image."},
        {"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo…"}}
      ]
    }]
  }'
```

Only `data:image/...;base64,...` URLs are forwarded. `http(s)://` image
URLs are dropped silently — pre-encode them base64 on your side, or open
an issue if this becomes painful.

### JSON mode

```bash
curl -s -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  $URL/v1/chat/completions -d '{
    "model":"claude-haiku-4-5",
    "max_tokens":200,
    "response_format":{"type":"json_object"},
    "messages":[{"role":"user","content":"3 facts about Moscow in fields city, country, facts"}]
  }'
```

Non-streaming responses get code fences (` ```json … ``` `) stripped if
Claude wraps the JSON. Streaming JSON mode does NOT strip fences (would
require buffering the whole stream); use non-streaming if you need
guaranteed raw JSON.

### Responses API (`/v1/responses`)

If you're using a recent LangChain JS or Vercel AI SDK, it'll hit this
path automatically. The wire format is different from Chat Completions:

```bash
curl -s -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  $URL/v1/responses -d '{
    "model":"claude-opus-4-7",
    "input":"скажи привет",
    "max_output_tokens":50
  }'
```

Or with structured input:

```bash
curl -s -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  $URL/v1/responses -d '{
    "model":"claude-sonnet-4-6",
    "instructions":"You write only haiku.",
    "input":[
      {"type":"message","role":"user",
       "content":[{"type":"input_text","text":"docker"}]}
    ],
    "tools":[{
      "type":"function","name":"get_weather",
      "description":"Get weather",
      "parameters":{"type":"object","properties":{"city":{"type":"string"}}}
    }],
    "max_output_tokens":300
  }'
```

Streaming emits typed events:
`response.created` → `response.in_progress` → `response.output_item.added`
→ `response.output_text.delta` (per chunk) → `response.output_text.done`
→ `response.completed` → `data: [DONE]`.

---

## Configuration

All config is via env vars (use `.env` with docker-compose, or export
directly if running the binary).

| Var | Default | Required | Meaning |
|---|---|---|---|
| `PROXY_API_KEY` | — | yes | Bearer token clients must send |
| `TOKEN_FILE` | `./data/anthropic-tokens.json` | no | Where login writes / serve reads OAuth tokens |
| `DEFAULT_MODEL` | `claude-opus-4-7` | no | Used when request omits `model` |
| `REQUEST_TIMEOUT` | `300s` | no | Per-request upstream timeout (non-streaming) |
| `PORT` | `8080` | no | Listen port |
| `BIND` | `0.0.0.0` | no | Listen interface |
| `HOST_PORT` | `8080` | no | docker-compose port mapping on the host |
| `BIND_ADDR` | `0.0.0.0` | no | docker-compose bind interface on the host |
| `BLUESHIP_REV` | `3f07ab03a7567f25d4b8ece943374d570f47ab44` | no | git revision of blueship to clone in Docker build |

---

## Models

Four short names are exposed:

| Model | Use for |
|---|---|
| `claude-opus-4-7` | Smartest. Default. Best for complex reasoning, code, agents. |
| `claude-sonnet-5` | Newest Sonnet. Strong agentic coding and tool-use balance. |
| `claude-sonnet-4-6` | Balanced. Good for most n8n workflows. |
| `claude-haiku-4-5` | Fastest / cheapest. Quick classifications, simple chats. |

Anthropic accepts these short names and date-suffixed forms
(`claude-opus-4-7-20…`) interchangeably. The proxy passes whatever you
send through; it doesn't validate the model name client-side.

---

## Limitations / caveats

### Function calling

- **Streaming tool arguments arrive as a single chunk**, not token-by-token.
  blueship surfaces tool input only once the JSON is fully assembled
  (Anthropic's `content_block_stop` event). LangChain, Vercel AI SDK,
  n8n agents accumulate args fine. A bespoke SDK that expects per-token
  argument deltas would break.
- **`tool_choice: "required"` and force-specific** are best-effort. They
  get translated into a system-prompt nudge (`You MUST call the tool
  named X`). Claude almost always complies, but it isn't a hard
  guarantee — Anthropic's API has a real `tool_choice` field, but
  blueship's `CompletionRequest` doesn't currently expose it.
- **`strict: true` on tool input schema is ignored.** Claude is well-
  behaved about schemas but not 100%. Validate args on your side if
  you need certainty.
- **Built-in tools** (`web_search`, `file_search`, `code_interpreter`,
  `computer_use`) are dropped. Only `type:"function"` tools are translated.

### Vision

- Only `data:image/...;base64,...` content parts are forwarded.
- `http(s)://` image URLs are silently dropped (blueship's
  `ImageSource` is base64-only).
- Vision **inside `tool_result`** is not forwarded — text only.

### JSON mode

- `response_format: {type: "json_object"}` and `{type: "json_schema"}`
  translate to a system instruction. There is no first-class JSON
  guarantee from Anthropic.
- Code fences are stripped from non-streaming output as a safety net.
  Streaming JSON mode does not strip (would require buffering).

### Streaming

- Anthropic streams via its own event-typed SSE. We translate it to
  the OpenAI shape — text deltas line up cleanly, tool calls arrive
  as single chunks, finish reasons map.
- If the upstream stream errors mid-flight, we emit a terminal chunk
  with `finish_reason: stop` (we can't switch back to a JSON error
  response after sending stream headers).

### Identity bleed

OAuth-authed requests must carry the system block
`You are Claude Code, Anthropic's official CLI for Claude.` — Anthropic
requires it. blueship injects this automatically. Sometimes Claude
introduces itself as "Claude Code" in responses; that's a side effect
of the required identity, not a bug in the proxy. You can override it
with a strong system prompt of your own.

### Multi-tenancy

This is single-tenant: one OAuth token file, one upstream Claude
account. If you need multiple Claude accounts behind one proxy,
this isn't the design — you'd want to key by `PROXY_API_KEY` →
token file. Open an issue if you actually need that.

---

## Updating

```bash
ssh root@<host> 'cd /opt/claude-proxy && \
  git pull && \
  docker compose build && \
  docker compose up -d'
```

To pin to a different `blueship` revision:

```bash
# in .env
BLUESHIP_REV=<sha or tag>
# then
docker compose build --no-cache
docker compose up -d
```

---

## Adding HTTPS

claude-proxy serves plain HTTP. For TLS, put Caddy in front:

`Caddyfile`:
```
your.domain.com {
  reverse_proxy claude-proxy:8080
}
```

Extend `docker-compose.yml` with:

```yaml
  caddy:
    image: caddy:2-alpine
    restart: unless-stopped
    ports:
      - "80:80"
      - "443:443"
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile
      - caddy_data:/data
      - caddy_config:/config
    depends_on:
      - claude-proxy

volumes:
  caddy_data:
  caddy_config:
```

Point a DNS A record at the server, Caddy will provision a Let's
Encrypt cert automatically. Use `https://your.domain.com/v1` as the
base URL from n8n.

---

## Troubleshooting

### `404 page not found` from n8n / LangChain

Check the access log:
```bash
docker compose logs claude-proxy | grep '"path"'
```
You'll see the exact path the client requested. Two common cases:
- Path is `/v1/responses` — your LangChain version uses Responses API,
  which is supported, but you may have hit it before this proxy added
  Responses support. Update.
- Path is something weird like `/openai/v1/chat/completions` — your
  client is prepending an extra prefix. Strip `/openai` from the base
  URL in the client.

### `Cannot read properties of undefined (reading 'map')`

LangChain JS's Responses-API parser. The proxy is supposed to emit
`content: []` and `annotations: []` (not `null`) on every output item;
custom `MarshalJSON` enforces this. If you see this after an update,
something regressed — open an issue with the response payload.

### `invalid_request_error: no user/assistant messages`

You sent `messages: []` or only system/tool messages. Anthropic
requires at least one user or assistant turn.

### `anthropic API status 401`

Your OAuth token expired and the refresh failed. Most likely your
Claude subscription session ended (Anthropic invalidates refresh
tokens when you sign out, change password, or after long inactivity).
Re-run:
```bash
docker compose run --rm claude-proxy login
docker compose restart claude-proxy
```

### `anthropic API status 429` / `overloaded`

Anthropic is rate-limiting or temporarily overloaded. blueship's
client has built-in retry with backoff (1s, 2s, 5s); if you still
see this it's worth waiting.

### Tokens not persisting between restarts

Check that `./data/` exists in your project root and the
docker-compose volume mount actually wrote to it:
```bash
ls -la ./data/
cat ./data/anthropic-tokens.json | jq .
```

---

## Building locally (without Docker)

```bash
# claude-proxy assumes blueship is a sibling directory.
mkdir -p ../blueship && git clone https://github.com/rasimio/blueship.git ../blueship

go build -o claude-proxy .
./claude-proxy login
PROXY_API_KEY=$(openssl rand -hex 32) ./claude-proxy serve
```

The `go.mod` has `replace github.com/rasimio/blueship => ../blueship`,
so blueship must literally live one directory up from `claude-proxy/`.

---

## License

MIT. Use it however you like. No warranty.

## Acknowledgements

- [`blueship`](https://github.com/rasimio/blueship) — the framework
  this proxy thinly wraps; it owns the actual Anthropic OAuth + Messages
  API client code.
- The Claude Code CLI team at Anthropic for the OAuth subscription
  flow that makes this possible.
