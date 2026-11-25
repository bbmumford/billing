package billing

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// customerCountingProvider is the fakeProvider with its EnsureCustomer calls
// counted and a distinct provider customer minted on each, so a Service test can
// tell one provider customer from one per call.
type customerCountingProvider struct {
	*fakeProvider
	customers int
}

func (p *customerCountingProvider) EnsureCustomer(context.Context, *Customer) (string, error) {
	p.customers++
	return fmt.Sprintf("cus_%d", p.customers), nil
}

// Service.EnsureCustomer is get-or-create keyed by the partition and the subject:
// a repeated call for the pair answers the stored row and makes no provider call,
// so a partition's callers that name no subject share one customer and one
// provider customer across every call. Another subject in the partition, and the
// same subject in another partition, are customers of their own.
func TestService_EnsureCustomerAnswersOneCustomerForThePartitionAndSubject(t *testing.T) {
	for _, subject := range []string{"", "u1"} {
		t.Run("subject "+subject, func(t *testing.T) {
			eachStore(t, func(t *testing.T, store Store) {
				prov := &customerCountingProvider{fakeProvider: &fakeProvider{}}
				svc := newService(t, store, prov)
				first, err := svc.EnsureCustomer(ctx, &Customer{Partition: "acme", UserID: subject, BillingEmail: "a@x", Currency: "usd"})
				if err != nil || first.ID == "" || first.ProviderRef == "" {
					t.Fatalf("first call: %+v %v", first, err)
				}
				if prov.customers != 1 {
					t.Fatalf("provider customers after the first call = %d, want 1", prov.customers)
				}
				for i := 2; i <= 3; i++ {
					again, err := svc.EnsureCustomer(ctx, &Customer{Partition: "acme", UserID: subject, BillingEmail: "other@x", Currency: "eur"})
					if err != nil {
						t.Fatalf("call %d: %v", i, err)
					}
					if again.ID != first.ID || again.ProviderRef != first.ProviderRef {
						t.Fatalf("call %d answered customer %s/%s, want the stored %s/%s", i, again.ID, again.ProviderRef, first.ID, first.ProviderRef)
					}
					if again.BillingEmail != "a@x" || again.Currency != "usd" {
						t.Fatalf("call %d answered %+v, want the stored row, not the request's values", i, again)
					}
					if prov.customers != 1 {
						t.Fatalf("provider customers after call %d = %d, want 1", i, prov.customers)
					}
				}
				stored, err := store.GetCustomerByUser(ctx, "acme", subject)
				if err != nil || stored.ID != first.ID || stored.ProviderRef != first.ProviderRef {
					t.Fatalf("stored customer for the pair = %+v %v, want %s/%s", stored, err, first.ID, first.ProviderRef)
				}

				other, err := svc.EnsureCustomer(ctx, &Customer{Partition: "acme", UserID: subject + "-other"})
				if err != nil || other.ID == first.ID || other.ProviderRef == first.ProviderRef {
					t.Fatalf("another subject in the partition answered %+v %v, want a customer of its own", other, err)
				}
				elsewhere, err := svc.EnsureCustomer(ctx, &Customer{Partition: "globex", UserID: subject})
				if err != nil || elsewhere.ID == first.ID || elsewhere.ProviderRef == first.ProviderRef {
					t.Fatalf("the subject in another partition answered %+v %v, want a customer of its own", elsewhere, err)
				}
				if prov.customers != 3 {
					t.Fatalf("provider customers = %d, want 3: one per (partition, subject)", prov.customers)
				}
				if back, err := svc.EnsureCustomer(ctx, &Customer{Partition: "acme", UserID: subject}); err != nil || back.ID != first.ID {
					t.Fatalf("after the others, the pair answered %+v %v, want %s", back, err, first.ID)
				}
			})
		})
	}
}

// An empty partition names no customer: no row is written, the provider is not
// asked, and the call is refused with ErrNoPartition. The store's selectors read
// no row under the empty partition, so a row written there could never be found
// or carry its provider reference.
func TestService_EnsureCustomerRefusesAnEmptyPartition(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		prov := &customerCountingProvider{fakeProvider: &fakeProvider{}}
		svc := newService(t, store, prov)
		c := &Customer{UserID: "u1", BillingEmail: "a@x"}
		if _, err := svc.EnsureCustomer(ctx, c); !errors.Is(err, ErrNoPartition) {
			t.Fatalf("EnsureCustomer with no partition = %v, want ErrNoPartition", err)
		}
		if prov.customers != 0 {
			t.Fatalf("the provider was asked %d times for a customer with no partition", prov.customers)
		}
		if c.ID != "" {
			t.Fatalf("a customer id %q was assigned to a refused customer", c.ID)
		}
	})
}

// racedStore is the reference MemStore whose first CreateCustomer is lost to a
// concurrent writer: the row for the pair lands through the store before the
// write answers ErrCustomerExists, as a second caller's insert under the
// uniqueness constraint would.
type racedStore struct {
	*MemStore
	raced bool
	rival *Customer
}

func (s *racedStore) CreateCustomer(c context.Context, cust *Customer) error {
	if !s.raced {
		s.raced = true
		s.rival = &Customer{Partition: cust.Partition, UserID: cust.UserID, BillingEmail: "rival@x"}
		if err := s.MemStore.CreateCustomer(c, s.rival); err != nil {
			return err
		}
		return ErrCustomerExists
	}
	return s.MemStore.CreateCustomer(c, cust)
}

// A create that loses the race to a concurrent caller answers that caller's row:
// the Service re-reads the pair and carries on with the stored customer, so the
// pair still ends with one row and one provider customer.
func TestService_EnsureCustomerAnswersTheRowAConcurrentCallerCreated(t *testing.T) {
	store := &racedStore{MemStore: NewMemStore()}
	prov := &customerCountingProvider{fakeProvider: &fakeProvider{}}
	svc := newService(t, store, prov)
	got, err := svc.EnsureCustomer(ctx, &Customer{Partition: "acme", UserID: "u1", BillingEmail: "a@x"})
	if err != nil {
		t.Fatalf("EnsureCustomer: %v", err)
	}
	if !store.raced || store.rival == nil || got.ID != store.rival.ID {
		t.Fatalf("answered customer %+v, want the concurrent caller's row %+v", got, store.rival)
	}
	if got.BillingEmail != "rival@x" {
		t.Fatalf("answered %+v, want the stored row's values", got)
	}
	if prov.customers != 1 || got.ProviderRef == "" {
		t.Fatalf("provider customers = %d, ref %q; want one provider customer on the stored row", prov.customers, got.ProviderRef)
	}
	stored, err := store.GetCustomerByUser(ctx, "acme", "u1")
	if err != nil || stored.ID != got.ID || stored.ProviderRef != got.ProviderRef {
		t.Fatalf("stored = %+v %v, want %s/%s", stored, err, got.ID, got.ProviderRef)
	}
}

// Store.CreateCustomer keeps one customer per (partition, subject): a second
// create for the pair answers ErrCustomerExists and writes nothing, whatever id
// it carries; the empty subject is a pair like any other. Another subject in the
// partition and the same subject in another partition are stored. Both stores
// agree, because a test against the memory store certifies the SQL one.
func TestCustomer_CreateRefusesASecondCustomerForThePartitionAndSubject(t *testing.T) {
	for _, subject := range []string{"", "u1"} {
		t.Run("subject "+subject, func(t *testing.T) {
			eachStore(t, func(t *testing.T, s Store) {
				first := &Customer{Partition: "acme", UserID: subject, BillingEmail: "a@x"}
				if err := s.CreateCustomer(ctx, first); err != nil {
					t.Fatalf("first create: %v", err)
				}
				second := &Customer{ID: "second-id", Partition: "acme", UserID: subject, BillingEmail: "b@x"}
				if err := s.CreateCustomer(ctx, second); !errors.Is(err, ErrCustomerExists) {
					t.Fatalf("second create for the pair = %v, want ErrCustomerExists", err)
				}
				if _, err := s.GetCustomer(ctx, "acme", "second-id"); !errors.Is(err, ErrNotFound) {
					t.Fatalf("the refused customer was stored: %v", err)
				}
				got, err := s.GetCustomerByUser(ctx, "acme", subject)
				if err != nil || got.ID != first.ID || got.BillingEmail != "a@x" {
					t.Fatalf("the pair answers %+v %v, want the first row %s", got, err, first.ID)
				}
				if err := s.CreateCustomer(ctx, &Customer{Partition: "acme", UserID: subject + "-other"}); err != nil {
					t.Fatalf("another subject in the partition: %v", err)
				}
				if err := s.CreateCustomer(ctx, &Customer{Partition: "globex", UserID: subject}); err != nil {
					t.Fatalf("the subject in another partition: %v", err)
				}
			})
		})
	}
}

// The fresh SQL schema carries the uniqueness itself, so a row written past
// CreateCustomer is held to the same pair: a second insert for (partition,
// subject) is refused by the table, and a different pair is taken.
func TestSQLStore_FreshSchemaKeepsOneCustomerPerPartitionAndSubject(t *testing.T) {
	db := openDB(t)
	s := NewSQLStore(db)
	if err := s.InitSchema(ctx); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}
	insert := `INSERT INTO billing_customers (id, partition, user_id) VALUES (?, ?, ?)`
	if _, err := db.ExecContext(ctx, insert, "c1", "acme", "u1"); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if _, err := db.ExecContext(ctx, insert, "c2", "acme", "u1"); err == nil {
		t.Fatal("the fresh schema took a second customer for (acme, u1)")
	}
	if _, err := db.ExecContext(ctx, insert, "c3", "acme", ""); err != nil {
		t.Fatalf("the empty subject in the partition: %v", err)
	}
	if _, err := db.ExecContext(ctx, insert, "c4", "acme", ""); err == nil {
		t.Fatal("the fresh schema took a second customer for (acme, \"\")")
	}
	if _, err := db.ExecContext(ctx, insert, "c5", "globex", "u1"); err != nil {
		t.Fatalf("the subject in another partition: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM billing_customers`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("rows = %d %v, want 3", n, err)
	}
}
