package proxy

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OTA-EITA/llm-slo/internal/config"
)

// upstream is a fake provider. It remembers the last request it saw.
type upstream struct {
	*httptest.Server
	lastPath   string
	lastHeader http.Header
	lastBody   []byte
}

func newUpstream(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) *upstream {
	u := &upstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.lastPath = r.URL.RequestURI()
		u.lastHeader = r.Header.Clone()
		u.lastBody, _ = io.ReadAll(r.Body)
		h(w, r)
	}))
	t.Cleanup(u.Close)
	return u
}

func newProxy(t *testing.T, cfgJSON string, logs *bytes.Buffer) (*Server, *httptest.Server) {
	t.Helper()
	c, err := config.Parse([]byte(cfgJSON))
	if err != nil {
		t.Fatal(err)
	}
	opts := []Option{}
	if logs != nil {
		opts = append(opts, WithLogger(log.New(logs, "", 0)))
	}
	s, err := New(c, "test", opts...)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return s, ts
}

func post(t *testing.T, url, app, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test")
	if app != "" {
		req.Header.Set("X-LLM-App", app)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func metricsText(s *Server) string {
	var b bytes.Buffer
	s.Registry.WriteText(&b)
	return b.String()
}

func wantLine(t *testing.T, text, line string) {
	t.Helper()
	if !strings.Contains(text, line+"\n") {
		t.Errorf("missing %q in:\n%s", line, text)
	}
}

func TestOpenAINonStream(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		// answer gzipped when asked, like real providers do
		w.Header().Set("Content-Type", "application/json")
		body := `{"model":"gpt-x-2026","choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":1000,"completion_tokens":200,"prompt_tokens_details":{"cached_tokens":600}}}`
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			io.WriteString(gz, body)
			gz.Close()
			return
		}
		io.WriteString(w, body)
	})
	var logs bytes.Buffer
	s, ts := newProxy(t, fmt.Sprintf(`{
		"log_requests": true,
		"upstreams":[{"prefix":"/openai","url":%q,"api":"openai","provider":"openai"}],
		"prices":{"gpt-x":{"input_per_mtok":2,"output_per_mtok":8,"cache_read_per_mtok":1}}}`, up.URL+"/base"), &logs)

	resp, body := post(t, ts.URL+"/openai/v1/chat/completions?api-version=1", "support-bot", `{"model":"gpt-x","messages":[{"role":"user","content":"secret prompt"}]}`)
	if resp.StatusCode != 200 || !strings.Contains(body, `"content":"hi"`) {
		t.Fatalf("response not passed through: %d %s", resp.StatusCode, body)
	}
	if up.lastPath != "/base/v1/chat/completions?api-version=1" {
		t.Errorf("upstream path %q", up.lastPath)
	}
	if up.lastHeader.Get("X-LLM-App") != "" {
		t.Error("app header leaked upstream")
	}
	if up.lastHeader.Get("Authorization") != "Bearer sk-test" {
		t.Error("auth header not passed through")
	}

	m := metricsText(s)
	l := `{app="support-bot",gen_ai_provider_name="openai",gen_ai_request_model="gpt-x",gen_ai_operation_name="chat"}`
	wantLine(t, m, `gen_ai_client_inference_usage_input_tokens_total`+l+` 1000`)
	wantLine(t, m, `gen_ai_client_inference_usage_output_tokens_total`+l+` 200`)
	wantLine(t, m, `gen_ai_client_inference_usage_cache_read_input_tokens_total`+l+` 600`)
	// 400*2 + 600*1 + 200*8 = 3000 per million = 0.003
	wantLine(t, m, `llm_slo_cost_usd_total`+l+` 0.003`)
	wantLine(t, m, `gen_ai_client_inference_duration_seconds_count{app="support-bot",gen_ai_provider_name="openai",gen_ai_request_model="gpt-x",gen_ai_operation_name="chat",error_type=""} 1`)
	if strings.Contains(m, "time_to_first_chunk_seconds_count{") {
		t.Error("first chunk recorded for a non-streaming call")
	}

	if strings.Contains(logs.String(), "secret prompt") || strings.Contains(logs.String(), `"hi"`) {
		t.Fatalf("log contains content: %s", logs.String())
	}
	var entry map[string]any
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatalf("log is not one JSON line: %v %s", err, logs.String())
	}
	if entry["gen_ai.usage.input_tokens"] != float64(1000) || entry["gen_ai.response.model"] != "gpt-x-2026" || entry["cost_usd"] != 0.003 {
		t.Errorf("log entry: %v", entry)
	}
}

func TestOpenAIStreamInjectsUsageAndMeasuresFirstChunk(t *testing.T) {
	const delay = 150 * time.Millisecond
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		io.WriteString(w, "data: {\"model\":\"gpt-x\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n")
		f.Flush()
		time.Sleep(delay)
		io.WriteString(w, "data: {\"model\":\"gpt-x\",\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n")
		f.Flush()
		time.Sleep(delay)
		io.WriteString(w, "data: {\"model\":\"gpt-x\",\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n")
	})
	s, ts := newProxy(t, fmt.Sprintf(`{
		"upstreams":[{"prefix":"/openai","url":%q,"api":"openai","provider":"openai","inject_stream_usage":true}],
		"slos":[{"name":"ttfc","kind":"time_to_first_chunk","objective":0.99,"threshold_seconds":0.1}]}`, up.URL), nil)

	resp, body := post(t, ts.URL+"/openai/v1/chat/completions", "", `{"model":"gpt-x","stream":true}`)
	if resp.StatusCode != 200 || !strings.Contains(body, "[DONE]") {
		t.Fatalf("stream not passed through: %s", body)
	}
	if !strings.Contains(string(up.lastBody), `"include_usage":true`) {
		t.Errorf("include_usage not injected: %s", up.lastBody)
	}
	m := metricsText(s)
	l := `app="unknown",gen_ai_provider_name="openai",gen_ai_request_model="gpt-x",gen_ai_operation_name="chat",error_type=""`
	// the role-only event arrived at once; the first real chunk after ~delay
	wantLine(t, m, `gen_ai_client_inference_time_to_first_chunk_seconds_bucket{`+l+`,le="0.1"} 0`)
	wantLine(t, m, `gen_ai_client_inference_time_to_first_chunk_seconds_bucket{`+l+`,le="0.32"} 1`)
	wantLine(t, m, `gen_ai_client_inference_usage_output_tokens_total{app="unknown",gen_ai_provider_name="openai",gen_ai_request_model="gpt-x",gen_ai_operation_name="chat"} 1`)
	wantLine(t, m, `llm_slo_price_missing_total{app="unknown",gen_ai_provider_name="openai",gen_ai_request_model="gpt-x",gen_ai_operation_name="chat"} 1`)
}

func TestStreamWithoutUsageIsCounted(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\ndata: [DONE]\n\n")
	})
	s, ts := newProxy(t, fmt.Sprintf(`{"upstreams":[{"prefix":"/o","url":%q,"api":"openai","provider":"openai"}]}`, up.URL), nil)
	post(t, ts.URL+"/o/v1/chat/completions", "a", `{"model":"m","stream":true}`)
	if strings.Contains(string(up.lastBody), "include_usage") {
		t.Error("injected although inject_stream_usage is off")
	}
	wantLine(t, metricsText(s), `llm_slo_usage_missing_total{app="a",gen_ai_provider_name="openai",gen_ai_request_model="m",gen_ai_operation_name="chat"} 1`)
}

func TestHTTPErrorsAreLabelled(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"message":"slow down"}}`)
	})
	s, ts := newProxy(t, fmt.Sprintf(`{"upstreams":[{"prefix":"/a","url":%q,"api":"anthropic","provider":"anthropic"}]}`, up.URL), nil)
	resp, body := post(t, ts.URL+"/a/v1/messages", "bot", `{"model":"sonnet-x","messages":[]}`)
	if resp.StatusCode != 429 || !strings.Contains(body, "slow down") {
		t.Fatalf("error not passed through: %d %s", resp.StatusCode, body)
	}
	m := metricsText(s)
	wantLine(t, m, `gen_ai_client_inference_duration_seconds_count{app="bot",gen_ai_provider_name="anthropic",gen_ai_request_model="sonnet-x",gen_ai_operation_name="chat",error_type="429"} 1`)
	if strings.Contains(m, "llm_slo_usage_missing_total{") {
		t.Error("an error response was counted as missing usage")
	}
}

func TestUpstreamDown(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {})
	url := up.URL
	up.Close()
	s, ts := newProxy(t, fmt.Sprintf(`{"upstreams":[{"prefix":"/g","url":%q,"api":"gemini","provider":"gcp.vertex_ai"}]}`, url), nil)
	resp, _ := post(t, ts.URL+"/g/v1/projects/p/locations/l/publishers/google/models/gemini-y:generateContent", "", `{}`)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d", resp.StatusCode)
	}
	wantLine(t, metricsText(s), `gen_ai_client_inference_duration_seconds_count{app="unknown",gen_ai_provider_name="gcp.vertex_ai",gen_ai_request_model="gemini-y",gen_ai_operation_name="generate_content",error_type="_OTHER"} 1`)
}

func TestClientGoesAwayMidStream(t *testing.T) {
	release := make(chan struct{})
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"Hi\"}}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	defer close(release)
	s, ts := newProxy(t, fmt.Sprintf(`{"upstreams":[{"prefix":"/a","url":%q,"api":"anthropic","provider":"anthropic"}]}`, up.URL), nil)

	req, _ := http.NewRequest("POST", ts.URL+"/a/v1/messages", strings.NewReader(`{"model":"sonnet-x","stream":true}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	resp.Body.Read(buf)
	resp.Body.Close() // hang up after the first bytes

	deadline := time.Now().Add(3 * time.Second)
	want := `gen_ai_client_inference_duration_seconds_count{app="unknown",gen_ai_provider_name="anthropic",gen_ai_request_model="sonnet-x",gen_ai_operation_name="chat",error_type="client_closed"} 1`
	for time.Now().Before(deadline) {
		if strings.Contains(metricsText(s), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("missing %q in:\n%s", want, metricsText(s))
}

func TestModelCardinalityIsCapped(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{}`) })
	s, ts := newProxy(t, fmt.Sprintf(`{"max_models":2,"apps":["bot"],"upstreams":[{"prefix":"/o","url":%q,"api":"openai","provider":"openai"}]}`, up.URL), nil)
	for i := 0; i < 5; i++ {
		post(t, ts.URL+"/o/v1/chat/completions", fmt.Sprintf("app-%d", i), fmt.Sprintf(`{"model":"m%d"}`, i))
	}
	m := metricsText(s)
	if strings.Contains(m, `gen_ai_request_model="m2"`) || !strings.Contains(m, `gen_ai_request_model="_other"`) {
		t.Errorf("model cap not applied:\n%s", m)
	}
	if strings.Contains(m, `app="app-1"`) || !strings.Contains(m, `app="_other"`) {
		t.Errorf("app allow list not applied")
	}
}

func TestRequestTooLarge(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) { t.Error("large request was forwarded") })
	_, ts := newProxy(t, fmt.Sprintf(`{"max_request_bytes":10,"upstreams":[{"prefix":"/o","url":%q,"api":"openai","provider":"openai"}]}`, up.URL), nil)
	resp, _ := post(t, ts.URL+"/o/v1/chat/completions", "", `{"model":"m","messages":[]}`)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestNoRoute(t *testing.T) {
	_, ts := newProxy(t, `{"upstreams":[{"prefix":"/o","url":"http://127.0.0.1:1","api":"openai","provider":"openai"}]}`, nil)
	resp, _ := post(t, ts.URL+"/other/v1/chat/completions", "", `{}`)
	if resp.StatusCode != 404 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	resp, _ = post(t, ts.URL+"/openai/v1", "", `{}`) // "/o" must not match "/openai"
	if resp.StatusCode != 404 {
		t.Fatalf("prefix matched a longer segment: %d", resp.StatusCode)
	}
}

func TestUpstreamBreaksOffMidBody(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		conn, buf, _ := w.(http.Hijacker).Hijack()
		buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 1000\r\n\r\n{\"model\":")
		buf.Flush()
		conn.Close()
	})
	s, ts := newProxy(t, fmt.Sprintf(`{"upstreams":[{"prefix":"/o","url":%q,"api":"openai","provider":"openai"}]}`, up.URL), nil)
	req, _ := http.NewRequest("POST", ts.URL+"/o/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	if resp, err := http.DefaultClient.Do(req); err == nil {
		io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	wantLine(t, metricsText(s), `gen_ai_client_inference_duration_seconds_count{app="unknown",gen_ai_provider_name="openai",gen_ai_request_model="m",gen_ai_operation_name="chat",error_type="incomplete"} 1`)
}
