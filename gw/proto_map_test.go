package gateway

import (
	"encoding/json"
	"math"
	"strconv"
	"testing"

	"google.golang.org/protobuf/types/dynamicpb"
)

const protoMapSource = `syntax = "proto3";
package maps.v1;
enum Color { COLOR_UNSPECIFIED = 0; COLOR_RED = 1; }
message Inner { bytes blob = 1; Color color = 2; }
message Msg {
  map<string, int64> counts = 1;
  map<int32, Inner> inners = 2;
  map<bool, Color> flags = 3;
  uint64 big = 4;
  fixed64 fixed = 5;
}
`

// Proto map fields convert both ways (they used to panic in dynamicpb),
// and the map and append paths render them the same.
func TestProtoMapFields(t *testing.T) {
	fd, err := compileProtoBytes("maps.proto", []byte(protoMapSource), nil)
	if err != nil {
		t.Fatal(err)
	}
	md := fd.Messages().ByName("Msg")
	maxU := strconv.FormatUint(math.MaxUint64, 10)
	args := map[string]any{
		"counts": map[string]any{"a": "9007199254740993", "b": 2},
		"inners": map[string]any{"7": map[string]any{"blob": "aGk=", "color": "COLOR_RED"}},
		"flags":  map[string]any{"true": "COLOR_RED"},
		"big":    maxU,
		"fixed":  "18446744073709551614",
	}
	msg := dynamicpb.NewMessage(md)
	if err := argsToMessage(args, msg); err != nil {
		t.Fatal(err)
	}
	mapPath, err := json.Marshal(messageToMap(msg))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"big":"` + maxU + `","counts":{"a":"9007199254740993","b":"2"},"fixed":"18446744073709551614","flags":{"true":"COLOR_RED"},"inners":{"7":{"blob":"aGk=","color":"COLOR_RED"}}}`
	if string(mapPath) != want {
		t.Errorf("map path:\n got %s\nwant %s", mapPath, want)
	}
	// The append path keeps declaration order; compare as values.
	appendPath := appendProtoMessage(nil, msg, nil)
	var a any
	if err := json.Unmarshal(appendPath, &a); err != nil {
		t.Fatalf("append path: %v: %s", err, appendPath)
	}
	if ja, _ := json.Marshal(a); string(ja) != string(mapPath) {
		t.Errorf("append path differs:\n got %s\nwant %s", ja, mapPath)
	}

	for name, bad := range map[string]map[string]any{
		"negative uint64": {"big": -1},
		"uint64 overflow": {"big": "18446744073709551616"},
		"bad map key":     {"inners": map[string]any{"x": map[string]any{}}},
		"map not object":  {"counts": []any{1}},
	} {
		if err := argsToMessage(bad, dynamicpb.NewMessage(md)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
