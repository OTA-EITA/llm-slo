package usage

import (
	"strings"
	"testing"
)

// feed writes the body line by line, each line cut into pieces of the given
// size the way it arrives from the network, and returns the 1-based line on
// which the first chunk was reported, or -1.
func feed(s *Stream, body string, piece int) int {
	first := -1
	for n, line := range strings.SplitAfter(body, "\n") {
		for len(line) > 0 {
			k := min(piece, len(line))
			if s.Write([]byte(line[:k])) {
				if first >= 0 {
					panic("first chunk reported twice")
				}
				first = n + 1
			}
			line = line[k:]
		}
	}
	return first
}

const openaiStream = `data: {"id":"1","model":"gpt-x-2026","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}

data: {"id":"1","model":"gpt-x-2026","choices":[{"index":0,"delta":{"content":"Hel"}}]}

data: {"id":"1","model":"gpt-x-2026","choices":[{"index":0,"delta":{"content":"lo"}}]}

data: {"id":"1","model":"gpt-x-2026","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: {"id":"1","model":"gpt-x-2026","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":0}}}

data: [DONE]

`

func TestOpenAIStream(t *testing.T) {
	for _, piece := range []int{1, 7, 64, 1 << 20} {
		s := NewStream(OpenAI)
		// the role-only event (line 1-2) must not count; the "Hel" event ends at line 3
		if got := feed(s, openaiStream, piece); got != 3 {
			t.Errorf("piece %d: first chunk at line %d, want 3", piece, got)
		}
		want := Usage{Input: 11, Output: 2, Known: true, ResponseModel: "gpt-x-2026"}
		if got := s.Usage(); got != want {
			t.Errorf("piece %d: usage %+v, want %+v", piece, got, want)
		}
	}
}

func TestOpenAIStreamWithoutUsage(t *testing.T) {
	s := NewStream(OpenAI)
	feed(s, strings.Replace(openaiStream, `"usage":{"prompt_tokens":11,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":0}}`, `"x":1`, 1), 16)
	if u := s.Usage(); u.Known {
		t.Fatalf("usage should be unknown, got %+v", u)
	}
}

func TestOpenAIToolCallIsAChunk(t *testing.T) {
	s := NewStream(OpenAI)
	body := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":null,\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"get\"}}]}}]}\n\n"
	if feed(s, body, 5) != 1 {
		t.Fatal("tool call delta not counted as output")
	}
}

const responsesStream = `event: response.created
data: {"type":"response.created","response":{"model":"r-1","usage":null}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"Hi"}

event: response.completed
data: {"type":"response.completed","response":{"model":"r-1","usage":{"input_tokens":9,"output_tokens":4,"output_tokens_details":{"reasoning_tokens":1}}}}

`

func TestResponsesStream(t *testing.T) {
	s := NewStream(OpenAI)
	if got := feed(s, responsesStream, 9); got != 5 {
		t.Errorf("first chunk at line %d, want 5", got)
	}
	want := Usage{Input: 9, Output: 4, Reasoning: 1, Known: true, ResponseModel: "r-1"}
	if got := s.Usage(); got != want {
		t.Errorf("usage %+v, want %+v", got, want)
	}
}

const anthropicStream = `event: message_start
data: {"type":"message_start","message":{"id":"m","type":"message","role":"assistant","content":[],"model":"sonnet-x","usage":{"input_tokens":25,"output_tokens":1,"cache_read_input_tokens":100,"cache_creation_input_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: ping
data: {"type": "ping"}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":15}}

event: message_stop
data: {"type":"message_stop"}
`

func TestAnthropicStream(t *testing.T) {
	s := NewStream(Anthropic)
	if got := feed(s, anthropicStream, 13); got != 11 {
		t.Errorf("first chunk at line %d, want 11", got)
	}
	want := Usage{Input: 125, Output: 15, CacheRead: 100, Known: true, ResponseModel: "sonnet-x"}
	if got := s.Usage(); got != want {
		t.Errorf("usage %+v, want %+v", got, want)
	}
}

func TestGeminiStream(t *testing.T) {
	body := "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"Hi\"}]}}],\"usageMetadata\":{\"promptTokenCount\":7,\"candidatesTokenCount\":1},\"modelVersion\":\"gemini-y-001\"}\r\n\r\n" +
		"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\" there\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":7,\"candidatesTokenCount\":3,\"thoughtsTokenCount\":2},\"modelVersion\":\"gemini-y-001\"}\r\n\r\n"
	s := NewStream(Gemini)
	if got := feed(s, body, 10); got != 1 {
		t.Errorf("first chunk at line %d, want 1", got)
	}
	want := Usage{Input: 7, Output: 5, Reasoning: 2, Known: true, ResponseModel: "gemini-y-001"}
	if got := s.Usage(); got != want {
		t.Errorf("usage %+v, want %+v", got, want)
	}
}

func TestHugeLineIsSkipped(t *testing.T) {
	s := NewStream(OpenAI)
	huge := "data: {\"choices\":[{\"delta\":{\"content\":\"" + strings.Repeat("x", maxLine+10) + "\"}}]}\n\n"
	tail := "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n"
	feed(s, huge+tail, 1<<16)
	if u := s.Usage(); !u.Known || u.Input != 1 {
		t.Fatalf("usage after a skipped line: %+v", u)
	}
	if len(s.buf) > maxLine {
		t.Fatalf("buffer kept growing: %d", len(s.buf))
	}
}

func TestNoTrailingNewline(t *testing.T) {
	s := NewStream(OpenAI)
	s.Write([]byte(`data: {"choices":[],"usage":{"prompt_tokens":2,"completion_tokens":3}}`))
	if u := s.Usage(); u.Input != 2 || u.Output != 3 {
		t.Fatalf("last line without newline lost: %+v", u)
	}
}

func TestWholeBodyInOneWrite(t *testing.T) {
	s := NewStream(OpenAI)
	if !s.Write([]byte(openaiStream)) {
		t.Fatal("first chunk not reported when the whole body arrives at once")
	}
	if s.Write([]byte(openaiStream)) {
		t.Fatal("first chunk reported twice")
	}
	if u := s.Usage(); u.Input != 11 || u.Output != 2 {
		t.Fatalf("usage %+v", u)
	}
}
