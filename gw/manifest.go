package gateway

import (
	"fmt"

	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

// serviceManifest is the declared metadata a registration carries for
// one (namespace, version): the services it calls (Depends), the team
// that owns it (Owner) and whether admin readers without a token may
// see it (Public). It rides the registry KV value so every
// cluster node stores the same copy on its slot. The zero value means
// "no manifest" (old clients, boot-time AddProto).
type serviceManifest struct {
	Depends []manifestDependency `json:"depends,omitempty"`
	Owner   *manifestOwner       `json:"owner,omitempty"`
	Public  bool                 `json:"public,omitempty"`
}

type manifestDependency struct {
	Service string   `json:"service"`
	Lines   []string `json:"lines"`
}

type manifestOwner struct {
	Team    string `json:"team,omitempty"`
	Contact string `json:"contact,omitempty"`
}

// manifestFromBinding validates and copies the manifest fields of a
// ServiceBinding. Lines are canonicalised through parseVersion, so
// "v01" and "stable" are refused the same way a binding version is.
func manifestFromBinding(b *cpv1.ServiceBinding) (serviceManifest, error) {
	var m serviceManifest
	seen := map[string]bool{}
	for _, d := range b.GetDepends() {
		svc := d.GetService()
		if err := validateNS(svc); err != nil {
			return serviceManifest{}, fmt.Errorf("depends: service: %w", err)
		}
		if seen[svc] {
			return serviceManifest{}, fmt.Errorf("depends: service %q listed twice", svc)
		}
		seen[svc] = true
		if len(d.GetLines()) == 0 {
			return serviceManifest{}, fmt.Errorf("depends: service %q: at least one line required", svc)
		}
		dep := manifestDependency{Service: svc}
		seenLine := map[string]bool{}
		for _, l := range d.GetLines() {
			if l == "" {
				return serviceManifest{}, fmt.Errorf("depends: service %q: empty line", svc)
			}
			canon, _, err := parseVersion(l)
			if err != nil {
				return serviceManifest{}, fmt.Errorf("depends: service %q: %w", svc, err)
			}
			if seenLine[canon] {
				continue
			}
			seenLine[canon] = true
			dep.Lines = append(dep.Lines, canon)
		}
		m.Depends = append(m.Depends, dep)
	}
	if o := b.GetOwner(); o != nil && (o.GetTeam() != "" || o.GetContact() != "") {
		m.Owner = &manifestOwner{Team: o.GetTeam(), Contact: o.GetContact()}
	}
	m.Public = b.GetPublic()
	return m, nil
}

// setSlotManifestLocked stores m on the slot at key, if the slot
// exists. Latest registration wins: replicas of one (namespace,
// version) are expected to carry the same manifest, and when they do
// not, the last one to register (or heartbeat, in cluster mode) is
// what the admin API reports. Caller holds g.mu.
func (g *Gateway) setSlotManifestLocked(key poolKey, m serviceManifest) {
	if s := g.slots[key]; s != nil {
		if s.manifest.Public != m.Public {
			g.publicGen.Add(1)
		}
		s.manifest = m
	}
}

// protoDepends / protoOwner render the slot manifest back to the wire
// shape for ListServices.
func (m serviceManifest) protoDepends() []*cpv1.Dependency {
	if len(m.Depends) == 0 {
		return nil
	}
	out := make([]*cpv1.Dependency, 0, len(m.Depends))
	for _, d := range m.Depends {
		out = append(out, &cpv1.Dependency{
			Service: d.Service,
			Lines:   append([]string(nil), d.Lines...),
		})
	}
	return out
}

func (m serviceManifest) protoOwner() *cpv1.Owner {
	if m.Owner == nil {
		return nil
	}
	return &cpv1.Owner{Team: m.Owner.Team, Contact: m.Owner.Contact}
}
