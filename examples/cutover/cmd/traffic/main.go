// Command traffic is the client side of the cutover example: -clients
// clients call the GraphQL endpoint through the load balancer in a
// loop and count every failed request.
//
// A request fails on a transport error, a status other than 200, a
// body that is not JSON, or a response with GraphQL errors or no data.
// Each client sends X-Client-Id (the switchover sticky header).
//
// GET /stats (on -control) returns the counts so far, per operation
// and per load balancer cluster (the x-gw-cluster response header),
// and the last failures. POST /stop ends the loop; /stats keeps
// answering.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

type op struct {
	name, body string
}

var ops = []op{
	{"ListItems", `{"operationName":"ListItems","query":"query ListItems { items { listItems { id name } } }"}`},
	{"GetItem", `{"operationName":"GetItem","query":"query GetItem($id: String!) { items { getItem(id: $id) { id name } } }","variables":{"id":"2"}}`},
	{"AddNote", `{"operationName":"AddNote","query":"mutation AddNote { items { addNote(body: {text: \"hi\"}) { accepted } } }"}`},
}

type failure struct {
	At     time.Time `json:"at"`
	Client string    `json:"client"`
	Op     string    `json:"op"`
	Error  string    `json:"error"`
}

type stats struct {
	mu        sync.Mutex
	Total     int64            `json:"total"`
	OK        int64            `json:"ok"`
	Failed    int64            `json:"failed"`
	ByOp      map[string]int64 `json:"byOp"`
	ByCluster map[string]int64 `json:"byCluster"`
	Failures  []failure        `json:"lastFailures"`
}

func main() {
	target := flag.String("target", "http://envoy:10000/api/graphql", "GraphQL URL")
	clients := flag.Int("clients", 20, "concurrent clients")
	every := flag.Duration("every", 100*time.Millisecond, "pause between two requests of one client")
	control := flag.String("control", ":9100", "stats listen address")
	flag.Parse()

	st := &stats{ByOp: map[string]int64{}, ByCluster: map[string]int64{}}
	ctx, cancel := context.WithCancel(context.Background())
	var stopped atomic.Bool
	hc := &http.Client{Timeout: 30 * time.Second}

	var wg sync.WaitGroup
	for i := 0; i < *clients; i++ {
		id := fmt.Sprintf("c-%02d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; ctx.Err() == nil; n++ {
				o := ops[n%len(ops)]
				cluster, err := call(ctx, hc, *target, id, o)
				if ctx.Err() != nil && err != nil {
					return // stopped mid-request
				}
				st.add(id, o.name, cluster, err)
				select {
				case <-ctx.Done():
				case <-time.After(*every):
				}
			}
		}()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(st)
	})
	mux.HandleFunc("POST /stop", func(w http.ResponseWriter, r *http.Request) {
		if stopped.CompareAndSwap(false, true) {
			cancel()
			wg.Wait()
		}
		fmt.Fprintln(w, "stopped")
	})
	log.Printf("traffic: %d clients -> %s", *clients, *target)
	log.Fatal(http.ListenAndServe(*control, mux))
}

func call(ctx context.Context, hc *http.Client, target, client string, o op) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader([]byte(o.body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Client-Id", client)
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	cluster := resp.Header.Get("X-Gw-Cluster")
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return cluster, err
	}
	if resp.StatusCode != http.StatusOK {
		return cluster, fmt.Errorf("HTTP %d: %.200s", resp.StatusCode, b)
	}
	var out struct {
		Data   map[string]any    `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return cluster, fmt.Errorf("body: %v: %.200s", err, b)
	}
	if len(out.Errors) > 0 || out.Data == nil {
		return cluster, fmt.Errorf("graphql: %.300s", b)
	}
	return cluster, nil
}

func (s *stats) add(client, op, cluster string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Total++
	s.ByOp[op]++
	if cluster == "" {
		cluster = "(none)"
	}
	s.ByCluster[cluster]++
	if err == nil {
		s.OK++
		return
	}
	s.Failed++
	s.Failures = append(s.Failures, failure{At: time.Now().UTC(), Client: client, Op: op, Error: err.Error()})
	if len(s.Failures) > 50 {
		s.Failures = s.Failures[1:]
	}
	log.Printf("FAIL %s %s: %v", client, op, err)
}
