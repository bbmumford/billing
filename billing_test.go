package billing

import (
	"context"
	"errors"
	"testing"
	"time"
)

var ctx = context.Background()

// at is a fixed base time; cases add explicit offsets where ordering/expiry matters.
var at = time.Unix(1_000_000, 0).UTC()

func TestCustomer_CreateGetPartitionScopeAndProviderRef(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		c := &Customer{Partition: "acme", UserID: "u1", BillingEmail: "a@x", Currency: "usd"}
		if err := s.CreateCustomer(ctx, c); err != nil || c.ID == "" {
			t.Fatalf("create: %v", err)
		}
		if _, err := s.GetCustomer(ctx, "globex", c.ID); !errors.Is(err, ErrNotFound) {
			t.Fatal("a different partition must not see the customer")
		}
		if err := s.SetCustomerProviderRef(ctx, "acme", c.ID, "cus_123"); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetCustomerByProviderRef(ctx, "cus_123")
		if err != nil || got.ID != c.ID {
			t.Fatalf("by provider ref: %+v %v", got, err)
		}
		if u, err := s.GetCustomerByUser(ctx, "acme", "u1"); err != nil || u.ID != c.ID {
			t.Fatalf("by user: %+v %v", u, err)
		}
	})
}

func TestSubscription_StatusValidationAndUpdate(t *testing.T) {
	if !ValidSubscriptionStatus("downgrade_pending") || ValidSubscriptionStatus("bogus") {
		t.Fatal("status validation set is wrong")
	}
	eachStore(t, func(t *testing.T, s Store) {
		sub := &Subscription{Partition: "acme", CustomerID: "c1", PriceID: "p1", Status: "active", Quantity: 2}
		s.CreateSubscription(ctx, sub)
		sub.Status = "downgrade_pending"
		if err := s.UpdateSubscription(ctx, sub); err != nil {
			t.Fatal(err)
		}
		got, _ := s.GetSubscription(ctx, "acme", sub.ID)
		if got.Status != "downgrade_pending" || got.UpdatedAt.IsZero() || got.Quantity != 2 {
			t.Fatalf("update: %+v", got)
		}
		if subs, _ := s.ListSubscriptionsByCustomer(ctx, "acme", "c1"); len(subs) != 1 {
			t.Fatalf("list by customer: got %d", len(subs))
		}
	})
}

func TestInvoice_WithItemsAndPaidStatus(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		inv := &Invoice{Partition: "acme", CustomerID: "c1", Currency: "usd", Status: "open", AmountDueCents: 3000}
		items := []*InvoiceItem{
			{ProductID: "prod1", Description: "seat", Quantity: 3, UnitAmountCents: 1000, AmountCents: ComputeItemAmount(3, 1000)},
		}
		if err := s.CreateInvoice(ctx, inv, items); err != nil {
			t.Fatal(err)
		}
		its, _ := s.ListInvoiceItems(ctx, inv.ID)
		if len(its) != 1 || its[0].AmountCents != 3000 || its[0].InvoiceID != inv.ID {
			t.Fatalf("items: %+v", its)
		}
		if err := s.UpdateInvoiceStatus(ctx, "acme", inv.ID, "paid", 3000); err != nil {
			t.Fatal(err)
		}
		got, _ := s.GetInvoice(ctx, "acme", inv.ID)
		if got.Status != "paid" || got.AmountPaidCents != 3000 || got.PaidAt == nil {
			t.Fatalf("paid: %+v", got)
		}
	})
}

func TestPaymentLedger_IdempotentRecordAndStatus(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		row := &PaymentLedgerRow{IdempotencyKey: "idem-1", Partition: "acme", CustomerID: "c1", AmountCents: 500, Currency: "usd", Status: "pending", ClientSecretSealed: "sealed"}
		existing, err := s.RecordPayment(ctx, row)
		if err != nil || existing != nil {
			t.Fatalf("first record must insert (existing nil); got existing=%v err=%v", existing, err)
		}
		dup, err := s.RecordPayment(ctx, &PaymentLedgerRow{IdempotencyKey: "idem-1", Partition: "acme", CustomerID: "c1", AmountCents: 500, Currency: "usd", Status: "pending"})
		if err != nil || dup == nil {
			t.Fatalf("retry must return the existing row; got dup=%v err=%v", dup, err)
		}
		if err := s.UpdatePaymentStatus(ctx, "idem-1", "succeeded", "sealed-charge"); err != nil {
			t.Fatal(err)
		}
		got, _ := s.GetPaymentByKey(ctx, "idem-1")
		if got.Status != "succeeded" || got.ChargeRefSealed != "sealed-charge" {
			t.Fatalf("status: %+v", got)
		}
		if ps, _ := s.ListPaymentsByCustomer(ctx, "acme", "c1"); len(ps) != 1 {
			t.Fatalf("one payment per idempotency key; got %d", len(ps))
		}
	})
}

func TestUsage_SummarizeWindow(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		for i, q := range []int64{5, 10, 3} {
			s.RecordUsage(ctx, &UsageRecord{Partition: "acme", CustomerID: "c1", ProductID: "api", Quantity: q, RecordedAt: at.Add(time.Duration(i) * time.Minute)})
		}
		// one record outside the window must not count
		s.RecordUsage(ctx, &UsageRecord{Partition: "acme", CustomerID: "c1", ProductID: "api", Quantity: 99, RecordedAt: at.Add(time.Hour)})
		sum, _ := s.SummarizeUsage(ctx, "acme", "c1", at, at.Add(10*time.Minute))
		if len(sum) != 1 || sum[0].TotalQuantity != 18 || sum[0].PeakQuantity != 10 {
			t.Fatalf("summary: %+v", sum)
		}
	})
}

func TestCredit_BalanceExcludesExpired(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		now := at
		s.GrantCredit(ctx, &Credit{Partition: "acme", CustomerID: "c1", AmountCents: 1000, Currency: "usd"})
		exp := now.Add(-time.Hour)
		s.GrantCredit(ctx, &Credit{Partition: "acme", CustomerID: "c1", AmountCents: 500, Currency: "usd", ExpiresAt: &exp})
		future := now.Add(time.Hour)
		s.GrantCredit(ctx, &Credit{Partition: "acme", CustomerID: "c1", AmountCents: 200, Currency: "eur", ExpiresAt: &future})
		bal, _ := s.CreditBalance(ctx, "acme", "c1", now)
		if bal["usd"] != 1000 || bal["eur"] != 200 {
			t.Fatalf("balance must exclude the expired usd credit; got %+v", bal)
		}
	})
}

// TestCustomer_EmptyPartitionReachesNothing pins the partition boundary on billing
// records. An empty partition used to drop the partition predicate across
// twenty-nine store queries, so a caller that left the field unset read and
// mutated another partition's customers, subscriptions, credits and usage.
//
// It compares three outcomes rather than checking each call against what that call
// expected: the owning partition reads its customer, a wrong one and an empty one
// do not, and the empty one must not behave like the owning one. A case that only
// asserts the intended call succeeded cannot tell a working filter from one that
// admits everyone — which is how twenty-nine of these survived.
func TestCustomer_EmptyPartitionReachesNothing(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		c := &Customer{Partition: "acme", UserID: "u1", BillingEmail: "a@x", Currency: "usd"}
		if err := s.CreateCustomer(ctx, c); err != nil {
			t.Fatalf("create: %v", err)
		}

		if _, err := s.GetCustomer(ctx, "acme", c.ID); err != nil {
			t.Fatalf("the owning partition must read its own customer: %v", err)
		}
		if _, err := s.GetCustomer(ctx, "globex", c.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a different partition must not read the customer, got %v", err)
		}
		if _, err := s.GetCustomer(ctx, "", c.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("an empty partition must not read the customer, got %v", err)
		}
		if _, err := s.GetCustomerByUser(ctx, "", "u1"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("an empty partition must not find a customer by user, got %v", err)
		}

		// The mutate half. A write across the boundary changes what another
		// partition is charged, so it is asserted separately from the reads.
		if err := s.SetCustomerProviderRef(ctx, "", c.ID, "cus_stolen"); err == nil {
			if got, gerr := s.GetCustomer(ctx, "acme", c.ID); gerr == nil && got.ProviderRef == "cus_stolen" {
				t.Fatal("an empty partition rewrote another partition's provider ref")
			}
		}
		if err := s.SetCustomerProviderRef(ctx, "acme", c.ID, "cus_123"); err != nil {
			t.Fatalf("the owning partition must set its own provider ref: %v", err)
		}
	})
}
