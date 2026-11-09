package controlclient

import (
	"fmt"
	"path/filepath"

	"github.com/iodesystems/gwag/v2/gw/profile"
)

// LoadProfile reads a service profile (gwag.yaml or gwag.json, see
// package profile and `gwag service init`) and returns one Service
// per provided version: the spec read from the file the profile
// names (proto imports from its import directories), Depends, Owner,
// Public, Subscribes, and HealthURL / SpecURL so the gateway
// verifies the instance. serviceAddr is Options.ServiceAddr; a
// GraphQL endpoint given as a path is resolved against it.
//
//	svcs, err := controlclient.LoadProfile("gwag.yaml", addr)
//	reg, err := controlclient.SelfRegister(ctx, controlclient.Options{
//	    GatewayAddr: gw, ServiceAddr: addr, Services: svcs,
//	})
//
// Stability: experimental
func LoadProfile(path, serviceAddr string) ([]Service, error) {
	p, err := profile.Load(path)
	if err != nil {
		return nil, fmt.Errorf("controlclient: %w", err)
	}
	if err := p.Validate(profile.ModePush); err != nil {
		return nil, fmt.Errorf("controlclient: profile %s: %w", path, err)
	}
	return ServicesFromProfile(p, filepath.Dir(path), serviceAddr)
}

// ServicesFromProfile is LoadProfile for a profile already parsed;
// dir is the directory spec files are relative to.
//
// Stability: experimental
func ServicesFromProfile(p *profile.Profile, dir, serviceAddr string) ([]Service, error) {
	var deps []Dependency
	for _, d := range p.DependsSorted() {
		deps = append(deps, Dependency{Service: d.Service, Lines: d.Lines})
	}
	var subs []Subscription
	for _, s := range p.Subscribes {
		subs = append(subs, Subscription{Channel: s.Channel, Group: s.Group})
	}
	var owner *Owner
	if p.Owner != nil {
		owner = &Owner{Team: p.Owner.Team, Contact: p.Owner.Contact}
	}
	var out []Service
	for _, v := range p.Versions() {
		pv := p.Provides[v]
		s := Service{
			Namespace:  p.Service,
			Version:    v,
			Depends:    deps,
			Owner:      owner,
			Public:     p.Public,
			Subscribes: subs,
			HealthURL:  p.HealthURL(v),
			SpecURL:    pv.URL,
		}
		switch pv.Kind {
		case profile.KindGraphQL:
			ep := pv.Endpoint
			if ep != "" && ep[0] == '/' {
				if serviceAddr == "" {
					return nil, fmt.Errorf("controlclient: provides.%s.endpoint %q is a path; pass the service address", v, ep)
				}
				r, err := profile.Resolve(serviceAddr, ep)
				if err != nil {
					return nil, fmt.Errorf("controlclient: provides.%s.endpoint: %w", v, err)
				}
				ep = r
			}
			s.GraphQLEndpoint = ep
			s.SpecURL = ""
		case profile.KindOpenAPI:
			b, _, err := p.ReadSpec(dir, v)
			if err != nil {
				return nil, fmt.Errorf("controlclient: provides.%s: %w", v, err)
			}
			s.OpenAPISpec = b
		default:
			src, imports, err := p.ReadSpec(dir, v)
			if err != nil {
				return nil, fmt.Errorf("controlclient: provides.%s: %w", v, err)
			}
			s.ProtoSource = src
			if len(imports) > 0 {
				s.ProtoImports = imports
			}
		}
		out = append(out, s)
	}
	return out, nil
}
