package runner

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

// TestBuildJSONOutput_Shape pins the field names + types the sweep
// driver consumes. If anything in this list moves, the sweep driver
// must move with it; failing this test forces the conversation.
func TestBuildJSONOutput_Shape(t *testing.T) {
	s := NewStats()
	s.RecordCode("200")
	s.RecordCode("200")
	s.RecordCode("500")
	s.RecordOK(800 * time.Microsecond)
	s.RecordOK(1200 * time.Microsecond)
	s.RecordErr(ErrHTTP, "status=500")

	opts := Options{RPS: 1000, Concurrency: 64, Duration: 30 * time.Second}
	res := PassResult{
		Label:   "gateway",
		Targets: []Target{{Label: "http://gw/api/graphql", MetricsURL: "http://gw/api/metrics"}},
		Stats:   []*Stats{s},
		Elapsed: 30 * time.Second,
	}

	out := BuildJSONOutput(opts, res)

	if out.Schema != JSONSchemaVersion {
		t.Errorf("schema = %q, want %q", out.Schema, JSONSchemaVersion)
	}
	if out.Label != "gateway" {
		t.Errorf("label = %q", out.Label)
	}
	if out.TargetRPS != 1000 {
		t.Errorf("target_rps = %d", out.TargetRPS)
	}
	if out.Concurrency != 64 {
		t.Errorf("concurrency = %d", out.Concurrency)
	}
	if out.Duration != 30 {
		t.Errorf("duration_seconds = %v", out.Duration)
	}
	if len(out.Targets) != 1 {
		t.Fatalf("targets = %d, want 1", len(out.Targets))
	}
	tr := out.Targets[0]
	if tr.Label != "http://gw/api/graphql" || tr.MetricsURL != "http://gw/api/metrics" {
		t.Errorf("target label/metrics_url unexpected: %+v", tr)
	}
	if tr.OK != 2 || tr.Errs != 1 {
		t.Errorf("ok=%d errs=%d, want 2 / 1", tr.OK, tr.Errs)
	}
	if tr.AchievedRPS == 0 {
		t.Error("achieved_rps zero")
	}
	if tr.P50Us == 0 || tr.P95Us == 0 || tr.P99Us == 0 || tr.MeanUs == 0 {
		t.Errorf("latency µs fields unpopulated: %+v", tr)
	}
	if got := tr.Codes["200"]; got != 2 {
		t.Errorf("codes[200] = %d, want 2", got)
	}
	if got := tr.Errors["http"]; got != 1 {
		t.Errorf("errors[http] = %d, want 1", got)
	}
	if out.Gateway != nil {
		t.Errorf("gateway block should be omitted when PreSnap/PostSnap are nil; got %+v", out.Gateway)
	}
}

// TestBuildJSONOutput_GatewayOmittedWithoutSnapshots covers the
// branch where ServerMetrics was off (or fetching failed): gateway
// must be omitted, not emitted as an empty object.
func TestBuildJSONOutput_GatewayOmittedWithoutSnapshots(t *testing.T) {
	s := NewStats()
	s.RecordOK(time.Millisecond)
	out := BuildJSONOutput(Options{RPS: 100}, PassResult{
		Targets: []Target{{Label: "x"}},
		Stats:   []*Stats{s},
		Elapsed: time.Second,
	})
	if out.Gateway != nil {
		t.Errorf("gateway = %+v, want nil", out.Gateway)
	}
	// Round-trip through JSON and decode into a map to verify the
	// `gateway` key is genuinely absent (not just zero-valued).
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(out); err != nil {
		t.Fatal(err)
	}
	var roundtrip map[string]any
	if err := json.Unmarshal(buf.Bytes(), &roundtrip); err != nil {
		t.Fatal(err)
	}
	if _, present := roundtrip["gateway"]; present {
		t.Errorf("`gateway` key present in JSON when no snapshots; want absent. JSON=%s", buf.String())
	}
}

// TestWriteJSON_IndentedAndDecodable belts-and-braces: the on-disk
// shape must decode back into the same struct without manual
// massaging, so a consumer using encoding/json can deserialise.
func TestWriteJSON_IndentedAndDecodable(t *testing.T) {
	s := NewStats()
	s.RecordOK(500 * time.Microsecond)
	var buf bytes.Buffer
	err := WriteJSON(&buf, Options{RPS: 10}, PassResult{
		Targets: []Target{{Label: "t"}},
		Stats:   []*Stats{s},
		Elapsed: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("\n  \"schema\":")) {
		t.Errorf("output not indented; got: %s", buf.String())
	}
	var got JSONOutput
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, buf.String())
	}
	if got.Schema != JSONSchemaVersion {
		t.Errorf("schema after roundtrip = %q", got.Schema)
	}
}
