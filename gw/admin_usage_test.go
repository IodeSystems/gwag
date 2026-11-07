package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type usageResp struct {
	Window string `json:"window"`
	Edges  []struct {
		Caller, Namespace, Version, Method string
		Count                              uint64
		LastSeenUnixMs                     int64
	} `json:"edges"`
	Services []struct {
		Namespace, Version string
		TotalCount         uint64
		CallerCount        int
		LastSeenUnixMs     int64
	} `json:"services"`
}

func TestAdminHuma_UsageEdges(t *testing.T) {
	old := nowFunc
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { nowFunc = old })

	gw := New(WithoutMetrics(), WithoutBackpressure(), WithAdminToken([]byte("tok")), WithCallerHeaders("X-Caller-Service"))
	t.Cleanup(gw.Close)
	stampSlot(t, gw, "users", "v1", "")
	stampSlot(t, gw, "users", "v2", "")
	stampSlot(t, gw, "greeter", "v1", "")

	rec := func(ns, ver, method, caller string) {
		ctx := context.Background()
		if caller != "" {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("X-Caller-Service", caller)
			ctx = withHTTPRequest(ctx, req)
		}
		gw.cfg.metrics.RecordDispatch(ctx, ns, ver, method, time.Millisecond, nil)
	}
	rec("users", "v1", "list", "ui")
	rec("users", "v1", "list", "ui")
	rec("users", "v1", "list", "billing")
	rec("users", "v1", "get", "ui")
	rec("greeter", "v1", "Hello", "") // anonymous -> unknown

	get := func(q string) usageResp {
		var out usageResp
		rr := httptest.NewRecorder()
		newAdminRouter(t, gw).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/services/usage"+q, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
		if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	out := get("?window=1h")
	if len(out.Edges) != 4 {
		t.Fatalf("edges=%d want 4: %+v", len(out.Edges), out.Edges)
	}
	top := out.Edges[0]
	if top.Caller != "ui" || top.Namespace != "users" || top.Version != "v1" || top.Method != "list" || top.Count != 2 {
		t.Errorf("top edge=%+v", top)
	}
	if top.LastSeenUnixMs != now.UnixMilli() {
		t.Errorf("lastSeen=%d want %d", top.LastSeenUnixMs, now.UnixMilli())
	}
	var sawUnknown bool
	for _, e := range out.Edges {
		if e.Namespace == "greeter" && e.Caller == "unknown" {
			sawUnknown = true
		}
	}
	if !sawUnknown {
		t.Error("anonymous call not recorded as caller=unknown")
	}
	byKey := map[string]int{}
	for i, s := range out.Services {
		byKey[s.Namespace+"/"+s.Version] = i
	}
	u1 := out.Services[byKey["users/v1"]]
	if u1.TotalCount != 4 || u1.CallerCount != 2 {
		t.Errorf("users/v1 rollup=%+v", u1)
	}
	i2, ok := byKey["users/v2"]
	if !ok || out.Services[i2].TotalCount != 0 || out.Services[i2].LastSeenUnixMs != 0 {
		t.Errorf("users/v2 should be listed with zero traffic: %+v", out.Services)
	}

	// Window: edges drop out of 1m after 5m idle but stay in 1h.
	now = now.Add(5 * time.Minute)
	if got := get("?window=1m"); len(got.Edges) != 0 {
		t.Errorf("1m after 5m idle: %d edges, want 0", len(got.Edges))
	}
	if got := get("?window=1h"); len(got.Edges) != 4 {
		t.Errorf("1h after 5m idle: %d edges, want 4", len(got.Edges))
	}

	f := get("?window=1h&namespace=users&version=v1")
	if len(f.Edges) != 3 || len(f.Services) != 1 {
		t.Errorf("filtered edges=%d services=%d, want 3/1", len(f.Edges), len(f.Services))
	}
}
