package gateway

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// Admin routes of the event stream mirror (event_mirror.go), on
// green. Self-ingested like every admin route. The status needs dev;
// start and stop need admin. A system key may call all three
// (systemKeyOps).
func registerEventMirror(api huma.API, g *Gateway) {
	huma.Register(api, huma.Operation{
		OperationID: "getClusterMirror",
		Method:      http.MethodGet,
		Path:        "/admin/cluster/mirror",
		Summary:     "Event stream mirror status on green: the configured source, this node's leafnode connections, and per event stream whether green sources it from blue, both sides' last sequence and message count, and the lag NATS reports. Dev, admin or system key.",
	}, func(ctx context.Context, _ *struct{}) (*mirrorOut, error) {
		v, err := g.mirrorStatus(ctx)
		if err != nil {
			return nil, mirrorHumaErr(err)
		}
		return &mirrorOut{Body: v}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "startClusterMirror",
		Method:      http.MethodPost,
		Path:        "/admin/cluster/mirror",
		Summary:     "Start the event stream mirror: green's stream of each of blue's event streams (or the listed ones, by stream name or channel subject) sources blue's. A stream green does not have is created with blue's subject. Idempotent. Admin or system key.",
	}, func(ctx context.Context, in *mirrorIn) (*mirrorOut, error) {
		v, err := g.startMirror(ctx, in.Body.From, in.Body.Streams)
		if err != nil {
			return nil, mirrorHumaErr(err)
		}
		return &mirrorOut{Body: v}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "stopClusterMirror",
		Method:      http.MethodPost,
		Path:        "/admin/cluster/mirror/stop",
		Summary:     "Stop the event stream mirror for every mirrored stream (or the listed ones): the source is removed, the streams and their messages stay as normal streams. Admin or system key.",
	}, func(ctx context.Context, in *mirrorStopIn) (*mirrorOut, error) {
		v, err := g.stopMirror(ctx, in.Body.Streams)
		if err != nil {
			return nil, mirrorHumaErr(err)
		}
		return &mirrorOut{Body: v}, nil
	})
}

type mirrorIn struct {
	Body struct {
		From    string   `json:"from,omitempty" doc:"blue's mirror endpoint; when set it must be the configured source"`
		Streams []string `json:"streams,omitempty" doc:"stream names (EV_...) or channel subjects; empty: every event stream blue has"`
	}
}

type mirrorStopIn struct {
	Body struct {
		Streams []string `json:"streams,omitempty" doc:"stream names or channel subjects; empty: every mirrored stream"`
	}
}

type mirrorOut struct {
	Body mirrorView
}

func mirrorHumaErr(err error) error {
	switch {
	case errors.Is(err, errMirrorOff):
		return huma.Error409Conflict(err.Error())
	case errors.Is(err, errMirrorFrom):
		return huma.Error400BadRequest(err.Error())
	case errors.Is(err, errMirrorUnknown):
		return huma.Error404NotFound(err.Error())
	case errors.Is(err, errMirrorBlue):
		return huma.Error503ServiceUnavailable(err.Error())
	}
	return huma.Error500InternalServerError(err.Error())
}
