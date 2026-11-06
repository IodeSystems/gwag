package ir

import (
	"context"
	"io"
	"log"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
)

const openapiStreamSpec = `{
  "openapi": "3.2.0",
  "info": {"title": "ticks", "version": "1.0.0"},
  "paths": {
    "/sse": {"get": {"operationId": "sseJSON",
      "parameters": [{"name": "n", "in": "query", "schema": {"type": "integer"}}],
      "responses": {"200": {"description": "ok", "content": {"text/event-stream": {"itemSchema": {
        "type": "object", "required": ["data"],
        "properties": {
          "data": {"type": "string", "contentMediaType": "application/json",
                   "contentSchema": {"$ref": "#/components/schemas/Tick"}},
          "event": {"type": "string"}, "id": {"type": "string"},
          "retry": {"type": "integer", "minimum": 0}}}}}}}}},
    "/sse-raw": {"get": {"operationId": "sseRaw",
      "responses": {"200": {"description": "ok", "content": {"text/event-stream": {"itemSchema": {
        "type": "object", "required": ["data"],
        "properties": {
          "data": {"type": "string"}, "event": {"type": "string"},
          "id": {"type": "string"}, "retry": {"type": "integer"}}}}}}}}},
    "/jsonl": {"get": {"operationId": "jsonl",
      "responses": {"200": {"description": "ok", "content": {"application/jsonl": {"itemSchema": {"$ref": "#/components/schemas/Tick"}}}}}}},
    "/ndjson": {"post": {"operationId": "ndjson",
      "responses": {"200": {"description": "ok", "content": {"application/x-ndjson": {"itemSchema": {"$ref": "#/components/schemas/Tick"}}}}}}},
    "/seq": {"get": {"operationId": "seq",
      "responses": {"200": {"description": "ok", "content": {"application/json-seq": {"itemSchema": {"$ref": "#/components/schemas/Tick"}}}}}}},
    "/both": {"get": {"operationId": "both",
      "responses": {"200": {"description": "ok", "content": {
        "application/json": {"schema": {"type": "array", "items": {"$ref": "#/components/schemas/Tick"}}},
        "application/jsonl": {"itemSchema": {"$ref": "#/components/schemas/Tick"}}}}}}},
    "/legacy": {"get": {"operationId": "legacy",
      "responses": {"200": {"description": "ok", "content": {"text/event-stream": {"schema": {"type": "string"}}}}}}}
  },
  "components": {"schemas": {"Tick": {"type": "object", "required": ["n"],
    "properties": {"n": {"type": "integer"}, "msg": {"type": "string"}}}}}
}`

func loadStreamSpec(t *testing.T) (*openapi3.T, *Service) {
	t.Helper()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData([]byte(openapiStreamSpec))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("validate: %v", err)
	}
	svc := IngestOpenAPI(doc)
	svc.Namespace, svc.Version = "ticks", "v1"
	PopulateSchemaIDs(svc)
	return doc, svc
}

func opNamed(svc *Service, name string) *Operation {
	for _, o := range svc.Operations {
		if o.Name == name {
			return o
		}
	}
	return nil
}

func TestOpenAPIIngest_StreamedResponses(t *testing.T) {
	_, svc := loadStreamSpec(t)
	cases := []struct {
		op, media, output string
		sseData           bool
	}{
		{"sseJSON", StreamSSE, "Tick", true},
		{"jsonl", StreamJSONL, "Tick", false},
		{"ndjson", StreamNDJSON, "Tick", false},
		{"seq", StreamJSONSeq, "Tick", false},
		{"sseRaw", StreamSSE, "sseRawEvent", false},
	}
	for _, c := range cases {
		op := opNamed(svc, c.op)
		if op == nil {
			t.Fatalf("%s missing", c.op)
		}
		if op.Kind != OpSubscription {
			t.Errorf("%s: kind = %v, want subscription", c.op, op.Kind)
		}
		if op.Output == nil || op.Output.Named != c.output || !op.OutputRequired || op.OutputRepeated {
			t.Errorf("%s: output = %+v required=%v repeated=%v, want %s!", c.op, op.Output, op.OutputRequired, op.OutputRepeated, c.output)
		}
		st := OpenAPIStreamOf(op.Origin.(*openapi3.Operation))
		if st == nil || st.MediaType != c.media || st.SSEData != c.sseData {
			t.Errorf("%s: stream = %+v, want %s sseData=%v", c.op, st, c.media, c.sseData)
		}
	}
	if args := opNamed(svc, "sseJSON").Args; len(args) != 1 || args[0].Name != "n" {
		t.Errorf("sseJSON args = %+v", args)
	}
	env := svc.Types["sseRawEvent"]
	if env == nil || len(env.Fields) != 4 {
		t.Fatalf("sseRawEvent = %+v", env)
	}

	// Unary JSON wins over a stream alternative: one op, unchanged shape.
	both := opNamed(svc, "both")
	if both.Kind != OpQuery || !both.OutputRepeated || both.Output.Named != "Tick" {
		t.Errorf("both = kind %v output %+v repeated %v, want unary [Tick]", both.Kind, both.Output, both.OutputRepeated)
	}
	// 3.0/3.1-style text/event-stream without itemSchema: unchanged
	// (unary, no output).
	legacy := opNamed(svc, "legacy")
	if legacy.Kind != OpQuery || legacy.Output != nil {
		t.Errorf("legacy = kind %v output %+v, want query with no output", legacy.Kind, legacy.Output)
	}
}

func TestOpenAPIStream_Renders(t *testing.T) {
	doc, svc := loadStreamSpec(t)
	got, err := RenderOpenAPI(svc)
	if err != nil || got != doc {
		t.Fatalf("RenderOpenAPI is not the source doc: %v", err)
	}
	fds, err := RenderProtoFiles([]*Service{svc})
	if err != nil {
		t.Fatalf("RenderProtoFiles: %v", err)
	}
	streaming := map[string]bool{}
	for _, f := range fds.File {
		for _, s := range f.Service {
			for _, m := range s.Method {
				streaming[m.GetName()] = m.GetServerStreaming()
			}
		}
	}
	for _, name := range []string{"sseJSON", "sseRaw", "jsonl", "ndjson", "seq"} {
		if !streaming[name] {
			t.Errorf("%s: not a server-streaming rpc", name)
		}
	}
	if streaming["both"] {
		t.Errorf("both: rendered server-streaming")
	}
}

func readAll(t *testing.T, s *OpenAPIStream, body string) []any {
	t.Helper()
	r := NewOpenAPIStreamReader(s, strings.NewReader(body))
	var out []any
	for {
		v, err := r.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		out = append(out, v)
	}
}

func TestOpenAPIStreamReader(t *testing.T) {
	ticks := []any{map[string]any{"n": 1.0}, map[string]any{"n": 2.0}}
	cases := []struct {
		name string
		s    *OpenAPIStream
		body string
		want []any
	}{
		{"jsonl", &OpenAPIStream{MediaType: StreamJSONL}, "{\"n\":1}\n\n{\"n\":2}\r\n", ticks},
		{"ndjson no trailing newline", &OpenAPIStream{MediaType: StreamNDJSON}, "{\"n\":1}\n{\"n\":2}", ticks},
		{"json-seq", &OpenAPIStream{MediaType: StreamJSONSeq}, "\x1e{\"n\":1}\n\x1e{\"n\":2}\n", ticks},
		{"sse data JSON, comments, multi-line, CR endings",
			&OpenAPIStream{MediaType: StreamSSE, SSEData: true},
			": heartbeat\n\nevent: tick\nid: 7\ndata: {\"n\":\ndata: 1}\n\r\ndata: {\"n\":2}\r\r" +
				"data: {\"n\":3}", // unterminated: dropped
			ticks},
		{"sse envelope",
			&OpenAPIStream{MediaType: StreamSSE},
			"retry: 50\nevent: a\nid: x\ndata: hello\ndata:world\n\ndata: second\n\n",
			[]any{
				map[string]any{"data": "hello\nworld", "event": "a", "id": "x", "retry": 50.0},
				map[string]any{"data": "second", "id": "x"},
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := readAll(t, c.s, c.body)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %#v\nwant %#v", got, c.want)
			}
		})
	}
}

// A malformed item is skipped and reported through OnSkip; the items
// around it still come through, in every framing.
func TestOpenAPIStreamReader_BadItemSkipped(t *testing.T) {
	ticks := []any{map[string]any{"n": 1.0}, map[string]any{"n": 2.0}}
	cases := []struct {
		name string
		s    *OpenAPIStream
		body string
	}{
		{"jsonl", &OpenAPIStream{MediaType: StreamJSONL}, "{\"n\":1}\nnope\n{\"n\":2}\n"},
		{"ndjson", &OpenAPIStream{MediaType: StreamNDJSON}, "{\"n\":1}\n{\"n\":\n{\"n\":2}\n"},
		{"json-seq", &OpenAPIStream{MediaType: StreamJSONSeq}, "\x1e{\"n\":1}\n\x1e{\"n\":\n\x1e{\"n\":2}\n"},
		{"sse data JSON", &OpenAPIStream{MediaType: StreamSSE, SSEData: true}, "data: {\"n\":1}\n\ndata: nope\n\ndata: {\"n\":2}\n\n"},
		{"sse envelope JSON data", &OpenAPIStream{MediaType: StreamSSE, sseDataJSON: true},
			"data: {\"n\":1}\n\ndata: nope\n\ndata: {\"n\":2}\n\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := NewOpenAPIStreamReader(c.s, strings.NewReader(c.body))
			skipped := 0
			r.OnSkip = func(error) { skipped++ }
			var got []any
			for {
				v, err := r.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatalf("Next: %v", err)
				}
				if m, ok := v.(map[string]any); ok && c.s.sseDataJSON {
					v = m["data"]
				}
				got = append(got, v)
			}
			if !reflect.DeepEqual(got, ticks) {
				t.Errorf("got %#v want %#v", got, ticks)
			}
			if skipped != 1 {
				t.Errorf("skipped = %d, want 1", skipped)
			}
		})
	}
}

// A record cut off at EOF is a bad item too: skipped, then EOF.
func TestOpenAPIStreamReader_TruncatedTail(t *testing.T) {
	r := NewOpenAPIStreamReader(&OpenAPIStream{MediaType: StreamJSONSeq}, strings.NewReader("\x1e{\"n\":1}\n\x1e{\"n\":"))
	skipped := 0
	r.OnSkip = func(error) { skipped++ }
	if _, err := r.Next(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Next(); err != io.EOF || skipped != 1 {
		t.Fatalf("err=%v skipped=%d", err, skipped)
	}
}

func TestSkipLogger_RateLimited(t *testing.T) {
	var buf strings.Builder
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	f := NewSkipLogger("x", time.Hour)
	for i := 0; i < 5; i++ {
		f(io.ErrUnexpectedEOF)
	}
	if n := strings.Count(buf.String(), "skipped malformed"); n != 1 {
		t.Fatalf("logged %d lines, want 1:\n%s", n, buf.String())
	}
}
