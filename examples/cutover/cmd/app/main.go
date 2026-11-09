// Command app is the upstream side of the cutover example: an OpenAPI
// service registered with both clusters, and an event producer and
// consumer that start on blue and move to green.
//
// Ports: -listen (the OpenAPI service the gateways call) and -control
// (the script's API: GET /status, POST /register-green,
// POST /producer/stop, POST /producer/start?on=green,
// POST /consumer/move?to=green, POST /deregister?cluster=blue).
//
// The service registers spec blue.json with blue at start and
// green.json with green on POST /register-green (once green runs). They differ in one operation: getItem is /items/{id} on blue
// and /v2/items/{id} on green (the next spec revision). The service
// answers both paths the same, so replay verification shows a path
// diff on GetItem and a match on the other operations.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	natsjs "github.com/nats-io/nats.go/jetstream"

	"github.com/iodesystems/gwag/v2/events"
	"github.com/iodesystems/gwag/v2/events/jetstream"
	ordersv1 "github.com/iodesystems/gwag/v2/examples/events/gen/orders/v1"
	"github.com/iodesystems/gwag/v2/gw/controlclient"
)

//go:embed blue.json
var blueSpec []byte

//go:embed green.json
var greenSpec []byte

const (
	stream = "EV_orders_v1_OrderShipped"
	group  = "notify"
)

func main() {
	listen := flag.String("listen", ":9000", "OpenAPI service listen address")
	control := flag.String("control", ":9001", "control API listen address")
	self := flag.String("self", "http://app:9000", "base URL the gateways call")
	blueCP := flag.String("blue-cp", "blue-0:50090,blue-1:50090,blue-2:50090", "blue control planes")
	greenCP := flag.String("green-cp", "green-0:50090,green-1:50090,green-2:50090", "green control planes")
	blueNATS := flag.String("blue-nats", "nats://blue-0:14222,nats://blue-1:14222,nats://blue-2:14222", "blue NATS")
	greenNATS := flag.String("green-nats", "nats://green-0:14222,nats://green-1:14222,nats://green-2:14222", "green NATS")
	rate := flag.Duration("publish-every", 50*time.Millisecond, "time between two events")
	flag.Parse()

	a := &app{natsURL: map[string]string{"blue": *blueNATS, "green": *greenNATS}, regs: map[string]*controlclient.Registration{}, seen: map[string]int{}, every: *rate}
	go func() { log.Fatal(http.ListenAndServe(*listen, a.service())) }()
	a.register("blue", *blueCP, blueSpec, *self)
	a.registerGreen = func() { a.register("green", *greenCP, greenSpec, *self) }
	if err := a.startConsumer("blue"); err != nil {
		log.Fatalf("consumer on blue: %v", err)
	}
	if err := a.startProducer("blue"); err != nil {
		log.Fatalf("producer on blue: %v", err)
	}
	log.Printf("app: service %s, control %s", *listen, *control)
	log.Fatal(http.ListenAndServe(*control, a.controlMux()))
}

type app struct {
	registerGreen func()
	greenOnce     sync.Once

	natsURL map[string]string
	every   time.Duration
	notes   atomic.Int64

	mu       sync.Mutex
	regs     map[string]*controlclient.Registration
	conns    map[string]*nats.Conn
	prodOn   string
	prodStop context.CancelFunc
	prodDone chan struct{}
	produced map[string]int
	nextID   int
	consOn   string
	consStop func()
	seen     map[string]int // event id -> deliveries
	consumed map[string]int // per cluster
}

// service is the OpenAPI upstream: both spec revisions' paths.
func (a *app) service() http.Handler {
	mux := http.NewServeMux()
	items := []map[string]any{{"id": "1", "name": "anvil"}, {"id": "2", "name": "rope"}, {"id": "3", "name": "lamp"}}
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("GET /items", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, items) })
	item := func(w http.ResponseWriter, r *http.Request) {
		for _, it := range items {
			if it["id"] == r.PathValue("id") {
				writeJSON(w, it)
				return
			}
		}
		http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
	}
	mux.HandleFunc("GET /items/{id}", item)
	mux.HandleFunc("GET /v2/items/{id}", item)
	mux.HandleFunc("POST /notes", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		a.notes.Add(1)
		writeJSON(w, map[string]any{"text": in.Text, "accepted": true})
	})
	return mux
}

func (a *app) register(cluster, addrs string, spec []byte, self string) {
	go func() {
		reg, err := controlclient.SelfRegister(context.Background(), controlclient.Options{
			GatewayAddrs: strings.Split(addrs, ","),
			ServiceAddr:  self,
			InstanceID:   "app@" + cluster,
			Services:     []controlclient.Service{{Namespace: "items", Version: "v1", OpenAPISpec: spec}},
			Logger:       func(f string, args ...any) { log.Printf(cluster+": "+f, args...) },
		})
		if err != nil {
			log.Printf("register with %s: %v", cluster, err)
			return
		}
		a.mu.Lock()
		a.regs[cluster] = reg
		a.mu.Unlock()
		log.Printf("registered items/v1 with %s (%s)", cluster, reg.GatewayAddr())
	}()
}

func (a *app) conn(cluster string) (*nats.Conn, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.conns == nil {
		a.conns = map[string]*nats.Conn{}
	}
	if nc := a.conns[cluster]; nc != nil {
		return nc, nil
	}
	var nc *nats.Conn
	var err error
	for i := 0; i < 60; i++ {
		nc, err = nats.Connect(a.natsURL[cluster], nats.MaxReconnects(-1))
		if err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		return nil, err
	}
	a.conns[cluster] = nc
	return nc, nil
}

func (a *app) bus(cluster string) (*ordersv1.Bus, error) {
	nc, err := a.conn(cluster)
	if err != nil {
		return nil, err
	}
	b, err := jetstream.New(nc, jetstream.WithPrefetch(1))
	if err != nil {
		return nil, err
	}
	return ordersv1.NewBus(b), nil
}

func (a *app) startProducer(cluster string) error {
	bus, err := a.bus(cluster)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.prodStop != nil {
		return errors.New("producer is running")
	}
	if a.produced == nil {
		a.produced = map[string]int{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	a.prodOn, a.prodStop, a.prodDone = cluster, cancel, done
	go func() {
		defer close(done)
		t := time.NewTicker(a.every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			a.mu.Lock()
			id := "o-" + strconv.Itoa(a.nextID+1)
			a.mu.Unlock()
			// Retry until stored: a publish that errors is retried with
			// the same Nats-Msg-Id, so it is stored once.
			for {
				pctx, pcancel := context.WithTimeout(context.Background(), 5*time.Second)
				err := bus.PublishOrderShipped(pctx, ordersv1.OrderShippedSubject{OrderId: id}, &ordersv1.OrderShipped{OrderId: id, Carrier: "ups"}, events.MsgID(id))
				pcancel()
				if err == nil {
					break
				}
				log.Printf("publish %s on %s: %v (retrying)", id, cluster, err)
				time.Sleep(200 * time.Millisecond)
			}
			a.mu.Lock()
			a.nextID++
			a.produced[cluster]++
			a.mu.Unlock()
		}
	}()
	return nil
}

func (a *app) stopProducer() {
	a.mu.Lock()
	stop, done := a.prodStop, a.prodDone
	a.prodStop, a.prodDone, a.prodOn = nil, nil, ""
	a.mu.Unlock()
	if stop != nil {
		stop()
		<-done
	}
}

func (a *app) startConsumer(cluster string) error {
	bus, err := a.bus(cluster)
	if err != nil {
		return err
	}
	stop, err := bus.OnOrderShipped(context.Background(), group, func(ctx context.Context, s ordersv1.OrderShippedSubject, m *ordersv1.OrderShipped) error {
		a.mu.Lock()
		a.seen[s.OrderId]++
		if a.consumed == nil {
			a.consumed = map[string]int{}
		}
		a.consumed[cluster]++
		a.mu.Unlock()
		return nil
	})
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.consOn, a.consStop = cluster, stop
	a.mu.Unlock()
	return nil
}

// pending is the consumer's messages not yet delivered or not yet
// acked, on the cluster it runs on.
func (a *app) pending(ctx context.Context) (uint64, error) {
	a.mu.Lock()
	cluster := a.consOn
	a.mu.Unlock()
	nc, err := a.conn(cluster)
	if err != nil {
		return 0, err
	}
	js, err := natsjs.New(nc)
	if err != nil {
		return 0, err
	}
	c, err := js.Consumer(ctx, stream, group)
	if err != nil {
		return 0, err
	}
	info, err := c.Info(ctx)
	if err != nil {
		return 0, err
	}
	return info.NumPending + uint64(info.NumAckPending), nil
}

func (a *app) streamMsgs(ctx context.Context, cluster string) (uint64, error) {
	nc, err := a.conn(cluster)
	if err != nil {
		return 0, err
	}
	js, err := natsjs.New(nc)
	if err != nil {
		return 0, err
	}
	s, err := js.Stream(ctx, stream)
	if err != nil {
		return 0, err
	}
	info, err := s.Info(ctx)
	if err != nil {
		return 0, err
	}
	return info.State.Msgs, nil
}

func (a *app) controlMux() http.Handler {
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, v any, err error) {
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		pend, perr := a.pending(ctx)
		a.mu.Lock()
		out := map[string]any{
			"producerOn": a.prodOn, "consumerOn": a.consOn,
			"produced": a.produced, "producedTotal": a.nextID,
			"consumed": a.consumed, "unique": len(a.seen),
			"notes": a.notes.Load(),
		}
		dups, missing := 0, 0
		for _, n := range a.seen {
			dups += n - 1
		}
		for i := 1; i <= a.nextID; i++ {
			if a.seen["o-"+strconv.Itoa(i)] == 0 {
				missing++
			}
		}
		regs := []string{}
		for c := range a.regs {
			regs = append(regs, c)
		}
		a.mu.Unlock()
		out["duplicates"], out["missing"], out["registeredWith"] = dups, missing, regs
		if perr != nil {
			out["pendingError"] = perr.Error()
		} else {
			out["pending"] = pend
		}
		streams := map[string]any{}
		// ?streams=blue,green: message count of each cluster's stream.
		for _, c := range strings.Split(r.URL.Query().Get("streams"), ",") {
			if c == "" {
				continue
			}
			sctx, scancel := context.WithTimeout(r.Context(), 3*time.Second)
			n, err := a.streamMsgs(sctx, c)
			scancel()
			if err != nil {
				streams[c] = err.Error()
			} else {
				streams[c] = n
			}
		}
		if len(streams) > 0 {
			out["streamMsgs"] = streams
		}
		reply(w, out, nil)
	})
	mux.HandleFunc("POST /producer/stop", func(w http.ResponseWriter, r *http.Request) {
		a.stopProducer()
		reply(w, map[string]string{"producer": "stopped"}, nil)
	})
	mux.HandleFunc("POST /producer/start", func(w http.ResponseWriter, r *http.Request) {
		on := r.URL.Query().Get("on")
		reply(w, map[string]string{"producer": on}, a.startProducer(on))
	})
	mux.HandleFunc("POST /consumer/move", func(w http.ResponseWriter, r *http.Request) {
		to := r.URL.Query().Get("to")
		a.mu.Lock()
		stop := a.consStop
		a.consStop, a.consOn = nil, ""
		a.mu.Unlock()
		if stop != nil {
			stop()
		}
		reply(w, map[string]string{"consumer": to}, a.startConsumer(to))
	})
	mux.HandleFunc("POST /register-green", func(w http.ResponseWriter, r *http.Request) {
		a.greenOnce.Do(a.registerGreen)
		reply(w, map[string]string{"registering": "green"}, nil)
	})
	mux.HandleFunc("POST /deregister", func(w http.ResponseWriter, r *http.Request) {
		c := r.URL.Query().Get("cluster")
		a.mu.Lock()
		reg := a.regs[c]
		delete(a.regs, c)
		a.mu.Unlock()
		if reg == nil {
			reply(w, nil, fmt.Errorf("not registered with %q", c))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		reply(w, map[string]string{"deregistered": c}, reg.Close(ctx))
	})
	return mux
}
