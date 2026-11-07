//go:build e2e

package harness

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"
)

// Declarations: the review op, the notifications read and the
// declaration sinks (gw/declaration_sinks.go).

// ReviewExternalDependent marks id reviewed (POST
// /api/admin/external-dependents/{id}/review) and returns the stored
// declaration.
func (a *Admin) ReviewExternalDependent(ctx context.Context, id, reviewAt, by string) (*ExternalDependent, error) {
	var out struct {
		ExternalDependent ExternalDependent `json:"externalDependent"`
	}
	body := map[string]any{"reviewAt": reviewAt}
	if by != "" {
		body["by"] = by
	}
	if err := a.Do(ctx, http.MethodPost, "/admin/external-dependents/"+url.PathEscape(id)+"/review", body, &out); err != nil {
		return nil, err
	}
	return &out.ExternalDependent, nil
}

// Notification is one item of GET /api/admin/notifications.
type Notification struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Since  string `json:"since"`
	Href   string `json:"href"`
}

// Notifications lists the items that need a person's attention.
func (a *Admin) Notifications(ctx context.Context) ([]Notification, error) {
	var out struct {
		Items []Notification `json:"items"`
	}
	if err := a.Do(ctx, http.MethodGet, "/admin/notifications", nil, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// HookRequest is one POST a HookSink received.
type HookRequest struct {
	Header http.Header
	Body   []byte
}

// HookSink is a real HTTP receiver for --declarations-hook: it
// records every request and answers 204.
type HookSink struct {
	URL string

	mu   sync.Mutex
	reqs []HookRequest
}

// NewHookSink listens on 127.0.0.1 and stops when the test ends.
func NewHookSink(t testing.TB) *HookSink {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(FreePort(t)))
	if err != nil {
		t.Fatalf("harness: hook sink: %v", err)
	}
	h := &HookSink{URL: "http://" + ln.Addr().String() + "/hook"}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.reqs = append(h.reqs, HookRequest{Header: r.Header.Clone(), Body: b})
		h.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return h
}

// Requests returns a copy of the requests so far.
func (h *HookSink) Requests() []HookRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]HookRequest(nil), h.reqs...)
}

// WaitN waits until exactly n requests have arrived, then checks for
// settle that no more arrive. It fails on more than n.
func (h *HookSink) WaitN(t testing.TB, n int, timeout, settle time.Duration) []HookRequest {
	t.Helper()
	count := func() error {
		if got := len(h.Requests()); got > n {
			return Permanent(fmt.Errorf("hook sink: %d requests, want %d", got, n))
		} else if got < n {
			return fmt.Errorf("hook sink: %d requests, want %d", got, n)
		}
		return nil
	}
	Eventually(t, timeout, fmt.Sprintf("%d hook requests", n), count)
	Consistently(t, settle, fmt.Sprintf("still %d hook requests", n), count)
	return h.Requests()
}

// VerifyHookSignature checks X-Gwag-Signature on r against secret.
func VerifyHookSignature(r HookRequest, secret string) error {
	ts := r.Header.Get("X-Gwag-Timestamp")
	if ts == "" {
		return fmt.Errorf("no X-Gwag-Timestamp")
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts + "."))
	m.Write(r.Body)
	want := "sha256=" + hex.EncodeToString(m.Sum(nil))
	if !hmac.Equal([]byte(r.Header.Get("X-Gwag-Signature")), []byte(want)) {
		return fmt.Errorf("signature %q, want %q", r.Header.Get("X-Gwag-Signature"), want)
	}
	return nil
}

// ClusterWithNodeArgs is Cluster with extra flags per node: nodeArgs
// gets each node's name and data directory (e.g. for a per-node file
// path) and returns flags appended after opts.Args.
func (e *Env) ClusterWithNodeArgs(n int, opts GatewayOpts, nodeArgs func(name, dataDir string) []string) *Cluster {
	e.t.Helper()
	c := &Cluster{env: e, opts: opts}
	for i := 1; i <= n; i++ {
		o := opts
		o.Name = "n" + strconv.Itoa(i)
		o.NATS = true
		g := e.newGateway(o)
		g.opts.Args = append(append([]string(nil), opts.Args...), nodeArgs(g.Name, g.DataDir)...)
		c.Nodes = append(c.Nodes, g)
	}
	for _, g := range c.Nodes {
		g.Proc = e.Start(g.Name, Binary(e.t, "gateway"), g.args(c.peersOf(g))...)
	}
	for _, g := range c.Nodes {
		g.WaitReady()
	}
	c.WaitPeers(n, 60*time.Second)
	return c
}
