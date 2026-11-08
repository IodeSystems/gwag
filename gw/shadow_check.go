package gateway

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/reflect/protoreflect"

	sr "github.com/iodesystems/gwag/v2/events/schemaregistry"
	"github.com/iodesystems/gwag/v2/gw/internal/srserve"
)

// Shadow check.
//
// A message with (gwag.ps.v1.shadow) is gwag's copy of a message
// another system produces. When a proto binding registers through the
// control plane, the gateway compares each such message with the
// subject it names in its own Schema Registry. The result is a
// warning: the registration always succeeds, and the gateway never
// calls an external registry (the corp schemas are in a context
// filled by `gwag schema import`).
//
// Lookup rule: the option's registry name is the context
// (srserve.NormContext: "corp" is ".corp"). If that context holds the
// subject, it is checked there. Otherwise the other non-default
// contexts, sorted by name, are tried and the first that holds the
// subject is used. The default context holds what gwag registers
// itself and is never searched. A subject no context holds is not
// checked.
//
// The check runs at Register time only; a later import is not seen
// until the next Register.

const shadowCheckTimeout = 10 * time.Second

// Outcomes of shadow_check_total.
const (
	shadowMatch    = "match"
	shadowMismatch = "mismatch"
	shadowNotFound = "not_found"
	shadowError    = "error"
)

// shadowMetrics counts shadow checks. Like appKeyMetrics it lives on
// the default Prometheus registry only.
type shadowMetrics struct {
	check *prometheus.CounterVec
}

func newShadowMetrics(pm *prometheusMetrics) *shadowMetrics {
	m := &shadowMetrics{check: prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: pm.prefix + "_shadow_check_total",
		Help: "Shadow checks at Register, by outcome: match, mismatch, not_found (no context holds the subject), error (the check could not run).",
	}, []string{"outcome"})}
	pm.registry.MustRegister(m.check)
	return m
}

func (g *Gateway) recordShadowCheck(outcome string) {
	if g.shadowMetrics == nil {
		return
	}
	g.shadowMetrics.check.WithLabelValues(outcome).Inc()
}

// shadowWarnings holds the warnings of the latest Register of each
// (namespace, version) taken by this gateway.
type shadowWarnings struct {
	mu sync.Mutex
	by map[poolKey][]string
}

func (w *shadowWarnings) set(k poolKey, warnings []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(warnings) == 0 {
		delete(w.by, k)
		return
	}
	if w.by == nil {
		w.by = map[poolKey][]string{}
	}
	w.by[k] = warnings
}

func (w *shadowWarnings) get(k poolKey) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.by[k]...)
}

// noteShadowWarnings stores the warnings of a successful Register for
// ListServices (replacing the line's earlier ones, so a fixed shadow
// clears them) and returns them for the response.
func (g *Gateway) noteShadowWarnings(prep []preparedBinding, by map[poolKey][]string) []string {
	var all []string
	for _, p := range prep {
		k := poolKey{namespace: p.namespace, version: p.version}
		g.shadowWarn.set(k, by[k])
		all = append(all, by[k]...)
	}
	return all
}

// checkShadows checks every shadow message of the proto bindings in
// prep and returns the warnings per binding. Nothing to check, or the
// registry is off: nil.
func (g *Gateway) checkShadows(ctx context.Context, prep []preparedBinding) map[poolKey][]string {
	if g.sr == nil {
		return nil
	}
	var out map[poolKey][]string
	for _, p := range prep {
		if p.fileDesc == nil {
			continue
		}
		var msgs []protoreflect.MessageDescriptor
		var walk func(protoreflect.MessageDescriptors)
		walk = func(ms protoreflect.MessageDescriptors) {
			for i := 0; i < ms.Len(); i++ {
				msgs = append(msgs, ms.Get(i))
				walk(ms.Get(i).Messages())
			}
		}
		walk(p.fileDesc.Messages())
		for _, md := range msgs {
			s, ok := sr.ShadowOf(md)
			if !ok {
				continue
			}
			for _, w := range g.checkShadow(ctx, p.namespace, p.version, md, s) {
				if out == nil {
					out = map[poolKey][]string{}
				}
				k := poolKey{namespace: p.namespace, version: p.version}
				out[k] = append(out[k], w)
			}
		}
	}
	return out
}

// checkShadow checks one shadow message. It returns the warning
// lines (none on a match, a miss or a registry that is not ready) and
// records the outcome.
func (g *Gateway) checkShadow(ctx context.Context, ns, ver string, md protoreflect.MessageDescriptor, s sr.Shadow) []string {
	store := g.sr.store.Load()
	if store == nil {
		return nil // cluster registry not joined yet
	}
	ctx, cancel := context.WithTimeout(ctx, shadowCheckTimeout)
	defer cancel()
	who := fmt.Sprintf("shadow %s of %s/%s", md.FullName(), ns, ver)
	if s.Registry == "" || s.Subject == "" {
		g.recordShadowCheck(shadowError)
		w := fmt.Sprintf("%s: option needs registry and subject", who)
		g.srWarnf("%s", w)
		return []string{w}
	}
	cname, found, err := g.sr.shadowContext(ctx, store, s)
	if err != nil {
		g.recordShadowCheck(shadowError)
		w := fmt.Sprintf("%s: subject %s: %v", who, s.Subject, err)
		g.srWarnf("%s", w)
		return []string{w}
	}
	if !found {
		g.recordShadowCheck(shadowNotFound)
		return nil
	}
	c, err := sr.NewClient(sr.Config{
		URL:        "http://gwag.internal/contexts/" + cname,
		HTTPClient: g.sr.internalHTTP(),
	})
	if err != nil {
		g.recordShadowCheck(shadowError)
		w := fmt.Sprintf("%s: %v", who, err)
		g.srWarnf("%s", w)
		return []string{w}
	}
	probs, err := sr.CheckShadow(ctx, c, s, md)
	if err != nil {
		g.recordShadowCheck(shadowError)
		w := fmt.Sprintf("%s vs subject %s (context %s): check failed: %v", who, s.Subject, cname, err)
		g.srWarnf("%s", w)
		return []string{w}
	}
	var lines []string
	for _, p := range probs {
		if !p.Warning {
			lines = append(lines, p.String())
		}
	}
	if len(lines) == 0 {
		g.recordShadowCheck(shadowMatch)
		return nil
	}
	g.recordShadowCheck(shadowMismatch)
	w := fmt.Sprintf("%s differs from subject %s (context %s, version %s): %s",
		who, s.Subject, cname, shadowVersion(s), strings.Join(lines, "; "))
	g.srWarnf("%s", w)
	return []string{w}
}

func shadowVersion(s sr.Shadow) string {
	if s.Version == "" {
		return "latest"
	}
	return s.Version
}

// shadowContext finds the context that holds s.Subject: the context
// named by s.Registry first, then the other non-default contexts in
// name order. found is false when none does.
func (r *schemaRegistry) shadowContext(ctx context.Context, store *srserve.Store, s sr.Shadow) (name string, found bool, err error) {
	has := func(c string) (bool, error) {
		_, err := store.Context(c).Versions(ctx, s.Subject, false)
		var e *srserve.Error
		if errors.As(err, &e) && e.Code == 40401 {
			return false, nil
		}
		return err == nil, err
	}
	var order []string
	if c, nerr := srserve.NormContext(s.Registry); nerr == nil && c != srserve.DefaultContext {
		order = append(order, c)
	}
	all, err := store.Contexts(ctx)
	if err != nil {
		return "", false, err
	}
	sort.Strings(all)
	for _, c := range all {
		if c != srserve.DefaultContext && (len(order) == 0 || c != order[0]) {
			order = append(order, c)
		}
	}
	for _, c := range order {
		ok, err := has(c)
		if err != nil {
			return "", false, err
		}
		if ok {
			return c, true, nil
		}
	}
	return "", false, nil
}
