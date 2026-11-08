package protocodec

import (
	"testing"

	"google.golang.org/protobuf/types/dynamicpb"
)

// TestArgsToMessage_ProtoNameKey: a top-level arg keyed by the proto
// field name (user_id, as the GraphQL renderer names proto op args)
// is written; the lowerCamel key wins when both are present.
func TestArgsToMessage_ProtoNameKey(t *testing.T) {
	md := testMessage(t)
	f := md.Fields().ByName("user_id")
	msg := dynamicpb.NewMessage(md)
	if err := gwagOpts.ArgsToMessage(map[string]any{"user_id": "7"}, msg); err != nil {
		t.Fatal(err)
	}
	if got := msg.Get(f).Int(); got != 7 {
		t.Fatalf("user_id = %d, want 7", got)
	}
	msg = dynamicpb.NewMessage(md)
	if err := gwagOpts.ArgsToMessage(map[string]any{"user_id": "7", "userId": "8"}, msg); err != nil {
		t.Fatal(err)
	}
	if got := msg.Get(f).Int(); got != 8 {
		t.Fatalf("user_id = %d, want 8 (lowerCamel key)", got)
	}
}
