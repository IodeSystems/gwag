// Package v1 is the frozen orders v1 API, kept for clients that cannot
// move yet. It holds no logic of its own: each handler maps the v1
// request onto the domain, calls the store, and maps the domain result
// back to v1's wire types. The one exception is applyDiscount, an
// operation v2 removed; it is v1-only code over the domain with a
// sunset date (discount.go).
//
// v1 imports domain, never v2. When v3 ships, v2 becomes a package
// like this one, also over domain, and v1 does not change.
//
// The wire types and operation ids must not change: the committed
// snapshot api/orders/v1/openapi.json and the contract goldens in
// testdata/contract/v1 fail the build if they do.
package v1

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/iodesystems/gwag/v2/examples/versioning/internal/domain"
	"github.com/iodesystems/gwag/v2/gw/gat"
)

// Order is the v1 wire type, as the legacy service had it.
type Order struct {
	ID           string  `json:"id"`
	Amount       float64 `json:"amount" doc:"Order total in US dollars."`
	CustomerName string  `json:"customerName"`
	Status       string  `json:"status" enum:"open,closed"`
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

// API is the v1 adapter over a store.
type API struct {
	Store *domain.Store
	// DiscountSunset is when applyDiscount stops working. Zero: never.
	DiscountSunset time.Time
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

func (a *API) get(_ context.Context, in *getIn) (*orderOut, error) {
	o, err := a.Store.Get(in.ID)
	if err != nil {
		return nil, mapErr(in.ID, err)
	}
	v, err := ToV1(o)
	if err != nil {
		return nil, huma.Error409Conflict(err.Error())
	}
	return &orderOut{Body: v}, nil
}

func (a *API) list(_ context.Context, in *listIn) (*listOut, error) {
	out := &listOut{}
	out.Body.Orders = []Order{}
	f := domain.Filter{MinAmountCents: DollarsToCents(in.MinAmount), Currency: DefaultCurrency}
	for _, o := range a.Store.List(f) {
		v, err := ToV1(o)
		if err != nil {
			continue // not expressible in v1; see ToV1
		}
		out.Body.Orders = append(out.Body.Orders, v)
	}
	return out, nil
}

// mapErr gives domain errors the status and text the legacy service
// returned.
func mapErr(id string, err error) error {
	if errors.Is(err, domain.ErrNotFound) {
		return huma.Error404NotFound(fmt.Sprintf("order %s not found", id))
	}
	return err
}

// Register adds the v1 operations to api (a huma API of its own, the v1
// OpenAPI document) and to g.
func Register(api huma.API, g *gat.Gateway, a *API) {
	gat.Register(api, g, huma.Operation{
		OperationID: "getOrder", Method: http.MethodGet, Path: "/v1/orders/{id}",
	}, a.get)
	gat.Register(api, g, huma.Operation{
		OperationID: "listOrders", Method: http.MethodGet, Path: "/v1/orders",
	}, a.list)
	gat.Register(api, g, huma.Operation{
		OperationID: "applyDiscount", Method: http.MethodPost, Path: "/v1/orders/{id}/discount",
	}, a.discount)
}
