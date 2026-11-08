package gateway

import (
	"context"
	"errors"
	"sort"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// Control-plane checks with WithRequireAppKeys.
//
//	Register, Heartbeat, Deregister   app key covering every namespace
//	DrainInstance                     of the registration, or admin
//	ForgetPeer, RetractStable,        admin token
//	Deprecate, Undeprecate
//	ListRegistrations, ListPeers,     open (reads)
//	ListServices
//	SignSubscriptionToken             its own check (signer or admin)
//
// Register checks the key before it compiles or introspects anything,
// and the namespaces after. In-process calls (no gRPC peer: the
// janitor, AdminHumaRouter handlers, embedders, tests) are not
// checked; the admin routes have AdminMiddleware.

// App-key operations: the op label of app_key_auth_total and the
// operation the AppKeyAuthorizer delegate is told.
const (
	appKeyOpRegister    = "register"
	appKeyOpHeartbeat   = "heartbeat"
	appKeyOpDeregister  = "deregister"
	appKeyOpDrain       = "drain_instance"
	appKeyOpSchemaWrite = "schema_registry_write"
)

// cpCaller is the checked caller of one control-plane RPC.
type cpCaller struct {
	// open: nothing to check (app keys not required, in-process call,
	// or the admin token).
	open  bool
	op    string
	grant appKeyGrant
}

// controlPlaneCaller resolves the caller of a control-plane RPC. A
// valid key returns a caller whose cover must be called with the
// namespaces the RPC touches. Records failures on app_key_auth_total.
func (g *Gateway) controlPlaneCaller(ctx context.Context, op string) (cpCaller, error) {
	if !g.cfg.requireAppKeys {
		return cpCaller{open: true}, nil
	}
	p, ok := peer.FromContext(ctx)
	if !ok {
		return cpCaller{open: true}, nil
	}
	remote := ""
	if p.Addr != nil {
		remote = p.Addr.String()
	}
	md, _ := metadata.FromIncomingContext(ctx)
	tok := bearerFromMetadata(md)
	if tok == "" {
		g.recordAppKeyAuth(op, appKeyOutcome(errAppKeyMissing))
		return cpCaller{}, appKeyStatus(errAppKeyMissing)
	}
	if g.isAdminBearer(ctx, tok, op, remote) {
		g.recordAppKeyAuth(op, "ok_admin")
		return cpCaller{open: true}, nil
	}
	grant, err := g.checkAppKey(ctx, tok, op, nil, remote)
	if err != nil {
		g.recordAppKeyAuth(op, appKeyOutcome(err))
		return cpCaller{}, appKeyStatus(err)
	}
	return cpCaller{op: op, grant: grant}, nil
}

// cover checks that the key covers namespaces and records the result.
func (c cpCaller) cover(g *Gateway, namespaces []string) error {
	if c.open {
		return nil
	}
	err := c.grant.covers(namespaces)
	g.recordAppKeyAuth(c.op, appKeyOutcome(err))
	return appKeyStatus(err)
}

// controlPlaneAdmin is the check of the operator RPCs: with
// WithRequireAppKeys a remote caller needs the admin token.
func (g *Gateway) controlPlaneAdmin(ctx context.Context, op string) error {
	if !g.cfg.requireAppKeys {
		return nil
	}
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil
	}
	remote := ""
	if p.Addr != nil {
		remote = p.Addr.String()
	}
	md, _ := metadata.FromIncomingContext(ctx)
	tok := bearerFromMetadata(md)
	if tok == "" {
		g.recordAppKeyAuth(op, "missing")
		return status.Error(codes.Unauthenticated, "controlplane: "+op+" needs the admin token")
	}
	if g.isAdminBearer(ctx, tok, op, remote) {
		g.recordAppKeyAuth(op, "ok_admin")
		return nil
	}
	g.recordAppKeyAuth(op, "admin_required")
	return status.Error(codes.PermissionDenied, "controlplane: "+op+" needs the admin token")
}

// isAdminBearer reports whether tok is the admin token, or a token
// the AdminAuthorizer delegate accepts with the admin role. A minted
// app key is never sent to the admin checks.
func (g *Gateway) isAdminBearer(ctx context.Context, tok, op, remote string) bool {
	if _, ok := parseAppKey(tok); ok {
		return false
	}
	role, _, _, st := g.authenticateAdminSubject(ctx, tok, "GRPC", "/gateway.controlplane.v1.ControlPlane/"+op, remote, true)
	return st == 0 && role == adminRoleAdmin
}

// appKeyStatus maps a check error to a gRPC status error.
func appKeyStatus(err error) error {
	if err == nil {
		return nil
	}
	var nsErr *appKeyNamespaceError
	switch {
	case errors.As(err, &nsErr):
		return status.Error(codes.PermissionDenied, "controlplane: "+err.Error())
	case errors.Is(err, errAppKeyUnavailable):
		return status.Error(codes.Unavailable, "controlplane: "+err.Error())
	}
	return status.Error(codes.Unauthenticated, "controlplane: "+err.Error())
}

// lineNamespaces returns the namespaces of "ns/version" lines, sorted
// and without repeats.
func lineNamespaces(lines []string) []string {
	var out []string
	for _, l := range lines {
		ns, _, _ := strings.Cut(l, "/")
		if !containsString(out, ns) {
			out = append(out, ns)
		}
	}
	sort.Strings(out)
	return out
}

// drainInstanceNamespaces returns the namespaces of the registration
// a DrainInstance names, from the cluster-wide drain view.
func (g *Gateway) drainInstanceNamespaces(name string) ([]string, error) {
	regID, err := g.drains.resolveInstance(name)
	if err != nil {
		return nil, err
	}
	d := &g.drains
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	if rec := d.regs[regID]; rec != nil {
		for k := range rec.lines {
			if !containsString(out, k.namespace) {
				out = append(out, k.namespace)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}
