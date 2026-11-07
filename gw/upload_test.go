package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestUploadScalar_RejectsLiteral verifies the Upload scalar refuses
// inline literals — clients must supply files via multipart, never as
// `mutation { upload(file: "literal") }`.
func TestUploadScalar_RejectsLiteral(t *testing.T) {
	s := UploadScalar()
	if got := s.ParseLiteral(nil); got != nil {
		t.Fatalf("ParseLiteral(nil) = %v; want nil", got)
	}
}

// TestUploadScalar_ParseValueIdentityForUpload pins the multipart →
// resolver hand-off: ParseValue is identity for *Upload (inline
// uploads). Non-string non-*Upload values are a hard error so a
// misconfigured client doesn't silently drop the file.
func TestUploadScalar_ParseValueIdentityForUpload(t *testing.T) {
	s := UploadScalar()
	up := &Upload{Filename: "f.txt"}
	if got := s.ParseValue(up); got != up {
		t.Fatalf("ParseValue(*Upload) = %v; want identity", got)
	}
	if err, ok := s.ParseValue(42).(error); !ok || err == nil {
		t.Fatalf("ParseValue(int) should error, got %v", s.ParseValue(42))
	}
}

// TestUploadScalar_ParseValueAcceptsTusID — a string variable value is
// the tus upload-id form: client uploaded via the tus endpoint, then
// references the id in a GraphQL mutation variable. ParseValue wraps
// it in a *Upload{TusID:…}; the dispatcher opens the body at dispatch
// time via Open(ctx, store).
func TestUploadScalar_ParseValueAcceptsTusID(t *testing.T) {
	s := UploadScalar()
	got, ok := s.ParseValue("abc123").(*Upload)
	if !ok {
		t.Fatalf("ParseValue(string) = %T; want *Upload", s.ParseValue("abc123"))
	}
	if got.TusID != "abc123" {
		t.Errorf("TusID = %q; want abc123", got.TusID)
	}
	if got.File != nil {
		t.Errorf("File set unexpectedly on tus form (lazy-open should defer)")
	}
	// Empty string is rejected so misconfigured clients don't slip
	// through with an effectively-null reference.
	if _, ok := s.ParseValue("").(error); !ok {
		t.Errorf("ParseValue(\"\") should error")
	}
}

// TestUpload_OpenInline returns the captured File without consulting
// the store.
func TestUpload_OpenInline(t *testing.T) {
	rc := &nopReadCloser{Reader: strings.NewReader("hello")}
	up := &Upload{File: rc}
	got, err := up.Open(context.Background(), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got != rc {
		t.Errorf("Open returned %v; want the captured File", got)
	}
}

// TestUpload_OpenTusBackfillsMeta — the *Upload from ParseValue has
// only TusID. Open(ctx, store) materialises File and backfills
// Filename / ContentType / Size from the staged record so the
// dispatcher sees full metadata.
func TestUpload_OpenTusBackfillsMeta(t *testing.T) {
	store := newTestStore(t)
	defer store.Close()
	ctx := context.Background()
	id, err := store.Create(ctx, UploadMeta{
		Filename:    "doc.pdf",
		ContentType: "application/pdf",
		Length:      5,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := store.Append(ctx, id, 0, strings.NewReader("hello"), true); err != nil {
		t.Fatalf("Append: %v", err)
	}

	up := &Upload{TusID: id}
	rc, err := up.Open(ctx, store)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	body, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(body) != "hello" {
		t.Errorf("body = %q; want hello", body)
	}
	if up.Filename != "doc.pdf" {
		t.Errorf("Filename = %q; want doc.pdf", up.Filename)
	}
	if up.ContentType != "application/pdf" {
		t.Errorf("ContentType = %q; want application/pdf", up.ContentType)
	}
	if up.Size != 5 {
		t.Errorf("Size = %d; want 5", up.Size)
	}
}

// TestUpload_OpenTusNoStoreErrors — opening a tus-staged Upload with
// no store configured is a clear configuration error, not a panic or
// silent nil reader.
func TestUpload_OpenTusNoStoreErrors(t *testing.T) {
	up := &Upload{TusID: "abc"}
	_, err := up.Open(context.Background(), nil)
	if err == nil {
		t.Fatalf("Open with nil store: err = nil; want config error")
	}
	if !strings.Contains(err.Error(), "no UploadStore configured") {
		t.Errorf("err = %v; want mention of missing UploadStore", err)
	}
}

// nopReadCloser turns an io.Reader into an io.ReadCloser whose Close
// is a no-op. Used only by Upload tests where the body is an in-memory
// string.
type nopReadCloser struct{ io.Reader }

func (nopReadCloser) Close() error { return nil }

// TestSchemaSDL_ContainsUploadScalar pins that the Upload scalar is
// discoverable in SDL even when no ingested field references it yet
// (chunk 3 adds field-level binding). Clients declare
// `scalar Upload` in their codegen against the gateway's SDL, so its
// absence breaks the codegen flow.
func TestSchemaSDL_ContainsUploadScalar(t *testing.T) {
	gw := newSchemaTestGateway(t)
	if err := gw.AddProtoBytes("greeter.proto", testProtoBytes(t, "greeter.proto"),
		To(nopGRPCConn{}),
		As("greeter"),
	); err != nil {
		t.Fatalf("AddProtoBytes: %v", err)
	}
	srv := httptest.NewServer(gw.SchemaHandler())
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET schema: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	sdl := string(body)
	if !strings.Contains(sdl, "scalar Upload") {
		t.Errorf("SDL missing `scalar Upload`:\n%s", sdl)
	}
}

// TestParseGraphqlMultipart_HappyPath covers the spec wire shape: a
// single file at a single variable path. Verifies the file body, the
// filename, and that the variable slot now holds a *Upload.
func TestParseGraphqlMultipart_HappyPath(t *testing.T) {
	body, ct := buildMultipartUpload(t, multipartCase{
		operations: `{"query":"mutation($f: Upload!){ upload(file: $f) }","variables":{"f":null}}`,
		mapJSON:    `{"0":["variables.f"]}`,
		files:      []multipartFile{{name: "0", filename: "hello.txt", body: "hello-bytes"}},
	})
	r := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body))
	r.Header.Set("Content-Type", ct)

	opts, err := parseGraphqlRequest(r)
	if err != nil {
		t.Fatalf("parseGraphqlRequest: %v", err)
	}
	if !strings.Contains(opts.Query, "mutation") {
		t.Fatalf("Query not preserved: %q", opts.Query)
	}
	up, ok := opts.Variables["f"].(*Upload)
	if !ok {
		t.Fatalf("variables[f] = %T; want *Upload", opts.Variables["f"])
	}
	if up.Filename != "hello.txt" {
		t.Errorf("filename = %q; want hello.txt", up.Filename)
	}
	buf, _ := io.ReadAll(up.File)
	if string(buf) != "hello-bytes" {
		t.Errorf("body = %q; want hello-bytes", string(buf))
	}
	_ = up.File.Close()
}

// TestParseGraphqlMultipart_NestedListPath substitutes into
// `variables.files.1` — the spec's array-index path form.
func TestParseGraphqlMultipart_NestedListPath(t *testing.T) {
	body, ct := buildMultipartUpload(t, multipartCase{
		operations: `{"query":"mutation($fs:[Upload!]!){ uploadMany(files: $fs) }","variables":{"fs":[null,null]}}`,
		mapJSON:    `{"a":["variables.fs.0"],"b":["variables.fs.1"]}`,
		files: []multipartFile{
			{name: "a", filename: "first.txt", body: "first"},
			{name: "b", filename: "second.txt", body: "second"},
		},
	})
	r := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body))
	r.Header.Set("Content-Type", ct)

	opts, err := parseGraphqlRequest(r)
	if err != nil {
		t.Fatalf("parseGraphqlRequest: %v", err)
	}
	fs, ok := opts.Variables["fs"].([]any)
	if !ok || len(fs) != 2 {
		t.Fatalf("variables[fs] = %T (%v); want []any len 2", opts.Variables["fs"], opts.Variables["fs"])
	}
	for i, want := range []string{"first", "second"} {
		up, ok := fs[i].(*Upload)
		if !ok {
			t.Fatalf("fs[%d] = %T; want *Upload", i, fs[i])
		}
		buf, _ := io.ReadAll(up.File)
		if string(buf) != want {
			t.Errorf("fs[%d] body = %q; want %q", i, string(buf), want)
		}
		_ = up.File.Close()
	}
}

// TestParseGraphqlMultipart_BatchedRejected pins that the array form
// of `operations` rejects with a clear error, matching the explicit
// scope of chunk 1.
func TestParseGraphqlMultipart_BatchedRejected(t *testing.T) {
	body, ct := buildMultipartUpload(t, multipartCase{
		operations: `[{"query":"mutation { ok }"}]`,
		mapJSON:    `{}`,
	})
	r := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body))
	r.Header.Set("Content-Type", ct)

	_, err := parseGraphqlRequest(r)
	if err == nil || !strings.Contains(err.Error(), "batched") {
		t.Fatalf("err = %v; want batched-operations rejection", err)
	}
}

// TestParseGraphqlMultipart_MissingFileForMap rejects requests that
// declare a map entry without the corresponding file part.
func TestParseGraphqlMultipart_MissingFileForMap(t *testing.T) {
	body, ct := buildMultipartUpload(t, multipartCase{
		operations: `{"query":"mutation($f:Upload!){ upload(file:$f) }","variables":{"f":null}}`,
		mapJSON:    `{"0":["variables.f"]}`,
		// No file parts.
	})
	r := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body))
	r.Header.Set("Content-Type", ct)

	_, err := parseGraphqlRequest(r)
	if err == nil || !strings.Contains(err.Error(), "file part") {
		t.Fatalf("err = %v; want missing-file rejection", err)
	}
}

// TestServeGraphQLJSON_MultipartParseError surfaces parser errors as
// a 400 response with a GraphQL errors envelope, not a "no operation"
// fall-through.
func TestServeGraphQLJSON_MultipartParseError(t *testing.T) {
	gw := newSchemaTestGateway(t)

	// Invalid: missing `operations` form part.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("map", "{}"); err != nil {
		t.Fatalf("WriteField: %v", err)
	}
	_ = mw.Close()

	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL, mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", resp.StatusCode)
	}
	var env struct {
		Errors []map[string]any `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Errors) == 0 {
		t.Fatalf("response had no errors envelope: %+v", env)
	}
	msg, _ := env.Errors[0]["message"].(string)
	if !strings.Contains(msg, "operations") {
		t.Errorf("error message = %q; want mention of `operations`", msg)
	}
}

// TestUploadScalar_SerializeReturnsNil pins the input-only contract:
// even if a resolver accidentally returns an *Upload, the wire output
// is null rather than something unparseable.
func TestUploadScalar_SerializeReturnsNil(t *testing.T) {
	s := UploadScalar()
	if got := s.Serialize(&Upload{Filename: "x"}); got != nil {
		t.Fatalf("Serialize = %v; want nil", got)
	}
}

// --- helpers ---------------------------------------------------------

type multipartFile struct {
	name     string
	filename string
	body     string
}

type multipartCase struct {
	operations string
	mapJSON    string
	files      []multipartFile
}

func buildMultipartUpload(t *testing.T, c multipartCase) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("operations", c.operations); err != nil {
		t.Fatalf("WriteField operations: %v", err)
	}
	if err := mw.WriteField("map", c.mapJSON); err != nil {
		t.Fatalf("WriteField map: %v", err)
	}
	for _, f := range c.files {
		fw, err := mw.CreateFormFile(f.name, f.filename)
		if err != nil {
			t.Fatalf("CreateFormFile: %v", err)
		}
		if _, err := io.WriteString(fw, f.body); err != nil {
			t.Fatalf("WriteFile body: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return buf.Bytes(), mw.FormDataContentType()
}

