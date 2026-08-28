package billing

import (
	"context"
	"errors"
	"testing"
	"time"
)

var ctx = context.Background()

func storeAt(t time.Time) *MemStore {
	m := NewMemStore()
	m.now = func() time.Time { return t }
	return m
}

func TestCustomer_CreateGetOwnerScopeAndProviderRef(t *testing.T) {
	m := storeAt(time.Unix(1000, 0).UTC())
	c := &Customer{Owner: "acme", UserID: "u1", BillingEmail: "a@x", Currency: "usd"}
	if err := m.CreateCustomer(ctx, c); err != nil || c.ID == "" {
		t.Fatalf("create: %v", err)
	}
	if _, err := m.GetCustomer(ctx, "globex", c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("a different owner must not see the customer")
	}
	// link + look up by opaque provider ref
	if err := m.SetCustomerProviderRef(ctx, "acme", c.ID, "cus_123"); err != nil {
		t.Fatal(err)
	}
	got, err := m.GetCustomerByProviderRef(ctx, "cus_123")
	if err != nil || got.ID != c.ID {
		t.Fatalf("by provider ref: %+v %v", got, err)
	}
	// look up by opaque user id, owner-scoped
	if u, err := m.GetCustomerByUser(ctx, "acme", "u1"); err != nil || u.ID != c.ID {
		t.Fatalf("by user: %+v %v", u, err)
	}
}

func TestSubscription_StatusValidationAndUpdate(t *testing.T) {
	if !ValidSubscriptionStatus("downgrade_pending") || ValidSubscriptionStatus("bogus") {
		t.Fatal("status validation set is wrong")
	}
	m := storeAt(time.Unix(2000, 0).UTC())
	s := &Subscription{Owner: "acme", CustomerID: "c1", PriceID: "p1", Status: "active", Quantity: 2}
	m.CreateSubscription(ctx, s)
	s.Status = "downgrade_pending"
	if err := m.UpdateSubscription(ctx, s); err != nil {
		t.Fatal(err)
	}
	got, _ := m.GetSubscription(ctx, "acme", s.ID)
	if got.Status != "downgrade_pending" || got.UpdatedAt.IsZero() {
		t.Fatalf("update: %+v", got)
	}
	if subs, _ := m.ListSubscriptionsByCustomer(ctx, "acme", "c1"); len(subs) != 1 {
		t.Fatalf("list by customer: got %d", len(subs))
	}
}

func TestInvoice_WithItemsAndPaidStatus(t *testing.T) {
	m := storeAt(time.Unix(3000, 0).UTC())
	inv := &Invoice{Owner: "acme", CustomerID: "c1", Currency: "usd", Status: "open", AmountDueCents: 3000}
	items := []*InvoiceItem{
		{ProductID: "prod1", Description: "seat", Quantity: 3, UnitAmountCents: 1000, AmountCents: ComputeItemAmount(3, 1000)},
	}
	if err := m.CreateInvoice(ctx, inv, items); err != nil {
		t.Fatal(err)
	}
	its, _ := m.ListInvoiceItems(ctx, inv.ID)
	if len(its) != 1 || its[0].AmountCents != 3000 || its[0].InvoiceID != inv.ID {
		t.Fatalf("items: %+v", its)
	}
	if err := m.UpdateInvoiceStatus(ctx, "acme", inv.ID, "paid", 3000); err != nil {
		t.Fatal(err)
	}
	got, _ := m.GetInvoice(ctx, "acme", inv.ID)
	if got.Status != "paid" || got.AmountPaidCents != 3000 || got.PaidAt == nil {
		t.Fatalf("paid: %+v", got)
	}
}

func TestPaymentLedger_IdempotentRecordAndStatus(t *testing.T) {
	m := storeAt(time.Unix(4000, 0).UTC())
	row := &PaymentLedgerRow{IdempotencyKey: "idem-1", Owner: "acme", CustomerID: "c1", AmountCents: 500, Currency: "usd", Status: "pending", ClientSecretSealed: "sealed"}
	existing, err := m.RecordPayment(ctx, row)
	if err != nil || existing != nil {
		t.Fatalf("first record must insert (existing nil); got existing=%v err=%v", existing, err)
	}
	// a retry with the same key returns the existing row and does NOT double-record
	dup, err := m.RecordPayment(ctx, &PaymentLedgerRow{IdempotencyKey: "idem-1", Owner: "acme", CustomerID: "c1", AmountCents: 500, Currency: "usd", Status: "pending"})
	if err != nil || dup == nil {
		t.Fatalf("retry must return the existing row; got dup=%v err=%v", dup, err)
	}
	// status advance + sealed charge ref
	if err := m.UpdatePaymentStatus(ctx, "idem-1", "succeeded", "sealed-charge"); err != nil {
		t.Fatal(err)
	}
	got, _ := m.GetPaymentByKey(ctx, "idem-1")
	if got.Status != "succeeded" || got.ChargeRefSealed != "sealed-charge" {
		t.Fatalf("status: %+v", got)
	}
	if ps, _ := m.ListPaymentsByCustomer(ctx, "acme", "c1"); len(ps) != 1 {
		t.Fatalf("one payment per idempotency key; got %d", len(ps))
	}
}

func TestUsage_SummarizeWindow(t *testing.T) {
	m := NewMemStore()
	base := time.Unix(5000, 0).UTC()
	for i, q := range []int64{5, 10, 3} {
		m.RecordUsage(ctx, &UsageRecord{Owner: "acme", CustomerID: "c1", ProductID: "api", Quantity: q, RecordedAt: base.Add(time.Duration(i) * time.Minute)})
	}
	// one record outside the window must not count
	m.RecordUsage(ctx, &UsageRecord{Owner: "acme", CustomerID: "c1", ProductID: "api", Quantity: 99, RecordedAt: base.Add(time.Hour)})
	sum, _ := m.SummarizeUsage(ctx, "acme", "c1", base, base.Add(10*time.Minute))
	if len(sum) != 1 || sum[0].TotalQuantity != 18 || sum[0].PeakQuantity != 10 {
		t.Fatalf("summary: %+v", sum)
	}
}

func TestCredit_BalanceExcludesExpired(t *testing.T) {
	now := time.Unix(6000, 0).UTC()
	m := storeAt(now)
	m.GrantCredit(ctx, &Credit{Owner: "acme", CustomerID: "c1", AmountCents: 1000, Currency: "usd"})
	exp := now.Add(-time.Hour)
	m.GrantCredit(ctx, &Credit{Owner: "acme", CustomerID: "c1", AmountCents: 500, Currency: "usd", ExpiresAt: &exp})
	future := now.Add(time.Hour)
	m.GrantCredit(ctx, &Credit{Owner: "acme", CustomerID: "c1", AmountCents: 200, Currency: "eur", ExpiresAt: &future})
	bal, _ := m.CreditBalance(ctx, "acme", "c1", now)
	if bal["usd"] != 1000 || bal["eur"] != 200 {
		t.Fatalf("balance must exclude the expired usd credit; got %+v", bal)
	}
}
