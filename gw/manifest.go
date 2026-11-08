package gateway

import (
	"fmt"
	"strings"

	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

// serviceManifest is the declared metadata a registration carries for
// one (namespace, version): the services it calls (Depends), the team
// that owns it (Owner) and whether admin readers without a token may
// see it (Public). It rides the registry KV value so every
// cluster node stores the same copy on its slot. The zero value means
// "no manifest" (old clients, boot-time AddProto).
type serviceManifest struct {
	Depends    []manifestDependency   `json:"depends,omitempty"`
	Owner      *manifestOwner         `json:"owner,omitempty"`
	Public     bool                   `json:"public,omitempty"`
	Subscribes []manifestSubscription `json:"subscribes,omitempty"`

	// ownChannels are the channel keys the binding's own proto
	// declares. A subscription to one of them is satisfied by the
	// binding itself (gateDepsLocked). Set by the control plane at
	// Register; not replicated (only the owner node decides the gate).
	ownChannels []string
}

// manifestDependency is one declared dependency. Channel / Group are
// set only on the dependencies gateDepsLocked derives from
// subscriptions; Service / Lines then name the slot that declares the
// channel, or are empty while no slot declares it.
type manifestDependency struct {
	Service string   `json:"service"`
	Lines   []string `json:"lines"`
	Channel string   `json:"channel,omitempty"`
	Group   string   `json:"group,omitempty"`
}

// manifestSubscription is one ServiceBinding.subscribes entry.
type manifestSubscription struct {
	Channel string `json:"channel"`
	Group   string `json:"group,omitempty"`
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
	seenSub := map[manifestSubscription]bool{}
	for _, sub := range b.GetSubscribes() {
		ms := manifestSubscription{Channel: sub.GetChannel(), Group: sub.GetGroup()}
		if ms.Channel == "" {
			return serviceManifest{}, fmt.Errorf("subscribes: empty channel")
		}
		if !strings.Contains(ms.Channel, ".") {
			return serviceManifest{}, fmt.Errorf("subscribes: channel %q: want <proto package>.<name>", ms.Channel)
		}
		if seenSub[ms] {
			return serviceManifest{}, fmt.Errorf("subscribes: channel %q group %q listed twice", ms.Channel, ms.Group)
		}
		seenSub[ms] = true
		m.Subscribes = append(m.Subscribes, ms)
	}
	return m, nil
}

// setSlotManifestLocked stores m on the slot at key, if the slot
// exists. Latest registration wins: replicas of one (namespace,
// version) are expected to carry the same manifest, and when they do
// not, the last one to register (or heartbeat, in cluster mode) is
// what the admin API reports. Caller holds g.mu.
func (g *Gateway) setSlotManifestLocked(key poolKey, m serviceManifest) {
	if s := g.slots[key]; s != nil {
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

func (m serviceManifest) protoSubscribes() []*cpv1.Subscription {
	if len(m.Subscribes) == 0 {
		return nil
	}
	out := make([]*cpv1.Subscription, 0, len(m.Subscribes))
	for _, s := range m.Subscribes {
		out = append(out, &cpv1.Subscription{Channel: s.Channel, Group: s.Group})
	}
	return out
}

// channelDeclarerLocked returns the slot that declares the named
// channel key. Caller holds g.mu.
func (g *Gateway) channelDeclarerLocked(channel string) (poolKey, bool) {
	for k, s := range g.slots {
		for _, b := range s.channelBindings {
			if b.Channel == channel {
				return k, true
			}
		}
	}
	return poolKey{}, false
}

// gateDepsLocked is the dependency list the start gate, the cycle
// check, the dependency graph and the sunset rule use for a binding at
// self: its declared depends, plus one dependency per subscription
// resolved to the slot that declares the channel (Lines = that slot's
// version). A subscription to a channel no slot declares yet is a
// dependency with no lines, so it is never met. A subscription to a
// channel the binding declares itself is dropped. Caller holds g.mu.
func (g *Gateway) gateDepsLocked(m serviceManifest, self poolKey) []manifestDependency {
	if len(m.Subscribes) == 0 {
		return m.Depends
	}
	out := append([]manifestDependency(nil), m.Depends...)
	for _, sub := range m.Subscribes {
		if containsString(m.ownChannels, sub.Channel) {
			continue
		}
		d := manifestDependency{Channel: sub.Channel, Group: sub.Group}
		if k, ok := g.channelDeclarerLocked(sub.Channel); ok {
			if k == self {
				continue
			}
			d.Service, d.Lines = k.namespace, []string{k.version}
		}
		out = append(out, d)
	}
	return out
}

func (m serviceManifest) protoOwner() *cpv1.Owner {
	if m.Owner == nil {
		return nil
	}
	return &cpv1.Owner{Team: m.Owner.Team, Contact: m.Owner.Contact}
}
