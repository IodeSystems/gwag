package v1

import (
	"math"
	"strings"
	"testing"

	"github.com/iodesystems/gwag/v2/examples/versioning/internal/domain"
)

// Every cent amount survives domain -> v1 -> domain.
func TestAmountRoundTrip(t *testing.T) {
	for c := int64(0); c <= 1_000_000; c++ {
		if got := DollarsToCents(CentsToDollars(c)); got != c {
			t.Fatalf("%d cents -> %v dollars -> %d cents", c, CentsToDollars(c), got)
		}
	}
}

// v1 input rounds to the nearest cent, half away from zero.
func TestDollarsToCentsRounds(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want int64
	}{{10, 1000}, {9.99, 999}, {0.004, 0}, {0.005, 1}, {12.345, 1235}} {
		if got := DollarsToCents(tc.in); got != tc.want {
			t.Errorf("DollarsToCents(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// legacyDiscount is the float formula of the legacy service.
func legacyDiscount(dollars float64, percent int32) float64 {
	return math.Round(dollars*float64(100-percent)) / 100
}

// The integer discount matches the legacy float code except at exact
// half cents, where float error made the legacy code round down. v1
// now rounds those up. This is a known change of a frozen version,
// kept on purpose: the goldens do not hit it, and the count below
// fails the test if the rule changes again.
func TestDiscountAgainstLegacy(t *testing.T) {
	diff, n := 0, 0
	for c := int64(0); c <= 100_000; c++ {
		for p := int32(1); p <= 100; p++ {
			n++
			if CentsToDollars(DiscountCents(c, p)) != legacyDiscount(CentsToDollars(c), p) {
				diff++
			}
		}
	}
	if diff != 7432 {
		t.Errorf("%d of %d (amount, percent) pairs differ from legacy, want 7432", diff, n)
	}
	if got, legacy := CentsToDollars(DiscountCents(29, 50)), legacyDiscount(0.29, 50); got != 0.15 || legacy != 0.14 {
		t.Errorf("0.29 at 50%%: v1 %v (want 0.15), legacy %v (want 0.14)", got, legacy)
	}
}

func TestToV1(t *testing.T) {
	o := domain.Order{ID: "o-9", AmountCents: 500, Currency: "USD", Customer: "Ann", Status: domain.StatusRefunded, RefundedCents: 500}
	v, err := ToV1(o)
	if err != nil {
		t.Fatal(err)
	}
	if want := (Order{ID: "o-9", Amount: 5, CustomerName: "Ann", Status: "closed"}); v != want {
		t.Errorf("ToV1 = %+v, want %+v (refunded shows as closed)", v, want)
	}

	o.Currency = "EUR"
	if _, err := ToV1(o); err == nil || !strings.Contains(err.Error(), "use v2") {
		t.Errorf("ToV1 of a EUR order: err %v, want one naming v2", err)
	}
}
