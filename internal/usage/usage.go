// Package usage reads model names, token counts and the first output chunk
// from LLM API requests and responses. It never keeps prompt or completion
// text: payloads are decoded into structs that only have the fields we count.
package usage

import (
	"encoding/json"
	"strings"
)

// API is the wire format an upstream speaks.
type API string

const (
	OpenAI    API = "openai"    // OpenAI, Azure OpenAI, vLLM, LiteLLM and other OpenAI-compatible servers
	Anthropic API = "anthropic" // Anthropic Messages API
	Gemini    API = "gemini"    // Gemini API and Vertex AI generateContent
)

func (a API) Valid() bool { return a == OpenAI || a == Anthropic || a == Gemini }

// Usage is normalised to the OpenTelemetry GenAI conventions:
// Input includes cached tokens, Output includes reasoning tokens.
type Usage struct {
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
	Reasoning  int64
	// Known is false when the response carried no usage at all
	// (for example an OpenAI stream without stream_options.include_usage).
	Known         bool
	ResponseModel string
}

// Request is what we need to know about an outgoing call.
type Request struct {
	Model     string
	Stream    bool
	Operation string // gen_ai.operation.name
}

// ParseRequest reads the model, the stream flag and the operation from a
// request. path is the path sent upstream (after the route prefix).
func ParseRequest(api API, path string, body []byte) Request {
	var r Request
	switch api {
	case Gemini:
		// .../models/{model}:generateContent or :streamGenerateContent
		seg := path
		if i := strings.LastIndex(seg, "/models/"); i >= 0 {
			seg = seg[i+len("/models/"):]
		}
		model, action, _ := strings.Cut(seg, ":")
		r.Model = model
		r.Stream = action == "streamGenerateContent"
		switch {
		case strings.Contains(action, "mbed"):
			r.Operation = "embeddings"
		default:
			r.Operation = "generate_content"
		}
		return r
	}

	var b struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.Unmarshal(body, &b)
	r.Model, r.Stream = b.Model, b.Stream

	switch {
	case strings.Contains(path, "/embeddings"):
		r.Operation = "embeddings"
	case strings.HasSuffix(strings.TrimSuffix(path, "/"), "/completions") && !strings.Contains(path, "/chat/"):
		r.Operation = "text_completion"
	default:
		r.Operation = "chat"
	}
	if r.Model == "" && api == OpenAI {
		// Azure OpenAI: /openai/deployments/{deployment}/chat/completions
		if _, rest, ok := strings.Cut(path, "/deployments/"); ok {
			r.Model, _, _ = strings.Cut(rest, "/")
		}
	}
	return r
}

// InjectStreamUsage asks an OpenAI-compatible server to send a final usage
// chunk on streams. It leaves the body alone when the client already set it.
func InjectStreamUsage(body []byte) ([]byte, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body, false
	}
	var opts map[string]json.RawMessage
	if raw, ok := m["stream_options"]; ok && string(raw) != "null" {
		if json.Unmarshal(raw, &opts) != nil {
			return body, false
		}
		if _, set := opts["include_usage"]; set {
			return body, false
		}
	}
	if opts == nil {
		opts = map[string]json.RawMessage{}
	}
	opts["include_usage"] = json.RawMessage("true")
	raw, _ := json.Marshal(opts)
	m["stream_options"] = raw
	out, err := json.Marshal(m)
	if err != nil {
		return body, false
	}
	return out, true
}

// ParseBody reads usage from a whole (non-streamed) JSON response.
func ParseBody(api API, body []byte) Usage {
	switch api {
	case OpenAI:
		var b openaiBody
		if json.Unmarshal(body, &b) != nil {
			return Usage{}
		}
		u := b.Usage.normalise()
		u.ResponseModel = b.Model
		return u
	case Anthropic:
		var b anthropicMessage
		if json.Unmarshal(body, &b) != nil {
			return Usage{}
		}
		u := b.Usage.normalise()
		u.ResponseModel = b.Model
		return u
	case Gemini:
		// streamGenerateContent without alt=sse returns a JSON array
		trimmed := strings.TrimSpace(string(body))
		if strings.HasPrefix(trimmed, "[") {
			var arr []geminiResponse
			if json.Unmarshal(body, &arr) != nil || len(arr) == 0 {
				return Usage{}
			}
			var u Usage
			for _, r := range arr {
				if r.UsageMetadata != nil {
					u = r.UsageMetadata.normalise()
				}
				if r.ModelVersion != "" {
					u.ResponseModel = r.ModelVersion
				}
			}
			return u
		}
		var r geminiResponse
		if json.Unmarshal(body, &r) != nil {
			return Usage{}
		}
		var u Usage
		if r.UsageMetadata != nil {
			u = r.UsageMetadata.normalise()
		}
		u.ResponseModel = r.ModelVersion
		return u
	}
	return Usage{}
}

// ---- OpenAI ----

type openaiUsage struct {
	PromptTokens     *int64 `json:"prompt_tokens"`
	CompletionTokens *int64 `json:"completion_tokens"`
	InputTokens      *int64 `json:"input_tokens"`  // Responses API
	OutputTokens     *int64 `json:"output_tokens"` // Responses API
	PromptDetails    *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	InputDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	CompletionDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
	OutputDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

// prompt_tokens already includes cached tokens and completion_tokens
// already includes reasoning tokens, which is what the conventions want.
func (o *openaiUsage) normalise() Usage {
	if o == nil {
		return Usage{}
	}
	var u Usage
	if o.PromptTokens != nil {
		u.Input, u.Known = *o.PromptTokens, true
	}
	if o.InputTokens != nil {
		u.Input, u.Known = *o.InputTokens, true
	}
	if o.CompletionTokens != nil {
		u.Output, u.Known = *o.CompletionTokens, true
	}
	if o.OutputTokens != nil {
		u.Output, u.Known = *o.OutputTokens, true
	}
	if o.PromptDetails != nil {
		u.CacheRead = o.PromptDetails.CachedTokens
	}
	if o.InputDetails != nil {
		u.CacheRead = o.InputDetails.CachedTokens
	}
	if o.CompletionDetails != nil {
		u.Reasoning = o.CompletionDetails.ReasoningTokens
	}
	if o.OutputDetails != nil {
		u.Reasoning = o.OutputDetails.ReasoningTokens
	}
	return u
}

// openaiBody covers Chat Completions, legacy Completions, Embeddings and the
// Responses API, which all put model and usage at the top level.
type openaiBody struct {
	Model string       `json:"model"`
	Usage *openaiUsage `json:"usage"`
}

// ---- Anthropic ----

type anthropicUsage struct {
	InputTokens              *int64 `json:"input_tokens"`
	OutputTokens             *int64 `json:"output_tokens"`
	CacheReadInputTokens     int64  `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64  `json:"cache_creation_input_tokens"`
}

// Anthropic's input_tokens excludes cache reads and cache writes:
// total input = input_tokens + cache_read_input_tokens + cache_creation_input_tokens.
func (a *anthropicUsage) normalise() Usage {
	if a == nil || (a.InputTokens == nil && a.OutputTokens == nil) {
		return Usage{}
	}
	u := Usage{Known: true, CacheRead: a.CacheReadInputTokens, CacheWrite: a.CacheCreationInputTokens}
	if a.InputTokens != nil {
		u.Input = *a.InputTokens + a.CacheReadInputTokens + a.CacheCreationInputTokens
	}
	if a.OutputTokens != nil {
		u.Output = *a.OutputTokens
	}
	return u
}

type anthropicMessage struct {
	Model string          `json:"model"`
	Usage *anthropicUsage `json:"usage"`
}

// ---- Gemini / Vertex AI ----

type geminiUsage struct {
	PromptTokenCount        int64 `json:"promptTokenCount"`
	CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
	CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
	ThoughtsTokenCount      int64 `json:"thoughtsTokenCount"`
	ToolUsePromptTokenCount int64 `json:"toolUsePromptTokenCount"`
}

// promptTokenCount includes cached content. Thoughts are reported apart from
// candidates, so they are added to output (the conventions count reasoning
// as output). Tool-use prompt tokens are input the model read.
func (g *geminiUsage) normalise() Usage {
	return Usage{
		Known:     true,
		Input:     g.PromptTokenCount + g.ToolUsePromptTokenCount,
		Output:    g.CandidatesTokenCount + g.ThoughtsTokenCount,
		CacheRead: g.CachedContentTokenCount,
		Reasoning: g.ThoughtsTokenCount,
	}
}

type geminiResponse struct {
	ModelVersion  string       `json:"modelVersion"`
	UsageMetadata *geminiUsage `json:"usageMetadata"`
	Candidates    []struct {
		Content struct {
			Parts []struct {
				Text         string          `json:"text"`
				FunctionCall json.RawMessage `json:"functionCall"`
				InlineData   json.RawMessage `json:"inlineData"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
}
