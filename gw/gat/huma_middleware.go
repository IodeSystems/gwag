package gat

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

// inboundRequestKey carries the transport request (GraphQL HTTP request,
// or a synthetic one for gRPC ingress) to the middleware runner. It is
// private and separate from WithHTTPRequest on purpose: that key also
// turns on Authorization forwarding to OpenAPI upstreams, which the
// middleware runner must not switch on as a side effect.
type inboundRequestKey struct{}

func withInboundRequest(ctx context.Context, r *http.Request) context.Context {
	return context.WithValue(ctx, inboundRequestKey{}, r)
}

func inboundRequestFrom(ctx context.Context) *http.Request {
	if r, ok := ctx.Value(inboundRequestKey{}).(*http.Request); ok {
		return r
	}
	return HTTPRequestFromContext(ctx)
}

// runHumaMiddlewares runs an operation's huma middleware chain (the API
// stack captured at Register time, then op.Middlewares — the order
// huma.Register uses) around an in-process call, so GraphQL and gRPC
// enforce the same middlewares REST does.
//
// Without this an op guarded only by a huma middleware (auth, role
// check) was open over every surface except REST: the in-process
// dispatcher called the handler directly.
//
// A middleware that calls next continues to the handler with the
// context it passed (huma.WithValue values reach the handler). One that
// does not call next short-circuited the request; whatever status and
// body it wrote (typically via huma.WriteErr) becomes the returned
// error. Fails closed: no inbound request in ctx means the middlewares
// see an empty request, not a skipped chain.
func runHumaMiddlewares(ctx context.Context, chain huma.Middlewares, op *huma.Operation, call func(context.Context) (any, error)) (any, error) {
	hctx := newInprocHumaContext(ctx, op)
	var (
		out    any
		err    error
		called bool
	)
	chain.Handler(func(c huma.Context) {
		called = true
		out, err = call(c.Context())
	})(hctx)
	if !called {
		return nil, hctx.shortCircuitError()
	}
	return out, err
}

// requestRejection is the error a short-circuiting middleware, or a
// failed input check (inputCheck), turns into. GetStatus carries the
// status, so GraphQL extensions and the gRPC code map it like any
// handler status error.
type requestRejection struct {
	status int
	msg    string
}

func (e *requestRejection) Error() string  { return e.msg }
func (e *requestRejection) GetStatus() int { return e.status }

// inprocHumaContext is the huma.Context a middleware sees on an
// in-process call. Request accessors read the inbound transport
// request; response writes are captured so a rejection can be turned
// into an error. Method and Operation are the op's own (what the
// middleware sees over REST); path parameters are not available — the
// transport request carries no route — so Param returns "".
type inprocHumaContext struct {
	ctx    context.Context
	op     *huma.Operation
	r      *http.Request
	status int
	header http.Header
	body   bytes.Buffer
}

func newInprocHumaContext(ctx context.Context, op *huma.Operation) *inprocHumaContext {
	r := inboundRequestFrom(ctx)
	if r == nil {
		r = &http.Request{Method: op.Method, URL: &url.URL{Path: op.Path}, Header: http.Header{}}
	}
	return &inprocHumaContext{ctx: ctx, op: op, r: r, header: http.Header{}}
}

func (c *inprocHumaContext) shortCircuitError() error {
	status := c.status
	if status == 0 {
		// A middleware that neither called next nor wrote a status
		// still refused the request. Do not let that read as success.
		status = http.StatusForbidden
	}
	msg := http.StatusText(status)
	var model struct {
		Title  string `json:"title"`
		Detail string `json:"detail"`
	}
	if raw := bytes.TrimSpace(c.body.Bytes()); len(raw) > 0 {
		if json.Unmarshal(raw, &model) == nil && (model.Detail != "" || model.Title != "") {
			msg = model.Detail
			if msg == "" {
				msg = model.Title
			}
		} else {
			msg = string(raw)
		}
	}
	return &requestRejection{status: status, msg: fmt.Sprintf("%s: %s", c.op.OperationID, msg)}
}

func (c *inprocHumaContext) Operation() *huma.Operation { return c.op }
func (c *inprocHumaContext) Context() context.Context   { return c.ctx }
func (c *inprocHumaContext) TLS() *tls.ConnectionState  { return c.r.TLS }
func (c *inprocHumaContext) Version() huma.ProtoVersion {
	return huma.ProtoVersion{Proto: c.r.Proto, ProtoMajor: c.r.ProtoMajor, ProtoMinor: c.r.ProtoMinor}
}
func (c *inprocHumaContext) Method() string     { return c.op.Method }
func (c *inprocHumaContext) Host() string       { return c.r.Host }
func (c *inprocHumaContext) RemoteAddr() string { return c.r.RemoteAddr }
func (c *inprocHumaContext) URL() url.URL {
	if c.r.URL == nil {
		return url.URL{}
	}
	return *c.r.URL
}
func (c *inprocHumaContext) Param(string) string { return "" }
func (c *inprocHumaContext) Query(name string) string {
	if c.r.URL == nil {
		return ""
	}
	return c.r.URL.Query().Get(name)
}
func (c *inprocHumaContext) Header(name string) string { return c.r.Header.Get(name) }
func (c *inprocHumaContext) EachHeader(cb func(name, value string)) {
	for name, vals := range c.r.Header {
		for _, v := range vals {
			cb(name, v)
		}
	}
}

// BodyReader is empty: the transport body (a GraphQL envelope or a
// proto message) is not the op's body and has already been consumed.
func (c *inprocHumaContext) BodyReader() io.Reader { return strings.NewReader("") }
func (c *inprocHumaContext) GetMultipartForm() (*multipart.Form, error) {
	return nil, http.ErrNotMultipart
}
func (c *inprocHumaContext) SetReadDeadline(time.Time) error { return nil }
func (c *inprocHumaContext) SetStatus(code int)              { c.status = code }
func (c *inprocHumaContext) Status() int                     { return c.status }
func (c *inprocHumaContext) SetHeader(name, value string)    { c.header.Set(name, value) }
func (c *inprocHumaContext) AppendHeader(name, value string) { c.header.Add(name, value) }
func (c *inprocHumaContext) BodyWriter() io.Writer           { return &c.body }
