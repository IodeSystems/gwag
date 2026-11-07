package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bufbuild/protocompile"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// uploadProtoSource is the .proto fixture exercising the
// `(gwag.upload.v1.upload) = true` field extension. Inline string +
// protocompile + dynamicpb avoids a generated package in testdata.
const uploadProtoSource = `syntax = "proto3";
package gwag_test.upload.v1;

import "gwag/upload/v1/options.proto";

service UploadService {
  rpc Upload(UploadRequest) returns (UploadResponse);
}

message UploadRequest {
  bytes data = 1 [(gwag.upload.v1.upload) = true];
  string filename = 2;
}

message UploadResponse {
  int64 received_bytes = 1;
  string sha_prefix = 2;
}
`

type uploadProtoFixture struct {
	reqDesc       protoreflect.MessageDescriptor
	respDesc      protoreflect.MessageDescriptor
	grpcAddr      string
	receivedBytes atomic.Int64
	receivedFirst atomic.Pointer[[]byte]
}

func newUploadProtoFixture(t *testing.T) *uploadProtoFixture {
	t.Helper()
	uploadOpts, err := os.ReadFile("proto/upload/v1/options.proto")
	if err != nil {
		t.Fatalf("read upload options proto: %v", err)
	}
	files := map[string][]byte{
		"upload.proto":                 []byte(uploadProtoSource),
		"gwag/upload/v1/options.proto": uploadOpts,
	}
	c := protocompile.Compiler{
		Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{
			Accessor: func(p string) (io.ReadCloser, error) {
				if b, ok := files[p]; ok {
					return io.NopCloser(bytes.NewReader(b)), nil
				}
				return nil, os.ErrNotExist
			},
		}),
		SourceInfoMode: protocompile.SourceInfoStandard,
	}
	out, err := c.Compile(context.Background(), "upload.proto")
	if err != nil {
		t.Fatalf("compile upload proto: %v", err)
	}
	fd := out[0]

	svc := fd.Services().Get(0)
	method := svc.Methods().Get(0)
	fix := &uploadProtoFixture{
		reqDesc:  method.Input(),
		respDesc: method.Output(),
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	grpcSrv := grpc.NewServer()
	sd := grpc.ServiceDesc{
		ServiceName: string(svc.FullName()),
		HandlerType: (*any)(nil),
		Methods: []grpc.MethodDesc{
			{
				MethodName: string(method.Name()),
				Handler: func(_ any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
					in := dynamicpb.NewMessage(fix.reqDesc)
					if err := dec(in); err != nil {
						return nil, err
					}
					data := in.Get(fix.reqDesc.Fields().ByName("data")).Bytes()
					fix.receivedBytes.Store(int64(len(data)))
					if fix.receivedFirst.Load() == nil {
						copied := append([]byte(nil), data...)
						fix.receivedFirst.Store(&copied)
					}
					resp := dynamicpb.NewMessage(fix.respDesc)
					resp.Set(fix.respDesc.Fields().ByName("received_bytes"), protoreflect.ValueOfInt64(int64(len(data))))
					prefix := ""
					if len(data) > 0 {
						prefix = string(data[:min(len(data), 8)])
					}
					resp.Set(fix.respDesc.Fields().ByName("sha_prefix"), protoreflect.ValueOfString(prefix))
					return resp, nil
				},
			},
		},
	}
	grpcSrv.RegisterService(&sd, struct{}{})
	go func() { _ = grpcSrv.Serve(lis) }()
	t.Cleanup(grpcSrv.Stop)
	fix.grpcAddr = lis.Addr().String()
	return fix
}

// registerUploadGateway wires the gateway against the fixture and
// returns it; caller registers any additional cleanups.
func registerUploadGateway(t *testing.T, fix *uploadProtoFixture, opts ...Option) *Gateway {
	t.Helper()
	uploadOpts, err := os.ReadFile("proto/upload/v1/options.proto")
	if err != nil {
		t.Fatalf("read upload options proto: %v", err)
	}
	gw := New(append([]Option{WithoutMetrics(), WithoutBackpressure()}, opts...)...)
	t.Cleanup(gw.Close)
	if err := gw.AddProtoBytes("upload.proto", []byte(uploadProtoSource),
		ProtoImports(map[string][]byte{"gwag/upload/v1/options.proto": uploadOpts}),
		To(fix.grpcAddr),
		As("uploads"),
	); err != nil {
		t.Fatalf("AddProtoBytes: %v", err)
	}
	return gw
}

// fetchSDL pulls the GraphQL SDL from the gateway's schema handler
// over HTTP — same view a codegen tool gets.
func fetchSDL(t *testing.T, gw *Gateway) string {
	t.Helper()
	srv := httptest.NewServer(gw.SchemaHandler())
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/schema/graphql")
	if err != nil {
		t.Fatalf("schema GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("schema status=%d body=%s", resp.StatusCode, body)
	}
	return string(body)
}

// TestProtoUpload_SchemaExposesUploadScalar pins that the upload
// extension on a bytes field surfaces as `Upload!` in the GraphQL
// SDL — the contract the multipart-spec parser binds against.
func TestProtoUpload_SchemaExposesUploadScalar(t *testing.T) {
	fix := newUploadProtoFixture(t)
	gw := registerUploadGateway(t, fix, WithUploadDataDir(t.TempDir()))
	sdl := fetchSDL(t, gw)
	if !strings.Contains(sdl, "scalar Upload") {
		t.Errorf("SDL missing scalar Upload; got:\n%s", sdl)
	}
	if !strings.Contains(sdl, "data: Upload") {
		t.Errorf("SDL doesn't bind data: Upload; got:\n%s", sdl)
	}
}

// TestProtoUpload_InlineMultipartRoundTrip — graphql-multipart-spec
// inline upload reaches the gRPC backend's bytes field intact.
func TestProtoUpload_InlineMultipartRoundTrip(t *testing.T) {
	fix := newUploadProtoFixture(t)
	gw := registerUploadGateway(t, fix, WithUploadDataDir(t.TempDir()))
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)

	body := []byte("payload-bytes-into-proto")
	mpBody, ct := buildGraphQLMultipart(t, graphQLMultipartCase{
		query:     `query Up($f: Upload!) { uploads { upload(data: $f, filename: "x.bin") { receivedBytes shaPrefix } } }`,
		variables: map[string]any{"f": nil},
		mapJSON:   `{"0":["variables.f"]}`,
		files: []uploadMultipartFilePart{{
			name: "0", filename: "x.bin", contentType: "application/octet-stream", body: string(body),
		}},
	})
	resp, err := http.Post(srv.URL+"/graphql", ct, mpBody)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	var out struct {
		Data struct {
			Uploads struct {
				Upload map[string]any `json:"upload"`
			} `json:"uploads"`
		} `json:"data"`
		Errors []any `json:"errors"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	if len(out.Errors) > 0 {
		t.Fatalf("graphql errors: %v", out.Errors)
	}
	got := out.Data.Uploads.Upload["receivedBytes"]
	if got != "24" && got != float64(24) {
		t.Errorf("receivedBytes=%v want 24", got)
	}
	if pref := out.Data.Uploads.Upload["shaPrefix"]; pref != "payload-" {
		t.Errorf("shaPrefix=%v want %q", pref, "payload-")
	}
	if got := fix.receivedBytes.Load(); got != int64(len(body)) {
		t.Errorf("backend received %d bytes; want %d", got, len(body))
	}
	if first := fix.receivedFirst.Load(); first == nil || !bytes.Equal(*first, body) {
		t.Errorf("backend bytes mismatch")
	}
}

// TestProtoUpload_TusStagedRoundTrip — tus upload-id resolved from the
// upload store reaches the bytes field.
func TestProtoUpload_TusStagedRoundTrip(t *testing.T) {
	fix := newUploadProtoFixture(t)
	gw := registerUploadGateway(t, fix, WithUploadDataDir(t.TempDir()))
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)
	tusSrv := httptest.NewServer(gw.UploadsTusHandler())
	t.Cleanup(tusSrv.Close)

	body := []byte("tus-staged-payload-for-proto")
	tusID := stageTusBody(t, tusSrv.URL, "tus.bin", "application/octet-stream", body)

	query := map[string]any{
		"query":     `query Up($f: Upload!) { uploads { upload(data: $f, filename: "tus.bin") { receivedBytes shaPrefix } } }`,
		"variables": map[string]any{"f": tusID},
	}
	qb, _ := json.Marshal(query)
	resp, err := http.Post(srv.URL+"/graphql", "application/json", bytes.NewReader(qb))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	var out struct {
		Data struct {
			Uploads struct {
				Upload map[string]any `json:"upload"`
			} `json:"uploads"`
		} `json:"data"`
		Errors []any `json:"errors"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	if len(out.Errors) > 0 {
		t.Fatalf("graphql errors: %v", out.Errors)
	}
	if got := fix.receivedBytes.Load(); got != int64(len(body)) {
		t.Errorf("backend received %d bytes; want %d", got, len(body))
	}
	if first := fix.receivedFirst.Load(); first == nil || !bytes.Equal(*first, body) {
		t.Errorf("backend bytes mismatch")
	}
}

// TestProtoUpload_LimitRejectsOversize — WithUploadLimit caps the
// dispatcher-side read; an inline upload over the cap surfaces as a
// rejection and the backend never sees the call.
func TestProtoUpload_LimitRejectsOversize(t *testing.T) {
	fix := newUploadProtoFixture(t)
	gw := registerUploadGateway(t, fix,
		WithUploadDataDir(t.TempDir()),
		WithUploadLimit(8),
	)
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)

	body := []byte("twenty-four-bytes-payload")
	mpBody, ct := buildGraphQLMultipart(t, graphQLMultipartCase{
		query:     `mutation Up($f: Upload!) { uploads { upload(data: $f, filename: "x.bin") { receivedBytes } } }`,
		variables: map[string]any{"f": nil},
		mapJSON:   `{"0":["variables.f"]}`,
		files: []uploadMultipartFilePart{{
			name: "0", filename: "x.bin", contentType: "application/octet-stream", body: string(body),
		}},
	})
	resp, err := http.Post(srv.URL+"/graphql", ct, mpBody)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if got := fix.receivedBytes.Load(); got != 0 {
		t.Errorf("backend received %d bytes despite cap; want 0", got)
	}
}

// stageTusBody runs the tus POST + PATCH dance and returns the
// upload-id the gateway uses to look up the staged body.
func stageTusBody(t *testing.T, baseURL, filename, contentType string, body []byte) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Upload-Length", strconv.FormatInt(int64(len(body)), 10))
	req.Header.Set("Upload-Metadata", "filename "+b64(filename)+",content-type "+b64(contentType))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST tus: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("tus POST status=%d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		t.Fatalf("tus POST returned no Location")
	}
	patchURL := loc
	if !strings.HasPrefix(loc, "http") {
		patchURL = baseURL + loc
	}
	preq, err := http.NewRequest(http.MethodPatch, patchURL, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new patch: %v", err)
	}
	preq.Header.Set("Tus-Resumable", "1.0.0")
	preq.Header.Set("Upload-Offset", "0")
	preq.Header.Set("Content-Type", "application/offset+octet-stream")
	presp, err := http.DefaultClient.Do(preq)
	if err != nil {
		t.Fatalf("PATCH tus: %v", err)
	}
	presp.Body.Close()
	if presp.StatusCode != http.StatusNoContent {
		t.Fatalf("tus PATCH status=%d", presp.StatusCode)
	}
	id := loc
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}
	return id
}
