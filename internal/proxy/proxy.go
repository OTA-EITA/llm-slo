// Package proxy forwards LLM API calls unchanged and records latency, time
// to first chunk, token usage and cost for each one.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/OTA-EITA/llm-slo/internal/config"
	"github.com/OTA-EITA/llm-slo/internal/metrics"
	"github.com/OTA-EITA/llm-slo/internal/usage"
)

// maxBuffered bounds how much of a non-streamed response we keep to read its
// usage. Bigger responses still pass through; only their usage is lost.
const maxBuffered = 16 << 20

type Server struct {
	cfg       *config.Config
	Registry  *metrics.Registry
	m         *instruments
	routes    []*route // longest prefix first
	apps      *metrics.Limiter
	models    *metrics.Limiter
	now       func() time.Time
	logger    *log.Logger
	transport http.RoundTripper
}

type route struct {
	up     config.Upstream
	target *url.URL
	proxy  *httputil.ReverseProxy
}

type Option func(*Server)

// WithTransport replaces the HTTP transport used to reach upstreams.
func WithTransport(t http.RoundTripper) Option { return func(s *Server) { s.transport = t } }

// WithLogger sets where request logs go when log_requests is on.
func WithLogger(l *log.Logger) Option { return func(s *Server) { s.logger = l } }

func New(cfg *config.Config, version string, opts ...Option) (*Server, error) {
	s := &Server{
		cfg:      cfg,
		Registry: &metrics.Registry{},
		apps:     metrics.NewLimiter(100, cfg.Apps),
		models:   metrics.NewLimiter(cfg.MaxModels, nil),
		now:      time.Now,
		logger:   log.New(io.Discard, "", 0),
	}
	for _, o := range opts {
		o(s)
	}
	if s.transport == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.ResponseHeaderTimeout = 10 * time.Minute // long generations; the client sets its own deadline
		s.transport = t
	}
	s.m = newInstruments(s.Registry, cfg, version)
	for _, u := range cfg.Upstreams {
		target, err := url.Parse(u.URL)
		if err != nil {
			return nil, err
		}
		rt := &route{up: u, target: target}
		rt.proxy = &httputil.ReverseProxy{
			Rewrite:        rt.rewrite,
			Transport:      s.transport,
			ModifyResponse: s.modifyResponse,
			ErrorHandler:   s.errorHandler,
			FlushInterval:  -1, // pass streamed bytes on as soon as they arrive
		}
		s.routes = append(s.routes, rt)
	}
	sort.Slice(s.routes, func(i, j int) bool { return len(s.routes[i].up.Prefix) > len(s.routes[j].up.Prefix) })
	return s, nil
}

func (rt *route) rewrite(pr *httputil.ProxyRequest) {
	p := strings.TrimPrefix(pr.In.URL.Path, rt.up.Prefix)
	if p == "" || p[0] != '/' {
		p = "/" + p
	}
	pr.Out.URL.Path = p
	pr.Out.URL.RawPath = ""
	pr.SetURL(rt.target)
	// Let the transport ask for gzip and decompress it, so we can read usage.
	pr.Out.Header.Del("Accept-Encoding")
}

func (s *Server) match(path string) *route {
	for _, rt := range s.routes {
		if rt.up.Prefix == "/" || path == rt.up.Prefix || strings.HasPrefix(path, rt.up.Prefix+"/") {
			return rt
		}
	}
	return nil
}

type ctxKey struct{}

// call is one proxied request, from arrival to the last byte of the answer.
type call struct {
	s        *Server
	up       config.Upstream
	req      usage.Request
	labels   []string // app, provider, model, operation
	start    time.Time
	once     sync.Once
	status   int
	stream   *usage.Stream
	buf      bytes.Buffer
	overflow bool
	ttfc     time.Duration
	gotChunk bool
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
		return
	}
	start := s.now()
	app := r.Header.Get(s.cfg.AppHeader)
	r.Header.Del(s.cfg.AppHeader) // this header is ours; the provider does not need it
	if app == "" {
		app = s.cfg.DefaultApp
	}
	app = s.apps.Value(app)

	rt := s.match(r.URL.Path)
	if rt == nil {
		http.Error(w, "llm-slo: no upstream for this path", http.StatusNotFound)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, s.cfg.MaxRequestBytes+1))
	_ = r.Body.Close()
	if err != nil {
		http.Error(w, "llm-slo: reading request: "+err.Error(), http.StatusBadRequest)
		return
	}
	upPath := strings.TrimPrefix(r.URL.Path, rt.up.Prefix)
	req := usage.ParseRequest(rt.up.API, upPath, body)
	c := &call{
		s: s, up: rt.up, req: req, start: start,
		labels: []string{app, rt.up.Provider, s.models.Value(orUnknown(req.Model)), req.Operation},
	}
	if int64(len(body)) > s.cfg.MaxRequestBytes {
		c.finish(ErrTooLarge, usage.Usage{})
		http.Error(w, "llm-slo: request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	if req.Stream && rt.up.InjectStreamUsage {
		if nb, ok := usage.InjectStreamUsage(body); ok {
			body = nb
		}
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.Header.Set("Content-Length", strconv.Itoa(len(body)))
	r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, c))
	rt.proxy.ServeHTTP(w, r)
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func (s *Server) modifyResponse(resp *http.Response) error {
	c, _ := resp.Request.Context().Value(ctxKey{}).(*call)
	if c == nil {
		return nil
	}
	c.status = resp.StatusCode
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mt == "text/event-stream" && resp.StatusCode < 400 {
		c.stream = usage.NewStream(c.up.API)
	}
	resp.Body = &meteredBody{rc: resp.Body, c: c, ctx: resp.Request.Context()}
	return nil
}

func (s *Server) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	c, _ := r.Context().Value(ctxKey{}).(*call)
	kind := ErrOther
	var ne net.Error
	switch {
	case errors.Is(err, context.Canceled):
		kind = ErrClient
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		kind = ErrTimeout
	}
	if c != nil {
		c.finish(kind, usage.Usage{})
	}
	if kind != ErrClient {
		http.Error(w, "llm-slo: upstream error", http.StatusBadGateway)
	}
}

// meteredBody passes the response through unchanged while it is read.
type meteredBody struct {
	rc  io.ReadCloser
	c   *call
	ctx context.Context
	eof bool
}

func (b *meteredBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		b.c.observe(p[:n])
	}
	switch {
	case err == io.EOF:
		b.eof = true
		b.c.complete()
	case err != nil && b.ctx.Err() != nil:
		// the client hung up, so the proxy cancelled the upstream request
		b.c.finish(ErrClient, usage.Usage{})
	case err != nil:
		b.c.finish(ErrIncomplete, usage.Usage{})
	}
	return n, err
}

func (b *meteredBody) Close() error {
	err := b.rc.Close()
	if !b.eof {
		// The proxy closes the body early when the client goes away.
		b.c.finish(ErrClient, usage.Usage{})
	}
	return err
}

func (c *call) observe(p []byte) {
	if c.stream != nil {
		if c.stream.Write(p) && !c.gotChunk {
			c.gotChunk = true
			c.ttfc = c.s.now().Sub(c.start)
		}
		return
	}
	if c.overflow {
		return
	}
	if c.buf.Len()+len(p) > maxBuffered {
		c.overflow = true
		c.buf.Reset()
		return
	}
	c.buf.Write(p)
}

// complete is called at EOF of the upstream body.
func (c *call) complete() {
	if c.status >= 400 {
		c.finish(strconv.Itoa(c.status), usage.Usage{})
		return
	}
	var u usage.Usage
	if c.stream != nil {
		u = c.stream.Usage()
	} else if !c.overflow {
		u = usage.ParseBody(c.up.API, c.buf.Bytes())
	}
	c.finish("", u)
}

func (c *call) finish(errType string, u usage.Usage) {
	c.once.Do(func() { c.record(errType, u) })
}

func (c *call) record(errType string, u usage.Usage) {
	s := c.s
	d := s.now().Sub(c.start)
	callLbl := append(append([]string(nil), c.labels...), errType)
	s.m.duration.Observe(d.Seconds(), callLbl...)
	if c.gotChunk {
		s.m.firstChunk.Observe(c.ttfc.Seconds(), callLbl...)
	}

	var cost float64
	priced := false
	if u.Known {
		s.m.input.Add(float64(u.Input), c.labels...)
		s.m.output.Add(float64(u.Output), c.labels...)
		s.m.cacheRead.Add(float64(u.CacheRead), c.labels...)
		s.m.cacheWrite.Add(float64(u.CacheWrite), c.labels...)
		s.m.reasoning.Add(float64(u.Reasoning), c.labels...)
		if p, ok := s.cfg.PriceFor(c.req.Model, u.ResponseModel); ok {
			cost, priced = p.Cost(u), true
			s.m.cost.Add(cost, c.labels...)
		} else {
			s.m.noPrice.Inc(c.labels...)
		}
	} else if errType == "" {
		s.m.usageMissing.Inc(c.labels...)
	}

	if s.cfg.LogRequests {
		entry := map[string]any{
			"time":                      c.start.UTC().Format(time.RFC3339Nano),
			"app":                       c.labels[0],
			"gen_ai.provider.name":      c.up.Provider,
			"gen_ai.request.model":      c.req.Model,
			"gen_ai.operation.name":     c.req.Operation,
			"gen_ai.request.stream":     c.req.Stream,
			"http.response.status_code": c.status,
			"duration_seconds":          round(d.Seconds()),
		}
		if errType != "" {
			entry["error.type"] = errType
		}
		if u.ResponseModel != "" {
			entry["gen_ai.response.model"] = u.ResponseModel
		}
		if c.gotChunk {
			entry["time_to_first_chunk_seconds"] = round(c.ttfc.Seconds())
		}
		if u.Known {
			entry["gen_ai.usage.input_tokens"] = u.Input
			entry["gen_ai.usage.output_tokens"] = u.Output
			if u.CacheRead > 0 {
				entry["gen_ai.usage.cache_read.input_tokens"] = u.CacheRead
			}
			if u.CacheWrite > 0 {
				entry["gen_ai.usage.cache_write.input_tokens"] = u.CacheWrite
			}
			if u.Reasoning > 0 {
				entry["gen_ai.usage.reasoning.output_tokens"] = u.Reasoning
			}
		}
		if priced {
			entry["cost_usd"] = cost
		}
		line, _ := json.Marshal(entry)
		s.logger.Println(string(line))
	}
}

func round(v float64) float64 { return float64(int64(v*1e6+0.5)) / 1e6 }
