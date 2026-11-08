package gateway

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/reflect/protoreflect"

	sr "github.com/iodesystems/gwag/v2/events/schemaregistry"
	"github.com/iodesystems/gwag/v2/gw/internal/srserve"
)

// Schema Registry.
//
// With WithSchemaRegistry the gateway serves the Confluent Schema
// Registry REST API (SchemaRegistryHandler), so Kafka serdes, Kafka
// Connect and registry UIs can use gwag in place of a Confluent
// registry. State lives in the JetStream KV bucket gwag-schemas in a
// cluster (replicated like the other buckets), in memory otherwise.
//
// Every named channel of a registered proto slot is registered as a
// subject: <topic>-value (TopicNameStrategy; see
// SchemaRegistryOptions.Strategy), topic = the Kafka backend's default
// topic name (events.StreamName of the channel key) unless Topics maps
// it. The schema is schemaregistry.SchemaOf of the message's file, with
// its imports registered under their import paths as references:
// exactly what the Go serde's AutoRegister would send, so a serde in
// lookup mode finds it. Existing ids come in with `gwag schema import`
// (POST /gwag/import, admin token).
//
// The REST API is READONLY unless SchemaRegistryOptions.Writes; then
// the dev and admin tokens (basic-auth password or bearer) may
// register and delete schemas, an app key may register and delete the
// subjects of its namespaces' channels (ownedSubjects), and the admin
// token may set compatibility levels. A delete is refused while
// another schema references the version. Reads need no token.

const schemasBucketName = "gwag-schemas"

// SchemaRegistryOptions configures WithSchemaRegistry.
//
// Stability: experimental
type SchemaRegistryOptions struct {
	// Strategy names the subject of a channel's message: "topic"
	// (default, <topic>-value), "record" (<message full name>) or
	// "topic-record" (<topic>-<message full name>).
	Strategy string
	// Topics maps a channel key (orders.v1.OrderEvents) to its Kafka
	// topic. Default: events.StreamName(key) (EV_orders_v1_OrderEvents).
	Topics map[string]string
	// Writes turns on writes over the REST API (mode READWRITE): the
	// dev and admin tokens register and delete schemas, an app key
	// the subjects of its namespaces' channels, and the admin token
	// sets compatibility levels.
	Writes bool
	// FirstID is the lowest schema id gwag allocates (default 1). Ids
	// are always allocated above every id the registry holds, imported
	// ones included; set FirstID above the source registry's ids when
	// gwag registers its channels before the import runs.
	FirstID int
}

// WithSchemaRegistry turns on the served Schema Registry. See
// SchemaRegistryHandler. An unknown Strategy panics in New.
//
// Stability: experimental
func WithSchemaRegistry(o SchemaRegistryOptions) Option {
	return func(cfg *config) { cfg.schemaRegistry = &o }
}

type schemaRegistry struct {
	g        *Gateway
	o        SchemaRegistryOptions
	strategy sr.SubjectNameStrategy
	store    atomic.Pointer[srserve.Store]
	public   http.Handler
	internal http.Handler
	kick     chan struct{}

	mu sync.Mutex
	// done maps a subject to the schema last registered there, so an
	// index rebuild only registers what changed.
	done map[string]string
}

func newSchemaRegistry(g *Gateway) *schemaRegistry {
	o := g.cfg.schemaRegistry
	if o == nil {
		return nil
	}
	strategy, _ := sr.StrategyByName(o.Strategy) // validated in New
	r := &schemaRegistry{g: g, o: *o, strategy: strategy, kick: make(chan struct{}, 1), done: map[string]string{}}
	r.public = srserve.NewHandler(r.store.Load, srserve.HandlerOptions{Writes: o.Writes, Caller: g.schemaRegistryCaller})
	r.internal = srserve.NewHandler(r.store.Load, srserve.HandlerOptions{Writes: true, Caller: func(*http.Request) srserve.Caller { return srserve.Caller{Role: srserve.RoleAdmin} }})
	if g.cfg.cluster == nil {
		r.store.Store(srserve.NewStore(srserve.NewMemKV(), o.FirstID))
	}
	go r.syncLoop()
	return r
}

// SchemaRegistryHandler serves the Schema Registry REST API at its
// root; mount it under a prefix with http.StripPrefix, e.g.
//
//	mux.Handle("/schema-registry/", http.StripPrefix("/schema-registry", g.SchemaRegistryHandler()))
//
// and point clients at http://host:port/schema-registry. Without
// WithSchemaRegistry every request gets 404. In a cluster requests get
// 503 (error_code 50003) until the gateway has joined JetStream.
//
// Stability: experimental
func (g *Gateway) SchemaRegistryHandler() http.Handler {
	if g.sr == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", srserve.ContentType)
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error_code":40400,"message":"schema registry is not enabled on this gateway"}` + "\n"))
		})
	}
	return g.sr.public
}

// schemaRegistryCaller resolves the caller from a bearer token or the
// basic-auth password (Confluent clients send basic auth; the user
// name is ignored): the admin or dev token (or an AdminAuthorizer
// OK), else an app key. A minted app key skips the admin checks. App
// keys work here whether or not WithRequireAppKeys is set.
func (g *Gateway) schemaRegistryCaller(r *http.Request) srserve.Caller {
	tok := bearerFromRequest(r)
	if tok == "" {
		if _, pw, ok := r.BasicAuth(); ok {
			tok = pw
		}
	}
	if tok == "" {
		return srserve.Caller{}
	}
	if _, minted := parseAppKey(tok); !minted {
		switch g.adminRoleFromBearer(r.Context(), tok, r.Method, r.URL.Path) {
		case adminRoleAdmin:
			return srserve.Caller{Role: srserve.RoleAdmin}
		case adminRoleDev:
			return srserve.Caller{Role: srserve.RoleDev}
		}
	}
	if isAdminPublicMethod(r.Method) {
		return srserve.Caller{} // reads need no key; do not count them
	}
	grant, err := g.checkAppKey(r.Context(), tok, appKeyOpSchemaWrite, nil, r.RemoteAddr)
	if err != nil {
		g.recordAppKeyAuth(appKeyOpSchemaWrite, appKeyOutcome(err))
		return srserve.Caller{}
	}
	return srserve.Caller{Role: srserve.RoleApp, Owns: func(subject string) bool {
		ok := g.sr.ownedSubjects(grant.namespaces)[subject]
		outcome := "ok"
		if !ok {
			outcome = "namespace_forbidden"
		}
		g.recordAppKeyAuth(appKeyOpSchemaWrite, outcome)
		return ok
	}}
}

// ownedSubjects is the set of subjects that belong to the named
// channels of namespaces: each channel message's subject, and the
// subjects of the files it imports (by import path, as the sync and
// the serdes register them; well-known files excluded). An import
// file two namespaces share belongs to both.
func (r *schemaRegistry) ownedSubjects(namespaces []string) map[string]bool {
	out := map[string]bool{}
	idx := r.g.channelBindingIndex.Load()
	if idx == nil {
		return out
	}
	var imports func(fd protoreflect.FileDescriptor)
	imports = func(fd protoreflect.FileDescriptor) {
		imps := fd.Imports()
		for i := 0; i < imps.Len(); i++ {
			dep := imps.Get(i).FileDescriptor
			if sr.Builtin(dep.Path()) || out[dep.Path()] {
				continue
			}
			out[dep.Path()] = true
			imports(dep)
		}
	}
	for _, ch := range idx.channels {
		if !containsString(namespaces, ch.namespace) {
			continue
		}
		topic := r.o.Topics[ch.key]
		if topic == "" {
			topic = ch.streamName()
		}
		for _, t := range ch.types {
			md := ch.descs[t]
			if md == nil {
				continue
			}
			out[r.strategy(topic, md)] = true
			imports(md.ParentFile())
		}
	}
	return out
}

// openSchemasBucket opens (creating at R1) the bucket. No TTL; one
// value per key is enough (every write is a compare-and-set).
func openSchemasBucket(ctx context.Context, js jetstream.JetStream) (jetstream.KeyValue, error) {
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: schemasBucketName, Replicas: 1})
	if err != nil && !errors.Is(err, jetstream.ErrBucketExists) {
		return nil, fmt.Errorf("schemas bucket: %w", err)
	}
	if kv == nil {
		if kv, err = js.KeyValue(ctx, schemasBucketName); err != nil {
			return nil, fmt.Errorf("schemas bucket open: %w", err)
		}
	}
	return kv, nil
}

// bindCluster switches the registry to the cluster bucket.
func (r *schemaRegistry) bindCluster(kv jetstream.KeyValue) {
	if r == nil {
		return
	}
	r.store.Store(srserve.NewStore(srserve.JetStreamKV{KV: kv}, r.o.FirstID))
	r.mu.Lock()
	r.done = map[string]string{}
	r.mu.Unlock()
	r.trigger()
}

// trigger asks the sync loop to register the channels again.
func (r *schemaRegistry) trigger() {
	if r == nil {
		return
	}
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

const srSyncRetry = 2 * time.Second

func (r *schemaRegistry) syncLoop() {
	life := r.g.life
	var retry <-chan time.Time
	for {
		select {
		case <-life.Done():
			return
		case <-r.kick:
		case <-retry:
		}
		retry = nil
		if err := r.sync(life); err != nil && life.Err() == nil {
			r.g.srWarnf("schema registry: register channels: %v (retrying)", err)
			retry = time.After(srSyncRetry)
		}
	}
}

// handlerTransport sends a client's requests straight to a handler.
type handlerTransport struct{ h http.Handler }

func (t handlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	t.h.ServeHTTP(rec, req)
	return rec.Result(), nil
}

// sync registers every named channel's message files (and their
// imports) through the REST API, as the Go serde does.
func (r *schemaRegistry) sync(ctx context.Context) error {
	if r.store.Load() == nil {
		return nil // bindCluster triggers again
	}
	idx := r.g.channelBindingIndex.Load()
	if idx == nil {
		return nil
	}
	c, err := sr.NewClient(sr.Config{URL: "http://gwag.internal", HTTPClient: &http.Client{Transport: handlerTransport{r.internal}}})
	if err != nil {
		return err
	}
	var firstErr error
	for _, ch := range idx.channels {
		topic := r.o.Topics[ch.key]
		if topic == "" {
			topic = ch.streamName()
		}
		for _, t := range ch.types {
			md := ch.descs[t]
			if md == nil {
				continue
			}
			if err := r.register(ctx, c, r.strategy(topic, md), md.ParentFile()); err != nil && firstErr == nil {
				firstErr = fmt.Errorf("channel %s: %w", ch.key, err)
			}
		}
	}
	return firstErr
}

func (r *schemaRegistry) register(ctx context.Context, c *sr.Client, subject string, fd protoreflect.FileDescriptor) error {
	s, err := sr.SchemaOf(fd, nil)
	if err != nil {
		return err
	}
	r.mu.Lock()
	same := r.done[subject] == s.Schema
	r.mu.Unlock()
	if same {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := c.EnsureFile(cctx, subject, fd, sr.RegisterOptions{Register: true}); err != nil {
		return err
	}
	r.mu.Lock()
	r.done[subject] = s.Schema
	r.mu.Unlock()
	return nil
}

func (g *Gateway) srWarnf(format string, args ...any) {
	if cl := g.cfg.cluster; cl != nil && cl.Server != nil {
		cl.Server.Warnf("gateway: "+format, args...)
		return
	}
	log.Printf("gateway: "+format, args...)
}
