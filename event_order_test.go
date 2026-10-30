package billing

import (
	"errors"
	"testing"
	"time"
)

// eventAt is a provider event's created instant n minutes after the fixed test
// clock; the cases order events by it, never by the store clock.
func eventAt(n int) *time.Time {
	t := at.Add(time.Duration(n) * time.Minute)
	return &t
}

// A subscription carries the created instant of the provider event last applied
// to it. Creation stores what the caller hands it (nil for a subscription no
// event has touched); UpdateSubscription stores the caller's instant and keeps
// the stored one when the caller hands none, so a write that is not an event's
// (an item or cancel write read back whole) never clears it.
func TestEventCreatedAt_ARowKeepsTheInstantOfTheEventLastApplied(t *testing.T) {
	eachClockedStore(t, func(t *testing.T, s unpaidStore, clk *stepClock) {
		sub := &Subscription{Partition: "acme", CustomerID: "c1", PriceID: "p1", ProviderRef: "sub_1",
			Status: "active", Quantity: 1}
		if err := s.CreateSubscription(ctx, sub); err != nil {
			t.Fatalf("CreateSubscription: %v", err)
		}
		if got := mustGet(t, s, "acme", sub.ID); got.EventCreatedAt != nil {
			t.Fatalf("EventCreatedAt after create = %v, want nil", *got.EventCreatedAt)
		}

		row := mustGet(t, s, "acme", sub.ID)
		row.Status = "past_due"
		row.EventCreatedAt = eventAt(10)
		if err := s.UpdateSubscription(ctx, row); err != nil {
			t.Fatalf("UpdateSubscription(event 10): %v", err)
		}
		got := mustGet(t, s, "acme", sub.ID)
		if got.EventCreatedAt == nil || !got.EventCreatedAt.Equal(*eventAt(10)) {
			t.Fatalf("EventCreatedAt after the first event = %v, want %v", got.EventCreatedAt, *eventAt(10))
		}

		row = mustGet(t, s, "acme", sub.ID)
		row.EventCreatedAt = nil
		row.Quantity = 3
		if err := s.UpdateSubscription(ctx, row); err != nil {
			t.Fatalf("UpdateSubscription(no instant): %v", err)
		}
		got = mustGet(t, s, "acme", sub.ID)
		if got.EventCreatedAt == nil || !got.EventCreatedAt.Equal(*eventAt(10)) {
			t.Fatalf("EventCreatedAt after a write with no instant = %v, want the stored %v", got.EventCreatedAt, *eventAt(10))
		}
		if got.Quantity != 3 {
			t.Fatalf("Quantity after a write with no instant = %d, want 3", got.Quantity)
		}

		row = mustGet(t, s, "acme", sub.ID)
		row.Status = "active"
		row.EventCreatedAt = eventAt(20)
		if err := s.UpdateSubscription(ctx, row); err != nil {
			t.Fatalf("UpdateSubscription(event 20): %v", err)
		}
		got = mustGet(t, s, "acme", sub.ID)
		if got.EventCreatedAt == nil || !got.EventCreatedAt.Equal(*eventAt(20)) {
			t.Fatalf("EventCreatedAt after a later event = %v, want %v", got.EventCreatedAt, *eventAt(20))
		}
		if got.Status != "active" {
			t.Fatalf("Status after a later event = %q, want active", got.Status)
		}
	})
}

// An event created earlier than the one the row carries is refused with
// ErrStaleEvent and writes nothing: the status, the instant, the status clocks
// and UpdatedAt all hold. The provider does not order its deliveries, and the
// store is the last place two deliveries racing can be told apart, so the
// refusal is the store's and not only the applier's.
func TestEventCreatedAt_AnEarlierEventIsRefusedAndWritesNothing(t *testing.T) {
	eachClockedStore(t, func(t *testing.T, s unpaidStore, clk *stepClock) {
		sub := &Subscription{Partition: "acme", CustomerID: "c1", PriceID: "p1", ProviderRef: "sub_1",
			Status: "past_due", Quantity: 1}
		if err := s.CreateSubscription(ctx, sub); err != nil {
			t.Fatalf("CreateSubscription: %v", err)
		}
		clk.set(clk.day(1))
		row := mustGet(t, s, "acme", sub.ID)
		row.Status = "active"
		row.EventCreatedAt = eventAt(20)
		if err := s.UpdateSubscription(ctx, row); err != nil {
			t.Fatalf("UpdateSubscription(event 20): %v", err)
		}
		before := mustGet(t, s, "acme", sub.ID)

		clk.set(clk.day(2))
		stale := mustGet(t, s, "acme", sub.ID)
		stale.Status = "past_due"
		stale.Quantity = 9
		stale.EventCreatedAt = eventAt(10)
		err := s.UpdateSubscription(ctx, stale)
		if !errors.Is(err, ErrStaleEvent) {
			t.Fatalf("UpdateSubscription(event 10 after event 20) = %v, want ErrStaleEvent", err)
		}
		after := mustGet(t, s, "acme", sub.ID)
		if after.Status != "active" || after.Quantity != 1 {
			t.Fatalf("row after the refused write: status %q quantity %d, want active 1", after.Status, after.Quantity)
		}
		if after.EventCreatedAt == nil || !after.EventCreatedAt.Equal(*eventAt(20)) {
			t.Fatalf("EventCreatedAt after the refused write = %v, want %v", after.EventCreatedAt, *eventAt(20))
		}
		if !after.UpdatedAt.Equal(before.UpdatedAt) || !after.StatusChangedAt.Equal(before.StatusChangedAt) {
			t.Fatalf("clocks moved on a refused write: updated %v -> %v, status changed %v -> %v",
				before.UpdatedAt, after.UpdatedAt, before.StatusChangedAt, after.StatusChangedAt)
		}
		if after.EnteredUnpaidAt != nil {
			t.Fatalf("EnteredUnpaidAt after the refused write = %v, want nil", *after.EnteredUnpaidAt)
		}
	})
}

// An event created at the same instant as the one the row carries is applied:
// the provider's created instant has one-second grain, so two distinct events
// may share it, and the second is not dropped. A row carrying no instant takes
// any event. An absent row still answers ErrNotFound, not ErrStaleEvent.
func TestEventCreatedAt_TheSameInstantAndANoInstantRowAreApplied(t *testing.T) {
	eachClockedStore(t, func(t *testing.T, s unpaidStore, clk *stepClock) {
		sub := &Subscription{Partition: "acme", CustomerID: "c1", PriceID: "p1", ProviderRef: "sub_1",
			Status: "active", Quantity: 1}
		if err := s.CreateSubscription(ctx, sub); err != nil {
			t.Fatalf("CreateSubscription: %v", err)
		}
		row := mustGet(t, s, "acme", sub.ID)
		row.EventCreatedAt = eventAt(10)
		row.Status = "past_due"
		if err := s.UpdateSubscription(ctx, row); err != nil {
			t.Fatalf("UpdateSubscription(event 10 onto a row with no instant): %v", err)
		}
		row = mustGet(t, s, "acme", sub.ID)
		row.EventCreatedAt = eventAt(10)
		row.Quantity = 2
		if err := s.UpdateSubscription(ctx, row); err != nil {
			t.Fatalf("UpdateSubscription(event 10 again): %v", err)
		}
		if got := mustGet(t, s, "acme", sub.ID); got.Quantity != 2 || got.Status != "past_due" {
			t.Fatalf("row after an equal-instant write: status %q quantity %d, want past_due 2", got.Status, got.Quantity)
		}
		absent := &Subscription{ID: "missing", Partition: "acme", Status: "active", EventCreatedAt: eventAt(10)}
		if err := s.UpdateSubscription(ctx, absent); !errors.Is(err, ErrNotFound) {
			t.Fatalf("UpdateSubscription(absent row) = %v, want ErrNotFound", err)
		}
		wrongPartition := mustGet(t, s, "acme", sub.ID)
		wrongPartition.Partition = "other"
		wrongPartition.EventCreatedAt = eventAt(5)
		if err := s.UpdateSubscription(ctx, wrongPartition); !errors.Is(err, ErrNotFound) {
			t.Fatalf("UpdateSubscription(row under another partition) = %v, want ErrNotFound", err)
		}
	})
}
