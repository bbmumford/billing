package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

func upsertLimited(t *testing.T, s Store, id string, maxUses int64) {
	t.Helper()
	if err := s.UpsertDiscount(ctx, &Discount{DiscountID: id, Partition: CatalogPartition, Type: "percentage", Value: 10, MaxUses: maxUses, Active: true}); err != nil {
		t.Fatalf("UpsertDiscount: %v", err)
	}
}

func usesOf(t *testing.T, s Store, id string) int64 {
	t.Helper()
	d, err := s.GetDiscount(ctx, CatalogPartition, id)
	if err != nil {
		t.Fatalf("GetDiscount: %v", err)
	}
	return d.CurrentUses
}

// A discount with a use limit is applied at most that many times. The use is taken in the same
// write that records the application, and a write that would pass the limit is refused with
// ErrDiscountExhausted and records nothing; MaxUses 0 is no limit.
func TestRecordAppliedDiscount_TheUseLimitIsHeld(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		upsertLimited(t, s, "cpn_once", 1)
		if _, err := s.RecordAppliedDiscount(ctx, "p1", "s1", "cpn_once"); err != nil {
			t.Fatalf("first use: %v", err)
		}
		for _, sub := range []string{"s2", "s3"} {
			if _, err := s.RecordAppliedDiscount(ctx, "p1", sub, "cpn_once"); !errors.Is(err, ErrDiscountExhausted) {
				t.Fatalf("use on %s = %v, want ErrDiscountExhausted", sub, err)
			}
			if applied, _ := s.IsDiscountApplied(ctx, "p1", sub, "cpn_once"); applied {
				t.Fatalf("a refused use recorded the pair for %s", sub)
			}
		}
		if got := usesOf(t, s, "cpn_once"); got != 1 {
			t.Fatalf("CurrentUses = %d, want 1", got)
		}
		upsertLimited(t, s, "cpn_free", 0)
		for _, sub := range []string{"s1", "s2", "s3"} {
			if _, err := s.RecordAppliedDiscount(ctx, "p1", sub, "cpn_free"); err != nil {
				t.Fatalf("unlimited use on %s: %v", sub, err)
			}
		}
		if got := usesOf(t, s, "cpn_free"); got != 3 {
			t.Fatalf("unlimited CurrentUses = %d, want 3", got)
		}
	})
}

// A customer-wide use, recorded under its customer's reference, counts against the limit as a
// subscription's does; each partition's application of the catalog's discount is its own.
func TestRecordAppliedDiscount_CustomerWideUsesCountAndPartitionsAreApart(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		upsertLimited(t, s, "cpn_two", 2)
		for i := 0; i < 2; i++ {
			if _, err := s.RecordAppliedDiscount(ctx, "p1", customerWideRef(fmt.Sprint("c", i)), "cpn_two"); err != nil {
				t.Fatalf("customer-wide use %d: %v", i, err)
			}
		}
		if _, err := s.RecordAppliedDiscount(ctx, "p1", customerWideRef("c2"), "cpn_two"); !errors.Is(err, ErrDiscountExhausted) {
			t.Fatalf("third customer-wide use = %v, want ErrDiscountExhausted", err)
		}
		if _, err := s.RecordAppliedDiscount(ctx, "p1", "s1", "cpn_two"); !errors.Is(err, ErrDiscountExhausted) {
			t.Fatalf("subscription use past the limit = %v, want ErrDiscountExhausted", err)
		}
		if got := usesOf(t, s, "cpn_two"); got != 2 {
			t.Fatalf("CurrentUses = %d, want 2", got)
		}
		upsertLimited(t, s, "cpn_p1", 0)
		if _, err := s.RecordAppliedDiscount(ctx, "p2", "s1", "cpn_p1"); err != nil {
			t.Fatalf("p2's use of the catalog's discount = %v", err)
		}
		if applied, _ := s.IsDiscountApplied(ctx, "p1", "s1", "cpn_p1"); applied {
			t.Fatal("p1 sees p2's application")
		}
		if applied, _ := s.IsDiscountApplied(ctx, "p2", "s1", "cpn_p1"); !applied {
			t.Fatal("p2's application was not recorded")
		}
	})
}

// ReleaseAppliedDiscount gives back a use and the pair, so a provider attach that failed after
// the use was taken leaves the limit as it was and a retry can take the use again.
func TestReleaseAppliedDiscount_GivesTheUseBack(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		upsertLimited(t, s, "cpn_once", 1)
		if _, err := s.RecordAppliedDiscount(ctx, "p1", "s1", "cpn_once"); err != nil {
			t.Fatal(err)
		}
		if err := s.ReleaseAppliedDiscount(ctx, "p1", "s1", "cpn_once"); err != nil {
			t.Fatalf("release: %v", err)
		}
		if applied, _ := s.IsDiscountApplied(ctx, "p1", "s1", "cpn_once"); applied || usesOf(t, s, "cpn_once") != 0 {
			t.Fatalf("after release: applied %v, uses %d; want neither", applied, usesOf(t, s, "cpn_once"))
		}
		if _, err := s.RecordAppliedDiscount(ctx, "p1", "s2", "cpn_once"); err != nil {
			t.Fatalf("use after release: %v", err)
		}
		if err := s.ReleaseAppliedDiscount(ctx, "p1", "s9", "cpn_once"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("release of a pair not held = %v, want ErrNotFound", err)
		}
		if usesOf(t, s, "cpn_once") != 1 {
			t.Fatalf("a refused release changed the uses to %d", usesOf(t, s, "cpn_once"))
		}
		if _, err := s.RecordAppliedDiscount(ctx, "p1", "s3", "cpn_free"); !errors.Is(err, ErrDiscountNotFound) {
			t.Fatalf("use of an absent discount = %v, want ErrDiscountNotFound", err)
		}
		upsertLimited(t, s, "cpn_cw", 0)
		if err := s.ReleaseAppliedDiscount(ctx, "p1", "", "cpn_cw"); !errors.Is(err, ErrNotFound) || usesOf(t, s, "cpn_cw") != 0 {
			t.Fatalf("release with no recorded use = %v, uses %d; want ErrNotFound and 0", err, usesOf(t, s, "cpn_cw"))
		}
	})
}

// Concurrent uses never pass the limit: the check and the count are one write.
func TestRecordAppliedDiscount_ConcurrentUsesStayWithinTheLimit(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "b.db")+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("db.Close: %v", err)
		}
	})
	sqlStore := NewSQLStore(db)
	if err := sqlStore.InitSchema(ctx); err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]Store{"mem": NewMemStore(), "sql": sqlStore} {
		t.Run(name, func(t *testing.T) {
			upsertLimited(t, s, "cpn_three", 3)
			var wg sync.WaitGroup
			var mu sync.Mutex
			taken := 0
			for i := 0; i < 12; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					_, err := s.RecordAppliedDiscount(ctx, "p1", fmt.Sprintf("s%d", i), "cpn_three")
					if err == nil {
						mu.Lock()
						taken++
						mu.Unlock()
					}
				}(i)
			}
			wg.Wait()
			if uses := usesOf(t, s, "cpn_three"); taken != 3 || uses != 3 {
				t.Fatalf("uses taken %d, CurrentUses %d; want 3 and 3", taken, uses)
			}
		})
	}
}

// attachingProvider is the fakeProvider with the DiscountApplier surface: it counts attaches and
// fails the ones a test names.
type attachingProvider struct {
	fakeProvider
	mu       sync.Mutex
	attached []string
	fail     map[string]bool
}

func (p *attachingProvider) AttachDiscount(_ context.Context, in DiscountAttachInput) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail[in.SubscriptionRef] {
		return "", errors.New("provider refused the attach")
	}
	p.attached = append(p.attached, in.SubscriptionRef)
	return "di_" + in.SubscriptionRef, nil
}

// Service.ApplyDiscount takes the use before the provider attach, so a discount at its limit is
// refused without asking the provider, and an attach that fails gives the use back.
func TestApplyDiscount_TakesTheUseBeforeTheProviderAttach(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		cust := &Customer{Partition: "p1", UserID: "u1", ProviderRef: "cus_1"}
		if err := s.CreateCustomer(ctx, cust); err != nil {
			t.Fatal(err)
		}
		for _, ref := range []string{"s_fails", "s1", "s2"} {
			if err := s.CreateSubscription(ctx, &Subscription{Partition: "p1", CustomerID: cust.ID, PriceID: "price", ProviderRef: ref, Status: "active", Quantity: 1}); err != nil {
				t.Fatal(err)
			}
		}
		upsertLimited(t, s, "cpn_once", 1)
		prov := &attachingProvider{fail: map[string]bool{"s_fails": true}}
		svc := newService(t, s, prov)
		if _, err := svc.ApplyDiscount(ctx, "p1", cust.ID, "s_fails", "cpn_once", "k0"); err == nil {
			t.Fatal("a failed attach answered success")
		}
		if applied, _ := s.IsDiscountApplied(ctx, "p1", "s_fails", "cpn_once"); applied || usesOf(t, s, "cpn_once") != 0 {
			t.Fatalf("a failed attach kept the use: applied %v, uses %d", applied, usesOf(t, s, "cpn_once"))
		}
		if res, err := svc.ApplyDiscount(ctx, "p1", cust.ID, "s1", "cpn_once", "k1"); err != nil || res.AlreadyApplied {
			t.Fatalf("first apply = %+v, %v", res, err)
		}
		if res, err := svc.ApplyDiscount(ctx, "p1", cust.ID, "s1", "cpn_once", "k1"); err != nil || !res.AlreadyApplied {
			t.Fatalf("repeat apply = %+v, %v; want AlreadyApplied", res, err)
		}
		if _, err := svc.ApplyDiscount(ctx, "p1", cust.ID, "s2", "cpn_once", "k2"); !errors.Is(err, ErrDiscountExhausted) {
			t.Fatalf("apply past the limit = %v, want ErrDiscountExhausted", err)
		}
		if _, err := svc.ApplyDiscount(ctx, "p1", cust.ID, "", "cpn_once", "k3"); !errors.Is(err, ErrDiscountExhausted) {
			t.Fatalf("customer-wide apply past the limit = %v, want ErrDiscountExhausted", err)
		}
		if len(prov.attached) != 1 || prov.attached[0] != "s1" {
			t.Fatalf("provider attaches = %q, want only s1's", prov.attached)
		}
		upsertLimited(t, s, "cpn_cw", 1)
		if _, err := svc.ApplyDiscount(ctx, "p1", cust.ID, "", "cpn_cw", "k4"); err != nil {
			t.Fatalf("customer-wide apply: %v", err)
		}
		if got := usesOf(t, s, "cpn_cw"); got != 1 {
			t.Fatalf("customer-wide apply took %d uses, want 1", got)
		}
	})
}
