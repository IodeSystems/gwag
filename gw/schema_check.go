package gateway

import (
	"fmt"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/iodesystems/gwag/v2/gw/ir"
)

// checkCandidateLocked builds the candidate slot a registration would
// create at key (attach sets its per-kind handle) and checks that the
// gateway schema builds with it. nil attach skips the check. Caller
// holds g.mu.
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

// checkSchemaLocked builds the gateway schema with cands in place of
// the slots at their keys, without touching g.slots, the dispatchers
// or the live schema, and returns the build error. Registration calls
// it before it swaps anything in, so a service whose schema cannot
// build (an invalid name, a type clash with another service) is
// refused and the gateway keeps serving what it served. The build is
// the same render assembleLocked runs, over an empty dispatcher
// registry. Caller holds g.mu.
func (g *Gateway) checkSchemaLocked(cands ...*slot) error {
	if len(cands) == 0 {
		return nil
	}
	overlay := make(map[poolKey]*slot, len(g.slots)+len(cands))
	for k, s := range g.slots {
		overlay[k] = s
	}
	var stableSnap map[string]int
	stable := g.effectiveAllowedTiers().Stable
	if stable {
		stableSnap = g.stableSnapshotLocked()
	}
	for _, c := range cands {
		g.bakeSlotIRLocked(c)
		overlay[c.key] = c
		// The registration advances stable to its vN; render the
		// alias it will get.
		if n := ir.ParseRuntimeVersionN(c.key.version); stable && n > 0 && n > stableSnap[c.key.namespace] {
			if stableSnap == nil {
				stableSnap = map[string]int{}
			}
			stableSnap[c.key.namespace] = n
		}
	}
	svcs := make([]*ir.Service, 0, len(overlay))
	for _, s := range overlay {
		svcs = append(svcs, s.ir...)
	}
	if _, _, err := g.renderSchemaLocked(overlay, svcs, ir.NewDispatchRegistry(), stableSnap); err != nil {
		return schemaCheckError(cands, err)
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

// checkPreparedLocked checks that the schema builds with every binding
// of a Register that would create or swap a slot, all together (v1
// and v2 in one request are checked side by side). Run before anything
// is joined or, in a cluster, written to the registry KV, so a binding
// that cannot build is refused on the node that received it and never
// reaches the other nodes. Waiting bindings are checked too: the join
// would refuse them later. Caller holds g.mu.
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
