package billing

import (
	"errors"
	"testing"
	"time"
)

// UpdateSubscriptionItem writes a subscription's price and quantity and leaves every
// other column as stored. The status written by UpdateSubscription after the item's
// caller read the row stays, which is what lets an item change run beside the
// provider event that owns the status.
func TestUpdateSubscriptionItem_WritesPriceAndQuantityAndNeverStatus(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		start, end := at, at.Add(30*24*time.Hour)
		sub := &Subscription{Partition: "acme", CustomerID: "c1", PriceID: "p1", ProviderRef: "sub_1",
			Status: "active", Quantity: 1, CurrentPeriodStart: &start, CurrentPeriodEnd: &end, CreatedAt: at}
		if err := s.CreateSubscription(ctx, sub); err != nil {
			t.Fatalf("CreateSubscription: %v", err)
		}
		read, err := s.GetSubscription(ctx, "acme", sub.ID)
		if err != nil {
			t.Fatalf("GetSubscription: %v", err)
		}

		event := *read
		event.Status = "past_due"
		event.CancelAtPeriodEnd = true
		if err := s.UpdateSubscription(ctx, &event); err != nil {
			t.Fatalf("UpdateSubscription: %v", err)
		}
		before, _ := s.GetSubscription(ctx, "acme", sub.ID)

		if err := s.UpdateSubscriptionItem(ctx, "acme", sub.ID, "p2", 3); err != nil {
			t.Fatalf("UpdateSubscriptionItem: %v", err)
		}
		got, err := s.GetSubscription(ctx, "acme", sub.ID)
		if err != nil {
			t.Fatalf("GetSubscription: %v", err)
		}
		if got.PriceID != "p2" || got.Quantity != 3 {
			t.Fatalf("item = (%q, %d), want (p2, 3)", got.PriceID, got.Quantity)
		}
		if got.Status != "past_due" {
			t.Fatalf("status = %q, want the %q written after the read", got.Status, "past_due")
		}
		if !got.CancelAtPeriodEnd || got.ProviderRef != "sub_1" || got.CustomerID != "c1" {
			t.Fatalf("other columns moved: %+v", got)
		}
		if got.CurrentPeriodStart == nil || !got.CurrentPeriodStart.Equal(start) ||
			got.CurrentPeriodEnd == nil || !got.CurrentPeriodEnd.Equal(end) {
			t.Fatalf("period moved: %v..%v, want %v..%v", got.CurrentPeriodStart, got.CurrentPeriodEnd, start, end)
		}
		if !got.CreatedAt.Equal(before.CreatedAt) || got.UpdatedAt.Before(before.UpdatedAt) {
			t.Fatalf("times: created %v (was %v), updated %v (was %v)", got.CreatedAt, before.CreatedAt, got.UpdatedAt, before.UpdatedAt)
		}
	})
}

// The write is partition-scoped like every other: another partition, an empty
// partition and an unknown id each answer ErrNotFound and change nothing.
func TestUpdateSubscriptionItem_ReachesOnlyItsPartition(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		sub := &Subscription{Partition: "acme", CustomerID: "c1", PriceID: "p1", Status: "active", Quantity: 1}
		if err := s.CreateSubscription(ctx, sub); err != nil {
			t.Fatalf("CreateSubscription: %v", err)
		}
		for _, tc := range []struct{ partition, id string }{
			{"globex", sub.ID},
			{"", sub.ID},
			{"acme", "missing"},
		} {
			if err := s.UpdateSubscriptionItem(ctx, tc.partition, tc.id, "p9", 9); !errors.Is(err, ErrNotFound) {
				t.Fatalf("UpdateSubscriptionItem(%q, %q) = %v, want ErrNotFound", tc.partition, tc.id, err)
			}
		}
		got, _ := s.GetSubscription(ctx, "acme", sub.ID)
		if got.PriceID != "p1" || got.Quantity != 1 {
			t.Fatalf("a refused write changed the row: %+v", got)
		}
	})
}
