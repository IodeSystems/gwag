package gateway

import (
	"context"
	"fmt"
	"reflect"
	"sync"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// InjectType returns a Transform that hides every field/arg of Go
// type T from the external schema (default; override with Hide(false))
// and at runtime populates it via resolve.
//
// Resolver:
//
//	func(ctx context.Context, current *T) (T, error)
//
// `current` is nil when the caller didn't send a value (always nil
// under Hide(true) since the arg isn't on the wire). Returning a zero
// T is a write — there's no skip sentinel.
//
// For proto messages, prefer the pointer type (e.g.
// InjectType[*authpb.Context]) — it sidesteps the value-copy on the
// embedded sync.Mutex that go vet flags. The library accepts either
// the pointer or the value form.
//
// Coverage today: schema rewrite (HideType) applies to every format;
// runtime injection runs only for proto-message T on the proto
// dispatcher path. OpenAPI and downstream-GraphQL dispatchers don't
// yet execute the runtime middleware chain.
func InjectType[T any](resolve func(ctx context.Context, current *T) (T, error), opts ...InjectOption) Transform {
	cfg := injectConfig{hide: true}
	for _, o := range opts {
		o.applyInject(&cfg)
	}

	irName := irNameForGoType[T]()

	if cfg.hide && cfg.nullable {
		panic(fmt.Sprintf("gateway: InjectType[%s]: Hide(true) + Nullable(true) is rejected — arg is gone from the schema, nullability is moot", irName))
	}

	var schema []SchemaRewrite
	if cfg.hide {
		schema = append(schema, HideTypeRewrite{Name: irName})
	}
	if cfg.nullable {
		schema = append(schema, NullableTypeRewrite{Name: irName})
	}

	runtime := protoInjectMiddlewareFor[T](cfg.hide, resolve)
	return Transform{Schema: schema, Runtime: runtime}
}

// protoInjectMiddlewareFor wires the runtime half of an InjectType
// registration. Returns nil when T is not a proto-message-typed value
// or pointer — the schema rewrite still applies, but runtime is a
// no-op.
func protoInjectMiddlewareFor[T any](hide bool, resolve func(ctx context.Context, current *T) (T, error)) Middleware {
	rt := reflect.TypeOf((*T)(nil)).Elem()
	tIsPointer := rt.Kind() == reflect.Ptr
	msgType := rt
	if !tIsPointer {
		msgType = reflect.PointerTo(rt)
	}
	if !msgType.Implements(protoMessageType) {
		return nil
	}
	zero := reflect.New(msgType.Elem()).Interface().(proto.Message)
	target := zero.ProtoReflect().Descriptor().FullName()

	return func(next Handler) Handler {
		return func(ctx context.Context, req protoreflect.ProtoMessage) (protoreflect.ProtoMessage, error) {
			adapter := func(ctx context.Context, current proto.Message) (proto.Message, error) {
				cur, err := unmarshalCurrent[T](current, tIsPointer, msgType)
				if err != nil {
					return nil, err
				}
				res, err := resolve(ctx, cur)
				if err != nil {
					return nil, err
				}
				if tIsPointer {
					msg, ok := any(res).(proto.Message)
					if !ok || msg == nil {
						return nil, nil
					}
					return msg, nil
				}
				rv := res
				msg, ok := any(&rv).(proto.Message)
				if !ok {
					return nil, fmt.Errorf("InjectType: *%T does not implement proto.Message", &rv)
				}
				return msg, nil
			}
			if err := injectProtoFields(ctx, req, target, hide, adapter); err != nil {
				return nil, err
			}
			return next(ctx, req)
		}
	}
}

// unmarshalCurrent boxes `current` (a wire-shape proto.Message read
// off the dynamicpb request) into a *T the user's resolver can read.
// Returns (nil, nil) when current is nil — the resolver sees absence
// as nil.
func unmarshalCurrent[T any](current proto.Message, tIsPointer bool, msgType reflect.Type) (*T, error) {
	if current == nil {
		return nil, nil
	}
	b, err := proto.Marshal(current)
	if err != nil {
		return nil, err
	}
	cur := new(T)
	if tIsPointer {
		// T is a pointer (e.g. *Context). *cur (a T) needs a fresh
		// allocation to point at; unmarshal into that new value.
		msgVal := reflect.New(msgType.Elem())
		msg := msgVal.Interface().(proto.Message)
		if err := proto.Unmarshal(b, msg); err != nil {
			return nil, err
		}
		reflect.ValueOf(cur).Elem().Set(msgVal)
		return cur, nil
	}
	// T is a value type (e.g. Context). cur (*T) is itself the
	// pointer-to-message; unmarshal directly into it.
	msg, ok := any(cur).(proto.Message)
	if !ok {
		return nil, fmt.Errorf("InjectType: *%T does not implement proto.Message", cur)
	}
	if err := proto.Unmarshal(b, msg); err != nil {
		return nil, err
	}
	return cur, nil
}

// injectProtoFields walks dyn for fields whose message type matches
// `target` and replaces each with resolve's output. Hide(true) passes
// nil current to the resolver; Hide(false) passes the dynamic value
// (or nil when unset).
func injectProtoFields(
	ctx context.Context,
	req protoreflect.ProtoMessage,
	target protoreflect.FullName,
	hide bool,
	resolve func(ctx context.Context, current proto.Message) (proto.Message, error),
) error {
	dyn, ok := req.(*dynamicpb.Message)
	if !ok {
		return nil
	}
	md := dyn.Descriptor()
	for i := 0; i < md.Fields().Len(); i++ {
		fd := md.Fields().Get(i)
		if fd.Kind() != protoreflect.MessageKind {
			continue
		}
		if fd.Message().FullName() != target {
			continue
		}
		var current proto.Message
		if !hide && dyn.Has(fd) {
			current = dyn.Get(fd).Message().Interface()
		}
		val, err := resolveCachedProto(ctx, target, current, resolve)
		if err != nil {
			return err
		}
		if val == nil {
			continue
		}
		b, err := proto.Marshal(val)
		if err != nil {
			return err
		}
		sub := dynamicpb.NewMessage(fd.Message())
		if err := proto.Unmarshal(b, sub); err != nil {
			return err
		}
		dyn.Set(fd, protoreflect.ValueOfMessage(sub))
	}
	return nil
}

// resolveCachedProto memoises resolver output per request, keyed on
// the proto target. The cache only kicks in when current is nil — a
// per-call-site current makes by-type caching wrong.
func resolveCachedProto(
	ctx context.Context,
	target protoreflect.FullName,
	current proto.Message,
	resolve func(ctx context.Context, current proto.Message) (proto.Message, error),
) (proto.Message, error) {
	if current != nil {
		return resolve(ctx, current)
	}
	cache, ok := ctx.Value(injectCacheCtxKey{}).(*sync.Map)
	if !ok {
		return resolve(ctx, nil)
	}
	entry, _ := cache.LoadOrStore(cacheKey{name: target}, &cachedResult{})
	cr := entry.(*cachedResult)
	cr.once.Do(func() {
		cr.val, cr.err = resolve(ctx, nil)
	})
	return cr.val, cr.err
}

// cacheKey identifies one inject-resolver entry on the per-request
// cache. Exactly one of `name` (InjectType), `path` (InjectPath), or
// `header` (InjectHeader) is populated; the others stay zero.
type cacheKey struct {
	name   protoreflect.FullName
	path   string
	header string
}

type cachedResult struct {
	once sync.Once
	val  proto.Message
	err  error
}

type injectCacheCtxKey struct{}

func withInjectCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, injectCacheCtxKey{}, &sync.Map{})
}

// InjectOption tunes an InjectType registration.
type InjectOption interface {
	applyInject(*injectConfig)
}

type injectConfig struct {
	hide     bool
	nullable bool
}

type hideOption bool

func (h hideOption) applyInject(c *injectConfig) { c.hide = bool(h) }

// Hide controls whether the targeted args are stripped from the
// external schema. Default true (today's HideAndInject semantics).
// Pass Hide(false) to keep the arg on the wire and have the resolver
// inspect-and-decide.
func Hide(hide bool) InjectOption { return hideOption(hide) }

type nullableOption bool

func (n nullableOption) applyInject(c *injectConfig) { c.nullable = bool(n) }

// Nullable flips the targeted args' nullability in the external
// schema. Pairs naturally with Hide(false): the caller can omit the
// arg entirely, the resolver decides what to fill in.
//
// Hide(true) + Nullable(true) is rejected at registration (the arg is
// gone from the schema; nullability is moot) — the panic surfaces at
// the user's call site, not at schema rebuild.
func Nullable(nullable bool) InjectOption { return nullableOption(nullable) }

// InjectPath returns a Transform targeting one specific schema
// location. `path` is "namespace.op.arg" (op + arg names match
// IR.Operation.Name and IR.Arg.Name verbatim — proto-pascal,
// OpenAPI-camel, etc.). The match applies to every version of the
// namespace.
//
// Resolver:
//
//	func(ctx context.Context, current any) (any, error)
//
// `current` is nil when the caller didn't send the arg (always nil
// under Hide(true)). Otherwise it's the canonical IR-typed value
// (string, float64, bool, map[string]any, …). The resolver returns
// the value to write; nil leaves the arg untouched.
//
// Coverage today matches InjectType: schema rewrite (HidePathRewrite)
// applies to every format; runtime injection runs for proto
// dispatchers only.
//
// Path miss is silent today — at every schema rebuild, paths that
// resolve fire; paths that don't are dormant. The dormant warn-log +
// resolver-return-type validation are deferred to the injector
// inventory work.
func InjectPath(path string, resolve func(ctx context.Context, current any) (any, error), opts ...InjectOption) Transform {
	cfg := injectConfig{hide: true}
	for _, o := range opts {
		o.applyInject(&cfg)
	}

	if cfg.hide && cfg.nullable {
		panic(fmt.Sprintf("gateway: InjectPath(%q): Hide(true) + Nullable(true) is rejected — arg is gone from the schema, nullability is moot", path))
	}

	var schema []SchemaRewrite
	if cfg.hide {
		schema = append(schema, HidePathRewrite{Path: path})
	}
	if cfg.nullable {
		schema = append(schema, NullablePathRewrite{Path: path})
	}
	runtime := injectPathMiddleware(path, cfg.hide, resolve)
	return Transform{Schema: schema, Runtime: runtime}
}

// injectPathMiddleware wires the runtime half of InjectPath. Returns
// nil if `path` is malformed — schema half still applies, runtime is
// a no-op.
func injectPathMiddleware(path string, hide bool, resolve func(ctx context.Context, current any) (any, error)) Middleware {
	targetNS, targetOp, targetArg, ok := splitInjectPath(path)
	if !ok {
		return nil
	}
	return func(next Handler) Handler {
		return func(ctx context.Context, req protoreflect.ProtoMessage) (protoreflect.ProtoMessage, error) {
			info, ok := dispatchOpInfoFromContext(ctx)
			if !ok || info.namespace != targetNS || info.op != targetOp {
				return next(ctx, req)
			}
			dyn, ok := req.(*dynamicpb.Message)
			if !ok {
				return next(ctx, req)
			}
			fd := dyn.Descriptor().Fields().ByName(protoreflect.Name(targetArg))
			if fd == nil {
				return next(ctx, req)
			}
			var current any
			if !hide && dyn.Has(fd) {
				current = protoToAny(fd, dyn.Get(fd))
			}
			val, err := resolveCachedPath(ctx, path, current, resolve)
			if err != nil {
				return nil, err
			}
			if val == nil {
				return next(ctx, req)
			}
			if err := setField(dyn, fd, val); err != nil {
				return nil, err
			}
			return next(ctx, req)
		}
	}
}

// resolveCachedPath memoises path-resolver output per request, keyed
// on the path string. The cache only kicks in when current is nil —
// per-call-site current makes by-path caching wrong.
func resolveCachedPath(
	ctx context.Context,
	path string,
	current any,
	resolve func(ctx context.Context, current any) (any, error),
) (any, error) {
	if current != nil {
		return resolve(ctx, current)
	}
	cache, ok := ctx.Value(injectCacheCtxKey{}).(*sync.Map)
	if !ok {
		return resolve(ctx, nil)
	}
	entry, _ := cache.LoadOrStore(cacheKey{path: path}, &cachedPathResult{})
	cr := entry.(*cachedPathResult)
	cr.once.Do(func() {
		cr.val, cr.err = resolve(ctx, nil)
	})
	return cr.val, cr.err
}

type cachedPathResult struct {
	once sync.Once
	val  any
	err  error
}

// InjectHeader returns a Transform that stamps one outbound header
// (OpenAPI HTTP dispatch) or gRPC metadata key (proto dispatch) on
// every dispatch the gateway sends. ForwardHeaders' inbound
// allowlist is unaffected — header injection writes through directly,
// and runs after forwarded headers so injectors override.
//
// Resolver:
//
//	func(ctx context.Context, current *string) (string, error)
//
// `current` is nil under Hide(true) (the inbound value is ignored);
// under Hide(false) it's the inbound HTTP header value (nil when
// absent or when the call didn't come in over HTTP). Returning ""
// skips the header for this dispatch — there's no separate skip
// sentinel, since an empty header has no useful HTTP meaning.
//
// Nullable(true) is rejected at registration: headers aren't part of
// the GraphQL schema, so nullability is moot.
//
// Coverage: every outbound dispatch (proto + OpenAPI). gRPC ingress
// (a service calling the gateway's gRPC ingress to reach another
// proto pool) also fires the injectors on its outbound leg.
func InjectHeader(name string, resolve func(ctx context.Context, current *string) (string, error), opts ...InjectOption) Transform {
	if name == "" {
		panic("gateway: InjectHeader: empty header name")
	}
	cfg := injectConfig{hide: true}
	for _, o := range opts {
		o.applyInject(&cfg)
	}
	if cfg.nullable {
		panic(fmt.Sprintf("gateway: InjectHeader(%q): Nullable(true) is rejected — headers aren't in the GraphQL schema", name))
	}
	return Transform{
		Headers: []HeaderInjector{{Name: name, Hide: cfg.hide, Fn: resolve}},
	}
}

// applyHeaderInjectors runs each injector once and returns the
// header values to stamp on the outbound dispatch. Empty results are
// skipped. The per-request cache memoises Hide(true) results across
// sibling dispatches in one GraphQL operation; Hide(false) bypasses
// the cache because `current` varies per call site (though for
// headers it's the same inbound HTTP request — kept symmetric with
// InjectType / InjectPath for one mental model).
func applyHeaderInjectors(ctx context.Context, injectors []HeaderInjector) (map[string]string, error) {
	if len(injectors) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(injectors))
	for _, inj := range injectors {
		var current *string
		if !inj.Hide {
			if r := HTTPRequestFromContext(ctx); r != nil {
				if v := r.Header.Get(inj.Name); v != "" {
					current = &v
				}
			}
		}
		v, err := resolveCachedHeader(ctx, inj.Name, current, inj.Fn)
		if err != nil {
			return nil, err
		}
		if v == "" {
			continue
		}
		out[inj.Name] = v
	}
	return out, nil
}

// resolveCachedHeader memoises header-resolver output per request,
// keyed on header name. Cache only applies when current is nil — a
// per-call-site current (Hide(false) with an inbound value) bypasses.
func resolveCachedHeader(
	ctx context.Context,
	name string,
	current *string,
	resolve func(ctx context.Context, current *string) (string, error),
) (string, error) {
	if current != nil {
		return resolve(ctx, current)
	}
	cache, ok := ctx.Value(injectCacheCtxKey{}).(*sync.Map)
	if !ok {
		return resolve(ctx, nil)
	}
	entry, _ := cache.LoadOrStore(cacheKey{header: name}, &cachedHeaderResult{})
	cr := entry.(*cachedHeaderResult)
	cr.once.Do(func() {
		cr.val, cr.err = resolve(ctx, nil)
	})
	return cr.val, cr.err
}

type cachedHeaderResult struct {
	once sync.Once
	val  string
	err  error
}
