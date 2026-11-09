package gateway

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

// Admin routes for external dependents (external_dependents.go).
// Writes go through AdminMiddleware's bearer check like every
// non-GET admin route; the list is a read, public like the graph.
func registerExternalDependents(api huma.API, g *Gateway) {
	registerNotifications(api, g)

	huma.Register(api, huma.Operation{
		OperationID: "listExternalDependents",
		Method:      http.MethodGet,
		Path:        "/admin/external-dependents",
		Summary:     "List declared external dependents (consumers gwag cannot see that pin lines). reviewDue marks the ones past their review date.",
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
		Summary:     "Create or replace an external dependent: a consumer gwag cannot see (customer, partner, app) that pins lines. It counts as a dependent in the sunset rule, dependency_unmet and drain coverage until it is deleted. reviewAt is required; past it the declaration still counts and is listed as review-due. Pins on a service or line that is not registered, and a review date in the past or more than a year out, are accepted with a warning.",
	}, func(ctx context.Context, in *putExternalDependentIn) (*putExternalDependentOut, error) {
		x := externalDependent{
			ID:            in.ID,
			Name:          in.Body.Name,
			Kind:          in.Body.Kind,
			Note:          in.Body.Note,
			LastOp:        changeOpPut,
			UpdatedUnixMs: nowFunc().UnixMilli(),
			UpdatedBy:     adminIdentityFromContext(ctx),
		}
		if in.Body.Owner != nil {
			x.Owner = &manifestOwner{Team: in.Body.Owner.Team, Contact: in.Body.Owner.Contact}
		}
		for _, d := range in.Body.Depends {
			x.Depends = append(x.Depends, manifestDependency{Service: d.Service, Lines: append([]string(nil), d.Lines...)})
		}
		errs := normalizeExternalDependent(&x)
		if ms, err := parseReviewAt(in.Body.ReviewAt); err != nil {
			errs = append(errs, externalFieldError{"body.reviewAt", err.Error()})
		} else {
			x.ReviewUnixMs = ms
		}
		if len(errs) > 0 {
			return nil, externalValidationError(errs)
		}
		if prev, ok := g.externals.get(x.ID); ok {
			// A PUT keeps the record of the last review.
			x.ReviewedUnixMs, x.ReviewedBy = prev.ReviewedUnixMs, prev.ReviewedBy
		}
		if err := g.checkNotFrozen(ctx, "an external dependent write"); err != nil {
			return nil, err
		}
		if err := g.putExternalDependent(ctx, x); err != nil {
			return nil, err
		}
		g.mu.Lock()
		warnings := g.externalWarningsLocked(x)
		g.mu.Unlock()
		out := &putExternalDependentOut{}
		now := nowFunc()
		out.Body.ExternalDependent = adminExternalDependent(x, now)
		out.Body.Warnings = append(append([]string{}, warnings...), reviewAtWarnings(x.ReviewUnixMs, now)...)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "reviewExternalDependent",
		Method:      http.MethodPost,
		Path:        "/admin/external-dependents/{id}/review",
		Summary:     "Mark an external dependent reviewed: set a new reviewAt (must be in the future) and record reviewedAt and reviewedBy (the authenticated caller: role, plus the subject the admin delegate names). Every other field stays as it is.",
	}, func(ctx context.Context, in *reviewExternalDependentIn) (*putExternalDependentOut, error) {
		x, ok := g.externals.get(in.ID)
		if !ok {
			return nil, huma.Error404NotFound(fmt.Sprintf("no external dependent %q", in.ID))
		}
		now := nowFunc()
		ms, err := parseReviewAt(in.Body.ReviewAt)
		if err == nil && ms <= now.UnixMilli() {
			err = fmt.Errorf("reviewAt must be in the future")
		}
		if err != nil {
			return nil, externalValidationError([]externalFieldError{{"body.reviewAt", err.Error()}})
		}
		x.ReviewUnixMs = ms
		x.ReviewedUnixMs = now.UnixMilli()
		x.ReviewedBy = adminIdentityFromContext(ctx)
		x.LastOp = changeOpReview
		x.UpdatedUnixMs = now.UnixMilli()
		x.UpdatedBy = x.ReviewedBy
		if err := g.checkNotFrozen(ctx, "an external dependent review"); err != nil {
			return nil, err
		}
		if err := g.putExternalDependent(ctx, x); err != nil {
			return nil, err
		}
		out := &putExternalDependentOut{}
		out.Body.ExternalDependent = adminExternalDependent(x, now)
		out.Body.Warnings = append([]string{}, reviewAtWarnings(x.ReviewUnixMs, now)...)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "deleteExternalDependent",
		Method:      http.MethodDelete,
		Path:        "/admin/external-dependents/{id}",
		Summary:     "Delete an external dependent. Its pins stop counting at once. The hook and delta sinks record the caller's identity.",
	}, func(ctx context.Context, in *externalDependentIDIn) (*deleteExternalDependentOut, error) {
		if err := g.checkNotFrozen(ctx, "an external dependent delete"); err != nil {
			return nil, err
		}
		ok, err := g.deleteExternalDependent(ctx, in.ID, adminIdentityFromContext(ctx))
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

// reviewAtFarOut is the review horizon past which a PUT or review
// warns. Accepted: the warning is a prompt, not a rule.
const reviewAtFarOut = 366 * 24 * time.Hour

// parseReviewAt reads a required RFC 3339 reviewAt.
func parseReviewAt(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("reviewAt is required (RFC 3339 date-time)")
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0, fmt.Errorf("reviewAt must be an RFC 3339 date-time")
	}
	return t.UnixMilli(), nil
}

func reviewAtWarnings(ms int64, now time.Time) []string {
	at := time.UnixMilli(ms)
	switch {
	case !at.After(now):
		return []string{"reviewAt is not in the future: the declaration is review-due now"}
	case at.Sub(now) > reviewAtFarOut:
		return []string{"reviewAt is more than a year out"}
	}
	return nil
}

func externalValidationError(errs []externalFieldError) error {
	details := make([]error, len(errs))
	for i, e := range errs {
		details[i] = &huma.ErrorDetail{Location: e.Location, Message: e.Message}
	}
	return huma.Error422UnprocessableEntity("validation failed", details...)
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
	ReviewAt      string             `json:"reviewAt" doc:"RFC 3339. Empty only on a declaration stored before reviewAt existed."`
	ReviewDue     bool               `json:"reviewDue" doc:"reviewAt is past (or missing). The declaration still counts as a dependent; a person must review it."`
	ReviewedAt    string             `json:"reviewedAt" doc:"RFC 3339 time of the last review op; empty = never reviewed."`
	ReviewedBy    string             `json:"reviewedBy" doc:"Authenticated identity that made the last review op: the role (admin or dev), plus :subject when the admin delegate names one. Taken from the credential, not from the request; empty = never reviewed."`
	UpdatedUnixMs int64              `json:"updatedUnixMs"`
	UpdatedBy     string             `json:"updatedBy" doc:"Authenticated identity that last wrote it (put or review), same form as reviewedBy; empty on a declaration stored before this field existed."`
}

func adminExternalDependent(x externalDependent, now time.Time) externalDependentInfo {
	info := externalDependentInfo{
		ID:            x.ID,
		Name:          x.Name,
		Kind:          x.Kind,
		Note:          x.Note,
		ReviewDue:     x.reviewDue(now),
		ReviewedBy:    x.ReviewedBy,
		UpdatedUnixMs: x.UpdatedUnixMs,
		UpdatedBy:     x.UpdatedBy,
		Depends:       []externalPinInfo{}, // never nil — JSON null breaks NonNull
	}
	if x.Owner != nil {
		info.Owner = &externalOwnerInfo{Team: x.Owner.Team, Contact: x.Owner.Contact}
	}
	info.ReviewAt = rfc3339Ms(x.ReviewUnixMs)
	info.ReviewedAt = rfc3339Ms(x.ReviewedUnixMs)
	for _, d := range x.Depends {
		info.Depends = append(info.Depends, externalPinInfo{Service: d.Service, Lines: append([]string{}, d.Lines...)})
	}
	return info
}

// rfc3339Ms formats a Unix-ms time as RFC 3339 UTC; 0 → "".
func rfc3339Ms(ms int64) string {
	if ms == 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
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
		Name     string           `json:"name,omitempty" doc:"Display name, e.g. \"Acme Corp\"."`
		Kind     string           `json:"kind" doc:"customer, partner, app or other."`
		Owner    *externalOwnerIn `json:"owner,omitempty"`
		Depends  []externalPinIn  `json:"depends" doc:"Pinned lines per service; at least one."`
		Note     string           `json:"note,omitempty"`
		ReviewAt string           `json:"reviewAt" doc:"RFC 3339 date-time, required. Past it the declaration still counts; it is listed as review-due and raises a notification."`
	}
}

type reviewExternalDependentIn struct {
	ID   string `path:"id" doc:"Lower-case slug: [a-z0-9][a-z0-9_-]{0,62}."`
	Body struct {
		ReviewAt string `json:"reviewAt" doc:"Next review date, RFC 3339; must be in the future. The reviewer is the authenticated caller; the body names no one."`
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
