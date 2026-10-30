package gateway

import (
	"math"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// protoreflect.Value.Int panics on a uint64 value, so the map path
// must read unsigned kinds with Uint.
func TestMessageToMap_Uint64(t *testing.T) {
	msg := dynamicpb.NewMessage((&wrapperspb.UInt64Value{}).ProtoReflect().Descriptor())
	msg.Set(msg.Descriptor().Fields().ByName("value"), protoreflect.ValueOfUint64(math.MaxUint64))
	if got := messageToMap(msg)["value"]; got != "18446744073709551615" {
		t.Fatalf("got %v", got)
	}
}
