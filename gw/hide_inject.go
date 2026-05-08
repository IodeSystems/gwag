package gateway

import (
	"context"
	"sync"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// HideAndInject hides every input field whose proto type matches T from
// the external schema, and at runtime populates those fields by calling
// resolve(ctx). The two halves are paired by construction so they
// cannot drift; resolve is called once per request per type and cached
// on the request context.
func HideAndInject[T proto.Message](resolve func(context.Context) (T, error)) Transform {
	var zero T
	target := zero.ProtoReflect().Descriptor().FullName()
	return Transform{
		Schema: []SchemaRewrite{HideTypeRewrite{Name: string(target)}},
		Runtime: injectFieldsOfType(target, func(ctx context.Context) (proto.Message, error) {
			return resolve(ctx)
		}),
	}
}

type cacheKey struct{ name protoreflect.FullName }

type cachedResult struct {
	once sync.Once
	val  proto.Message
	err  error
}

// injectFieldsOfType returns Middleware that finds any field on the
// incoming request matching `target`'s message type and populates it
// from `resolve`, memoising per request context.
func injectFieldsOfType(target protoreflect.FullName, resolve func(context.Context) (proto.Message, error)) Middleware {
	return func(next Handler) Handler {
		return func(ctx context.Context, req protoreflect.ProtoMessage) (protoreflect.ProtoMessage, error) {
			if err := injectInto(ctx, req, target, resolve); err != nil {
				return nil, err
			}
			return next(ctx, req)
		}
	}
}

func injectInto(ctx context.Context, req protoreflect.ProtoMessage, target protoreflect.FullName, resolve func(context.Context) (proto.Message, error)) error {
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
		val, err := resolveCached(ctx, target, resolve)
		if err != nil {
			return err
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

func resolveCached(ctx context.Context, target protoreflect.FullName, resolve func(context.Context) (proto.Message, error)) (proto.Message, error) {
	cache, ok := ctx.Value(injectCacheCtxKey{}).(*sync.Map)
	if !ok {
		return resolve(ctx)
	}
	entry, _ := cache.LoadOrStore(cacheKey{name: target}, &cachedResult{})
	cr := entry.(*cachedResult)
	cr.once.Do(func() {
		cr.val, cr.err = resolve(ctx)
	})
	return cr.val, cr.err
}

type injectCacheCtxKey struct{}

func withInjectCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, injectCacheCtxKey{}, &sync.Map{})
}
