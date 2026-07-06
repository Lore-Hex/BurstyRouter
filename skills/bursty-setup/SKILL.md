---
name: bursty-setup
description: Interactively set up BurstyRouter. Use when a user asks to set up burstyrouter, wire bursty into Cursor, expose my local model, publish a local model through ngrok, configure Claude Code or Anthropic SDKs with BurstyRouter, configure aider, OpenAI SDKs, Codex CLI, or OpenHands with BurstyRouter, or burst local requests to TrustedRouter.
---

# BurstyRouter Setup

Guide the user through setup interactively. Ask for the missing choice, then emit only the relevant copy-paste blocks.

## First Questions

Ask:

1. Which run mode: Ollama local-only, local plus TrustedRouter burst, or TrustedRouter-only?
2. Is the harness remote and unable to reach localhost? If yes, include ngrok.
3. Which harness: Cursor, Claude Code / Anthropic SDKs, aider / OpenAI SDKs / Codex CLI / OpenHands, or generic OpenAI-compatible?

After the user answers, show only:

- The selected install/run block.
- The selected ngrok block if needed.
- The selected harness wiring block.
- One verification block.
- The routing cheatsheet.

## Install And Run

Install:

```bash
go install github.com/Lore-Hex/BurstyRouter/cmd/burstyrouter@latest
```

If Go install is not desired, tell the user to download the latest release binary from:

```text
https://github.com/Lore-Hex/BurstyRouter/releases/latest
```

Ollama local-only:

```bash
burstyrouter -local-url http://127.0.0.1:11434
```

Local plus TrustedRouter burst:

```bash
export TRUSTEDROUTER_API_KEY="tr_..."
burstyrouter -local-url http://127.0.0.1:11434 -tr-api-key "$TRUSTEDROUTER_API_KEY"
```

Local plus aliases for cloud-facing model names:

```bash
export TRUSTEDROUTER_API_KEY="tr_..."
burstyrouter -local-url http://127.0.0.1:11434 \
  -tr-api-key "$TRUSTEDROUTER_API_KEY" \
  -alias gpt-4o=llama3.2 \
  -alias anthropic/claude-haiku-4.5=qwen2.5-coder:32b \
  -savings-reference gpt-4o
```

Explain briefly when aliases are shown: the savings number is an honest counterfactual from TrustedRouter catalog prices. With aliases, the alias key is the preferred price reference; `-savings-reference` is the fallback for local-native names. Without a catalog price anchor, BurstyRouter reports tokens only and no saved dollars.

To burst unmapped local-native model ids, add a fallback model:

```bash
burstyrouter -local-url http://127.0.0.1:11434 \
  -tr-api-key "$TRUSTEDROUTER_API_KEY" \
  -burst-fallback-model openai/gpt-4o-mini
```

TrustedRouter-only:

```bash
export TRUSTEDROUTER_API_KEY="tr_..."
burstyrouter -tr-api-key "$TRUSTEDROUTER_API_KEY"
```

Verify local process:

```bash
export BURSTY_HOST="http://127.0.0.1:8383"
curl -fsS "$BURSTY_HOST/healthz"
curl -fsS "$BURSTY_HOST/v1/models"
curl -is "$BURSTY_HOST/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -d '{"model":"local/llama3.2","messages":[{"role":"user","content":"ping"}]}' \
  | awk 'BEGIN{found=0} /^X-Bursty-Route:/ {print; found=1} END{exit found?0:1}'
```

## Ngrok

Use ngrok only for a remote harness. Strongly instruct the user to set `BURSTY_TOKEN` whenever BurstyRouter is internet-exposed.

```bash
export BURSTY_TOKEN="$(openssl rand -hex 24)"
burstyrouter -local-url http://127.0.0.1:11434 -tr-api-key "$TRUSTEDROUTER_API_KEY" -token "$BURSTY_TOKEN"
```

```bash
ngrok config add-authtoken "$NGROK_AUTHTOKEN"
ngrok http 8383
```

Stable domain:

```bash
ngrok http --domain=<your-domain>.ngrok.app 8383
```

Token verification:

```bash
export BURSTY_HOST="https://<your-domain>.ngrok.app"
curl -fsS -H "Authorization: Bearer $BURSTY_TOKEN" "$BURSTY_HOST/v1/models"
```

`x-api-key: $BURSTY_TOKEN` is also accepted for Anthropic-family clients.

## Other Burst Targets

TrustedRouter is the default burst target, but `-tr-base-url` can point at any bearer-keyed OpenAI-compatible `/v1` base URL. Savings/pricing features use the TrustedRouter catalog.

```bash
export TRUSTEDROUTER_API_KEY="<upstream bearer token>"
burstyrouter -local-url http://127.0.0.1:11434 \
  -tr-api-key "$TRUSTEDROUTER_API_KEY" \
  -tr-base-url "https://openrouter.ai/api/v1"
```

If that upstream lacks `/v1/messages` or `/v1/responses`, BurstyRouter returns a clean `501 endpoint_not_supported` envelope.

## Cloud Controls

Mention these only when the user asks about cost, safety, disabling cloud, or strict local-first operation.

```bash
# No automatic bursts; only explicit non-local provider requests can use cloud.
burstyrouter -local-url http://127.0.0.1:11434 -tr-api-key "$TRUSTEDROUTER_API_KEY" -cloud explicit

# Disable cloud entirely.
burstyrouter -local-url http://127.0.0.1:11434 -tr-api-key "$TRUSTEDROUTER_API_KEY" -cloud off

# Cap priced cloud spend per UTC day. Unpriced cloud usage counts $0 toward the cap.
burstyrouter -local-url http://127.0.0.1:11434 -tr-api-key "$TRUSTEDROUTER_API_KEY" -max-cloud-spend 1.00
```

## Harness Wiring

Ask which harness and emit only that path.

### Cursor

Tell the user:

```text
Settings -> Models -> OpenAI API override
Base URL: https://<host>/v1
API key: $BURSTY_TOKEN, or any string when BURSTY_TOKEN is unset
Models: local/llama3.2, anthropic/claude-haiku-4.5
```

With aliases, list alias ids such as:

```text
Models: gpt-4o, anthropic/claude-haiku-4.5
```

Verify:

```bash
curl -is "https://<host>/v1/chat/completions" \
  -H "Authorization: Bearer $BURSTY_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"model":"local/llama3.2","messages":[{"role":"user","content":"ping"}]}' \
  | awk 'BEGIN{found=0} /^X-Bursty-Route:/ {print; found=1} END{exit found?0:1}'
```

### Claude Code / Anthropic SDKs

BurstyRouter forwards `/v1/messages` to TrustedRouter. Local OpenAI servers do not serve `/v1/messages`.

```bash
export ANTHROPIC_BASE_URL="https://<host>"
export ANTHROPIC_API_KEY="${BURSTY_TOKEN:-any-string}"
```

These clients send `x-api-key`; BurstyRouter accepts it when `BURSTY_TOKEN` is set.

Verify:

```bash
curl -is "https://<host>/v1/messages" \
  -H "x-api-key: $BURSTY_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"model":"anthropic/claude-haiku-4.5","max_tokens":16,"messages":[{"role":"user","content":"ping"}]}' \
  | awk 'BEGIN{found=0} /^X-Bursty-Route:/ {print; found=1} END{exit found?0:1}'
```

### Aider / OpenAI SDKs / Codex CLI / OpenHands

```bash
export OPENAI_BASE_URL="https://<host>/v1"
export OPENAI_API_KEY="${BURSTY_TOKEN:-any-string}"
```

Older OpenAI-compatible tools may use:

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

### Generic OpenAI-Compatible

```text
Base URL: https://<host>/v1
API key: $BURSTY_TOKEN, or any string when BURSTY_TOKEN is unset
Model: local/<local-model>, an alias id such as gpt-4o, or a TrustedRouter model
```

Verify with `/v1/chat/completions` and assert `X-Bursty-Route` is present.

## Routing Cheatsheet

Pin local with the `local/` prefix:

```json
{"model":"local/llama3.2","messages":[{"role":"user","content":"ping"}]}
```

Pin local with `provider.only`:

```json
{"model":"llama3.2","provider":{"only":["local"]},"messages":[{"role":"user","content":"ping"}]}
```

Force TrustedRouter with a non-local provider:

```json
{"model":"anthropic/claude-haiku-4.5","provider":{"order":["anthropic"]},"messages":[{"role":"user","content":"ping"}]}
```

Alias cloud id to local model:

```bash
burstyrouter -local-url http://127.0.0.1:11434 \
  -alias gpt-4o=llama3.2 \
  -savings-reference gpt-4o
```

Bursting:

- `provider.only:["local"]` and `local/` are hard local pins and do not burst.
- Aliased requests burst with the original cloud-facing model id.
- Default local-first requests burst when local is full, local errors, or local returns a model-missing `404`, unless the model is an unmapped local-native id with no `/`.
- Use `-burst-fallback-model` to let unmapped local-native ids burst with a configured upstream model.
- `/v1/messages` and `/v1/responses` are TrustedRouter-only passthrough endpoints.

Read routing:

```bash
curl -fsS -H "Authorization: Bearer $BURSTY_TOKEN" "$BURSTY_HOST/stats"
```

Headers:

```http
X-Bursty-Route: local
X-Bursty-Reason: policy
```

Reasons: `policy`, `forced`, `burst-full`, `burst-error`.
