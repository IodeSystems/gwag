package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Replay verification (record/replay shadow verification).
//
// Blue (the cluster serving clients) records a sample of GraphQL HTTP
// requests: the client request, the client response, and every
// upstream call ("leg") the request made, with the upstream request
// as sent and the upstream response as received. Off the client path,
// a worker posts the recording to green's verify endpoint.
//
// Green runs the client request through its own pipeline with a
// replay session on the context. Every upstream seam (gRPC Invoke,
// OpenAPI and downstream-GraphQL HTTP, MCP tools/call, in-process
// proto handlers) answers from blue's recording instead of calling
// out, so nothing upstream runs twice and mutations are safe. Green
// compares each upstream request it built with the one blue sent, and
// its client response with blue's, and returns the result. A call
// green makes that blue never made cannot be answered: the leg is
// "unverifiable" and green's resolver gets an error.
//
// The comparison runs on green: green holds the descriptors of its own
// proto types, so it can decode both sides' wire bytes, and the
// endpoint can be called by hand with a saved recording.
//
// Code and metric names say "replay", not "shadow": shadow_check_total
// already names the (gwag.ps.v1.shadow) schema check.

// Outcomes of replay_verify_total.
const (
	ReplayOutcomeMatch        = "match"
	ReplayOutcomeDiff         = "diff"
	ReplayOutcomeUnverifiable = "unverifiable"
	ReplayOutcomeDropped      = "dropped"
	ReplayOutcomeError        = "error"
)

// Per-leg outcomes in ReplayLegResult.Outcome.
const (
	replayLegMatch        = "match"
	replayLegDiff         = "diff"
	replayLegUnverifiable = "unverifiable"
	replayLegNotCalled    = "not_called"
)

// Leg kinds.
const (
	replayKindGRPC    = "grpc"
	replayKindOpenAPI = "openapi"
	replayKindGraphQL = "graphql"
	replayKindMCP     = "mcp"
	replayKindInproc  = "inproc"
)

const (
	replayDefaultQueue    = 256
	replayDefaultWorkers  = 2
	replayDefaultMaxBytes = 4 << 20
	replayDefaultMaxLegs  = 128
	replayDefaultKeep     = 100
	replayDefaultTimeout  = 10 * time.Second
	replayDefaultLogEvery = 10 * time.Second
	replayMaxDiffs        = 50
	// replayVerifyBodyLimit caps the body the verify endpoint reads.
	replayVerifyBodyLimit = 64 << 20
	replayAbsent          = "(absent)"
)

// ReplayVerifyOptions configures recording on blue. See
// WithReplayVerify and docs/cutover.md.
//
// Stability: experimental
type ReplayVerifyOptions struct {
	// Target is green's verify endpoint, e.g.
	// https://green.example:8080/api/admin/replay/verify. Required.
	Target string
	// Token is the bearer sent to Target (green's admin token, hex,
	// or a credential green's AdminAuthorizer accepts). The endpoint
	// needs the admin role.
	Token string
	// Client sends recordings to Target. Nil: a client with Timeout.
	Client *http.Client
	// Timeout bounds one post to Target. 0: 10s.
	Timeout time.Duration

	// Match selects the requests that may be recorded. The first rule
	// that matches decides the sample rate. No rules: every request
	// matches at SampleRate.
	Match []ReplayMatch
	// SampleRate is the fraction (0..1] of matching requests that are
	// recorded, used when the matching rule sets none. 0 means 1.
	SampleRate float64

	// QueueSize bounds the recordings waiting to be sent. A recording
	// that does not fit is dropped and counted (outcome "dropped");
	// the client request is never slowed. 0: 256.
	QueueSize int
	// Workers is the number of goroutines posting to Target. 0: 2.
	Workers int
	// MaxBytes caps one recording: client request and response bodies
	// plus every leg body. A request over it is not recorded and is
	// counted as dropped. 0: 4 MiB.
	MaxBytes int
	// MaxLegs caps the upstream calls in one recording. 0: 128.
	MaxLegs int
	// Keep is the number of recent non-matching results kept for
	// GET /admin/replay/diffs. 0: 100.
	Keep int
	// LogEvery is the least time between two diff log lines. 0: 10s.
	LogEvery time.Duration

	// Redact, when set, runs on the worker before a recording is
	// sent. It may change the recording (remove a header, blank a
	// body). Returning false drops the recording without counting it.
	// Green compares what it receives, so a field redacted on blue
	// shows as a diff unless green's pipeline produces the same
	// redacted value; redact client-request credentials green does not
	// need, not leg payloads.
	Redact func(*ReplayExchange) bool
}

// ReplayMatch is one selection rule. Empty fields match anything; all
// set fields must match.
//
// Stability: experimental
type ReplayMatch struct {
	// OperationName is the GraphQL operation name (the request's
	// operationName, or the name of the document's only operation).
	OperationName string
	// Header is a request header that must be present.
	Header string
	// HeaderValue, with Header, is the value it must have.
	HeaderValue string
	// SampleRate overrides ReplayVerifyOptions.SampleRate for requests
	// this rule matches. 0: use the global rate.
	SampleRate float64
}

// WithReplayVerify turns on recording for replay verification: a
// sample of GraphQL HTTP requests to Handler is recorded and posted to
// opts.Target off the client path.
//
// Stability: experimental
func WithReplayVerify(opts ReplayVerifyOptions) Option {
	return func(cfg *config) {
		o := opts
		cfg.replayVerify = &o
	}
}

// WithReplayVerifyEndpoint serves POST /admin/replay/verify on the
// admin router (AdminHumaRouter), for a blue gateway's recordings. The
// route needs the admin role. Off by default: it runs a client request
// as whatever identity the recording carries.
//
// Stability: experimental
func WithReplayVerifyEndpoint() Option {
	return func(cfg *config) { cfg.replayEndpoint = true }
}

// ReplayExchange is one recorded client request with its upstream
// calls. It is the body of POST /admin/replay/verify.
//
// Stability: experimental
type ReplayExchange struct {
	ID            string        `json:"id"`
	RecordedAt    time.Time     `json:"recordedAt"`
	OperationName string        `json:"operationName,omitempty"`
	RemoteAddr    string        `json:"remoteAddr,omitempty"`
	Request       ReplayMessage `json:"request"`
	Response      ReplayMessage `json:"response"`
	Legs          []ReplayLeg   `json:"legs"`
}

// ReplayLeg is one upstream call.
//
// Stability: experimental
type ReplayLeg struct {
	// Kind: grpc, openapi, graphql, mcp, inproc.
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Version   string `json:"version"`
	// Op names the operation within the service: the gRPC method
	// name, the OpenAPI operationId (or "METHOD /path" without one),
	// the downstream GraphQL field label, the MCP tool name.
	Op string `json:"op"`
	// Target is the replica address or base URL blue used. Not
	// compared: replicas differ per pick and per cluster.
	Target   string        `json:"target,omitempty"`
	Request  ReplayMessage `json:"request"`
	Response ReplayMessage `json:"response"`
}

// ReplayMessage is a request or response, client side or upstream.
// gRPC: Path is the method path, Header the metadata, Body the
// deterministic wire bytes; a failed call has Code and Message.
// HTTP: Status/StatusText on responses. Error is a transport error
// (no response at all).
//
// Stability: experimental
type ReplayMessage struct {
	Method     string              `json:"method,omitempty"`
	Path       string              `json:"path,omitempty"`
	Status     int                 `json:"status,omitempty"`
	StatusText string              `json:"statusText,omitempty"`
	Code       int                 `json:"code,omitempty"`
	Message    string              `json:"message,omitempty"`
	Error      string              `json:"error,omitempty"`
	Header     map[string][]string `json:"header,omitempty"`
	Body       []byte              `json:"body,omitempty"`
}

// ReplayResult is green's answer to one recording.
//
// Stability: experimental
type ReplayResult struct {
	ID string `json:"id"`
	// Outcome: match, diff, unverifiable, error.
	Outcome  string            `json:"outcome"`
	Error    string            `json:"error,omitempty"`
	Legs     []ReplayLegResult `json:"legs"`
	Response ReplayMessage     `json:"response"`
	// Diffs lists every difference, legs first, capped at 50.
	Diffs     []ReplayDiff `json:"diffs"`
	Truncated bool         `json:"truncated,omitempty"`
}

// ReplayLegResult is one upstream call green made, or one blue made
// that green did not (Outcome not_called).
//
// Stability: experimental
type ReplayLegResult struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Version   string `json:"version"`
	Op        string `json:"op"`
	// Outcome: match, diff, unverifiable (blue made no such call),
	// not_called (blue made it, green did not).
	Outcome string `json:"outcome"`
	// BlueLeg is the index of the paired leg in the recording, -1 for
	// none.
	BlueLeg int `json:"blueLeg"`
	// Request is the upstream request green built; nil for not_called.
	Request *ReplayMessage `json:"request,omitempty"`
}

// ReplayDiff is one difference. Path is "legs[i].request.body.x" or
// "response.body.data.x"; Blue and Green are JSON-encoded values, or
// "(absent)".
//
// Stability: experimental
type ReplayDiff struct {
	Path  string `json:"path"`
	Blue  string `json:"blue"`
	Green string `json:"green"`
}

// replayActive gates every seam: 0 means no gateway in this process
// records or replays, so dispatch pays one atomic load.
var replayActive atomic.Int32

func replayOn() bool { return replayActive.Load() != 0 }

// ---- session -------------------------------------------------------

type replaySessionCtxKey struct{}
type replayCallCtxKey struct{}

// replaySession is one recorded (blue) or replayed (green) client
// request. Upstream seams find it on the context.
type replaySession struct {
	replay bool

	mu   sync.Mutex
	legs []ReplayLeg

	// blue
	budget   int
	maxLegs  int
	overflow bool

	// green
	used    []bool
	results []replayLegCompare
}

// replayLegCompare is one ReplayLegResult plus its diffs (paths
// relative to the leg).
type replayLegCompare struct {
	res   ReplayLegResult
	diffs []ReplayDiff
}

func withReplaySession(ctx context.Context, s *replaySession) context.Context {
	return context.WithValue(ctx, replaySessionCtxKey{}, s)
}

func replaySessionFrom(ctx context.Context) *replaySession {
	if !replayOn() {
		return nil
	}
	s, _ := ctx.Value(replaySessionCtxKey{}).(*replaySession)
	return s
}

// isReplaying reports whether ctx belongs to a green replay; metrics
// and the request log skip those.
func isReplaying(ctx context.Context) bool {
	s := replaySessionFrom(ctx)
	return s != nil && s.replay
}

type replayKey struct {
	kind, ns, ver, op string
}

func (k replayKey) String() string {
	return fmt.Sprintf("%s %s/%s %s", k.kind, k.ns, k.ver, k.op)
}

// replayCall is one upstream call within a session. On blue the
// attempt loop overwrites leg on every attempt and commit keeps the
// last, so a retried call is one leg.
type replayCall struct {
	s      *replaySession
	key    replayKey
	leg    ReplayLeg
	filled bool
	// target is the base URL / endpoint of the current attempt's
	// replica (HTTP kinds), set by the attempt loop.
	target string
}

func (s *replaySession) begin(key replayKey) *replayCall {
	return &replayCall{s: s, key: key}
}

func withReplayCall(ctx context.Context, c *replayCall) context.Context {
	return context.WithValue(ctx, replayCallCtxKey{}, c)
}

func replayCallFrom(ctx context.Context) *replayCall {
	if !replayOn() {
		return nil
	}
	c, _ := ctx.Value(replayCallCtxKey{}).(*replayCall)
	return c
}

// set fills the leg for the current attempt.
func (c *replayCall) set(target string, req, resp ReplayMessage) {
	c.leg = ReplayLeg{
		Kind: c.key.kind, Namespace: c.key.ns, Version: c.key.ver, Op: c.key.op,
		Target: target, Request: req, Response: resp,
	}
	c.filled = true
}

// unrecordable marks the session as not recordable (a body that
// cannot be copied).
func (c *replayCall) unrecordable() {
	c.s.mu.Lock()
	c.s.overflow = true
	c.s.mu.Unlock()
}

// commit adds the last attempt's leg to a blue session.
func (c *replayCall) commit() {
	if !c.filled || c.s.replay {
		return
	}
	s := c.s
	n := len(c.leg.Request.Body) + len(c.leg.Response.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.overflow {
		return
	}
	if len(s.legs) >= s.maxLegs || n > s.budget {
		s.overflow = true
		s.legs = nil
		return
	}
	s.budget -= n
	s.legs = append(s.legs, c.leg)
}

// target returns the Target of the first unused blue leg for key, so
// a replayed HTTP request is built against the base URL blue used and
// error texts that name it read the same.
func (s *replaySession) target(key replayKey) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, i := range s.candidatesLocked(key) {
		return s.legs[i].Target
	}
	return "http://replay.unrecorded.invalid"
}

// candidatesLocked returns unused blue legs for key: exact key first;
// when none, the same kind, namespace and op in another version (a
// routing change shows as a version diff, not as unverifiable).
func (s *replaySession) candidatesLocked(key replayKey) []int {
	var exact, other []int
	for i, l := range s.legs {
		if s.used[i] || l.Kind != key.kind || l.Namespace != key.ns || l.Op != key.op {
			continue
		}
		if l.Version == key.ver {
			exact = append(exact, i)
		} else {
			other = append(other, i)
		}
	}
	if len(exact) > 0 {
		return exact
	}
	return other
}

// errReplayUnrecorded is what a seam returns on green when blue made
// no matching call.
var errReplayUnrecorded = errors.New("replay: blue made no such upstream call; green's call is unverifiable")

// match pairs green's upstream request with an unused blue leg of the
// same key: the first whose request compares equal, else the first.
// It records the comparison and returns blue's leg (its response is
// the answer), or errReplayUnrecorded.
func (s *replaySession) match(key replayKey, green ReplayMessage, decode func([]byte) any) (*ReplayLeg, error) {
	gdoc := replayLegDoc(key.ns, key.ver, green, decode)
	s.mu.Lock()
	defer s.mu.Unlock()
	res := ReplayLegResult{Kind: key.kind, Namespace: key.ns, Version: key.ver, Op: key.op, BlueLeg: -1}
	g := green
	res.Request = &g
	cands := s.candidatesLocked(key)
	if len(cands) == 0 {
		res.Outcome = replayLegUnverifiable
		s.results = append(s.results, replayLegCompare{res: res})
		return nil, errReplayUnrecorded
	}
	pick := cands[0]
	var diffs []ReplayDiff
	for n, i := range cands {
		b := s.legs[i]
		var d []ReplayDiff
		replayDiffValues("", replayLegDoc(b.Namespace, b.Version, b.Request, decode), gdoc, &d)
		if len(d) == 0 {
			pick, diffs = i, nil
			break
		}
		if n == 0 {
			diffs = d
		}
	}
	s.used[pick] = true
	res.BlueLeg = pick
	res.Outcome = replayLegMatch
	if len(diffs) > 0 {
		res.Outcome = replayLegDiff
	}
	s.results = append(s.results, replayLegCompare{res: res, diffs: diffs})
	leg := s.legs[pick]
	return &leg, nil
}

// replayLegDoc is the compared form of an upstream request: where it
// went (namespace, version, method, path), the headers minus
// gwag-stamped observability and deadline metadata, and the decoded
// body.
func replayLegDoc(ns, ver string, m ReplayMessage, decode func([]byte) any) map[string]any {
	doc := map[string]any{
		"namespace": ns,
		"version":   ver,
		"method":    m.Method,
		"path":      m.Path,
	}
	h := map[string]any{}
	for k, vs := range m.Header {
		lk := strings.ToLower(k)
		if replayIgnoredHeader(lk) {
			continue
		}
		vals := make([]any, len(vs))
		for i, v := range vs {
			vals[i] = v
		}
		h[lk] = vals
	}
	doc["header"] = h
	if decode == nil {
		decode = replayDecodeBody
	}
	doc["body"] = decode(m.Body)
	return doc
}

// replayIgnoredHeader: trace propagation headers (gwag stamps or
// forwards them per request) and the gRPC deadline. Everything else,
// injected headers included, is compared.
func replayIgnoredHeader(lk string) bool {
	if lk == "grpc-timeout" {
		return true
	}
	for _, h := range traceForwardHeaders {
		if lk == h {
			return true
		}
	}
	return false
}

// replayDecodeBody decodes JSON (numbers kept exact), else returns the
// text.
func replayDecodeBody(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err == nil && !dec.More() {
		return v
	}
	return string(b)
}

// replayProtoDecoder decodes wire bytes with desc into canonical JSON.
func replayProtoDecoder(desc protoreflect.MessageDescriptor) func([]byte) any {
	return func(b []byte) any {
		m := dynamicpb.NewMessage(desc)
		if err := proto.Unmarshal(b, m); err != nil {
			return "undecodable: " + hex.EncodeToString(b)
		}
		j, err := protojson.Marshal(m)
		if err != nil {
			return "undecodable: " + hex.EncodeToString(b)
		}
		return replayDecodeBody(j)
	}
}

// replayDiffValues appends the differences between blue and green.
func replayDiffValues(path string, blue, green any, out *[]ReplayDiff) {
	switch b := blue.(type) {
	case map[string]any:
		if g, ok := green.(map[string]any); ok {
			keys := make([]string, 0, len(b)+len(g))
			for k := range b {
				keys = append(keys, k)
			}
			for k := range g {
				if _, ok := b[k]; !ok {
					keys = append(keys, k)
				}
			}
			sort.Strings(keys)
			for _, k := range keys {
				p := k
				if path != "" {
					p = path + "." + k
				}
				bv, bok := b[k]
				gv, gok := g[k]
				switch {
				case !bok:
					*out = append(*out, ReplayDiff{Path: p, Blue: replayAbsent, Green: replayJSON(gv)})
				case !gok:
					*out = append(*out, ReplayDiff{Path: p, Blue: replayJSON(bv), Green: replayAbsent})
				default:
					replayDiffValues(p, bv, gv, out)
				}
			}
			return
		}
	case []any:
		if g, ok := green.([]any); ok {
			n := max(len(b), len(g))
			for i := 0; i < n; i++ {
				p := path + "[" + strconv.Itoa(i) + "]"
				switch {
				case i >= len(b):
					*out = append(*out, ReplayDiff{Path: p, Blue: replayAbsent, Green: replayJSON(g[i])})
				case i >= len(g):
					*out = append(*out, ReplayDiff{Path: p, Blue: replayJSON(b[i]), Green: replayAbsent})
				default:
					replayDiffValues(p, b[i], g[i], out)
				}
			}
			return
		}
	}
	if !reflect.DeepEqual(blue, green) {
		*out = append(*out, ReplayDiff{Path: path, Blue: replayJSON(blue), Green: replayJSON(green)})
	}
}

func replayJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

func replayHeader(h http.Header) map[string][]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string][]string, len(h))
	for k, v := range h {
		out[k] = append([]string(nil), v...)
	}
	return out
}

// ---- HTTP seam (OpenAPI, downstream GraphQL) -----------------------

// doUpstreamHTTP sends an upstream HTTP request. With a replay call on
// the request context it records the exchange (blue) or answers it
// from the recording (green).
func doUpstreamHTTP(client *http.Client, req *http.Request) (*http.Response, error) {
	if c := replayCallFrom(req.Context()); c != nil {
		return c.doHTTP(client, req)
	}
	return client.Do(req)
}

func (c *replayCall) doHTTP(client *http.Client, req *http.Request) (*http.Response, error) {
	msg := ReplayMessage{Method: req.Method, Path: req.URL.RequestURI(), Header: replayHeader(req.Header)}
	if req.Body != nil && req.Body != http.NoBody {
		if req.GetBody == nil {
			// A streamed body (multipart upload) cannot be copied.
			if c.s.replay {
				msg.Body = []byte("(streamed body not compared)")
			} else {
				c.unrecordable()
				return client.Do(req)
			}
		} else {
			rc, err := req.GetBody()
			if err == nil {
				msg.Body, _ = io.ReadAll(rc)
				_ = rc.Close()
			}
		}
	}
	if c.s.replay {
		leg, err := c.s.match(c.key, msg, nil)
		if err != nil {
			if req.Body != nil {
				_ = req.Body.Close()
			}
			return nil, err
		}
		if req.Body != nil {
			_ = req.Body.Close()
		}
		if leg.Response.Error != "" {
			return nil, errors.New(leg.Response.Error)
		}
		st := leg.Response.Status
		text := leg.Response.StatusText
		if text == "" {
			text = fmt.Sprintf("%d %s", st, http.StatusText(st))
		}
		return &http.Response{
			StatusCode: st,
			Status:     text,
			Proto:      "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
			Header:        http.Header(leg.Response.Header),
			Body:          io.NopCloser(bytes.NewReader(leg.Response.Body)),
			ContentLength: int64(len(leg.Response.Body)),
			Request:       req,
		}, nil
	}
	target := c.target
	if target == "" {
		target = req.URL.Scheme + "://" + req.URL.Host
	}
	resp, err := client.Do(req)
	if err != nil {
		c.set(target, msg, ReplayMessage{Error: err.Error()})
		return nil, err
	}
	body, rerr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	if rerr != nil {
		// The caller sees a short body; a replay could not reproduce
		// the read error.
		c.unrecordable()
		return resp, nil
	}
	c.set(target, msg, ReplayMessage{
		Status: resp.StatusCode, StatusText: resp.Status,
		Header: replayHeader(resp.Header), Body: body,
	})
	return resp, nil
}

// ---- proto seam (gRPC upstream, in-process handlers) ---------------

func replayGRPCRequest(ctx context.Context, method string, req protoreflect.ProtoMessage) ReplayMessage {
	msg := ReplayMessage{Path: method}
	if md, ok := metadata.FromOutgoingContext(ctx); ok && len(md) > 0 {
		msg.Header = map[string][]string(md.Copy())
	}
	msg.Body, _ = proto.MarshalOptions{Deterministic: true}.Marshal(req)
	return msg
}

// recordProto fills the leg for one proto attempt on blue.
func (c *replayCall) recordProto(callCtx context.Context, target, method string, req, resp protoreflect.ProtoMessage, err error) {
	msg := replayGRPCRequest(callCtx, method, req)
	var out ReplayMessage
	if err != nil {
		st := status.Convert(err)
		out.Code = int(st.Code())
		out.Message = st.Message()
		if out.Code == 0 {
			out.Code = int(codes.Unknown) // a non-status error still failed
		}
	} else if resp != nil {
		out.Body, _ = proto.MarshalOptions{Deterministic: true}.Marshal(resp)
	}
	c.set(target, msg, out)
}

// replayProto answers one proto call on green from the recording. The
// response is a pooled dynamic message of outputDesc, as the live
// path returns.
func (c *replayCall) replayProto(callCtx context.Context, method string, md protoreflect.MethodDescriptor, req protoreflect.ProtoMessage) (protoreflect.ProtoMessage, error) {
	msg := replayGRPCRequest(callCtx, method, req)
	leg, err := c.s.match(c.key, msg, replayProtoDecoder(md.Input()))
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	if leg.Response.Code != 0 {
		return nil, status.Error(codes.Code(leg.Response.Code), leg.Response.Message)
	}
	resp := acquireDynamicMessage(md.Output())
	if err := proto.Unmarshal(leg.Response.Body, resp); err != nil {
		releaseDynamicMessage(md.Output(), resp)
		return nil, status.Error(codes.Internal, "replay: decode recorded response: "+err.Error())
	}
	return resp, nil
}

// ---- MCP seam ------------------------------------------------------

func replayMCPRequest(tool string, args map[string]any) ReplayMessage {
	b, _ := json.Marshal(map[string]any{"name": tool, "arguments": args})
	return ReplayMessage{Method: "tools/call", Path: tool, Body: b}
}

// ---- client side (blue) --------------------------------------------

// replayRecorder is blue's recording state: options, the send queue,
// counters and the recent non-matching results.
type replayRecorder struct {
	g     *Gateway
	opts  ReplayVerifyOptions
	queue chan *ReplayExchange

	counts sync.Map // outcome → *atomic.Int64

	mu      sync.Mutex
	recent  []replayRecent
	lastLog time.Time
}

type replayRecent struct {
	at     time.Time
	opName string
	result ReplayResult
}

func newReplayRecorder(g *Gateway, o ReplayVerifyOptions) *replayRecorder {
	if o.QueueSize <= 0 {
		o.QueueSize = replayDefaultQueue
	}
	if o.Workers <= 0 {
		o.Workers = replayDefaultWorkers
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = replayDefaultMaxBytes
	}
	if o.MaxLegs <= 0 {
		o.MaxLegs = replayDefaultMaxLegs
	}
	if o.Keep <= 0 {
		o.Keep = replayDefaultKeep
	}
	if o.Timeout <= 0 {
		o.Timeout = replayDefaultTimeout
	}
	if o.LogEvery <= 0 {
		o.LogEvery = replayDefaultLogEvery
	}
	if o.SampleRate <= 0 || o.SampleRate > 1 {
		o.SampleRate = 1
	}
	if o.Client == nil {
		o.Client = &http.Client{Timeout: o.Timeout}
	}
	return &replayRecorder{g: g, opts: o, queue: make(chan *ReplayExchange, o.QueueSize)}
}

func (rr *replayRecorder) start(life context.Context) {
	for i := 0; i < rr.opts.Workers; i++ {
		go rr.worker(life)
	}
}

func (rr *replayRecorder) count(outcome string) {
	if v, ok := rr.counts.Load(outcome); ok {
		v.(*atomic.Int64).Add(1)
	} else {
		n := &atomic.Int64{}
		v, _ := rr.counts.LoadOrStore(outcome, n)
		v.(*atomic.Int64).Add(1)
	}
	rr.g.recordReplayVerify(outcome)
}

func (rr *replayRecorder) countOf(outcome string) int64 {
	if v, ok := rr.counts.Load(outcome); ok {
		return v.(*atomic.Int64).Load()
	}
	return 0
}

// sample reports whether a request with this operation name and these
// headers is recorded.
func (rr *replayRecorder) sample(opName string, h http.Header) bool {
	rate := rr.opts.SampleRate
	if len(rr.opts.Match) > 0 {
		var hit *ReplayMatch
		for i := range rr.opts.Match {
			m := &rr.opts.Match[i]
			if m.OperationName != "" && m.OperationName != opName {
				continue
			}
			if m.Header != "" {
				v, ok := h[http.CanonicalHeaderKey(m.Header)]
				if !ok || (m.HeaderValue != "" && (len(v) == 0 || v[0] != m.HeaderValue)) {
					continue
				}
			}
			hit = m
			break
		}
		if hit == nil {
			return false
		}
		if hit.SampleRate > 0 && hit.SampleRate <= 1 {
			rate = hit.SampleRate
		}
	}
	return rate >= 1 || replayRand() < rate
}

func (rr *replayRecorder) needsOpName() bool {
	for _, m := range rr.opts.Match {
		if m.OperationName != "" {
			return true
		}
	}
	return false
}

func (rr *replayRecorder) enqueue(ex *ReplayExchange) {
	select {
	case rr.queue <- ex:
	default:
		rr.count(ReplayOutcomeDropped)
	}
}

func (rr *replayRecorder) worker(life context.Context) {
	for {
		select {
		case <-life.Done():
			return
		case ex := <-rr.queue:
			rr.send(life, ex)
		}
	}
}

func (rr *replayRecorder) send(life context.Context, ex *ReplayExchange) {
	if rr.opts.Redact != nil && !rr.opts.Redact(ex) {
		return
	}
	body, err := json.Marshal(ex)
	if err != nil {
		rr.fail(ex, "encode: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(life, rr.opts.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rr.opts.Target, bytes.NewReader(body))
	if err != nil {
		rr.fail(ex, err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if rr.opts.Token != "" {
		req.Header.Set("Authorization", "Bearer "+rr.opts.Token)
	}
	resp, err := rr.opts.Client.Do(req)
	if err != nil {
		if life.Err() != nil {
			return // the gateway is closing; not green's failure
		}
		rr.fail(ex, err.Error())
		return
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(io.LimitReader(resp.Body, replayVerifyBodyLimit))
	if err != nil {
		rr.fail(ex, err.Error())
		return
	}
	if resp.StatusCode != http.StatusOK {
		rr.fail(ex, fmt.Sprintf("verify endpoint: %s: %s", resp.Status, strings.TrimSpace(string(rb))))
		return
	}
	var res ReplayResult
	if err := json.Unmarshal(rb, &res); err != nil {
		rr.fail(ex, "decode result: "+err.Error())
		return
	}
	switch res.Outcome {
	case ReplayOutcomeMatch, ReplayOutcomeDiff, ReplayOutcomeUnverifiable, ReplayOutcomeError:
	default:
		res.Outcome = ReplayOutcomeError
		if res.Error == "" {
			res.Error = "verify endpoint returned no outcome"
		}
	}
	rr.done(ex, res)
}

func (rr *replayRecorder) fail(ex *ReplayExchange, msg string) {
	rr.done(ex, ReplayResult{ID: ex.ID, Outcome: ReplayOutcomeError, Error: msg})
}

func (rr *replayRecorder) done(ex *ReplayExchange, res ReplayResult) {
	rr.count(res.Outcome)
	if res.Outcome == ReplayOutcomeMatch {
		return
	}
	now := time.Now()
	rr.mu.Lock()
	rr.recent = append(rr.recent, replayRecent{at: now, opName: ex.OperationName, result: res})
	if over := len(rr.recent) - rr.opts.Keep; over > 0 {
		rr.recent = append(rr.recent[:0:0], rr.recent[over:]...)
	}
	logIt := now.Sub(rr.lastLog) >= rr.opts.LogEvery
	if logIt {
		rr.lastLog = now
	}
	rr.mu.Unlock()
	if logIt {
		first := ""
		if len(res.Diffs) > 0 {
			d := res.Diffs[0]
			first = d.Path + ": blue=" + d.Blue + " green=" + d.Green
		}
		replayLog("replay verify: id=%s op=%q outcome=%s diffs=%d first=%q err=%q (rate-limited; GET /admin/replay/diffs lists recent ones)",
			ex.ID, ex.OperationName, res.Outcome, len(res.Diffs), first, res.Error)
	}
}

func (rr *replayRecorder) recentResults() []replayRecent {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	out := make([]replayRecent, len(rr.recent))
	copy(out, rr.recent)
	return out
}

// replayRecordHandler wraps the GraphQL HTTP handler on blue. A
// sampled POST application/json request runs with a recording
// session; once the response is written the recording is queued.
// Nothing here blocks on the queue or on green.
func (g *Gateway) replayRecordHandler(next http.Handler) http.Handler {
	rr := g.replayRec
	if rr == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") || r.Body == nil {
			next.ServeHTTP(w, r)
			return
		}
		limit := rr.opts.MaxBytes
		orig := r.Body
		buf, err := io.ReadAll(io.LimitReader(orig, int64(limit)+1))
		if err != nil || len(buf) > limit {
			// Too big (or unreadable): pass the bytes read plus the
			// rest through untouched.
			r.Body = struct {
				io.Reader
				io.Closer
			}{io.MultiReader(bytes.NewReader(buf), orig), orig}
			next.ServeHTTP(w, r)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(buf))
		opName, ok := replayOperationName(buf, rr.needsOpName())
		if !ok || !rr.sample(opName, r.Header) {
			next.ServeHTTP(w, r)
			return
		}
		s := &replaySession{budget: limit - len(buf), maxLegs: rr.opts.MaxLegs}
		cw := &replayCaptureWriter{ResponseWriter: w, max: limit - len(buf)}
		ex := &ReplayExchange{
			ID:            replayID(),
			RecordedAt:    time.Now().UTC(),
			OperationName: opName,
			RemoteAddr:    r.RemoteAddr,
			Request: ReplayMessage{
				Method: r.Method,
				Path:   r.URL.RequestURI(),
				Header: replayClientHeader(r.Header),
				Body:   buf,
			},
		}
		next.ServeHTTP(cw, r.WithContext(withReplaySession(r.Context(), s)))
		s.mu.Lock()
		over := s.overflow
		legs := s.legs
		budget := s.budget
		s.mu.Unlock()
		if over || cw.overflow || cw.stream || cw.buf.Len() > budget {
			rr.count(ReplayOutcomeDropped)
			return
		}
		status := cw.status
		if status == 0 {
			status = http.StatusOK
		}
		ex.Response = ReplayMessage{Status: status, Header: replayHeader(cw.Header()), Body: cw.buf.Bytes()}
		ex.Legs = legs
		if ex.Legs == nil {
			ex.Legs = []ReplayLeg{}
		}
		rr.enqueue(ex)
	})
}

// replayClientHeader copies the client headers green needs to run the
// request as the same caller. Hop-by-hop headers are left out.
func replayClientHeader(h http.Header) map[string][]string {
	out := replayHeader(h)
	for _, k := range []string{"Connection", "Keep-Alive", "Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade", "Content-Length"} {
		delete(out, k)
	}
	return out
}

// replayOperationName reads operationName from a JSON request body.
// ok is false for a batch (array) or a body that is not an object:
// those are not recorded. With parse set and no operationName, the
// name of the document's only operation is used.
func replayOperationName(body []byte, parse bool) (string, bool) {
	var req struct {
		Query         string `json:"query"`
		OperationName string `json:"operationName"`
	}
	trim := bytes.TrimLeft(body, " \t\r\n")
	if len(trim) == 0 || trim[0] != '{' {
		return "", false
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return "", false
	}
	if req.OperationName != "" || !parse || req.Query == "" {
		return req.OperationName, true
	}
	return singleOperationName(req.Query), true
}

// replayCaptureWriter passes the response through and keeps a copy.
type replayCaptureWriter struct {
	http.ResponseWriter
	buf      bytes.Buffer
	status   int
	max      int
	overflow bool
	stream   bool
}

func (w *replayCaptureWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
		if strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") ||
			strings.HasPrefix(w.Header().Get("Content-Type"), "multipart/mixed") {
			w.stream = true
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *replayCaptureWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if !w.overflow && !w.stream {
		if w.buf.Len()+len(p) > w.max {
			w.overflow = true
			w.buf.Reset()
		} else {
			w.buf.Write(p)
		}
	}
	return w.ResponseWriter.Write(p)
}

func (w *replayCaptureWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *replayCaptureWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func replayID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// replayRand returns a uniform float in [0,1).
func replayRand() float64 {
	var b [8]byte
	_, _ = rand.Read(b[:])
	u := uint64(b[0])<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 | uint64(b[3])<<32 |
		uint64(b[4])<<24 | uint64(b[5])<<16 | uint64(b[6])<<8 | uint64(b[7])
	return float64(u>>11) / (1 << 53)
}

// ---- verify (green) ------------------------------------------------

// replayBufferWriter is the ResponseWriter green's replay writes to.
type replayBufferWriter struct {
	h      http.Header
	buf    bytes.Buffer
	status int
}

func (w *replayBufferWriter) Header() http.Header { return w.h }
func (w *replayBufferWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}
func (w *replayBufferWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.buf.Write(p)
}

// serveReplayVerify is POST /admin/replay/verify.
func (g *Gateway) serveReplayVerify(w http.ResponseWriter, r *http.Request) {
	if !g.cfg.replayEndpoint {
		http.NotFound(w, r)
		return
	}
	var ex ReplayExchange
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, replayVerifyBodyLimit))
	if err := dec.Decode(&ex); err != nil {
		http.Error(w, "replay verify: decode recording: "+err.Error(), http.StatusBadRequest)
		return
	}
	res := g.ReplayVerify(r.Context(), &ex)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

// ReplayVerify runs a recording through this gateway's GraphQL
// pipeline with every upstream call answered from the recording, and
// compares. No upstream is called. The endpoint
// (WithReplayVerifyEndpoint) calls it; tests and tools may too.
//
// Stability: experimental
func (g *Gateway) ReplayVerify(ctx context.Context, ex *ReplayExchange) ReplayResult {
	res := ReplayResult{ID: ex.ID, Legs: []ReplayLegResult{}, Diffs: []ReplayDiff{}}
	h, err := g.graphqlHandler()
	if err != nil {
		res.Outcome, res.Error = ReplayOutcomeError, err.Error()
		return res
	}
	if !replayOn() {
		// A gateway built without the endpoint option, called
		// directly: the seams must see the session.
		replayActive.Add(1)
		defer replayActive.Add(-1)
	}
	s := &replaySession{replay: true, legs: ex.Legs, used: make([]bool, len(ex.Legs))}
	method := ex.Request.Method
	if method == "" {
		method = http.MethodPost
	}
	path := ex.Request.Path
	if path == "" {
		path = "/graphql"
	}
	req, err := http.NewRequestWithContext(withReplaySession(context.WithoutCancel(ctx), s), method, path, bytes.NewReader(ex.Request.Body))
	if err != nil {
		res.Outcome, res.Error = ReplayOutcomeError, "build request: "+err.Error()
		return res
	}
	for k, vs := range ex.Request.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.RemoteAddr = ex.RemoteAddr
	bw := &replayBufferWriter{h: http.Header{}}
	h.ServeHTTP(bw, req)
	if bw.status == 0 {
		bw.status = http.StatusOK
	}
	res.Response = ReplayMessage{Status: bw.status, Header: replayHeader(bw.h), Body: bw.buf.Bytes()}

	s.mu.Lock()
	results := s.results
	used := s.used
	s.mu.Unlock()

	var diffs []ReplayDiff
	unverifiable := false
	for i, lc := range results {
		res.Legs = append(res.Legs, lc.res)
		if lc.res.Outcome == replayLegUnverifiable {
			unverifiable = true
			diffs = append(diffs, ReplayDiff{Path: fmt.Sprintf("legs[%d]", i), Blue: replayAbsent, Green: replayJSON("green would call " + replayKey{lc.res.Kind, lc.res.Namespace, lc.res.Version, lc.res.Op}.String())})
		}
		for _, d := range lc.diffs {
			p := fmt.Sprintf("legs[%d].request", i)
			if d.Path != "" {
				p += "." + d.Path
			}
			diffs = append(diffs, ReplayDiff{Path: p, Blue: d.Blue, Green: d.Green})
		}
	}
	for i, u := range used {
		if u {
			continue
		}
		l := ex.Legs[i]
		idx := len(res.Legs)
		res.Legs = append(res.Legs, ReplayLegResult{Kind: l.Kind, Namespace: l.Namespace, Version: l.Version, Op: l.Op, Outcome: replayLegNotCalled, BlueLeg: i})
		diffs = append(diffs, ReplayDiff{Path: fmt.Sprintf("legs[%d]", idx), Blue: replayJSON("blue called " + replayKey{l.Kind, l.Namespace, l.Version, l.Op}.String()), Green: replayAbsent})
	}
	if ex.Response.Status != 0 && ex.Response.Status != res.Response.Status {
		diffs = append(diffs, ReplayDiff{Path: "response.status", Blue: strconv.Itoa(ex.Response.Status), Green: strconv.Itoa(res.Response.Status)})
	}
	var rd []ReplayDiff
	replayDiffValues("", replayDecodeBody(ex.Response.Body), replayDecodeBody(res.Response.Body), &rd)
	for _, d := range rd {
		p := "response.body"
		if d.Path != "" {
			p += "." + d.Path
		}
		diffs = append(diffs, ReplayDiff{Path: p, Blue: d.Blue, Green: d.Green})
	}
	if len(diffs) > replayMaxDiffs {
		diffs, res.Truncated = diffs[:replayMaxDiffs], true
	}
	if diffs != nil {
		res.Diffs = diffs
	}
	switch {
	case unverifiable:
		res.Outcome = ReplayOutcomeUnverifiable
	case len(diffs) > 0:
		res.Outcome = ReplayOutcomeDiff
	default:
		res.Outcome = ReplayOutcomeMatch
	}
	return res
}
