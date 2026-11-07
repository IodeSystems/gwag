package gateway

import (
	"fmt"
	"testing"
)

// Cost of the public schema view (schema_view.go) next to the full
// rebuild it follows: the self-ingested admin surface plus 20 proto
// namespaces, half of them public. The view is built once per schema
// generation, on first use.
func BenchmarkSchemaView_Build(b *testing.B) {
	g := New(WithoutMetrics(), WithoutBackpressure())
	b.Cleanup(g.Close)
	_, spec, err := g.AdminHumaRouter()
	if err != nil {
		b.Fatal(err)
	}
	if err := g.AddOpenAPIBytes(spec, As("admin"), To("http://localhost/api")); err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		for _, f := range []string{"greeter.proto", "library.proto"} {
			opts := []ServiceOption{To(nopGRPCConn{}), As(fmt.Sprintf("%s%d", f[:3], i))}
			if i%2 == 0 {
				opts = append(opts, AsPublic())
			}
			if err := g.AddProtoBytes(f, testProtoBytes(b, f), opts...); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.Run("full", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			g.mu.Lock()
			err := g.assembleLocked()
			g.mu.Unlock()
			if err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("public", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			g.publicGen.Add(1)
			if _, err := g.publicSchema(); err != nil {
				b.Fatal(err)
			}
		}
	})
}
