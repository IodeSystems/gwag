package gateway

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// subBroker shares NATS subscriptions across WebSocket clients
// listening on the same subject. With 10k clients on the same channel,
// the broker holds ONE NATS sub and fans out each message in-process
// to 10k consumer channels. Per-subject reference counted; the NATS
// sub is closed when the last consumer leaves.
type subBroker struct {
	nc *nats.Conn

	mu   sync.Mutex
	subs map[string]*subFanout // subject → fanout
}

type subFanout struct {
	subject    string
	outputDesc protoreflect.MessageDescriptor
	natsSub    *nats.Subscription

	mu       sync.Mutex
	nextID   uint64
	targets  map[uint64]chan any
}

func newSubBroker(nc *nats.Conn) *subBroker {
	return &subBroker{nc: nc, subs: map[string]*subFanout{}}
}

// acquire opens (or joins) the fanout for subject and returns a channel
// of decoded events plus a release func. release must be called exactly
// once per acquire — typically deferred against the subscriber's
// context. The returned channel is bidirectional only because graphql-
// go's subscription executor type-asserts against `chan interface{}`;
// callers must not write to it.
//
// outputDesc is captured on the FIRST acquire for a subject; later
// acquires reuse the cached fanout regardless of any later descriptor
// argument (one subject, one type — the registration story enforces).
func (b *subBroker) acquire(subject string, outputDesc protoreflect.MessageDescriptor) (chan any, func(), error) {
	b.mu.Lock()
	f, ok := b.subs[subject]
	if !ok {
		f = &subFanout{
			subject:    subject,
			outputDesc: outputDesc,
			targets:    map[uint64]chan any{},
		}
		sub, err := b.nc.Subscribe(subject, f.deliver)
		if err != nil {
			b.mu.Unlock()
			return nil, nil, fmt.Errorf("nats subscribe %s: %w", subject, err)
		}
		f.natsSub = sub
		b.subs[subject] = f
	}
	b.mu.Unlock()

	f.mu.Lock()
	id := f.nextID
	f.nextID++
	ch := make(chan any, 32)
	f.targets[id] = ch
	f.mu.Unlock()

	released := atomic.Bool{}
	release := func() {
		if !released.CompareAndSwap(false, true) {
			return
		}
		f.mu.Lock()
		if c, ok := f.targets[id]; ok {
			close(c)
			delete(f.targets, id)
		}
		empty := len(f.targets) == 0
		f.mu.Unlock()
		if !empty {
			return
		}
		// Last consumer left — close the NATS sub and drop the entry.
		b.mu.Lock()
		if cur, ok := b.subs[subject]; ok && cur == f {
			delete(b.subs, subject)
			_ = f.natsSub.Unsubscribe()
		}
		b.mu.Unlock()
	}
	return ch, release, nil
}

// deliver decodes the proto-encoded NATS payload once and fans it out
// to every active target channel. Non-blocking sends: a slow consumer
// drops events rather than gating the broker. Trade-off note: drop
// policy keeps healthy consumers fast; consider switching to "kick
// the slow one" if drops become operationally meaningful.
//
// The event message itself is pool-acquired so steady-state
// subscriptions don't allocate a fresh dynamicpb per delivery; we
// release it once messageToMap has built the canonical payload (the
// payload is the only thing fanned out — no consumer holds the
// underlying message).
func (f *subFanout) deliver(msg *nats.Msg) {
	event := acquireDynamicMessage(f.outputDesc)
	defer releaseDynamicMessage(f.outputDesc, event)
	if err := proto.Unmarshal(msg.Data, event); err != nil {
		return
	}
	payload := messageToMap(event)

	f.mu.Lock()
	chans := make([]chan any, 0, len(f.targets))
	for _, c := range f.targets {
		chans = append(chans, c)
	}
	f.mu.Unlock()

	for _, c := range chans {
		select {
		case c <- payload:
		default:
		}
	}
}

// activeSubjectCount is informational — useful for tests or future
// metrics. Returns the number of NATS subjects with ≥1 in-process
// consumer.
func (b *subBroker) activeSubjectCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

// activeSubjects returns one entry per subject the broker is fanning
// out, sorted by subject name. Order is stable so the admin UI shows
// the same listing across polls.
func (b *subBroker) activeSubjects() []subjectInfo {
	b.mu.Lock()
	out := make([]subjectInfo, 0, len(b.subs))
	for _, f := range b.subs {
		f.mu.Lock()
		consumers := len(f.targets)
		f.mu.Unlock()
		out = append(out, subjectInfo{
			Subject:   f.subject,
			Consumers: consumers,
		})
	}
	b.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Subject < out[j].Subject })
	return out
}

// subjectInfo describes one active subscription subject and how many
// in-process WebSocket consumers are listening on it.
type subjectInfo struct {
	Subject   string `json:"subject"`
	Consumers int    `json:"consumers"`
}

// activeSubjects returns a snapshot of currently-fanned-out
// subscription subjects. Empty when the broker hasn't been
// initialised yet (no cluster, or no subscribe has happened).
func (g *Gateway) activeSubjects() []subjectInfo {
	g.mu.Lock()
	br := g.broker
	g.mu.Unlock()
	if br == nil {
		return nil
	}
	return br.activeSubjects()
}

// silence unused context import warning when build tags trim file.
var _ = context.Background
