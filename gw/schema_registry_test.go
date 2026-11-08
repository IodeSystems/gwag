package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	sr "github.com/iodesystems/gwag/v2/events/schemaregistry"
	"github.com/iodesystems/gwag/v2/events/schemaregistry/srtest"
	ordersv1 "github.com/iodesystems/gwag/v2/examples/events/gen/orders/v1"
	"github.com/iodesystems/gwag/v2/gw/internal/srserve"
	cpv1 "github.com/iodesystems/gwag/v2/gw/proto/controlplane/v1"
)

// srServer mounts g's registry under /schema-registry.
func srServer(t *testing.T, g *Gateway) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("/schema-registry/", http.StripPrefix("/schema-registry", g.SchemaRegistryHandler()))
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s.URL + "/schema-registry"
}

func srClient(t *testing.T, url string, cfg sr.Config) *sr.Client {
	t.Helper()
	cfg.URL = url
	c, err := sr.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// addOrdersProto registers examples/events/proto/orders/v1/orders.proto
// under its import path, as a service would.
func addOrdersProto(t *testing.T, g *Gateway) {
	t.Helper()
	src, err := os.ReadFile("../examples/events/proto/orders/v1/orders.proto")
	if err != nil {
		t.Fatal(err)
	}
	opts, err := os.ReadFile("proto/ps/v1/options.proto")
	if err != nil {
		t.Fatal(err)
	}
	imports := map[string][]byte{"gw/proto/ps/v1/options.proto": opts}
	if err := g.AddProtoBytes("orders/v1/orders.proto", src, To("127.0.0.1:1"), As("orders"), ProtoImports(imports)); err != nil {
		t.Fatal(err)
	}
	g.Handler() // assembles the schema and the channel index
}

// waitSubject polls until subject has a version.
func waitSubject(t *testing.T, c *sr.Client, subject string) *sr.Schema {
	t.Helper()
	var got *sr.Schema
	waitUntilErr(t, 20*time.Second, "subject "+subject, func() error {
		s, err := c.SubjectVersion(context.Background(), subject, "latest", "")
		got = s
		return err
	})
	return got
}

// A proto slot's named channels show up as subjects, and the Go serde
// in lookup mode (generated code, protoc) finds what gwag registered
// from source (protocompile): same canonical bytes, same id.
func TestSchemaRegistry_OwnedChannels(t *testing.T) {
	g := New(WithoutMetrics(), WithoutBackpressure(), WithSchemaRegistry(SchemaRegistryOptions{
		Topics: map[string]string{"orders.v1.OrderShipped": "shipped"},
	}))
	t.Cleanup(g.Close)
	addOrdersProto(t, g)
	url := srServer(t, g)
	c := srClient(t, url, sr.Config{})
	shipped := waitSubject(t, c, "shipped-value")
	evs := waitSubject(t, c, "EV_orders_v1_OrderEvents-value")
	if shipped.ID != evs.ID {
		t.Errorf("one file, two ids: %d %d", shipped.ID, evs.ID)
	}
	ctx := context.Background()
	found, err := c.EnsureFile(ctx, "shipped-value", ordersv1.File_orders_v1_orders_proto, sr.RegisterOptions{})
	if err != nil || found.ID != shipped.ID {
		t.Fatalf("serde lookup: %+v %v (gwag registered id %d)", found, err, shipped.ID)
	}
	// The REST API is READONLY: the admin token does not register.
	admin := srClient(t, url, sr.Config{Username: "u", Password: g.AdminTokenHex()})
	_, err = admin.Register(ctx, "x-value", sr.Schema{Schema: `syntax = "proto3"; message X {}`}, false)
	var e *sr.Error
	if !errors.As(err, &e) || e.Code != 42205 {
		t.Errorf("register on READONLY: %v", err)
	}
}

func TestSchemaRegistry_Writes(t *testing.T) {
	g := New(WithoutMetrics(), WithoutBackpressure(), WithSchemaRegistry(SchemaRegistryOptions{Writes: true, FirstID: 1000}))
	t.Cleanup(g.Close)
	url := srServer(t, g)
	ctx := context.Background()
	x := sr.Schema{Schema: `syntax = "proto3"; package x; message X {}`}
	for _, tc := range []struct {
		cfg  sr.Config
		code int
	}{
		{sr.Config{}, 40101},
		{sr.Config{Username: "u", Password: "wrong"}, 40101},
	} {
		_, err := srClient(t, url, tc.cfg).Register(ctx, "x-value", x, false)
		var e *sr.Error
		if !errors.As(err, &e) || e.Code != tc.code {
			t.Errorf("%+v: %v, want %d", tc.cfg, err, tc.code)
		}
	}
	for _, cfg := range []sr.Config{{Token: g.DevTokenHex()}, {Username: "u", Password: g.AdminTokenHex()}, {Token: g.AdminTokenHex()}} {
		id, err := srClient(t, url, cfg).Register(ctx, "x-value", x, false)
		if err != nil || id != 1000 {
			t.Errorf("dev or admin register: %d %v", id, err)
		}
	}
	// Compatibility levels stay admin only.
	req, _ := http.NewRequest(http.MethodPut, url+"/config", strings.NewReader(`{"compatibility":"NONE"}`))
	req.Header.Set("Authorization", "Bearer "+g.DevTokenHex())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("PUT /config as dev: %d", resp.StatusCode)
	}
}

// An app key writes the subjects of its namespaces' channels (and
// their imports), not others; an expired or revoked key is
// anonymous. Works without WithRequireAppKeys.
func TestSchemaRegistry_AppKeys(t *testing.T) {
	g := New(WithoutMetrics(), WithoutBackpressure(), WithSchemaRegistry(SchemaRegistryOptions{Writes: true}))
	t.Cleanup(g.Close)
	addOrdersProto(t, g)
	url := srServer(t, g)
	ctx := context.Background()
	anon := srClient(t, url, sr.Config{})
	waitSubject(t, anon, "EV_orders_v1_OrderEvents-value")
	ordersID, ordersKey := mintKey(t, g, time.Hour, "orders")
	_, billingKey := mintKey(t, g, time.Hour, "billing")
	orders := srClient(t, url, sr.Config{Username: "svc", Password: ordersKey})
	fd := ordersv1.File_orders_v1_orders_proto

	if _, err := orders.EnsureFile(ctx, "EV_orders_v1_OrderEvents-value", fd, sr.RegisterOptions{Register: true}); err != nil {
		t.Fatalf("app key on its channel subject: %v", err)
	}
	code := func(err error) int {
		var e *sr.Error
		if errors.As(err, &e) {
			return e.Code
		}
		return 0
	}
	x := sr.Schema{Schema: `syntax = "proto3"; package x; message X {}`}
	if _, err := orders.Register(ctx, "corp-imported-value", x, false); code(err) != 40301 {
		t.Errorf("app key on a subject of no channel of its namespaces: %v", err)
	}
	if _, err := srClient(t, url, sr.Config{Token: billingKey}).EnsureFile(ctx, "EV_orders_v1_OrderEvents-value", fd, sr.RegisterOptions{Register: true}); code(err) != 40301 {
		t.Errorf("another namespace's key: %v", err)
	}
	// The dev token adds the imported subject; the app key may not
	// delete it, the dev token may.
	if _, err := srClient(t, url, sr.Config{Token: g.DevTokenHex()}).Register(ctx, "corp-imported-value", x, false); err != nil {
		t.Fatal(err)
	}
	del := func(tok, path string) int {
		req, _ := http.NewRequest(http.MethodDelete, url+path, nil)
		req.SetBasicAuth("u", tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := del(ordersKey, "/subjects/corp-imported-value"); c != http.StatusForbidden {
		t.Errorf("app key deletes a foreign subject: %d", c)
	}
	if c := del(g.DevTokenHex(), "/subjects/corp-imported-value"); c != http.StatusOK {
		t.Errorf("dev deletes an unreferenced subject: %d", c)
	}
	// Referenced: the channel subject references options.proto.
	if c := del(ordersKey, "/subjects/gw%2Fproto%2Fps%2Fv1%2Foptions.proto/versions/1"); c != http.StatusUnprocessableEntity {
		t.Errorf("app key deletes a referenced import: %d", c)
	}
	if c := del(ordersKey, "/subjects/EV_orders_v1_OrderEvents-value"); c != http.StatusOK {
		t.Errorf("app key deletes its channel subject: %d", c)
	}
	if err := g.deleteAppKey(ctx, ordersID); err != nil {
		t.Fatal(err)
	}
	if _, err := orders.Register(ctx, "EV_orders_v1_OrderEvents-value", x, false); code(err) != 40101 {
		t.Errorf("revoked key: %v", err)
	}
}

func TestSchemaRegistry_Off(t *testing.T) {
	g := New(WithoutMetrics(), WithoutBackpressure())
	t.Cleanup(g.Close)
	_, err := srClient(t, srServer(t, g), sr.Config{}).Subjects(context.Background(), false)
	if !sr.IsNotFound(err) {
		t.Fatalf("off: %v", err)
	}
}

// Three gateways on one 3-node cluster register the same channels at
// once and race on admin registrations of distinct schemas: every
// schema gets one id, the same on every node; a restarted gateway
// serves the same ids.
func TestSchemaRegistry_Cluster(t *testing.T) {
	cls, _ := startThreeNodeNATS(t)
	opts := func(cl *Cluster) []Option {
		return []Option{WithCluster(cl), WithoutMetrics(), WithoutBackpressure(), WithSchemaRegistry(SchemaRegistryOptions{Writes: true})}
	}
	gws := make([]*Gateway, 3)
	urls := make([]string, 3)
	for i, cl := range cls {
		gws[i] = New(opts(cl)...)
		t.Cleanup(gws[i].Close)
		urls[i] = srServer(t, gws[i])
	}
	var wg sync.WaitGroup
	for _, g := range gws {
		wg.Add(1)
		go func(g *Gateway) {
			defer wg.Done()
			g.ControlPlane() // boots cluster tracking
			addOrdersProto(t, g)
		}(g)
	}
	wg.Wait()

	// Concurrent admin registrations: 3 nodes x the same 10 schemas.
	const n = 10
	ctx := context.Background()
	got := make([][]int, 3)
	for i := range gws {
		c := srClient(t, urls[i], sr.Config{Token: gws[i].AdminTokenHex()})
		waitUntilErr(t, 30*time.Second, "store ready", func() error {
			_, err := c.Subjects(ctx, false)
			return err
		})
		wg.Add(1)
		go func(i int, c *sr.Client) {
			defer wg.Done()
			for k := 0; k < n; k++ {
				id, err := c.Register(ctx, fmt.Sprintf("race-%d-value", k%3), sr.Schema{Schema: fmt.Sprintf(`syntax = "proto3"; package race; message M%d {}`, k)}, false)
				if err != nil {
					t.Errorf("node %d register %d: %v", i, k, err)
					return
				}
				got[i] = append(got[i], id)
			}
		}(i, c)
	}
	wg.Wait()
	if t.Failed() {
		return
	}
	seen := map[int]int{}
	for k := 0; k < n; k++ {
		for i := range got {
			if got[i][k] != got[0][k] {
				t.Fatalf("schema %d: node %d id %d, node 0 id %d", k, i, got[i][k], got[0][k])
			}
		}
		if prev, dup := seen[got[0][k]]; dup {
			t.Fatalf("schemas %d and %d share id %d", prev, k, got[0][k])
		}
		seen[got[0][k]] = k
	}

	// Every node serves the same subjects with the same ids.
	snapshot := func(url string) (map[string]int, error) {
		c := srClient(t, url, sr.Config{})
		subs, err := c.Subjects(ctx, false)
		if err != nil {
			return nil, err
		}
		out := map[string]int{}
		for _, s := range subs {
			vs, err := c.Versions(ctx, s, false)
			if err != nil {
				return nil, err
			}
			for _, v := range vs {
				x, err := c.Version(ctx, s, v, "", false)
				if err != nil {
					return nil, err
				}
				out[fmt.Sprintf("%s@%d", s, v)] = x.ID
			}
		}
		return out, nil
	}
	var want map[string]int
	waitUntilErr(t, 20*time.Second, "owned subjects on every node", func() error {
		var err error
		if want, err = snapshot(urls[0]); err != nil {
			return err
		}
		if _, ok := want["EV_orders_v1_OrderEvents-value@1"]; !ok {
			return fmt.Errorf("node 0 subjects %v", keys(want))
		}
		for i := 1; i < 3; i++ {
			got, err := snapshot(urls[i])
			if err != nil {
				return err
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				return fmt.Errorf("node %d %v, node 0 %v", i, got, want)
			}
		}
		return nil
	})
	if _, ok := want["EV_orders_v1_OrderEvents-value@2"]; ok {
		t.Errorf("three nodes registering one file made two versions: %v", want)
	}

	// The bucket follows the replica policy with the other buckets.
	waitStream(t, cls[0].JS, "KV_"+schemasBucketName, func(c jetstream.StreamConfig) bool { return c.Replicas == 3 })

	// Restart gateway 2 on the same NATS node: same ids.
	gws[2].Close()
	g := New(opts(cls[2])...)
	t.Cleanup(g.Close)
	g.ControlPlane()
	url := srServer(t, g)
	waitUntilErr(t, 30*time.Second, "restarted node serves the same ids", func() error {
		got, err := snapshot(url)
		if err != nil {
			return err
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			return fmt.Errorf("restarted %v, want %v", got, want)
		}
		return nil
	})
	// And a lookup on the restarted node matches the generated file.
	c := srClient(t, url, sr.Config{})
	found, err := c.EnsureFile(ctx, "EV_orders_v1_OrderEvents-value", ordersv1.File_orders_v1_orders_proto, sr.RegisterOptions{})
	if err != nil || found.ID != want["EV_orders_v1_OrderEvents-value@1"] {
		t.Fatalf("lookup after restart: %+v %v", found, err)
	}
}

func keys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// control.proto is a default subject, available before any service
// registers, under a reserved id. An import of a registry with ids from
// 1 (and a clashing owned channel registered by gwag) works beside it.
func TestSchemaRegistry_ControlPlaneSubject(t *testing.T) {
	g := New(WithoutMetrics(), WithoutBackpressure(), WithSchemaRegistry(SchemaRegistryOptions{}))
	t.Cleanup(g.Close)
	url := srServer(t, g)
	c := srClient(t, url, sr.Config{})
	got := waitSubject(t, c, ControlPlaneSubject)
	if got.ID != srserve.ReservedTop {
		t.Errorf("id %d, want the reserved %d", got.ID, srserve.ReservedTop)
	}
	if got.Schema != cpv1.ControlProto {
		t.Errorf("schema text differs from control.proto (%d vs %d bytes)", len(got.Schema), len(cpv1.ControlProto))
	}
	if got.SchemaType != "" && got.SchemaType != "PROTOBUF" {
		t.Errorf("type %q", got.SchemaType)
	}

	// The documented curl.
	resp, err := http.Get(url + "/subjects/" + ControlPlaneSubject + "/versions/latest")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET latest: %d", resp.StatusCode)
	}

	// Import of a registry whose ids start at 1.
	ctx := context.Background()
	src := srtest.New()
	defer src.Close()
	sc := srClient(t, src.URL, sr.Config{})
	for i := 0; i < 3; i++ {
		if _, err := sc.Register(ctx, fmt.Sprintf("corp-%d-value", i), sr.Schema{SchemaType: "PROTOBUF", Schema: fmt.Sprintf(`syntax = "proto3"; package corp; message C%d {}`, i)}, false); err != nil {
			t.Fatal(err)
		}
	}
	b, err := srserve.Export(ctx, sc, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(b)
	req, _ := http.NewRequest(http.MethodPost, url+"/gwag/import", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+g.AdminTokenHex())
	iresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer iresp.Body.Close()
	if iresp.StatusCode != 200 {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(iresp.Body)
		t.Fatalf("import beside the default subject: %d %s", iresp.StatusCode, buf.String())
	}
	imported, err := c.SubjectVersion(ctx, "corp-0-value", "latest", "")
	if err != nil || imported.ID != 1 {
		t.Errorf("imported id: %+v %v, want 1", imported, err)
	}
	if again, err := c.SubjectVersion(ctx, ControlPlaneSubject, "latest", ""); err != nil || again.ID != srserve.ReservedTop {
		t.Errorf("default subject after import: %+v %v", again, err)
	}
	// Owned channels still register above the imported ids.
	addOrdersProto(t, g)
	evs := waitSubject(t, c, "EV_orders_v1_OrderEvents-value")
	if evs.ID <= 3 || evs.ID >= srserve.ReservedTop-10 {
		t.Errorf("channel id %d", evs.ID)
	}
}
