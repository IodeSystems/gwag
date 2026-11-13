package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/IodeSystems/graphql-go/v2/language/ast"
	"github.com/IodeSystems/graphql-go/v2/language/parser"
	"github.com/prometheus/client_golang/prometheus"
)

// Internal switchover (cutover slice S5).
//
// Blue (the cluster clients reach) forwards a share of GraphQL client
// requests to green's GraphQL endpoint and returns green's response.
// The client's own headers go with the request: green authenticates
// the client itself.
//
// Which requests go: rules like replay verification's (operation
// name, header, header value), each with a percent. A client's side is
// sticky: hash(key) mod 100 < percent sends it to green, with key the
// caller id, else a configured header, else the source address. The
// bucket of a key never changes, so raising the percent only moves
// clients from blue to green.
//
// When green fails (see switchoverBadStatus and forward), blue serves
// the request itself and reports green's error. A mutation is served
// on blue only when the request provably never reached green (it was
// not completely written); otherwise the client gets the error, so a
// mutation never runs twice.
//
// The share is set at runtime with the setSwitchover admin op and, in
// a cluster, stored under the "switchover" key of the gwag-cluster
// bucket. Every node with WithSwitchover reads it from the stream
// leader once a second (kv_leader.go).

// Outcomes of switchover_total.
const (
	SwitchoverOutcomeGreen    = "green"
	SwitchoverOutcomeFallback = "fallback"
	SwitchoverOutcomeError    = "error"
	SwitchoverOutcomeBlue     = "blue"
)

// Reasons of switchover_green_errors_total.
const (
	// The request was not completely sent: dial, TLS or write error.
	// Green cannot have run it.
	SwitchoverReasonConnect = "connect"
	// No complete answer within Timeout after the request was sent.
	SwitchoverReasonTimeout = "timeout"
	// The connection failed after the request was sent, before a
	// complete answer.
	SwitchoverReasonTransport = "transport"
	// Green answered 5xx, 404, 405, 1xx or 3xx.
	SwitchoverReasonStatus = "status"
	// Green answered a body that is not a GraphQL JSON response (no
	// JSON object with data or errors).
	SwitchoverReasonBadResponse = "bad_response"
)

// SwitchoverHeader marks a request blue forwarded. A gateway never
// forwards a request that carries it, so two gateways that point at
// each other do not loop. A client that sends it stays on blue.
//
// Stability: experimental
const SwitchoverHeader = "X-Gwag-Switchover"

const (
	switchoverKVKey           = "switchover"
	switchoverPoll            = time.Second
	switchoverDefaultTimeout  = 30 * time.Second
	switchoverDefaultMaxBytes = 4 << 20
	switchoverDefaultKeep     = 50
	switchoverDefaultLogEvery = 10 * time.Second
)

var errSwitchoverTimeout = errors.New("switchover timeout")

// SwitchoverOptions configures forwarding on blue. See WithSwitchover
// and docs/cutover.md.
//
// Stability: experimental
type SwitchoverOptions struct {
	// Target is green's GraphQL endpoint, e.g.
	// https://green.example:8080/api/graphql. Empty: nothing is
	// forwarded until setSwitchover sets a target.
	Target string
	// Percent (0..100) is the share of matching requests sent to
	// green, used when the matching rule sets none.
	Percent int
	// Rules select the requests that may go to green. The first rule
	// that matches decides; a request no rule matches stays on blue.
	// No rules: every request matches at Percent.
	Rules []SwitchoverRule
	// StickyHeader is the request header used as the sticky key when
	// the request has no caller id (e.g. X-Client-Id). Empty, or
	// absent on the request: the source address (RemoteAddr host).
	StickyHeader string
	// Client sends requests to green. Nil: a client without a
	// timeout that does not follow redirects. A Client.Timeout also
	// ends SSE responses.
	Client *http.Client
	// Timeout bounds one forwarded request until green's complete
	// answer (for an SSE or multipart response: until its headers).
	// 0: 30s.
	Timeout time.Duration
	// MaxBytes caps the request body that is forwarded. A larger
	// request stays on blue. 0: 4 MiB.
	MaxBytes int
	// Keep is the number of recent green errors kept for
	// GET /admin/switchover. 0: 50.
	Keep int
	// LogEvery is the least time between two green-error log lines.
	// 0: 10s.
	LogEvery time.Duration
}

// SwitchoverRule is one selection rule. Empty match fields match
// anything; all set fields must match.
//
// Stability: experimental
type SwitchoverRule struct {
	// OperationName is the GraphQL operation name (the request's
	// operationName, or the name of the document's only operation).
	// A WebSocket upgrade has no operation name: a rule that sets one
	// never matches it.
	OperationName string `json:"operationName,omitempty"`
	// Header is a request header that must be present.
	Header string `json:"header,omitempty"`
	// HeaderValue, with Header, is the value it must have.
	HeaderValue string `json:"headerValue,omitempty"`
	// Percent (1..100) overrides SwitchoverOptions.Percent for the
	// requests this rule matches. 0: the global percent.
	Percent int `json:"percent,omitempty"`
	// Exclude keeps the matching requests on blue.
	Exclude bool `json:"exclude,omitempty"`
}

// WithSwitchover turns on forwarding of a share of GraphQL client
// requests from this gateway (blue) to green. The runtime setting
// (setSwitchover admin op, gwag ctl cluster switchover) replaces Target,
// Percent and Rules until it is cleared.
//
// Stability: experimental
func WithSwitchover(opts SwitchoverOptions) Option {
	return func(cfg *config) {
		o := opts
		o.Rules = append([]SwitchoverRule(nil), opts.Rules...)
		cfg.switchover = &o
	}
}

// switchoverRecord is the runtime setting: the KV value under
// switchoverKVKey, and the setSwitchover body.
type switchoverRecord struct {
	Target    string           `json:"target,omitempty"`
	Percent   int              `json:"percent"`
	Rules     []SwitchoverRule `json:"rules,omitempty"`
	SetBy     string           `json:"set_by,omitempty"`
	SetUnixMs int64            `json:"set_unix_ms,omitempty"`
}

// switchoverConfig is the setting in force, validated.
type switchoverConfig struct {
	rec     switchoverRecord
	target  *url.URL // nil: no target, nothing is forwarded
	needsOp bool
	active  bool
	runtime bool
}

func buildSwitchoverConfig(rec switchoverRecord, static string, runtime bool) (*switchoverConfig, error) {
	c := &switchoverConfig{rec: rec, runtime: runtime}
	if rec.Percent < 0 || rec.Percent > 100 {
		return nil, fmt.Errorf("percent %d is not in 0..100", rec.Percent)
	}
	for i, r := range rec.Rules {
		if r.Percent < 0 || r.Percent > 100 {
			return nil, fmt.Errorf("rules[%d]: percent %d is not in 0..100", i, r.Percent)
		}
		if r.HeaderValue != "" && r.Header == "" {
			return nil, fmt.Errorf("rules[%d]: headerValue needs header", i)
		}
		if r.OperationName != "" {
			c.needsOp = true
		}
	}
	target := rec.Target
	if target == "" {
		target = static
	}
	if target != "" {
		u, err := url.Parse(target)
		if err != nil {
			return nil, fmt.Errorf("target: %v", err)
		}
		if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("target %q: want an http or https URL", target)
		}
		c.target = u
		c.rec.Target = target
	}
	if c.target != nil {
		if len(rec.Rules) == 0 {
			c.active = rec.Percent > 0
		}
		for _, r := range rec.Rules {
			if !r.Exclude && (r.Percent > 0 || rec.Percent > 0) {
				c.active = true
			}
		}
	}
	return c, nil
}

// share is the percent that applies to a request: 0 when no rule
// matches or the rule excludes it.
func (c *switchoverConfig) share(opName string, h http.Header) int {
	if len(c.rec.Rules) == 0 {
		return c.rec.Percent
	}
	for i := range c.rec.Rules {
		r := &c.rec.Rules[i]
		if r.OperationName != "" && r.OperationName != opName {
			continue
		}
		if r.Header != "" {
			v, ok := h[http.CanonicalHeaderKey(r.Header)]
			if !ok || (r.HeaderValue != "" && (len(v) == 0 || v[0] != r.HeaderValue)) {
				continue
			}
		}
		if r.Exclude {
			return 0
		}
		if r.Percent > 0 {
			return r.Percent
		}
		return c.rec.Percent
	}
	return 0
}

// switchoverBucket is the sticky bucket (0..99) of key. FNV-1a, no
// seed: every blue node puts a key in the same bucket.
func switchoverBucket(key string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return int(h.Sum32() % 100)
}

// switchoverToGreen reports whether a request with this sticky key
// goes to green at percent. Monotone in percent: a key on green at p
// is on green at every q > p.
func switchoverToGreen(key string, percent int) bool {
	return switchoverBucket(key) < percent
}

// switchoverRecent is one recent green error.
type switchoverRecent struct {
	at      time.Time
	reason  string
	outcome string
	opName  string
	err     string
}

type switchover struct {
	g      *Gateway
	opts   SwitchoverOptions
	client *http.Client
	cur    atomic.Pointer[switchoverConfig]

	outcomes map[string]*atomic.Int64
	reasons  map[string]*atomic.Int64
	metrics  *switchoverMetrics

	mu      sync.Mutex
	recent  []switchoverRecent
	lastLog time.Time
	lastRaw []byte // last KV value applied (cluster)
}

var (
	switchoverOutcomes = []string{SwitchoverOutcomeGreen, SwitchoverOutcomeFallback, SwitchoverOutcomeError, SwitchoverOutcomeBlue}
	switchoverReasons  = []string{SwitchoverReasonConnect, SwitchoverReasonTimeout, SwitchoverReasonTransport, SwitchoverReasonStatus, SwitchoverReasonBadResponse}
)

func newSwitchover(g *Gateway, o SwitchoverOptions, m *switchoverMetrics) *switchover {
	if o.Timeout <= 0 {
		o.Timeout = switchoverDefaultTimeout
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = switchoverDefaultMaxBytes
	}
	if o.Keep <= 0 {
		o.Keep = switchoverDefaultKeep
	}
	if o.LogEvery <= 0 {
		o.LogEvery = switchoverDefaultLogEvery
	}
	s := &switchover{g: g, opts: o, client: o.Client, metrics: m,
		outcomes: map[string]*atomic.Int64{}, reasons: map[string]*atomic.Int64{}}
	if s.client == nil {
		s.client = &http.Client{
			Transport:     http.DefaultTransport.(*http.Transport).Clone(),
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	for _, k := range switchoverOutcomes {
		s.outcomes[k] = &atomic.Int64{}
	}
	for _, k := range switchoverReasons {
		s.reasons[k] = &atomic.Int64{}
	}
	c, err := buildSwitchoverConfig(s.staticRecord(), o.Target, false)
	if err != nil {
		panic("gateway: WithSwitchover: " + err.Error())
	}
	s.cur.Store(c)
	return s
}

func (s *switchover) staticRecord() switchoverRecord {
	return switchoverRecord{Percent: s.opts.Percent, Rules: s.opts.Rules}
}

func (s *switchover) count(outcome string) {
	s.outcomes[outcome].Add(1)
	if s.metrics != nil {
		s.metrics.total.WithLabelValues(outcome).Inc()
	}
}

// noteError counts a green error and keeps it for the admin view.
// outcome is fallback, error, or green (an SSE stream that broke after
// its headers went to the client).
func (s *switchover) noteError(reason, outcome, opName string, err error) {
	s.reasons[reason].Add(1)
	if s.metrics != nil {
		s.metrics.greenErrors.WithLabelValues(reason).Inc()
	}
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	now := time.Now()
	s.mu.Lock()
	s.recent = append(s.recent, switchoverRecent{at: now, reason: reason, outcome: outcome, opName: opName, err: msg})
	if over := len(s.recent) - s.opts.Keep; over > 0 {
		s.recent = append(s.recent[:0:0], s.recent[over:]...)
	}
	logIt := now.Sub(s.lastLog) >= s.opts.LogEvery
	if logIt {
		s.lastLog = now
	}
	s.mu.Unlock()
	if logIt {
		replayLog("switchover: green error reason=%s op=%q outcome=%s err=%q (rate-limited; GET /admin/switchover lists recent ones)",
			reason, opName, outcome, msg)
	}
}

func (s *switchover) recentErrors() []switchoverRecent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]switchoverRecent(nil), s.recent...)
}

// stickyKey is the key a request's side is decided by: the caller id
// (WithCallerIDExtractor and its flavors, or WithCallerHeaders), else
// StickyHeader, else the source address.
func (s *switchover) stickyKey(r *http.Request) string {
	g := s.g
	if ex := g.cfg.callerIDExtractor; ex != nil {
		if v, err := ex(withHTTPRequest(r.Context(), r)); err == nil && v != "" {
			return v
		}
	} else if len(g.cfg.callerHeaders) > 0 {
		if v := callerFromContext(withHTTPRequest(r.Context(), r), g.cfg.callerHeaders); v != "unknown" {
			return v
		}
	}
	if h := s.opts.StickyHeader; h != "" {
		if v := r.Header.Get(h); v != "" {
			return v
		}
	}
	return peerIP(r)
}

func (s *switchover) toGreen(c *switchoverConfig, opName string, r *http.Request) bool {
	p := c.share(opName, r.Header)
	if p <= 0 {
		return false
	}
	return switchoverToGreen(s.stickyKey(r), p)
}

// switchoverHandler wraps the GraphQL HTTP handler on blue. It is the
// outermost layer: a forwarded request is not instrumented, logged or
// replay-recorded on blue (green serves it). A fallback runs next as
// any other request.
func (g *Gateway) switchoverHandler(next http.Handler) http.Handler {
	s := g.switchover
	if s == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := s.cur.Load()
		if !c.active || r.Header.Get(SwitchoverHeader) != "" {
			next.ServeHTTP(w, r)
			return
		}
		if isWebSocketUpgrade(r) {
			s.serveWebSocket(c, w, r, next)
			return
		}
		if r.Method != http.MethodPost || !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") || r.Body == nil {
			s.count(SwitchoverOutcomeBlue)
			next.ServeHTTP(w, r)
			return
		}
		orig := r.Body
		buf, err := io.ReadAll(io.LimitReader(orig, int64(s.opts.MaxBytes)+1))
		if err != nil || len(buf) > s.opts.MaxBytes {
			r.Body = struct {
				io.Reader
				io.Closer
			}{io.MultiReader(bytes.NewReader(buf), orig), orig}
			s.count(SwitchoverOutcomeBlue)
			next.ServeHTTP(w, r)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(buf))
		opName, ok := replayOperationName(buf, c.needsOp)
		if !ok || !s.toGreen(c, opName, r) {
			s.count(SwitchoverOutcomeBlue)
			next.ServeHTTP(w, r)
			return
		}
		s.forward(c, w, r, buf, opName, next)
	})
}

func isWebSocketUpgrade(r *http.Request) bool {
	return r.Method == http.MethodGet && strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// switchoverHopHeaders are not forwarded in either direction.
var switchoverHopHeaders = []string{"Connection", "Keep-Alive", "Proxy-Connection", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade", "Content-Length"}

func copySwitchoverHeader(dst, src http.Header) {
	for k, vv := range src {
		dst[k] = append([]string(nil), vv...)
	}
	for _, f := range src.Values("Connection") {
		for _, k := range strings.Split(f, ",") {
			dst.Del(strings.TrimSpace(k))
		}
	}
	for _, k := range switchoverHopHeaders {
		dst.Del(k)
	}
}

// targetURL is c.target with the client's query string when the
// target has none.
func (c *switchoverConfig) targetURL(r *http.Request) *url.URL {
	u := *c.target
	if u.RawQuery == "" {
		u.RawQuery = r.URL.RawQuery
	}
	return &u
}

// setSwitchoverForwarded sets the X-Forwarded-* headers of the
// request to green from the client request r, keeping what a proxy in
// front of blue set.
func setSwitchoverForwarded(h http.Header, r *http.Request) {
	if ip := peerIP(r); ip != "" {
		if prior := r.Header.Values("X-Forwarded-For"); len(prior) > 0 {
			ip = strings.Join(prior, ", ") + ", " + ip
		}
		h.Set("X-Forwarded-For", ip)
	}
	if v := r.Header.Get("X-Forwarded-Host"); v != "" {
		h.Set("X-Forwarded-Host", v)
	} else if r.Host != "" {
		h.Set("X-Forwarded-Host", r.Host)
	}
	switch {
	case r.Header.Get("X-Forwarded-Proto") != "":
		h.Set("X-Forwarded-Proto", r.Header.Get("X-Forwarded-Proto"))
	case r.TLS != nil:
		h.Set("X-Forwarded-Proto", "https")
	default:
		h.Set("X-Forwarded-Proto", "http")
	}
	h.Set(SwitchoverHeader, "1")
}

// switchoverBadStatus: statuses that are not green's GraphQL answer.
// gat answers request errors with 4xx and a GraphQL body; 404 and 405
// mean Target is not a GraphQL endpoint.
func switchoverBadStatus(code int) bool {
	return code < 200 || (code >= 300 && code < 400) || code == http.StatusNotFound || code == http.StatusMethodNotAllowed || code >= 500
}

func switchoverStreamType(ct string) bool {
	ct = strings.ToLower(ct)
	return strings.HasPrefix(ct, "text/event-stream") || strings.HasPrefix(ct, "multipart/mixed")
}

// isGraphQLResponse: a JSON object with data or errors.
func isGraphQLResponse(ct string, body []byte) bool {
	if !strings.Contains(strings.ToLower(ct), "json") {
		return false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return false
	}
	_, d := m["data"]
	_, e := m["errors"]
	return d || e
}

// forward sends one POST to green and writes green's answer, or falls
// back (see fail).
func (s *switchover) forward(c *switchoverConfig, w http.ResponseWriter, r *http.Request, body []byte, opName string, next http.Handler) {
	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	timer := time.AfterFunc(s.opts.Timeout, func() { cancel(errSwitchoverTimeout) })
	defer timer.Stop()
	var sent atomic.Bool
	trace := &httptrace.ClientTrace{WroteRequest: func(i httptrace.WroteRequestInfo) {
		if i.Err == nil {
			sent.Store(true)
		}
	}}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodPost, c.targetURL(r).String(), bytes.NewReader(body))
	if err != nil {
		s.fail(w, r, next, body, opName, SwitchoverReasonConnect, err, false)
		return
	}
	copySwitchoverHeader(req.Header, r.Header)
	setSwitchoverForwarded(req.Header, r)
	failReason := func() string {
		if errors.Is(context.Cause(ctx), errSwitchoverTimeout) {
			return SwitchoverReasonTimeout
		}
		return SwitchoverReasonTransport
	}
	resp, err := s.client.Do(req)
	if err != nil {
		if r.Context().Err() != nil {
			return // the client went away; not green's failure
		}
		if !sent.Load() {
			s.fail(w, r, next, body, opName, SwitchoverReasonConnect, err, false)
			return
		}
		s.fail(w, r, next, body, opName, failReason(), err, true)
		return
	}
	defer resp.Body.Close()
	if switchoverBadStatus(resp.StatusCode) {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		s.fail(w, r, next, body, opName, SwitchoverReasonStatus,
			fmt.Errorf("green answered %s: %s", resp.Status, strings.TrimSpace(string(snippet))), true)
		return
	}
	ct := resp.Header.Get("Content-Type")
	if switchoverStreamType(ct) {
		timer.Stop()
		copySwitchoverHeader(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		s.count(SwitchoverOutcomeGreen)
		if err := copyFlushing(w, resp.Body); err != nil && r.Context().Err() == nil {
			s.noteError(SwitchoverReasonTransport, SwitchoverOutcomeGreen, opName, err)
		}
		return
	}
	rb, err := io.ReadAll(resp.Body)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		s.fail(w, r, next, body, opName, failReason(), err, true)
		return
	}
	if enc := resp.Header.Get("Content-Encoding"); (enc == "" || enc == "identity") && !isGraphQLResponse(ct, rb) {
		n := len(rb)
		if n > 256 {
			n = 256
		}
		s.fail(w, r, next, body, opName, SwitchoverReasonBadResponse,
			fmt.Errorf("green answered %s with %q: not a GraphQL response: %s", resp.Status, ct, strings.TrimSpace(string(rb[:n]))), true)
		return
	}
	copySwitchoverHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(rb)
	s.count(SwitchoverOutcomeGreen)
}

func copyFlushing(w http.ResponseWriter, body io.Reader) error {
	f, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return nil // the client went away
			}
			if f != nil {
				f.Flush()
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// fail handles a green failure. Blue serves the request when that is
// safe: the request never reached green (sent false), or it is not a
// mutation. Otherwise green may have run the mutation, and the client
// gets the error.
func (s *switchover) fail(w http.ResponseWriter, r *http.Request, next http.Handler, body []byte, opName, reason string, err error, sent bool) {
	if !sent || !switchoverIsMutation(body) {
		s.noteError(reason, SwitchoverOutcomeFallback, opName, err)
		s.count(SwitchoverOutcomeFallback)
		r.Body = io.NopCloser(bytes.NewReader(body))
		next.ServeHTTP(w, r)
		return
	}
	s.noteError(reason, SwitchoverOutcomeError, opName, err)
	s.count(SwitchoverOutcomeError)
	msg := fmt.Sprintf("switchover: green failed after the request was sent (%s: %v); a mutation is not run again on blue", reason, err)
	out, _ := json.Marshal(map[string]any{"errors": []any{map[string]any{
		"message":    msg,
		"extensions": map[string]any{"code": "unavailable", "status": http.StatusBadGateway, "switchoverReason": reason},
	}}})
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusBadGateway)
	_, _ = w.Write(out)
}

// switchoverIsMutation reports whether the request body may run a
// mutation: the selected operation is a mutation, or the operation
// cannot be told (no query text, e.g. a persisted document id; a parse
// error; several operations and no operationName).
func switchoverIsMutation(body []byte) bool {
	var req struct {
		Query         string `json:"query"`
		OperationName string `json:"operationName"`
	}
	if json.Unmarshal(body, &req) != nil || req.Query == "" {
		return true
	}
	doc, err := parser.Parse(parser.ParseParams{Source: req.Query})
	if err != nil {
		return true
	}
	var ops []*ast.OperationDefinition
	for _, d := range doc.Definitions {
		if op, ok := d.(*ast.OperationDefinition); ok {
			ops = append(ops, op)
		}
	}
	var sel *ast.OperationDefinition
	switch {
	case req.OperationName != "":
		for _, op := range ops {
			if op.Name != nil && op.Name.Value == req.OperationName {
				sel = op
			}
		}
	case len(ops) == 1:
		sel = ops[0]
	}
	if sel == nil {
		return true
	}
	return sel.Operation == ast.OperationTypeMutation
}

// serveWebSocket proxies a graphql-transport-ws upgrade to green when
// the request goes there. A failed dial or a non-101 error status
// falls back to blue: nothing ran on green. The proxied connection is
// registered like a local one, so Drain ends it.
func (s *switchover) serveWebSocket(c *switchoverConfig, w http.ResponseWriter, r *http.Request, next http.Handler) {
	if !s.toGreen(c, "", r) {
		s.count(SwitchoverOutcomeBlue)
		next.ServeHTTP(w, r)
		return
	}
	g := s.g
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	key := g.registerWSConn(cancel)
	defer g.unregisterWSConn(key)
	target := c.target
	proxy := &httputil.ReverseProxy{
		Transport: s.client.Transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			u := *target
			if u.RawQuery == "" {
				u.RawQuery = pr.In.URL.RawQuery
			}
			pr.Out.URL = &u
			pr.Out.Host = ""
			setSwitchoverForwarded(pr.Out.Header, pr.In)
		},
		ModifyResponse: func(resp *http.Response) error {
			if resp.StatusCode != http.StatusSwitchingProtocols && switchoverBadStatus(resp.StatusCode) {
				return fmt.Errorf("green answered %s", resp.Status)
			}
			s.count(SwitchoverOutcomeGreen)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if ctx.Err() != nil {
				return
			}
			reason := SwitchoverReasonConnect
			if strings.HasPrefix(err.Error(), "green answered") {
				reason = SwitchoverReasonStatus
			}
			s.noteError(reason, SwitchoverOutcomeFallback, "", err)
			s.count(SwitchoverOutcomeFallback)
			next.ServeHTTP(w, r)
		},
	}
	if proxy.Transport == nil {
		proxy.Transport = http.DefaultTransport
	}
	proxy.ServeHTTP(w, r.WithContext(ctx))
}

// registerWSConn adds cancel to the WebSocket registry Drain cancels.
func (g *Gateway) registerWSConn(cancel context.CancelFunc) uintptr {
	g.wsMu.Lock()
	defer g.wsMu.Unlock()
	for k := uintptr(1); ; k++ {
		if _, exists := g.wsConns[k]; !exists {
			g.wsConns[k] = cancel
			return k
		}
	}
}

func (g *Gateway) unregisterWSConn(k uintptr) {
	g.wsMu.Lock()
	delete(g.wsConns, k)
	g.wsMu.Unlock()
}

// ---- runtime setting ---------------------------------------------

var errSwitchoverOff = errors.New("switchover is not enabled on this gateway (WithSwitchover)")

// setRuntime stores rec as the runtime setting (nil clears it): in a
// cluster in the gwag-cluster bucket, which every node reads; here at
// once.
func (s *switchover) setRuntime(ctx context.Context, rec *switchoverRecord) (*switchoverConfig, error) {
	var c *switchoverConfig
	var err error
	if rec == nil {
		c, err = buildSwitchoverConfig(s.staticRecord(), s.opts.Target, false)
	} else {
		rec.SetUnixMs = time.Now().UnixMilli()
		rec.SetBy = s.g.nodeName()
		c, err = buildSwitchoverConfig(*rec, s.opts.Target, true)
		if err == nil && c.target == nil && (rec.Percent > 0 || len(rec.Rules) > 0) {
			err = errors.New("no target: set target, or start the gateway with WithSwitchover Target")
		}
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errSwitchoverInvalid, err)
	}
	if cl := s.g.cfg.cluster; cl != nil {
		s.g.mu.Lock()
		t := s.g.peers
		s.g.mu.Unlock()
		if t == nil || t.cluster == nil {
			return nil, errors.New("switchover: cluster tracking has not started")
		}
		kctx, cancel := context.WithTimeout(ctx, 2*kvCallTimeout)
		defer cancel()
		var raw []byte
		if rec == nil {
			err = t.cluster.Delete(kctx, switchoverKVKey)
		} else {
			raw, _ = json.Marshal(rec)
			err = kvPutRetry(kctx, t.cluster, switchoverKVKey, raw)
		}
		if err != nil {
			return nil, fmt.Errorf("switchover: %w", err)
		}
		s.mu.Lock()
		s.lastRaw = raw
		s.mu.Unlock()
	}
	s.cur.Store(c)
	return c, nil
}

var errSwitchoverInvalid = errors.New("invalid switchover setting")

// applyKV applies the runtime record as the stream leader holds it.
func (s *switchover) applyKV(raw []byte, ok bool) {
	if !ok {
		raw = nil
	}
	s.mu.Lock()
	same := bytes.Equal(raw, s.lastRaw) && (raw != nil || !s.cur.Load().runtime)
	if !same {
		s.lastRaw = raw
	}
	s.mu.Unlock()
	if same {
		return
	}
	if raw == nil {
		c, _ := buildSwitchoverConfig(s.staticRecord(), s.opts.Target, false)
		s.cur.Store(c)
		return
	}
	var rec switchoverRecord
	err := json.Unmarshal(raw, &rec)
	var c *switchoverConfig
	if err == nil {
		c, err = buildSwitchoverConfig(rec, s.opts.Target, true)
	}
	if err != nil {
		replayLog("switchover: ignoring the cluster setting: %v", err)
		return
	}
	s.cur.Store(c)
}

// pollLoop reads the runtime setting from the gwag-cluster stream
// leader every switchoverPoll. A failed read keeps the setting in
// force.
func (s *switchover) pollLoop(life context.Context) {
	tk := time.NewTicker(switchoverPoll)
	defer tk.Stop()
	for {
		select {
		case <-life.Done():
			return
		case <-tk.C:
		}
		s.g.mu.Lock()
		t := s.g.peers
		s.g.mu.Unlock()
		if t == nil {
			continue
		}
		kctx, cancel := kvCtx(life)
		v, ok, err := kvLeaderGet(kctx, s.g.cfg.cluster.Conn, clusterBucketName, switchoverKVKey)
		cancel()
		if err != nil {
			continue
		}
		s.applyKV(v, ok)
	}
}

// nodeName names this gateway in records it writes.
func (g *Gateway) nodeName() string {
	if cl := g.cfg.cluster; cl != nil && cl.Server != nil {
		return cl.Server.Name()
	}
	return "standalone"
}

// ---- metrics -----------------------------------------------------

// switchoverMetrics lives on the default Prometheus registry only,
// like replayMetrics.
type switchoverMetrics struct {
	total       *prometheus.CounterVec
	greenErrors *prometheus.CounterVec
}

func newSwitchoverMetrics(pm *prometheusMetrics) *switchoverMetrics {
	m := &switchoverMetrics{
		total: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: pm.prefix + "_switchover_total",
			Help: "GraphQL requests seen by switchover (WithSwitchover) while it is active, by outcome: green (served by green), fallback (green failed, blue served it), error (green failed after a mutation was sent; the client got the error), blue (stays on blue).",
		}, []string{"outcome"}),
		greenErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: pm.prefix + "_switchover_green_errors_total",
			Help: "Green failures of forwarded requests, by reason: connect (not sent), timeout, transport (connection failed after send), status (5xx, 404, 405), bad_response (not a GraphQL response).",
		}, []string{"reason"}),
	}
	pm.registry.MustRegister(m.total, m.greenErrors)
	return m
}
