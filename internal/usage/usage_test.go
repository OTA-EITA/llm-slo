package usage

import (
	"strings"
	"testing"
)

func TestParseRequest(t *testing.T) {
	cases := []struct {
		api  API
		path string
		body string
		want Request
	}{
		{OpenAI, "/v1/chat/completions", `{"model":"gpt-x","stream":true,"messages":[]}`, Request{"gpt-x", true, "chat"}},
		{OpenAI, "/openai/deployments/support-4o/chat/completions", `{"messages":[]}`, Request{"support-4o", false, "chat"}},
		{OpenAI, "/v1/embeddings", `{"model":"emb-1","input":"x"}`, Request{"emb-1", false, "embeddings"}},
		{OpenAI, "/v1/completions", `{"model":"old"}`, Request{"old", false, "text_completion"}},
		{OpenAI, "/v1/responses", `{"model":"r-1","stream":true}`, Request{"r-1", true, "chat"}},
		{Anthropic, "/v1/messages", `{"model":"sonnet-x","stream":true}`, Request{"sonnet-x", true, "chat"}},
		{Gemini, "/v1/projects/p/locations/us-central1/publishers/google/models/gemini-y:streamGenerateContent", ``, Request{"gemini-y", true, "generate_content"}},
		{Gemini, "/v1beta/models/gemini-y:generateContent", ``, Request{"gemini-y", false, "generate_content"}},
	}
	for _, c := range cases {
		if got := ParseRequest(c.api, c.path, []byte(c.body)); got != c.want {
			t.Errorf("%s %s: got %+v, want %+v", c.api, c.path, got, c.want)
		}
	}
}

func TestInjectStreamUsage(t *testing.T) {
	out, changed := InjectStreamUsage([]byte(`{"model":"m","stream":true}`))
	if !changed || !strings.Contains(string(out), `"stream_options":{"include_usage":true}`) {
		t.Fatalf("not injected: %s", out)
	}
	out, changed = InjectStreamUsage([]byte(`{"model":"m","stream":true,"stream_options":{"include_usage":false}}`))
	if changed || !strings.Contains(string(out), `"include_usage":false`) {
		t.Fatalf("client choice overwritten: %s", out)
	}
	out, changed = InjectStreamUsage([]byte(`{"stream_options":{"foo":1}}`))
	if !changed || !strings.Contains(string(out), `"include_usage":true`) || !strings.Contains(string(out), `"foo":1`) {
		t.Fatalf("other options lost: %s", out)
	}
	if _, changed = InjectStreamUsage([]byte(`not json`)); changed {
		t.Fatal("changed invalid body")
	}
}

func TestParseBody(t *testing.T) {
	cases := []struct {
		name string
		api  API
		body string
		want Usage
	}{
		{"openai chat", OpenAI,
			`{"model":"gpt-x-2026","usage":{"prompt_tokens":120,"completion_tokens":30,"prompt_tokens_details":{"cached_tokens":100},"completion_tokens_details":{"reasoning_tokens":10}}}`,
			Usage{Input: 120, Output: 30, CacheRead: 100, Reasoning: 10, Known: true, ResponseModel: "gpt-x-2026"}},
		{"openai responses", OpenAI,
			`{"model":"r-1","usage":{"input_tokens":50,"output_tokens":7,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":3}}}`,
			Usage{Input: 50, Output: 7, Reasoning: 3, Known: true, ResponseModel: "r-1"}},
		{"openai embeddings", OpenAI,
			`{"model":"emb-1","usage":{"prompt_tokens":8,"total_tokens":8}}`,
			Usage{Input: 8, Known: true, ResponseModel: "emb-1"}},
		{"openai no usage", OpenAI, `{"model":"m"}`, Usage{ResponseModel: "m"}},
		{"anthropic with cache", Anthropic,
			`{"model":"sonnet-x","usage":{"input_tokens":20,"output_tokens":5,"cache_read_input_tokens":1000,"cache_creation_input_tokens":200}}`,
			Usage{Input: 1220, Output: 5, CacheRead: 1000, CacheWrite: 200, Known: true, ResponseModel: "sonnet-x"}},
		{"gemini", Gemini,
			`{"modelVersion":"gemini-y-001","usageMetadata":{"promptTokenCount":300,"candidatesTokenCount":40,"cachedContentTokenCount":256,"thoughtsTokenCount":60,"toolUsePromptTokenCount":5}}`,
			Usage{Input: 305, Output: 100, CacheRead: 256, Reasoning: 60, Known: true, ResponseModel: "gemini-y-001"}},
		{"gemini json array stream", Gemini,
			`[{"candidates":[{"content":{"parts":[{"text":"a"}]}}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1}},
			  {"candidates":[{"content":{"parts":[{"text":"b"}]}}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2},"modelVersion":"g"}]`,
			Usage{Input: 3, Output: 2, Known: true, ResponseModel: "g"}},
		{"garbage", OpenAI, `<html>bad gateway</html>`, Usage{}},
	}
	for _, c := range cases {
		if got := ParseBody(c.api, []byte(c.body)); got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}
