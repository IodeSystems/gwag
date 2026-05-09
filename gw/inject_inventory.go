package gateway

import (
	"fmt"
	"runtime"

	"github.com/iodesystems/go-api-gateway/gw/ir"
)

// InjectorKind identifies which constructor produced an InjectorRecord.
type InjectorKind string

const (
	InjectorKindType   InjectorKind = "type"
	InjectorKindPath   InjectorKind = "path"
	InjectorKindHeader InjectorKind = "header"
)

// InjectorState captures the inventory entry's resolution against the
// live schema at the moment InjectorInventory() was called.
type InjectorState string

const (
	// InjectorStateActive: at least one schema landing matches (for
	// type-keyed and path-keyed) or — for header injectors — registration
	// is well-formed.
	InjectorStateActive InjectorState = "active"

	// InjectorStateDormant: no schema landing currently resolves. The
	// rule activates if a future schema rebuild brings the target into
	// existence; harmless otherwise.
	InjectorStateDormant InjectorState = "dormant"
)

// InjectorRecord is the registration-time view of one InjectType /
// InjectPath / InjectHeader call. Captured by the Inject* constructors
// and surfaced through Transform.Inventory + Gateway.InjectorInventory.
type InjectorRecord struct {
	Kind InjectorKind

	// Exactly one of the following identifies the rule, depending on
	// Kind:
	//   - Kind=InjectorKindType   → TypeName (IR-named-type, proto FullName for proto messages)
	//   - Kind=InjectorKindPath   → Path ("namespace.op.arg")
	//   - Kind=InjectorKindHeader → HeaderName (literal HTTP header / gRPC metadata key)
	TypeName   string
	Path       string
	HeaderName string

	// Hide reflects the visibility flag at registration. true means the
	// arg/header is stripped from the external schema; false means the
	// resolver inspects what the caller sent.
	Hide bool

	// Nullable reflects the Nullable(true) option (paired with
	// Hide(false) for InjectType/InjectPath; rejected for InjectHeader).
	Nullable bool

	// RegisteredAt is the user's call site (file:line + function name)
	// captured via runtime.Caller at construction time.
	RegisteredAt InjectorFrame
}

// InjectorFrame is one captured stack frame.
type InjectorFrame struct {
	File     string
	Line     int
	Function string
}

func (f InjectorFrame) String() string {
	if f.File == "" {
		return ""
	}
	if f.Function != "" {
		return fmt.Sprintf("%s:%d (%s)", f.File, f.Line, f.Function)
	}
	return fmt.Sprintf("%s:%d", f.File, f.Line)
}

// captureInjectorFrame walks `skip` frames above the caller to find the
// user's registration site. Returns a zeroed frame on lookup failure.
func captureInjectorFrame(skip int) InjectorFrame {
	pc, file, line, ok := runtime.Caller(skip + 1)
	if !ok {
		return InjectorFrame{}
	}
	fn := ""
	if f := runtime.FuncForPC(pc); f != nil {
		fn = f.Name()
	}
	return InjectorFrame{File: file, Line: line, Function: fn}
}

// InjectorLanding is one concrete arg/field/header that an inventory
// entry currently affects. Renderable as a row in the admin UI.
type InjectorLanding struct {
	// Kind is "arg", "field", or "header".
	Kind string

	// For Kind=="arg": Namespace, Version, Op, ArgName populated.
	// For Kind=="field": Namespace, Version, TypeName, FieldName populated.
	// For Kind=="header": HeaderName populated.
	Namespace string
	Version   string
	Op        string
	TypeName  string
	FieldName string
	ArgName   string
	HeaderName string
}

// InjectorEntry is one row of the admin inventory: an InjectorRecord
// plus its current schema landings + state.
type InjectorEntry struct {
	InjectorRecord
	State    InjectorState
	Landings []InjectorLanding
}

// InjectorInventory enumerates every Inject* registration on this
// gateway, paired with where it currently lands in the live (un-rewritten)
// IR. Powers the admin inventory endpoint + UI tab.
//
// Walks pools / OpenAPI sources / GraphQL ingest sources to compute the
// pre-rewrite IR (so HidePath/HideType "landings" are visible — once
// rewrites apply, hidden args are gone and the inventory would lose its
// answer to "what got hidden, where?"). Caller need not hold g.mu.
func (g *Gateway) InjectorInventory() ([]InjectorEntry, error) {
	g.mu.Lock()
	svcs, err := g.collectIRRawLocked()
	records := collectInjectorRecords(g.transforms)
	g.mu.Unlock()
	if err != nil {
		return nil, err
	}

	entries := make([]InjectorEntry, 0, len(records))
	for _, rec := range records {
		entry := InjectorEntry{InjectorRecord: rec}
		switch rec.Kind {
		case InjectorKindType:
			entry.Landings = landingsForType(svcs, rec.TypeName)
		case InjectorKindPath:
			entry.Landings = landingsForPath(svcs, rec.Path)
		case InjectorKindHeader:
			entry.Landings = []InjectorLanding{{Kind: "header", HeaderName: rec.HeaderName}}
		}
		entry.State = InjectorStateDormant
		if len(entry.Landings) > 0 {
			entry.State = InjectorStateActive
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// collectInjectorRecords flattens every Transform.Inventory entry in
// registration order. Caller holds g.mu.
func collectInjectorRecords(transforms []Transform) []InjectorRecord {
	var out []InjectorRecord
	for _, tx := range transforms {
		out = append(out, tx.Inventory...)
	}
	return out
}

// collectIRRawLocked walks proto pools + OpenAPI + GraphQL sources and
// produces the raw (no rewrites applied) IR snapshot used by the
// inventory. Caller holds g.mu. Mirrors gatewayServicesAsIR but skips
// applySchemaRewrites + PopulateSchemaIDs (neither is needed for
// landing lookup, and rewrites would erase what we're trying to surface).
func (g *Gateway) collectIRRawLocked() ([]*ir.Service, error) {
	out := []*ir.Service{}

	for k, s := range g.slots {
		switch s.kind {
		case slotKindProto:
			if s.proto == nil {
				continue
			}
			svcs := ir.IngestProto(s.proto.file)
			for _, svc := range svcs {
				svc.Namespace = k.namespace
				svc.Version = k.version
				svc.Internal = g.isInternal(k.namespace)
				out = append(out, svc)
			}
		case slotKindOpenAPI:
			if s.openapi == nil {
				continue
			}
			svc := ir.IngestOpenAPI(s.openapi.doc)
			svc.Namespace = k.namespace
			svc.Version = k.version
			svc.Internal = g.isInternal(k.namespace)
			out = append(out, svc)
		case slotKindGraphQL:
			if s.graphql == nil {
				continue
			}
			svc, err := ir.IngestGraphQL(s.graphql.rawIntrospection)
			if err != nil {
				continue
			}
			svc.Namespace = k.namespace
			svc.Version = k.version
			svc.Internal = g.isInternal(k.namespace)
			out = append(out, svc)
		}
	}
	return out, nil
}

// landingsForType returns every (svc, op, arg) and (svc, type, field)
// in svcs whose IR-named type matches name. Walks the same shapes
// NullableTypeRewrite walks; sibling logic.
func landingsForType(svcs []*ir.Service, name string) []InjectorLanding {
	if name == "" {
		return nil
	}
	var out []InjectorLanding
	for _, svc := range svcs {
		// Args on top-level operations.
		for _, op := range svc.Operations {
			for _, a := range op.Args {
				if a.Type.IsNamed() && a.Type.Named == name {
					out = append(out, InjectorLanding{
						Kind: "arg", Namespace: svc.Namespace, Version: svc.Version,
						Op: op.Name, ArgName: a.Name,
					})
				}
			}
		}
		// Args on grouped operations.
		for _, grp := range svc.Groups {
			collectTypeArgsInGroup(grp, svc, name, &out)
		}
		// Fields on object/input types.
		for _, t := range svc.Types {
			if t.TypeKind != ir.TypeObject && t.TypeKind != ir.TypeInput {
				continue
			}
			for _, f := range t.Fields {
				if f.Type.IsNamed() && f.Type.Named == name {
					out = append(out, InjectorLanding{
						Kind: "field", Namespace: svc.Namespace, Version: svc.Version,
						TypeName: t.Name, FieldName: f.Name,
					})
				}
			}
		}
	}
	return out
}

func collectTypeArgsInGroup(grp *ir.OperationGroup, svc *ir.Service, name string, out *[]InjectorLanding) {
	for _, op := range grp.Operations {
		for _, a := range op.Args {
			if a.Type.IsNamed() && a.Type.Named == name {
				*out = append(*out, InjectorLanding{
					Kind: "arg", Namespace: svc.Namespace, Version: svc.Version,
					Op: op.Name, ArgName: a.Name,
				})
			}
		}
	}
	for _, sub := range grp.Groups {
		collectTypeArgsInGroup(sub, svc, name, out)
	}
}

// landingsForPath returns the (svc, op, arg) triples that "ns.op.arg"
// resolves to today, across every version of the namespace.
func landingsForPath(svcs []*ir.Service, path string) []InjectorLanding {
	ns, opName, arg, ok := splitInjectPath(path)
	if !ok {
		return nil
	}
	var out []InjectorLanding
	for _, svc := range svcs {
		if svc.Namespace != ns {
			continue
		}
		collectPathArgInOps(svc.Operations, svc, opName, arg, &out)
		for _, grp := range svc.Groups {
			collectPathArgInGroup(grp, svc, opName, arg, &out)
		}
	}
	return out
}

func collectPathArgInOps(ops []*ir.Operation, svc *ir.Service, opName, arg string, out *[]InjectorLanding) {
	for _, op := range ops {
		if op.Name != opName {
			continue
		}
		for _, a := range op.Args {
			if a.Name == arg {
				*out = append(*out, InjectorLanding{
					Kind: "arg", Namespace: svc.Namespace, Version: svc.Version,
					Op: op.Name, ArgName: a.Name,
				})
			}
		}
	}
}

func collectPathArgInGroup(grp *ir.OperationGroup, svc *ir.Service, opName, arg string, out *[]InjectorLanding) {
	collectPathArgInOps(grp.Operations, svc, opName, arg, out)
	for _, sub := range grp.Groups {
		collectPathArgInGroup(sub, svc, opName, arg, out)
	}
}
