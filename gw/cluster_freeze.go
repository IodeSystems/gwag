package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Cutover freeze.
//
// During the state copy (cluster_state.go) blue's copied state must
// not change. A freeze is one record, the key "freeze" of the
// gwag-cluster bucket (in memory standalone), with an expiry: an
// operator who crashes mid-copy freezes the cluster for the TTL at
// most. While it holds, every node refuses the admin and
// control-plane writes that change copied state:
//
//	stable / deprecated   RetractStable, Deprecate, Undeprecate
//	                      (control plane and admin routes)
//	app and system keys   createAppKey, createSystemKey, revokeAppKey
//	external dependents   putExternalDependent, reviewExternalDependent,
//	                      deleteExternalDependent
//	Schema Registry       REST writes (register, delete, config,
//	                      mode, import); a register of a schema the
//	                      subject already holds is answered
//
// with HTTP 409 / gRPC FailedPrecondition. Registrations, heartbeats,
// drains and client traffic are not frozen. Each check reads the
// record from the bucket's stream leader (kvLeaderEntry), so a freeze
// set on one node applies on every node at the next write; when the
// leader cannot be asked, the write fails (503 / Unavailable) rather
// than pass unchecked. Gateways older than the freeze do not read the
// record.

const (
	clusterFreezeKey = "freeze"
	freezeDefaultTTL = 10 * time.Minute
	freezeMaxTTL     = 24 * time.Hour
)

// freezeRecord is the value of clusterFreezeKey.
type freezeRecord struct {
	UntilUnixMs int64  `json:"until_unix_ms"`
	SetUnixMs   int64  `json:"set_unix_ms"`
	SetBy       string `json:"set_by,omitempty"`
	Note        string `json:"note,omitempty"`
}

func (f freezeRecord) until() time.Time { return time.UnixMilli(f.UntilUnixMs) }

// active reports whether the freeze holds at now.
func (f freezeRecord) active(now time.Time) bool { return now.Before(f.until()) }

// cutoverError is an error with an HTTP status (huma.StatusError) and
// a gRPC code, so one value answers both the admin routes and the
// control plane. Its JSON is a problem document like huma's.
type cutoverError struct {
	status int
	code   codes.Code
	msg    string
}

func (e *cutoverError) Error() string { return e.msg }

// GetStatus implements huma.StatusError.
func (e *cutoverError) GetStatus() int { return e.status }

// GRPCStatus is read by grpc's status.FromError.
func (e *cutoverError) GRPCStatus() *status.Status { return status.New(e.code, e.msg) }

// ContentType implements huma.ContentTypeFilter.
func (e *cutoverError) ContentType(string) string { return "application/problem+json" }

func (e *cutoverError) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any{"title": http.StatusText(e.status), "status": e.status, "detail": e.msg})
}

// errFrozen is the error of a write refused by f.
func errFrozen(what string, f freezeRecord) *cutoverError {
	by := ""
	if f.SetBy != "" {
		by = " by " + f.SetBy
	}
	return &cutoverError{
		status: http.StatusConflict,
		code:   codes.FailedPrecondition,
		msg:    fmt.Sprintf("cluster frozen for cutover until %s (set%s): %s is refused; `gwag cluster unfreeze` ends the freeze", f.until().UTC().Format(time.RFC3339), by, what),
	}
}

func errFreezeUnavailable(err error) *cutoverError {
	return &cutoverError{status: http.StatusServiceUnavailable, code: codes.Unavailable, msg: "cutover freeze: cannot read the freeze record: " + err.Error()}
}

// getFreeze returns the freeze in force (ok false when there is none
// or it expired).
func (g *Gateway) getFreeze(ctx context.Context) (freezeRecord, bool, error) {
	now := nowFunc()
	cl := g.cfg.cluster
	if cl == nil {
		p := g.freeze.Load()
		if p == nil || !p.active(now) {
			return freezeRecord{}, false, nil
		}
		return *p, true, nil
	}
	if cl.Conn == nil {
		return freezeRecord{}, false, errors.New("cluster connection not ready")
	}
	kctx, cancel := kvCtx(ctx)
	defer cancel()
	v, ok, err := kvLeaderGet(kctx, cl.Conn, clusterBucketName, clusterFreezeKey)
	if err != nil {
		return freezeRecord{}, false, err
	}
	if !ok {
		return freezeRecord{}, false, nil
	}
	var f freezeRecord
	if err := json.Unmarshal(v, &f); err != nil {
		return freezeRecord{}, false, fmt.Errorf("freeze record: %w", err)
	}
	if !f.active(now) {
		return freezeRecord{}, false, nil
	}
	return f, true, nil
}

// checkNotFrozen returns nil when no freeze holds; otherwise the
// error that refuses what.
func (g *Gateway) checkNotFrozen(ctx context.Context, what string) error {
	f, ok, err := g.getFreeze(ctx)
	if err != nil {
		return errFreezeUnavailable(err)
	}
	if ok {
		return errFrozen(what, f)
	}
	return nil
}

// clusterKV returns the gwag-cluster bucket; nil before the node
// joined JetStream.
func (g *Gateway) clusterKV() jetstream.KeyValue {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.peers == nil {
		return nil
	}
	return g.peers.cluster
}

// setFreeze freezes the cluster for ttl (replacing a freeze in force).
func (g *Gateway) setFreeze(ctx context.Context, ttl time.Duration, note, by string) (freezeRecord, error) {
	if ttl <= 0 || ttl > freezeMaxTTL {
		return freezeRecord{}, fmt.Errorf("ttl must be positive and at most %s", freezeMaxTTL)
	}
	now := nowFunc()
	f := freezeRecord{UntilUnixMs: now.Add(ttl).UnixMilli(), SetUnixMs: now.UnixMilli(), SetBy: by, Note: sanitizeSubject(note)}
	if g.cfg.cluster == nil {
		g.freeze.Store(&f)
		return f, nil
	}
	kv := g.clusterKV()
	if kv == nil {
		return freezeRecord{}, errAppKeyUnavailable
	}
	b, _ := json.Marshal(f)
	kctx, cancel := context.WithTimeout(ctx, 2*kvCallTimeout)
	defer cancel()
	if err := kvPutRetry(kctx, kv, clusterFreezeKey, b); err != nil {
		return freezeRecord{}, fmt.Errorf("freeze: kv put: %w", err)
	}
	return f, nil
}

// clearFreeze ends the freeze. Returns whether one was in force.
func (g *Gateway) clearFreeze(ctx context.Context) (bool, error) {
	_, was, err := g.getFreeze(ctx)
	if err != nil {
		return false, err
	}
	if g.cfg.cluster == nil {
		g.freeze.Store(nil)
		return was, nil
	}
	kv := g.clusterKV()
	if kv == nil {
		return false, errAppKeyUnavailable
	}
	kctx, cancel := context.WithTimeout(ctx, 2*kvCallTimeout)
	defer cancel()
	if err := kvDeleteRetry(kctx, kv, clusterFreezeKey); err != nil {
		return false, fmt.Errorf("unfreeze: kv delete: %w", err)
	}
	return was, nil
}

// ---- admin routes ---------------------------------------------------

type freezeInfo struct {
	Frozen bool   `json:"frozen"`
	Until  string `json:"until,omitempty" doc:"RFC 3339. The freeze ends by itself then."`
	SetAt  string `json:"setAt,omitempty" doc:"RFC 3339."`
	SetBy  string `json:"setBy,omitempty" doc:"Authenticated identity that set the freeze."`
	Note   string `json:"note,omitempty"`
}

func freezeInfoOf(f freezeRecord, ok bool) freezeInfo {
	if !ok {
		return freezeInfo{}
	}
	return freezeInfo{
		Frozen: true,
		Until:  f.until().UTC().Format(time.RFC3339),
		SetAt:  time.UnixMilli(f.SetUnixMs).UTC().Format(time.RFC3339),
		SetBy:  f.SetBy,
		Note:   f.Note,
	}
}

type freezeIn struct {
	Body struct {
		TTL  string `json:"ttl,omitempty" doc:"How long the freeze holds: a Go duration (10m) or days (1d). Default 10m, at most 24h. Freezing again replaces the end time."`
		Note string `json:"note,omitempty" doc:"Shown in the refusals and the freeze state."`
	}
}

type freezeOut struct {
	Body freezeInfo
}

type unfreezeOut struct {
	Body struct {
		WasFrozen bool `json:"wasFrozen"`
	}
}

func freezeHumaErr(err error) error {
	var ce *cutoverError
	switch {
	case errors.As(err, &ce):
		return ce
	case errors.Is(err, errAppKeyUnavailable):
		return huma.Error503ServiceUnavailable("cluster store not ready")
	}
	return err
}

func registerClusterFreeze(api huma.API, g *Gateway) {
	huma.Register(api, huma.Operation{
		OperationID: "freezeCluster",
		Method:      http.MethodPost,
		Path:        "/admin/cluster/freeze",
		Summary:     "Freeze the admin writes that change cluster state copied in a cutover (stable, deprecated, app and system keys, external dependents, Schema Registry writes) on every node, for ttl. Registrations and traffic are not frozen. Admin only.",
	}, func(ctx context.Context, in *freezeIn) (*freezeOut, error) {
		ttl := freezeDefaultTTL
		if in.Body.TTL != "" {
			d, err := ParseAppKeyTTL(in.Body.TTL)
			if err != nil {
				return nil, huma.Error422UnprocessableEntity(err.Error())
			}
			ttl = d
		}
		if ttl > freezeMaxTTL {
			return nil, huma.Error422UnprocessableEntity(fmt.Sprintf("ttl must be at most %s", freezeMaxTTL))
		}
		f, err := g.setFreeze(ctx, ttl, in.Body.Note, adminIdentityFromContext(ctx))
		if err != nil {
			return nil, freezeHumaErr(err)
		}
		return &freezeOut{Body: freezeInfoOf(f, true)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "unfreezeCluster",
		Method:      http.MethodPost,
		Path:        "/admin/cluster/unfreeze",
		Summary:     "End the cutover freeze on every node. Admin only.",
	}, func(ctx context.Context, _ *struct{}) (*unfreezeOut, error) {
		was, err := g.clearFreeze(ctx)
		if err != nil {
			if !errors.Is(err, errAppKeyUnavailable) {
				err = errFreezeUnavailable(err)
			}
			return nil, freezeHumaErr(err)
		}
		out := &unfreezeOut{}
		out.Body.WasFrozen = was
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "getClusterFreeze",
		Method:      http.MethodGet,
		Path:        "/admin/cluster/freeze",
		Summary:     "The cutover freeze in force, if any.",
	}, func(ctx context.Context, _ *struct{}) (*freezeOut, error) {
		f, ok, err := g.getFreeze(ctx)
		if err != nil {
			return nil, errFreezeUnavailable(err)
		}
		return &freezeOut{Body: freezeInfoOf(f, ok)}, nil
	})
}
