package billing

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A retried usage report is counted once, on both stores: a report carrying
// the key a stored record was reported under is answered that record and
// stores nothing, another partition's report under the key is its own record,
// and the summary counts each report once.
func TestARetriedUsageReportCountsOnce(t *testing.T) {
	ctx := context.Background()
	at := time.Unix(1_700_000_000, 0).UTC()
	eachStore(t, func(t *testing.T, s Store) {
		for i := 0; i < 3; i++ {
			u := &UsageRecord{IdempotencyKey: "event-1", Partition: "pa", CustomerID: "c1", ProductID: "ot", Quantity: 7, RecordedAt: at.Add(time.Duration(i) * time.Second)}
			if err := s.RecordUsage(ctx, u); err != nil {
				t.Fatalf("report %d: %v", i, err)
			}
			if !u.RecordedAt.Equal(at) {
				t.Fatalf("retry %d answered RecordedAt %v, want the stored record's %v", i, u.RecordedAt, at)
			}
		}
		if err := s.RecordUsage(ctx, &UsageRecord{IdempotencyKey: "event-1", Partition: "pb", CustomerID: "c1", ProductID: "ot", Quantity: 7, RecordedAt: at}); err != nil {
			t.Fatalf("pb's report under the key: %v", err)
		}
		for part, want := range map[string]int64{"pa": 7, "pb": 7} {
			sum, err := s.SummarizeUsage(ctx, part, "c1", at, at.Add(time.Minute))
			if err != nil || len(sum) != 1 || sum[0].TotalQuantity != want {
				t.Fatalf("%s summary = %+v, %v; want one product totalling %d", part, sum, err, want)
			}
		}
	})
}

// A usage report under a key the partition holds for another report is
// refused with ErrIdempotencyConflict and stores nothing.
func TestAUsageReportIsKeyed(t *testing.T) {
	ctx := context.Background()
	at := time.Unix(1_700_000_000, 0).UTC()
	eachStore(t, func(t *testing.T, s Store) {
		if err := s.RecordUsage(ctx, &UsageRecord{IdempotencyKey: "event-1", Partition: "pa", CustomerID: "c1", ProductID: "ot", Quantity: 7, RecordedAt: at}); err != nil {
			t.Fatal(err)
		}
		if err := s.RecordUsage(ctx, &UsageRecord{IdempotencyKey: "event-1", Partition: "pa", CustomerID: "c1", ProductID: "ot", Quantity: 8, RecordedAt: at}); !errors.Is(err, ErrIdempotencyConflict) {
			t.Fatalf("another quantity under the key = %v, want ErrIdempotencyConflict", err)
		}
		sum, err := s.SummarizeUsage(ctx, "pa", "c1", at, at.Add(time.Minute))
		if err != nil || len(sum) != 1 || sum[0].TotalQuantity != 7 {
			t.Fatalf("summary = %+v, %v; want 7 alone", sum, err)
		}
	})
}
