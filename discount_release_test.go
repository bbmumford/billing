package billing

import (
	"context"
	"errors"
	"testing"
)

// A discount use is given back only against the record that it was taken, on
// both stores: a release naming no application (an empty reference) or one the
// partition never recorded is refused with ErrNotFound and gives nothing back,
// so no caller can reopen a discount at its limit.
func TestADiscountUseIsReleasedOnlyAgainstItsRecord(t *testing.T) {
	ctx := context.Background()
	eachStore(t, func(t *testing.T, s Store) {
		if err := s.UpsertDiscount(ctx, &Discount{DiscountID: "once", Partition: CatalogPartition, Type: "percentage", Value: 10, MaxUses: 1, Active: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.RecordAppliedDiscount(ctx, "pa", "sub_1", "once"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.RecordAppliedDiscount(ctx, "pa", "", "once"); !errors.Is(err, ErrNotFound) {
			t.Errorf("an application with no reference = %v, want ErrNotFound", err)
		}
		for _, ref := range []string{"", "sub_never"} {
			if err := s.ReleaseAppliedDiscount(ctx, "pa", ref, "once"); !errors.Is(err, ErrNotFound) {
				t.Errorf("release of %q = %v, want ErrNotFound", ref, err)
			}
		}
		if err := s.ReleaseAppliedDiscount(ctx, "pb", "sub_1", "once"); !errors.Is(err, ErrNotFound) {
			t.Errorf("another partition's release of pa's application = %v, want ErrNotFound", err)
		}
		if d, _ := s.GetDiscount(ctx, CatalogPartition, "once"); d.CurrentUses != 1 {
			t.Fatalf("uses after refused releases = %d, want 1", d.CurrentUses)
		}
		if err := s.ReleaseAppliedDiscount(ctx, "pa", "sub_1", "once"); err != nil {
			t.Fatalf("release of the recorded application = %v", err)
		}
		if d, _ := s.GetDiscount(ctx, CatalogPartition, "once"); d.CurrentUses != 0 {
			t.Fatalf("uses after the release = %d, want 0", d.CurrentUses)
		}
	})
}

// A customer-wide application is recorded like a subscription's: a repeat
// answers AlreadyApplied without a second use or attach, and an attach the
// provider refuses gives its use back through that record.
func TestACustomerWideApplicationIsRecorded(t *testing.T) {
	ctx := context.Background()
	eachStore(t, func(t *testing.T, s Store) {
		for _, id := range []string{"cw", "cw_fails"} {
			if err := s.UpsertDiscount(ctx, &Discount{DiscountID: id, Partition: CatalogPartition, Type: "percentage", Value: 10, MaxUses: 5, Active: true}); err != nil {
				t.Fatal(err)
			}
		}
		cust := &Customer{Partition: "pa", UserID: "u", ProviderRef: "cus_1"}
		if err := s.CreateCustomer(ctx, cust); err != nil {
			t.Fatal(err)
		}
		prov := &attachingProvider{}
		svc := newService(t, s, prov)
		if res, err := svc.ApplyDiscount(ctx, "pa", cust.ID, "", "cw", "k1"); err != nil || res.AlreadyApplied {
			t.Fatalf("first customer-wide apply = %+v, %v", res, err)
		}
		if res, err := svc.ApplyDiscount(ctx, "pa", cust.ID, "", "cw", "k2"); err != nil || !res.AlreadyApplied {
			t.Fatalf("repeat customer-wide apply = %+v, %v; want AlreadyApplied", res, err)
		}
		if d, _ := s.GetDiscount(ctx, CatalogPartition, "cw"); d.CurrentUses != 1 || len(prov.attached) != 1 {
			t.Fatalf("uses %d, attaches %d; want 1 and 1", d.CurrentUses, len(prov.attached))
		}
		prov.fail = map[string]bool{"": true}
		if _, err := svc.ApplyDiscount(ctx, "pa", cust.ID, "", "cw_fails", "k3"); err == nil {
			t.Fatal("a refused attach answered success")
		}
		if d, _ := s.GetDiscount(ctx, CatalogPartition, "cw_fails"); d.CurrentUses != 0 {
			t.Fatalf("a refused customer-wide attach kept %d uses", d.CurrentUses)
		}
	})
}
