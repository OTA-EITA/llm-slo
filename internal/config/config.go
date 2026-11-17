// Package config loads and checks the JSON configuration.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/OTA-EITA/llm-slo/internal/usage"
)

type Config struct {
	Listen        string `json:"listen"`         // proxy, default ":8080"
	MetricsListen string `json:"metrics_listen"` // /metrics, default ":9464"
	// AppHeader names the request header that says which app is calling.
	// It is removed before the request goes upstream.
	AppHeader  string `json:"app_header"`
	DefaultApp string `json:"default_app"`
	// Apps, if set, is the only list of app names that become labels.
	Apps []string `json:"apps"`
	// MaxModels caps distinct model label values (default 50).
	MaxModels int `json:"max_models"`
	// MaxRequestBytes caps request bodies the proxy will read (default 32 MiB).
	MaxRequestBytes int64 `json:"max_request_bytes"`
	// LogRequests writes one JSON line per call to stderr: model, tokens,
	// latency, cost. Never prompts or completions.
	LogRequests bool `json:"log_requests"`

	Upstreams []Upstream       `json:"upstreams"`
	Prices    map[string]Price `json:"prices"`
	SLOs      []SLO            `json:"slos"`
	Budgets   []Budget         `json:"budgets"`
}

type Upstream struct {
	// Prefix is the path prefix clients call, e.g. "/azure". It is stripped
	// before forwarding.
	Prefix string `json:"prefix"`
	URL    string `json:"url"`
	// API is the wire format: openai, anthropic or gemini.
	API usage.API `json:"api"`
	// Provider is the gen_ai.provider.name label, e.g. azure.ai.openai.
	Provider string `json:"provider"`
	// InjectStreamUsage adds stream_options.include_usage=true to OpenAI
	// streaming requests that do not set it, so streams report tokens.
	InjectStreamUsage bool `json:"inject_stream_usage"`
}

// Price is in US dollars per one million tokens. Input is the price of
// uncached input. Leave CacheRead or CacheWrite at 0 to charge them as Input.
type Price struct {
	Input      float64 `json:"input_per_mtok"`
	Output     float64 `json:"output_per_mtok"`
	CacheRead  float64 `json:"cache_read_per_mtok"`
	CacheWrite float64 `json:"cache_write_per_mtok"`
}

// SLO kinds.
const (
	Availability = "availability"
	Latency      = "latency"
	FirstChunk   = "time_to_first_chunk"
)

type SLO struct {
	Name      string  `json:"name"`
	Kind      string  `json:"kind"`
	Objective float64 `json:"objective"` // e.g. 0.995
	// ThresholdSeconds is required for latency and time_to_first_chunk.
	ThresholdSeconds float64 `json:"threshold_seconds"`
	// Optional selectors. Empty means "all".
	App      string `json:"app"`
	Model    string `json:"model"`
	Provider string `json:"provider"`
}

type Budget struct {
	App       string  `json:"app"`
	USDPerDay float64 `json:"usd_per_day"`
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

func Parse(b []byte) (*Config, error) {
	var c Config
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields() // a typo in a field name should not silently do nothing
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	c.defaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) defaults() {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.MetricsListen == "" {
		c.MetricsListen = ":9464"
	}
	if c.AppHeader == "" {
		c.AppHeader = "X-LLM-App"
	}
	if c.DefaultApp == "" {
		c.DefaultApp = "unknown"
	}
	if c.MaxModels == 0 {
		c.MaxModels = 50
	}
	if c.MaxRequestBytes == 0 {
		c.MaxRequestBytes = 32 << 20
	}
}

var sloName = regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`)

func (c *Config) Validate() error {
	var errs []error
	bad := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }

	prefixes := map[string]bool{}
	for i, u := range c.Upstreams {
		if !strings.HasPrefix(u.Prefix, "/") || strings.HasSuffix(u.Prefix, "/") && u.Prefix != "/" {
			bad("upstreams[%d].prefix %q must start with / and not end with /", i, u.Prefix)
		}
		if prefixes[u.Prefix] {
			bad("upstreams[%d].prefix %q is used twice", i, u.Prefix)
		}
		prefixes[u.Prefix] = true
		pu, err := url.Parse(u.URL)
		if err != nil || (pu.Scheme != "http" && pu.Scheme != "https") || pu.Host == "" {
			bad("upstreams[%d].url %q must be an http(s) URL", i, u.URL)
		}
		if !u.API.Valid() {
			bad("upstreams[%d].api %q must be openai, anthropic or gemini", i, u.API)
		}
		if u.Provider == "" {
			bad("upstreams[%d].provider is required (gen_ai.provider.name, e.g. openai, azure.ai.openai, anthropic, gcp.vertex_ai)", i)
		}
		if u.InjectStreamUsage && u.API != usage.OpenAI {
			bad("upstreams[%d].inject_stream_usage only applies to api openai", i)
		}
	}
	for m, p := range c.Prices {
		if p.Input < 0 || p.Output < 0 || p.CacheRead < 0 || p.CacheWrite < 0 {
			bad("prices[%q] must not be negative", m)
		}
	}
	names := map[string]bool{}
	for i, s := range c.SLOs {
		if !sloName.MatchString(s.Name) {
			bad("slos[%d].name %q may only use letters, digits, _ . -", i, s.Name)
		}
		if names[s.Name] {
			bad("slos[%d].name %q is used twice", i, s.Name)
		}
		names[s.Name] = true
		if s.Objective <= 0 || s.Objective >= 1 {
			bad("slos[%d].objective must be between 0 and 1 (e.g. 0.995), got %v", i, s.Objective)
		}
		switch s.Kind {
		case Availability:
			if s.ThresholdSeconds != 0 {
				bad("slos[%d]: threshold_seconds does not apply to availability", i)
			}
		case Latency, FirstChunk:
			if s.ThresholdSeconds <= 0 {
				bad("slos[%d]: %s needs threshold_seconds", i, s.Kind)
			}
		default:
			bad("slos[%d].kind %q must be availability, latency or time_to_first_chunk", i, s.Kind)
		}
	}
	for i, b := range c.Budgets {
		if b.USDPerDay <= 0 {
			bad("budgets[%d].usd_per_day must be positive", i)
		}
	}
	return errors.Join(errs...)
}

// Thresholds returns the SLO thresholds of a kind, so the histograms can
// have a bucket boundary at each of them.
func (c *Config) Thresholds(kind string) []float64 {
	var out []float64
	for _, s := range c.SLOs {
		if s.Kind == kind {
			out = append(out, s.ThresholdSeconds)
		}
	}
	sort.Float64s(out)
	return out
}

// PriceFor finds the price for a model: an exact name first, then the
// longest configured name that the model starts with, so "gpt-x" also
// prices dated snapshots such as "gpt-x-2026-01-01".
func (c *Config) PriceFor(models ...string) (Price, bool) {
	for _, m := range models {
		if p, ok := c.Prices[m]; ok && m != "" {
			return p, true
		}
	}
	for _, m := range models {
		best := ""
		for name := range c.Prices {
			if name != "" && strings.HasPrefix(m, name) && len(name) > len(best) {
				best = name
			}
		}
		if best != "" {
			return c.Prices[best], true
		}
	}
	return Price{}, false
}

// Cost returns the US dollar cost of one call.
func (p Price) Cost(u usage.Usage) float64 {
	read, write := p.CacheRead, p.CacheWrite
	if read == 0 {
		read = p.Input
	}
	if write == 0 {
		write = p.Input
	}
	plain := u.Input - u.CacheRead - u.CacheWrite
	if plain < 0 {
		plain = 0
	}
	return (float64(plain)*p.Input + float64(u.CacheRead)*read + float64(u.CacheWrite)*write + float64(u.Output)*p.Output) / 1e6
}
