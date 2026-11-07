package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Declaration change sinks (declaration_sinks.go).

type hookRequest struct {
	header http.Header
	body   []byte
}

// hookSink is an HTTP receiver that records every POST and answers
// with the status statuses returns for the n-th request (from 1).
type hookSink struct {
	srv      *httptest.Server
	mu       sync.Mutex
	reqs     []hookRequest
	statuses func(n int) int
}

func newHookSink(t *testing.T, statuses func(n int) int) *hookSink {
	t.Helper()
	h := &hookSink{statuses: statuses}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.reqs = append(h.reqs, hookRequest{header: r.Header.Clone(), body: b})
		n := len(h.reqs)
		statuses := h.statuses
		h.mu.Unlock()
		code := http.StatusOK
		if statuses != nil {
			code = statuses(n)
		}
		w.WriteHeader(code)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *hookSink) requests() []hookRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]hookRequest(nil), h.reqs...)
}

func (h *hookSink) waitN(t *testing.T, n int) []hookRequest {
	t.Helper()
	waitUntil(t, 10*time.Second, strconv.Itoa(n)+" hook requests", func() bool { return len(h.requests()) >= n })
	return h.requests()
}

type deltaBody struct {
	Schema string
	Seq    uint64
	Op     string
	ID     string
	Before *struct {
		ID, Name, ReviewAt, ReviewedBy, UpdatedBy string
	}
	After *struct {
		ID, Name, ReviewAt, ReviewedBy, UpdatedBy string
	}
	At string
	By string
}

type fullBody struct {
	Schema       string
	Seq          uint64
	At           string
	Declarations []struct {
		ID, Name, ReviewAt string
		Depends            []struct {
			Service string
			Lines   []string
		}
	}
}

func fastHook(url string, mode DeclarationsSinkMode, secret string) DeclarationsHookOptions {
	return DeclarationsHookOptions{URL: url, Mode: mode, Secret: []byte(secret), MaxAttempts: 3, InitialBackoff: 10 * time.Millisecond, MaxBackoff: 40 * time.Millisecond}
}

func TestDeclarationsHook_DeltaSignedPutReviewDelete(t *testing.T) {
	sink := newHookSink(t, nil)
	g := newExternalGateway(t, WithDeclarationsHook(fastHook(sink.srv.URL, DeclarationsDelta, strongTestSecret)))

	putExternal(t, g, "acme", acmeV1)
	if code, body := adminDo(t, g, http.MethodPost, "/admin/external-dependents/acme/review", `{"reviewAt":"2027-09-30T00:00:00Z"}`); code != http.StatusOK {
		t.Fatalf("review: %d %s", code, body)
	}
	if code, body := adminDo(t, g, http.MethodDelete, "/admin/external-dependents/acme", ""); code != http.StatusOK {
		t.Fatalf("delete: %d %s", code, body)
	}
	reqs := sink.waitN(t, 3)
	wantOps := []string{"put", "review", "delete"}
	for i, r := range reqs {
		ts := r.header.Get("X-Gwag-Timestamp")
		if _, err := strconv.ParseInt(ts, 10, 64); err != nil {
			t.Fatalf("req %d: timestamp %q", i, ts)
		}
		if got, want := r.header.Get("X-Gwag-Signature"), "sha256="+signDeclarationsHook([]byte(strongTestSecret), ts, r.body); got != want {
			t.Errorf("req %d: signature %q, want %q", i, got, want)
		}
		if signDeclarationsHook([]byte("other"), ts, r.body) == strings.TrimPrefix(r.header.Get("X-Gwag-Signature"), "sha256=") {
			t.Errorf("req %d: signature verifies with the wrong secret", i)
		}
		if r.header.Get("X-Gwag-Event") != wantOps[i] || r.header.Get("X-Gwag-Delivery") == "" {
			t.Errorf("req %d headers: %v", i, r.header)
		}
		var d deltaBody
		if err := json.Unmarshal(r.body, &d); err != nil {
			t.Fatal(err)
		}
		if d.Schema != "gwag.declarations.delta/v1" || d.Op != wantOps[i] || d.ID != "acme" || d.Seq != uint64(i+1) || d.At == "" {
			t.Errorf("req %d: %s", i, r.body)
		}
		switch d.Op {
		case "put":
			if d.Before != nil || d.After == nil || d.After.Name != "Acme Corp" || d.After.ReviewAt != "2027-06-30T00:00:00Z" || d.After.UpdatedBy != "admin" || d.By != "admin" {
				t.Errorf("put: %s", r.body)
			}
		case "review":
			if d.Before == nil || d.Before.ReviewAt != "2027-06-30T00:00:00Z" || d.After == nil || d.After.ReviewAt != "2027-09-30T00:00:00Z" || d.After.ReviewedBy != "admin" || d.By != "admin" {
				t.Errorf("review: %s", r.body)
			}
		case "delete":
			if d.Before == nil || d.Before.ID != "acme" || d.After != nil || d.By != "admin" {
				t.Errorf("delete: %s", r.body)
			}
		}
	}
}

func TestDeclarationsHook_FullMode(t *testing.T) {
	sink := newHookSink(t, nil)
	g := newExternalGateway(t, WithDeclarationsHook(fastHook(sink.srv.URL, DeclarationsFull, "")))
	putExternal(t, g, "acme", acmeV1)
	putExternal(t, g, "beta", strings.Replace(acmeV1, "Acme Corp", "Beta", 1))
	reqs := sink.waitN(t, 2)
	if reqs[0].header.Get("X-Gwag-Signature") != "" {
		t.Errorf("signed without a secret")
	}
	var f fullBody
	if err := json.Unmarshal(reqs[1].body, &f); err != nil {
		t.Fatal(err)
	}
	if f.Schema != "gwag.declarations.full/v1" || f.Seq != 2 || f.At == "" || len(f.Declarations) != 2 ||
		f.Declarations[0].ID != "acme" || f.Declarations[1].Name != "Beta" || f.Declarations[0].Depends[0].Lines[0] != "v1" {
		t.Errorf("full body: %s", reqs[1].body)
	}
}

func TestDeclarationsHook_RetriesThenSucceeds(t *testing.T) {
	sink := newHookSink(t, func(n int) int {
		if n < 3 {
			return http.StatusInternalServerError
		}
		return http.StatusNoContent
	})
	g := New(WithoutBackpressure(), WithAdminToken([]byte("tok")), WithDeclarationsHook(fastHook(sink.srv.URL, DeclarationsDelta, strongTestSecret)))
	t.Cleanup(g.Close)
	putExternal(t, g, "acme", acmeV1)
	reqs := sink.waitN(t, 3)
	if reqs[0].header.Get("X-Gwag-Delivery") != reqs[2].header.Get("X-Gwag-Delivery") {
		t.Errorf("a retry changed the delivery id")
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(sink.requests()); n != 3 {
		t.Errorf("requests=%d, want 3", n)
	}
	wantMetric(t, g, 0, "declarations_hook_failures_total")
	if items := notificationItems(t, g); len(items) != 0 {
		t.Errorf("notifications=%+v", items)
	}
}

func TestDeclarationsHook_GivesUp(t *testing.T) {
	sink := newHookSink(t, func(int) int { return http.StatusInternalServerError })
	g := New(WithoutBackpressure(), WithAdminToken([]byte("tok")), WithDeclarationsHook(fastHook(sink.srv.URL, DeclarationsDelta, strongTestSecret)))
	t.Cleanup(g.Close)

	start := time.Now()
	putExternal(t, g, "acme", acmeV1)
	if d := time.Since(start); d > time.Second {
		t.Errorf("PUT waited on the hook: %v", d)
	}
	sink.waitN(t, 3)
	wantMetric(t, g, 1, "declarations_hook_failures_total")
	if n := len(sink.requests()); n != 3 {
		t.Errorf("requests=%d, want MaxAttempts=3", n)
	}
	var failed []notificationTestItem
	for _, it := range notificationItems(t, g) {
		if it.Kind == "declaration_hook_failed" {
			failed = append(failed, it)
		}
	}
	if len(failed) != 1 || !strings.Contains(failed[0].Detail, "put acme (seq 1)") || !strings.Contains(failed[0].Detail, "3 tries") ||
		failed[0].Href != "/?select=ext:acme" {
		t.Fatalf("hook failure items=%+v", failed)
	}

	// A 4xx is not retried.
	sink.mu.Lock()
	sink.statuses = func(int) int { return http.StatusUnauthorized }
	sink.mu.Unlock()
	putExternal(t, g, "beta", acmeV1)
	sink.waitN(t, 4)
	wantMetric(t, g, 2, "declarations_hook_failures_total")
	time.Sleep(100 * time.Millisecond)
	if n := len(sink.requests()); n != 4 {
		t.Errorf("4xx retried: requests=%d, want 4", n)
	}
}

func readSinkFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDeclarationsFile_FullAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "declarations.json")
	g := newExternalGateway(t, WithDeclarationsFile(DeclarationsFileOptions{Path: path, Mode: DeclarationsFull}))

	// Standalone: the file exists from New, with the empty set.
	var f fullBody
	if err := json.Unmarshal([]byte(readSinkFile(t, path)), &f); err != nil || f.Schema != "gwag.declarations.full/v1" || f.Seq != 0 || f.Declarations == nil || len(f.Declarations) != 0 {
		t.Fatalf("initial file: %v %+v", err, f)
	}
	old, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()

	putExternal(t, g, "acme", acmeV1)
	if err := json.Unmarshal([]byte(readSinkFile(t, path)), &f); err != nil || f.Seq != 1 || len(f.Declarations) != 1 || f.Declarations[0].ID != "acme" {
		t.Fatalf("after put: %v %+v", err, f)
	}
	// Replaced, not rewritten in place: a reader holding the old file
	// still reads the old, complete content.
	b, _ := io.ReadAll(old)
	if !strings.Contains(string(b), `"declarations": []`) {
		t.Errorf("old file changed under its reader: %s", b)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o644 {
		t.Errorf("mode=%v err=%v, want 0644", st.Mode().Perm(), err)
	}

	if code, _ := adminDo(t, g, http.MethodDelete, "/admin/external-dependents/acme", ""); code != http.StatusOK {
		t.Fatal("delete failed")
	}
	if err := json.Unmarshal([]byte(readSinkFile(t, path)), &f); err != nil || f.Seq != 2 || len(f.Declarations) != 0 {
		t.Fatalf("after delete: %v %+v", err, f)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("temp files left behind: %v", entries)
	}
}

func TestDeclarationsFile_DeltaAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "declarations.jsonl")
	g := newExternalGateway(t, WithDeclarationsFile(DeclarationsFileOptions{Path: path, Mode: DeclarationsDelta}))
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("delta file written before any change: %v", err)
	}
	putExternal(t, g, "acme", acmeV1)
	adminDo(t, g, http.MethodPost, "/admin/external-dependents/acme/review", `{"reviewAt":"2027-09-30T00:00:00Z"}`)
	adminDo(t, g, http.MethodDelete, "/admin/external-dependents/acme", "")
	lines := strings.Split(strings.TrimSpace(readSinkFile(t, path)), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines=%q", lines)
	}
	for i, op := range []string{"put", "review", "delete"} {
		var d deltaBody
		if err := json.Unmarshal([]byte(lines[i]), &d); err != nil || d.Op != op || d.Seq != uint64(i+1) || d.Schema != "gwag.declarations.delta/v1" {
			t.Errorf("line %d: %v %s", i, err, lines[i])
		}
	}
}

func TestDeclarationsSinks_BadOptionsPanic(t *testing.T) {
	for name, opt := range map[string]Option{
		"hook mode": WithDeclarationsHook(DeclarationsHookOptions{URL: "http://x", Mode: "both"}),
		"hook url":  WithDeclarationsHook(DeclarationsHookOptions{}),
		"file path": WithDeclarationsFile(DeclarationsFileOptions{Mode: DeclarationsFull}),
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: no panic", name)
				}
			}()
			New(WithoutMetrics(), opt).Close()
		}()
	}
}

// Cluster: three gateways, each with the same hook and its own file.
// One POST per change, from the node that accepted the write; every
// node's file follows the replicated set.
func TestDeclarationsSinks_Cluster(t *testing.T) {
	resetSharedCluster(t)
	_, _ = getSharedCluster(t)
	sink := newHookSink(t, nil)
	dir := t.TempDir()
	var nodes []*Gateway
	add := func(cl *Cluster) {
		i := len(nodes)
		g := New(WithCluster(cl), WithClusterReplicas(ClusterReplicasAuto), WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("tok")),
			WithDeclarationsHook(fastHook(sink.srv.URL, DeclarationsDelta, strongTestSecret)),
			WithDeclarationsFile(DeclarationsFileOptions{Path: filepath.Join(dir, "n"+strconv.Itoa(i)+".json"), Mode: DeclarationsFull}),
		)
		t.Cleanup(g.Close)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		if _, err := g.startClusterTracking(ctx); err != nil {
			t.Fatalf("node %d: %v", i, err)
		}
		nodes = append(nodes, g)
	}
	add(testCluster.a)
	add(testCluster.b)
	waitForPeers(t, nodes[0], nodes[1])
	// The third gateway joins once the buckets are at R=2: a put sent
	// while a bucket's replica count changes can get no ack.
	waitUntil(t, 15*time.Second, "buckets at R=2", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		s, err := testCluster.a.JS.Stream(ctx, "KV_"+externalDependentsBucketName)
		if err != nil {
			return false
		}
		info, err := s.Info(ctx)
		return err == nil && info.Config.Replicas == 2
	})
	add(testCluster.a)

	fileHas := func(i int, ids ...string) func() bool {
		return func() bool {
			b, err := os.ReadFile(filepath.Join(dir, "n"+strconv.Itoa(i)+".json"))
			if err != nil {
				return false
			}
			var f fullBody
			if json.Unmarshal(b, &f) != nil || len(f.Declarations) != len(ids) {
				return false
			}
			for j, id := range ids {
				if f.Declarations[j].ID != id {
					return false
				}
			}
			return true
		}
	}

	putExternal(t, nodes[0], "acme", acmeV1)
	putExternal(t, nodes[1], "beta", acmeV1)
	waitUntil(t, 10*time.Second, "acme on node 2", func() bool { _, ok := nodes[2].externals.get("acme"); return ok })
	if code, body := adminDo(t, nodes[2], http.MethodPost, "/admin/external-dependents/acme/review", `{"reviewAt":"2027-09-30T00:00:00Z"}`); code != http.StatusOK {
		t.Fatalf("review on node 2: %d %s", code, body)
	}
	for i := range nodes {
		waitUntil(t, 10*time.Second, "node "+strconv.Itoa(i)+" file has acme and beta", fileHas(i, "acme", "beta"))
	}
	waitUntil(t, 10*time.Second, "beta on node 2", func() bool { _, ok := nodes[2].externals.get("beta"); return ok })
	if code, body := adminDo(t, nodes[2], http.MethodDelete, "/admin/external-dependents/beta", ""); code != http.StatusOK {
		t.Fatalf("delete: %d %s", code, body)
	}
	for i := range nodes {
		waitUntil(t, 10*time.Second, "node "+strconv.Itoa(i)+" file has acme only", fileHas(i, "acme"))
	}

	reqs := sink.waitN(t, 4)
	time.Sleep(300 * time.Millisecond) // room for a duplicate to show up
	reqs = sink.requests()
	// Nodes POST concurrently: order by seq, the KV revision.
	bySeq := map[uint64]string{}
	var seqs []uint64
	for _, r := range reqs {
		var d deltaBody
		if err := json.Unmarshal(r.body, &d); err != nil {
			t.Fatal(err)
		}
		if _, dup := bySeq[d.Seq]; d.Seq == 0 || dup {
			t.Errorf("seq %d zero or repeated: %s", d.Seq, r.body)
		}
		bySeq[d.Seq] = d.Op + " " + d.ID
		seqs = append(seqs, d.Seq)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	var ops []string
	for _, q := range seqs {
		ops = append(ops, bySeq[q])
	}
	if strings.Join(ops, ",") != "put acme,put beta,review acme,delete beta" {
		t.Errorf("hook POSTs=%v, want one per change", ops)
	}
}
