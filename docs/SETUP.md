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

Verify:

```bash
curl -is "https://<host>/v1/messages" \
  -H "Authorization: Bearer $BURSTY_TOKEN" \
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
Model: local/<local-model> or a TrustedRouter model
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

Read routing:

```bash
curl -fsS -H "Authorization: Bearer $BURSTY_TOKEN" "$BURSTY_HOST/stats"
```

Response headers:

```http
X-Bursty-Route: local
X-Bursty-Reason: policy
```

Reasons: `policy`, `forced`, `burst-full`, `burst-error`.
