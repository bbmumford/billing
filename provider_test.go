package billing

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// revEncryptor is a reversible test Encryptor — proves the seam round-trips without a real cipher.
type revEncryptor struct{}

func (revEncryptor) Encrypt(p string) (string, error) { return "enc:" + p, nil }
func (revEncryptor) Decrypt(c string) (string, error) { return strings.TrimPrefix(c, "enc:"), nil }

// fakeProvider is a test PaymentProvider that counts intent creations so idempotency can be asserted,
// and records every cancel it is asked for, answering the CancelResult or error a test sets.
type fakeProvider struct {
	intents      int
	subs         int
	cancels      []cancelCall
	cancelAnswer CancelResult
	cancelErr    error
}

// cancelCall is one CancelSubscription the fakeProvider is asked for.
type cancelCall struct {
	providerRef string
	atPeriodEnd bool
}

func (f *fakeProvider) EnsureCustomer(_ context.Context, c *Customer) (string, error) {
	return "cus_" + c.UserID, nil
}
func (f *fakeProvider) CreatePaymentIntent(_ context.Context, in PaymentIntentInput) (PaymentIntentResult, error) {
	f.intents++
	return PaymentIntentResult{IntentRef: "pi_1", ClientSecret: "secret-xyz", ChargeRef: "ch_1", Status: "requires_confirmation"}, nil
}
func (f *fakeProvider) CreateSubscription(_ context.Context, in SubscriptionInput) (SubscriptionResult, error) {
	f.subs++
	return SubscriptionResult{ProviderRef: "sub_1", Status: "active"}, nil
}
func (f *fakeProvider) CancelSubscription(_ context.Context, providerRef string, atPeriodEnd bool) (CancelResult, error) {
	f.cancels = append(f.cancels, cancelCall{providerRef: providerRef, atPeriodEnd: atPeriodEnd})
	if f.cancelErr != nil {
		return CancelResult{}, f.cancelErr
	}
	return f.cancelAnswer, nil
}

func newService(t *testing.T, store Store, prov PaymentProvider) *Service {
	t.Helper()
	svc, err := NewService(Config{Store: store, Provider: prov, Encryptor: revEncryptor{}, Now: func() time.Time { return time.Unix(9000, 0).UTC() }})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestService_RequiresSeams(t *testing.T) {
	if _, err := NewService(Config{Provider: &fakeProvider{}, Encryptor: revEncryptor{}}); err == nil {
		t.Fatal("nil Store must be rejected")
	}
	if _, err := NewService(Config{Store: NewMemStore(), Encryptor: revEncryptor{}}); err == nil {
		t.Fatal("nil Provider must be rejected")
	}
	if _, err := NewService(Config{Store: NewMemStore(), Provider: &fakeProvider{}}); err == nil {
		t.Fatal("nil Encryptor must be rejected — ledger secrets must be sealed at rest")
	}
}

func TestService_EnsureCustomerSetsProviderRef(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		s := newService(t, store, &fakeProvider{})
		c := &Customer{Partition: "acme", UserID: "u1", BillingEmail: "a@x"}
		got, err := s.EnsureCustomer(ctx, c)
		if err != nil || got.ProviderRef != "cus_u1" {
			t.Fatalf("ensure: %+v %v", got, err)
		}
		stored, _ := store.GetCustomer(ctx, "acme", got.ID)
		if stored.ProviderRef != "cus_u1" {
			t.Fatalf("provider ref not persisted: %+v", stored)
		}
	})
}

func TestService_ChargeSealsSecretIdempotentlyAndReveals(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		prov := &fakeProvider{}
		s := newService(t, store, prov)
		req := ChargeRequest{Partition: "acme", CustomerID: "c1", CustomerRef: "cus_1", AmountCents: 500, Currency: "usd", IdempotencyKey: "idem-1"}
		row, secret, err := s.Charge(ctx, req)
		if err != nil || secret != "secret-xyz" {
			t.Fatalf("charge: secret=%q err=%v", secret, err)
		}
		// the store holds only the SEALED secret, never the plaintext
		stored, _ := store.GetPaymentByKey(ctx, "idem-1")
		if stored.ClientSecretSealed == "secret-xyz" || stored.ClientSecretSealed != "enc:secret-xyz" {
			t.Fatalf("client secret must be sealed at rest; got %q", stored.ClientSecretSealed)
		}
		if stored.ChargeRefSealed != "enc:ch_1" {
			t.Fatalf("charge ref must be sealed; got %q", stored.ChargeRefSealed)
		}
		// a retry with the same key does NOT create a second provider intent and returns the revealed secret
		row2, secret2, err := s.Charge(ctx, req)
		if err != nil || secret2 != "secret-xyz" || row2.IdempotencyKey != row.IdempotencyKey {
			t.Fatalf("idempotent retry: secret=%q err=%v", secret2, err)
		}
		if prov.intents != 1 {
			t.Fatalf("provider must be called exactly once across retries; got %d", prov.intents)
		}
		if cr, _ := s.RevealChargeRef(stored); cr != "ch_1" {
			t.Fatalf("RevealChargeRef: got %q want ch_1", cr)
		}
		if _, _, err := s.Charge(ctx, ChargeRequest{IdempotencyKey: "idem-2", AmountCents: 0}); !errors.Is(err, ErrAmountInvalid) {
			t.Fatal("a non-positive amount must be ErrAmountInvalid")
		}
	})
}

func TestService_SubscribePersistsFromProvider(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		s := newService(t, store, &fakeProvider{})
		sub, err := s.Subscribe(ctx, "acme", "c1", "cus_1", "price1", "price_ref_1", 2, "")
		if err != nil || sub.ProviderRef != "sub_1" || sub.Status != "active" || sub.Quantity != 2 {
			t.Fatalf("subscribe: %+v %v", sub, err)
		}
		if got, _ := store.GetSubscription(ctx, "acme", sub.ID); got == nil || got.Status != "active" {
			t.Fatal("subscription must persist from the provider result")
		}
	})
}
