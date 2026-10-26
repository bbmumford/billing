package billing

import (
	"context"
	"errors"
	"testing"
	"time"
)

// UpdateSubscriptionCancel writes cancel_at_period_end and canceled_at, moves
// updated_at, and leaves every other column as stored. The status and the two
// instants the status carries stay what the provider's event wrote, so a cancel
// beside a past_due row changes neither the status nor the unpaid clock the hold
// rule counts; the provider's own instant is stored, not the store's clock.
func TestUpdateSubscriptionCancel_WritesTheCancelFieldsAndNeverTheStatusOrItsClocks(t *testing.T) {
	eachClockedStore(t, func(t *testing.T, s unpaidStore, clk *stepClock) {
		start, end := at, at.Add(30*24*time.Hour)
		sub := &Subscription{Partition: "acme", CustomerID: "c1", PriceID: "p1", ProviderRef: "sub_1",
			Status: "active", Quantity: 2, CurrentPeriodStart: &start, CurrentPeriodEnd: &end}
		if err := s.CreateSubscription(ctx, sub); err != nil {
			t.Fatalf("CreateSubscription: %v", err)
		}
		clk.set(clk.day(1))
		setStatus(t, s, "acme", sub.ID, "past_due")
		before := mustGet(t, s, "acme", sub.ID)
		wantUnpaid(t, before, clk.day(1))

		clk.set(clk.day(3))
		providerInstant := clk.day(3).Add(-time.Hour)
		if err := s.UpdateSubscriptionCancel(ctx, "acme", sub.ID, true, &providerInstant); err != nil {
			t.Fatalf("UpdateSubscriptionCancel: %v", err)
		}
		got := mustGet(t, s, "acme", sub.ID)
		if !got.CancelAtPeriodEnd || got.CanceledAt == nil || !got.CanceledAt.Equal(providerInstant) {
			t.Fatalf("cancel fields = (%v, %v), want (true, %v)", got.CancelAtPeriodEnd, got.CanceledAt, providerInstant)
		}
		if got.Status != "past_due" {
			t.Fatalf("status = %q after the cancel write, want the event's %q", got.Status, "past_due")
		}
		if !got.StatusChangedAt.Equal(before.StatusChangedAt) {
			t.Fatalf("StatusChangedAt moved from %v to %v on a cancel write", before.StatusChangedAt, got.StatusChangedAt)
		}
		wantUnpaid(t, got, clk.day(1))
		if got.PriceID != "p1" || got.Quantity != 2 || got.ProviderRef != "sub_1" || got.CustomerID != "c1" {
			t.Fatalf("other columns moved: %+v", got)
		}
		if got.CurrentPeriodStart == nil || !got.CurrentPeriodStart.Equal(start) ||
			got.CurrentPeriodEnd == nil || !got.CurrentPeriodEnd.Equal(end) {
			t.Fatalf("period moved: %v..%v, want %v..%v", got.CurrentPeriodStart, got.CurrentPeriodEnd, start, end)
		}
		if !got.CreatedAt.Equal(before.CreatedAt) || !got.UpdatedAt.Equal(clk.day(3)) {
			t.Fatalf("times: created %v (before %v), updated %v (want %v)", got.CreatedAt, before.CreatedAt, got.UpdatedAt, clk.day(3))
		}

		// The write mirrors what the provider answers each time: no flag and no
		// instant clears both, and the status and its clock still stand.
		clk.set(clk.day(4))
		if err := s.UpdateSubscriptionCancel(ctx, "acme", sub.ID, false, nil); err != nil {
			t.Fatalf("UpdateSubscriptionCancel(clear): %v", err)
		}
		got = mustGet(t, s, "acme", sub.ID)
		if got.CancelAtPeriodEnd || got.CanceledAt != nil {
			t.Fatalf("cancel fields after clearing = (%v, %v), want (false, nil)", got.CancelAtPeriodEnd, got.CanceledAt)
		}
		if got.Status != "past_due" || !got.StatusChangedAt.Equal(before.StatusChangedAt) {
			t.Fatalf("status %q changed at %v after clearing, want past_due at %v", got.Status, got.StatusChangedAt, before.StatusChangedAt)
		}
		wantUnpaid(t, got, clk.day(1))
	})
}

// The write is partition-scoped like every other: another partition, an empty
// partition and an unknown id each answer ErrNotFound and change nothing.
func TestUpdateSubscriptionCancel_ReachesOnlyItsPartition(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		sub := &Subscription{Partition: "acme", CustomerID: "c1", PriceID: "p1", ProviderRef: "sub_1", Status: "active", Quantity: 1}
		if err := s.CreateSubscription(ctx, sub); err != nil {
			t.Fatalf("CreateSubscription: %v", err)
		}
		when := at.Add(time.Hour)
		for _, tc := range []struct{ partition, id string }{
			{"globex", sub.ID},
			{"", sub.ID},
			{"acme", "missing"},
		} {
			if err := s.UpdateSubscriptionCancel(ctx, tc.partition, tc.id, true, &when); !errors.Is(err, ErrNotFound) {
				t.Fatalf("UpdateSubscriptionCancel(%q, %q) = %v, want ErrNotFound", tc.partition, tc.id, err)
			}
		}
		got := mustGet(t, s, "acme", sub.ID)
		if got.CancelAtPeriodEnd || got.CanceledAt != nil {
			t.Fatalf("a refused write changed the row: %+v", got)
		}
	})
}

// cancellingStore is the reference MemStore with its cancel write counted, so a
// Service test can tell whether the store is written, and in what order against
// the provider call.
type cancellingStore struct {
	*MemStore
	cancelWrites int
	onCancel     func()
}

func (s *cancellingStore) UpdateSubscriptionCancel(c context.Context, partition, id string, cancelAtPeriodEnd bool, canceledAt *time.Time) error {
	s.cancelWrites++
	if s.onCancel != nil {
		s.onCancel()
	}
	return s.MemStore.UpdateSubscriptionCancel(c, partition, id, cancelAtPeriodEnd, canceledAt)
}

// Service.CancelSubscription calls the provider first, at once or at period end
// as asked, and stores what the provider answers: the row's two cancel fields
// become the provider's, the status and its clocks stay what the provider's
// event last wrote, and the row answered is the stored one.
func TestService_CancelSubscriptionCallsTheProviderFirstAndStoresItsAnswer(t *testing.T) {
	providerInstant := at.Add(90 * time.Minute)
	for _, atPeriodEnd := range []bool{false, true} {
		t.Run(map[bool]string{false: "at once", true: "at period end"}[atPeriodEnd], func(t *testing.T) {
			eachStore(t, func(t *testing.T, store Store) {
				prov := &fakeProvider{cancelAnswer: CancelResult{CancelAtPeriodEnd: atPeriodEnd, CanceledAt: &providerInstant}}
				svc := newService(t, store, prov)
				sub := &Subscription{Partition: "acme", CustomerID: "c1", PriceID: "p1", ProviderRef: "sub_9",
					Status: "active", Quantity: 1}
				if err := store.CreateSubscription(ctx, sub); err != nil {
					t.Fatalf("CreateSubscription: %v", err)
				}
				setStatus(t, store, "acme", sub.ID, "past_due")
				before := mustGet(t, store, "acme", sub.ID)
				since, ok := before.UnpaidSince()
				if !ok {
					t.Fatal("the past_due row answers no unpaid clock")
				}

				got, err := svc.CancelSubscription(ctx, "acme", sub.ID, atPeriodEnd)
				if err != nil {
					t.Fatalf("CancelSubscription: %v", err)
				}
				if len(prov.cancels) != 1 || prov.cancels[0].providerRef != "sub_9" || prov.cancels[0].atPeriodEnd != atPeriodEnd {
					t.Fatalf("provider cancels = %+v, want one of sub_9 with atPeriodEnd=%v", prov.cancels, atPeriodEnd)
				}
				if got.CancelAtPeriodEnd != atPeriodEnd || got.CanceledAt == nil || !got.CanceledAt.Equal(providerInstant) {
					t.Fatalf("answered cancel fields = (%v, %v), want the provider's (%v, %v)",
						got.CancelAtPeriodEnd, got.CanceledAt, atPeriodEnd, providerInstant)
				}
				if got.Status != "past_due" || !got.StatusChangedAt.Equal(before.StatusChangedAt) {
					t.Fatalf("status %q changed at %v, want past_due at %v: the cancel wrote a status", got.Status, got.StatusChangedAt, before.StatusChangedAt)
				}
				wantUnpaid(t, got, since)
				stored := mustGet(t, store, "acme", sub.ID)
				if stored.Status != got.Status || stored.CancelAtPeriodEnd != got.CancelAtPeriodEnd ||
					(stored.CanceledAt == nil) != (got.CanceledAt == nil) ||
					(stored.CanceledAt != nil && !stored.CanceledAt.Equal(*got.CanceledAt)) {
					t.Fatalf("answered %+v, stored %+v: the answer is not the stored row", got, stored)
				}
			})
		})
	}
}

// A provider that refuses the cancel leaves the row untouched: the store is not
// written at all, so a failed provider call cannot leave a local cancel the
// provider never made.
func TestService_CancelSubscriptionWritesNothingWhenTheProviderRefuses(t *testing.T) {
	store := &cancellingStore{MemStore: NewMemStore()}
	refusal := errors.New("provider says no")
	prov := &fakeProvider{cancelErr: refusal}
	svc := newService(t, store, prov)
	sub := &Subscription{Partition: "acme", CustomerID: "c1", PriceID: "p1", ProviderRef: "sub_9", Status: "active", Quantity: 1}
	if err := store.CreateSubscription(ctx, sub); err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}
	if _, err := svc.CancelSubscription(ctx, "acme", sub.ID, false); !errors.Is(err, refusal) {
		t.Fatalf("CancelSubscription = %v, want the provider's refusal", err)
	}
	if len(prov.cancels) != 1 {
		t.Fatalf("provider cancels = %d, want 1", len(prov.cancels))
	}
	if store.cancelWrites != 0 {
		t.Fatalf("the store was written %d times after a provider refusal, want 0", store.cancelWrites)
	}
	got := mustGet(t, store, "acme", sub.ID)
	if got.CancelAtPeriodEnd || got.CanceledAt != nil || got.Status != "active" {
		t.Fatalf("row changed after a refused cancel: %+v", got)
	}
}

// The store write comes after the provider has answered, never before: the
// provider has not been asked when the store is written is the failure this
// test names.
func TestService_CancelSubscriptionWritesTheStoreOnlyAfterTheProviderAnswered(t *testing.T) {
	store := &cancellingStore{MemStore: NewMemStore()}
	prov := &fakeProvider{}
	store.onCancel = func() {
		if len(prov.cancels) != 1 {
			t.Fatalf("the store was written with %d provider cancels made, want 1 made before the write", len(prov.cancels))
		}
	}
	svc := newService(t, store, prov)
	sub := &Subscription{Partition: "acme", CustomerID: "c1", PriceID: "p1", ProviderRef: "sub_9", Status: "active", Quantity: 1}
	if err := store.CreateSubscription(ctx, sub); err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}
	if _, err := svc.CancelSubscription(ctx, "acme", sub.ID, true); err != nil {
		t.Fatalf("CancelSubscription: %v", err)
	}
	if store.cancelWrites != 1 {
		t.Fatalf("cancel writes = %d, want 1", store.cancelWrites)
	}
}

// A subscription with no provider reference has no provider subscription to
// cancel. It is refused with ErrNoProviderSubscription before the provider is
// asked, and nothing is written: an operation that changes a subscription at the
// provider makes no local-only change.
func TestService_CancelSubscriptionRefusesASubscriptionWithoutAProviderSubscription(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		prov := &fakeProvider{}
		svc := newService(t, store, prov)
		sub := &Subscription{Partition: "acme", CustomerID: "c1", PriceID: "p1", Status: "active", Quantity: 1}
		if err := store.CreateSubscription(ctx, sub); err != nil {
			t.Fatalf("CreateSubscription: %v", err)
		}
		if _, err := svc.CancelSubscription(ctx, "acme", sub.ID, false); !errors.Is(err, ErrNoProviderSubscription) {
			t.Fatalf("CancelSubscription = %v, want ErrNoProviderSubscription", err)
		}
		if len(prov.cancels) != 0 {
			t.Fatalf("the provider was asked %d times for a subscription it does not hold", len(prov.cancels))
		}
		got := mustGet(t, store, "acme", sub.ID)
		if got.CancelAtPeriodEnd || got.CanceledAt != nil || got.Status != "active" {
			t.Fatalf("row changed after a refused cancel: %+v", got)
		}
	})
}

// The read is inside the partition named: another partition's subscription and
// an unknown id answer ErrNotFound, and the provider is not asked.
func TestService_CancelSubscriptionReadsOnlyInsideThePartition(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		prov := &fakeProvider{}
		svc := newService(t, store, prov)
		sub := &Subscription{Partition: "acme", CustomerID: "c1", PriceID: "p1", ProviderRef: "sub_9", Status: "active", Quantity: 1}
		if err := store.CreateSubscription(ctx, sub); err != nil {
			t.Fatalf("CreateSubscription: %v", err)
		}
		for _, tc := range []struct{ partition, id string }{{"globex", sub.ID}, {"", sub.ID}, {"acme", "missing"}} {
			if _, err := svc.CancelSubscription(ctx, tc.partition, tc.id, false); !errors.Is(err, ErrNotFound) {
				t.Fatalf("CancelSubscription(%q, %q) = %v, want ErrNotFound", tc.partition, tc.id, err)
			}
		}
		if len(prov.cancels) != 0 {
			t.Fatalf("the provider was asked %d times from outside the partition", len(prov.cancels))
		}
	})
}
