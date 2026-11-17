package config

import (
	"math"
	"strings"
	"testing"

	"github.com/OTA-EITA/llm-slo/internal/usage"
)

const good = `{
  "upstreams": [
    {"prefix": "/azure", "url": "https://example.openai.azure.com", "api": "openai", "provider": "azure.ai.openai", "inject_stream_usage": true},
    {"prefix": "/anthropic", "url": "https://api.anthropic.com", "api": "anthropic", "provider": "anthropic"}
  ],
  "prices": {"model-a": {"input_per_mtok": 2, "output_per_mtok": 8, "cache_read_per_mtok": 0.5}},
  "slos": [
    {"name": "bot-availability", "kind": "availability", "objective": 0.995, "app": "bot"},
    {"name": "bot-latency", "kind": "latency", "objective": 0.95, "threshold_seconds": 8, "app": "bot"},
    {"name": "bot-ttfc", "kind": "time_to_first_chunk", "objective": 0.99, "threshold_seconds": 1.5}
  ],
  "budgets": [{"app": "bot", "usd_per_day": 20}]
}`

func TestParseDefaults(t *testing.T) {
	c, err := Parse([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8080" || c.MetricsListen != ":9464" || c.AppHeader != "X-LLM-App" || c.DefaultApp != "unknown" || c.MaxModels != 50 {
		t.Fatalf("defaults not applied: %+v", c)
	}
	if got := c.Thresholds(Latency); len(got) != 1 || got[0] != 8 {
		t.Fatalf("thresholds: %v", got)
	}
}

func TestValidate(t *testing.T) {
	cases := map[string]string{
		"unknown field":   `{"upstream": []}`,
		"bad prefix":      `{"upstreams":[{"prefix":"azure","url":"https://x","api":"openai","provider":"p"}]}`,
		"trailing slash":  `{"upstreams":[{"prefix":"/azure/","url":"https://x","api":"openai","provider":"p"}]}`,
		"bad url":         `{"upstreams":[{"prefix":"/a","url":"ftp://x","api":"openai","provider":"p"}]}`,
		"bad api":         `{"upstreams":[{"prefix":"/a","url":"https://x","api":"bedrock","provider":"p"}]}`,
		"no provider":     `{"upstreams":[{"prefix":"/a","url":"https://x","api":"openai"}]}`,
		"inject on other": `{"upstreams":[{"prefix":"/a","url":"https://x","api":"gemini","provider":"p","inject_stream_usage":true}]}`,
		"objective 99.5":  `{"slos":[{"name":"s","kind":"availability","objective":99.5}]}`,
		"no threshold":    `{"slos":[{"name":"s","kind":"latency","objective":0.9}]}`,
		"bad kind":        `{"slos":[{"name":"s","kind":"errors","objective":0.9}]}`,
		"bad name":        `{"slos":[{"name":"a b","kind":"availability","objective":0.9}]}`,
		"negative price":  `{"prices":{"m":{"input_per_mtok":-1}}}`,
		"zero budget":     `{"budgets":[{"app":"a","usd_per_day":0}]}`,
	}
	for name, in := range cases {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: accepted %s", name, in)
		}
	}
}

func TestPriceFor(t *testing.T) {
	c := &Config{Prices: map[string]Price{"gpt-x": {Input: 1}, "gpt-x-mini": {Input: 2}}}
	if p, ok := c.PriceFor("gpt-x-mini-2026-01-01"); !ok || p.Input != 2 {
		t.Fatalf("longest prefix not chosen: %+v %v", p, ok)
	}
	if p, ok := c.PriceFor("deployment-name", "gpt-x-2026"); !ok || p.Input != 1 {
		t.Fatalf("fallback to response model failed: %+v %v", p, ok)
	}
	if _, ok := c.PriceFor("other"); ok {
		t.Fatal("priced an unknown model")
	}
}

func TestCost(t *testing.T) {
	p := Price{Input: 2, Output: 8, CacheRead: 0.5}
	// 1,000,000 input of which 400,000 cached, 100,000 output:
	// 600,000*2 + 400,000*0.5 + 100,000*8 = 1.2 + 0.2 + 0.8 = 2.2 USD
	got := p.Cost(usage.Usage{Input: 1_000_000, CacheRead: 400_000, Output: 100_000})
	if math.Abs(got-2.2) > 1e-9 {
		t.Fatalf("cost %v, want 2.2", got)
	}
	// cache write charged as input when no price is set
	got = Price{Input: 3}.Cost(usage.Usage{Input: 1_000_000, CacheWrite: 1_000_000})
	if math.Abs(got-3) > 1e-9 {
		t.Fatalf("cost %v, want 3", got)
	}
}

func TestErrorsAreAllReported(t *testing.T) {
	_, err := Parse([]byte(`{"upstreams":[{"prefix":"a","url":"x","api":"y"}]}`))
	if err == nil || strings.Count(err.Error(), "\n") < 3 {
		t.Fatalf("expected several errors at once, got %v", err)
	}
}
