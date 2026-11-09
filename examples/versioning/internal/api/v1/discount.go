package v1

import (
	"context"
	"fmt"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

// applyDiscount was removed in v2: prices are final once an order is
// placed, and money goes back through refundOrder. v1 clients still
// call it, so it stays here, written against the domain, until its
// sunset date. After that date it answers 410 Gone with the reason,
// so a client that missed the notice gets a message, not a 404.
//
// The date is a flag of the service (-discount-sunset), not a
// constant: the date moves when a client asks for more time, and
// moving it must not need a release.
func (a *API) discount(_ context.Context, in *discountIn) (*orderOut, error) {
	now := time.Now
	if a.Now != nil {
		now = a.Now
	}
	if !a.DiscountSunset.IsZero() && !now().Before(a.DiscountSunset) {
		return nil, huma.Error410Gone(fmt.Sprintf(
			"applyDiscount was removed on %s; orders v2 has no discounts, use refundOrder",
			a.DiscountSunset.Format(time.DateOnly)))
	}
	o, err := a.Store.Get(in.ID)
	if err != nil {
		return nil, mapErr(in.ID, err)
	}
	v, err := ToV1(o)
	if err != nil {
		return nil, huma.Error409Conflict(err.Error())
	}
	if v.Status != "open" {
		return nil, huma.Error409Conflict(fmt.Sprintf("order %s is %s", in.ID, v.Status))
	}
	o, err = a.Store.SetAmount(in.ID, DiscountCents(o.AmountCents, in.Body.Percent))
	if err != nil {
		// Open with a partial refund (v2 only): v1 has no word for it.
		return nil, huma.Error409Conflict(fmt.Sprintf("order %s has a refund and cannot be discounted", in.ID))
	}
	v, _ = ToV1(o)
	return &orderOut{Body: v}, nil
}
