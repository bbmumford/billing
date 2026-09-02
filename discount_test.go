package billing

import "testing"

// Discount catalogue + applied-ledger semantics must be identical across MemStore
// and SQLStore: upsert/read, cross-partition isolation, apply idempotency, and the
// current_uses bump.
func TestDiscount_CatalogueApplyIdempotencyAndPartition(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		d := &Discount{DiscountID: "cpn_10", Partition: "p1", Type: "percentage", Value: 10, Active: true}
		if err := s.UpsertDiscount(ctx, d); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		if got, err := s.GetDiscount(ctx, "p1", "cpn_10"); err != nil || got.Value != 10 || !got.Active {
			t.Fatalf("get: %+v err=%v", got, err)
		}
		if _, err := s.GetDiscount(ctx, "p2", "cpn_10"); err != ErrDiscountNotFound {
			t.Fatalf("cross-partition get must be ErrDiscountNotFound, got %v", err)
		}
		if _, err := s.GetDiscount(ctx, "p1", "nope"); err != ErrDiscountNotFound {
			t.Fatalf("missing get must be ErrDiscountNotFound, got %v", err)
		}

		if applied, _ := s.IsDiscountApplied(ctx, "p1", "sub_1", "cpn_10"); applied {
			t.Fatal("not applied yet")
		}
		if _, err := s.RecordAppliedDiscount(ctx, "p1", "sub_1", "cpn_10"); err != nil {
			t.Fatalf("record: %v", err)
		}
		if applied, _ := s.IsDiscountApplied(ctx, "p1", "sub_1", "cpn_10"); !applied {
			t.Fatal("should be applied")
		}
		if got, _ := s.GetDiscount(ctx, "p1", "cpn_10"); got.CurrentUses != 1 {
			t.Fatalf("current_uses want 1, got %d", got.CurrentUses)
		}
		if _, err := s.RecordAppliedDiscount(ctx, "p1", "sub_1", "cpn_10"); err != ErrDiscountAlreadyApplied {
			t.Fatalf("re-record must be ErrDiscountAlreadyApplied, got %v", err)
		}
		if applied, _ := s.IsDiscountApplied(ctx, "p2", "sub_1", "cpn_10"); applied {
			t.Fatal("p2 must not see p1's application")
		}
	})
}

func TestComputeDiscountAmountCents(t *testing.T) {
	if got := ComputeDiscountAmountCents(&Discount{Type: "percentage", Value: 10}, 1000); got != 100 {
		t.Fatalf("10%% of 1000 = 100, got %d", got)
	}
	if got := ComputeDiscountAmountCents(&Discount{Type: "fixed_amount", Value: 300}, 1000); got != 300 {
		t.Fatalf("fixed 300, got %d", got)
	}
	if got := ComputeDiscountAmountCents(&Discount{Type: "fixed_amount", Value: 5000}, 1000); got != 1000 {
		t.Fatalf("fixed clamps to subtotal 1000, got %d", got)
	}
	if got := ComputeDiscountAmountCents(nil, 1000); got != 0 {
		t.Fatalf("nil discount = 0, got %d", got)
	}
}
