package gateway

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMetricPrefix(t *testing.T) {
	scrape := func(opts ...Option) string {
		g := New(append([]Option{WithAdminToken([]byte("t"))}, opts...)...)
		t.Cleanup(g.Close)
		g.cfg.metrics.RecordDispatch(context.Background(), "ns", "v1", "m", time.Millisecond, nil)
		rec := httptest.NewRecorder()
		g.MetricsHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
		return rec.Body.String()
	}
	def := scrape()
	if !strings.Contains(def, "go_api_gateway_dispatch_duration_seconds") || strings.Contains(def, "gwag_") {
		t.Errorf("default scrape must use go_api_gateway_ only:\n%s", def)
	}
	opt := scrape(WithMetricPrefix("gwag"))
	if !strings.Contains(opt, "gwag_dispatch_duration_seconds") || strings.Contains(opt, "go_api_gateway_") {
		t.Errorf("prefixed scrape must use gwag_ only:\n%s", opt)
	}
}
