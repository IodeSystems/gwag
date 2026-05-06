package gateway

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// SchemaProtoHandler returns an http.Handler for GET /schema/proto.
// It emits a FileDescriptorSet for selected services with the
// gateway's transformations applied — hidden fields stripped,
// internal namespaces skipped. NOT the raw registered protos:
// clients that codegen from this output get the contract surface as
// the gateway actually exposes it.
//
// Query parameters:
//   - service=ns[:ver][,ns[:ver]...]  — selector, comma separated.
//                                       Missing version → all versions
//                                       of that namespace. Missing
//                                       service param → all services.
//
// Response: application/protobuf, the marshalled FileDescriptorSet.
//
// Caveats:
//   - Hidden fields are stripped from message descriptors. Field
//     numbers of remaining fields are preserved (wire-compatible with
//     non-hidden fields), but the resulting messages cannot be
//     wire-compatibly produced by callers using the *original* proto.
//   - Cross-file imports are emitted only for files that themselves
//     have at least one selected/non-internal service. If a hidden
//     namespace was the source of a referenced type, the reference
//     remains in the descriptor — callers may see dangling type
//     references. (Acceptable for v1; refine when concrete cases
//     appear.)
func (g *Gateway) SchemaProtoHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		selectors, err := parseProtoSelectors(r.URL.Query().Get("service"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		g.mu.Lock()
		hides := map[protoreflect.FullName]bool{}
		for _, p := range g.pairs {
			for _, t := range p.Hides {
				hides[t] = true
			}
		}
		// Collect matching pools' file descriptors.
		matched := []*pool{}
		for _, p := range g.pools {
			if g.isInternal(p.key.namespace) {
				continue
			}
			if !matchSelectors(p.key, selectors) {
				continue
			}
			matched = append(matched, p)
		}
		g.mu.Unlock()

		// Walk + transform + dedupe by file path.
		fds := &descriptorpb.FileDescriptorSet{}
		seen := map[string]bool{}
		for _, p := range matched {
			collectTransformedFiles(p.file, hides, fds, seen)
		}
		// Stable ordering for deterministic bytes.
		sort.Slice(fds.File, func(i, j int) bool {
			return fds.File[i].GetName() < fds.File[j].GetName()
		})

		out, err := proto.MarshalOptions{Deterministic: true}.Marshal(fds)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/protobuf")
		w.Header().Set("Content-Disposition", `attachment; filename="services.fds"`)
		_, _ = w.Write(out)
	})
}

// serviceSelector picks a (namespace, optional version) for export.
type serviceSelector struct {
	namespace string
	version   string // "" = any version
}

// schemaFilter wraps a parsed selector list and exposes the per-source
// match shapes the schema-build helpers need. An empty selector list
// matches everything (no filter active). Used by /schema/graphql to
// build a per-request filtered schema; the cached g.schema is built
// with an empty filter (= everything).
type schemaFilter struct {
	selectors []serviceSelector
}

func (f schemaFilter) matchPool(k poolKey) bool { return matchSelectors(k, f.selectors) }

func (f schemaFilter) matchNS(ns string) bool { return matchOpenAPISelectors(ns, f.selectors) }

func parseProtoSelectors(raw string) ([]serviceSelector, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	out := make([]serviceSelector, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		ns, ver, _ := strings.Cut(p, ":")
		ns = strings.TrimSpace(ns)
		ver = strings.TrimSpace(ver)
		if err := validateNS(ns); err != nil {
			return nil, fmt.Errorf("service selector %q: %w", p, err)
		}
		if ver != "" {
			if _, _, err := parseVersion(ver); err != nil {
				return nil, fmt.Errorf("service selector %q: %w", p, err)
			}
		}
		out = append(out, serviceSelector{namespace: ns, version: ver})
	}
	return out, nil
}

func matchSelectors(k poolKey, sels []serviceSelector) bool {
	if len(sels) == 0 {
		return true
	}
	for _, s := range sels {
		if s.namespace != k.namespace {
			continue
		}
		if s.version == "" || s.version == k.version {
			return true
		}
	}
	return false
}

// collectTransformedFiles walks fd plus its transitive imports and
// adds (transformed) FileDescriptorProtos into fds, deduped by path.
func collectTransformedFiles(
	fd protoreflect.FileDescriptor,
	hides map[protoreflect.FullName]bool,
	fds *descriptorpb.FileDescriptorSet,
	seen map[string]bool,
) {
	if seen[string(fd.Path())] {
		return
	}
	seen[string(fd.Path())] = true
	for i := 0; i < fd.Imports().Len(); i++ {
		collectTransformedFiles(fd.Imports().Get(i).FileDescriptor, hides, fds, seen)
	}
	fds.File = append(fds.File, transformFileDescriptor(fd, hides))
}

// transformFileDescriptor returns a FileDescriptorProto with hidden
// field types stripped from every message. Field numbers are
// preserved on the remaining fields. Method definitions are kept
// intact (the gateway's GraphQL filtering applies only at the
// GraphQL surface — direct gRPC callers still see them).
func transformFileDescriptor(
	fd protoreflect.FileDescriptor,
	hides map[protoreflect.FullName]bool,
) *descriptorpb.FileDescriptorProto {
	src := protodesc.ToFileDescriptorProto(fd)
	if len(hides) == 0 {
		return src
	}
	for _, m := range src.MessageType {
		stripHiddenFields(m, hides)
	}
	return src
}

// SchemaOpenAPIHandler returns an http.Handler for GET /schema/openapi.
// It re-emits each ingested OpenAPI spec keyed by namespace, optionally
// filtered by the selector.
//
// Query parameters:
//   - service=ns[,ns,...]  — comma-separated namespaces. Empty → all.
//                            (Version qualifier accepted but ignored;
//                            OpenAPI specs aren't versioned today.)
//
// Response: application/json, an object {"<ns>": <spec>, ...}.
func (g *Gateway) SchemaOpenAPIHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		selectors, err := parseProtoSelectors(r.URL.Query().Get("service"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		g.mu.Lock()
		out := map[string]any{}
		for ns, src := range g.openAPISources {
			if g.isInternal(ns) {
				continue
			}
			if !matchOpenAPISelectors(ns, selectors) {
				continue
			}
			out[ns] = src.doc
		}
		g.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if err := writeJSON(w, out); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
}

func matchOpenAPISelectors(ns string, sels []serviceSelector) bool {
	if len(sels) == 0 {
		return true
	}
	for _, s := range sels {
		if s.namespace == ns {
			return true
		}
	}
	return false
}

// stripHiddenFields removes fields whose type_name resolves to a
// hidden FullName. Recurses into nested types.
func stripHiddenFields(
	m *descriptorpb.DescriptorProto,
	hides map[protoreflect.FullName]bool,
) {
	out := m.Field[:0]
	for _, f := range m.Field {
		if f.TypeName != nil {
			tn := strings.TrimPrefix(*f.TypeName, ".")
			if hides[protoreflect.FullName(tn)] {
				continue
			}
		}
		out = append(out, f)
	}
	// Re-slice to drop the trailing now-unused entries.
	for i := len(out); i < len(m.Field); i++ {
		m.Field[i] = nil
	}
	m.Field = out
	for _, nested := range m.NestedType {
		stripHiddenFields(nested, hides)
	}
}
