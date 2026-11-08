//go:build e2e

package harness

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// PeerView is one entry of GET /admin/peers.
type PeerView struct {
	NodeID   string `json:"nodeId"`
	Name     string `json:"name"`
	Version  string `json:"version"`
	MinLevel int    `json:"minLevel"`
	MaxLevel int    `json:"maxLevel"`
}

// PeersView is GET /admin/peers: the peers, the cluster feature level
// and the JetStream meta group as the answering node sees it.
type PeersView struct {
	Peers       []PeerView `json:"peers"`
	ActiveLevel int        `json:"activeLevel"`
	MetaLeader  string     `json:"metaLeader"`
	MetaSize    int        `json:"metaSize"`
	MetaOffline []string   `json:"metaOffline"`
}

// Peer returns the entry with the given name, or nil.
func (v *PeersView) Peer(name string) *PeerView {
	for i := range v.Peers {
		if v.Peers[i].Name == name {
			return &v.Peers[i]
		}
	}
	return nil
}

// PeersView reads GET /api/admin/peers.
func (g *Gateway) PeersView(ctx context.Context) (*PeersView, error) {
	var out PeersView
	if err := g.Get(ctx, "/api/admin/peers", &out, Bearer(g.Token())); err != nil {
		return nil, err
	}
	return &out, nil
}

// NodeID is the node's NATS server id, from /api/health.
func (g *Gateway) NodeID(t testing.TB) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	body, _, err := g.Health(ctx)
	if err != nil {
		t.Fatalf("%s health: %v", g.Name, err)
	}
	id, _ := body["node_id"].(string)
	if id == "" {
		t.Fatalf("%s health has no node_id: %v", g.Name, body)
	}
	return id
}

// EvacuateResult is POST /admin/peers/{id}/evacuate.
type EvacuateResult struct {
	Name      string   `json:"name"`
	Remaining []string `json:"remaining"`
	Done      bool     `json:"done"`
	Left      bool     `json:"left"`
}

// Evacuate asks g to move every stream replica off the live peer
// nodeID, and asks again until no stream is on it and every replica
// is current (a replica raise in between can place one back, the way
// `gwag peer evacuate --wait` does). With leave, the peer is then
// removed from the JetStream meta group.
//
// The gateway refuses to start an evacuation (409) while a bucket or
// event stream is below the cluster's replica count or has a replica
// that is not current; that is asked again too, as --wait does, and
// each new refusal is logged.
func (g *Gateway) Evacuate(t testing.TB, nodeID string, leave bool, timeout time.Duration) EvacuateResult {
	t.Helper()
	var res EvacuateResult
	lastRefusal := ""
	Eventually(t, timeout, fmt.Sprintf("%s evacuates %s", g.Name, nodeID), func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var r EvacuateResult
		if err := g.Post(ctx, "/api/admin/peers/"+nodeID+"/evacuate", map[string]any{"leave": leave}, &r, Bearer(g.Token())); err != nil {
			var he *HTTPError
			if errors.As(err, &he) && he.Status == http.StatusConflict && he.Body != lastRefusal {
				lastRefusal = he.Body
				t.Logf("%s: evacuation of %s not started yet: %s", g.Name, nodeID, he.Body)
			}
			return err
		}
		res = r
		if !r.Done {
			return fmt.Errorf("remaining %v", r.Remaining)
		}
		if leave && !r.Left {
			return errors.New("done but not left")
		}
		return nil
	})
	return res
}

// ForgetResult is POST /admin/peers/{id}/forget.
type ForgetResult struct {
	Removed     bool `json:"removed"`
	NewReplicas int  `json:"newReplicas"`
	MetaRemoved bool `json:"metaRemoved"`
}

// Forget waits until the stopped peer nodeID has expired (ForgetPeer
// refuses a live one), then forgets it through g.
func (g *Gateway) Forget(t testing.TB, nodeID string, timeout time.Duration) ForgetResult {
	t.Helper()
	var res ForgetResult
	Eventually(t, timeout, fmt.Sprintf("%s forgets %s", g.Name, nodeID), func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return g.Post(ctx, "/api/admin/peers/"+nodeID+"/forget", nil, &res, Bearer(g.Token()))
	})
	return res
}

// FinalizeResult is POST /admin/cluster/finalize.
type FinalizeResult struct {
	PreviousLevel int      `json:"previousLevel"`
	ActiveLevel   int      `json:"activeLevel"`
	Blockers      []string `json:"blockers"`
}

// Finalize posts /admin/cluster/finalize and returns the result and
// the HTTP status (409 when a live peer blocks the raise).
func (g *Gateway) Finalize(ctx context.Context, level int, dryRun bool) (FinalizeResult, int, error) {
	var out FinalizeResult
	body := map[string]any{"dryRun": dryRun}
	if level > 0 {
		body["level"] = level
	}
	status, err := g.do(ctx, http.MethodPost, "/api/admin/cluster/finalize", body, &out, []ReqOpt{Bearer(g.Token())})
	return out, status, err
}
