package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	cpv1 "github.com/iodesystems/go-api-gateway/gw/proto/controlplane/v1"
)

// AdminHumaRouter mounts the gateway's admin routes — peers,
// services, forget-peer, sign-subscription-token — defined via huma
// (so they emit OpenAPI) on a *http.ServeMux that the example
// gateway can mount at any prefix. Returns the OpenAPI spec bytes
// alongside the mux so the gateway can self-ingest via
// AddOpenAPIBytes (huma → OpenAPI → GraphQL).
//
// Each handler delegates to the existing controlPlane gRPC service
// in-process. The huma route is a thin REST shape on top, which the
// gateway's OpenAPI ingestion lifts into the GraphQL surface — same
// path any external service-defined OpenAPI takes.
func (g *Gateway) AdminHumaRouter() (*http.ServeMux, []byte, error) {
	mux := http.NewServeMux()
	cfg := huma.DefaultConfig("Admin", "1.0.0")
	// Huma defaults OpenAPIPath to "/openapi", which would serve at
	// /api/admin/../openapi.json after StripPrefix("/api"). Move it
	// under /admin/openapi so inbound /api/admin/openapi.json
	// resolves cleanly.
	cfg.OpenAPIPath = "/admin/openapi"
	api := humago.New(mux, cfg)

	cp, ok := g.ControlPlane().(*controlPlane)
	if !ok {
		return nil, nil, fmt.Errorf("AdminHumaRouter: ControlPlane impl is not *controlPlane")
	}

	huma.Register(api, huma.Operation{
		OperationID: "listPeers",
		Method:      http.MethodGet,
		Path:        "/admin/peers",
		Summary:     "List active cluster peers.",
	}, func(ctx context.Context, _ *struct{}) (*peersOut, error) {
		resp, err := cp.ListPeers(ctx, &cpv1.ListPeersRequest{})
		if err != nil {
			return nil, err
		}
		out := &peersOut{}
		out.Body.Peers = []peerInfo{} // never nil — JSON null breaks NonNull
		for _, p := range resp.GetPeers() {
			out.Body.Peers = append(out.Body.Peers, peerInfo{
				NodeID:       p.GetNodeId(),
				Name:         p.GetName(),
				JoinedUnixMs: p.GetJoinedUnixMs(),
			})
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "listServices",
		Method:      http.MethodGet,
		Path:        "/admin/services",
		Summary:     "List registered services with hashes.",
	}, func(ctx context.Context, _ *struct{}) (*servicesOut, error) {
		resp, err := cp.ListServices(ctx, &cpv1.ListServicesRequest{})
		if err != nil {
			return nil, err
		}
		out := &servicesOut{}
		out.Body.Services = []serviceInfo{}
		for _, s := range resp.GetServices() {
			out.Body.Services = append(out.Body.Services, serviceInfo{
				Namespace:    s.GetNamespace(),
				Version:      s.GetVersion(),
				HashHex:      s.GetHashHex(),
				ReplicaCount: s.GetReplicaCount(),
			})
		}
		out.Body.StableVN = []stableVNEntry{}
		for ns, vN := range resp.GetStableVn() {
			out.Body.StableVN = append(out.Body.StableVN, stableVNEntry{
				Namespace: ns,
				VN:        vN,
			})
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "forgetPeer",
		Method:      http.MethodPost,
		Path:        "/admin/peers/{nodeId}/forget",
		Summary:     "Drop a TTL-expired peer and shrink the cluster's stream replicas.",
	}, func(ctx context.Context, in *forgetIn) (*forgetOut, error) {
		resp, err := cp.ForgetPeer(ctx, &cpv1.ForgetPeerRequest{NodeId: in.NodeID})
		if err != nil {
			return nil, err
		}
		out := &forgetOut{}
		out.Body.Removed = resp.GetRemoved()
		out.Body.NewReplicas = resp.GetNewReplicas()
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "signSubscriptionToken",
		Method:      http.MethodPost,
		Path:        "/admin/sign",
		Summary:     "Mint an HMAC subscription token (subject to the optional delegate).",
	}, func(ctx context.Context, in *signIn) (*signOut, error) {
		resp, err := cp.SignSubscriptionToken(ctx, &cpv1.SignSubscriptionTokenRequest{
			Channel:    in.Body.Channel,
			TtlSeconds: in.Body.TtlSeconds,
			Kid:        in.Body.Kid,
		})
		if err != nil {
			return nil, err
		}
		out := &signOut{}
		out.Body.Code = resp.GetCode().String()
		out.Body.Hmac = resp.GetHmac()
		out.Body.TimestampUnix = resp.GetTimestampUnix()
		out.Body.Reason = resp.GetReason()
		out.Body.Kid = resp.GetKid()
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "listChannels",
		Method:      http.MethodGet,
		Path:        "/admin/channels",
		Summary:     "List active subscription subjects with their in-process consumer counts.",
	}, func(_ context.Context, _ *struct{}) (*channelsOut, error) {
		out := &channelsOut{}
		// Always emit a non-nil slice so JSON Subscription[] is never null.
		out.Body.Channels = []channelInfo{}
		for _, s := range g.ActiveSubjects() {
			out.Body.Channels = append(out.Body.Channels, channelInfo{
				Subject:   s.Subject,
				Consumers: s.Consumers,
			})
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "listInjectors",
		Method:      http.MethodGet,
		Path:        "/admin/injectors",
		Summary:     "List every InjectType / InjectPath / InjectHeader registration with its current schema landings.",
	}, func(ctx context.Context, _ *struct{}) (*injectorsOut, error) {
		entries, err := g.InjectorInventory()
		if err != nil {
			return nil, err
		}
		out := &injectorsOut{}
		out.Body.Injectors = []injectorInfo{}
		for _, e := range entries {
			info := injectorInfo{
				Kind:       string(e.Kind),
				TypeName:   e.TypeName,
				Path:       e.Path,
				HeaderName: e.HeaderName,
				Hide:       e.Hide,
				Nullable:   e.Nullable,
				State:      string(e.State),
				RegisteredAt: registeredAtInfo{
					File:     e.RegisteredAt.File,
					Line:     e.RegisteredAt.Line,
					Function: e.RegisteredAt.Function,
				},
				Landings: []injectorLandingInfo{},
			}
			for _, l := range e.Landings {
				info.Landings = append(info.Landings, injectorLandingInfo{
					Kind:       l.Kind,
					Namespace:  l.Namespace,
					Version:    l.Version,
					Op:         l.Op,
					TypeName:   l.TypeName,
					FieldName:  l.FieldName,
					ArgName:    l.ArgName,
					HeaderName: l.HeaderName,
				})
			}
			out.Body.Injectors = append(out.Body.Injectors, info)
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "retractStable",
		Method:      http.MethodPost,
		Path:        "/admin/services/{namespace}/stable/retract",
		Summary:     "Retract a namespace's `stable` alias to a lower vN. Operator-driven; refuses to skip past vNs that aren't currently registered.",
	}, func(ctx context.Context, in *retractStableIn) (*retractStableOut, error) {
		resp, err := cp.RetractStable(ctx, &cpv1.RetractStableRequest{
			Namespace: in.Namespace,
			TargetVN:  in.Body.TargetVN,
		})
		if err != nil {
			return nil, err
		}
		out := &retractStableOut{}
		out.Body.PriorVN = resp.GetPriorVN()
		out.Body.NewVN = resp.GetNewVN()
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "deprecate",
		Method:      http.MethodPost,
		Path:        "/admin/services/{namespace}/{version}/deprecate",
		Summary:     "Manually deprecate a (namespace, version). The reason surfaces as @deprecated in SDL alongside any auto-deprecation of older vN cuts.",
	}, func(ctx context.Context, in *deprecateIn) (*deprecateOut, error) {
		_, err := cp.Deprecate(ctx, &cpv1.DeprecateRequest{
			Namespace: in.Namespace,
			Version:   in.Version,
			Reason:    in.Body.Reason,
		})
		if err != nil {
			return nil, err
		}
		return &deprecateOut{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "undeprecate",
		Method:      http.MethodPost,
		Path:        "/admin/services/{namespace}/{version}/undeprecate",
		Summary:     "Clear a previously-set manual deprecation. Auto-deprecation (older vN cuts) is unaffected.",
	}, func(ctx context.Context, in *undeprecateIn) (*undeprecateOut, error) {
		resp, err := cp.Undeprecate(ctx, &cpv1.UndeprecateRequest{
			Namespace: in.Namespace,
			Version:   in.Version,
		})
		if err != nil {
			return nil, err
		}
		out := &undeprecateOut{}
		out.Body.PriorReason = resp.GetPriorReason()
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "drain",
		Method:      http.MethodPost,
		Path:        "/admin/drain",
		Summary:     "Trigger graceful drain. /health flips to 503 immediately; the call returns when active streams reach 0 (or the per-request timeout expires).",
	}, func(ctx context.Context, in *drainIn) (*drainOut, error) {
		ttl := time.Duration(in.Body.TimeoutSeconds) * time.Second
		if ttl <= 0 {
			ttl = 30 * time.Second
		}
		dctx, cancel := context.WithTimeout(ctx, ttl)
		defer cancel()
		err := g.Drain(dctx)
		out := &drainOut{}
		out.Body.Drained = err == nil
		out.Body.ActiveStreams = int(g.streamGlobal.Load())
		if err != nil {
			out.Body.Reason = err.Error()
		}
		return out, nil
	})

	spec, err := json.Marshal(api.OpenAPI())
	if err != nil {
		return nil, nil, fmt.Errorf("marshal openapi: %w", err)
	}
	return mux, spec, nil
}

type peerInfo struct {
	NodeID       string `json:"nodeId"`
	Name         string `json:"name,omitempty"`
	JoinedUnixMs int64  `json:"joinedUnixMs"`
}

type peersOut struct {
	Body struct {
		Peers []peerInfo `json:"peers"`
	}
}

type serviceInfo struct {
	Namespace    string `json:"namespace"`
	Version      string `json:"version"`
	HashHex      string `json:"hashHex"`
	ReplicaCount uint32 `json:"replicaCount"`
}

// stableVNEntry surfaces the per-namespace stable alias target. Plan §4
// monotonic invariant: VN advances at registration, decreases only via
// RetractStable. The UI matches `serviceInfo.version == "v" + VN` to
// flag which row is the current stable target.
type stableVNEntry struct {
	Namespace string `json:"namespace"`
	VN        uint32 `json:"vN"`
}

type servicesOut struct {
	Body struct {
		Services []serviceInfo   `json:"services"`
		StableVN []stableVNEntry `json:"stableVN"`
	}
}

type forgetIn struct {
	NodeID string `path:"nodeId"`
}

type forgetOut struct {
	Body struct {
		Removed     bool   `json:"removed"`
		NewReplicas uint32 `json:"newReplicas"`
	}
}

type signIn struct {
	Body struct {
		Channel    string `json:"channel"`
		TtlSeconds int64  `json:"ttlSeconds"`
		// Optional rotation key id; empty = legacy default secret.
		Kid string `json:"kid,omitempty"`
	}
}

type signOut struct {
	Body struct {
		Code          string `json:"code"`
		Hmac          string `json:"hmac,omitempty"`
		TimestampUnix int64  `json:"timestampUnix,omitempty"`
		Reason        string `json:"reason,omitempty"`
		// Echoes the kid the gateway signed under; clients must pass
		// it back as the `kid` arg on the subscribe field.
		Kid string `json:"kid,omitempty"`
	}
}

type channelInfo struct {
	Subject   string `json:"subject"`
	Consumers int    `json:"consumers"`
}

type channelsOut struct {
	Body struct {
		Channels []channelInfo `json:"channels"`
	}
}

type retractStableIn struct {
	Namespace string `path:"namespace"`
	Body      struct {
		TargetVN uint32 `json:"targetVN"`
	}
}

type retractStableOut struct {
	Body struct {
		PriorVN uint32 `json:"priorVN"`
		NewVN   uint32 `json:"newVN"`
	}
}

type deprecateIn struct {
	Namespace string `path:"namespace"`
	Version   string `path:"version"`
	Body      struct {
		Reason string `json:"reason"`
	}
}

type deprecateOut struct {
	Body struct {
	}
}

type undeprecateIn struct {
	Namespace string `path:"namespace"`
	Version   string `path:"version"`
}

type undeprecateOut struct {
	Body struct {
		PriorReason string `json:"priorReason"`
	}
}

type drainIn struct {
	Body struct {
		// TimeoutSeconds caps how long Drain waits for active streams
		// to reach zero before returning. 0 → 30s default.
		TimeoutSeconds int64 `json:"timeoutSeconds,omitempty"`
	}
}

type drainOut struct {
	Body struct {
		Drained       bool   `json:"drained"`
		ActiveStreams int    `json:"activeStreams"`
		Reason        string `json:"reason,omitempty"`
	}
}

type registeredAtInfo struct {
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	Function string `json:"function,omitempty"`
}

type injectorLandingInfo struct {
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace,omitempty"`
	Version    string `json:"version,omitempty"`
	Op         string `json:"op,omitempty"`
	TypeName   string `json:"typeName,omitempty"`
	FieldName  string `json:"fieldName,omitempty"`
	ArgName    string `json:"argName,omitempty"`
	HeaderName string `json:"headerName,omitempty"`
}

type injectorInfo struct {
	Kind         string                `json:"kind"`
	TypeName     string                `json:"typeName,omitempty"`
	Path         string                `json:"path,omitempty"`
	HeaderName   string                `json:"headerName,omitempty"`
	Hide         bool                  `json:"hide"`
	Nullable     bool                  `json:"nullable"`
	State        string                `json:"state"`
	RegisteredAt registeredAtInfo      `json:"registeredAt"`
	Landings     []injectorLandingInfo `json:"landings"`
}

type injectorsOut struct {
	Body struct {
		Injectors []injectorInfo `json:"injectors"`
	}
}
