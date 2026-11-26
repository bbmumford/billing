package billing

import (
	"context"
	"errors"
	"testing"
)

// Every write of a row with no partition is refused with ErrNoPartition and
// stores nothing, on both stores: every read refuses the empty partition, so a
// row written there could never be read, listed or removed again.
func TestEveryWriteRefusesARowWithNoPartition(t *testing.T) {
	ctx := context.Background()
	eachStore(t, func(t *testing.T, s Store) {
		writes := map[string]func() error{
			"CreateCustomer": func() error { return s.CreateCustomer(ctx, &Customer{ID: "c0", UserID: "u"}) },
			"CreateProduct":  func() error { return s.CreateProduct(ctx, &Product{ID: "prod0", Name: "n"}) },
			"CreatePrice":    func() error { return s.CreatePrice(ctx, &Price{ID: "price0", ProductID: "prod0"}) },
			"CreateSubscription": func() error {
				return s.CreateSubscription(ctx, &Subscription{ID: "sub0", CustomerID: "c0", PriceID: "price0", Quantity: 1, Status: "active"})
			},
			"CreateSubscriptionWithItems": func() error {
				return s.CreateSubscriptionWithItems(ctx, &Subscription{ID: "sub1", CustomerID: "c0", Status: "active"},
					[]SubscriptionItem{{PriceID: "price0", Quantity: 1}})
			},
			"CreateInvoice": func() error {
				return s.CreateInvoice(ctx, &Invoice{ID: "inv0", CustomerID: "c0", Status: "open"}, []*InvoiceItem{{ID: "it0", Quantity: 1}})
			},
			"RecordUsage": func() error {
				return s.RecordUsage(ctx, &UsageRecord{ID: "use0", CustomerID: "c0", ProductID: "prod0", Quantity: 1})
			},
			"GrantCredit": func() error {
				return s.GrantCredit(ctx, &Credit{ID: "cr0", CustomerID: "c0", AmountCents: 100, Currency: "aud"})
			},
			"AddPaymentMethod": func() error {
				return s.AddPaymentMethod(ctx, &PaymentMethod{ID: "pm0", CustomerID: "c0", ProviderRef: "pm_x", Active: true})
			},
			"UpsertDiscount": func() error {
				return s.UpsertDiscount(ctx, &Discount{DiscountID: "d0", Type: "percentage", Value: 5, Active: true})
			},
		}
		for name, write := range writes {
			if err := write(); !errors.Is(err, ErrNoPartition) {
				t.Errorf("%s with no partition = %v, want ErrNoPartition", name, err)
			}
		}
		// Nothing was stored under the empty partition: a later write of the same
		// ids under a real partition is not refused as a duplicate.
		if err := s.CreateCustomer(ctx, &Customer{ID: "c0", Partition: "p1", UserID: "u"}); err != nil {
			t.Errorf("CreateCustomer c0 under p1 after the refusal = %v", err)
		}
		if err := s.GrantCredit(ctx, &Credit{ID: "cr0", Partition: "p1", CustomerID: "c0", AmountCents: 100, Currency: "aud"}); err != nil {
			t.Errorf("GrantCredit cr0 under p1 after the refusal = %v", err)
		}
	})
}
