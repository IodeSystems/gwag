// Package server wires the orders service: one store, one huma API per
// version on one mux, one gat gateway over both.
//
//	/v1/...           v1 REST (adapter)          /v1/openapi  v1 spec
//	/v2/...           v2 REST                    /v2/openapi  v2 spec
//	/api/graphql      one GraphQL schema, both versions
//	/api/grpc/...     orders.v1.* and orders.v2.* over Connect / gRPC
//	/api/schema/...   schema views, ?version=v1|v2
package server

import (
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/iodesystems/gwag/v2/examples/versioning/internal/api/v1"
	"github.com/iodesystems/gwag/v2/examples/versioning/internal/api/v2"
	"github.com/iodesystems/gwag/v2/examples/versioning/internal/domain"
	"github.com/iodesystems/gwag/v2/gw/gat"
)

// Options configures New.
type Options struct {
	// NoV1 builds the service without v1, as it will be after v1 is
	// deleted.
	NoV1 bool
	// DiscountSunset is when v1 applyDiscount starts answering 410.
	DiscountSunset time.Time
	// Now is the clock of v1 applyDiscount; nil is time.Now.
	Now func() time.Time
}

// Service is a built orders service.
type Service struct {
	Handler http.Handler
	Gat     *gat.Gateway
	Store   *domain.Store
}

func humaAPI(mux *http.ServeMux, version, apiVersion string) huma.API {
	cfg := huma.DefaultConfig("orders", apiVersion)
	cfg.OpenAPIPath = "/" + version + "/openapi"
	cfg.SchemasPath = "/" + version + "/schemas"
	cfg.DocsPath = ""
	cfg.CreateHooks = nil // no $schema links in bodies (the legacy service had none)
	return humago.New(mux, cfg)
}

// New builds the service over a fresh seeded store.
func New(o Options) (*Service, error) {
	mux := http.NewServeMux()
	g, err := gat.New()
	if err != nil {
		return nil, err
	}
	store := domain.NewStore()

	apiV2 := humaAPI(mux, "v2", "2.0.0")
	v2.Register(apiV2, g, &v2.API{Store: store})
	if !o.NoV1 {
		apiV1 := humaAPI(mux, "v1", "1.0.0")
		v1.Register(apiV1, g, &v1.API{Store: store, DiscountSunset: o.DiscountSunset, Now: o.Now})
		if err := gat.RegisterHumaVersion(apiV1, g, "v1"); err != nil {
			return nil, err
		}
	}
	if err := gat.RegisterHumaVersion(apiV2, g, "v2"); err != nil {
		return nil, err
	}
	if err := gat.RegisterHuma(apiV2, g, "/api"); err != nil {
		return nil, err
	}
	if err := gat.RegisterGRPC(mux, g, "/api/grpc"); err != nil {
		return nil, err
	}
	return &Service{Handler: mux, Gat: g, Store: store}, nil
}
