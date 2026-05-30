package runner

import "testing"

func TestMetricsURLWithPath(t *testing.T) {
	cases := []struct {
		name, target, path, want string
	}{
		{"default-from-base", "http://gw:8080", "/api/metrics", "http://gw:8080/api/metrics"},
		{"strips-path-and-query", "http://gw/api/graphql?x=1", "/api/metrics", "http://gw/api/metrics"},
		{"raw-metrics-path", "http://gw:8080/api/ingress/foo", "/metrics", "http://gw:8080/metrics"},
		{"empty-path-disables", "http://gw:8080", "", ""},
		{"bad-target", "://nope", "/metrics", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := MetricsURLWithPath(c.target, c.path); got != c.want {
				t.Errorf("MetricsURLWithPath(%q, %q) = %q, want %q", c.target, c.path, got, c.want)
			}
		})
	}
}

func TestMetricsURLFromGateway_DefaultsToAPIMetrics(t *testing.T) {
	if got := MetricsURLFromGateway("http://gw:8080/api/graphql"); got != "http://gw:8080/api/metrics" {
		t.Errorf("got %q", got)
	}
}
