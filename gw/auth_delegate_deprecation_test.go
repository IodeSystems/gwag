package gateway

import (
	"testing"
)

// TestWarnSubscribeDelegateDeprecated_FiresOncePerNSVer covers the
// once-per-(ns,ver) gate: the first call for a tuple returns true
// (the warning fires); subsequent calls for the same tuple return
// false (heartbeats / replica joins don't spam logs). A new (ns,
// different ver) re-fires.
func TestWarnSubscribeDelegateDeprecated_FiresOncePerNSVer(t *testing.T) {
	gw := New(WithoutMetrics(), WithoutBackpressure())
	t.Cleanup(gw.Close)

	if !gw.warnSubscribeDelegateDeprecated(authorizerNamespace, "v1") {
		t.Fatal("first call for _events_auth/v1 should fire")
	}
	if gw.warnSubscribeDelegateDeprecated(authorizerNamespace, "v1") {
		t.Fatal("second call for _events_auth/v1 should be silent")
	}
	if gw.warnSubscribeDelegateDeprecated(authorizerNamespace, "v1") {
		t.Fatal("third call for _events_auth/v1 should still be silent")
	}
	if !gw.warnSubscribeDelegateDeprecated(authorizerNamespace, "v2") {
		t.Fatal("first call for _events_auth/v2 should fire (different version)")
	}
}

// TestWarnSubscribeDelegateDeprecated_OtherNSSilent — only the
// reserved authorizer namespace triggers; other namespaces never do,
// even ones that look related.
func TestWarnSubscribeDelegateDeprecated_OtherNSSilent(t *testing.T) {
	gw := New(WithoutMetrics(), WithoutBackpressure())
	t.Cleanup(gw.Close)

	for _, ns := range []string{"users", "_admin_auth", "_events", "events_auth", "_events_authz"} {
		if gw.warnSubscribeDelegateDeprecated(ns, "v1") {
			t.Errorf("ns=%q should not fire deprecation warning", ns)
		}
	}
}
