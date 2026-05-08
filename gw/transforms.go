package gateway

import (
	"reflect"

	"google.golang.org/protobuf/proto"

	"github.com/iodesystems/go-api-gateway/gw/ir"
)

var protoMessageType = reflect.TypeOf((*proto.Message)(nil)).Elem()

// HideType returns a SchemaRewrite that strips every field/arg whose
// IR-named type matches Go type T from every Object/Input type and
// Operation.Args across the schema.
//
// Resolution of T → IR type name:
//   - Pointer-to-proto-message (e.g. *authpb.Context): the message's
//     proto FullName, which equals the IR Type.Named for proto-origin
//     services.
//   - Other Go types: the package-qualified name (e.g. "x/y/pkg.Foo").
//     Won't match anything in IR until a non-proto path registers a
//     binding for the same name.
func HideType[T any]() SchemaRewrite {
	return HideTypeRewrite{Name: irNameForGoType[T]()}
}

// HideTypeRewrite is the concrete SchemaRewrite returned by HideType.
// Exported so renderers operating below the IR (e.g. the proto FDS
// exporter post-processing the same-kind shortcut) can recover the
// hidden type name.
type HideTypeRewrite struct {
	Name string
}

func (h HideTypeRewrite) apply(svcs []*ir.Service) {
	if h.Name == "" {
		return
	}
	ir.Hides(svcs, map[string]bool{h.Name: true})
}

// irNameForGoType derives the IR-named-type string for Go type T.
func irNameForGoType[T any]() string {
	rt := reflect.TypeOf((*T)(nil)).Elem()
	pt := rt
	if pt.Kind() != reflect.Ptr {
		pt = reflect.PointerTo(pt)
	}
	if pt.Implements(protoMessageType) {
		zero := reflect.New(pt.Elem()).Interface().(proto.Message)
		return string(zero.ProtoReflect().Descriptor().FullName())
	}
	if rt.Kind() == reflect.Ptr {
		rt = rt.Elem()
	}
	if pkg := rt.PkgPath(); pkg != "" {
		return pkg + "." + rt.Name()
	}
	return rt.Name()
}

// applySchemaRewrites runs every SchemaRewrite from the gateway's
// Transforms over `svcs` in registration order. Caller holds g.mu.
func (g *Gateway) applySchemaRewrites(svcs []*ir.Service) {
	for _, tx := range g.transforms {
		for _, rw := range tx.Schema {
			rw.apply(svcs)
		}
	}
}

// hiddenTypeNames returns every IR type name registered via HideType
// across the gateway's Transforms. Used by the proto FDS exporter and
// the proto IRTypeBuilder, which both work outside the IR-service walk
// that applySchemaRewrites covers. Caller holds g.mu.
func (g *Gateway) hiddenTypeNames() []string {
	var out []string
	for _, tx := range g.transforms {
		for _, rw := range tx.Schema {
			if h, ok := rw.(HideTypeRewrite); ok && h.Name != "" {
				out = append(out, h.Name)
			}
		}
	}
	return out
}
