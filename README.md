# BurstyRouter

BurstyRouter is a local-first OpenAI-compatible proxy: send requests to your local rig first, burst to TrustedRouter when local is full, failing, or missing the model, and never lose a request just because the machine under your desk is busy.

```text
brew tap Lore-Hex/homebrew-tap && brew install burstyrouter
export TRUSTEDROUTER_API_KEY="tr_..." # optional: enables cloud passthrough/bursting
burstyrouter
Point your tools at http://localhost:8383/v1
```

Alternates: `go install github.com/Lore-Hex/BurstyRouter/cmd/burstyrouter@latest`, download a binary from the [latest release](https://github.com/Lore-Hex/BurstyRouter/releases/latest), or run the Docker image you build from this repo.

## Routing Contract

| Request directive or condition | Behavior |
| --- | --- |
| No directive | Local first when `-local-url` is configured; TrustedRouter when local is absent. |
| `-alias gpt-4o=llama3.2` and request `model: "gpt-4o"` | Local first; forwards to local as `llama3.2`, but any burst uses the original `gpt-4o` id. |
| `model: "local/<name>"` | Forced local; forwards to local as `<name>`. |
| `provider.only: ["local"]` | Forced local; strips `provider` before local forwarding. |
| `provider.order: ["local"]` | Local preference, not a hard pin; can still burst when the model is burst-capable. |
| Any non-local provider in `provider.only` or `provider.order` | Forced TrustedRouter. |
| Local-native id with no `/`, no alias, and no fallback model | Effectively local-only; local full returns `429`, and local errors surface without a doomed burst. |
| Local-native id with `-burst-fallback-model` set | Can burst; BurstyRouter substitutes the fallback model only in the burst body. |
| Local semaphore full | Bursts to TrustedRouter when not forced, TR is configured, and the model is burst-capable; otherwise returns `429`. |
| Local connect error, `429`, `5xx`, or model-missing `404` | Bursts to TrustedRouter when `-burst-on-error=true`, not forced, TR is configured, and the model is burst-capable. |
| `/v1/messages` or `/v1/responses` | TrustedRouter-only raw passthrough; local-forced requests return `400`, local-only mode returns `501`, and upstream `404` maps to a Bursty `501`. |

## Configuration

| Flag | Env | Default |
| --- | --- | --- |
| `-listen` | `BURSTY_LISTEN` | `:8383` |
| `-local-url` | `BURSTY_LOCAL_URL` | `""` |
| `-tr-api-key` | `TRUSTEDROUTER_API_KEY` | `""` |
| `-tr-base-url` | `BURSTY_TR_BASE_URL` | `https://api.quillrouter.com/v1` |
| `-tr-catalog-url` | `BURSTY_TR_CATALOG_URL` | `https://trustedrouter.com/v1` |
| `-local-max-concurrency` | `BURSTY_LOCAL_MAX_CONCURRENCY` | `4` |
| `-local-queue-wait` | `BURSTY_LOCAL_QUEUE_WAIT` | `0s` |
| `-burst-on-error` | `BURSTY_BURST_ON_ERROR` | `true` |
| `-burst-fallback-model` | `BURSTY_BURST_FALLBACK_MODEL` | `""` |
| `-alias from=to` | `BURSTY_ALIASES=a=b,c=d` | `""` |
| `-savings-reference` | `BURSTY_SAVINGS_REFERENCE` | `""` |
| `-state-file` | `BURSTY_STATE_FILE` | `$XDG_STATE_HOME/bursty/state.json` or `~/.bursty/state.json`; `""` disables |
| `-cloud` | `BURSTY_CLOUD` | `auto` |
| `-max-cloud-spend` | `BURSTY_MAX_CLOUD_SPEND` | `0` |
| `-no-autodetect` | none | `false` |
| `-version` | none | `false` |
| `-token` | `BURSTY_TOKEN` | `""` |

When `-local-url` is unset, BurstyRouter probes `OLLAMA_HOST`, Ollama, LM Studio, llama.cpp, and vLLM on common localhost ports. If no local server is found, `TRUSTEDROUTER_API_KEY` enables pure cloud passthrough; without either, startup prints an actionable error. Use `-no-autodetect` to disable local probing. Set `BURSTY_TOKEN` whenever the proxy is reachable outside localhost. Auth accepts either `Authorization: Bearer <token>` or `x-api-key: <token>`.

Aliases map cloud-facing ids to local model ids. For example, `-alias gpt-4o=qwen2.5-coder:32b` lets tools request `gpt-4o`; local receives `qwen2.5-coder:32b`, while bursts still send `gpt-4o`.

## Savings

BurstyRouter keeps an honest savings meter in `/stats` and `X-Bursty-Saved-USD`. Local tokens are priced only as a labeled counterfactual using TrustedRouter catalog prices. The reference is chosen in order: the alias key for aliased requests, the requested TrustedRouter-known model, `-savings-reference`, then tokens-only with no dollars. BurstyRouter never invents a price when the catalog has no price anchor.

For local model names that are not cloud ids, pair the local alias with an explicit savings reference:

```bash
burstyrouter -local-url http://127.0.0.1:11434 \
  -tr-api-key "$TRUSTEDROUTER_API_KEY" \
  -alias gpt-4o=llama3.2 \
  -savings-reference gpt-4o
```

Cloud spend is priced from the actual model id returned by the cloud response when that model exists in the TrustedRouter catalog. Unpriced cloud usage still counts tokens in stats but counts `$0` toward the spend cap.

## Cloud Controls

`-cloud=auto|explicit|off` controls cloud egress. `auto` preserves normal bursting. `explicit` disables automatic bursts, so local-full requests return `429` and local errors surface, while requests that explicitly name a non-local provider can still go out. `off` disables the cloud upstream entirely; explicit cloud requests fail closed with `cloud disabled by -cloud=off`.

Send `SIGHUP` to toggle runtime cloud egress between the configured mode and `off`. `/stats` reports the effective mode.

`-max-cloud-spend <usd>` sets a per-UTC-day cloud spend cap. Once priced cloud spend reaches the cap, all cloud sends return `429 cloud_budget_exhausted` with `Retry-After` set to seconds until UTC midnight. Unpriced cloud usage honestly counts as `$0` toward the cap.

## Bursting To Other Clouds

`-tr-base-url` can point at any bearer-keyed OpenAI-compatible `/v1` base URL, including OpenRouter, Together, Groq, or your own upstream. TrustedRouter is only the default. Savings/pricing features use the TrustedRouter catalog.

If that upstream does not implement `/v1/messages` or `/v1/responses`, BurstyRouter maps its `404` to a clean `501 endpoint_not_supported` Bursty error envelope.

## Endpoints

| Endpoint | Mode |
| --- | --- |
| `GET /healthz` | Local health metadata. |
| `GET /stats` | Bursty counters; bearer-protected when `BURSTY_TOKEN` is set. |
| `GET /v1/models` | Merged local and TrustedRouter model list. |
| `POST /v1/chat/completions` | Local-capable, burst-capable. |
| `POST /v1/embeddings` | Local-capable, burst-capable. |
| `POST /v1/messages` | TrustedRouter-only raw passthrough. |
| `POST /v1/responses` | TrustedRouter-only raw passthrough. |

## Responses

Non-streaming JSON responses get a top-level Bursty block:

```json
{
  "bursty": {
    "route": "local",
    "reason": "policy"
  }
}
```

Every routed response also includes:

```http
X-Bursty-Route: local
X-Bursty-Reason: policy
```

Routes are `local` or `trustedrouter`. Reasons are `policy`, `forced`, `burst-full`, or `burst-error`. Streaming responses pass through byte-for-byte and use headers only.

## Stats

`GET /stats` reports `in_flight_local`, `bursts_full`, `bursts_error`, `bursts_skipped_unmapped`, `forced_local`, `forced_tr`, `requests_total`, `cloud_mode`, `cloud_blocked_budget`, `cloud_blocked_mode`, `savings`, global `routes`, and `endpoint_routes` for `chat_completions`, `embeddings`, `messages`, and `responses`.

## Setup

Use [docs/SETUP.md](docs/SETUP.md) for a copy-paste setup reference. Run `scripts/smoke.sh` to verify a local install against Ollama. Agent harnesses can use [skills/bursty-setup/SKILL.md](skills/bursty-setup/SKILL.md) as an interactive setup skill.

## License

Elastic License 2.0. You may use, copy, modify, and redistribute BurstyRouter, but you may not offer it to third parties as a managed service.
