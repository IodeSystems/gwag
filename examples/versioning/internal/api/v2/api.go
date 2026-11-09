// Package v2 is the current orders API. Its handlers do the work: they
// map v2 wire types to and from the domain and call the store.
package v2

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/iodesystems/gwag/v2/examples/versioning/internal/domain"
	"github.com/iodesystems/gwag/v2/gw/gat"
)

// Cents is money in integer minor units.
type Cents int64

func (Cents) Schema(huma.Registry) *huma.Schema {
	return gat.IntegerScalar("Cents", "Money in integer minor units (cents) of the order's currency.")
}

// Order is the v2 wire type.
type Order struct {
	ID            string `json:"id"`
	AmountCents   Cents  `json:"amountCents"`
	Currency      string `json:"currency" doc:"ISO 4217 code."`
	Customer      string `json:"customer"`
	Status        string `json:"status" enum:"open,closed,refunded"`
	RefundedCents Cents  `json:"refundedCents"`
}

// FromDomain maps a domain order to v2. v2 shows every field.
func FromDomain(o domain.Order) Order {
	return Order{
		ID:            o.ID,
		AmountCents:   Cents(o.AmountCents),
		Currency:      o.Currency,
		Customer:      o.Customer,
		Status:        string(o.Status),
		RefundedCents: Cents(o.RefundedCents),
	}
}

type GetIn struct {
	ID string `path:"id"`
}
type OrderOut struct{ Body Order }

type ListIn struct {
	MinAmountCents Cents  `query:"minAmountCents"`
	Currency       string `query:"currency"`
}
type ListOut struct {
	Body struct {
		Orders []Order `json:"orders"`
	}
}

type RefundIn struct {
	ID   string `path:"id"`
	Body struct {
		AmountCents Cents `json:"amountCents" minimum:"1"`
	}
}

// API is the v2 handlers over a store.
type API struct {
	Store *domain.Store
}

func (a *API) Get(_ context.Context, in *GetIn) (*OrderOut, error) {
	o, err := a.Store.Get(in.ID)
	if err != nil {
		return nil, mapErr(in.ID, err)
	}
	return &OrderOut{Body: FromDomain(o)}, nil
}

func (a *API) List(_ context.Context, in *ListIn) (*ListOut, error) {
	out := &ListOut{}
	out.Body.Orders = []Order{}
	for _, o := range a.Store.List(domain.Filter{MinAmountCents: int64(in.MinAmountCents), Currency: in.Currency}) {
		out.Body.Orders = append(out.Body.Orders, FromDomain(o))
	}
	return out, nil
}

func (a *API) Refund(_ context.Context, in *RefundIn) (*OrderOut, error) {
	o, err := a.Store.Refund(in.ID, int64(in.Body.AmountCents))
	if err != nil {
		return nil, mapErr(in.ID, err)
	}
	return &OrderOut{Body: FromDomain(o)}, nil
}

func mapErr(id string, err error) error {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return huma.Error404NotFound(fmt.Sprintf("order %s not found", id))
	case errors.Is(err, domain.ErrInvalid):
		return huma.Error409Conflict(err.Error())
	}
	return err
}

// Register adds the v2 operations to api (a huma API of its own, the
// v2 OpenAPI document) and to g.
func Register(api huma.API, g *gat.Gateway, a *API) {
	gat.Register(api, g, huma.Operation{
		OperationID: "getOrder", Method: http.MethodGet, Path: "/v2/orders/{id}",
	}, a.Get)
	gat.Register(api, g, huma.Operation{
		OperationID: "listOrders", Method: http.MethodGet, Path: "/v2/orders",
	}, a.List)
	gat.Register(api, g, huma.Operation{
		OperationID: "refundOrder", Method: http.MethodPost, Path: "/v2/orders/{id}/refund",
	}, a.Refund)
}
