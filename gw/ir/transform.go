package ir

import (
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

