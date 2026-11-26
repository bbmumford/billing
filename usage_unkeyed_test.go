package billing

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A usage report with neither an idempotency key nor an ID is refused with
// ErrIdempotencyKeyRequired and stores nothing, on both stores: a record the
// store could not recognise again would count twice on every retry.
func TestAnUnkeyedUsageReportIsRefused(t *testing.T) {
	ctx := context.Background()
	at := time.Unix(1_700_000_000, 0).UTC()
	eachStore(t, func(t *testing.T, s Store) {
		for i := 0; i < 2; i++ {
			if err := s.RecordUsage(ctx, &UsageRecord{Partition: "pa", CustomerID: "c1", ProductID: "ot", Quantity: 7, RecordedAt: at}); !errors.Is(err, ErrIdempotencyKeyRequired) {
				t.Fatalf("unkeyed report %d = %v, want ErrIdempotencyKeyRequired", i, err)
			}
		}
		if sum, _ := s.SummarizeUsage(ctx, "pa", "c1", at, at.Add(time.Minute)); len(sum) != 0 {
			t.Fatalf("unkeyed reports were stored: %+v", sum)
		}
	})
}
