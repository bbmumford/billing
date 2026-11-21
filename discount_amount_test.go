package billing

import (
	"math"
	"testing"
)

// The amount a discount takes off is integer minor units, never below zero and never above the
// subtotal: a negative or not-a-number value takes nothing off, a percentage is the subtotal's
// share rounded to the nearest unit, a half up, and a fixed amount is rounded the same way.
func TestComputeDiscountAmountCents_IntegerUnitsClampedToTheSubtotal(t *testing.T) {
	for _, c := range []struct {
		name     string
		d        *Discount
		subtotal int64
		want     int64
	}{
		{"ten percent", &Discount{Type: "percentage", Value: 10}, 1000, 100},
		{"a third, rounded to the nearest unit", &Discount{Type: "percentage", Value: 33.3}, 999, 333},
		{"a half unit rounds up", &Discount{Type: "percentage", Value: 50}, 1, 1},
		{"under a half unit rounds down", &Discount{Type: "percentage", Value: 10}, 4, 0},
		{"negative percentage", &Discount{Type: "percentage", Value: -50}, 1000, 0},
		{"over a hundred percent", &Discount{Type: "percentage", Value: 150}, 1000, 1000},
		{"not-a-number percentage", &Discount{Type: "percentage", Value: math.NaN()}, 1000, 0},
		{"infinite percentage", &Discount{Type: "percentage", Value: math.Inf(1)}, 1000, 1000},
		{"fixed", &Discount{Type: "fixed_amount", Value: 300}, 1000, 300},
		{"fixed over the subtotal", &Discount{Type: "fixed_amount", Value: 5000}, 1000, 1000},
		{"negative fixed", &Discount{Type: "fixed_amount", Value: -250}, 1000, 0},
		{"fixed fraction rounds to the nearest unit", &Discount{Type: "fixed_amount", Value: 299.5}, 1000, 300},
		{"not-a-number fixed", &Discount{Type: "fixed_amount", Value: math.NaN()}, 1000, 0},
		{"unknown type", &Discount{Type: "bogus", Value: 10}, 1000, 0},
		{"nil discount", nil, 1000, 0},
		{"no subtotal", &Discount{Type: "percentage", Value: 10}, 0, 0},
		{"negative subtotal", &Discount{Type: "fixed_amount", Value: 10}, -5, 0},
		{"a subtotal past int32", &Discount{Type: "percentage", Value: 50}, 9_000_000_000_000_000_000, 4_500_000_000_000_000_000},
		{"a near-whole percentage of the largest subtotal", &Discount{Type: "percentage", Value: 99.995}, math.MaxInt64, math.MaxInt64},
		{"a fixed amount past int32", &Discount{Type: "fixed_amount", Value: 3_000_000_000}, 5_000_000_000, 3_000_000_000},
	} {
		if got := ComputeDiscountAmountCents(c.d, c.subtotal); got != c.want {
			t.Errorf("%s: ComputeDiscountAmountCents = %d, want %d", c.name, got, c.want)
		}
	}
}
