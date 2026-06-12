package relay

import "sync"

// Coalesce wraps a Source so concurrent subscriptions to the SAME channel share
// ONE upstream subscription, fanned out in-process to every subscriber. So a
// relay holds a single NATS sub per channel no matter how many local clients
// watch it: N viewers of a hot channel = 1 upstream sub + 1 wire copy fanned out
// N times in-process, not N upstream subs and N wire copies. New applies this by
// default (opt out with WithoutCoalescing).
//
// The wrapped Source must honor the Source contract that deliver runs on its own
// goroutine (not synchronously during Subscribe) — both NatsSource and any sane
// bus do; otherwise the fan-out lock would re-enter.
func Coalesce(src Source) Source {
	return &coalescer{src: src, groups: map[string]*coGroup{}}
}

type coGroup struct {
	cancel func() // upstream cancel, invoked when the last subscriber leaves
	subs   map[int]func([]byte)
	nextID int
}

type coalescer struct {
	src    Source
	mu     sync.Mutex
	groups map[string]*coGroup
}

func (c *coalescer) Subscribe(channel string, deliver func([]byte)) (func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	g := c.groups[channel]
	if g == nil {
		g = &coGroup{subs: map[int]func([]byte){}}
		cancel, err := c.src.Subscribe(channel, func(payload []byte) { c.fanout(channel, payload) })
		if err != nil {
			return func() {}, err
		}
		g.cancel = cancel
		c.groups[channel] = g
	}
	id := g.nextID
	g.nextID++
	g.subs[id] = deliver

	return func() { c.unsub(channel, id) }, nil
}

func (c *coalescer) fanout(channel string, payload []byte) {
	c.mu.Lock()
	g := c.groups[channel]
	var fns []func([]byte)
	if g != nil {
		fns = make([]func([]byte), 0, len(g.subs))
		for _, d := range g.subs {
			fns = append(fns, d)
		}
	}
	c.mu.Unlock()
	for _, d := range fns {
		d(payload)
	}
}

func (c *coalescer) unsub(channel string, id int) {
	c.mu.Lock()
	g := c.groups[channel]
	if g == nil {
		c.mu.Unlock()
		return
	}
	delete(g.subs, id)
	if len(g.subs) > 0 {
		c.mu.Unlock()
		return
	}
	delete(c.groups, channel)
	cancel := g.cancel
	c.mu.Unlock()
	cancel() // last subscriber gone → drop the upstream subscription
}
