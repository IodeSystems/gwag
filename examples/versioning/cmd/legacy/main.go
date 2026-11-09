// Command legacy is the orders service as it was before v2: v1 only,
// float amounts, its own store. It is kept to record the v1 contract
// goldens (testdata/contract/v1) and the v1 OpenAPI spec once, so the
// rewrite of v1 as an adapter over the domain can be checked against
// what clients saw. It is not deployed. Delete it with v1.
//
//	go run ./examples/versioning/cmd/legacy -record DIR   # write goldens
//	go run ./examples/versioning/cmd/legacy -spec FILE    # write the v1 OpenAPI spec
//	go run ./examples/versioning/cmd/legacy -listen :8090 # serve it
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"sync"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/iodesystems/gwag/v2/examples/versioning/internal/cases"
	"github.com/iodesystems/gwag/v2/gw/contract"
	"github.com/iodesystems/gwag/v2/gw/gat"
)

type Order struct {
	ID           string  `json:"id"`
	Amount       float64 `json:"amount" doc:"Order total in US dollars."`
	CustomerName string  `json:"customerName"`
	Status       string  `json:"status" enum:"open,closed"`
}

type store struct {
	mu     sync.Mutex
	orders map[string]Order
}

func newStore() *store {
	return &store{orders: map[string]Order{
		"o-1": {ID: "o-1", Amount: 12.5, CustomerName: "Ada Lovelace", Status: "open"},
		"o-2": {ID: "o-2", Amount: 9.99, CustomerName: "Grace Hopper", Status: "closed"},
	}}
}

type getIn struct {
	ID string `path:"id"`
}
type orderOut struct{ Body Order }

type listIn struct {
	MinAmount float64 `query:"minAmount" doc:"Only orders of at least this many dollars."`
}
type listOut struct {
	Body struct {
		Orders []Order `json:"orders"`
	}
}

type discountIn struct {
	ID   string `path:"id"`
	Body struct {
		Percent int32 `json:"percent" minimum:"1" maximum:"100"`
	}
}

func register(api huma.API, g *gat.Gateway, s *store) {
	gat.Register(api, g, huma.Operation{
		OperationID: "getOrder", Method: http.MethodGet, Path: "/v1/orders/{id}",
	}, func(_ context.Context, in *getIn) (*orderOut, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		o, ok := s.orders[in.ID]
		if !ok {
			return nil, huma.Error404NotFound(fmt.Sprintf("order %s not found", in.ID))
		}
		return &orderOut{Body: o}, nil
	})
	gat.Register(api, g, huma.Operation{
		OperationID: "listOrders", Method: http.MethodGet, Path: "/v1/orders",
	}, func(_ context.Context, in *listIn) (*listOut, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		out := &listOut{}
		out.Body.Orders = []Order{}
		for _, o := range s.orders {
			if o.Amount >= in.MinAmount {
				out.Body.Orders = append(out.Body.Orders, o)
			}
		}
		sort.Slice(out.Body.Orders, func(i, j int) bool { return out.Body.Orders[i].ID < out.Body.Orders[j].ID })
		return out, nil
	})
	gat.Register(api, g, huma.Operation{
		OperationID: "applyDiscount", Method: http.MethodPost, Path: "/v1/orders/{id}/discount",
	}, func(_ context.Context, in *discountIn) (*orderOut, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		o, ok := s.orders[in.ID]
		if !ok {
			return nil, huma.Error404NotFound(fmt.Sprintf("order %s not found", in.ID))
		}
		if o.Status != "open" {
			return nil, huma.Error409Conflict(fmt.Sprintf("order %s is %s", in.ID, o.Status))
		}
		o.Amount = math.Round(o.Amount*float64(100-in.Body.Percent)) / 100
		s.orders[in.ID] = o
		return &orderOut{Body: o}, nil
	})
}

func newHandler() (http.Handler, huma.API, error) {
	mux := http.NewServeMux()
	cfg := huma.DefaultConfig("orders", "1.0.0")
	cfg.OpenAPIPath = "/v1/openapi"
	cfg.SchemasPath = "/v1/schemas"
	cfg.DocsPath = ""
	cfg.CreateHooks = nil // no $schema links in bodies
	api := humago.New(mux, cfg)
	g, err := gat.New()
	if err != nil {
		return nil, nil, err
	}
	register(api, g, newStore())
	if err := gat.RegisterHuma(api, g, "/api"); err != nil {
		return nil, nil, err
	}
	return mux, api, nil
}

func main() {
	listen := flag.String("listen", "", "serve on this address")
	record := flag.String("record", "", "record the v1 contract goldens into this directory and exit")
	spec := flag.String("spec", "", "write the v1 OpenAPI spec to this file and exit")
	flag.Parse()

	h, api, err := newHandler()
	if err != nil {
		log.Fatal(err)
	}
	switch {
	case *record != "":
		srv := httptest.NewServer(h)
		defer srv.Close()
		if err := os.MkdirAll(*record, 0o755); err != nil {
			log.Fatal(err)
		}
		if err := contract.Record(srv.URL, *record, cases.V1()); err != nil {
			log.Fatal(err)
		}
	case *spec != "":
		b, err := json.Marshal(api.OpenAPI())
		if err != nil {
			log.Fatal(err)
		}
		if err := os.WriteFile(*spec, b, 0o644); err != nil {
			log.Fatal(err)
		}
	case *listen != "":
		log.Printf("legacy orders v1 on %s", *listen)
		log.Fatal(http.ListenAndServe(*listen, h))
	default:
		flag.Usage()
		os.Exit(2)
	}
}
