package billing

import (
	"errors"
	"testing"
)

// invoiceWithStatus stores a partition acme invoice for c1 with the status given and answers it.
func invoiceWithStatus(t *testing.T, s Store, status string) *Invoice {
	t.Helper()
	inv := &Invoice{Partition: "acme", CustomerID: "c1", ProviderRef: "in_1", Status: status, Currency: "usd", AmountDueCents: 3000, CreatedAt: at}
	if err := s.CreateInvoice(ctx, inv, nil); err != nil {
		t.Fatalf("CreateInvoice: %v", err)
	}
	return inv
}

// paymentWithStatus records a partition acme ledger row under key k with the status given.
func paymentWithStatus(t *testing.T, s Store, status string) {
	t.Helper()
	if _, err := s.RecordPayment(ctx, &PaymentLedgerRow{IdempotencyKey: "k", Partition: "acme", CustomerID: "c1",
		AmountCents: 500, Currency: "usd", Status: status, ChargeRefSealed: "sealed-first"}); err != nil {
		t.Fatalf("RecordPayment: %v", err)
	}
}

// A paid invoice stays paid: a payment_failed event delivered after the paid one (the provider
// does not order its deliveries) is refused with ErrStatusOutOfOrder and changes neither the
// status nor the amount paid; the paid event delivered again is applied and changes nothing.
func TestUpdateInvoiceStatus_ALateFailureDoesNotUnpayAnInvoice(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		inv := invoiceWithStatus(t, s, "open")
		if err := s.UpdateInvoiceStatus(ctx, "acme", inv.ID, "paid", 3000); err != nil {
			t.Fatalf("open -> paid: %v", err)
		}
		if err := s.UpdateInvoiceStatus(ctx, "acme", inv.ID, "payment_failed", 0); !errors.Is(err, ErrStatusOutOfOrder) {
			t.Fatalf("paid -> payment_failed = %v, want ErrStatusOutOfOrder", err)
		}
		got, _ := s.GetInvoice(ctx, "acme", inv.ID)
		if got.Status != "paid" || got.AmountPaidCents != 3000 || got.PaidAt == nil {
			t.Fatalf("invoice after the late failure = %+v, want paid 3000 kept", got)
		}
		if err := s.UpdateInvoiceStatus(ctx, "acme", inv.ID, "paid", 3000); err != nil {
			t.Fatalf("paid delivered again: %v", err)
		}
		if err := s.UpdateInvoiceStatus(ctx, "globex", inv.ID, "paid", 3000); !errors.Is(err, ErrNotFound) {
			t.Fatalf("another partition's write = %v, want ErrNotFound", err)
		}
		if err := s.UpdateInvoiceStatus(ctx, "acme", "nope", "paid", 1); !errors.Is(err, ErrNotFound) {
			t.Fatalf("absent invoice = %v, want ErrNotFound", err)
		}
	})
}

// A payment that succeeded stays succeeded: a late failure (requires_payment_method) or cancel is
// refused with ErrStatusOutOfOrder and writes neither the status nor the charge ref, while the
// provider's moves among the statuses short of an outcome are applied.
func TestUpdatePaymentStatus_ALateFailureDoesNotUndoASuccess(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		paymentWithStatus(t, s, "requires_confirmation")
		for _, st := range []string{"processing", "requires_payment_method", "processing", "succeeded"} {
			if err := s.UpdatePaymentStatus(ctx, "acme", "k", st, ""); err != nil {
				t.Fatalf("-> %s: %v", st, err)
			}
		}
		for _, st := range []string{"requires_payment_method", "processing", "canceled"} {
			if err := s.UpdatePaymentStatus(ctx, "acme", "k", st, "sealed-late"); !errors.Is(err, ErrStatusOutOfOrder) {
				t.Fatalf("succeeded -> %s = %v, want ErrStatusOutOfOrder", st, err)
			}
		}
		got, _ := s.GetPaymentByKey(ctx, "acme", "k")
		if got.Status != "succeeded" || got.ChargeRefSealed != "sealed-first" {
			t.Fatalf("row after the late writes = %+v, want succeeded with the first charge ref", got)
		}
		if err := s.UpdatePaymentStatus(ctx, "globex", "k", "succeeded", ""); !errors.Is(err, ErrNotFound) {
			t.Fatalf("another partition's write = %v, want ErrNotFound", err)
		}
	})
}

// The order itself, stated pair by pair so a change to it is a change to this table.
var invoiceOrderCases = []struct {
	from, to string
	applies  bool
}{
	{"draft", "open", true}, {"open", "draft", false},
	{"open", "payment_failed", true}, {"payment_failed", "open", true}, {"payment_failed", "payment_failed", true},
	{"open", "paid", true}, {"payment_failed", "paid", true}, {"open", "void", true},
	{"open", "uncollectible", true}, {"uncollectible", "paid", true}, {"uncollectible", "void", true},
	{"uncollectible", "open", false}, {"uncollectible", "payment_failed", false},
	{"paid", "payment_failed", false}, {"paid", "open", false}, {"paid", "void", false}, {"paid", "paid", true},
	{"void", "paid", false}, {"void", "open", false}, {"void", "void", true},
	{"mystery", "paid", true}, {"mystery", "draft", false}, {"paid", "mystery", false}, {"open", "mystery", true},
}

var paymentOrderCases = []struct {
	from, to string
	applies  bool
}{
	{"requires_payment_method", "requires_confirmation", true}, {"requires_confirmation", "requires_action", true},
	{"requires_action", "processing", true}, {"processing", "requires_payment_method", true},
	{"processing", "requires_capture", true}, {"requires_capture", "succeeded", true}, {"processing", "succeeded", true},
	{"requires_payment_method", "canceled", true}, {"processing", "canceled", true},
	{"succeeded", "requires_payment_method", false}, {"succeeded", "processing", false}, {"succeeded", "canceled", false},
	{"succeeded", "succeeded", true}, {"canceled", "succeeded", false}, {"canceled", "requires_payment_method", false},
	{"canceled", "canceled", true}, {"mystery", "succeeded", true}, {"succeeded", "mystery", false},
}

// Both stores hold each pair as the table states, and so does the order the stores consult.
func TestStatusOrder_BothStoresHoldTheOrder(t *testing.T) {
	for _, c := range invoiceOrderCases {
		if got := invoiceStatusOrder.follows(c.from, c.to); got != c.applies {
			t.Errorf("invoice order %s -> %s = %v, want %v", c.from, c.to, got, c.applies)
		}
	}
	for _, c := range paymentOrderCases {
		if got := paymentStatusOrder.follows(c.from, c.to); got != c.applies {
			t.Errorf("payment order %s -> %s = %v, want %v", c.from, c.to, got, c.applies)
		}
	}
	eachStore(t, func(t *testing.T, s Store) {
		for i, c := range invoiceOrderCases {
			inv := &Invoice{Partition: "acme", CustomerID: "c1", Status: c.from, Currency: "usd", CreatedAt: at}
			if err := s.CreateInvoice(ctx, inv, nil); err != nil {
				t.Fatal(err)
			}
			err := s.UpdateInvoiceStatus(ctx, "acme", inv.ID, c.to, int64(i))
			got, _ := s.GetInvoice(ctx, "acme", inv.ID)
			if c.applies && (err != nil || got.Status != c.to) || !c.applies && (!errors.Is(err, ErrStatusOutOfOrder) || got.Status != c.from) {
				t.Errorf("invoice %s -> %s: err %v, status %q; applies %v", c.from, c.to, err, got.Status, c.applies)
			}
		}
		for _, c := range paymentOrderCases {
			key := c.from + ">" + c.to
			if _, err := s.RecordPayment(ctx, &PaymentLedgerRow{IdempotencyKey: key, Partition: "acme", CustomerID: "c1", Status: c.from}); err != nil {
				t.Fatal(err)
			}
			err := s.UpdatePaymentStatus(ctx, "acme", key, c.to, "")
			got, _ := s.GetPaymentByKey(ctx, "acme", key)
			if c.applies && (err != nil || got.Status != c.to) || !c.applies && (!errors.Is(err, ErrStatusOutOfOrder) || got.Status != c.from) {
				t.Errorf("payment %s -> %s: err %v, status %q; applies %v", c.from, c.to, err, got.Status, c.applies)
			}
		}
	})
}
