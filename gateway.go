// Package gateway is a small library for fronting gRPC services with a
// GraphQL surface. The zero-config path takes a list of .proto files and
// gRPC destinations and exposes them as a namespaced GraphQL API; the
// power features are layered on as middleware.
//
// This file is the public API sketch. Bodies are stubbed; see README.md
// for the design and examples/ for runnable wiring.
package gateway

import (
	"context"
	"iter"
	"net/http"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Gateway aggregates registered protos, their gRPC destinations, and the
// schema/runtime middleware chain. It is safe to construct, configure,
// and serve from a single goroutine; concurrent registration after Serve
// is a separate (later) concern.
type Gateway struct {
	// unexported impl
}

// New returns an empty Gateway. Add services with AddProto, layer
// behavior with Use, then mount via Handler.
func New(opts ...Option) *Gateway { return nil }

// Option configures the Gateway at construction time.
type Option func(*config)

type config struct{} // placeholder

// AddProto registers a .proto file at the given filesystem path. The
// services and RPCs declared in the file are mounted under a namespace
// (default: filename stem) on the GraphQL surface, and routed to the
// destination configured via To().
//
// Returns an error at registration time, not Serve time, so config
// mistakes surface during boot.
func (g *Gateway) AddProto(path string, opts ...ServiceOption) error { return nil }

// ServiceOption tunes a single AddProto call.
type ServiceOption func(*serviceConfig)

type serviceConfig struct{} // placeholder

// As overrides the default namespace (filename stem) for a registered
// proto. Collisions across registered protos are an error.
func As(namespace string) ServiceOption { return func(*serviceConfig) {} }

// To wires the gRPC destination for a registered proto. The variants:
//
//   - To(addr string)         — sugar for grpc.NewClient(addr) on first use
//   - To(conn *grpc.ClientConn) — caller-managed connection, dial opts, mTLS
//
// A future variant (To(Resolver)) will plug service-discovery shaped
// destinations (NATS, DNS, etc.); intentionally absent until needed.
func To(dest any) ServiceOption { return func(*serviceConfig) {} }

// Use appends middleware to both pipelines (schema rewrite + per-request
// runtime). Pair-shaped middleware (HideAndInject etc.) populates both
// halves; single-shaped middleware fills one and no-ops the other.
func (g *Gateway) Use(mw ...Pair) {}

// Handler returns the http.Handler that serves the assembled GraphQL
// schema. Calling Handler triggers schema assembly; subsequent AddProto
// calls return an error.
func (g *Gateway) Handler() http.Handler { return nil }

// ---------------------------------------------------------------------
// Middleware primitives. Same shape across unary and streaming, applied
// at two boundaries (GraphQL resolver in, gRPC client out).
// ---------------------------------------------------------------------

// Handler is the unary core: take a request, return a response.
// proto.Message lets middleware operate on dynamic types when the
// gateway parses .proto at runtime; typed wrappers exist for the
// codegen path.
type Handler func(ctx context.Context, req protoreflect.ProtoMessage) (protoreflect.ProtoMessage, error)

// Middleware wraps a Handler. Compose with Use; each layer can filter
// (return error without calling next), transform req before next, or
// transform resp after.
//
//	func mw(next gateway.Handler) gateway.Handler {
//	    return func(ctx context.Context, req protoreflect.ProtoMessage) (protoreflect.ProtoMessage, error) {
//	        // pre
//	        resp, err := next(ctx, req)
//	        if err != nil { return nil, err }
//	        // post
//	        return resp, nil
//	    }
//	}
type Middleware func(next Handler) Handler

// StreamHandler is the streaming core. Inputs and outputs are
// iter.Seq2[T, error] so middleware reads as a `for ... range` loop and
// errors flow alongside values without a sidecar channel. Cancellation
// rides ctx; close the input early-return to signal client-side end.
type StreamHandler func(ctx context.Context, in iter.Seq2[protoreflect.ProtoMessage, error]) iter.Seq2[protoreflect.ProtoMessage, error]

// StreamMiddleware wraps a StreamHandler. Same compositional rules as
// Middleware; chosen as a separate type because forcing iter.Seq2 on
// unary calls is annoying noise.
type StreamMiddleware func(next StreamHandler) StreamHandler

// Pair bundles a schema-time transform and a runtime middleware so they
// can be declared together and stay in sync. Either half may be nil.
type Pair struct {
	Schema  SchemaMiddleware
	Runtime Middleware
	Stream  StreamMiddleware
}

// ---------------------------------------------------------------------
// Schema layer. Operates on the assembled *graphql.Schema in memory
// after gateway loads protos; same chain shape as runtime.
// ---------------------------------------------------------------------

// Schema is the schema-rewrite pipeline's value. Kept as an opaque
// pointer here; the impl wraps graphql-go's *graphql.Schema and the
// proto-level metadata needed for matching rules ("any field of type
// auth.v1.Context").
type Schema struct {
	// unexported impl
}

// SchemaHandler rewrites a Schema. Returning an error fails gateway
// boot — schema rules are static, so misconfiguration should not be
// quietly tolerated.
type SchemaHandler func(*Schema) (*Schema, error)

// SchemaMiddleware wraps a SchemaHandler.
type SchemaMiddleware func(next SchemaHandler) SchemaHandler

// ---------------------------------------------------------------------
// Built-ins. The canonical ones live here; users compose more in their
// own packages.
// ---------------------------------------------------------------------

// HideAndInject hides every input field whose proto type matches T from
// the external schema, and at runtime populates those fields by calling
// resolve(ctx). The two halves are paired by construction so they
// cannot drift; resolve is called once per request per type and cached
// on the request context.
//
// Typical use is auth: a registered AuthService returns the auth
// context blob, the type is hidden from external clients, and any RPC
// whose input embeds it gets it filled transparently.
func HideAndInject[T protoreflect.ProtoMessage](resolve func(context.Context) (T, error)) Pair {
	return Pair{}
}

// Reject is the canonical short-circuit error: a middleware that
// rejects a request (auth failed, rate-limited, etc.) returns Reject so
// the gateway can map it to the right GraphQL error code. Plain errors
// are returned as opaque internal errors.
func Reject(code Code, msg string) error {
	return &rejection{Code: code, Msg: msg}
}

// Code categorises a Reject. Maps to GraphQL error extensions and, when
// the request is bridged from gRPC, to a status code.
type Code int

const (
	CodeUnauthenticated Code = iota + 1
	CodePermissionDenied
	CodeResourceExhausted
	CodeInvalidArgument
	CodeNotFound
	CodeInternal
)

type rejection struct {
	Code Code
	Msg  string
}

func (r *rejection) Error() string { return r.Msg }

// Compile-time assertions that the public types satisfy expected
// shapes. Keeps refactors honest before any impl exists.
var (
	_ Handler                  = (Handler)(nil)
	_ Middleware               = (Middleware)(nil)
	_ StreamHandler            = (StreamHandler)(nil)
	_ StreamMiddleware         = (StreamMiddleware)(nil)
	_ SchemaHandler            = (SchemaHandler)(nil)
	_ SchemaMiddleware         = (SchemaMiddleware)(nil)
	_ grpc.ClientConnInterface = (*grpc.ClientConn)(nil) // compile-time check that grpc dep is wired
)
