# BurstyRouter

BurstyRouter is a local-first OpenAI-compatible proxy: send requests to your local rig first, burst to TrustedRouter when local is full, failing, or missing the model, and never lose a request just because the machine under your desk is busy.

```text
go install github.com/Lore-Hex/BurstyRouter/cmd/burstyrouter@latest
burstyrouter -local-url http://127.0.0.1:11434 -tr-api-key "$TRUSTEDROUTER_API_KEY"
point your tool at http://localhost:8383/v1
done
```

## Routing Contract

| Request directive or condition | Behavior |
| --- | --- |
| No directive | Local first when `-local-url` is configured; TrustedRouter when local is absent. |
| `model: "local/<name>"` | Forced local; forwards to local as `<name>`. |
| `provider.only: ["local"]` | Forced local; strips `provider` before local forwarding. |
| `provider.order: ["local"]` | Local preference, not a hard pin; can still burst. |
| Any non-local provider in `provider.only` or `provider.order` | Forced TrustedRouter. |
| Local semaphore full | Bursts to TrustedRouter when not forced and TR is configured; otherwise returns `429`. |
| Local connect error, `429`, `5xx`, or model-missing `404` | Bursts to TrustedRouter when `-burst-on-error=true`, not forced, and TR is configured. |
| `/v1/messages` or `/v1/responses` | TrustedRouter-only raw passthrough; local-forced requests return `400`, and local-only mode returns `501`. |

## Configuration

| Flag | Env | Default |
| --- | --- | --- |
| `-listen` | `BURSTY_LISTEN` | `:8383` |
| `-local-url` | `BURSTY_LOCAL_URL` | `""` |
| `-tr-api-key` | `TRUSTEDROUTER_API_KEY` | `""` |
| `-tr-base-url` | `BURSTY_TR_BASE_URL` | `https://api.quillrouter.com/v1` |
| `-local-max-concurrency` | `BURSTY_LOCAL_MAX_CONCURRENCY` | `4` |
| `-local-queue-wait` | `BURSTY_LOCAL_QUEUE_WAIT` | `0s` |
| `-burst-on-error` | `BURSTY_BURST_ON_ERROR` | `true` |
| `-token` | `BURSTY_TOKEN` | `""` |

At least one of `-local-url` or `-tr-api-key` is required. Set `BURSTY_TOKEN` whenever the proxy is reachable outside localhost.

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

`GET /stats` reports `in_flight_local`, `bursts_full`, `bursts_error`, `forced_local`, `forced_tr`, `requests_total`, global `routes`, and `endpoint_routes` for `chat_completions`, `embeddings`, `messages`, and `responses`.

## Setup

Use [docs/SETUP.md](docs/SETUP.md) for a copy-paste setup reference. Agent harnesses can use [skills/bursty-setup/SKILL.md](skills/bursty-setup/SKILL.md) as an interactive setup skill.

## License

Elastic License 2.0. You may use, copy, modify, and redistribute BurstyRouter, but you may not offer it to third parties as a managed service.
