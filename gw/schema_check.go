package gateway

import (
	"fmt"
	"sort"
	"strings"

	graphql "github.com/IodeSystems/graphql-go/v3"
	"github.com/getkin/kin-openapi/openapi3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/iodesystems/gwag/v2/gw/ir"
)

// The registration check. Before a registration creates or swaps a
// slot, the candidate's namespace is rendered alone (the candidate in
// place, with the other versions of its namespace, which change its
// type prefixes): a render failure (an invalid name, an enum problem)
// refuses it. Then each gateway-global name the render produced (type
// names and root fields) is compared against schemaNameIndex, the
// names the other namespaces render, and a name another namespace
// renders for a different type refuses it too. The cost is one
// namespace render, not a gateway render, so N registrations cost
// O(N).
//
// The full schema build still runs where it ran (assembleLocked). A
// clash the check cannot see (two different scalars of one name: all
// scalars share one identity, since x-gwag-scalar scalars are shared
// by name) fails there; assembleLocked keeps the last good schema
// live and logs and counts the error once.

// schemaNameIndex holds the gateway-global names each namespace
// renders. A name maps to an identity: the proto full name for a type
// built from proto (every namespace that imports a proto message
// shares its one GraphQL type), "scalar" for a scalar (shared by
// name), else the namespace. Two identities for one name is a clash.
// Kept per namespace and refreshed for a namespace only after one of
// its slots changed (dirty), so a check does not re-render the
// gateway. Caller holds g.mu.
type schemaNameIndex struct {
	init  bool
	byNS  map[string]map[string]string          // namespace → name → identity
	names map[string]map[string]map[string]bool // name → identity → namespaces
	dirty map[string]bool
}

// markNamesDirtyLocked records that ns's rendered names may have
// changed (a slot of ns was baked, released or its stable vN moved).
// Caller holds g.mu.
func (g *Gateway) markNamesDirtyLocked(ns string) {
	if g.names.dirty == nil {
		g.names.dirty = map[string]bool{}
	}
	g.names.dirty[ns] = true
}

// refreshNamesLocked re-renders the namespaces marked dirty and
// updates the index. The first call indexes every namespace. A
// namespace that does not render alone keeps the names it had.
// Caller holds g.mu.
func (g *Gateway) refreshNamesLocked() {
	idx := &g.names
	if !idx.init {
		idx.init = true
		idx.byNS = map[string]map[string]string{}
		idx.names = map[string]map[string]map[string]bool{}
		for k := range g.slots {
			g.markNamesDirtyLocked(k.namespace)
		}
	}
	if len(idx.dirty) == 0 {
		return
	}
	groups := map[string]map[poolKey]*slot{}
	for k, s := range g.slots {
		if idx.dirty[k.namespace] && s.ir != nil {
			if groups[k.namespace] == nil {
				groups[k.namespace] = map[poolKey]*slot{}
			}
			groups[k.namespace][k] = s
		}
	}
	for ns := range idx.dirty {
		var names map[string]string
		if group := groups[ns]; len(group) > 0 {
			n, err := g.renderNamesLocked(ns, group, g.stableVN[ns])
			if err != nil {
				continue
			}
			names = n
		}
		idx.setNS(ns, names)
	}
	idx.dirty = nil
}

func (idx *schemaNameIndex) setNS(ns string, names map[string]string) {
	for name, id := range idx.byNS[ns] {
		ids := idx.names[name]
		delete(ids[id], ns)
		if len(ids[id]) == 0 {
			delete(ids, id)
		}
		if len(ids) == 0 {
			delete(idx.names, name)
		}
	}
	if len(names) == 0 {
		delete(idx.byNS, ns)
		return
	}
	idx.byNS[ns] = names
	for name, id := range names {
		ids := idx.names[name]
		if ids == nil {
			ids = map[string]map[string]bool{}
			idx.names[name] = ids
		}
		if ids[id] == nil {
			ids[id] = map[string]bool{}
		}
		ids[id][ns] = true
	}
}

// clash returns a namespace other than the skipped ones that renders
// name for an identity other than id, or "".
func (idx *schemaNameIndex) clash(name, id string, skip map[string]bool) string {
	var hit []string
	for other, nss := range idx.names[name] {
		if other == id {
			continue
		}
		for ns := range nss {
			if !skip[ns] {
				hit = append(hit, ns)
			}
		}
	}
	if len(hit) == 0 {
		return ""
	}
	sort.Strings(hit)
	return hit[0]
}

// namespaceSlotsLocked returns the live slots of ns with baked IR.
// Caller holds g.mu.
func (g *Gateway) namespaceSlotsLocked(ns string) map[poolKey]*slot {
	out := map[poolKey]*slot{}
	for k, s := range g.slots {
		if k.namespace == ns && s.ir != nil {
			out[k] = s
		}
	}
	return out
}

// renderNamesLocked renders the slots of one namespace alone, as they
// render in the gateway schema (same type prefixes, enum renames,
// hidden types and stable alias), and returns the gateway-global
// names the render adds, each with its identity. A render error is
// the error the full build would give for this namespace. Caller
// holds g.mu.
func (g *Gateway) renderNamesLocked(ns string, group map[poolKey]*slot, stableVN int) (map[string]string, error) {
	var svcs []*ir.Service
	protoIDs := map[string]string{}
	for _, s := range group {
		svcs = append(svcs, s.ir...)
		for _, svc := range s.ir {
			if svc.OriginKind != ir.KindProto {
				continue
			}
			for full := range svc.Types {
				n := exportedName(full)
				protoIDs[n] = "proto:" + full
				protoIDs[n+"_Input"] = "proto:" + full
			}
		}
	}
	var stableSnap map[string]int
	if stableVN > 0 && g.effectiveAllowedTiers().Stable {
		stableSnap = map[string]int{ns: stableVN}
	}
	schema, _, err := g.renderSchemaLocked(group, svcs, ir.NewDispatchRegistry(), stableSnap)
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	nsID := "ns:" + ns
	for name, t := range schema.TypeMap() {
		if baseSchemaType(name) {
			continue
		}
		switch {
		case protoIDs[name] != "":
			names[name] = protoIDs[name]
		case isScalar(t):
			names[name] = "scalar"
		default:
			names[name] = nsID
		}
	}
	for root, obj := range map[string]*graphql.Object{
		"Query":        schema.QueryType(),
		"Mutation":     schema.MutationType(),
		"Subscription": schema.SubscriptionType(),
	} {
		if obj == nil {
			continue
		}
		for f := range obj.Fields() {
			if root == "Query" && f == "_status" {
				continue
			}
			names[root+"."+f] = nsID
		}
	}
	return names, nil
}

func isScalar(t graphql.Type) bool {
	_, ok := t.(*graphql.Scalar)
	return ok
}

// baseSchemaType reports a type every gateway schema has whatever is
// registered: the root types, the built-in and introspection types,
// and Upload.
func baseSchemaType(name string) bool {
	if strings.HasPrefix(name, "__") {
		return true
	}
	switch name {
	case "Query", "Mutation", "Subscription", "String", "Int", "Float", "Boolean", "ID", "Upload":
		return true
	}
	return false
}

// checkCandidateLocked builds the candidate slot a registration would
// create at key (attach sets its per-kind handle) and checks it
// (checkSchemaLocked). nil attach skips the check. Caller holds g.mu.
func (g *Gateway) checkCandidateLocked(kind slotKind, key poolKey, deprecation string, bindings []ir.ChannelBinding, attach func(*slot) error) error {
	if attach == nil {
		return nil
	}
	c := &slot{key: key, kind: kind, deprecationReason: deprecation, channelBindings: bindings}
	if err := attach(c); err != nil {
		return err
	}
	return g.checkSchemaLocked(c)
}

// checkSchemaLocked checks cands (slots to be created or swapped in at
// their keys) without touching g.slots, the dispatchers or the live
// schema: each candidate namespace must render alone with its
// candidates in place, and no name it renders may be rendered for a
// different type by another namespace or by another candidate
// namespace. Caller holds g.mu.
func (g *Gateway) checkSchemaLocked(cands ...*slot) error {
	if len(cands) == 0 {
		return nil
	}
	g.refreshNamesLocked()
	stable := g.effectiveAllowedTiers().Stable
	groups := map[string]map[poolKey]*slot{}
	stableVN := map[string]int{}
	for _, c := range cands {
		g.bakeSlotIRLocked(c)
		ns := c.key.namespace
		if groups[ns] == nil {
			groups[ns] = g.namespaceSlotsLocked(ns)
			stableVN[ns] = g.stableVN[ns]
		}
		groups[ns][c.key] = c
		// The registration advances stable to its vN; render the
		// alias it will get.
		if n := ir.ParseRuntimeVersionN(c.key.version); stable && n > stableVN[ns] {
			stableVN[ns] = n
		}
	}
	nss := make([]string, 0, len(groups))
	for ns := range groups {
		nss = append(nss, ns)
	}
	sort.Strings(nss)
	// The candidate namespaces replace their own names; names of
	// candidate namespaces already checked join the index view.
	skip := map[string]bool{}
	for _, ns := range nss {
		skip[ns] = true
	}
	checked := schemaNameIndex{byNS: map[string]map[string]string{}, names: map[string]map[string]map[string]bool{}}
	for _, ns := range nss {
		names, err := g.renderNamesLocked(ns, groups[ns], stableVN[ns])
		if err != nil {
			return schemaCheckError(cands, err)
		}
		keys := make([]string, 0, len(names))
		for name := range names {
			keys = append(keys, name)
		}
		sort.Strings(keys)
		for _, name := range keys {
			id := names[name]
			other := g.names.clash(name, id, skip)
			if other == "" {
				other = checked.clash(name, id, nil)
			}
			if other != "" {
				return schemaCheckError(cands, fmt.Errorf("GraphQL name %s is already used by namespace %s for a different type", name, other))
			}
		}
		checked.setNS(ns, names)
	}
	return nil
}

func schemaCheckError(cands []*slot, err error) error {
	keys := make([]string, 0, len(cands))
	for _, c := range cands {
		keys = append(keys, c.key.namespace+"/"+c.key.version)
	}
	return fmt.Errorf("%s: schema does not build, registration refused: %w", strings.Join(keys, ", "), err)
}

// checkPreparedLocked checks every binding of a Register that would
// create or swap a slot, all together (v1 and v2 in one request are
// checked side by side). Run before anything is joined or, in a
// cluster, written to the registry KV, so a binding that cannot build
// is refused on the node that received it and never reaches the other
// nodes. Waiting bindings are checked too: the join would refuse them
// later. Caller holds g.mu.
func (g *Gateway) checkPreparedLocked(prep []preparedBinding) error {
	var cands []*slot
	for i := range prep {
		p := &prep[i]
		key := p.key()
		if s := g.slots[key]; s != nil && key.version != "unstable" {
			// A join to an identical slot builds the same schema; a
			// different one is refused by the vN lock.
			continue
		}
		c := &slot{key: key, kind: p.kind(), deprecationReason: g.deprecation[key]}
		switch {
		case p.isGraphQL:
			c.graphql = &graphQLSource{rawIntrospection: p.graphqlIntrospection}
		case p.isOpenAPI:
			loader := openapi3.NewLoader()
			loader.IsExternalRefsAllowed = false
			doc, err := loader.LoadFromData(p.openAPISpec)
			if err != nil {
				return fmt.Errorf("controlplane: %s/%s: parse openapi spec: %w", key.namespace, key.version, err)
			}
			c.openapi = &openAPISource{doc: doc}
		default:
			bindings, err := extractChannelBindings(p.fileDesc)
			if err != nil {
				return fmt.Errorf("controlplane: %s/%s: %w", key.namespace, key.version, err)
			}
			c.channelBindings = bindings
			c.proto = &pool{file: p.fileDesc}
		}
		cands = append(cands, c)
	}
	if err := g.checkSchemaLocked(cands...); err != nil {
		return status.Errorf(codes.InvalidArgument, "controlplane: %v", err)
	}
	return nil
}
