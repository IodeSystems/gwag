package v1

import (
	"fmt"
	"math"

	"github.com/iodesystems/gwag/v2/examples/versioning/internal/domain"
)

// The v1 <-> domain mapping rules. Each one is lossy in a known way;
// map_test.go pins them.
//
//   - amount: v1 dollars (float) <-> domain cents (int64). Output is
//     cents/100, exact for every cent value v1 can show. Input
//     (minAmount) rounds to the nearest cent, half away from zero.
//   - currency: v1 has none. v1 sees only USD orders: getOrder of
//     another currency is a 409 naming v2, listOrders leaves them out.
//   - customerName: renamed customer in v2; same value.
//   - status: v1 knows open and closed. A refunded order shows as
//     closed.
//   - refundedCents: added in v2; v1 does not show it.

// DefaultCurrency is the currency of every v1 order.
const DefaultCurrency = "USD"

// ToV1 maps a domain order to v1, or explains why v1 cannot show it.
func ToV1(o domain.Order) (Order, error) {
	if o.Currency != DefaultCurrency {
		return Order{}, fmt.Errorf("order %s is in %s; v1 shows only %s orders, use v2", o.ID, o.Currency, DefaultCurrency)
	}
	status := "open"
	if o.Status != domain.StatusOpen {
		status = "closed"
	}
	return Order{
		ID:           o.ID,
		Amount:       CentsToDollars(o.AmountCents),
		CustomerName: o.Customer,
		Status:       status,
	}, nil
}

// CentsToDollars is the v1 amount of c cents.
func CentsToDollars(c int64) float64 { return float64(c) / 100 }

// DollarsToCents rounds a v1 dollar amount to cents, half away from
// zero.
func DollarsToCents(d float64) int64 { return int64(math.Round(d * 100)) }

// DiscountCents is c reduced by percent, rounded to the cent half up,
// in integers. The legacy float code rounded some exact half cents
// down (0.29 at 50% gave 0.14, now 0.15); TestDiscountAgainstLegacy
// counts those cases.
func DiscountCents(c int64, percent int32) int64 {
	n := c * int64(100-percent)
	return (n + 50) / 100
}
