package gateway

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// Admin routes for per-instance drain (instance_drain.go). Self-
// ingested like every admin route, so they show up under the
// GraphQL `admin` namespace too.
func registerInstanceDrain(api huma.API, g *Gateway) {
	huma.Register(api, huma.Operation{
		OperationID: "listInstances",
		Method:      http.MethodGet,
		Path:        "/admin/instances",
		Summary:     "List registered instances (one per registration) with their lines and drain state: serving, draining, blocked (with reasons) or safe_to_stop.",
	}, func(ctx context.Context, _ *struct{}) (*instancesOut, error) {
		out := &instancesOut{}
		out.Body.Instances = []instanceDrainInfo{}
		for _, st := range g.listInstanceDrains("") {
			out.Body.Instances = append(out.Body.Instances, adminInstanceDrain(st))
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "drainInstance",
		Method:      http.MethodPost,
		Path:        "/admin/instances/{instance}/drain",
		Summary:     "Drain one instance (registration_id, or an instance_id carried by one registration): stop routing new requests to it on every node and report safe_to_stop when nothing is in flight. Held as blocked while a dependent would be left without a covering line. Idempotent; call again to poll. cancel=true withdraws the drain.",
	}, func(ctx context.Context, in *drainInstanceIn) (*drainInstanceOut, error) {
		st, err := g.drainInstance(ctx, in.Instance, in.Body.Cancel)
		if err != nil {
			if errors.Is(err, errDrainUnknownInstance) {
				return nil, huma.Error404NotFound(err.Error())
			}
			return nil, huma.Error400BadRequest(err.Error())
		}
		out := &drainInstanceOut{}
		out.Body = adminInstanceDrain(st)
		return out, nil
	})
}

type instanceDrainInfo struct {
	RegistrationID string   `json:"registrationId"`
	InstanceID     string   `json:"instanceId"`
	Addr           string   `json:"addr"`
	State          string   `json:"state" enum:"serving,draining,blocked,safe_to_stop"`
	Reasons        []string `json:"reasons"`
	Inflight       int32    `json:"inflight"`
	Lines          []string `json:"lines"`
}

func adminInstanceDrain(st instanceDrainStatus) instanceDrainInfo {
	info := instanceDrainInfo{
		RegistrationID: st.RegID,
		InstanceID:     st.InstanceID,
		Addr:           st.Addr,
		State:          st.State,
		Reasons:        append([]string{}, st.Reasons...), // never nil — JSON null breaks NonNull
		Inflight:       st.Inflight,
		Lines:          append([]string{}, st.Lines...),
	}
	return info
}

// adminLineDrains is the serviceInfo.drains projection: instances on
// (ns, ver) with a drain requested. Never nil.
func adminLineDrains(g *Gateway, ns, ver string) []instanceDrainInfo {
	out := []instanceDrainInfo{}
	for _, st := range g.listInstanceDrains(ns + "/" + ver) {
		out = append(out, adminInstanceDrain(st))
	}
	return out
}

type instancesOut struct {
	Body struct {
		Instances []instanceDrainInfo `json:"instances"`
	}
}

type drainInstanceIn struct {
	Instance string `path:"instance"`
	Body     struct {
		Cancel bool `json:"cancel,omitempty"`
	}
}

type drainInstanceOut struct {
	Body instanceDrainInfo
}
