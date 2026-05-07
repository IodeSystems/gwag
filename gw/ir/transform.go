package ir

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Selector is one (namespace, optional version) match — empty
// version matches any version of the namespace.
type Selector struct {
	Namespace string
	Version   string // "" matches any version
}

// ParseSelectors parses the gateway's `?service=ns:vN[,...]` query
// grammar into Selectors. Empty input returns nil (= match all).
func ParseSelectors(raw string) ([]Selector, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	out := []Selector{}
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		ns, ver, _ := strings.Cut(p, ":")
		out = append(out, Selector{Namespace: strings.TrimSpace(ns), Version: strings.TrimSpace(ver)})
	}
	return out, nil
}

// Filter keeps only Services that match at least one selector.
// Empty `sels` is a no-op (match all). Same semantic as the
// existing `?service=` selector grammar — a missing version on a
// selector matches any version of that namespace.
func Filter(svcs []*Service, sels []Selector) []*Service {
	if len(sels) == 0 {
		return svcs
	}
	out := []*Service{}
	for _, svc := range svcs {
		for _, s := range sels {
			if s.Namespace != svc.Namespace {
				continue
			}
			if s.Version == "" || s.Version == svc.Version {
				out = append(out, svc)
				break
			}
		}
	}
	return out
}

// HideInternal drops Services whose Internal flag is set —
// equivalent to the gateway's `_*` namespace convention.
func HideInternal(svcs []*Service) []*Service {
	out := make([]*Service, 0, len(svcs))
	for _, s := range svcs {
		if s.Internal {
			continue
		}
		out = append(out, s)
	}
	return out
}

// Hides strips Fields whose Type.Named is in the hide-set from
// every Object/Input Type across the input services. Mirrors the
// gateway's HideAndInject middleware Pair.Hides set. Mutates the
// services in place — caller is responsible for not sharing them
// with code that expected the un-stripped shape.
//
// Hides also rewrites the type's Origin to nil if it was present,
// since the descriptor is no longer faithful to the canonical
// shape — same-kind renderers fall through to the synthesis path
// instead of emitting the original (unstripped) descriptor.
func Hides(svcs []*Service, hide map[string]bool) {
	if len(hide) == 0 {
		return
	}
	for _, svc := range svcs {
		for _, t := range svc.Types {
			if t.TypeKind != TypeObject && t.TypeKind != TypeInput {
				continue
			}
			n := 0
			for _, f := range t.Fields {
				if f.Type.IsNamed() && hide[f.Type.Named] {
					continue
				}
				t.Fields[n] = f
				n++
			}
			if n != len(t.Fields) {
				t.Fields = t.Fields[:n]
				t.Origin = nil // descriptor no longer reflects the real fields
			}
		}
	}
}

// MultiVersionPrefix groups services by namespace; for namespaces
// with more than one (namespace, version) entry, the highest
// versionN keeps its names un-prefixed (= "latest, flat") and the
// others get their Operations and Types renamed to
// `<ns>_<vN>_<original>`, with @deprecated markers on every
// Operation and Field.
//
// Mirrors the gateway's existing latest-flat / older-prefixed
// naming policy. Runs in-place; same caveat as Hides applies.
func MultiVersionPrefix(svcs []*Service) []*Service {
	byNS := map[string][]*Service{}
	for _, s := range svcs {
		byNS[s.Namespace] = append(byNS[s.Namespace], s)
	}
	out := make([]*Service, 0, len(svcs))
	nsKeys := make([]string, 0, len(byNS))
	for k := range byNS {
		nsKeys = append(nsKeys, k)
	}
	sort.Strings(nsKeys)
	for _, ns := range nsKeys {
		group := byNS[ns]
		// Sort by numeric version ascending — latest (highest) is
		// the last entry.
		sort.Slice(group, func(i, j int) bool {
			return versionN(group[i].Version) < versionN(group[j].Version)
		})
		latest := group[len(group)-1]
		latestReason := fmt.Sprintf("%s is current", latest.Version)
		for _, svc := range group {
			if svc == latest {
				out = append(out, svc)
				continue
			}
			prefix := svc.Version + "_" // "<vN>_"
			renameService(svc, prefix, latestReason)
			out = append(out, svc)
		}
		// Latest goes last to preserve "newest at the bottom" feel
		// when consumers iterate; pull it forward to keep the
		// declaration order stable.
	}
	return out
}

// renameService prefixes every Operation.Name + Type.Name with
// `prefix` and stamps `dep` as the deprecation reason on every
// emitted Field and Operation. Type refs inside Field.Type / Arg.
// Type / Operation.Output are rewritten too so cross-references
// stay consistent.
func renameService(svc *Service, prefix, dep string) {
	// Rename types and rebuild the map.
	renamed := map[string]string{}
	newTypes := map[string]*Type{}
	for k, t := range svc.Types {
		newName := prefix + t.Name
		renamed[t.Name] = newName
		t.Name = newName
		newTypes[newName] = t
		_ = k
	}
	svc.Types = newTypes

	rewriteRef := func(r *TypeRef) {
		if r == nil {
			return
		}
		if r.IsNamed() {
			if nn, ok := renamed[r.Named]; ok {
				r.Named = nn
			}
		}
		if r.IsMap() {
			if nn, ok := renamed[r.Map.ValueType.Named]; ok && r.Map.ValueType.Named != "" {
				r.Map.ValueType.Named = nn
			}
		}
	}

	// Operations + Fields + Args.
	for _, op := range svc.Operations {
		op.Name = prefix + op.Name
		op.Deprecated = dep
		rewriteRef(op.Output)
		for _, a := range op.Args {
			rewriteRef(&a.Type)
		}
	}
	for _, t := range svc.Types {
		// Variants (union members) point at type names — rename them too.
		for i, v := range t.Variants {
			if nn, ok := renamed[v]; ok {
				t.Variants[i] = nn
			}
		}
		for _, f := range t.Fields {
			f.Deprecated = dep
			rewriteRef(&f.Type)
		}
	}
}

// versionN parses "vN" or "N" into an integer for ordering.
// Anything unparseable comes back as 0 so unfamiliar shapes sort
// before known versions.
func versionN(v string) int {
	if v == "" {
		return 0
	}
	if v[0] == 'v' || v[0] == 'V' {
		v = v[1:]
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return n
}
