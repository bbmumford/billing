package billing

import (
	"context"
	"errors"
	"time"
)

// PaymentMethod is a customer's stored payment instrument (card, bank account,
// crypto wallet). ProviderRef is the opaque provider (Stripe) payment-method id;
// billing never interprets it. Partition is the opaque scope the store filters on
// but never interprets. A soft-removed method has Active=false and is excluded
// from ListPaymentMethods. At most one Active method per customer carries
// IsDefault; the store enforces that invariant on add/set-default.
type PaymentMethod struct {
	ID             string
	Partition      string
	CustomerID     string
	Type           string // card, bank_account, crypto_wallet
	IsDefault      bool
	ProviderRef    string // opaque provider payment-method id (e.g. a Stripe pm_…)
	CardBrand      string
	CardLast4      string
	CardExpMonth   int32
	CardExpYear    int32
	CryptoCurrency string
	Active         bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// PaymentMethodProvider is an OPTIONAL PaymentProvider extension for payment-method
// management and setup intents. A PaymentProvider may also implement it; Service
// type-asserts on it and returns ErrProviderUnsupported when the wired provider
// does not — a deployment without card management fails closed rather than
// silently no-op. The core PaymentProvider stays unchanged so existing providers
// keep compiling.
type PaymentMethodProvider interface {
	// AttachPaymentMethod attaches a provider payment-method to a provider customer.
	AttachPaymentMethod(ctx context.Context, customerRef, paymentMethodRef string) error
	// CreateSetupIntent starts a setup intent for a provider customer so the client
	// SDK can collect and confirm a payment method off a charge. It returns the
	// opaque intent ref and the client secret (handed to the client SDK once).
	CreateSetupIntent(ctx context.Context, customerRef string) (SetupIntentResult, error)
}

// SetupIntentResult carries the neutral fields of a setup-intent creation; the
// provider ids are opaque strings billing does not interpret.
type SetupIntentResult struct {
	IntentRef    string
	ClientSecret string
}

// ErrProviderUnsupported is returned when an operation needs a provider capability
// the wired PaymentProvider does not implement (it is not a PaymentMethodProvider).
var ErrProviderUnsupported = errors.New("billing: provider does not support this operation")

// AddPaymentMethod persists a customer's payment method and, when the provider
// supports it and the method carries a provider ref, best-effort attaches it to
// the provider customer. The store write is authoritative: a provider attach
// failure (or a customer without a provider ref) does NOT fail the call — the
// method is saved and the attach is reconciled out of band. This mirrors the
// prior billing behaviour where the attach was explicitly non-fatal.
func (s *Service) AddPaymentMethod(ctx context.Context, pm *PaymentMethod) error {
	if err := s.store.AddPaymentMethod(ctx, pm); err != nil {
		return err
	}
	if pm.ProviderRef == "" {
		return nil
	}
	pmp, ok := s.provider.(PaymentMethodProvider)
	if !ok {
		return nil
	}
	cust, err := s.store.GetCustomer(ctx, pm.Partition, pm.CustomerID)
	if err != nil || cust.ProviderRef == "" {
		return nil // no provider customer to attach to; the store write stands
	}
	_ = pmp.AttachPaymentMethod(ctx, cust.ProviderRef, pm.ProviderRef) // best-effort
	return nil
}

// CreateSetupIntent resolves the customer's provider ref and asks the provider to
// start a setup intent. It fails closed (ErrProviderUnsupported) when no provider
// capability is wired, so a deployment without a money provider cannot silently
// hand back an empty client secret.
func (s *Service) CreateSetupIntent(ctx context.Context, partition, customerID string) (SetupIntentResult, error) {
	pmp, ok := s.provider.(PaymentMethodProvider)
	if !ok {
		return SetupIntentResult{}, ErrProviderUnsupported
	}
	cust, err := s.store.GetCustomer(ctx, partition, customerID)
	if err != nil {
		return SetupIntentResult{}, err
	}
	return pmp.CreateSetupIntent(ctx, cust.ProviderRef)
}
