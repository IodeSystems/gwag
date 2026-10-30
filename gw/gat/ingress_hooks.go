package gat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/IodeSystems/graphql-go/v2"
)

// Ingress hooks let a host put its own request handling around gat's
// GraphQL handler: tracing and metrics, more body formats, a gate on
// WebSocket upgrades, a GraphiQL page, its own plan cache. Each is off
// until set and costs nothing when off. Set them before serving; they
// are read without a lock.

// Request is one GraphQL request: an operation as query text, or by
// trusted document id. A RequestParser returns one.
//
// Stability: experimental
type Request struct {
	Query         string         `json:"query"`
	Variables     map[string]any `json:"variables"`
	OperationName string         `json:"operationName"`
	DocumentID    string         `json:"documentId"`
	Extensions    map[string]any `json:"extensions"`
}

// RequestHook runs before gat handles a request. It returns the
// context gat handles the request with, and done, called (if non-nil)
// with that context once the response is written: state the hook put
// on the context is there, so done need not be a per-request closure.
// ok=false refuses the request: the hook has written the response
// itself and gat does nothing more.
//
// Stability: experimental
type RequestHook func(w http.ResponseWriter, r *http.Request) (ctx context.Context, done func(context.Context), ok bool)

// PreRequest sets a hook that runs for every GraphQL HTTP request
// (query, mutation, batch, GraphiQL page) before its body is read.
// WebSocket upgrades take the WSUpgrade hook instead. Use it to start a
// trace span, attach values to the context, record the request's
// duration in done, or refuse it. nil disables it. Returns g for
// chaining.
//
// Stability: experimental
func (g *Gateway) PreRequest(fn RequestHook) *Gateway {
	g.preRequest = fn
	return g
}

// WSUpgrade sets a hook that runs before a graphql-transport-ws
// upgrade is accepted. The context it returns is the connection's:
// every subscription on it derives from it, so cancelling it closes
// the connection. done runs when the connection ends. Use it to cap
// connections per client or to refuse upgrades while draining. nil
// disables it. Returns g for chaining.
//
// Stability: experimental
func (g *Gateway) WSUpgrade(fn RequestHook) *Gateway {
	g.wsUpgrade = fn
	return g
}

// GraphiQL serves h instead of executing when a browser asks for the
// GraphQL endpoint as a page: Accept names text/html and not
// application/json, and the URL has no `raw` parameter. h runs after
// the PreRequest hook, with its context. nil (the default) serves no
// page. Returns g for chaining.
//
// Stability: experimental
func (g *Gateway) GraphiQL(h http.Handler) *Gateway {
	g.graphiql = h
	return g
}

// RequestParser reads a GraphQL request from a POST body of one content
// type (see ParseContentType). An error answers 400 with its message;
// an *http.MaxBytesError answers 413.
//
// Stability: experimental
type RequestParser func(r *http.Request) (Request, error)

// ParseContentType reads POST bodies of mediaType (for example
// "multipart/form-data"; parameters and case are ignored) with fn
// instead of as JSON. maxBytes caps those bodies in place of
// MaxRequestBytes (n <= 0: no cap). Bodies of any other type are read
// as JSON. Returns g for chaining.
//
// Stability: experimental
func (g *Gateway) ParseContentType(mediaType string, maxBytes int64, fn RequestParser) *Gateway {
	if g.parsers == nil {
		g.parsers = map[string]contentParser{}
	}
	g.parsers[strings.ToLower(mediaType)] = contentParser{parse: fn, maxBytes: maxBytes}
	return g
}

// AcceptStringVariables accepts `variables` sent as a JSON-encoded
// string (`"variables": "{\"id\": 1}"`), as some older clients do
// (default off: such a body is refused with 400). Returns g for
// chaining.
//
// Stability: experimental
func (g *Gateway) AcceptStringVariables(b bool) *Gateway {
	g.stringVariables = b
	return g
}

// EchoComplete makes graphql-transport-ws answer a client's `complete`
// with a `complete` of its own once the subscription has stopped
// (default off: the protocol has the server send nothing). For hosts
// whose clients expect the echo. Returns g for chaining.
//
// Stability: experimental
func (g *Gateway) EchoComplete(b bool) *Gateway {
	g.echoComplete = b
	return g
}

// UsePlanCache makes gat plan queries with c instead of a cache of its
// own. c's rules then replace those gat builds from MaxOperations,
// MaxDepth and DisableIntrospection. A nil c plans every query afresh
// with the spec rules only. Call before the first schema. Returns g for
// chaining.
//
// Stability: experimental
func (g *Gateway) UsePlanCache(c *graphql.PlanCache) *Gateway {
	g.planOnce.Do(func() { g.planCache = c })
	return g
}

type contentParser struct {
	parse    RequestParser
	maxBytes int64
}

// mediaType is a Content-Type header's type/subtype, lowercased.
// Allocation-free for the usual lowercase header.
func mediaType(ct string) string {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.ToLower(strings.TrimSpace(ct))
}

// limitBody applies a body cap: a declared length over it answers 413
// here (false); a chunked body gets a limiting reader.
func limitBody(w http.ResponseWriter, r *http.Request, maxBytes int64) bool {
	if maxBytes <= 0 {
		return true
	}
	if r.ContentLength > maxBytes {
		writeTooLarge(w)
		return false
	}
	if r.ContentLength < 0 {
		r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	}
	return true
}

func writeTooLarge(w http.ResponseWriter) {
	writeCodedError(w, "request body too large", "BAD_REQUEST", http.StatusRequestEntityTooLarge)
}

// parseWith reads the request with a registered parser, answering the
// client itself on failure (false).
func parseWith(w http.ResponseWriter, r *http.Request, p contentParser) (Request, bool) {
	if !limitBody(w, r, p.maxBytes) {
		return Request{}, false
	}
	req, err := p.parse(r)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeTooLarge(w)
		} else {
			writeError(w, err.Error(), http.StatusBadRequest)
		}
		return Request{}, false
	}
	return req, true
}

// isGraphiQLRequest is a browser asking for the endpoint as a page.
// The URL is parsed only once the Accept header says so.
func isGraphiQLRequest(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	if !strings.Contains(accept, "text/html") || strings.Contains(accept, "application/json") {
		return false
	}
	_, raw := r.URL.Query()["raw"]
	return !raw
}

// lenientRequest is a JSON body under AcceptStringVariables:
// variables decode to any, so a string holding them is accepted too.
type lenientRequest struct {
	Request
	Variables any `json:"variables"`
}

var errVariables = errors.New("gat: variables must be an object")

// decodeLenient reads the body into a pooled buffer and unmarshals it,
// which allocates less than a json.Decoder per request.
func decodeLenient(r io.Reader) (Request, error) {
	buf := lenientBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer func() {
		if buf.Cap() <= lenientBufPoolMax {
			lenientBufPool.Put(buf)
		}
	}()
	if _, err := buf.ReadFrom(r); err != nil {
		return Request{}, err
	}
	var wire lenientRequest
	// Unmarshal copies strings out, so the buffer can go back.
	if err := json.Unmarshal(buf.Bytes(), &wire); err != nil {
		return Request{}, err
	}
	return wire.request()
}

var lenientBufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

const lenientBufPoolMax = 64 << 10

// request returns the decoded Request; an error when variables are
// neither an object nor a string holding one.
func (w *lenientRequest) request() (Request, error) {
	req := w.Request
	switch v := w.Variables.(type) {
	case nil:
	case map[string]any:
		req.Variables = v
	case string:
		vars, err := stringVariables(v)
		if err != nil {
			return req, errVariables
		}
		req.Variables = vars
	default:
		return req, errVariables
	}
	return req, nil
}

// stringVariables decodes variables sent as a JSON-encoded string. Its
// own function so only the map, not the whole Request, goes to the
// heap.
func stringVariables(s string) (map[string]any, error) {
	if s == "" {
		return nil, nil
	}
	var vars map[string]any
	err := json.Unmarshal([]byte(s), &vars)
	return vars, err
}
