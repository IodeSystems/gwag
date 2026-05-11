package gateway

import (
	"strings"
	"testing"

	"github.com/iodesystems/go-api-gateway/gw/ir"
)

// TestInjectProtoSubscriptionAuthDoc_AppendsContract pins that the
// HMAC channel-auth contract gets appended to every proto
// subscription op's Description. Adopters reading SDL or the MCP
// search corpus see "you need a token to subscribe" without having
// to cross-reference docs.
func TestInjectProtoSubscriptionAuthDoc_AppendsContract(t *testing.T) {
	svc := &ir.Service{
		Operations: []*ir.Operation{
			{Name: "Hello", Kind: ir.OpQuery, Description: "Unary RPC."},
			{Name: "Greetings", Kind: ir.OpSubscription, Description: "Server-streaming."},
			{Name: "Echo", Kind: ir.OpSubscription},
		},
	}
	injectProtoSubscriptionAuthDoc(svc)

	if got := svc.Operations[0].Description; got != "Unary RPC." {
		t.Errorf("Hello (query) Description mutated: %q", got)
	}
	if got := svc.Operations[1].Description; !strings.Contains(got, "Server-streaming.") || !strings.Contains(got, "HMAC channel token") {
		t.Errorf("Greetings Description = %q, want original + HMAC contract", got)
	}
	if got := svc.Operations[2].Description; !strings.Contains(got, "HMAC channel token") {
		t.Errorf("Echo Description = %q, want HMAC contract on empty-doc subscription", got)
	}
}
