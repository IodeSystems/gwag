package gateway

import "context"

// dispatchOpInfo carries the namespace/version/op name of the
// operation being dispatched through the runtime middleware chain.
// Path-keyed Transforms (InjectPath) consult it to decide whether to
// fire on a given request; type-keyed Transforms (InjectType) ignore
// it.
type dispatchOpInfo struct {
	namespace string
	version   string
	op        string
}

type dispatchOpInfoCtxKey struct{}

func withDispatchOpInfo(ctx context.Context, namespace, version, op string) context.Context {
	return context.WithValue(ctx, dispatchOpInfoCtxKey{}, dispatchOpInfo{namespace: namespace, version: version, op: op})
}

func dispatchOpInfoFromContext(ctx context.Context) (dispatchOpInfo, bool) {
	v, ok := ctx.Value(dispatchOpInfoCtxKey{}).(dispatchOpInfo)
	return v, ok
}
