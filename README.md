# llm-slo

A small proxy that measures your LLM API calls the way you measure any other dependency: **latency, time to first chunk, errors, tokens and cost, per app**, exported to Prometheus. From a short config it also generates **SLO burn-rate alerts and daily cost budget alerts**.

```
your apps ──► llm-slo ──► Azure OpenAI / OpenAI / Anthropic / Vertex AI (Gemini) / any OpenAI-compatible server
                 │
                 └──► /metrics ──► Prometheus ──► Grafana, Alertmanager
```

- **No code changes in the apps.** Point the SDK's base URL at the proxy and add one header that names the app.
- **Requests and responses pass through unchanged** (streams are passed on as each byte arrives). The proxy only reads the model name and the usage numbers.
- **Prompts and completions are never stored or logged.**
- **One static binary, no dependencies** outside the Go standard library.
- **Metric names follow the OpenTelemetry GenAI semantic conventions**, so dashboards and alerts carry over if you later move to an OpenTelemetry-based tool.

It is meant for the stage where a team has started shipping LLM features and wants to know "is it up, is it fast, and what does it cost, per app" without bringing in a full LLM observability platform first.

## Quick start

```sh
go install github.com/OTA-EITA/llm-slo/cmd/llm-slo@latest

llm-slo check -config llm-slo.json      # validate
llm-slo serve -config llm-slo.json      # proxy on :8080, metrics on :9464/metrics
llm-slo rules -config llm-slo.json > llm-slo-rules.yml           # Prometheus rule file
llm-slo rules -config llm-slo.json -format k8s > prometheusrule.yml  # Prometheus Operator
```

A minimal config (a fuller one is in [`examples/llm-slo.json`](examples/llm-slo.json)):

```json
{
  "upstreams": [
    { "prefix": "/azure", "url": "https://YOUR-RESOURCE.openai.azure.com",
      "api": "openai", "provider": "azure.ai.openai", "inject_stream_usage": true }
  ],
  "prices": {
    "your-model": { "input_per_mtok": 1.0, "output_per_mtok": 4.0, "cache_read_per_mtok": 0.25 }
  },
  "slos": [
    { "name": "support-bot-availability", "kind": "availability", "objective": 0.995, "app": "support-bot" },
    { "name": "support-bot-first-chunk", "kind": "time_to_first_chunk", "objective": 0.95, "threshold_seconds": 2, "app": "support-bot" }
  ],
  "budgets": [ { "app": "support-bot", "usd_per_day": 30 } ]
}
```

Then point the apps at the proxy. With the OpenAI Python SDK and Azure OpenAI:

```python
from openai import AzureOpenAI

client = AzureOpenAI(
    azure_endpoint="http://llm-slo:8080/azure",
    api_key=os.environ["AZURE_OPENAI_API_KEY"],   # passed through untouched
    api_version="2024-10-21",
    default_headers={"X-LLM-App": "support-bot"},
)
```

The same idea works for the others: `base_url="http://llm-slo:8080/openai/v1"` for OpenAI, `base_url="http://llm-slo:8080/anthropic"` for the Anthropic SDK, and the Vertex AI endpoint for Gemini. Authentication headers are forwarded as they are. The `X-LLM-App` header is removed before the request leaves the proxy.

## What it records

One call through the proxy, with a stream (taken from a run against a local fake upstream):

```
gen_ai_client_inference_duration_seconds_count{app="support-bot",gen_ai_provider_name="azure.ai.openai",gen_ai_request_model="example-model",gen_ai_operation_name="chat",error_type=""} 1
gen_ai_client_inference_time_to_first_chunk_seconds_sum{app="support-bot",...,error_type=""} 0.406381083
gen_ai_client_inference_usage_input_tokens_total{app="support-bot",...} 1200
gen_ai_client_inference_usage_cache_read_input_tokens_total{app="support-bot",...} 1024
gen_ai_client_inference_usage_output_tokens_total{app="support-bot",...} 2
llm_slo_cost_usd_total{app="support-bot",...} 0.00044
```

| Metric | Type | Meaning |
|---|---|---|
| `gen_ai_client_inference_duration_seconds` | histogram | Request arrival to the last byte of the response |
| `gen_ai_client_inference_time_to_first_chunk_seconds` | histogram | Streams only: time to the first event that carries output (text, tool call, refusal, reasoning). Role-only, usage-only and keep-alive events do not count |
| `gen_ai_client_inference_usage_input_tokens_total` | counter | Input tokens, **including** cached input |
| `gen_ai_client_inference_usage_output_tokens_total` | counter | Output tokens, **including** reasoning |
| `gen_ai_client_inference_usage_cache_read_input_tokens_total` | counter | Input served from the provider's prompt cache |
| `gen_ai_client_inference_usage_cache_write_input_tokens_total` | counter | Input written to the cache (Anthropic) |
| `gen_ai_client_inference_usage_reasoning_output_tokens_total` | counter | Output spent on reasoning |
| `llm_slo_cost_usd_total` | counter | Estimated cost from your `prices` |
| `llm_slo_usage_missing_total` | counter | Successful calls whose response had no usage (should be 0) |
| `llm_slo_price_missing_total` | counter | Calls with usage but no price for the model (should be 0) |

Labels: `app`, `gen_ai_provider_name`, `gen_ai_request_model`, `gen_ai_operation_name`, and `error_type` on the histograms.

Histogram buckets are the boundaries the conventions recommend (0.01 s to 81.92 s, doubling), plus every `threshold_seconds` from your SLOs, so each SLO threshold is an exact bucket.

### Normalising token counts

Providers count differently. The proxy reports what the OpenTelemetry conventions ask for: input includes cached tokens, output includes reasoning tokens.

| Provider | What the response says | What is recorded |
|---|---|---|
| OpenAI, Azure OpenAI | `prompt_tokens` already includes `cached_tokens`; `completion_tokens` includes `reasoning_tokens` | as is |
| Anthropic | `input_tokens` excludes cache reads and writes | input = `input_tokens` + `cache_read_input_tokens` + `cache_creation_input_tokens` |
| Gemini, Vertex AI | `promptTokenCount` includes cached content; `thoughtsTokenCount` is separate from `candidatesTokenCount` | output = candidates + thoughts; input = prompt + tool-use prompt |

Cost = uncached input × `input_per_mtok` + cache reads × `cache_read_per_mtok` + cache writes × `cache_write_per_mtok` + output × `output_per_mtok`, per million tokens. A cache price left at 0 is charged at the input price. Prices are matched by request model first, then response model, then the longest configured name the model starts with (so `"my-model"` also prices `my-model-2026-01-01`). **No prices ship with the tool.** Copy them from your provider's price list and treat the result as an estimate, not an invoice.

### Errors

`error_type` is empty for successful calls. Otherwise:

| `error_type` | Meaning | Counts against availability |
|---|---|---|
| `500`, `503`, … | Provider error status | yes |
| `429` | Throttled | yes |
| `408`, `timeout` | Timed out | yes |
| `incomplete` | The response broke off mid-body | yes |
| `_OTHER` | Could not reach the provider | yes |
| `400`, `401`, … | Client errors | no |
| `client_closed` | The caller hung up | no (and excluded from the total) |
| `request_too_large` | Over `max_request_bytes` | no |

### Streams without usage

OpenAI-compatible servers only send token usage on streams when the request sets `stream_options.include_usage`. With `"inject_stream_usage": true` the proxy adds it to streaming requests that do not set it (a client's own setting is left alone). The extra final chunk has an empty `choices` list; current OpenAI SDKs handle that, but check older client code that reads `choices[0]` on every chunk. Without it, such calls show up in `llm_slo_usage_missing_total`.

## SLO and budget alerts

`llm-slo rules` turns each SLO into recording rules (`llm_slo:error_ratio:rate5m` … `rate3d`, labelled `slo="<name>"`) and the multiwindow, multi-burn-rate alerts from the Google SRE Workbook (chapter "Alerting on SLOs"):

| Alert | Long and short window | Burn rate | Severity |
|---|---|---|---|
| `LLMSLOBurnRatePage` | 1h and 5m | 14.4× | page |
| `LLMSLOBurnRatePage` | 6h and 30m | 6× | page |
| `LLMSLOBurnRateTicket` | 1d and 2h | 3× | ticket |
| `LLMSLOBurnRateTicket` | 3d and 6h | 1× | ticket |

SLO kinds:

- `availability`: share of calls that failed (see the table above).
- `latency`: share of successful calls slower than `threshold_seconds`.
- `time_to_first_chunk`: share of successful streams whose first chunk took longer than `threshold_seconds`. For chat UIs this is usually the number users feel.

An SLO can be narrowed with `app`, `model` and `provider`.

Each budget becomes two alerts: `LLMDailyBudgetExceeded` (spend over the last 24 hours is above the budget) and `LLMDailyBudgetOnPace` (the last hour's spend, kept up for a day, would pass it; fires after 15 minutes).

The generated rules are checked in CI with `promtool check rules`, and `promtool test rules` feeds synthetic series through them to confirm which alerts fire (see [`internal/rules/rules_test.go`](internal/rules/rules_test.go)). Whole-number thresholds select both `le="5"` and `le="5.0"`, because Prometheus 3 rewrites bucket labels to the float form on ingestion.

## Grafana

[`dashboards/llm-slo.json`](dashboards/llm-slo.json) has calls per second, error ratio, latency and time-to-first-chunk percentiles, tokens per minute by model, cache hit ratio, spend per day at the current pace, calls missing usage or price, and the SLO error ratios.

## Configuration reference

| Field | Default | |
|---|---|---|
| `listen` | `:8080` | Proxy address |
| `metrics_listen` | `:9464` | Address for `/metrics` (keep it off the public network) |
| `app_header` | `X-LLM-App` | Header that names the calling app; removed before forwarding |
| `default_app` | `unknown` | App label when the header is missing |
| `apps` | (any) | If set, only these app names become labels; others become `_other` |
| `max_models` | `50` | Distinct model labels kept; later ones become `_other` |
| `max_request_bytes` | 32 MiB | Larger requests get 413 and are not forwarded |
| `log_requests` | `false` | One JSON line per call on stderr (model, tokens, latency, cost; never content), using `gen_ai.*` attribute names |
| `upstreams[]` | | `prefix`, `url`, `api` (`openai`, `anthropic`, `gemini`), `provider` (the `gen_ai.provider.name` value, e.g. `openai`, `azure.ai.openai`, `anthropic`, `gcp.vertex_ai`, `gcp.gemini`), `inject_stream_usage` |
| `prices` | | Model name to `input_per_mtok`, `output_per_mtok`, `cache_read_per_mtok`, `cache_write_per_mtok` (USD per million tokens) |
| `slos[]` | | `name`, `kind`, `objective` (e.g. `0.995`), `threshold_seconds`, optional `app`, `model`, `provider` |
| `budgets[]` | | `app`, `usd_per_day` |

Unknown fields are rejected, so a typo does not silently turn a feature off. `llm-slo check` reports every problem at once.

`app` and `model` label values come from clients. Both are capped so one misbehaving caller cannot create unbounded series.

## Limits

- **AWS Bedrock with SigV4 signing is not supported.** A SigV4 signature covers the host name, and the proxy forwards to a different host than the client signed for. Bedrock models reached through an OpenAI-compatible gateway work like any other OpenAI-compatible upstream.
- Gemini `streamGenerateContent` is timed to the first chunk only with `alt=sse`. Without it the response is a JSON array; usage is still read, but time to first chunk is not recorded.
- Counters live in memory and start from zero when the process restarts. Prometheus `rate()` and `increase()` handle resets. With several replicas, aggregate with `sum`.
- The OpenTelemetry GenAI conventions are still at Development status, and names may change upstream. The names here follow the [semantic-conventions-genai](https://github.com/open-telemetry/semantic-conventions-genai) repository as of November 2026.
- Cost is an estimate from the configured prices. Discounts, batch pricing, provisioned throughput and taxes are not modelled.

## How it compares

LiteLLM, Langfuse, Helicone and OpenLIT do far more: routing and fallbacks, tracing of whole agent runs, prompt management, evaluations. llm-slo does one thing: it measures calls as an SRE would, with SLOs and error budgets, using only Prometheus. It fits teams that already run Prometheus and Alertmanager and want LLM calls alerted on like the rest of their dependencies.

## Development

```sh
go test -race ./...     # promtool tests run when promtool is on PATH
```

## License

MIT
