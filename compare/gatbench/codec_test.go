package gatbench_test

// The gRPC surface has more than one caller, and they don't cost the
// same. connect-go serves three wire shapes off one handler, and which
// one a client speaks changes how much serialization work the request
// does before it reaches the handler:
//
//	Connect JSON   application/json            curl, connect-web with the JSON codec
//	Connect proto  application/proto           connect-go clients (their default)
//	gRPC-Web       application/grpc-web+proto  browsers via grpc-web
//
// Reporting a single "gRPC" number would average over callers whose
// costs differ, so each gets its own row for both gat and the
// hand-written connect-go service.
//
// gRPC proper (application/grpc+proto) is deliberately absent: it
// requires HTTP/2, which an httptest.ResponseRecorder cannot provide,
// and standing up an h2c server would add a loopback round trip that
// dwarfs the codec difference being measured. It shares the binary
// codec path with the Connect-proto rows, plus HTTP/2 framing.

import (
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	gatbenchv1 "github.com/iodesystems/gwag/compare/gatbench/connectimpl/gen/gatbench/v1"
	"github.com/iodesystems/gwag/compare/gatbench/connectimpl"
)

// codecReq builds a unary POST for a given content type. Connect's
// unary protocol puts the bare message in the body — no envelope.
func codecReq(procedure, contentType string, body []byte) *http.Request {
	req := httptest.NewRequest(http.MethodPost, procedure, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Connect-Protocol-Version", "1")
	return req
}

// grpcWebReq wraps the message in gRPC-Web's 5-byte frame header: one
// compression flag byte, then a big-endian uint32 length.
func grpcWebReq(procedure string, body []byte) *http.Request {
	framed := make([]byte, 5+len(body))
	framed[0] = 0
	binary.BigEndian.PutUint32(framed[1:5], uint32(len(body)))
	copy(framed[5:], body)

	req := httptest.NewRequest(http.MethodPost, procedure, strings.NewReader(string(framed)))
	req.Header.Set("Content-Type", "application/grpc-web+proto")
	return req
}

// serveCodecOK drives one request and fails on a non-2xx. gRPC-Web and
// gRPC report application errors in trailers with a 200 status, so the
// body is checked for a non-zero grpc-status too — otherwise a failing
// handler would benchmark as a fast success.
func serveCodecOK(tb testing.TB, h http.Handler, req *http.Request) {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code < 200 || rec.Code >= 300 {
		tb.Fatalf("%s: %d %s", req.URL.Path, rec.Code, rec.Body.String())
	}
	if s := rec.Header().Get("Grpc-Status"); s != "" && s != "0" {
		tb.Fatalf("%s: grpc-status %s %s", req.URL.Path, s, rec.Header().Get("Grpc-Message"))
	}
	if body := rec.Body.String(); strings.Contains(body, "grpc-status: ") &&
		!strings.Contains(body, "grpc-status: 0") {
		tb.Fatalf("%s: trailer reports failure: %q", req.URL.Path, body)
	}
}

func runCodec(b *testing.B, h http.Handler, next func() *http.Request) {
	serveCodecOK(b, h, next())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		serveCodecOK(b, h, next())
	}
}

// nativeRequestBytes marshals a request for the hand-written service
// using its generated types.
func nativeRequestBytes(tb testing.TB, msg proto.Message) []byte {
	tb.Helper()
	out, err := proto.Marshal(msg)
	if err != nil {
		tb.Fatalf("marshal: %v", err)
	}
	return out
}

// --- gat: one handler, three callers ------------------------------------

func BenchmarkList_GatConnectProto(b *testing.B) {
	h := gatMux(b)
	proc, md := gatMethod(b, h, "listProjects")
	body := gatRequestBytes(b, md, map[string]any{"limit": 25})
	runCodec(b, h, func() *http.Request {
		return codecReq(proc, "application/proto", body)
	})
}

func BenchmarkList_GatGRPCWeb(b *testing.B) {
	h := gatMux(b)
	proc, md := gatMethod(b, h, "listProjects")
	body := gatRequestBytes(b, md, map[string]any{"limit": 25})
	runCodec(b, h, func() *http.Request {
		return grpcWebReq(proc, body)
	})
}

// --- connect-go: the same three callers ---------------------------------

func BenchmarkList_ConnectGoProto(b *testing.B) {
	h := connectimpl.NewConnect()
	body := nativeRequestBytes(b, &gatbenchv1.ListProjectsRequest{Limit: 25})
	runCodec(b, h, func() *http.Request {
		return codecReq(connectimpl.ListProcedure, "application/proto", body)
	})
}

func BenchmarkList_ConnectGoGRPCWeb(b *testing.B) {
	h := connectimpl.NewConnect()
	body := nativeRequestBytes(b, &gatbenchv1.ListProjectsRequest{Limit: 25})
	runCodec(b, h, func() *http.Request {
		return grpcWebReq(connectimpl.ListProcedure, body)
	})
}

// --- point-lookup counterparts ------------------------------------------

func BenchmarkGet_GatConnectProto(b *testing.B) {
	h := gatMux(b)
	proc, md := gatMethod(b, h, "getProject")
	body := gatRequestBytes(b, md, map[string]any{"id": "p1"})
	runCodec(b, h, func() *http.Request {
		return codecReq(proc, "application/proto", body)
	})
}

func BenchmarkGet_GatGRPCWeb(b *testing.B) {
	h := gatMux(b)
	proc, md := gatMethod(b, h, "getProject")
	body := gatRequestBytes(b, md, map[string]any{"id": "p1"})
	runCodec(b, h, func() *http.Request {
		return grpcWebReq(proc, body)
	})
}

func BenchmarkGet_ConnectGoProto(b *testing.B) {
	h := connectimpl.NewConnect()
	body := nativeRequestBytes(b, &gatbenchv1.GetProjectRequest{Id: "p1"})
	runCodec(b, h, func() *http.Request {
		return codecReq(connectimpl.GetProcedure, "application/proto", body)
	})
}

func BenchmarkGet_ConnectGoGRPCWeb(b *testing.B) {
	h := connectimpl.NewConnect()
	body := nativeRequestBytes(b, &gatbenchv1.GetProjectRequest{Id: "p1"})
	runCodec(b, h, func() *http.Request {
		return grpcWebReq(connectimpl.GetProcedure, body)
	})
}
