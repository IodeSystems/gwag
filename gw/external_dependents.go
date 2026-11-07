package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// External dependents: consumers gwag cannot see (an external API
// customer, a partner integration, a mobile app release) that pin a
// set of lines. An operator declares one through the admin API, for
// example "acme pins library [v1]".
//
// A declaration counts wherever a registered dependent counts:
//   - the admin graph's desired edges (consumerKind "external");
//   - the sunset-able rule (admin_graph.go);
//   - dependency_unmet (lifecycle_metrics.go), consumer_kind="external";
//   - drain coverage (instance_drain.go): draining the last ready
//     instance of a line only an external dependent's pin covers is
//     BLOCKED, with a reason naming it.
//
// It does not affect the start gate: it registers no instance and
// is nobody's dependency.
//
// Expiry: an optional expiresAt. Past it, the declaration stops
// counting everywhere, but it stays stored and listed with
// expired=true until an operator renews or deletes it. Expiry is
// evaluated against the clock at read time on every node, so it needs
// no KV TTL and no delete event (a TTL-expired key raises no event to
// watchers, which would leave the declaration live on other nodes).
//
// Storage: in memory standalone; in cluster mode the
// gwag-external-dependents KV bucket (no TTL, replicas raised and
// shrunk with the other buckets in setReplicas), mirrored into
// g.externals by a watch on every node.

const externalDependentsBucketName = "gwag-external-dependents"

// externalDependentKinds is the kind enum. "other" covers anything
// the first three do not.
var externalDependentKinds = []string{"customer", "partner", "app", "other"}

// externalIDPattern is the id rule: lower-case slug, valid as a KV
// key and a Prometheus label value.
var externalIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// externalDependent is one declaration, as stored (KV JSON value).
type externalDependent struct {
	ID            string               `json:"id"`
	Name          string               `json:"name,omitempty"`
	Kind          string               `json:"kind"`
	Owner         *manifestOwner       `json:"owner,omitempty"`
	Depends       []manifestDependency `json:"depends"`
	Note          string               `json:"note,omitempty"`
	ExpiresUnixMs int64                `json:"expires_unix_ms,omitempty"`
	UpdatedUnixMs int64                `json:"updated_unix_ms"`
}

// expired reports whether e has an expiry at or before now.
func (e externalDependent) expired(now time.Time) bool {
	return e.ExpiresUnixMs > 0 && now.UnixMilli() >= e.ExpiresUnixMs
}

// label names e in reasons: `external acme (Acme Corp)`.
func (e externalDependent) label() string {
	if e.Name != "" && e.Name != e.ID {
		return fmt.Sprintf("external %s (%s)", e.ID, e.Name)
	}
	return "external " + e.ID
}

// externalDependents holds the declarations. mu is a leaf lock.
type externalDependents struct {
	mu sync.Mutex
	m  map[string]externalDependent
}

func (x *externalDependents) set(e externalDependent) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.m == nil {
		x.m = map[string]externalDependent{}
	}
	x.m[e.ID] = e
}

func (x *externalDependents) remove(id string) (externalDependent, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	e, ok := x.m[id]
	delete(x.m, id)
	return e, ok
}

func (x *externalDependents) get(id string) (externalDependent, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	e, ok := x.m[id]
	return e, ok
}

// all returns every declaration, expired ones included, sorted by id.
func (x *externalDependents) all() []externalDependent {
	x.mu.Lock()
	out := make([]externalDependent, 0, len(x.m))
	for _, e := range x.m {
		out = append(out, e)
	}
	x.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// active returns the declarations that count at now, sorted by id.
func (x *externalDependents) active(now time.Time) []externalDependent {
	all := x.all()
	out := all[:0]
	for _, e := range all {
		if !e.expired(now) {
			out = append(out, e)
		}
	}
	return out
}

// externalFieldError is one validation failure at a huma location
// ("body.depends[1].lines").
type externalFieldError struct {
	Location string
	Message  string
}

// normalizeExternalDependent validates e in place: kind, depends
// (service names, lines canonicalised with parseVersion, the same
// rule as a binding's depends). Returns every field error found.
func normalizeExternalDependent(e *externalDependent) []externalFieldError {
	var errs []externalFieldError
	if !externalIDPattern.MatchString(e.ID) {
		errs = append(errs, externalFieldError{"path.id", "id must match " + externalIDPattern.String()})
	}
	if !containsString(externalDependentKinds, e.Kind) {
		errs = append(errs, externalFieldError{"body.kind", "kind must be one of " + strings.Join(externalDependentKinds, ", ")})
	}
	if len(e.Depends) == 0 {
		errs = append(errs, externalFieldError{"body.depends", "at least one dependency required"})
	}
	seen := map[string]bool{}
	for i := range e.Depends {
		d := &e.Depends[i]
		at := fmt.Sprintf("body.depends[%d]", i)
		d.Service = strings.TrimSpace(d.Service)
		if err := validateNS(d.Service); err != nil {
			errs = append(errs, externalFieldError{at + ".service", err.Error()})
		} else if seen[d.Service] {
			errs = append(errs, externalFieldError{at + ".service", fmt.Sprintf("service %q listed twice", d.Service)})
		}
		seen[d.Service] = true
		if len(d.Lines) == 0 {
			errs = append(errs, externalFieldError{at + ".lines", "at least one line required"})
			continue
		}
		var lines []string
		seenLine := map[string]bool{}
		for _, l := range d.Lines {
			l = strings.TrimSpace(l)
			if l == "" {
				errs = append(errs, externalFieldError{at + ".lines", "empty line"})
				continue
			}
			canon, _, err := parseVersion(l)
			if err != nil {
				errs = append(errs, externalFieldError{at + ".lines", err.Error()})
				continue
			}
			if !seenLine[canon] {
				seenLine[canon] = true
				lines = append(lines, canon)
			}
		}
		d.Lines = lines
	}
	if e.Owner != nil && e.Owner.Team == "" && e.Owner.Contact == "" {
		e.Owner = nil
	}
	return errs
}

// externalWarningsLocked lists the pins that name nothing registered
// right now. They are accepted: a dependent can be declared before
// the service or line exists. Caller holds g.mu.
func (g *Gateway) externalWarningsLocked(e externalDependent) []string {
	var out []string
	for _, d := range e.Depends {
		found := false
		var missing []string
		for k := range g.slots {
			if k.namespace == d.Service {
				found = true
			}
		}
		if !found {
			out = append(out, fmt.Sprintf("%s: no line is registered", d.Service))
			continue
		}
		for _, l := range d.Lines {
			if g.slots[poolKey{namespace: d.Service, version: l}] == nil {
				missing = append(missing, l)
			}
		}
		if len(missing) > 0 {
			out = append(out, fmt.Sprintf("%s: %s not registered", d.Service, strings.Join(missing, ", ")))
		}
	}
	return out
}

func (g *Gateway) externalKV() jetstream.KeyValue {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.peers == nil {
		return nil
	}
	return g.peers.externals
}

// putExternalDependent stores e (already normalised): the cluster KV
// first, so a failed write changes nothing, then this node's copy.
func (g *Gateway) putExternalDependent(ctx context.Context, e externalDependent) error {
	prev, _ := g.externals.get(e.ID)
	if kv := g.externalKV(); kv != nil {
		b, err := json.Marshal(e)
		if err != nil {
			return err
		}
		// Two call timeouts: room for kvPutRetry to retry a put lost
		// to a replica change. The same value written twice is the
		// same state.
		kctx, cancel := context.WithTimeout(ctx, 2*kvCallTimeout)
		err = kvPutRetry(kctx, kv, e.ID, b)
		cancel()
		if err != nil {
			return fmt.Errorf("external dependent: kv put: %w", err)
		}
	}
	g.externals.set(e)
	g.publishExternalChange(prev, e)
	return nil
}

// deleteExternalDependent removes the declaration. ok=false when this
// node knows no declaration with that id.
func (g *Gateway) deleteExternalDependent(ctx context.Context, id string) (bool, error) {
	prev, ok := g.externals.get(id)
	if !ok {
		return false, nil
	}
	if kv := g.externalKV(); kv != nil {
		kctx, cancel := context.WithTimeout(ctx, 2*kvCallTimeout)
		err := kvDeleteRetry(kctx, kv, id)
		cancel()
		if err != nil {
			return false, fmt.Errorf("external dependent: kv delete: %w", err)
		}
	}
	g.externals.remove(id)
	g.publishExternalChange(prev, externalDependent{})
	return true, nil
}

// kvDeleteRetry is kvPutRetry for a delete: a delete sent while the
// bucket's replica count changes can get no ack. Deleting twice is the
// same state; a missing key is success.
func kvDeleteRetry(ctx context.Context, kv jetstream.KeyValue, key string) error {
	for {
		actx, cancel := context.WithTimeout(ctx, kvPutAttemptTimeout)
		err := kv.Delete(actx, key)
		cancel()
		if err == nil || errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// publishExternalChange emits one service-change event per provider
// line a or b pins, so admin UIs refetch the graph.
func (g *Gateway) publishExternalChange(a, b externalDependent) {
	seen := map[poolKey]bool{}
	for _, e := range []externalDependent{a, b} {
		for _, d := range e.Depends {
			for _, l := range d.Lines {
				k := poolKey{namespace: d.Service, version: l}
				if seen[k] {
					continue
				}
				seen[k] = true
				g.publishServiceChange(adminEventsActionRegistered, k.namespace, k.version, "", 0)
			}
		}
	}
}

// openExternalDependentsBucket creates (or opens) the bucket. No TTL:
// declarations live until deleted; expiry is a field, read on every
// node (see the file comment).
func openExternalDependentsBucket(ctx context.Context, js jetstream.JetStream) (jetstream.KeyValue, error) {
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: externalDependentsBucketName, Replicas: 1})
	if err != nil && !errors.Is(err, jetstream.ErrBucketExists) {
		return nil, fmt.Errorf("external dependents bucket: %w", err)
	}
	if kv == nil {
		if kv, err = js.KeyValue(ctx, externalDependentsBucketName); err != nil {
			return nil, fmt.Errorf("external dependents bucket open: %w", err)
		}
	}
	return kv, nil
}

// externalsWatchLoop mirrors the bucket into g.externals. A put
// replaces, a delete or purge removes. IncludeHistory replays the
// current set, so a node that joins later sees every declaration.
func (t *peerTracker) externalsWatchLoop(ctx context.Context) {
	defer close(t.externalsDone)
	w, err := t.externals.WatchAll(ctx, jetstream.IncludeHistory())
	if err != nil {
		t.gw.cfg.cluster.Server.Warnf("gateway: external dependents watch start: %v", err)
		return
	}
	defer w.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.Updates():
			if !ok {
				return
			}
			if ev == nil {
				continue
			}
			t.gw.observeExternalKV(ev)
		}
	}
}

func (g *Gateway) observeExternalKV(ev jetstream.KeyValueEntry) {
	switch ev.Operation() {
	case jetstream.KeyValueDelete, jetstream.KeyValuePurge:
		g.externals.remove(ev.Key())
		return
	}
	var e externalDependent
	if err := json.Unmarshal(ev.Value(), &e); err != nil || e.ID != ev.Key() {
		g.cfg.cluster.Server.Warnf("gateway: external dependents bucket: bad value at %s: %v", ev.Key(), err)
		return
	}
	g.externals.set(e)
}
