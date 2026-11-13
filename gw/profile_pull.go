package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/iodesystems/gwag/v2/gw/profile"
	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

// Pulled services: an external or legacy service that runs no
// registration client serves its profile (profile.WellKnownPath) and
// an operator adds it by URL (`gwag ctl services add --profile-url`,
// AddServiceProfile, POST /admin/services/profiles).
//
// The gateway fetches the profile, fetches each provided spec from
// the service (Provide.URL), and registers the service through the
// same Register path a registration client takes, with Verify set
// from the profile. So a pulled service passes the same start gate
// and verification as a pushed one, and shows up in ListServices,
// the dependency graph and the metrics the same way.
//
// The gateway holds the registration: it heartbeats it itself, so
// the service sends nothing. The profile URL is the durable part: in
// memory standalone, in the gwag-pulled-services KV bucket in a
// cluster (no TTL). In a cluster one node holds each registration:
// the live peer with the lowest node id. When it goes, its registry
// entries expire and the next lowest registers the profile again.
// While ownership moves two nodes can hold one briefly; both are
// registrations of the same address.
//
// The profile and the specs are read when the profile is added, and
// again when the owner (re)registers it (gateway restart, owner
// change). A changed spec on a running service turns its instance red
// (verification compares against the registered spec); add the
// profile again to register the new one.
//
// Safety: an admin-only operation. The profile URL may name any host
// unless VerifyOptions.ProfileHosts restricts it; every other URL the
// profile names (address, spec, health, endpoint) must be on the
// profile URL's host, and redirects are not followed.

const pulledBucketName = "gwag-pulled-services"

// pulledTick is how often the pull loop reconciles the stored
// profiles with the registrations this node holds.
const pulledTick = time.Second

// pulledBeat is how often the owner heartbeats a pulled registration
// (default TTL / 3).
const pulledBeat = defaultTTL / 3

// pulledRecord is one stored profile URL (KV JSON value).
type pulledRecord struct {
	URL         string `json:"url"`
	AddedUnixMs int64  `json:"added_unix_ms"`
}

// pulledLocal is a profile this node holds a registration for, or
// failed to.
type pulledLocal struct {
	url     string
	service string
	regID   string
	err     string
	nextTry time.Time
	beatAt  time.Time
	addedMs int64
}

type pulledProfiles struct {
	mu    sync.Mutex
	recs  map[string]pulledRecord // standalone store
	local map[string]*pulledLocal // key → this node's registration

	kvMu sync.Mutex
	kv   jetstream.KeyValue
}

func pulledKey(u string) string {
	h := sha256.Sum256([]byte(u))
	return hex.EncodeToString(h[:8])
}

// setPulledKV stores the cluster bucket (opened with the other
// buckets in startClusterTracking).
func (g *Gateway) setPulledKV(kv jetstream.KeyValue) {
	g.pulled.kvMu.Lock()
	g.pulled.kv = kv
	g.pulled.kvMu.Unlock()
}

// pulledKV returns the cluster bucket; nil standalone. Unavailable
// while cluster tracking has not started.
func (g *Gateway) pulledKV(_ context.Context) (jetstream.KeyValue, error) {
	if g.cfg.cluster == nil {
		return nil, nil
	}
	g.pulled.kvMu.Lock()
	kv := g.pulled.kv
	g.pulled.kvMu.Unlock()
	if kv == nil {
		return nil, status.Error(codes.Unavailable, "controlplane: cluster tracking has not started yet; retry")
	}
	return kv, nil
}

// pulledRecords returns every stored profile, by key.
func (g *Gateway) pulledRecords(ctx context.Context) (map[string]pulledRecord, error) {
	kv, err := g.pulledKV(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]pulledRecord{}
	if kv == nil {
		g.pulled.mu.Lock()
		for k, r := range g.pulled.recs {
			out[k] = r
		}
		g.pulled.mu.Unlock()
		return out, nil
	}
	kctx, cancel := kvCtx(ctx)
	defer cancel()
	keys, err := kv.Keys(kctx)
	if errors.Is(err, jetstream.ErrNoKeysFound) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, k := range keys {
		e, err := kv.Get(kctx, k)
		if err != nil {
			continue
		}
		var r pulledRecord
		if json.Unmarshal(e.Value(), &r) == nil && r.URL != "" {
			out[k] = r
		}
	}
	return out, nil
}

// checkProfileURL refuses a URL that is not http(s) or whose host is
// not on the allow-list.
func (g *Gateway) checkProfileURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("profile url %q: want an http(s) URL", raw)
	}
	allow := g.cfg.verify.ProfileHosts
	if len(allow) == 0 {
		return u, nil
	}
	h := strings.ToLower(u.Hostname())
	for _, a := range allow {
		a = strings.ToLower(a)
		if h == a || (strings.HasPrefix(a, "*.") && strings.HasSuffix(h, a[1:])) {
			return u, nil
		}
	}
	return nil, fmt.Errorf("profile url %q: host %s is not in the allowed profile hosts", raw, h)
}

// pulledRequest reads the profile at rawURL and the specs it names,
// and builds the RegisterRequest the gateway sends itself.
func (g *Gateway) pulledRequest(ctx context.Context, rawURL string) (*cpv1.RegisterRequest, *profile.Profile, error) {
	u, err := g.checkProfileURL(rawURL)
	if err != nil {
		return nil, nil, err
	}
	c := g.verifyClient()
	ctx, cancel := context.WithTimeout(ctx, 2*g.cfg.verify.timeout())
	defer cancel()
	body, err := profile.Fetch(ctx, c, u.String())
	if err != nil {
		return nil, nil, err
	}
	p, err := profile.Parse(body)
	if err != nil {
		return nil, nil, err
	}
	if err := p.Validate(profile.ModePull); err != nil {
		return nil, nil, fmt.Errorf("profile %s: %w", rawURL, err)
	}
	origin := &url.URL{Scheme: u.Scheme, Host: u.Host}
	addr := p.Address
	if addr == "" {
		addr = origin.String()
	}
	// Every URL the profile names must be on the profile's host.
	if _, err := profile.Resolve(origin.String(), addr); err != nil && strings.Contains(addr, "://") {
		return nil, nil, fmt.Errorf("profile %s: address: %w", rawURL, err)
	} else if !strings.Contains(addr, "://") {
		if b, err := profile.BaseURL(addr); err != nil || !strings.EqualFold(b.Hostname(), origin.Hostname()) {
			return nil, nil, fmt.Errorf("profile %s: address %q is not on the profile's host %s", rawURL, addr, origin.Hostname())
		}
	}
	var deps []*cpv1.Dependency
	for _, d := range p.DependsSorted() {
		deps = append(deps, &cpv1.Dependency{Service: d.Service, Lines: d.Lines})
	}
	var subs []*cpv1.Subscription
	for _, s := range p.Subscribes {
		subs = append(subs, &cpv1.Subscription{Channel: s.Channel, Group: s.Group})
	}
	var owner *cpv1.Owner
	if p.Owner != nil {
		owner = &cpv1.Owner{Team: p.Owner.Team, Contact: p.Owner.Contact}
	}
	req := &cpv1.RegisterRequest{Addr: addr, InstanceId: "pull:" + rawURL}
	for _, v := range p.Versions() {
		pv := p.Provides[v]
		b := &cpv1.ServiceBinding{
			Namespace:  p.Service,
			Version:    v,
			Depends:    deps,
			Owner:      owner,
			Public:     p.Public,
			Subscribes: subs,
		}
		verify := &cpv1.Verify{HealthUrl: p.HealthURL(v)}
		switch pv.Kind {
		case profile.KindGraphQL:
			ep, err := profile.Resolve(origin.String(), pv.Endpoint)
			if err != nil {
				return nil, nil, fmt.Errorf("profile %s: provides.%s.endpoint: %w", rawURL, v, err)
			}
			b.GraphqlEndpoint = ep
		default:
			specURL, err := profile.Resolve(origin.String(), pv.URL)
			if err != nil {
				return nil, nil, fmt.Errorf("profile %s: provides.%s.url: %w", rawURL, v, err)
			}
			spec, err := profile.Fetch(ctx, c, specURL)
			if err != nil {
				return nil, nil, fmt.Errorf("profile %s: provides.%s: %w", rawURL, v, err)
			}
			if pv.Kind == profile.KindOpenAPI {
				b.OpenapiSpec = spec
			} else {
				b.ProtoSource = spec
			}
			verify.SpecUrl = specURL
		}
		if verify.HealthUrl != "" {
			if verify.HealthUrl, err = profile.Resolve(origin.String(), verify.HealthUrl); err != nil {
				return nil, nil, fmt.Errorf("profile %s: health: %w", rawURL, err)
			}
		}
		b.Verify = verify
		req.Services = append(req.Services, b)
	}
	return req, p, nil
}

// pulledOwner reports whether this node holds the registrations of
// pulled profiles: always standalone; the lowest live node id in a
// cluster.
func (g *Gateway) pulledOwner() bool {
	if g.cfg.cluster == nil {
		return true
	}
	g.mu.Lock()
	t := g.peers
	g.mu.Unlock()
	if t == nil {
		return false
	}
	live := t.LivePeers()
	return len(live) > 0 && live[0] == g.nodeID()
}

// registerPulled (re)registers one profile on this node, replacing a
// registration it held for the same key.
func (cp *controlPlane) registerPulled(ctx context.Context, key string, rec pulledRecord) ([]*cpv1.BindingStatus, string, error) {
	g := cp.gw
	req, p, err := g.pulledRequest(ctx, rec.URL)
	service := ""
	if p != nil {
		service = p.Service
	}
	g.pulled.mu.Lock()
	if g.pulled.local == nil {
		g.pulled.local = map[string]*pulledLocal{}
	}
	loc := g.pulled.local[key]
	if loc == nil {
		loc = &pulledLocal{url: rec.URL}
		g.pulled.local[key] = loc
	}
	old := loc.regID
	g.pulled.mu.Unlock()
	var resp *cpv1.RegisterResponse
	if err == nil {
		if old != "" {
			_, _ = cp.Deregister(context.Background(), &cpv1.DeregisterRequest{RegistrationId: old})
		}
		// A context without peer info: the in-process call needs no
		// app key.
		resp, err = cp.Register(context.WithoutCancel(ctx), req)
	}
	g.pulled.mu.Lock()
	defer g.pulled.mu.Unlock()
	loc.url, loc.addedMs = rec.URL, rec.AddedUnixMs
	if service != "" {
		loc.service = service
	}
	if err != nil {
		loc.err = err.Error()
		loc.nextTry = time.Now().Add(g.cfg.verify.retry())
		if old != "" {
			loc.regID = ""
		}
		return nil, service, err
	}
	loc.err = ""
	loc.regID = resp.GetRegistrationId()
	loc.beatAt = time.Now()
	return resp.GetBindings(), service, nil
}

// dropPulled deregisters this node's registration of key.
func (cp *controlPlane) dropPulled(key string) {
	g := cp.gw
	g.pulled.mu.Lock()
	loc := g.pulled.local[key]
	delete(g.pulled.local, key)
	g.pulled.mu.Unlock()
	if loc != nil && loc.regID != "" {
		_, _ = cp.Deregister(context.Background(), &cpv1.DeregisterRequest{RegistrationId: loc.regID})
	}
}

// pullLoop keeps this node's registrations of pulled profiles in line
// with the store: registers the ones it owns and does not hold (or
// whose registration expired), heartbeats the ones it holds, drops
// the rest. Stops with the gateway.
func (cp *controlPlane) pullLoop() {
	t := time.NewTicker(pulledTick)
	defer t.Stop()
	for {
		select {
		case <-cp.gw.life.Done():
			return
		case <-t.C:
		}
		cp.pullOnce(cp.gw.life)
	}
}

func (cp *controlPlane) pullOnce(ctx context.Context) {
	g := cp.gw
	if g.cfg.cluster == nil {
		g.pulled.mu.Lock()
		n := len(g.pulled.recs) + len(g.pulled.local)
		g.pulled.mu.Unlock()
		if n == 0 {
			return
		}
	} else if g.registryKV() == nil {
		return
	}
	recs, err := g.pulledRecords(ctx)
	if err != nil {
		return
	}
	owner := g.pulledOwner()
	g.pulled.mu.Lock()
	var drop []string
	for k := range g.pulled.local {
		if _, ok := recs[k]; !ok || !owner {
			drop = append(drop, k)
		}
	}
	g.pulled.mu.Unlock()
	for _, k := range drop {
		cp.dropPulled(k)
	}
	if !owner {
		return
	}
	now := time.Now()
	for k, rec := range recs {
		g.pulled.mu.Lock()
		loc := g.pulled.local[k]
		var regID string
		due, beat := true, false
		if loc != nil {
			regID = loc.regID
			due = loc.regID == "" && !now.Before(loc.nextTry)
			beat = now.Sub(loc.beatAt) >= pulledBeat
			if beat {
				loc.beatAt = now
			}
		}
		g.pulled.mu.Unlock()
		if regID != "" {
			if !beat {
				continue
			}
			resp, err := cp.Heartbeat(ctx, &cpv1.HeartbeatRequest{RegistrationId: regID})
			if err == nil && resp.GetOk() {
				continue
			}
			due = true
		}
		if due {
			if _, _, err := cp.registerPulled(ctx, k, rec); err != nil {
				cp.logf("controlplane: pulled profile %s: %v", rec.URL, err)
			}
		}
	}
}

// AddServiceProfile implements the RPC (see control.proto).
func (cp *controlPlane) AddServiceProfile(ctx context.Context, req *cpv1.AddServiceProfileRequest) (*cpv1.AddServiceProfileResponse, error) {
	if err := cp.gw.controlPlaneAdmin(ctx, "add_service_profile"); err != nil {
		return nil, err
	}
	bindings, service, err := cp.gw.addServiceProfile(ctx, req.GetUrl())
	if err != nil {
		return nil, err
	}
	return &cpv1.AddServiceProfileResponse{Service: service, Bindings: bindings}, nil
}

// addServiceProfile reads and checks the profile, stores its URL,
// and registers it when this node is the owner.
func (g *Gateway) addServiceProfile(ctx context.Context, rawURL string) ([]*cpv1.BindingStatus, string, error) {
	cp, _ := g.ControlPlane().(*controlPlane)
	// Read it once now, so a bad profile is refused to the operator
	// instead of retried in the background.
	if _, _, err := g.pulledRequest(ctx, rawURL); err != nil {
		return nil, "", status.Error(codes.InvalidArgument, "controlplane: "+err.Error())
	}
	key := pulledKey(rawURL)
	rec := pulledRecord{URL: rawURL, AddedUnixMs: time.Now().UnixMilli()}
	kv, err := g.pulledKV(ctx)
	if err != nil {
		return nil, "", err
	}
	if kv != nil {
		raw, _ := json.Marshal(rec)
		kctx, cancel := context.WithTimeout(ctx, 2*kvCallTimeout)
		err := kvPutRetry(kctx, kv, key, raw)
		cancel()
		if err != nil {
			return nil, "", status.Errorf(codes.Unavailable, "controlplane: store profile: %v", err)
		}
	} else {
		g.pulled.mu.Lock()
		if g.pulled.recs == nil {
			g.pulled.recs = map[string]pulledRecord{}
		}
		g.pulled.recs[key] = rec
		g.pulled.mu.Unlock()
	}
	if !g.pulledOwner() {
		return nil, "", nil
	}
	bindings, service, err := cp.registerPulled(ctx, key, rec)
	if err != nil {
		return nil, service, status.Error(codes.FailedPrecondition, err.Error())
	}
	return bindings, service, nil
}

// RemoveServiceProfile implements the RPC.
func (cp *controlPlane) RemoveServiceProfile(ctx context.Context, req *cpv1.RemoveServiceProfileRequest) (*cpv1.RemoveServiceProfileResponse, error) {
	if err := cp.gw.controlPlaneAdmin(ctx, "remove_service_profile"); err != nil {
		return nil, err
	}
	ok, err := cp.gw.removeServiceProfile(ctx, req.GetUrl())
	if err != nil {
		return nil, err
	}
	return &cpv1.RemoveServiceProfileResponse{Removed: ok}, nil
}

func (g *Gateway) removeServiceProfile(ctx context.Context, rawURL string) (bool, error) {
	cp, _ := g.ControlPlane().(*controlPlane)
	key := pulledKey(rawURL)
	recs, err := g.pulledRecords(ctx)
	if err != nil {
		return false, err
	}
	_, found := recs[key]
	kv, err := g.pulledKV(ctx)
	if err != nil {
		return false, err
	}
	if kv != nil {
		if found {
			if err := kvDeleteRetry(ctx, kv, key); err != nil {
				return false, status.Errorf(codes.Unavailable, "controlplane: delete profile: %v", err)
			}
		}
	} else {
		g.pulled.mu.Lock()
		delete(g.pulled.recs, key)
		g.pulled.mu.Unlock()
	}
	cp.dropPulled(key)
	return found, nil
}

// ListServiceProfiles implements the RPC.
func (cp *controlPlane) ListServiceProfiles(ctx context.Context, _ *cpv1.ListServiceProfilesRequest) (*cpv1.ListServiceProfilesResponse, error) {
	list, err := cp.gw.listServiceProfiles(ctx)
	if err != nil {
		return nil, err
	}
	return &cpv1.ListServiceProfilesResponse{Profiles: list}, nil
}

// listServiceProfiles lists the stored profiles; service, error and
// owner node come from this node's view (filled only on the owner).
func (g *Gateway) listServiceProfiles(ctx context.Context) ([]*cpv1.ServiceProfile, error) {
	recs, err := g.pulledRecords(ctx)
	if err != nil {
		return nil, err
	}
	owner := ""
	if g.cfg.cluster != nil {
		g.mu.Lock()
		t := g.peers
		g.mu.Unlock()
		if t != nil {
			if live := t.LivePeers(); len(live) > 0 {
				owner = live[0]
			}
		}
	}
	out := make([]*cpv1.ServiceProfile, 0, len(recs))
	g.pulled.mu.Lock()
	for k, r := range recs {
		sp := &cpv1.ServiceProfile{Url: r.URL, AddedUnixMs: r.AddedUnixMs, OwnerNode: owner}
		if loc := g.pulled.local[k]; loc != nil {
			sp.Service = loc.service
			sp.Error = loc.err
		}
		out = append(out, sp)
	}
	g.pulled.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].GetUrl() < out[j].GetUrl() })
	return out, nil
}

// Admin routes (self-ingested like every admin route).
func registerServiceProfiles(api huma.API, g *Gateway) {
	huma.Register(api, huma.Operation{
		OperationID: "listServiceProfiles",
		Method:      "GET",
		Path:        "/admin/services/profiles",
		Summary:     "List pulled services: the profile URLs the gateway registers services from (external or legacy services with no registration client).",
	}, func(ctx context.Context, _ *struct{}) (*serviceProfilesOut, error) {
		list, err := g.listServiceProfiles(ctx)
		if err != nil {
			return nil, huma.Error503ServiceUnavailable(err.Error())
		}
		out := &serviceProfilesOut{}
		out.Body.Profiles = []serviceProfileInfo{}
		for _, p := range list {
			out.Body.Profiles = append(out.Body.Profiles, serviceProfileInfo{
				URL: p.GetUrl(), Service: p.GetService(), AddedUnixMs: p.GetAddedUnixMs(), Error: p.GetError(), OwnerNode: p.GetOwnerNode(),
			})
		}
		return out, nil
	})
	huma.Register(api, huma.Operation{
		OperationID: "addServiceProfile",
		Method:      "POST",
		Path:        "/admin/services/profiles",
		Summary:     "Add a pulled service: the gateway reads the service profile at url (e.g. http://billing:8080/.well-known/gwag.json) and the specs it names, registers the service itself and verifies each instance (address, health, spec). Adding the same url again re-reads it.",
	}, func(ctx context.Context, in *serviceProfileIn) (*addServiceProfileOut, error) {
		bindings, service, err := g.addServiceProfile(ctx, in.Body.URL)
		if err != nil {
			if status.Code(err) == codes.InvalidArgument || status.Code(err) == codes.FailedPrecondition {
				return nil, huma.Error400BadRequest(status.Convert(err).Message())
			}
			return nil, huma.Error503ServiceUnavailable(err.Error())
		}
		out := &addServiceProfileOut{}
		out.Body.Service = service
		out.Body.Bindings = []bindingStatusInfo{}
		for _, b := range bindings {
			out.Body.Bindings = append(out.Body.Bindings, adminBindingStatus(b))
		}
		return out, nil
	})
	huma.Register(api, huma.Operation{
		OperationID: "removeServiceProfile",
		Method:      "POST",
		Path:        "/admin/services/profiles/remove",
		Summary:     "Remove a pulled service: drop the stored profile url and the registration the gateway holds for it.",
	}, func(ctx context.Context, in *serviceProfileIn) (*removeServiceProfileOut, error) {
		ok, err := g.removeServiceProfile(ctx, in.Body.URL)
		if err != nil {
			return nil, huma.Error503ServiceUnavailable(err.Error())
		}
		out := &removeServiceProfileOut{}
		out.Body.Removed = ok
		return out, nil
	})
}

type serviceProfileInfo struct {
	URL         string `json:"url"`
	Service     string `json:"service" doc:"Namespace from the profile; set on the node that holds the registration."`
	AddedUnixMs int64  `json:"addedUnixMs"`
	Error       string `json:"error" doc:"Last error reading or registering the profile, on the node that holds it; empty when registered."`
	OwnerNode   string `json:"ownerNode" doc:"Cluster node that holds the registration; empty standalone."`
}

type serviceProfilesOut struct {
	Body struct {
		Profiles []serviceProfileInfo `json:"profiles"`
	}
}

type serviceProfileIn struct {
	Body struct {
		URL string `json:"url" doc:"http(s) URL of the service profile."`
	}
}

type bindingStatusInfo struct {
	Namespace     string   `json:"namespace"`
	Version       string   `json:"version"`
	State         string   `json:"state" enum:"ready,waiting,degraded"`
	Verification  string   `json:"verification" enum:"none,pending,green,red" doc:"none: the binding declares no verify."`
	VerifyReasons []string `json:"verifyReasons"`
}

func adminBindingStatus(b *cpv1.BindingStatus) bindingStatusInfo {
	return bindingStatusInfo{
		Namespace:     b.GetNamespace(),
		Version:       b.GetVersion(),
		State:         adminInstanceState(b.GetState()),
		Verification:  adminVerifyState(b.GetVerification()),
		VerifyReasons: append([]string{}, b.GetVerifyReasons()...),
	}
}

type addServiceProfileOut struct {
	Body struct {
		Service  string              `json:"service"`
		Bindings []bindingStatusInfo `json:"bindings" doc:"State of each provided version after registering; empty when another cluster node holds the registration."`
	}
}

type removeServiceProfileOut struct {
	Body struct {
		Removed bool `json:"removed"`
	}
}
