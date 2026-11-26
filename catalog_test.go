package billing

import (
	"context"
	"errors"
	"testing"
)

// The catalog is written only under CatalogPartition, on both stores: a
// product, price, price reference or discount written under a partition is
// refused with ErrCatalogPartition and stores nothing.
func TestTheCatalogIsWrittenOnlyUnderCatalogPartition(t *testing.T) {
	ctx := context.Background()
	eachStore(t, func(t *testing.T, s Store) {
		if err := s.CreateProduct(ctx, &Product{ID: "prod", Partition: "pa", Name: "n"}); !errors.Is(err, ErrCatalogPartition) {
			t.Errorf("CreateProduct under pa = %v, want ErrCatalogPartition", err)
		}
		if err := s.CreatePrice(ctx, &Price{ID: "price", Partition: "pa", ProductID: "prod"}); !errors.Is(err, ErrCatalogPartition) {
			t.Errorf("CreatePrice under pa = %v, want ErrCatalogPartition", err)
		}
		if err := s.UpsertDiscount(ctx, &Discount{DiscountID: "d", Partition: "pa", Type: "percentage", Value: 5}); !errors.Is(err, ErrCatalogPartition) {
			t.Errorf("UpsertDiscount under pa = %v, want ErrCatalogPartition", err)
		}
		if _, err := s.GetProduct(ctx, "pa", "prod"); !errors.Is(err, ErrNotFound) {
			t.Errorf("a refused product was stored: %v", err)
		}
		if err := s.CreatePrice(ctx, &Price{ID: "price", Partition: CatalogPartition, ProductID: "prod"}); err != nil {
			t.Fatalf("CreatePrice under the catalog = %v", err)
		}
		if err := s.SetPriceProviderRef(ctx, "pa", "price", "price_x"); !errors.Is(err, ErrCatalogPartition) {
			t.Errorf("SetPriceProviderRef under pa = %v, want ErrCatalogPartition", err)
		}
		if err := s.SetPriceProviderRef(ctx, CatalogPartition, "price", "price_x"); err != nil {
			t.Errorf("SetPriceProviderRef under the catalog = %v", err)
		}
	})
}

// Every partition subscribes to a price the catalog holds, with no copy in its
// own rows: SubscribeItems reads each price under CatalogPartition whatever the
// caller's partition, and the subscription row is the caller's.
func TestEveryPartitionSubscribesToACatalogPrice(t *testing.T) {
	ctx := context.Background()
	eachStore(t, func(t *testing.T, s Store) {
		if err := s.CreatePrice(ctx, &Price{ID: "orbtr-pro-monthly", Partition: CatalogPartition, ProductID: "orbtr-pro", ProviderRef: "price_pro", Currency: "aud", AmountCents: 1000}); err != nil {
			t.Fatal(err)
		}
		for _, part := range []string{"pa", "pb"} {
			prov := &itemsCreatingProvider{answer: SubscriptionResult{ProviderRef: "sub_" + part, Status: "active",
				Items: []SubscriptionItem{{PriceRef: "price_pro", Quantity: 1}}}}
			svc := newService(t, s, prov)
			sub, _, err := svc.SubscribeItems(ctx, part, "c-"+part, "cus_"+part, []SubscriptionItem{{PriceID: "orbtr-pro-monthly", Quantity: 1}}, "", "k-"+part)
			if err != nil {
				t.Fatalf("SubscribeItems(%s) = %v", part, err)
			}
			if sub.Partition != part || sub.PriceID != "orbtr-pro-monthly" {
				t.Fatalf("%s: row %+v, want the partition's row on the catalog price", part, sub)
			}
		}
	})
}

// Every partition applies a discount the catalog holds: ApplyDiscount reads it
// under CatalogPartition, each partition's application is its own pair, and
// both uses are counted on the catalog's one row.
func TestEveryPartitionAppliesACatalogDiscount(t *testing.T) {
	ctx := context.Background()
	eachStore(t, func(t *testing.T, s Store) {
		if err := s.UpsertDiscount(ctx, &Discount{DiscountID: "plan10", Partition: CatalogPartition, Type: "percentage", Value: 10, Active: true}); err != nil {
			t.Fatal(err)
		}
		prov := &attachingProvider{}
		svc := newService(t, s, prov)
		for _, part := range []string{"pa", "pb"} {
			cust := &Customer{Partition: part, UserID: "u", ProviderRef: "cus_" + part}
			if err := s.CreateCustomer(ctx, cust); err != nil {
				t.Fatal(err)
			}
			if err := s.CreateSubscription(ctx, &Subscription{Partition: part, CustomerID: cust.ID, PriceID: "price", ProviderRef: "sub_" + part, Status: "active", Quantity: 1}); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.ApplyDiscount(ctx, part, cust.ID, "sub_"+part, "plan10", "k"); err != nil {
				t.Fatalf("ApplyDiscount(%s) = %v", part, err)
			}
			if applied, _ := s.IsDiscountApplied(ctx, part, "sub_"+part, "plan10"); !applied {
				t.Fatalf("%s's application was not recorded", part)
			}
		}
		if d, err := s.GetDiscount(ctx, CatalogPartition, "plan10"); err != nil || d.CurrentUses != 2 {
			t.Fatalf("catalog discount after two partitions' uses = %+v, %v; want 2 uses", d, err)
		}
	})
}
