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

	var schema []SchemaRewrite
	if cfg.hide {
		schema = append(schema, HideTypeRewrite{Name: irName})
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

type cacheKey struct{ name protoreflect.FullName }

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
	hide bool
}

type hideOption bool

func (h hideOption) applyInject(c *injectConfig) { c.hide = bool(h) }

// Hide controls whether the targeted args are stripped from the
// external schema. Default true (today's HideAndInject semantics).
// Pass Hide(false) to keep the arg on the wire and have the resolver
// inspect-and-decide.
func Hide(hide bool) InjectOption { return hideOption(hide) }
