package billing

import (
	"context"
	"errors"
	"testing"
)

// keyRecordingProvider is the fakeProvider with the DiscountApplier surface,
// keeping every attach it is asked for.
type keyRecordingProvider struct {
	fakeProvider
	attached []DiscountAttachInput
}

func (p *keyRecordingProvider) AttachDiscount(_ context.Context, in DiscountAttachInput) (string, error) {
	p.attached = append(p.attached, in)
	return "di", nil
}

// ApplyDiscount hands the provider a key derived from the partition and the
// caller's key, as Charge does: two partitions applying under one key never
// meet at the provider's one account, and the caller's key never reaches it.
func TestApplyDiscountHandsTheProviderAKeyDerivedPerPartition(t *testing.T) {
	ctx := context.Background()
	eachStore(t, func(t *testing.T, s Store) {
		if err := s.UpsertDiscount(ctx, &Discount{DiscountID: "plan5", Partition: CatalogPartition, Type: "percentage", Value: 5, Active: true}); err != nil {
			t.Fatal(err)
		}
		prov := &keyRecordingProvider{}
		svc := newService(t, s, prov)
		for _, part := range []string{"pa", "pb"} {
			cust := &Customer{Partition: part, UserID: "u", ProviderRef: "cus_" + part}
			if err := s.CreateCustomer(ctx, cust); err != nil {
				t.Fatal(err)
			}
			if err := s.CreateSubscription(ctx, &Subscription{Partition: part, CustomerID: cust.ID, PriceID: "price", ProviderRef: "sub_" + part, Status: "active", Quantity: 1}); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.ApplyDiscount(ctx, part, cust.ID, "sub_"+part, "plan5", "key-1"); err != nil {
				t.Fatalf("ApplyDiscount(%s) = %v", part, err)
			}
		}
		if len(prov.attached) != 2 {
			t.Fatalf("attaches = %d, want 2", len(prov.attached))
		}
		a, b := prov.attached[0].IdempotencyKey, prov.attached[1].IdempotencyKey
		if a == "key-1" || b == "key-1" || a == b {
			t.Fatalf("provider keys %q and %q: want each partition's own key, never the caller's", a, b)
		}
		if a != providerIdempotencyKey("discount", "pa", "key-1") {
			t.Fatalf("pa's provider key = %q, want the key derived from pa and key-1", a)
		}
	})
}

// ApplyDiscount attaches only to a subscription the customer holds in the
// partition: another customer's subscription, another partition's and an
// unknown reference are refused with ErrDiscountSubscription before any use is
// taken, and the provider is not asked.
func TestApplyDiscountAttachesOnlyToTheCustomersOwnSubscription(t *testing.T) {
	ctx := context.Background()
	eachStore(t, func(t *testing.T, s Store) {
		if err := s.UpsertDiscount(ctx, &Discount{DiscountID: "plan5", Partition: CatalogPartition, Type: "percentage", Value: 5, Active: true}); err != nil {
			t.Fatal(err)
		}
		mine := &Customer{Partition: "pa", UserID: "u1", ProviderRef: "cus_1"}
		other := &Customer{Partition: "pa", UserID: "u2", ProviderRef: "cus_2"}
		foreign := &Customer{Partition: "pb", UserID: "u1", ProviderRef: "cus_3"}
		for _, c := range []*Customer{mine, other, foreign} {
			if err := s.CreateCustomer(ctx, c); err != nil {
				t.Fatal(err)
			}
		}
		for ref, c := range map[string]*Customer{"sub_other": other, "sub_foreign": foreign} {
			if err := s.CreateSubscription(ctx, &Subscription{Partition: c.Partition, CustomerID: c.ID, PriceID: "price", ProviderRef: ref, Status: "active", Quantity: 1}); err != nil {
				t.Fatal(err)
			}
		}
		prov := &keyRecordingProvider{}
		svc := newService(t, s, prov)
		for _, ref := range []string{"sub_other", "sub_foreign", "sub_unknown"} {
			if _, err := svc.ApplyDiscount(ctx, "pa", mine.ID, ref, "plan5", "k-"+ref); !errors.Is(err, ErrDiscountSubscription) {
				t.Errorf("ApplyDiscount to %s = %v, want ErrDiscountSubscription", ref, err)
			}
		}
		if len(prov.attached) != 0 {
			t.Fatalf("the provider was asked to attach %+v", prov.attached)
		}
		if d, _ := s.GetDiscount(ctx, CatalogPartition, "plan5"); d.CurrentUses != 0 {
			t.Fatalf("a refused application took %d uses", d.CurrentUses)
		}
	})
}
