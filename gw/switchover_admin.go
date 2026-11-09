package gateway

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// Admin routes for switchover (switchover.go). Self-ingested like
// every admin route. getSwitchover needs dev (a GET); setSwitchover
// and clearSwitchover need admin.
func registerSwitchoverRoutes(api huma.API, g *Gateway) {
	huma.Register(api, huma.Operation{
		OperationID: "getSwitchover",
		Method:      http.MethodGet,
		Path:        "/admin/switchover",
		Summary:     "Switchover to green (WithSwitchover) as this node applies it: target, percent, rules, where the setting comes from, this node's outcome and green-error counts, and recent green errors.",
	}, func(ctx context.Context, _ *struct{}) (*switchoverOut, error) {
		out := &switchoverOut{}
		out.Body = switchoverViewOf(g)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "setSwitchover",
		Method:      http.MethodPut,
		Path:        "/admin/switchover",
		Summary:     "Set the switchover share at runtime: target (empty keeps WithSwitchover's Target), percent (0..100) and rules, replacing the static Percent and Rules. In a cluster it is stored in the gwag-cluster bucket and every node with WithSwitchover applies it within about a second.",
	}, func(ctx context.Context, in *setSwitchoverIn) (*switchoverOut, error) {
		s := g.switchover
		if s == nil {
			return nil, huma.Error409Conflict(errSwitchoverOff.Error())
		}
		rec := &switchoverRecord{Target: in.Body.Target, Percent: in.Body.Percent, Rules: in.Body.Rules}
		if _, err := s.setRuntime(ctx, rec); err != nil {
			if errors.Is(err, errSwitchoverInvalid) {
				return nil, huma.Error400BadRequest(err.Error())
			}
			return nil, huma.Error503ServiceUnavailable(err.Error())
		}
		out := &switchoverOut{}
		out.Body = switchoverViewOf(g)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "clearSwitchover",
		Method:      http.MethodDelete,
		Path:        "/admin/switchover",
		Summary:     "Clear the runtime switchover setting: every node goes back to WithSwitchover's static Target, Percent and Rules.",
	}, func(ctx context.Context, _ *struct{}) (*switchoverOut, error) {
		s := g.switchover
		if s == nil {
			return nil, huma.Error409Conflict(errSwitchoverOff.Error())
		}
		if _, err := s.setRuntime(ctx, nil); err != nil {
			return nil, huma.Error503ServiceUnavailable(err.Error())
		}
		out := &switchoverOut{}
		out.Body = switchoverViewOf(g)
		return out, nil
	})
}

type setSwitchoverIn struct {
	Body struct {
		Target  string           `json:"target,omitempty" doc:"green's GraphQL URL; empty keeps WithSwitchover's Target"`
		Percent int              `json:"percent" minimum:"0" maximum:"100"`
		Rules   []SwitchoverRule `json:"rules,omitempty"`
	}
}

type switchoverCounts struct {
	Green    int64 `json:"green"`
	Fallback int64 `json:"fallback"`
	Error    int64 `json:"error"`
	Blue     int64 `json:"blue"`
}

type switchoverErrorCounts struct {
	Connect     int64 `json:"connect"`
	Timeout     int64 `json:"timeout"`
	Transport   int64 `json:"transport"`
	Status      int64 `json:"status"`
	BadResponse int64 `json:"badResponse"`
}

type switchoverRecentError struct {
	AtUnixMs      int64  `json:"atUnixMs"`
	Reason        string `json:"reason"`
	Outcome       string `json:"outcome"`
	OperationName string `json:"operationName"`
	Error         string `json:"error"`
}

type switchoverView struct {
	Enabled      bool                    `json:"enabled"`
	Active       bool                    `json:"active" doc:"a target is set and some request can go to green"`
	Source       string                  `json:"source" enum:"static,runtime"`
	Target       string                  `json:"target"`
	Percent      int                     `json:"percent"`
	Rules        []SwitchoverRule        `json:"rules"`
	StickyHeader string                  `json:"stickyHeader"`
	SetBy        string                  `json:"setBy"`
	SetAtUnixMs  int64                   `json:"setAtUnixMs"`
	Counts       switchoverCounts        `json:"counts"`
	GreenErrors  switchoverErrorCounts   `json:"greenErrors"`
	Recent       []switchoverRecentError `json:"recent"`
}

type switchoverOut struct {
	Body switchoverView
}

func switchoverViewOf(g *Gateway) switchoverView {
	v := switchoverView{Source: "static", Rules: []SwitchoverRule{}, Recent: []switchoverRecentError{}}
	s := g.switchover
	if s == nil {
		return v
	}
	c := s.cur.Load()
	v.Enabled = true
	v.Active = c.active
	if c.runtime {
		v.Source = "runtime"
	}
	v.Target = c.rec.Target
	v.Percent = c.rec.Percent
	v.Rules = append(v.Rules, c.rec.Rules...)
	v.StickyHeader = s.opts.StickyHeader
	v.SetBy = c.rec.SetBy
	v.SetAtUnixMs = c.rec.SetUnixMs
	v.Counts = switchoverCounts{
		Green:    s.outcomes[SwitchoverOutcomeGreen].Load(),
		Fallback: s.outcomes[SwitchoverOutcomeFallback].Load(),
		Error:    s.outcomes[SwitchoverOutcomeError].Load(),
		Blue:     s.outcomes[SwitchoverOutcomeBlue].Load(),
	}
	v.GreenErrors = switchoverErrorCounts{
		Connect:     s.reasons[SwitchoverReasonConnect].Load(),
		Timeout:     s.reasons[SwitchoverReasonTimeout].Load(),
		Transport:   s.reasons[SwitchoverReasonTransport].Load(),
		Status:      s.reasons[SwitchoverReasonStatus].Load(),
		BadResponse: s.reasons[SwitchoverReasonBadResponse].Load(),
	}
	for _, e := range s.recentErrors() {
		v.Recent = append(v.Recent, switchoverRecentError{
			AtUnixMs: e.at.UnixMilli(), Reason: e.reason, Outcome: e.outcome, OperationName: e.opName, Error: e.err,
		})
	}
	return v
}
