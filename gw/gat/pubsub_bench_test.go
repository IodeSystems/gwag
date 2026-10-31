package gat

import "testing"

// BenchmarkPubSubPublish measures one publish through the PubSub
// facade with one matching subscriber (drained by a goroutine) and
// eight non-matching ones, so pattern matching is on the measured
// path.
func BenchmarkPubSubPublish(b *testing.B) {
	g, _ := New()
	defer g.Close()
	ps := g.PubSub()
	ch, cancel := ps.Subscribe("bench.*")
	defer cancel()
	for _, p := range []string{"a", "a.b", "a.>", "other.*", "x.y.z", "bench.a.b", "*.c", "q.>"} {
		_, c := ps.Subscribe(p)
		defer c()
	}
	go func() {
		for range ch {
		}
	}()
	payload := []byte("payload")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ps.Publish("bench.a", payload)
	}
}
