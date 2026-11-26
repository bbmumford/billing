package billing

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// unpaidStore is what the unpaid-clock cases drive: the Store contract plus the
// cross-partition read, which only the concrete stores carry.
type unpaidStore interface {
	Store
	UnpaidReader
}

// stepClock answers the instant it holds; a case moves it between writes so each
// write's instant is known exactly on both stores.
type stepClock struct{ t time.Time }

func (c *stepClock) now() time.Time      { return c.t }
func (c *stepClock) set(t time.Time)     { c.t = t }
func (c *stepClock) day(n int) time.Time { return at.Add(time.Duration(n) * 24 * time.Hour) }

// eachClockedStore runs a case against MemStore and SQLStore with one clock both
// read for their write instants, so the two are held to the same instants.
func eachClockedStore(t *testing.T, fn func(t *testing.T, s unpaidStore, clk *stepClock)) {
	t.Helper()
	t.Run("mem", func(t *testing.T) {
		clk := &stepClock{t: at}
		m := NewMemStore()
		m.now = clk.now
		fn(t, m, clk)
	})
	t.Run("sql", func(t *testing.T) {
		clk := &stepClock{t: at}
		s := NewSQLStore(openDB(t))
		s.now = clk.now
		if err := s.InitSchema(ctx); err != nil {
			t.Fatalf("InitSchema: %v", err)
		}
		fn(t, s, clk)
	})
}

func mustGet(t *testing.T, s Store, partition, id string) *Subscription {
	t.Helper()
	got, err := s.GetSubscription(ctx, partition, id)
	if err != nil {
		t.Fatalf("GetSubscription(%q, %q): %v", partition, id, err)
	}
	return got
}

// setStatus writes the stored row back with a new status, the way the billing
// host's webhook applier does: read the row, change the status, write the row.
func setStatus(t *testing.T, s Store, partition, id, status string) {
	t.Helper()
	row := mustGet(t, s, partition, id)
	row.Status = status
	if err := s.UpdateSubscription(ctx, row); err != nil {
		t.Fatalf("UpdateSubscription(%s): %v", status, err)
	}
}

func wantUnpaid(t *testing.T, sub *Subscription, want time.Time) {
	t.Helper()
	got, ok := sub.UnpaidSince()
	if !ok {
		t.Fatalf("UnpaidSince() = (_, false) for status %q, want (%v, true)", sub.Status, want)
	}
	if !got.Equal(want) {
		t.Fatalf("UnpaidSince() = %v, want %v", got, want)
	}
}

func wantPaid(t *testing.T, sub *Subscription) {
	t.Helper()
	if got, ok := sub.UnpaidSince(); ok {
		t.Fatalf("UnpaidSince() = (%v, true) for status %q, want (_, false)", got, sub.Status)
	}
	if sub.EnteredUnpaidAt != nil {
		t.Fatalf("EnteredUnpaidAt = %v for status %q, want nil", *sub.EnteredUnpaidAt, sub.Status)
	}
}

// The provider reports unpaid and incomplete_expired; both are statuses the
// set accepts, beside the ones it already held.
func TestSubscriptionStatusSet_HoldsTheProviderStatuses(t *testing.T) {
	for _, st := range []string{"incomplete", "incomplete_expired", "trialing", "active", "past_due",
		"unpaid", "downgrade_pending", "paused", "canceled"} {
		if !ValidSubscriptionStatus(st) {
			t.Errorf("ValidSubscriptionStatus(%q) = false, want true", st)
		}
	}
	for _, st := range []string{"", "bogus", "PAST_DUE"} {
		if ValidSubscriptionStatus(st) {
			t.Errorf("ValidSubscriptionStatus(%q) = true, want false", st)
		}
	}
}

// StatusChangedAt is the creation instant, and then moves only on a write whose
// status differs from the stored one. A write that changes the period, the price
// or nothing at all leaves it where it was, while UpdatedAt moves on every write.
func TestStatusChangedAt_MovesOnlyOnAStatusChange(t *testing.T) {
	eachClockedStore(t, func(t *testing.T, s unpaidStore, clk *stepClock) {
		sub := &Subscription{Partition: "acme", CustomerID: "c1", PriceID: "p1", ProviderRef: "sub_1",
			Status: "active", Quantity: 1}
		if err := s.CreateSubscription(ctx, sub); err != nil {
			t.Fatalf("CreateSubscription: %v", err)
		}
		if !sub.StatusChangedAt.Equal(at) {
			t.Fatalf("created StatusChangedAt on the caller's struct = %v, want %v", sub.StatusChangedAt, at)
		}
		if got := mustGet(t, s, "acme", sub.ID); !got.StatusChangedAt.Equal(at) {
			t.Fatalf("stored StatusChangedAt after create = %v, want %v", got.StatusChangedAt, at)
		}

		clk.set(clk.day(1))
		row := mustGet(t, s, "acme", sub.ID)
		end := clk.day(30)
		row.CurrentPeriodEnd = &end
		row.CancelAtPeriodEnd = true
		row.Quantity = 4
		if err := s.UpdateSubscription(ctx, row); err != nil {
			t.Fatalf("UpdateSubscription: %v", err)
		}
		got := mustGet(t, s, "acme", sub.ID)
		if !got.StatusChangedAt.Equal(at) {
			t.Fatalf("StatusChangedAt after a write keeping the status = %v, want %v", got.StatusChangedAt, at)
		}
		if !got.UpdatedAt.Equal(clk.day(1)) {
			t.Fatalf("UpdatedAt = %v, want %v: every write moves it", got.UpdatedAt, clk.day(1))
		}

		clk.set(clk.day(2))
		setStatus(t, s, "acme", sub.ID, "past_due")
		if got := mustGet(t, s, "acme", sub.ID); !got.StatusChangedAt.Equal(clk.day(2)) {
			t.Fatalf("StatusChangedAt after active -> past_due = %v, want %v", got.StatusChangedAt, clk.day(2))
		}

		clk.set(clk.day(3))
		setStatus(t, s, "acme", sub.ID, "past_due")
		if got := mustGet(t, s, "acme", sub.ID); !got.StatusChangedAt.Equal(clk.day(2)) {
			t.Fatalf("StatusChangedAt after a replayed past_due = %v, want %v", got.StatusChangedAt, clk.day(2))
		}
	})
}

// The unpaid clock starts when the subscription enters past_due or unpaid from
// any other status, holds while it moves between the two, and clears when it
// leaves them, whichever status it leaves to. Entering again starts a new clock.
func TestUnpaidSince_PastDueToUnpaidKeepsTheClockAndPaymentClearsIt(t *testing.T) {
	eachClockedStore(t, func(t *testing.T, s unpaidStore, clk *stepClock) {
		sub := &Subscription{Partition: "acme", CustomerID: "c1", PriceID: "p1", ProviderRef: "sub_1",
			Status: "active", Quantity: 1}
		if err := s.CreateSubscription(ctx, sub); err != nil {
			t.Fatalf("CreateSubscription: %v", err)
		}
		wantPaid(t, mustGet(t, s, "acme", sub.ID))

		clk.set(clk.day(1))
		setStatus(t, s, "acme", sub.ID, "past_due")
		wantUnpaid(t, mustGet(t, s, "acme", sub.ID), clk.day(1))

		clk.set(clk.day(4))
		setStatus(t, s, "acme", sub.ID, "unpaid")
		got := mustGet(t, s, "acme", sub.ID)
		wantUnpaid(t, got, clk.day(1))
		if !got.StatusChangedAt.Equal(clk.day(4)) {
			t.Fatalf("StatusChangedAt after past_due -> unpaid = %v, want %v: the status did change", got.StatusChangedAt, clk.day(4))
		}

		clk.set(clk.day(5))
		setStatus(t, s, "acme", sub.ID, "past_due")
		wantUnpaid(t, mustGet(t, s, "acme", sub.ID), clk.day(1))

		clk.set(clk.day(6))
		setStatus(t, s, "acme", sub.ID, "active")
		wantPaid(t, mustGet(t, s, "acme", sub.ID))

		clk.set(clk.day(9))
		setStatus(t, s, "acme", sub.ID, "unpaid")
		wantUnpaid(t, mustGet(t, s, "acme", sub.ID), clk.day(9))

		for i, leave := range []string{"trialing", "canceled"} {
			clk.set(clk.day(10 + 2*i))
			setStatus(t, s, "acme", sub.ID, leave)
			wantPaid(t, mustGet(t, s, "acme", sub.ID))
			clk.set(clk.day(11 + 2*i))
			setStatus(t, s, "acme", sub.ID, "past_due")
			wantUnpaid(t, mustGet(t, s, "acme", sub.ID), clk.day(11+2*i))
		}
	})
}

// A subscription created already past due starts its clock at creation.
func TestUnpaidSince_StartsAtCreationForAnUnpaidStatus(t *testing.T) {
	eachClockedStore(t, func(t *testing.T, s unpaidStore, clk *stepClock) {
		clk.set(clk.day(2))
		sub := &Subscription{Partition: "acme", CustomerID: "c1", PriceID: "p1", Status: "past_due"}
		if err := s.CreateSubscription(ctx, sub); err != nil {
			t.Fatalf("CreateSubscription: %v", err)
		}
		wantUnpaid(t, sub, clk.day(2))
		wantUnpaid(t, mustGet(t, s, "acme", sub.ID), clk.day(2))
	})
}

// The store keeps both instants itself. A caller handing in a subscription with
// its own StatusChangedAt or EnteredUnpaidAt, on create or on update, moves
// neither: the clock that decides a suspension is not a value a writer supplies.
func TestUnpaidClock_IgnoresTheCallersInstants(t *testing.T) {
	eachClockedStore(t, func(t *testing.T, s unpaidStore, clk *stepClock) {
		early := at.Add(-365 * 24 * time.Hour)
		sub := &Subscription{Partition: "acme", CustomerID: "c1", PriceID: "p1", Status: "past_due",
			StatusChangedAt: early, EnteredUnpaidAt: &early}
		if err := s.CreateSubscription(ctx, sub); err != nil {
			t.Fatalf("CreateSubscription: %v", err)
		}
		got := mustGet(t, s, "acme", sub.ID)
		wantUnpaid(t, got, at)
		if !got.StatusChangedAt.Equal(at) {
			t.Fatalf("StatusChangedAt = %v, want the creation instant %v", got.StatusChangedAt, at)
		}

		clk.set(clk.day(1))
		got.EnteredUnpaidAt = &early
		got.StatusChangedAt = early
		got.Status = "unpaid"
		if err := s.UpdateSubscription(ctx, got); err != nil {
			t.Fatalf("UpdateSubscription: %v", err)
		}
		again := mustGet(t, s, "acme", sub.ID)
		wantUnpaid(t, again, at)
		if !again.StatusChangedAt.Equal(clk.day(1)) {
			t.Fatalf("StatusChangedAt = %v, want the write instant %v", again.StatusChangedAt, clk.day(1))
		}

		clk.set(clk.day(2))
		again.Status = "active"
		again.EnteredUnpaidAt = &early
		if err := s.UpdateSubscription(ctx, again); err != nil {
			t.Fatalf("UpdateSubscription: %v", err)
		}
		wantPaid(t, mustGet(t, s, "acme", sub.ID))
	})
}

// ListUnpaidSince answers, across every partition, the subscriptions whose
// unpaid clock is at or before the instant named, oldest clock first. A paid
// subscription, a clock after the instant and a row held under no partition are
// left out.
func TestListUnpaidSince_AnswersEveryPartitionAtOrBeforeTheInstant(t *testing.T) {
	eachClockedStore(t, func(t *testing.T, s unpaidStore, clk *stepClock) {
		create := func(partition, status string, day int) string {
			t.Helper()
			clk.set(clk.day(day))
			sub := &Subscription{Partition: partition, CustomerID: "c-" + partition, PriceID: "p1", Status: "active"}
			if err := s.CreateSubscription(ctx, sub); err != nil {
				t.Fatalf("CreateSubscription: %v", err)
			}
			if status != "active" {
				setStatus(t, s, partition, sub.ID, status)
			}
			return sub.ID
		}
		acme := create("acme", "past_due", 1)
		globex := create("globex", "unpaid", 3)
		create("initech", "past_due", 8)
		create("umbrella", "active", 0)
		create("hooli", "canceled", 0)

		// A row with no partition is refused, so none is stored to be listed.
		clk.set(clk.day(0))
		blank := &Subscription{CustomerID: "c-blank", PriceID: "p1", Status: "past_due"}
		if err := s.CreateSubscription(ctx, blank); !errors.Is(err, ErrNoPartition) {
			t.Fatalf("CreateSubscription with no partition = %v, want ErrNoPartition", err)
		}

		cases := []struct {
			before time.Time
			want   []string
		}{
			{clk.day(0), nil},
			{clk.day(1).Add(-time.Nanosecond), nil},
			{clk.day(1), []string{acme}},
			{clk.day(3), []string{acme, globex}},
			{clk.day(7), []string{acme, globex}},
		}
		for _, tc := range cases {
			got, err := s.ListUnpaidSince(ctx, tc.before)
			if err != nil {
				t.Fatalf("ListUnpaidSince(%v): %v", tc.before, err)
			}
			var ids []string
			for _, sub := range got {
				ids = append(ids, sub.ID)
				if _, ok := sub.UnpaidSince(); !ok {
					t.Errorf("ListUnpaidSince answered %s with status %q and no clock", sub.ID, sub.Status)
				}
			}
			if !reflect.DeepEqual(ids, tc.want) {
				t.Fatalf("ListUnpaidSince(day %v) = %v, want %v", tc.before.Sub(at)/(24*time.Hour), ids, tc.want)
			}
		}
		got, _ := s.ListUnpaidSince(ctx, clk.day(3))
		if got[0].Partition != "acme" || got[1].Partition != "globex" {
			t.Fatalf("partitions = %q, %q, want acme, globex", got[0].Partition, got[1].Partition)
		}

		setStatus(t, s, "acme", acme, "active")
		got, _ = s.ListUnpaidSince(ctx, clk.day(7))
		if len(got) != 1 || got[0].ID != globex {
			t.Fatalf("after acme pays, ListUnpaidSince = %d rows, want only globex", len(got))
		}
	})
}

// The cross-partition read is not part of the Store contract. The billing host
// holds a Store and every RPC operation reaches the store through it, so the
// read stays out of reach of the RPC surface; the rule's composition reaches
// it on the concrete store.
func TestListUnpaidSince_IsNotOnTheStoreContract(t *testing.T) {
	storeType := reflect.TypeOf((*Store)(nil)).Elem()
	for _, name := range []string{"ListUnpaidSince"} {
		if _, ok := storeType.MethodByName(name); ok {
			t.Fatalf("Store carries %s; a holder of the partition-scoped contract would reach every partition", name)
		}
	}
	var _ UnpaidReader = (*SQLStore)(nil)
	var _ UnpaidReader = (*MemStore)(nil)
}

// The comparison is against the row as stored when the write lands, not as the
// writer read it. A provider event moving the row to past_due between a writer's
// read and its write of past_due leaves the event's instants standing.
func TestUnpaidClock_ComparesWithTheRowAsStoredAtTheWrite(t *testing.T) {
	eachClockedStore(t, func(t *testing.T, s unpaidStore, clk *stepClock) {
		sub := &Subscription{Partition: "acme", CustomerID: "c1", PriceID: "p1", Status: "active"}
		if err := s.CreateSubscription(ctx, sub); err != nil {
			t.Fatalf("CreateSubscription: %v", err)
		}
		stale := mustGet(t, s, "acme", sub.ID)

		clk.set(clk.day(1))
		setStatus(t, s, "acme", sub.ID, "past_due")

		clk.set(clk.day(2))
		stale.Status = "past_due"
		if err := s.UpdateSubscription(ctx, stale); err != nil {
			t.Fatalf("UpdateSubscription: %v", err)
		}
		got := mustGet(t, s, "acme", sub.ID)
		wantUnpaid(t, got, clk.day(1))
		if !got.StatusChangedAt.Equal(clk.day(1)) {
			t.Fatalf("StatusChangedAt = %v, want the event's %v", got.StatusChangedAt, clk.day(1))
		}
	})
}

// UnpaidSince answers a clock only for a status in the unpaid set that carries
// one: a status outside the set answers false whatever EnteredUnpaidAt holds, and
// a status in the set with no instant answers false rather than a zero clock.
func TestUnpaidSince_AnswersOnlyAnUnpaidStatusWithAClock(t *testing.T) {
	when := at.Add(time.Hour)
	for _, tc := range []struct {
		sub  *Subscription
		want bool
	}{
		{&Subscription{Status: "past_due", EnteredUnpaidAt: &when, StatusChangedAt: at}, true},
		{&Subscription{Status: "unpaid", EnteredUnpaidAt: &when, StatusChangedAt: at}, true},
		{&Subscription{Status: "active", EnteredUnpaidAt: &when}, false},
		{&Subscription{Status: "canceled", EnteredUnpaidAt: &when}, false},
		{&Subscription{Status: "incomplete_expired", EnteredUnpaidAt: &when}, false},
		{&Subscription{Status: "past_due", StatusChangedAt: at}, false},
		{nil, false},
	} {
		got, ok := tc.sub.UnpaidSince()
		if ok != tc.want || (ok && !got.Equal(when)) || (!ok && !got.IsZero()) {
			t.Errorf("UnpaidSince(%+v) = (%v, %v), want (%v, %v)", tc.sub, got, ok, when, tc.want)
		}
	}
}
