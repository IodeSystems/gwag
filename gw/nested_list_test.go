package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/dynamicpb"
)

const nestedListSpec = `{
  "openapi": "3.0.3",
  "info": {"title": "geo", "version": "1.0.0"},
  "paths": {"/regions": {"post": {
    "operationId": "putRegion",
    "requestBody": {"required": true, "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Region"}}}},
    "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Region"}}}}}
  }}},
  "components": {"schemas": {
    "Point": {"type": "object", "required": ["lon", "lat"], "properties": {"lon": {"type": "number"}, "lat": {"type": "number"}}},
    "Region": {"type": "object", "required": ["points", "rings"], "properties": {
      "points": {"type": "array", "items": {"type": "array", "items": {"type": "number"}}},
      "rings":  {"type": "array", "items": {"type": "array", "items": {"$ref": "#/components/schemas/Point"}}}
    }}
  }}
}`

// Nested lists go upstream as nested JSON arrays, and a nested list of
// objects projects the selection (and __typename) on each element.
func TestOpenAPI_NestedList(t *testing.T) {
	var sent atomic.Pointer[[]byte]
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sent.Store(&body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"points":[[1,2],[3,4]],"rings":[[{"lon":1,"lat":2,"extra":true}]]}`))
	}))
	t.Cleanup(backend.Close)
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test-token")))
	t.Cleanup(gw.Close)
	if err := gw.AddOpenAPIBytes([]byte(nestedListSpec), To(backend.URL), As("geo")); err != nil {
		t.Fatalf("AddOpenAPIBytes: %v", err)
	}

	query, _ := json.Marshal(map[string]any{
		"query": `mutation { geo { putRegion(body: {points: [[1, 2], [3, 4]], rings: [[{lon: 1, lat: 2}]]}) { points rings { __typename lon } } } }`,
	})
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(string(query)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	gw.Handler().ServeHTTP(rr, req)
	want := `{"data":{"geo":{"putRegion":{"points":[[1,2],[3,4]],"rings":[[{"__typename":"geo_Point","lon":1}]]}}}}`
	if got := strings.TrimSpace(rr.Body.String()); got != want {
		t.Errorf("response = %s\nwant       %s", got, want)
	}
	body := sent.Load()
	if body == nil {
		t.Fatal("backend not called")
	}
	var up map[string]any
	if err := json.Unmarshal(*body, &up); err != nil {
		t.Fatalf("upstream body %s: %v", *body, err)
	}
	if b, _ := json.Marshal(up["points"]); string(b) != `[[1,2],[3,4]]` {
		t.Errorf("upstream points = %s", b)
	}
}

// Over gRPC each inner list travels as a PointsRow box ({value: [...]}).
// The gateway unboxes it for the upstream and boxes the response.
func TestGRPCIngress_OpenAPINestedList(t *testing.T) {
	var sent atomic.Pointer[[]byte]
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sent.Store(&body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"points":[[1,2],[3,4]],"rings":[[{"lon":1,"lat":2}]]}`))
	}))
	t.Cleanup(backend.Close)
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test-token")))
	t.Cleanup(gw.Close)
	if err := gw.AddOpenAPIBytes([]byte(nestedListSpec), To(backend.URL), As("geo")); err != nil {
		t.Fatalf("AddOpenAPIBytes: %v", err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.UnknownServiceHandler(gw.GRPCUnknownHandler()))
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	inDesc, outDesc := (&crossFormatFixture{gw: gw}).synthMethodDescriptors(t, "geo", "v1", "putRegion")
	sd := inDesc.ParentFile().Services().Get(0)

	in := dynamicpb.NewMessage(inDesc)
	if err := protojson.Unmarshal([]byte(`{"body":{"points":[{"value":[1,2]},{"value":[3,4]}],"rings":[{"value":[{"lon":1,"lat":2}]}]}}`), in); err != nil {
		t.Fatalf("build request: %v", err)
	}
	out := dynamicpb.NewMessage(outDesc)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Invoke(ctx, "/"+string(sd.FullName())+"/putRegion", in, out); err != nil {
		t.Fatalf("invoke: %v", err)
	}
	body := sent.Load()
	if body == nil {
		t.Fatal("backend not called")
	}
	var up, exp any
	_ = json.Unmarshal(*body, &up)
	_ = json.Unmarshal([]byte(`{"points":[[1,2],[3,4]],"rings":[[{"lon":1,"lat":2}]]}`), &exp)
	if !reflect.DeepEqual(up, exp) {
		t.Errorf("upstream body = %s", *body)
	}
	b, _ := protojson.Marshal(out)
	var got any
	_ = json.Unmarshal(b, &got)
	_ = json.Unmarshal([]byte(`{"points":[{"value":[1,2]},{"value":[3,4]}],"rings":[{"value":[{"lon":1,"lat":2}]}]}`), &exp)
	if !reflect.DeepEqual(got, exp) {
		t.Errorf("response = %s", b)
	}
}

const nestedMapSpec = `{
  "openapi": "3.0.3",
  "info": {"title": "grid", "version": "1.0.0"},
  "paths": {"/grids": {"post": {
    "operationId": "putGrid",
    "requestBody": {"required": true, "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Grid"}}}},
    "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Grid"}}}}}
  }}},
  "components": {"schemas": {
    "Grid": {"type": "object", "properties": {
      "cells": {"type": "object", "additionalProperties": {"type": "object", "additionalProperties": {"type": "integer"}}}
    }}
  }}
}`

// A map of map travels over gRPC as a CellsValue box ({value: {...}}).
// The gateway unboxes it for the upstream and boxes the response.
func TestGRPCIngress_OpenAPINestedMap(t *testing.T) {
	var sent atomic.Pointer[[]byte]
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sent.Store(&body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"cells":{"a":{"x":1,"y":2},"b":{"z":3}}}`))
	}))
	t.Cleanup(backend.Close)
	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("test-token")))
	t.Cleanup(gw.Close)
	if err := gw.AddOpenAPIBytes([]byte(nestedMapSpec), To(backend.URL), As("grid")); err != nil {
		t.Fatalf("AddOpenAPIBytes: %v", err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.UnknownServiceHandler(gw.GRPCUnknownHandler()))
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	inDesc, outDesc := (&crossFormatFixture{gw: gw}).synthMethodDescriptors(t, "grid", "v1", "putGrid")
	sd := inDesc.ParentFile().Services().Get(0)

	in := dynamicpb.NewMessage(inDesc)
	if err := protojson.Unmarshal([]byte(`{"body":{"cells":{"a":{"value":{"x":1,"y":2}},"b":{"value":{"z":3}}}}}`), in); err != nil {
		t.Fatalf("build request: %v", err)
	}
	out := dynamicpb.NewMessage(outDesc)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Invoke(ctx, "/"+string(sd.FullName())+"/putGrid", in, out); err != nil {
		t.Fatalf("invoke: %v", err)
	}
	body := sent.Load()
	if body == nil {
		t.Fatal("backend not called")
	}
	var up, exp any
	_ = json.Unmarshal(*body, &up)
	_ = json.Unmarshal([]byte(`{"cells":{"a":{"x":1,"y":2},"b":{"z":3}}}`), &exp)
	if !reflect.DeepEqual(up, exp) {
		t.Errorf("upstream body = %s", *body)
	}
	b, _ := protojson.Marshal(out)
	var got any
	_ = json.Unmarshal(b, &got)
	_ = json.Unmarshal([]byte(`{"cells":{"a":{"value":{"x":1,"y":2}},"b":{"value":{"z":3}}}}`), &exp)
	if !reflect.DeepEqual(got, exp) {
		t.Errorf("response = %s", b)
	}
}
