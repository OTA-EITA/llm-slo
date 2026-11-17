package usage

import (
	"bytes"
	"encoding/json"
	"strings"
)

// maxLine bounds one SSE line we are willing to hold while waiting for its end.
// Longer lines are skipped (and only cost us their usage, not the response).
const maxLine = 4 << 20

// Stream reads a Server-Sent Events body as it passes through. Feed it bytes
// with Write; it reports whether the first output chunk has just arrived.
type Stream struct {
	api        API
	buf        []byte
	skipping   bool
	sawChunk   bool
	usage      Usage
	responseMd string
}

func NewStream(api API) *Stream { return &Stream{api: api} }

// Write consumes p and returns true exactly once: when the first event that
// carries new, non-empty output (text, tool call, refusal, reasoning) is
// complete. Events that only carry roles, usage or end markers do not count,
// following gen_ai.client.inference.time_to_first_chunk.
func (s *Stream) Write(p []byte) (first bool) {
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			if !s.skipping {
				s.buf = append(s.buf, p...)
				if len(s.buf) > maxLine {
					s.buf, s.skipping = s.buf[:0], true
				}
			}
			return first
		}
		if s.skipping {
			s.skipping = false
		} else {
			line := append(s.buf, p[:i]...)
			if s.line(bytes.TrimRight(line, "\r")) && !s.sawChunk {
				s.sawChunk, first = true, true
			}
		}
		s.buf = s.buf[:0]
		p = p[i+1:]
	}
	return first
}

// Usage returns what the stream reported so far. Call it after the body ends.
func (s *Stream) Usage() Usage {
	if len(s.buf) > 0 && !s.skipping { // body ended without a trailing newline
		s.line(s.buf)
		s.buf = s.buf[:0]
	}
	u := s.usage
	if u.ResponseModel == "" {
		u.ResponseModel = s.responseMd
	}
	return u
}

// line handles one SSE line and reports whether it carried output.
func (s *Stream) line(l []byte) bool {
	data, ok := bytes.CutPrefix(l, []byte("data:"))
	if !ok {
		return false
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		return false // "[DONE]" and friends
	}
	switch s.api {
	case OpenAI:
		return s.openai(data)
	case Anthropic:
		return s.anthropic(data)
	case Gemini:
		return s.gemini(data)
	}
	return false
}

type openaiChunk struct {
	Type    string          `json:"type"` // Responses API events
	Model   string          `json:"model"`
	Delta   json.RawMessage `json:"delta"`
	Usage   *openaiUsage    `json:"usage"`
	Choices []struct {
		Delta struct {
			Content          string          `json:"content"`
			Refusal          string          `json:"refusal"`
			ReasoningContent string          `json:"reasoning_content"`
			Reasoning        string          `json:"reasoning"`
			ToolCalls        json.RawMessage `json:"tool_calls"`
			FunctionCall     json.RawMessage `json:"function_call"`
		} `json:"delta"`
		Text string `json:"text"` // legacy completions
	} `json:"choices"`
	Response *struct {
		Model string       `json:"model"`
		Usage *openaiUsage `json:"usage"`
	} `json:"response"`
}

func (s *Stream) openai(data []byte) bool {
	var c openaiChunk
	if json.Unmarshal(data, &c) != nil {
		return false
	}
	if c.Model != "" {
		s.responseMd = c.Model
	}
	if c.Usage != nil {
		if u := c.Usage.normalise(); u.Known {
			s.usage = u
		}
	}
	if c.Response != nil { // Responses API: response.created / response.completed
		if c.Response.Model != "" {
			s.responseMd = c.Response.Model
		}
		if u := c.Response.Usage.normalise(); u.Known {
			s.usage = u
		}
	}
	if c.Type != "" {
		// Responses API: response.output_text.delta, response.refusal.delta,
		// response.function_call_arguments.delta, response.reasoning_*.delta ...
		return strings.HasSuffix(c.Type, ".delta") && nonEmpty(c.Delta)
	}
	for _, ch := range c.Choices {
		d := ch.Delta
		if d.Content != "" || d.Refusal != "" || d.ReasoningContent != "" || d.Reasoning != "" ||
			nonEmpty(d.ToolCalls) || nonEmpty(d.FunctionCall) || ch.Text != "" {
			return true
		}
	}
	return false
}

type anthropicEvent struct {
	Type    string            `json:"type"`
	Message *anthropicMessage `json:"message"`
	Usage   *anthropicUsage   `json:"usage"`
	Delta   *struct {
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		Thinking    string `json:"thinking"`
	} `json:"delta"`
	ContentBlock *struct {
		Type string `json:"type"`
		Name string `json:"name"`
		Text string `json:"text"`
	} `json:"content_block"`
}

func (s *Stream) anthropic(data []byte) bool {
	var e anthropicEvent
	if json.Unmarshal(data, &e) != nil {
		return false
	}
	switch e.Type {
	case "message_start":
		if e.Message != nil {
			s.responseMd = e.Message.Model
			if u := e.Message.Usage.normalise(); u.Known {
				s.usage = u
			}
		}
	case "message_delta":
		// Counts in message_delta are cumulative. Output always comes here;
		// input and cache counts may too, and then they replace the start values.
		if e.Usage != nil {
			if e.Usage.OutputTokens != nil {
				s.usage.Output, s.usage.Known = *e.Usage.OutputTokens, true
			}
			if e.Usage.InputTokens != nil {
				merged := e.Usage.normalise()
				s.usage.Input, s.usage.CacheRead, s.usage.CacheWrite = merged.Input, merged.CacheRead, merged.CacheWrite
			}
		}
	case "content_block_delta":
		return e.Delta != nil && (e.Delta.Text != "" || e.Delta.PartialJSON != "" || e.Delta.Thinking != "")
	case "content_block_start":
		// a tool_use block start carries the tool name: that is output
		return e.ContentBlock != nil && (e.ContentBlock.Name != "" || e.ContentBlock.Text != "")
	}
	return false
}

func (s *Stream) gemini(data []byte) bool {
	var r geminiResponse
	if json.Unmarshal(data, &r) != nil {
		return false
	}
	if r.ModelVersion != "" {
		s.responseMd = r.ModelVersion
	}
	if r.UsageMetadata != nil { // cumulative; the last one is the total
		s.usage = r.UsageMetadata.normalise()
	}
	for _, c := range r.Candidates {
		for _, p := range c.Content.Parts {
			if p.Text != "" || nonEmpty(p.FunctionCall) || nonEmpty(p.InlineData) {
				return true
			}
		}
	}
	return false
}

func nonEmpty(raw json.RawMessage) bool {
	t := strings.TrimSpace(string(raw))
	return t != "" && t != "null" && t != `""` && t != "[]" && t != "{}"
}
