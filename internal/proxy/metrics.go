package proxy

import (
	"github.com/OTA-EITA/llm-slo/internal/config"
	"github.com/OTA-EITA/llm-slo/internal/metrics"
)

// DurationBuckets are the boundaries the OpenTelemetry GenAI conventions
// recommend for gen_ai.client.inference.duration and time_to_first_chunk.
var DurationBuckets = []float64{0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28, 2.56, 5.12, 10.24, 20.48, 40.96, 81.92}

// Label names. Metric and label names are the OpenTelemetry GenAI names
// converted to Prometheus style (dots become underscores).
var (
	callLabels  = []string{"app", "gen_ai_provider_name", "gen_ai_request_model", "gen_ai_operation_name", "error_type"}
	tokenLabels = []string{"app", "gen_ai_provider_name", "gen_ai_request_model", "gen_ai_operation_name"}
)

// Metric names, shared with the rules generator.
const (
	MetricDuration   = "gen_ai_client_inference_duration_seconds"
	MetricFirstChunk = "gen_ai_client_inference_time_to_first_chunk_seconds"
	MetricInput      = "gen_ai_client_inference_usage_input_tokens_total"
	MetricOutput     = "gen_ai_client_inference_usage_output_tokens_total"
	MetricCacheRead  = "gen_ai_client_inference_usage_cache_read_input_tokens_total"
	MetricCacheWrite = "gen_ai_client_inference_usage_cache_write_input_tokens_total"
	MetricReasoning  = "gen_ai_client_inference_usage_reasoning_output_tokens_total"
	MetricCost       = "llm_slo_cost_usd_total"
	MetricNoUsage    = "llm_slo_usage_missing_total"
	MetricNoPrice    = "llm_slo_price_missing_total"
)

// Values of the error_type label. HTTP errors use the status code ("500",
// "429"); successful calls have an empty error_type.
const (
	ErrTimeout    = "timeout"
	ErrClient     = "client_closed" // the caller went away; not the provider's fault
	ErrIncomplete = "incomplete"    // the upstream body broke off mid-response
	ErrOther      = "_OTHER"
	ErrTooLarge   = "request_too_large"
)

type instruments struct {
	duration, firstChunk                   *metrics.HistogramVec
	input, output, cacheRead, cacheWrite   *metrics.CounterVec
	reasoning, cost, usageMissing, noPrice *metrics.CounterVec
}

func newInstruments(r *metrics.Registry, c *config.Config, version string) *instruments {
	r.Info("llm_slo_build_info", "llm-slo build information.", []string{"version"}, []string{version})
	return &instruments{
		duration: r.Histogram(MetricDuration,
			"Time from the request reaching the proxy to the last byte of the response.",
			append(append([]float64(nil), DurationBuckets...), c.Thresholds(config.Latency)...), callLabels...),
		firstChunk: r.Histogram(MetricFirstChunk,
			"Time to the first streamed event that carries output (text, tool call, refusal or reasoning). Streaming calls only.",
			append(append([]float64(nil), DurationBuckets...), c.Thresholds(config.FirstChunk)...), callLabels...),
		input:        r.Counter(MetricInput, "Input tokens, including cached input.", tokenLabels...),
		output:       r.Counter(MetricOutput, "Output tokens, including reasoning.", tokenLabels...),
		cacheRead:    r.Counter(MetricCacheRead, "Input tokens served from the provider's cache.", tokenLabels...),
		cacheWrite:   r.Counter(MetricCacheWrite, "Input tokens written to the provider's cache.", tokenLabels...),
		reasoning:    r.Counter(MetricReasoning, "Output tokens spent on reasoning.", tokenLabels...),
		cost:         r.Counter(MetricCost, "Estimated cost in US dollars, from the configured prices.", tokenLabels...),
		usageMissing: r.Counter(MetricNoUsage, "Successful calls whose response carried no token usage.", tokenLabels...),
		noPrice:      r.Counter(MetricNoPrice, "Calls with token usage but no configured price for the model.", tokenLabels...),
	}
}
