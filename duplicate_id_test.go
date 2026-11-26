package billing

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Both stores refuse a product, price, invoice, usage record or credit whose
// id they already hold with ErrDuplicateID, and keep the stored row: neither
// overwrites it nor counts it twice.
func TestBothStoresRefuseAHeldID(t *testing.T) {
	ctx := context.Background()
	at := time.Unix(1_700_000_000, 0).UTC()
	eachStore(t, func(t *testing.T, s Store) {
		twice := map[string]func(n int64) error{
			"CreateProduct": func(n int64) error {
				return s.CreateProduct(ctx, &Product{ID: "prod", Partition: CatalogPartition, Name: "n"})
			},
			"CreatePrice": func(n int64) error {
				return s.CreatePrice(ctx, &Price{ID: "price", Partition: CatalogPartition, ProductID: "prod", AmountCents: n})
			},
			"CreateInvoice": func(n int64) error {
				return s.CreateInvoice(ctx, &Invoice{ID: "inv", Partition: "pa", CustomerID: "c1", Status: "open", AmountDueCents: n}, nil)
			},
			"RecordUsage": func(n int64) error {
				return s.RecordUsage(ctx, &UsageRecord{ID: "use", Partition: "pa", CustomerID: "c1", ProductID: "ot", Quantity: n, RecordedAt: at})
			},
			"GrantCredit": func(n int64) error {
				return s.GrantCredit(ctx, &Credit{ID: "cr", Partition: "pa", CustomerID: "c1", AmountCents: n, Currency: "aud"})
			},
		}
		for name, write := range twice {
			if err := write(1); err != nil {
				t.Fatalf("%s first write: %v", name, err)
			}
			if err := write(2); !errors.Is(err, ErrDuplicateID) {
				t.Errorf("%s of a held id = %v, want ErrDuplicateID", name, err)
			}
		}
		if p, _ := s.GetPrice(ctx, CatalogPartition, "price"); p == nil || p.AmountCents != 1 {
			t.Errorf("price after the refusal = %+v, want the first write's", p)
		}
		if inv, _ := s.GetInvoice(ctx, "pa", "inv"); inv == nil || inv.AmountDueCents != 1 {
			t.Errorf("invoice after the refusal = %+v, want the first write's", inv)
		}
		if sum, _ := s.SummarizeUsage(ctx, "pa", "c1", at, at.Add(time.Minute)); len(sum) != 1 || sum[0].TotalQuantity != 1 {
			t.Errorf("usage after the refusal = %+v, want 1 counted once", sum)
		}
		if bal, _ := s.CreditBalance(ctx, "pa", "c1", at); bal["aud"] != 1 {
			t.Errorf("credit balance after the refusal = %v, want 1", bal)
		}
	})
}
