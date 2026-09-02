package billing

import "testing"

// PaymentMethod store semantics must be identical across MemStore and SQLStore:
// round-trip, at-most-one-default per customer, cross-partition isolation, and
// soft-delete excluded from the active list.
func TestPaymentMethod_CRUDDefaultAndPartitionIsolation(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		pm1 := &PaymentMethod{Partition: "p1", CustomerID: "c1", Type: "card", ProviderRef: "pm_1", CardLast4: "4242"}
		if err := s.AddPaymentMethod(ctx, pm1); err != nil {
			t.Fatalf("add pm1: %v", err)
		}
		pm2 := &PaymentMethod{Partition: "p1", CustomerID: "c1", Type: "card", ProviderRef: "pm_2", IsDefault: true, CardLast4: "1111"}
		if err := s.AddPaymentMethod(ctx, pm2); err != nil {
			t.Fatalf("add pm2: %v", err)
		}
		if pm1.ID == "" || pm2.ID == "" {
			t.Fatal("store must assign ids")
		}

		list, err := s.ListPaymentMethods(ctx, "p1", "c1")
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(list) != 2 {
			t.Fatalf("want 2 methods, got %d", len(list))
		}
		defaults := 0
		for _, m := range list {
			if m.IsDefault {
				defaults++
				if m.ID != pm2.ID {
					t.Fatalf("default is %s, want pm2 %s", m.ID, pm2.ID)
				}
			}
		}
		if defaults != 1 {
			t.Fatalf("want exactly 1 default after adding a default method, got %d", defaults)
		}

		// SetDefault flips the sole default to pm1.
		if err := s.SetDefaultPaymentMethod(ctx, "p1", "c1", pm1.ID); err != nil {
			t.Fatalf("set default: %v", err)
		}
		if got, err := s.GetPaymentMethod(ctx, "p1", pm1.ID); err != nil || !got.IsDefault {
			t.Fatalf("pm1 should be default: %+v err=%v", got, err)
		}
		if got, _ := s.GetPaymentMethod(ctx, "p1", pm2.ID); got.IsDefault {
			t.Fatal("pm2 should no longer be default")
		}

		// Cross-partition isolation: p2 neither sees nor mutates p1's methods.
		if l2, _ := s.ListPaymentMethods(ctx, "p2", "c1"); len(l2) != 0 {
			t.Fatalf("p2 must not see p1 methods, got %d", len(l2))
		}
		if err := s.SetDefaultPaymentMethod(ctx, "p2", "c1", pm1.ID); err != ErrNotFound {
			t.Fatalf("cross-partition set-default must be ErrNotFound, got %v", err)
		}
		if err := s.RemovePaymentMethod(ctx, "p2", pm1.ID); err != ErrNotFound {
			t.Fatalf("cross-partition remove must be ErrNotFound, got %v", err)
		}

		// Soft-delete excludes the method from the active list.
		if err := s.RemovePaymentMethod(ctx, "p1", pm1.ID); err != nil {
			t.Fatalf("remove: %v", err)
		}
		list2, _ := s.ListPaymentMethods(ctx, "p1", "c1")
		if len(list2) != 1 || list2[0].ID != pm2.ID {
			t.Fatalf("after remove want only pm2, got %d", len(list2))
		}
	})
}
