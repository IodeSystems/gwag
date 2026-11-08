package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

// Admin routes for app keys (app_keys.go). All three need the admin
// role, the list included (adminAdminReadOps).

type appKeyInfo struct {
	ID          string   `json:"id" doc:"Key id: the part of the key after gwk_ and before the secret. Not secret."`
	Namespaces  []string `json:"namespaces" doc:"Namespaces the key covers."`
	Note        string   `json:"note"`
	CreatedAt   string   `json:"createdAt" doc:"RFC 3339."`
	CreatedBy   string   `json:"createdBy" doc:"Authenticated identity that minted the key."`
	ExpiresAt   string   `json:"expiresAt" doc:"RFC 3339."`
	Expired     bool     `json:"expired" doc:"Past expiresAt: calls with the key fail."`
	ExpiresSoon bool     `json:"expiresSoon" doc:"Expires within 7 days and has not expired yet."`
}

func adminAppKey(r appKeyRecord, now time.Time) appKeyInfo {
	left := r.expires().Sub(now)
	return appKeyInfo{
		ID:          r.ID,
		Namespaces:  append([]string{}, r.Namespaces...),
		Note:        r.Note,
		CreatedAt:   time.UnixMilli(r.CreatedUnixMs).UTC().Format(time.RFC3339),
		CreatedBy:   r.CreatedBy,
		ExpiresAt:   r.expires().UTC().Format(time.RFC3339),
		Expired:     left <= 0,
		ExpiresSoon: left > 0 && left <= appKeyWarnBefore,
	}
}

type createAppKeyIn struct {
	Body struct {
		Namespaces []string `json:"namespaces" minItems:"1" doc:"Namespaces the key covers."`
		TTL        string   `json:"ttl" doc:"Lifetime: days (30d) or a Go duration (720h)."`
		Note       string   `json:"note,omitempty" doc:"Free text shown in the list, e.g. the service and environment."`
	}
}

type createAppKeyOut struct {
	Body struct {
		Key    string     `json:"key" doc:"The key. Shown once; the gateway keeps only its hash."`
		AppKey appKeyInfo `json:"appKey"`
	}
}

type appKeysOut struct {
	Body struct {
		AppKeys []appKeyInfo `json:"appKeys"`
	}
}

type appKeyIDIn struct {
	ID string `path:"id"`
}

type revokeAppKeyOut struct {
	Body struct {
		Revoked bool `json:"revoked"`
	}
}

func appKeyHumaErr(err error) error {
	switch {
	case errors.Is(err, errAppKeyNotFound):
		return huma.Error404NotFound(err.Error())
	case errors.Is(err, errAppKeyUnavailable):
		return huma.Error503ServiceUnavailable(err.Error())
	}
	return err
}

func registerAppKeys(api huma.API, g *Gateway) {
	huma.Register(api, huma.Operation{
		OperationID: "createAppKey",
		Method:      http.MethodPost,
		Path:        "/admin/app-keys",
		Summary:     "Mint an app key for the listed namespaces, valid for ttl. The response carries the key once; the gateway stores its hash. Admin only.",
	}, func(ctx context.Context, in *createAppKeyIn) (*createAppKeyOut, error) {
		ttl, err := ParseAppKeyTTL(in.Body.TTL)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		r, key, err := g.createAppKey(ctx, in.Body.Namespaces, ttl, in.Body.Note, adminIdentityFromContext(ctx))
		if err != nil {
			if errors.Is(err, errAppKeyUnavailable) {
				return nil, appKeyHumaErr(err)
			}
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		out := &createAppKeyOut{}
		out.Body.Key = key
		out.Body.AppKey = adminAppKey(r, nowFunc())
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "listAppKeys",
		Method:      http.MethodGet,
		Path:        "/admin/app-keys",
		Summary:     "List app keys the gateway minted, soonest expiry first. Never returns the keys themselves. Admin only.",
	}, func(ctx context.Context, _ *struct{}) (*appKeysOut, error) {
		keys, err := g.listAppKeys(ctx)
		if err != nil {
			return nil, appKeyHumaErr(err)
		}
		out := &appKeysOut{}
		out.Body.AppKeys = []appKeyInfo{}
		now := nowFunc()
		for _, r := range keys {
			out.Body.AppKeys = append(out.Body.AppKeys, adminAppKey(r, now))
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "revokeAppKey",
		Method:      http.MethodDelete,
		Path:        "/admin/app-keys/{id}",
		Summary:     "Revoke an app key: delete it. The next call with it fails; with required app keys a registration that heartbeats with it drops at its TTL. Admin only.",
	}, func(ctx context.Context, in *appKeyIDIn) (*revokeAppKeyOut, error) {
		if err := g.deleteAppKey(ctx, in.ID); err != nil {
			if errors.Is(err, errAppKeyNotFound) {
				return nil, huma.Error404NotFound(fmt.Sprintf("no app key %q", in.ID))
			}
			return nil, appKeyHumaErr(err)
		}
		out := &revokeAppKeyOut{}
		out.Body.Revoked = true
		return out, nil
	})
}
