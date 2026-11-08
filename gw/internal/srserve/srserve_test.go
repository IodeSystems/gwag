package srserve_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/iodesystems/gwag/v2/events"
	sr "github.com/iodesystems/gwag/v2/events/schemaregistry"
	"github.com/iodesystems/gwag/v2/events/schemaregistry/srtest"
	ordersv1 "github.com/iodesystems/gwag/v2/examples/events/gen/orders/v1"
	"github.com/iodesystems/gwag/v2/gw/internal/srserve"
)

const token = "admin-secret"

// Tokens: token is admin, "dev" is dev, "app" is an app key owning
// the subjects that start with "app-".
func caller(r *http.Request) srserve.Caller {
	_, pw, ok := r.BasicAuth()
	if !ok {
		pw = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	switch pw {
	case token:
		return srserve.Caller{Role: srserve.RoleAdmin}
	case "dev":
		return srserve.Caller{Role: srserve.RoleDev}
	case "app":
		return srserve.Caller{Role: srserve.RoleApp, Owns: func(s string) bool { return strings.HasPrefix(s, "app-") }}
	}
	return srserve.Caller{}
}

func serve(t *testing.T, st *srserve.Store, writes bool) *httptest.Server {
	t.Helper()
	h := srserve.NewHandler(func() *srserve.Store { return st }, srserve.HandlerOptions{Writes: writes, Caller: caller})
	mux := http.NewServeMux()
	mux.Handle("/sr/", http.StripPrefix("/sr", h))
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func client(t *testing.T, url string, cfg sr.Config) *sr.Client {
	t.Helper()
	cfg.URL = url
	c, err := sr.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func code(err error) int {
	var e *sr.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return 0
}

// Registering through the REST API with the Go serde, then reading
// back: ids, versions, lookups, both formats, the read-only mode and
// the auth rules.
func TestRegisterAndRead(t *testing.T) {
	ctx := context.Background()
	st := srserve.NewStore(srserve.NewMemKV(), 100)
	s := serve(t, st, true)
	admin := client(t, s.URL+"/sr", sr.Config{Username: "any", Password: token})
	anon := client(t, s.URL+"/sr", sr.Config{})
	other := client(t, s.URL+"/sr", sr.Config{Token: "app"})

	fd := ordersv1.File_orders_v1_orders_proto
	if _, err := anon.EnsureFile(ctx, "orders-value", fd, sr.RegisterOptions{Register: true}); code(err) != 40101 {
		t.Fatalf("anonymous register: %v", err)
	}
	if _, err := other.EnsureFile(ctx, "orders-value", fd, sr.RegisterOptions{Register: true}); code(err) != 40301 {
		t.Fatalf("app key register of a subject it does not own: %v", err)
	}
	got, err := admin.EnsureFile(ctx, "orders-value", fd, sr.RegisterOptions{Register: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID < 100 || got.Version != 1 {
		t.Fatalf("registered %+v; want id >= 100 (first id), version 1", got)
	}
	// Again: same id and version.
	again, err := client(t, s.URL+"/sr", sr.Config{Username: "x", Password: token}).EnsureFile(ctx, "orders-value", fd, sr.RegisterOptions{Register: true})
	if err != nil || again.ID != got.ID || again.Version != 1 {
		t.Fatalf("second register %+v %v", again, err)
	}
	// Lookup-only from an anonymous reader finds it.
	found, err := anon.EnsureFile(ctx, "orders-value", fd, sr.RegisterOptions{})
	if err != nil || found.ID != got.ID {
		t.Fatalf("lookup %+v %v", found, err)
	}
	subs, err := anon.Subjects(ctx, false)
	if err != nil || len(subs) != 2 || subs[0] != "gw/proto/ps/v1/options.proto" || subs[1] != "orders-value" {
		t.Fatalf("subjects %v %v", subs, err)
	}
	// Default format is .proto text, serialized is what was registered.
	want, _ := sr.SchemaOf(fd, []sr.Reference{{Name: "gw/proto/ps/v1/options.proto", Subject: "gw/proto/ps/v1/options.proto", Version: 1}})
	ser, err := anon.SchemaByID(ctx, got.ID, "serialized")
	if err != nil || ser.Schema != want.Schema {
		t.Fatalf("serialized form differs from what was registered: %v", err)
	}
	txt, err := anon.SchemaByID(ctx, got.ID, "")
	if err != nil || !strings.Contains(txt.Schema, "message OrderCreated") {
		t.Fatalf("text form: %v %q", err, txt.Schema)
	}
	// The Go client compiles the text form with its references.
	if _, err := anon.FileByID(ctx, got.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := anon.SchemaByID(ctx, 99999, ""); code(err) != 40403 {
		t.Errorf("missing id: %v", err)
	}
	if _, err := anon.SubjectVersion(ctx, "nope", "latest", ""); code(err) != 40401 {
		t.Errorf("missing subject: %v", err)
	}
	if _, err := anon.SubjectVersion(ctx, "orders-value", "7", ""); code(err) != 40402 {
		t.Errorf("missing version: %v", err)
	}
	// A serde in lookup mode encodes, another decodes.
	serde, err := sr.NewSerde(anon)
	if err != nil {
		t.Fatal(err)
	}
	var m events.Message
	if err := serde.Encode(ctx, events.Channel{}, "orders", &ordersv1.OrderCreated{OrderId: "o-1"}, &m); err != nil {
		t.Fatal(err)
	}
	m.Headers = map[string]string{}
	if err := serde.Decode(ctx, events.Channel{}, "orders", &m); err != nil || m.Headers[events.HeaderType] != "orders.v1.OrderCreated" {
		t.Fatalf("decode: %v %v", err, m.Headers)
	}
	oc := &ordersv1.OrderCreated{}
	if err := proto.Unmarshal(m.Data, oc); err != nil || oc.OrderId != "o-1" {
		t.Fatalf("body %v %v", oc, err)
	}
}

func TestReadOnly(t *testing.T) {
	ctx := context.Background()
	s := serve(t, srserve.NewStore(srserve.NewMemKV(), 1), false)
	admin := client(t, s.URL+"/sr", sr.Config{Username: "u", Password: token})
	_, err := admin.EnsureFile(ctx, "orders-value", ordersv1.File_orders_v1_orders_proto, sr.RegisterOptions{Register: true})
	if code(err) != 42205 {
		t.Fatalf("register on READONLY: %v", err)
	}
	resp, err := http.Get(s.URL + "/sr/mode")
	if err != nil {
		t.Fatal(err)
	}
	var mode map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&mode)
	resp.Body.Close()
	if mode["mode"] != "READONLY" || resp.Header.Get("Content-Type") != srserve.ContentType {
		t.Fatalf("mode %v %s", mode, resp.Header.Get("Content-Type"))
	}
}

// bundleFrom copies src the way `gwag schema import` does.
func bundleFrom(t *testing.T, src *sr.Client) srserve.Bundle {
	t.Helper()
	ctx := context.Background()
	b, err := srserve.Export(ctx, src, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func postImport(t *testing.T, url string, b srserve.Bundle, tok string) (int, srserve.ImportResult, string) {
	t.Helper()
	body, _ := json.Marshal(b)
	req, _ := http.NewRequest(http.MethodPost, url+"/sr/gwag/import", bytes.NewReader(body))
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	var res srserve.ImportResult
	_ = json.Unmarshal(buf.Bytes(), &res)
	return resp.StatusCode, res, buf.String()
}

// Import keeps ids and versions; a record framed by the source decodes
// against gwag; re-import is a no-op; a conflicting id fails without
// writing; new ids go above the imported ones.
func TestImportPreservesIDs(t *testing.T) {
	ctx := context.Background()
	src := srtest.New()
	defer src.Close()
	sc := client(t, src.URL, sr.Config{})
	// Burn ids so the source's ids are not 1, 2.
	for i := 0; i < 5; i++ {
		_, _ = sc.Register(ctx, "filler-value", sr.Schema{Schema: `syntax = "proto3"; package f; message F` + string(rune('a'+i)) + ` {}`, SchemaType: "PROTOBUF"}, false)
	}
	srcSerde, _ := sr.NewSerde(sc, sr.AutoRegister())
	var rec events.Message
	if err := srcSerde.Encode(ctx, events.Channel{}, "corp-orders", &ordersv1.OrderCreated{OrderId: "o-9", Customer: "cy"}, &rec); err != nil {
		t.Fatal(err)
	}
	srcID, _, _, _ := sr.Unframe(rec.Data)

	st := srserve.NewStore(srserve.NewMemKV(), 1)
	s := serve(t, st, false)
	b := bundleFrom(t, sc)
	if code, _, body := postImport(t, s.URL, b, ""); code != 401 {
		t.Fatalf("anonymous import: %d %s", code, body)
	}
	status, res, body := postImport(t, s.URL, b, token)
	if status != 200 || res.SchemasAdded != len(b.Schemas) || len(res.Warnings) > 0 {
		t.Fatalf("import: %d %s", status, body)
	}
	status, res, body = postImport(t, s.URL, b, token)
	if status != 200 || res.SchemasAdded != 0 || res.VersionsAdded != 0 {
		t.Fatalf("re-import: %d %s", status, body)
	}

	gc := client(t, s.URL+"/sr", sr.Config{})
	got, err := gc.SubjectVersion(ctx, "corp-orders-value", "latest", "")
	if err != nil || got.ID != srcID {
		t.Fatalf("imported subject: %+v %v (source id %d)", got, err, srcID)
	}
	// The source's record decodes against gwag.
	dec, _ := sr.NewSerde(gc)
	rec.Headers = map[string]string{}
	if err := dec.Decode(ctx, events.Channel{}, "corp-orders", &rec); err != nil || rec.Headers[events.HeaderType] != "orders.v1.OrderCreated" {
		t.Fatalf("decode: %v %v", err, rec.Headers)
	}
	// A lookup-only serde finds the imported schema by content.
	look, _ := sr.NewSerde(gc)
	var m events.Message
	if err := look.Encode(ctx, events.Channel{}, "corp-orders", &ordersv1.OrderCreated{OrderId: "o-10"}, &m); err != nil {
		t.Fatal(err)
	}
	if id, _, _, _ := sr.Unframe(m.Data); id != srcID {
		t.Fatalf("lookup framed with %d, want %d", id, srcID)
	}

	// Conflict: the same id with another schema.
	bad := b
	bad.Schemas = append([]srserve.ImportSchema(nil), b.Schemas...)
	bad.Schemas[0].Schema = `syntax = "proto3"; package other; message X {}`
	bad.Schemas[0].Serialized = ""
	status, _, body = postImport(t, s.URL, bad, token)
	if status != 409 || !strings.Contains(body, "40901") {
		t.Fatalf("conflict: %d %s", status, body)
	}

	// A new registration gets an id above the imported ones.
	maxID := 0
	for _, x := range b.Schemas {
		maxID = max(maxID, x.ID)
	}
	v, err := st.Register(ctx, "new-value", sr.Schema{Schema: `syntax = "proto3"; package n; message N {}`, SchemaType: "PROTOBUF"})
	if err != nil || v.ID <= maxID {
		t.Fatalf("new id %d %v; imported max %d", v.ID, err, maxID)
	}
}

// Concurrent registrations of the same schemas: one id per schema,
// versions without gaps or duplicates.
func TestConcurrentRegister(t *testing.T) {
	ctx := context.Background()
	st := srserve.NewStore(srserve.NewMemKV(), 1)
	schemas := []string{
		`syntax = "proto3"; package c; message A {}`,
		`syntax = "proto3"; package c; message B {}`,
		`syntax = "proto3"; package c; message C {}`,
	}
	var wg sync.WaitGroup
	ids := make([][]int, 8)
	for w := range ids {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for _, s := range schemas {
				v, err := st.Register(ctx, "c-value", sr.Schema{Schema: s, SchemaType: "PROTOBUF"})
				if err != nil {
					t.Error(err)
					return
				}
				ids[w] = append(ids[w], v.ID)
			}
		}(w)
	}
	wg.Wait()
	for w := range ids {
		for i := range ids[w] {
			if ids[w][i] != ids[0][i] {
				t.Fatalf("worker %d got ids %v, worker 0 %v", w, ids[w], ids[0])
			}
		}
	}
	vs, err := st.Versions(ctx, "c-value", false)
	if err != nil || len(vs) != 3 {
		t.Fatalf("versions %v %v", vs, err)
	}
}

// Two imported versions hold one schema in two forms: a lookup gets the
// version with identical text, else the oldest equivalent one.
func TestLookupPrefersIdenticalThenOldest(t *testing.T) {
	ctx := context.Background()
	st := srserve.NewStore(srserve.NewMemKV(), 1)
	text := `syntax = "proto3"; package l; message M { string a = 1; }`
	spaced := `syntax = "proto3";  package l;  message M { string a = 1; }`
	other := `syntax = "proto3"; package l; message M { string a = 1; }` + "\n"
	_, err := st.Import(ctx, srserve.Bundle{
		Schemas:  []srserve.ImportSchema{{ID: 7, SchemaType: "PROTOBUF", Schema: text}, {ID: 8, SchemaType: "PROTOBUF", Schema: spaced}},
		Versions: []srserve.ImportVersion{{Subject: "l", Version: 1, ID: 7}, {Subject: "l", Version: 2, ID: 8}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]int{text: 1, spaced: 2, other: 1} {
		v, _, err := st.Lookup(ctx, "l", sr.Schema{Schema: in, SchemaType: "PROTOBUF"}, false)
		if err != nil || v.Version != want {
			t.Errorf("lookup %q: version %d %v, want %d", in, v.Version, err, want)
		}
	}
}

func TestCompatibility(t *testing.T) {
	ctx := context.Background()
	st := srserve.NewStore(srserve.NewMemKV(), 1)
	s := serve(t, st, false)
	c := client(t, s.URL+"/sr", sr.Config{})
	v1 := `syntax = "proto3"; package p; message M { string a = 1; int64 b = 2; } message Gone {}`
	if _, err := st.Register(ctx, "p-value", sr.Schema{Schema: v1, SchemaType: "PROTOBUF"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		schema string
		ok     bool
		msg    string
	}{
		{`syntax = "proto3"; package p; message M { string a = 1; int64 b = 2; string c = 3; } message Gone {}`, true, ""},
		{`syntax = "proto3"; package p; message M { string a = 1; } message Gone {}`, true, ""},
		{`syntax = "proto3"; package p; message M { string a = 1; string b = 2; } message Gone {}`, false, "field 2 is string, was int64"},
		{`syntax = "proto3"; package p; message M { string a = 1; int64 b = 2; }`, false, "message p.Gone removed"},
	} {
		res, err := c.CheckCompatibility(ctx, "p-value", "latest", sr.Schema{Schema: tc.schema})
		if err != nil {
			t.Fatal(err)
		}
		if res.IsCompatible != tc.ok || (tc.msg != "" && !strings.Contains(strings.Join(res.Messages, "\n"), tc.msg)) {
			t.Errorf("%s: %+v", tc.schema, res)
		}
	}
	if err := st.SetConfig(ctx, "p-value", "NONE"); err != nil {
		t.Fatal(err)
	}
	res, err := c.CheckCompatibility(ctx, "p-value", "latest", sr.Schema{Schema: `syntax = "proto3"; package p; message M { bytes a = 1; }`})
	if err != nil || !res.IsCompatible {
		t.Errorf("NONE: %+v %v", res, err)
	}
	if l, err := c.CompatibilityLevel(ctx, "p-value"); err != nil || l != "NONE" {
		t.Errorf("level %q %v", l, err)
	}
	if l, err := c.Config(ctx, "other"); err != nil || l != "" {
		t.Errorf("unset subject level %q %v", l, err)
	}
}
