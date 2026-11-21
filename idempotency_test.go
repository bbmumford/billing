package billing

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// intentProvider answers each payment intent with a secret naming the call and records the
// idempotency key it was handed, so a test can trace a reply to the intent that minted it and see
// which key reached the provider.
type intentProvider struct {
	fakeProvider
	mu      sync.Mutex
	intents int
	keys    []string
}

func (p *intentProvider) CreatePaymentIntent(_ context.Context, in PaymentIntentInput) (PaymentIntentResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.intents++
	p.keys = append(p.keys, in.IdempotencyKey)
	return PaymentIntentResult{IntentRef: fmt.Sprintf("pi_%d", p.intents),
		ClientSecret: fmt.Sprintf("secret_%d_%s", p.intents, in.CustomerRef), Status: "requires_confirmation"}, nil
}

func eachChargeStore(t *testing.T, fn func(t *testing.T, svc *Service, store Store, prov *intentProvider)) {
	t.Helper()
	eachStore(t, func(t *testing.T, store Store) {
		prov := &intentProvider{}
		svc, err := NewService(Config{Store: store, Provider: prov, Encryptor: revEncryptor{}})
		if err != nil {
			t.Fatal(err)
		}
		fn(t, svc, store, prov)
	})
}

// A ledger key belongs to the partition that charged with it. A second partition's charge under
// the same key is its own charge: the provider is asked again, under a key that is not the first
// partition's, and the second partition is answered its own row and secret, never the first's.
func TestCharge_TheIdempotencyKeyIsThePartitions(t *testing.T) {
	eachChargeStore(t, func(t *testing.T, svc *Service, store Store, prov *intentProvider) {
		a := ChargeRequest{Partition: "partA", CustomerID: "custA", CustomerRef: "cus_A", AmountCents: 500, Currency: "usd", IdempotencyKey: "order-1"}
		b := ChargeRequest{Partition: "partB", CustomerID: "custB", CustomerRef: "cus_B", AmountCents: 99900, Currency: "eur", IdempotencyKey: "order-1"}
		rowA, secretA, err := svc.Charge(ctx, a)
		if err != nil {
			t.Fatalf("A's charge: %v", err)
		}
		rowB, secretB, err := svc.Charge(ctx, b)
		if err != nil {
			t.Fatalf("B's charge: %v", err)
		}
		if rowB.Partition != "partB" || rowB.CustomerID != "custB" || rowB.AmountCents != 99900 || rowB.Currency != "eur" {
			t.Fatalf("B was answered row %+v, want its own charge", rowB)
		}
		if secretB == secretA || secretB != "secret_2_cus_B" {
			t.Fatalf("B was answered secret %q (A's is %q), want the provider's second intent's", secretB, secretA)
		}
		if prov.intents != 2 {
			t.Fatalf("provider intents = %d, want 2 (one per partition)", prov.intents)
		}
		if prov.keys[0] == "" || prov.keys[0] == prov.keys[1] || prov.keys[0] == "order-1" {
			t.Fatalf("provider keys = %q, want two derived keys that differ by partition", prov.keys)
		}
		for _, c := range []struct {
			partition string
			want      *PaymentLedgerRow
		}{{"partA", rowA}, {"partB", rowB}} {
			got, err := store.GetPaymentByKey(ctx, c.partition, "order-1")
			if err != nil || got.Partition != c.partition || got.CustomerID != c.want.CustomerID || got.AmountCents != c.want.AmountCents {
				t.Fatalf("GetPaymentByKey(%s) = %+v, %v; want %+v", c.partition, got, err, c.want)
			}
		}
		if _, err := store.GetPaymentByKey(ctx, "", "order-1"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("GetPaymentByKey with no partition = %v, want ErrNotFound", err)
		}
		// A's retry is answered A's row and secret, B's charge unchanged by it.
		again, secret, err := svc.Charge(ctx, a)
		if err != nil || again.Partition != "partA" || secret != secretA || prov.intents != 2 {
			t.Fatalf("A's retry = %+v %q %v, intents %d; want A's row and secret, no new intent", again, secret, err, prov.intents)
		}
	})
}

// A charge with no idempotency key is refused before the provider is asked or the ledger read,
// so a retried unkeyed charge can neither make a second intent nor be answered another one's.
func TestCharge_AnEmptyKeyIsRefused(t *testing.T) {
	eachChargeStore(t, func(t *testing.T, svc *Service, store Store, prov *intentProvider) {
		first := ChargeRequest{Partition: "partA", CustomerID: "custA", CustomerRef: "cus_A", AmountCents: 500, Currency: "usd"}
		for i := 0; i < 2; i++ {
			row, secret, err := svc.Charge(ctx, first)
			if !errors.Is(err, ErrIdempotencyKeyRequired) || row != nil || secret != "" {
				t.Fatalf("unkeyed charge %d = %+v %q %v, want ErrIdempotencyKeyRequired", i, row, secret, err)
			}
		}
		if prov.intents != 0 {
			t.Fatalf("provider intents = %d, want 0", prov.intents)
		}
		if rows, _ := store.ListPaymentsByCustomer(ctx, "partA", "custA"); len(rows) != 0 {
			t.Fatalf("ledger rows = %+v, want none", rows)
		}
		if _, _, err := svc.Charge(ctx, ChargeRequest{CustomerID: "custA", AmountCents: 500, Currency: "usd", IdempotencyKey: "k"}); !errors.Is(err, ErrNoPartition) {
			t.Fatalf("charge with no partition = %v, want ErrNoPartition", err)
		}
		if prov.intents != 0 {
			t.Fatalf("a charge with no partition asked the provider: %d intents", prov.intents)
		}
	})
}

// A retry is answered the stored charge only when it asks for the same charge. The same key with
// another amount, currency or customer is refused with ErrIdempotencyConflict and answers no row
// and no secret; the provider is not asked again.
func TestCharge_AKeyReusedForADifferentChargeIsRefused(t *testing.T) {
	base := ChargeRequest{Partition: "partA", CustomerID: "custA", CustomerRef: "cus_A", AmountCents: 500, Currency: "usd", IdempotencyKey: "order-1"}
	for name, change := range map[string]func(*ChargeRequest){
		"amount":   func(r *ChargeRequest) { r.AmountCents = 501 },
		"currency": func(r *ChargeRequest) { r.Currency = "eur" },
		"customer": func(r *ChargeRequest) { r.CustomerID, r.CustomerRef = "custZ", "cus_Z" },
	} {
		t.Run(name, func(t *testing.T) {
			eachChargeStore(t, func(t *testing.T, svc *Service, store Store, prov *intentProvider) {
				if _, _, err := svc.Charge(ctx, base); err != nil {
					t.Fatal(err)
				}
				other := base
				change(&other)
				row, secret, err := svc.Charge(ctx, other)
				if !errors.Is(err, ErrIdempotencyConflict) || row != nil || secret != "" {
					t.Fatalf("Charge = %+v %q %v, want ErrIdempotencyConflict and nothing answered", row, secret, err)
				}
				if prov.intents != 1 {
					t.Fatalf("provider intents = %d, want 1", prov.intents)
				}
				same := base
				same.Currency = "USD"
				if _, secret, err := svc.Charge(ctx, same); err != nil || secret != "secret_1_cus_A" {
					t.Fatalf("retry differing only in the currency's case = %q %v, want the stored charge", secret, err)
				}
			})
		})
	}
}

// A charge whose ledger insert finds a concurrent writer's row under its key is answered that row
// only when the row records the same charge; the race's loser asking for a different charge is
// refused, as a later retry would be.
func TestCharge_ARacingWritersDifferentChargeIsRefused(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		prov := &intentProvider{}
		racing := &racingStore{Store: store, row: &PaymentLedgerRow{IdempotencyKey: "order-1", Partition: "partA",
			CustomerID: "custA", AmountCents: 700, Currency: "usd", ClientSecretSealed: "x"}}
		svc, err := NewService(Config{Store: racing, Provider: prov, Encryptor: revEncryptor{}})
		if err != nil {
			t.Fatal(err)
		}
		row, secret, err := svc.Charge(ctx, ChargeRequest{Partition: "partA", CustomerID: "custA", CustomerRef: "cus_A",
			AmountCents: 500, Currency: "usd", IdempotencyKey: "order-1"})
		if !errors.Is(err, ErrIdempotencyConflict) || row != nil || secret != "" {
			t.Fatalf("Charge = %+v %q %v, want ErrIdempotencyConflict", row, secret, err)
		}
	})
}

// racingStore records row through the wrapped store just before Charge's own RecordPayment, as a
// concurrent writer landing first would.
type racingStore struct {
	Store
	row *PaymentLedgerRow
}

func (s *racingStore) RecordPayment(ctx context.Context, row *PaymentLedgerRow) (*PaymentLedgerRow, error) {
	if s.row != nil {
		first := s.row
		s.row = nil
		if _, err := s.Store.RecordPayment(ctx, first); err != nil {
			return nil, err
		}
	}
	return s.Store.RecordPayment(ctx, row)
}

// The ledger is keyed by (partition, key) in both stores: one key holds a row per partition, a
// row is read and its status written only within its partition, and a row with no partition or
// no key is refused.
func TestPaymentLedger_KeyedByPartitionAndKey(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		for _, p := range []string{"partA", "partB"} {
			if existing, err := s.RecordPayment(ctx, &PaymentLedgerRow{IdempotencyKey: "k", Partition: p, CustomerID: "c_" + p, AmountCents: 100, Currency: "usd", Status: "requires_confirmation"}); err != nil || existing != nil {
				t.Fatalf("RecordPayment(%s) = %+v, %v; want a new row", p, existing, err)
			}
		}
		if existing, err := s.RecordPayment(ctx, &PaymentLedgerRow{IdempotencyKey: "k", Partition: "partA", CustomerID: "other"}); err != nil || existing == nil || existing.CustomerID != "c_partA" {
			t.Fatalf("RecordPayment repeat = %+v, %v; want partA's stored row", existing, err)
		}
		if err := s.UpdatePaymentStatus(ctx, "partB", "k", "processing", ""); err != nil {
			t.Fatal(err)
		}
		a, _ := s.GetPaymentByKey(ctx, "partA", "k")
		b, _ := s.GetPaymentByKey(ctx, "partB", "k")
		if a.Status != "requires_confirmation" || b.Status != "processing" || b.CustomerID != "c_partB" {
			t.Fatalf("rows after partB's status write: A %+v B %+v", a, b)
		}
		if err := s.UpdatePaymentStatus(ctx, "partC", "k", "processing", ""); !errors.Is(err, ErrNotFound) {
			t.Fatalf("status write in a partition holding no row = %v, want ErrNotFound", err)
		}
		if err := s.UpdatePaymentStatus(ctx, "", "k", "processing", ""); !errors.Is(err, ErrNotFound) {
			t.Fatalf("status write with no partition = %v, want ErrNotFound", err)
		}
		if _, err := s.RecordPayment(ctx, &PaymentLedgerRow{IdempotencyKey: "k2"}); !errors.Is(err, ErrNoPartition) {
			t.Fatalf("RecordPayment with no partition = %v, want ErrNoPartition", err)
		}
		if _, err := s.RecordPayment(ctx, &PaymentLedgerRow{Partition: "partA"}); !errors.Is(err, ErrIdempotencyKeyRequired) {
			t.Fatalf("RecordPayment with no key = %v, want ErrIdempotencyKeyRequired", err)
		}
	})
}

// The provider key is a function of the operation, the partition and the key alone, and differs
// when any one of them does, the length prefixes keeping a separator inside a value from making
// two triples one.
func TestProviderIdempotencyKey_DerivedFromOperationPartitionAndKey(t *testing.T) {
	k := providerIdempotencyKey("charge", "partA", "order-1")
	if k != providerIdempotencyKey("charge", "partA", "order-1") || len(k) > 255 || k == "order-1" {
		t.Fatalf("key %q is not deterministic, too long for the provider, or the caller's key", k)
	}
	for _, other := range []string{
		providerIdempotencyKey("subscribe", "partA", "order-1"),
		providerIdempotencyKey("charge", "partB", "order-1"),
		providerIdempotencyKey("charge", "partA", "order-2"),
		providerIdempotencyKey("charge", "partAo", "rder-1"),
	} {
		if other == k {
			t.Fatalf("two different triples derived the same key %q", k)
		}
	}
	if got := providerIdempotencyKey("charge", "partA", ""); got != "" {
		t.Fatalf("empty key derived %q, want \"\"", got)
	}
}

// A database whose billing_payments is keyed by idempotency_key alone is rebuilt by InitSchema into
// the (partition, idempotency_key) key with every row kept, so a second partition can record under a
// key the first used; running InitSchema again changes nothing.
func TestSQLStore_InitSchemaRekeysTheLedgerByPartition(t *testing.T) {
	db := openDB(t)
	if _, err := db.ExecContext(ctx, `CREATE TABLE billing_payments (
		idempotency_key TEXT PRIMARY KEY, partition TEXT NOT NULL DEFAULT '', customer_id TEXT NOT NULL DEFAULT '',
		payment_intent_ref TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT '',
		amount_cents INTEGER NOT NULL DEFAULT 0, currency TEXT NOT NULL DEFAULT '',
		charge_ref_sealed TEXT NOT NULL DEFAULT '', client_secret_sealed TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL DEFAULT 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO billing_payments (idempotency_key, partition, customer_id, amount_cents,
		currency, client_secret_sealed, status) VALUES ('order-1', 'partA', 'custA', 500, 'usd', 'enc:secret_a', 'requires_confirmation')`); err != nil {
		t.Fatal(err)
	}
	s := NewSQLStore(db)
	for i := 0; i < 2; i++ {
		if err := s.InitSchema(ctx); err != nil {
			t.Fatalf("InitSchema %d: %v", i, err)
		}
	}
	a, err := s.GetPaymentByKey(ctx, "partA", "order-1")
	if err != nil || a.CustomerID != "custA" || a.AmountCents != 500 || a.ClientSecretSealed != "enc:secret_a" {
		t.Fatalf("partA's row after the rebuild = %+v, %v; want it kept", a, err)
	}
	if existing, err := s.RecordPayment(ctx, &PaymentLedgerRow{IdempotencyKey: "order-1", Partition: "partB", CustomerID: "custB", AmountCents: 99900, Currency: "eur"}); err != nil || existing != nil {
		t.Fatalf("partB's row under partA's key = %+v, %v; want a new row", existing, err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM billing_payments`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("ledger rows = %d, %v; want 2", n, err)
	}
	var idx int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_billing_payments_customer'`).Scan(&idx); err != nil || idx != 1 {
		t.Fatalf("customer index after the rebuild: %d, %v; want it present", idx, err)
	}
}
