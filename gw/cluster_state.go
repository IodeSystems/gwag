package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"google.golang.org/grpc/codes"

	"github.com/iodesystems/gwag/v2/gw/internal/srserve"
)

// Cluster state export and import (cutover S4, docs/cutover.md).
//
// Before green serves, it needs the state operators set on blue. One
// JSON document (clusterState, "v": 1) carries it:
//
//	stable marks          gwag-stable
//	deprecated marks      gwag-deprecated
//	app and system keys   gwag-app-keys, with the stored hashes: the
//	                      same keys work on green
//	external dependents   gwag-external-dependents
//	limits                gwag-limits: operator rate limits, quotas,
//	                      costs (upstream_limits.go)
//	Schema Registry       gwag-schemas, every context with its ids,
//	                      the default context included; the control
//	                      plane subject is left out (each gateway
//	                      registers its own)
//
// Not in it: registrations (services register with both clusters),
// the MCP include/exclude config, drains, notifications, peers, event
// streams, quota counters and usage rows.
//
// Export reads every bucket from its stream leader and refuses unless
// the cluster is frozen (cluster_freeze.go), or allowUnfrozen is set.
// Import is idempotent: an item green already holds with the same
// value is unchanged. An item green holds with another value is a
// conflict; with conflicts nothing is written unless overwrite, which
// takes the document's value. Schema Registry conflicts (a schema id
// or subject version green allocated differently) cannot be
// overwritten: ids are what Kafka messages carry.

const clusterStateVersion = 1

// clusterState is the document. Field names are camelCase; the
// stored records (keys, external dependents) keep their KV form.
type clusterState struct {
	V                  int                 `json:"v" doc:"Document version: 1."`
	ExportedAt         string              `json:"exportedAt,omitempty" doc:"RFC 3339."`
	ExportedBy         string              `json:"exportedBy,omitempty" doc:"Authenticated identity of the export call."`
	Node               string              `json:"node,omitempty" doc:"Cluster node name of the gateway that exported."`
	Frozen             bool                `json:"frozen,omitempty" doc:"The source was frozen during the export."`
	Stable             []cutoverStable     `json:"stable"`
	Deprecated         []cutoverDeprecated `json:"deprecated"`
	Keys               []appKeyRecord      `json:"keys" doc:"App and system keys as stored: id, kind, namespaces, SHA-256 hash of the key, expiry. The keys themselves are not stored anywhere."`
	ExternalDependents []externalDependent `json:"externalDependents"`
	Limits             []limitRecord       `json:"limits,omitempty" doc:"Operator rate limits, quotas and costs."`
	SchemaRegistry     *cutoverRegistry    `json:"schemaRegistry,omitempty" doc:"Absent when the source serves no Schema Registry."`
	Warnings           []string            `json:"warnings,omitempty"`
}

type cutoverStable struct {
	Namespace string `json:"namespace"`
	VN        int    `json:"vN"`
}

type cutoverDeprecated struct {
	Namespace string `json:"namespace"`
	Version   string `json:"version"`
	Reason    string `json:"reason"`
}

type cutoverRegistry struct {
	Contexts []cutoverRegistryContext `json:"contexts"`
}

type cutoverRegistryContext struct {
	Name     string                  `json:"name" doc:"Context name: . for the default context."`
	Schemas  []srserve.ImportSchema  `json:"schemas"`
	Versions []srserve.ImportVersion `json:"versions"`
	Global   string                  `json:"global,omitempty" doc:"The context's compatibility level, when set."`
	Levels   []cutoverLevel          `json:"levels"`
}

type cutoverLevel struct {
	Subject string `json:"subject"`
	Level   string `json:"level"`
}

// copyState is the copied state of this cluster, by key.
type copyState struct {
	stable     map[string]int
	deprecated map[string]string // deprecatedKey(ns, ver) → reason
	keys       map[string]appKeyRecord
	externals  map[string]externalDependent
}

// readCopyState reads the copied buckets. In a cluster every value
// comes from its stream leader, so a replica that is behind does not
// hide a value.
func (g *Gateway) readCopyState(ctx context.Context) (copyState, error) {
	s := copyState{stable: map[string]int{}, deprecated: map[string]string{}, keys: map[string]appKeyRecord{}, externals: map[string]externalDependent{}}
	cl := g.cfg.cluster
	if cl == nil {
		g.mu.Lock()
		for ns, vN := range g.stableVN {
			if vN > 0 {
				s.stable[ns] = vN
			}
		}
		for k, reason := range g.deprecation {
			s.deprecated[deprecatedKey(k.namespace, k.version)] = reason
		}
		g.mu.Unlock()
		keys, err := g.listAppKeys(ctx)
		if err != nil {
			return s, err
		}
		for _, r := range keys {
			s.keys[r.ID] = r
		}
		for _, e := range g.externals.all() {
			s.externals[e.ID] = e
		}
		return s, nil
	}
	if g.clusterKV() == nil {
		return s, errAppKeyUnavailable
	}
	read := func(bucket string, each func(key string, val []byte) error) error {
		kctx, cancel := context.WithTimeout(ctx, 4*kvCallTimeout)
		defer cancel()
		keys, err := kvLeaderKeys(kctx, cl.Conn, bucket)
		if err != nil {
			return fmt.Errorf("%s: %w", bucket, err)
		}
		for _, k := range keys {
			v, ok, err := kvLeaderGet(kctx, cl.Conn, bucket, k)
			if err != nil {
				return fmt.Errorf("%s %s: %w", bucket, k, err)
			}
			if !ok {
				continue
			}
			if err := each(k, v); err != nil {
				return fmt.Errorf("%s %s: %w", bucket, k, err)
			}
		}
		return nil
	}
	if err := read(stableBucketName, func(k string, v []byte) error {
		vN, err := parseStableKVValue(v)
		if err == nil {
			s.stable[k] = vN
		}
		return nil // a cleared (0) or bad value is not a mark
	}); err != nil {
		return s, err
	}
	if err := read(deprecatedBucketName, func(k string, v []byte) error {
		var d deprecatedKVValue
		if err := json.Unmarshal(v, &d); err != nil {
			return err
		}
		s.deprecated[k] = d.Reason
		return nil
	}); err != nil {
		return s, err
	}
	if err := read(appKeysBucketName, func(k string, v []byte) error {
		var r appKeyRecord
		if err := json.Unmarshal(v, &r); err != nil {
			return err
		}
		s.keys[k] = r
		return nil
	}); err != nil {
		return s, err
	}
	if err := read(externalDependentsBucketName, func(k string, v []byte) error {
		var e externalDependent
		if err := json.Unmarshal(v, &e); err != nil {
			return err
		}
		s.externals[k] = e
		return nil
	}); err != nil {
		return s, err
	}
	return s, nil
}

func stateKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// srStore returns the registry store; nil without a registry,
// errAppKeyUnavailable while the cluster store is not ready.
func (g *Gateway) srStore() (*srserve.Store, error) {
	if g.sr == nil {
		return nil, nil
	}
	st := g.sr.store.Load()
	if st == nil {
		return nil, errAppKeyUnavailable
	}
	return st, nil
}

// exportClusterState builds the document.
func (g *Gateway) exportClusterState(ctx context.Context, allowUnfrozen bool) (*clusterState, error) {
	_, frozen, err := g.getFreeze(ctx)
	if err != nil {
		return nil, errFreezeUnavailable(err)
	}
	doc := &clusterState{
		V:                  clusterStateVersion,
		ExportedAt:         nowFunc().UTC().Format(time.RFC3339),
		ExportedBy:         adminIdentityFromContext(ctx),
		Frozen:             frozen,
		Stable:             []cutoverStable{},
		Deprecated:         []cutoverDeprecated{},
		Keys:               []appKeyRecord{},
		ExternalDependents: []externalDependent{},
	}
	limits, err := g.limits.readRecords(ctx)
	if err != nil {
		return nil, fmt.Errorf("limits: %w", err)
	}
	for _, k := range stateKeys(limits) {
		doc.Limits = append(doc.Limits, limits[k])
	}
	if !frozen {
		if !allowUnfrozen {
			return nil, &cutoverError{status: http.StatusConflict, code: codes.FailedPrecondition, msg: "cluster state export: the cluster is not frozen; freeze it first (gwag ctl cluster freeze), or export with allowUnfrozen and copy again after the last change"}
		}
		doc.Warnings = append(doc.Warnings, "exported without a freeze: admin writes during the export may be missing or half there; freeze and export again before the switch")
		g.srWarnf("cluster state export without a freeze (allowUnfrozen), by %s", doc.ExportedBy)
	}
	if cl := g.cfg.cluster; cl != nil && cl.Server != nil {
		doc.Node = cl.Server.Name()
	}
	s, err := g.readCopyState(ctx)
	if err != nil {
		return nil, err
	}
	for _, ns := range stateKeys(s.stable) {
		doc.Stable = append(doc.Stable, cutoverStable{Namespace: ns, VN: s.stable[ns]})
	}
	for _, k := range stateKeys(s.deprecated) {
		ns, ver, ok := parseDeprecatedKey(k)
		if !ok {
			continue
		}
		doc.Deprecated = append(doc.Deprecated, cutoverDeprecated{Namespace: ns, Version: ver, Reason: s.deprecated[k]})
	}
	for _, k := range stateKeys(s.keys) {
		r := s.keys[k]
		if r.Namespaces == nil {
			r.Namespaces = []string{} // a system key; null would not validate on import
		}
		doc.Keys = append(doc.Keys, r)
	}
	for _, k := range stateKeys(s.externals) {
		doc.ExternalDependents = append(doc.ExternalDependents, s.externals[k])
	}
	st, err := g.srStore()
	if err != nil {
		return nil, err
	}
	if st != nil {
		reg, err := exportRegistry(ctx, st)
		if err != nil {
			return nil, fmt.Errorf("schema registry: %w", err)
		}
		doc.SchemaRegistry = reg
	}
	return doc, nil
}

func exportRegistry(ctx context.Context, st *srserve.Store) (*cutoverRegistry, error) {
	names, err := st.Contexts(ctx)
	if err != nil {
		return nil, err
	}
	reg := &cutoverRegistry{Contexts: []cutoverRegistryContext{}}
	for _, name := range names {
		c := st.Context(name)
		var skip func(string) bool
		if c.Default() {
			skip = func(s string) bool { return s == ControlPlaneSubject }
		}
		b, err := c.Bundle(ctx, skip)
		if err != nil {
			return nil, fmt.Errorf("context %s: %w", name, err)
		}
		if len(b.Versions) == 0 && b.Global == "" && len(b.Levels) == 0 {
			continue
		}
		rc := cutoverRegistryContext{Name: name, Schemas: b.Schemas, Versions: b.Versions, Global: b.Global, Levels: []cutoverLevel{}}
		if rc.Schemas == nil {
			rc.Schemas = []srserve.ImportSchema{}
		}
		if rc.Versions == nil {
			rc.Versions = []srserve.ImportVersion{}
		}
		for _, subj := range stateKeys(b.Levels) {
			rc.Levels = append(rc.Levels, cutoverLevel{Subject: subj, Level: b.Levels[subj]})
		}
		reg.Contexts = append(reg.Contexts, rc)
	}
	return reg, nil
}

// importCounts counts one section of an import.
type importCounts struct {
	Added       int `json:"added"`
	Unchanged   int `json:"unchanged"`
	Overwritten int `json:"overwritten"`
}

type importRegistryCounts struct {
	SchemasAdded  int `json:"schemasAdded"`
	VersionsAdded int `json:"versionsAdded"`
	Unchanged     int `json:"unchanged"`
}

type importResult struct {
	DryRun             bool                 `json:"dryRun"`
	Stable             importCounts         `json:"stable"`
	Deprecated         importCounts         `json:"deprecated"`
	Keys               importCounts         `json:"keys"`
	ExternalDependents importCounts         `json:"externalDependents"`
	Limits             importCounts         `json:"limits"`
	SchemaRegistry     importRegistryCounts `json:"schemaRegistry"`
	// Conflicts lists the items that differ (dry run, or with
	// overwrite: the ones overwritten).
	Conflicts []string `json:"conflicts"`
	Warnings  []string `json:"warnings"`
}

// keyRecordEqual compares two stored key records.
func keyRecordEqual(a, b appKeyRecord) bool {
	if len(a.Namespaces) == 0 && len(b.Namespaces) == 0 {
		a.Namespaces, b.Namespaces = nil, nil
	}
	return reflect.DeepEqual(a, b)
}

func externalEqual(a, b externalDependent) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// importClusterState applies doc. With conflicts and no overwrite,
// or any Schema Registry conflict, nothing is written and the error
// lists them.
func (g *Gateway) importClusterState(ctx context.Context, doc *clusterState, overwrite, dryRun bool) (*importResult, error) {
	if doc.V != clusterStateVersion {
		return nil, huma.Error422UnprocessableEntity(fmt.Sprintf("cluster state document version %d: this gateway reads version %d", doc.V, clusterStateVersion))
	}
	res := &importResult{DryRun: dryRun, Conflicts: []string{}, Warnings: []string{}}
	cur, err := g.readCopyState(ctx)
	if err != nil {
		return nil, err
	}

	// Validate and plan every section before writing anything.
	var conflicts []string
	stablePut := map[string]int{}
	for _, m := range doc.Stable {
		if err := validateNS(m.Namespace); err != nil || m.VN < 1 {
			return nil, huma.Error422UnprocessableEntity(fmt.Sprintf("stable %q v%d: invalid", m.Namespace, m.VN))
		}
		have, ok := cur.stable[m.Namespace]
		switch {
		case !ok:
			stablePut[m.Namespace] = m.VN
			res.Stable.Added++
		case have == m.VN:
			res.Stable.Unchanged++
		default:
			conflicts = append(conflicts, fmt.Sprintf("stable %s is v%d here, v%d in the document", m.Namespace, have, m.VN))
			if overwrite {
				stablePut[m.Namespace] = m.VN
				res.Stable.Overwritten++
			}
		}
	}
	depPut := map[string]cutoverDeprecated{}
	for _, d := range doc.Deprecated {
		if err := validateNS(d.Namespace); err != nil || d.Version == "" || d.Reason == "" {
			return nil, huma.Error422UnprocessableEntity(fmt.Sprintf("deprecated %s/%s: invalid", d.Namespace, d.Version))
		}
		k := deprecatedKey(d.Namespace, d.Version)
		have, ok := cur.deprecated[k]
		switch {
		case !ok:
			depPut[k] = d
			res.Deprecated.Added++
		case have == d.Reason:
			res.Deprecated.Unchanged++
		default:
			conflicts = append(conflicts, fmt.Sprintf("deprecated %s/%s has another reason here (%q, document %q)", d.Namespace, d.Version, have, d.Reason))
			if overwrite {
				depPut[k] = d
				res.Deprecated.Overwritten++
			}
		}
	}
	var keyPut []appKeyRecord
	for _, r := range doc.Keys {
		if !appKeyIDPattern.MatchString(r.ID) || len(r.Hash) != 64 || (r.Kind != "" && r.Kind != appKeyKindSystem) {
			return nil, huma.Error422UnprocessableEntity(fmt.Sprintf("key %q: invalid record", r.ID))
		}
		have, ok := cur.keys[r.ID]
		switch {
		case !ok:
			keyPut = append(keyPut, r)
			res.Keys.Added++
		case keyRecordEqual(have, r):
			res.Keys.Unchanged++
		default:
			conflicts = append(conflicts, fmt.Sprintf("%s %s differs here", r.kindName(), r.ID))
			if overwrite {
				keyPut = append(keyPut, r)
				res.Keys.Overwritten++
			}
		}
	}
	var extPut []externalDependent
	for _, e := range doc.ExternalDependents {
		if errs := normalizeExternalDependent(&e); len(errs) > 0 {
			return nil, huma.Error422UnprocessableEntity(fmt.Sprintf("external dependent %q: %s", e.ID, errs[0].Message))
		}
		have, ok := cur.externals[e.ID]
		switch {
		case !ok:
			extPut = append(extPut, e)
			res.ExternalDependents.Added++
		case externalEqual(have, e):
			res.ExternalDependents.Unchanged++
		default:
			conflicts = append(conflicts, fmt.Sprintf("external dependent %s differs here", e.ID))
			if overwrite {
				extPut = append(extPut, e)
				res.ExternalDependents.Overwritten++
			}
		}
	}

	var limitPut []limitRecord
	if len(doc.Limits) > 0 {
		have, err := g.limits.readRecords(ctx)
		if err != nil {
			return nil, fmt.Errorf("limits: %w", err)
		}
		for _, r := range doc.Limits {
			if err := r.validate(); err != nil {
				return nil, huma.Error422UnprocessableEntity(fmt.Sprintf("limit %s %s: %v", r.Kind, r.scope(), err))
			}
			cur, ok := have[r.key()]
			switch {
			case !ok:
				limitPut = append(limitPut, r)
				res.Limits.Added++
			case cur == r:
				res.Limits.Unchanged++
			default:
				conflicts = append(conflicts, fmt.Sprintf("%s %s %s differs here", r.Kind, r.scope(), r.Caller))
				if overwrite {
					limitPut = append(limitPut, r)
					res.Limits.Overwritten++
				}
			}
		}
	}

	// Schema Registry: a dry run of each context.
	var srConflicts []string
	var st *srserve.Store
	type srWork struct {
		c *srserve.Store
		b srserve.Bundle
	}
	var work []srWork
	if reg := doc.SchemaRegistry; reg != nil && len(reg.Contexts) > 0 {
		st, err = g.srStore()
		if err != nil {
			return nil, err
		}
		if st == nil {
			return nil, &cutoverError{status: http.StatusConflict, code: codes.FailedPrecondition, msg: "cluster state import: the document has Schema Registry contents and this gateway serves no Schema Registry (WithSchemaRegistry)"}
		}
		for _, rc := range reg.Contexts {
			name, err := srserve.NormContext(rc.Name)
			if err != nil {
				return nil, huma.Error422UnprocessableEntity(fmt.Sprintf("schema registry context %q: %v", rc.Name, err))
			}
			b := srserve.Bundle{Schemas: rc.Schemas, Versions: rc.Versions, Global: rc.Global, Levels: map[string]string{}}
			for _, l := range rc.Levels {
				b.Levels[l.Subject] = l.Level
			}
			c := st.Context(name)
			dry, err := c.ImportWith(ctx, b, srserve.ImportOptions{Default: true, DryRun: true})
			if err != nil {
				return nil, fmt.Errorf("schema registry context %s: %w", name, err)
			}
			for _, x := range dry.Conflicts {
				srConflicts = append(srConflicts, fmt.Sprintf("schema registry context %s: %s", name, x))
			}
			res.SchemaRegistry.SchemasAdded += dry.SchemasAdded
			res.SchemaRegistry.VersionsAdded += dry.VersionsAdded
			res.SchemaRegistry.Unchanged += dry.Unchanged
			res.Warnings = append(res.Warnings, dry.Warnings...)
			work = append(work, srWork{c: c, b: b})
		}
	}
	sort.Strings(conflicts)
	res.Conflicts = append(append(res.Conflicts, conflicts...), srConflicts...)
	if dryRun {
		return res, nil
	}
	if len(srConflicts) > 0 || (len(conflicts) > 0 && !overwrite) {
		msg := "cluster state import conflicts, nothing written: " + strings.Join(res.Conflicts, "; ")
		switch {
		case len(srConflicts) > 0:
			msg += ". Schema Registry conflicts cannot be overwritten: green allocated those ids itself (services registered channels with green before the import). Import into a green whose registry holds only its control plane subject"
		default:
			msg += ". Run with overwrite to take the document's values"
		}
		return nil, &cutoverError{status: http.StatusConflict, code: codes.FailedPrecondition, msg: msg}
	}

	// Write. The registry first: its conflicts are the ones a
	// concurrent registration can still cause.
	for _, w := range work {
		if _, err := w.c.ImportWith(ctx, w.b, srserve.ImportOptions{Default: true}); err != nil {
			return nil, fmt.Errorf("schema registry context %s: %w", w.c.Name(), err)
		}
	}
	for _, r := range keyPut {
		if err := g.putAppKey(ctx, r); err != nil {
			return nil, err
		}
	}
	for _, r := range limitPut {
		if err := g.limits.putRecord(ctx, r); err != nil {
			return nil, fmt.Errorf("limit %s %s: %w", r.Kind, r.scope(), err)
		}
	}
	for _, e := range extPut {
		if e.LastOp == "" {
			e.LastOp = changeOpPut
		}
		if err := g.putExternalDependent(ctx, e); err != nil {
			return nil, err
		}
	}
	if len(stablePut) > 0 || len(depPut) > 0 {
		if err := g.applyCopiedMarks(ctx, stablePut, depPut); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// applyCopiedMarks writes stable and deprecated marks: this node's
// state, then the buckets (the watch on every other node applies
// them), then a schema rebuild.
func (g *Gateway) applyCopiedMarks(ctx context.Context, stable map[string]int, dep map[string]cutoverDeprecated) error {
	g.mu.Lock()
	for ns, vN := range stable {
		g.retractStableLocked(ns, vN) // sets the value, up or down
	}
	for _, d := range dep {
		g.setDeprecationLocked(d.Namespace, d.Version, d.Reason)
	}
	t := g.peers
	rebuildErr := g.assembleLocked()
	g.mu.Unlock()
	if rebuildErr != nil {
		return fmt.Errorf("schema rebuild after import: %w", rebuildErr)
	}
	if t != nil {
		kctx, cancel := context.WithTimeout(ctx, 2*kvCallTimeout*time.Duration(1+len(stable)+len(dep)))
		defer cancel()
		for ns, vN := range stable {
			if err := writeStableForced(kctx, t.stable, ns, vN); err != nil {
				return err
			}
		}
		for k, d := range dep {
			b, _ := json.Marshal(deprecatedKVValue{Reason: d.Reason})
			if err := kvPutRetry(kctx, t.deprecated, k, b); err != nil {
				return fmt.Errorf("deprecated %s: %w", k, err)
			}
		}
	}
	for _, d := range dep {
		g.publishServiceChange(adminEventsActionRegistered, d.Namespace, d.Version, "", 0)
	}
	return nil
}

// ---- admin routes ---------------------------------------------------

type exportClusterStateIn struct {
	AllowUnfrozen bool `query:"allowUnfrozen" doc:"Export although the cluster is not frozen. The document then carries a warning."`
}

type exportClusterStateOut struct {
	Body *clusterState
}

type importClusterStateIn struct {
	Body struct {
		State     clusterState `json:"state" doc:"A document from exportClusterState."`
		Overwrite bool         `json:"overwrite,omitempty" doc:"Take the document's value for every conflicting item (Schema Registry conflicts excepted)."`
		DryRun    bool         `json:"dryRun,omitempty" doc:"Report what the import would do; write nothing."`
	}
}

type importClusterStateOut struct {
	Body *importResult
}

func clusterStateHumaErr(err error) error {
	var ce *cutoverError
	var se huma.StatusError
	switch {
	case errors.As(err, &ce):
		return ce
	case errors.As(err, &se):
		return se
	case errors.Is(err, errAppKeyUnavailable):
		return huma.Error503ServiceUnavailable("cluster store not ready")
	}
	return err
}

func registerClusterState(api huma.API, g *Gateway) {
	huma.Register(api, huma.Operation{
		OperationID: "exportClusterState",
		Method:      http.MethodGet,
		Path:        "/admin/cluster/state",
		Summary:     "Export the cluster state a cutover copies to another cluster: stable and deprecated marks, app and system keys (stored hashes), external dependents, operator limits, quotas and costs, Schema Registry contents with ids. Refused unless the cluster is frozen (allowUnfrozen overrides). Admin or system key.",
	}, func(ctx context.Context, in *exportClusterStateIn) (*exportClusterStateOut, error) {
		doc, err := g.exportClusterState(ctx, in.AllowUnfrozen)
		if err != nil {
			return nil, clusterStateHumaErr(err)
		}
		return &exportClusterStateOut{Body: doc}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:  "importClusterState",
		Method:       http.MethodPost,
		Path:         "/admin/cluster/state",
		MaxBodyBytes: 64 << 20,
		Summary:      "Import a document from exportClusterState. Idempotent. An item held here with another value is a conflict: nothing is written unless overwrite. Schema Registry conflicts always refuse. Registrations are not copied. Admin or system key.",
	}, func(ctx context.Context, in *importClusterStateIn) (*importClusterStateOut, error) {
		res, err := g.importClusterState(ctx, &in.Body.State, in.Body.Overwrite, in.Body.DryRun)
		if err != nil {
			return nil, clusterStateHumaErr(err)
		}
		return &importClusterStateOut{Body: res}, nil
	})
}
