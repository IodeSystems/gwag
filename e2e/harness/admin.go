//go:build e2e

package harness

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

// Admin is the gateway's admin surface: the /api/admin HTTP routes
// (writes carry the boot token as a bearer) and the control-plane gRPC
// calls the gwag CLI uses (drain, registrations).
type Admin struct{ g *Gateway }

// Admin returns the admin client for g.
func (g *Gateway) Admin() *Admin { return &Admin{g: g} }

// Do sends an admin request with the bearer token. path is relative
// to /api, e.g. "/admin/services".
func (a *Admin) Do(ctx context.Context, method, path string, body, out any) error {
	_, err := a.g.do(ctx, method, "/api"+path, body, out, []ReqOpt{Bearer(a.g.Token())})
	return err
}

// ---- services -------------------------------------------------------

// Services is GET /api/admin/services.
type Services struct {
	Environment string    `json:"environment"`
	Services    []Service `json:"services"`
	StableVN    []struct {
		Namespace string `json:"namespace"`
		VN        uint32 `json:"vN"`
	} `json:"stableVN"`
}

// Service is one (namespace, version) in Services.
type Service struct {
	Namespace               string `json:"namespace"`
	Version                 string `json:"version"`
	HashHex                 string `json:"hashHex"`
	ReplicaCount            uint32 `json:"replicaCount"`
	ManualDeprecationReason string `json:"manualDeprecationReason"`
	RejectedJoins           *struct {
		Count      uint32 `json:"count"`
		LastReason string `json:"lastReason"`
	} `json:"rejectedJoins"`
	Depends []struct {
		Service string   `json:"service"`
		Lines   []string `json:"lines"`
	} `json:"depends"`
	Owner *struct {
		Team    string `json:"team"`
		Contact string `json:"contact"`
	} `json:"owner"`
	Drains []struct {
		RegistrationID string   `json:"registrationId"`
		InstanceID     string   `json:"instanceId"`
		State          string   `json:"state"`
		Reasons        []string `json:"reasons"`
		Inflight       int32    `json:"inflight"`
	} `json:"drains"`
	Instances []Instance `json:"instances"`
}

// Instance is one registered instance of a Service.
type Instance struct {
	ReplicaID      string `json:"replicaId"`
	RegistrationID string `json:"registrationId"`
	InstanceID     string `json:"instanceId"`
	Addr           string `json:"addr"`
	State          string `json:"state"` // ready, waiting, degraded
	Depends        []struct {
		Service      string   `json:"service"`
		Lines        []string `json:"lines"`
		PresentLines []string `json:"presentLines"`
	} `json:"depends"`
}

// Services lists services (GET /api/admin/services).
func (a *Admin) Services(ctx context.Context) (*Services, error) {
	var out Services
	if err := a.Do(ctx, http.MethodGet, "/admin/services", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Service returns (namespace, version) from Services, or nil.
func (s *Services) Service(namespace, version string) *Service {
	for i := range s.Services {
		if s.Services[i].Namespace == namespace && s.Services[i].Version == version {
			return &s.Services[i]
		}
	}
	return nil
}

// Instance returns the instance with instance id id, or nil.
func (s *Service) Instance(id string) *Instance {
	for i := range s.Instances {
		if s.Instances[i].InstanceID == id {
			return &s.Instances[i]
		}
	}
	return nil
}

// WaitInstance waits until the gateway lists instance id on
// (namespace, version) in the given state ("ready", "waiting",
// "degraded"); state "" waits until the instance is gone.
func (a *Admin) WaitInstance(namespace, version, id, state string, timeout time.Duration) {
	t := a.g.env.t
	t.Helper()
	desc := fmt.Sprintf("%s lists %s on %s/%s as %q", a.g.Name, id, namespace, version, state)
	if state == "" {
		desc = fmt.Sprintf("%s no longer lists %s on %s/%s", a.g.Name, id, namespace, version)
	}
	Eventually(t, timeout, desc, func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ss, err := a.Services(ctx)
		if err != nil {
			return err
		}
		var inst *Instance
		if svc := ss.Service(namespace, version); svc != nil {
			inst = svc.Instance(id)
		}
		switch {
		case state == "" && inst == nil:
			return nil
		case state == "":
			return fmt.Errorf("still listed, state %s", inst.State)
		case inst == nil:
			return fmt.Errorf("not listed")
		case inst.State != state:
			return fmt.Errorf("state %s", inst.State)
		}
		return nil
	})
}

// Deprecate marks (namespace, version) deprecated with reason.
func (a *Admin) Deprecate(ctx context.Context, namespace, version, reason string) error {
	return a.Do(ctx, http.MethodPost, "/admin/services/"+namespace+"/"+version+"/deprecate",
		map[string]any{"reason": reason}, nil)
}

// Undeprecate clears a manual deprecation.
func (a *Admin) Undeprecate(ctx context.Context, namespace, version string) error {
	return a.Do(ctx, http.MethodPost, "/admin/services/"+namespace+"/"+version+"/undeprecate",
		map[string]any{}, nil)
}

// ---- graph ----------------------------------------------------------

// Graph is GET /api/admin/graph: the dependency graph.
type Graph struct {
	Environment  string        `json:"environment"`
	Window       string        `json:"window"`
	CallerSource string        `json:"callerSource"`
	Nodes        []GraphNode   `json:"nodes"`
	DesiredEdges []DesiredEdge `json:"desiredEdges"`
	ActiveEdges  []ActiveEdge  `json:"activeEdges"`

	ExternalDependents []ExternalDependent `json:"externalDependents"`
}

// GraphNode is one (namespace, version) in the graph.
type GraphNode struct {
	ID            string `json:"id"` // namespace/version
	Namespace     string `json:"namespace"`
	Version       string `json:"version"`
	Kind          string `json:"kind"`
	ReplicaCount  uint32 `json:"replicaCount"`
	ReadyCount    int    `json:"readyCount"`
	WaitingCount  int    `json:"waitingCount"`
	DegradedCount int    `json:"degradedCount"`
	DrainingCount int    `json:"drainingCount"`
	TrafficCount  uint64 `json:"trafficCount"`
	Sunsetable    bool   `json:"sunsetable"`
	SunsetReason  string `json:"sunsetReason"`
	Owner         *struct {
		Team string `json:"team"`
	} `json:"owner"`
	Instances []struct {
		RegistrationID string   `json:"registrationId"`
		InstanceID     string   `json:"instanceId"`
		Addr           string   `json:"addr"`
		State          string   `json:"state"`
		DrainState     string   `json:"drainState"`
		DrainReasons   []string `json:"drainReasons"`
	} `json:"instances"`
}

// DesiredEdge is a declared dependency.
type DesiredEdge struct {
	ConsumerKind      string   `json:"consumerKind"` // service, external
	ConsumerNamespace string   `json:"consumerNamespace"`
	ConsumerVersion   string   `json:"consumerVersion"`
	Provider          string   `json:"provider"`
	Lines             []string `json:"lines"`
	PresentLines      []string `json:"presentLines"`
	Met               bool     `json:"met"`
	InstanceCount     int      `json:"instanceCount"`
}

// ActiveEdge is observed traffic from a caller to (namespace, version).
type ActiveEdge struct {
	Caller            string `json:"caller"`
	ConsumerNamespace string `json:"consumerNamespace"`
	ConsumerVersion   string `json:"consumerVersion"`
	Namespace         string `json:"namespace"`
	Version           string `json:"version"`
	Count             uint64 `json:"count"`
	OkCount           uint64 `json:"okCount"`
	Methods           []struct {
		Method  string `json:"method"`
		Count   uint64 `json:"count"`
		OkCount uint64 `json:"okCount"`
	} `json:"methods"`
}

// Graph fetches the dependency graph. window: "1m", "1h" or "24h"
// ("" = server default, 1h).
func (a *Admin) Graph(ctx context.Context, window string) (*Graph, error) {
	path := "/admin/graph"
	if window != "" {
		path += "?window=" + url.QueryEscape(window)
	}
	var out Graph
	if err := a.Do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Node returns the node with id "namespace/version", or nil.
func (g *Graph) Node(id string) *GraphNode {
	for i := range g.Nodes {
		if g.Nodes[i].ID == id {
			return &g.Nodes[i]
		}
	}
	return nil
}

// Edge returns the active edge from caller to namespace/version, or
// nil.
func (g *Graph) Edge(caller, namespace, version string) *ActiveEdge {
	for i := range g.ActiveEdges {
		e := &g.ActiveEdges[i]
		if e.Caller == caller && e.Namespace == namespace && e.Version == version {
			return e
		}
	}
	return nil
}

// ---- external dependents -------------------------------------------

// ExternalDependent is a declared consumer the gateway cannot see
// (GET /api/admin/external-dependents, and the graph).
type ExternalDependent struct {
	ID    string `json:"id"`
	Name  string `json:"name,omitempty"`
	Kind  string `json:"kind"`
	Owner *struct {
		Team    string `json:"team,omitempty"`
		Contact string `json:"contact,omitempty"`
	} `json:"owner,omitempty"`
	Depends []ExternalPin `json:"depends"`
	Note    string        `json:"note,omitempty"`
	// ReviewAt is RFC 3339, required on PUT.
	ReviewAt   string `json:"reviewAt,omitempty"`
	ReviewDue  bool   `json:"reviewDue,omitempty"`
	ReviewedAt string `json:"reviewedAt,omitempty"`
	ReviewedBy string `json:"reviewedBy,omitempty"`
}

// ExternalPin is one pinned service and its accepted lines.
type ExternalPin struct {
	Service string   `json:"service"`
	Lines   []string `json:"lines"`
}

// PutExternalDependent creates or replaces x (PUT
// /api/admin/external-dependents/{id}) and returns the warnings.
func (a *Admin) PutExternalDependent(ctx context.Context, x ExternalDependent) ([]string, error) {
	body := map[string]any{"kind": x.Kind, "depends": x.Depends}
	if x.Name != "" {
		body["name"] = x.Name
	}
	if x.Owner != nil {
		body["owner"] = x.Owner
	}
	if x.Note != "" {
		body["note"] = x.Note
	}
	if x.ReviewAt != "" {
		body["reviewAt"] = x.ReviewAt
	}
	var out struct {
		Warnings []string `json:"warnings"`
	}
	err := a.Do(ctx, http.MethodPut, "/admin/external-dependents/"+url.PathEscape(x.ID), body, &out)
	return out.Warnings, err
}

// DeleteExternalDependent deletes the declaration with id.
func (a *Admin) DeleteExternalDependent(ctx context.Context, id string) error {
	return a.Do(ctx, http.MethodDelete, "/admin/external-dependents/"+url.PathEscape(id), nil, nil)
}

// ExternalDependents lists the declarations.
func (a *Admin) ExternalDependents(ctx context.Context) ([]ExternalDependent, error) {
	var out struct {
		ExternalDependents []ExternalDependent `json:"externalDependents"`
	}
	if err := a.Do(ctx, http.MethodGet, "/admin/external-dependents", nil, &out); err != nil {
		return nil, err
	}
	return out.ExternalDependents, nil
}

// ---- control plane --------------------------------------------------

// DrainInstance asks the gateway to drain an instance (registration id
// or instance id) and returns the drain state; cancel withdraws the
// drain. Same call as `gwag drain`; repeating it is how `gwag drain
// --wait` polls.
func (a *Admin) DrainInstance(ctx context.Context, instance string, cancel bool) (*cpv1.DrainInstanceResponse, error) {
	return a.g.ControlPlane().DrainInstance(ctx, &cpv1.DrainInstanceRequest{Instance: instance, Cancel: cancel})
}

// WaitDrainState waits until the graph shows instance (instance id or
// registration id) in drain state want: "serving", "draining",
// "blocked" or "safe_to_stop". It reads the graph, so it requests
// nothing; DrainInstance is the request.
func (a *Admin) WaitDrainState(instance, want string, timeout time.Duration) {
	t := a.g.env.t
	t.Helper()
	Eventually(t, timeout, fmt.Sprintf("%s: %s drain state %s", a.g.Name, instance, want), func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		g, err := a.Graph(ctx, "")
		if err != nil {
			return err
		}
		for _, n := range g.Nodes {
			for _, in := range n.Instances {
				if in.InstanceID != instance && in.RegistrationID != instance {
					continue
				}
				if in.DrainState != want {
					return fmt.Errorf("%s: state %s, reasons %v", n.ID, in.DrainState, in.DrainReasons)
				}
				return nil
			}
		}
		return fmt.Errorf("instance not in the graph")
	})
}

// DrainStateName is the lower-case short name of a drain state, as
// `gwag drain` prints it.
func DrainStateName(s cpv1.DrainState) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), "DRAIN_STATE_"))
}

// Registrations lists this node's control-plane registrations.
func (a *Admin) Registrations(ctx context.Context) ([]*cpv1.Registration, error) {
	r, err := a.g.ControlPlane().ListRegistrations(ctx, &cpv1.ListRegistrationsRequest{})
	if err != nil {
		return nil, err
	}
	return r.GetRegistrations(), nil
}
