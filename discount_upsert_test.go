package billing

import (
	"context"
	"errors"
	"testing"
)

// seedHeldDiscount stores d as a row its partition holds, past UpsertDiscount's
// catalog refusal: the row a database written before the catalog was
// platform-wide may still hold under a partition.
func seedHeldDiscount(t *testing.T, s Store, d Discount) {
	t.Helper()
	switch st := s.(type) {
	case *MemStore:
		st.mu.Lock()
		st.discounts[d.DiscountID] = &d
		st.mu.Unlock()
	case *SQLStore:
		if _, err := st.db.ExecContext(context.Background(), `INSERT INTO billing_discounts
			(discount_id, partition, code, type, value, max_uses, current_uses, active) VALUES (?,?,?,?,?,?,?,?)`,
			d.DiscountID, d.Partition, d.Code, d.Type, d.Value, d.MaxUses, d.CurrentUses, boolInt(d.Active)); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("no seed for %T", s)
	}
}

// UpsertDiscount never moves a discount to another partition: an id a row of
// another partition holds is refused with ErrDiscountPartition, and the
// holder's row (code, value, limit and uses) is as it was, on both stores.
func TestUpsertDiscountRefusesAnIDAnotherPartitionHolds(t *testing.T) {
	ctx := context.Background()
	eachStore(t, func(t *testing.T, s Store) {
		seedHeldDiscount(t, s, Discount{DiscountID: "d1", Partition: "pa", Code: "A10", Type: "percentage", Value: 10, MaxUses: 1, CurrentUses: 1, Active: true})
		err := s.UpsertDiscount(ctx, &Discount{DiscountID: "d1", Partition: CatalogPartition, Code: "B100", Type: "percentage", Value: 100, Active: true})
		if !errors.Is(err, ErrDiscountPartition) {
			t.Fatalf("UpsertDiscount of pa's id under the catalog = %v, want ErrDiscountPartition", err)
		}
		a, err := s.GetDiscount(ctx, "pa", "d1")
		if err != nil || a.Code != "A10" || a.Value != 10 || a.MaxUses != 1 || a.CurrentUses != 1 {
			t.Fatalf("pa's discount after the refusal = %+v, %v; want A10, 10, MaxUses 1, 1 use", a, err)
		}
		if _, err := s.GetDiscount(ctx, CatalogPartition, "d1"); !errors.Is(err, ErrDiscountNotFound) {
			t.Fatalf("the catalog reads d1 = %v, want ErrDiscountNotFound", err)
		}
	})
}

// A rewrite by the partition that holds the discount keeps the uses taken, on
// both stores: current_uses is the apply path's to move, so a re-seed of the
// coupon never gives back uses already spent.
func TestUpsertDiscountKeepsTheUsesTaken(t *testing.T) {
	ctx := context.Background()
	eachStore(t, func(t *testing.T, s Store) {
		if err := s.UpsertDiscount(ctx, &Discount{DiscountID: "d1", Partition: CatalogPartition, Type: "percentage", Value: 10, MaxUses: 5, Active: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.RecordAppliedDiscount(ctx, "pa", "sub_1", "d1"); err != nil {
			t.Fatal(err)
		}
		if err := s.UpsertDiscount(ctx, &Discount{DiscountID: "d1", Partition: CatalogPartition, Type: "percentage", Value: 20, MaxUses: 5, Active: true}); err != nil {
			t.Fatalf("rewrite by the holder = %v", err)
		}
		d, err := s.GetDiscount(ctx, CatalogPartition, "d1")
		if err != nil || d.Value != 20 || d.CurrentUses != 1 {
			t.Fatalf("after the rewrite = %+v, %v; want value 20 with 1 use kept", d, err)
		}
	})
}
