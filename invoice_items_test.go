package billing

import (
	"context"
	"testing"
)

// Invoice lines are listed only within the partition that holds the invoice,
// on both stores: another partition and the empty partition read none.
func TestInvoiceItemsAreListedOnlyWithinThePartition(t *testing.T) {
	ctx := context.Background()
	eachStore(t, func(t *testing.T, s Store) {
		inv := &Invoice{ID: "inv1", Partition: "pa", CustomerID: "c1", Status: "open", Currency: "aud"}
		if err := s.CreateInvoice(ctx, inv, []*InvoiceItem{{ID: "line1", Description: "plan", Quantity: 1, UnitAmountCents: 100, AmountCents: 100}}); err != nil {
			t.Fatal(err)
		}
		if got, err := s.ListInvoiceItems(ctx, "pa", "inv1"); err != nil || len(got) != 1 || got[0].ID != "line1" {
			t.Fatalf("pa lists %+v, %v; want line1", got, err)
		}
		for _, other := range []string{"pb", ""} {
			if got, err := s.ListInvoiceItems(ctx, other, "inv1"); err != nil || len(got) != 0 {
				t.Fatalf("partition %q lists pa's lines %+v, %v; want none", other, got, err)
			}
		}
	})
}
