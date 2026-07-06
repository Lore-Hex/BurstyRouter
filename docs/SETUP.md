# BurstyRouter Setup

This is the linear reference. For an interactive agent flow, use `skills/bursty-setup/SKILL.md`.

## 1. Install

```bash
go install github.com/Lore-Hex/BurstyRouter/cmd/burstyrouter@latest
```

Or download the latest release binary from:

```text
https://github.com/Lore-Hex/BurstyRouter/releases/latest
```

## 2. Run

Local-only with Ollama:

```bash
burstyrouter -local-url http://127.0.0.1:11434
```

Local plus TrustedRouter burst:

```bash
export TRUSTEDROUTER_API_KEY="tr_..."
burstyrouter -local-url http://127.0.0.1:11434 -tr-api-key "$TRUSTEDROUTER_API_KEY"
```

Local plus aliases for tool-facing cloud model names:

```bash
export TRUSTEDROUTER_API_KEY="tr_..."
burstyrouter -local-url http://127.0.0.1:11434 \
  -tr-api-key "$TRUSTEDROUTER_API_KEY" \
  -alias gpt-4o=llama3.2 \
  -alias anthropic/claude-haiku-4.5=qwen2.5-coder:32b \
  -savings-reference gpt-4o
```

The savings meter is an honest counterfactual: local tokens are priced only from TrustedRouter catalog prices, using the alias key first, then the requested TrustedRouter-known model, then `-savings-reference`. Without one of those price anchors, BurstyRouter counts tokens only and reports no saved dollars.

TrustedRouter-only:

```bash
export TRUSTEDROUTER_API_KEY="tr_..."
burstyrouter -tr-api-key "$TRUSTEDROUTER_API_KEY"
```

If the proxy is reachable from the internet, require a bearer token:

```bash
export BURSTY_TOKEN="$(openssl rand -hex 24)"
burstyrouter -local-url http://127.0.0.1:11434 -tr-api-key "$TRUSTEDROUTER_API_KEY" -token "$BURSTY_TOKEN"
```

Clients may authenticate with either `Authorization: Bearer $BURSTY_TOKEN` or `x-api-key: $BURSTY_TOKEN`.

## 3. Verify

Run the operator smoke against your local Ollama install:

```bash
scripts/smoke.sh
```

By default it uses `BURSTY_LOCAL_URL=http://127.0.0.1:11434` and starts BurstyRouter on `127.0.0.1:8383`. Set `BURSTY_MODEL` if you want to force a specific local model.

Without `BURSTY_TOKEN`:

```bash
export BURSTY_HOST="http://127.0.0.1:8383"
curl -fsS "$BURSTY_HOST/healthz"
curl -fsS "$BURSTY_HOST/v1/models"
curl -is "$BURSTY_HOST/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -d '{"model":"local/llama3.2","messages":[{"role":"user","content":"ping"}]}' \
  | awk 'BEGIN{found=0} /^X-Bursty-Route:/ {print; found=1} END{exit found?0:1}'
```

With `BURSTY_TOKEN`:

```bash
export BURSTY_HOST="http://127.0.0.1:8383"
curl -fsS "$BURSTY_HOST/healthz"
curl -fsS -H "Authorization: Bearer $BURSTY_TOKEN" "$BURSTY_HOST/v1/models"
curl -is "$BURSTY_HOST/v1/chat/completions" \
  -H "Authorization: Bearer $BURSTY_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"model":"local/llama3.2","messages":[{"role":"user","content":"ping"}]}' \
  | awk 'BEGIN{found=0} /^X-Bursty-Route:/ {print; found=1} END{exit found?0:1}'
```

With `x-api-key`:

```bash
curl -is "$BURSTY_HOST/v1/chat/completions" \
  -H "x-api-key: $BURSTY_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"ping"}]}' \
  | awk 'BEGIN{found=0} /^X-Bursty-Route:/ {print; found=1} END{exit found?0:1}'
```

## 3a. Burst To Another OpenAI-Compatible Cloud

TrustedRouter is the default burst target, but `-tr-base-url` may point at any bearer-keyed OpenAI-compatible `/v1` base URL.

```bash
export TRUSTEDROUTER_API_KEY="<upstream bearer token>"
burstyrouter -local-url http://127.0.0.1:11434 \
  -tr-api-key "$TRUSTEDROUTER_API_KEY" \
  -tr-base-url "https://openrouter.ai/api/v1"
```

Savings/pricing features use the TrustedRouter catalog. If the configured burst upstream lacks `/v1/messages` or `/v1/responses`, BurstyRouter returns a clean `501 endpoint_not_supported` envelope for those passthrough endpoints.

## 3b. Savings And Cloud Controls

Savings state is written to `$XDG_STATE_HOME/bursty/state.json` or `~/.bursty/state.json`; set `-state-file ""` to disable persistence.

Cloud egress modes:

```bash
# Normal local-first bursting.
burstyrouter -local-url http://127.0.0.1:11434 -tr-api-key "$TRUSTEDROUTER_API_KEY" -cloud auto

# No automatic bursts; only explicit non-local provider requests can use cloud.
burstyrouter -local-url http://127.0.0.1:11434 -tr-api-key "$TRUSTEDROUTER_API_KEY" -cloud explicit

# Disable cloud entirely.
burstyrouter -local-url http://127.0.0.1:11434 -tr-api-key "$TRUSTEDROUTER_API_KEY" -cloud off
```

Set a per-UTC-day cap:

```bash
burstyrouter -local-url http://127.0.0.1:11434 \
  -tr-api-key "$TRUSTEDROUTER_API_KEY" \
  -max-cloud-spend 1.00
```

Once priced cloud spend reaches the cap, cloud sends return `429 cloud_budget_exhausted` with `Retry-After` set to seconds until UTC midnight. Unpriced cloud usage counts `$0` toward this cap, and remains visible as unpriced token usage in `/stats`.

## 4. Expose With Ngrok

Use this only when the agent harness runs remotely and cannot reach localhost.

```bash
ngrok config add-authtoken "$NGROK_AUTHTOKEN"
ngrok http 8383
```

For a stable domain:

```bash
ngrok http --domain=<your-domain>.ngrok.app 8383
```

When internet-exposed, set `BURSTY_TOKEN`. Do not expose BurstyRouter without it.

```bash
export BURSTY_HOST="https://<your-domain>.ngrok.app"
curl -fsS -H "Authorization: Bearer $BURSTY_TOKEN" "$BURSTY_HOST/v1/models"
```

## 5. Wire A Harness

Use the public host for remote harnesses, for example `https://<your-domain>.ngrok.app`. Use `http://127.0.0.1:8383` for local harnesses.

When using a TrustedRouter SDK for Python, JavaScript, Swift, or Go against BurstyRouter, set both the inference base and the control base/catalog base to the BurstyRouter URL. If only inference is pointed at BurstyRouter, SDK catalog/account calls still go directly to the TrustedRouter control plane and bypass the proxy.

### Cursor

Settings -> Models -> OpenAI API override:

```text
Base URL: https://<host>/v1
API key: $BURSTY_TOKEN, or any string when BURSTY_TOKEN is unset
Models: local/llama3.2, anthropic/claude-haiku-4.5
```

With aliases, list the cloud-facing alias ids instead:

```text
Models: gpt-4o, anthropic/claude-haiku-4.5
```

Verify with a chat request:

```bash
curl -is "https://<host>/v1/chat/completions" \
  -H "Authorization: Bearer $BURSTY_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"model":"local/llama3.2","messages":[{"role":"user","content":"ping"}]}' \
  | awk 'BEGIN{found=0} /^X-Bursty-Route:/ {print; found=1} END{exit found?0:1}'
```

### Claude Code And Anthropic SDKs

BurstyRouter forwards `/v1/messages` to TrustedRouter. Local OpenAI servers do not serve this endpoint.

```bash
export ANTHROPIC_BASE_URL="https://<host>"
export ANTHROPIC_API_KEY="${BURSTY_TOKEN:-any-string}"
```

Anthropic-family clients send `x-api-key`; BurstyRouter accepts it when `BURSTY_TOKEN` is set.

Verify:

```bash
curl -is "https://<host>/v1/messages" \
  -H "x-api-key: $BURSTY_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"model":"anthropic/claude-haiku-4.5","max_tokens":16,"messages":[{"role":"user","content":"ping"}]}' \
  | awk 'BEGIN{found=0} /^X-Bursty-Route:/ {print; found=1} END{exit found?0:1}'
```

### Aider, OpenAI SDKs, Codex CLI, OpenHands

```bash
export OPENAI_BASE_URL="https://<host>/v1"
export OPENAI_API_KEY="${BURSTY_TOKEN:-any-string}"
```

If your tool uses the older variable:

```bash
export OPENAI_API_BASE="https://<host>/v1"
```

Verify:

```bash
curl -is "https://<host>/v1/chat/completions" \
  -H "Authorization: Bearer $BURSTY_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"model":"local/llama3.2","messages":[{"role":"user","content":"ping"}]}' \
  | awk 'BEGIN{found=0} /^X-Bursty-Route:/ {print; found=1} END{exit found?0:1}'
```

### Generic OpenAI-Compatible Integration

```text
Base URL: https://<host>/v1
API key: $BURSTY_TOKEN, or any string when BURSTY_TOKEN is unset
Model: local/<local-model>, an alias id such as gpt-4o, or a TrustedRouter model
```

## 6. Routing Cheatsheet

Pin local:

```json
{"model":"local/llama3.2","messages":[{"role":"user","content":"ping"}]}
```

Pin local with provider directives:

```json
{"model":"llama3.2","provider":{"only":["local"]},"messages":[{"role":"user","content":"ping"}]}
```

Force TrustedRouter:

```json
{"model":"anthropic/claude-haiku-4.5","provider":{"order":["anthropic"]},"messages":[{"role":"user","content":"ping"}]}
```

Alias cloud id to local model:

```bash
burstyrouter -local-url http://127.0.0.1:11434 \
  -alias gpt-4o=llama3.2 \
  -savings-reference gpt-4o
```

Allow unmapped local-native ids to burst with a fallback model:

```bash
burstyrouter -local-url http://127.0.0.1:11434 \
  -tr-api-key "$TRUSTEDROUTER_API_KEY" \
  -burst-fallback-model openai/gpt-4o-mini
```

Read routing:

```bash
curl -fsS -H "Authorization: Bearer $BURSTY_TOKEN" "$BURSTY_HOST/stats"
```

Response headers:

```http
X-Bursty-Route: local
X-Bursty-Reason: policy
```

Reasons: `policy`, `forced`, `burst-full`, `burst-error`, `burst-slow`.
