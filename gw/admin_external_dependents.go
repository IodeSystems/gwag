package gateway

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

// Admin routes for external dependents (external_dependents.go).
// Writes go through AdminMiddleware's bearer check like every
// non-GET admin route; the list is a read, public like the graph.
func registerExternalDependents(api huma.API, g *Gateway) {
	huma.Register(api, huma.Operation{
		OperationID: "listExternalDependents",
		Method:      http.MethodGet,
		Path:        "/admin/external-dependents",
		Summary:     "List declared external dependents (consumers gwag cannot see that pin lines), expired ones included.",
	}, func(ctx context.Context, _ *struct{}) (*externalDependentsOut, error) {
		out := &externalDependentsOut{}
		out.Body.ExternalDependents = []externalDependentInfo{}
		now := nowFunc()
		for _, x := range g.externals.all() {
			out.Body.ExternalDependents = append(out.Body.ExternalDependents, adminExternalDependent(x, now))
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "putExternalDependent",
		Method:      http.MethodPut,
		Path:        "/admin/external-dependents/{id}",
		Summary:     "Create or replace an external dependent: a consumer gwag cannot see (customer, partner, app) that pins lines. It counts as a dependent in the sunset rule, dependency_unmet and drain coverage until it expires or is deleted. Pins on a service or line that is not registered are accepted with a warning.",
	}, func(ctx context.Context, in *putExternalDependentIn) (*putExternalDependentOut, error) {
		x := externalDependent{
			ID:            in.ID,
			Name:          in.Body.Name,
			Kind:          in.Body.Kind,
			Note:          in.Body.Note,
			UpdatedUnixMs: nowFunc().UnixMilli(),
		}
		if in.Body.Owner != nil {
			x.Owner = &manifestOwner{Team: in.Body.Owner.Team, Contact: in.Body.Owner.Contact}
		}
		for _, d := range in.Body.Depends {
			x.Depends = append(x.Depends, manifestDependency{Service: d.Service, Lines: append([]string(nil), d.Lines...)})
		}
		errs := normalizeExternalDependent(&x)
		if in.Body.ExpiresAt != "" {
			t, err := time.Parse(time.RFC3339, in.Body.ExpiresAt)
			if err != nil {
				errs = append(errs, externalFieldError{"body.expiresAt", "expiresAt must be an RFC 3339 date-time"})
			} else {
				x.ExpiresUnixMs = t.UnixMilli()
			}
		}
		if len(errs) > 0 {
			details := make([]error, len(errs))
			for i, e := range errs {
				details[i] = &huma.ErrorDetail{Location: e.Location, Message: e.Message}
			}
			return nil, huma.Error422UnprocessableEntity("validation failed", details...)
		}
		if err := g.putExternalDependent(ctx, x); err != nil {
			return nil, err
		}
		g.mu.Lock()
		warnings := g.externalWarningsLocked(x)
		g.mu.Unlock()
		out := &putExternalDependentOut{}
		out.Body.ExternalDependent = adminExternalDependent(x, nowFunc())
		out.Body.Warnings = append([]string{}, warnings...)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "deleteExternalDependent",
		Method:      http.MethodDelete,
		Path:        "/admin/external-dependents/{id}",
		Summary:     "Delete an external dependent. Its pins stop counting at once.",
	}, func(ctx context.Context, in *externalDependentIDIn) (*deleteExternalDependentOut, error) {
		ok, err := g.deleteExternalDependent(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, huma.Error404NotFound(fmt.Sprintf("no external dependent %q", in.ID))
		}
		out := &deleteExternalDependentOut{}
		out.Body.Deleted = true
		return out, nil
	})
}

type externalOwnerInfo struct {
	Team    string `json:"team" doc:"Team on this side that owns the relationship."`
	Contact string `json:"contact" doc:"How to reach them (email, channel)."`
}

type externalPinInfo struct {
	Service string   `json:"service" doc:"Provider namespace."`
	Lines   []string `json:"lines" doc:"Accepted lines: vN or unstable."`
}

type externalDependentInfo struct {
	ID            string             `json:"id"`
	Name          string             `json:"name" doc:"Display name; empty = id."`
	Kind          string             `json:"kind" doc:"customer, partner, app or other."`
	Owner         *externalOwnerInfo `json:"owner,omitempty"`
	Depends       []externalPinInfo  `json:"depends"`
	Note          string             `json:"note"`
	ExpiresAt     string             `json:"expiresAt" doc:"RFC 3339; empty = no expiry."`
	Expired       bool               `json:"expired" doc:"Past expiresAt: listed, but no longer counts as a dependent."`
	UpdatedUnixMs int64              `json:"updatedUnixMs"`
}

func adminExternalDependent(x externalDependent, now time.Time) externalDependentInfo {
	info := externalDependentInfo{
		ID:            x.ID,
		Name:          x.Name,
		Kind:          x.Kind,
		Note:          x.Note,
		Expired:       x.expired(now),
		UpdatedUnixMs: x.UpdatedUnixMs,
		Depends:       []externalPinInfo{}, // never nil — JSON null breaks NonNull
	}
	if x.Owner != nil {
		info.Owner = &externalOwnerInfo{Team: x.Owner.Team, Contact: x.Owner.Contact}
	}
	if x.ExpiresUnixMs > 0 {
		info.ExpiresAt = time.UnixMilli(x.ExpiresUnixMs).UTC().Format(time.RFC3339)
	}
	for _, d := range x.Depends {
		info.Depends = append(info.Depends, externalPinInfo{Service: d.Service, Lines: append([]string{}, d.Lines...)})
	}
	return info
}

type externalDependentsOut struct {
	Body struct {
		ExternalDependents []externalDependentInfo `json:"externalDependents"`
	}
}

type externalDependentIDIn struct {
	ID string `path:"id" doc:"Lower-case slug: [a-z0-9][a-z0-9_-]{0,62}."`
}

type externalOwnerIn struct {
	Team    string `json:"team,omitempty"`
	Contact string `json:"contact,omitempty"`
}

type externalPinIn struct {
	Service string   `json:"service" doc:"Provider namespace. Need not be registered yet."`
	Lines   []string `json:"lines" doc:"Accepted lines: vN or unstable, the same rule as a binding's depends."`
}

type putExternalDependentIn struct {
	ID   string `path:"id" doc:"Lower-case slug: [a-z0-9][a-z0-9_-]{0,62}."`
	Body struct {
		Name      string           `json:"name,omitempty" doc:"Display name, e.g. \"Acme Corp\"."`
		Kind      string           `json:"kind" doc:"customer, partner, app or other."`
		Owner     *externalOwnerIn `json:"owner,omitempty"`
		Depends   []externalPinIn  `json:"depends" doc:"Pinned lines per service; at least one."`
		Note      string           `json:"note,omitempty"`
		ExpiresAt string           `json:"expiresAt,omitempty" doc:"RFC 3339 date-time. Past it the declaration stops counting and is listed as expired."`
	}
}

type putExternalDependentOut struct {
	Body struct {
		ExternalDependent externalDependentInfo `json:"externalDependent"`
		Warnings          []string              `json:"warnings" doc:"Pins on a service or line not registered now (accepted)."`
	}
}

type deleteExternalDependentOut struct {
	Body struct {
		Deleted bool `json:"deleted"`
	}
}
