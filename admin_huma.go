package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	cpv1 "github.com/iodesystems/go-api-gateway/controlplane/v1"
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
	api := humago.New(mux, huma.DefaultConfig("Admin", "1.0.0"))

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
		out.Body.Environment = resp.GetEnvironment()
		out.Body.Services = []serviceInfo{}
		for _, s := range resp.GetServices() {
			out.Body.Services = append(out.Body.Services, serviceInfo{
				Namespace:    s.GetNamespace(),
				Version:      s.GetVersion(),
				HashHex:      s.GetHashHex(),
				ReplicaCount: s.GetReplicaCount(),
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
		})
		if err != nil {
			return nil, err
		}
		out := &signOut{}
		out.Body.Code = resp.GetCode().String()
		out.Body.Hmac = resp.GetHmac()
		out.Body.TimestampUnix = resp.GetTimestampUnix()
		out.Body.Reason = resp.GetReason()
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

type servicesOut struct {
	Body struct {
		Environment string        `json:"environment,omitempty"`
		Services    []serviceInfo `json:"services"`
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
	}
}

type signOut struct {
	Body struct {
		Code          string `json:"code"`
		Hmac          string `json:"hmac,omitempty"`
		TimestampUnix int64  `json:"timestampUnix,omitempty"`
		Reason        string `json:"reason,omitempty"`
	}
}
