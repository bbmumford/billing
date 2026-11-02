package billing

import (
	"context"
	"errors"
	"time"
)

// Encryptor seals a sensitive ledger value (a provider charge id, a client secret) at rest and reveals
// it just-in-time. A deployment holding real payment secrets wires AES-GCM or an envelope KEK; the
// billing Service requires a non-nil Encryptor — these fields must never be stored in the clear. This is
// the same seam proven in the pharos subscriber and census vault layers.
type Encryptor interface {
	Encrypt(plaintext string) (string, error)
	Decrypt(ciphertext string) (string, error)
}

// PaymentProvider is the pluggable payment-gateway seam (Stripe, a mock, another PSP). The consumer
// implements it; the billing package holds neutral money-state and pulls no payment-SDK dependency. All
// provider identifiers it returns are opaque to billing (stored as ProviderRef / sealed ledger fields).
type PaymentProvider interface {
	// EnsureCustomer creates or returns the provider's customer id for a local customer (idempotent on
	// the provider side by the customer's identity).
	EnsureCustomer(ctx context.Context, c *Customer) (providerRef string, err error)
	// CreatePaymentIntent creates a payment intent for an amount; IdempotencyKey lets the provider dedupe
	// a retry. It returns the opaque intent ref, a client secret (handed to the client SDK once), and a
	// charge ref if already captured.
	CreatePaymentIntent(ctx context.Context, in PaymentIntentInput) (PaymentIntentResult, error)
	// CreateSubscription subscribes a provider customer to a provider price.
	CreateSubscription(ctx context.Context, in SubscriptionInput) (SubscriptionResult, error)
	// CancelSubscription cancels a provider subscription at once, or at period end, and answers the
	// cancel fields the provider holds afterwards (CancelResult); Service.CancelSubscription stores
	// exactly those, so the row carries the provider's answer and no value the caller inferred.
	CancelSubscription(ctx context.Context, providerRef string, atPeriodEnd bool) (CancelResult, error)
}

// PaymentIntentInput/Result carry the neutral fields of a payment-intent creation; all provider ids are
// opaque strings billing does not interpret.
type PaymentIntentInput struct {
	CustomerRef    string
	AmountCents    int64
	Currency       string
	IdempotencyKey string
	Metadata       map[string]string
}

type PaymentIntentResult struct {
	IntentRef    string
	ClientSecret string
	ChargeRef    string // may be empty until captured
	Status       string
}

// SubscriptionInput/Result carry the neutral fields of a provider subscription.
type SubscriptionInput struct {
	CustomerRef string
	PriceRef    string
	Quantity    int64
	// BillingProfileID is the billing profile the subscription pays for, as the
	// caller of Service.Subscribe names it; "" for a subscription paying for no
	// profile. The provider carries it on its subscription record so the
	// consumer that reads the provider's subscription events can link the
	// profile from the event alone, which no other field of the event answers.
	BillingProfileID string
}

type SubscriptionResult struct {
	ProviderRef        string
	Status             string
	CurrentPeriodStart *time.Time
	CurrentPeriodEnd   *time.Time
	// Items is every item the provider's subscription holds after the call,
	// each with its provider price reference and quantity, in the provider's
	// order. SubscriptionUpdater fills it, and Service.UpdateSubscription stores
	// it whole, so the local rows carry what the provider confirmed and not
	// what the caller asked for.
	Items []SubscriptionItem
}

// configError reports an invalid Service configuration.
type configError struct{ msg string }

func (e *configError) Error() string { return e.msg }

// Service orchestrates the neutral store, the pluggable PaymentProvider, and the Encryptor: it ensures
// provider customers, creates charges (sealing the client secret + charge ref at rest and recording the
// ledger idempotently), reveals those secrets just-in-time, and creates/cancels subscriptions. It
// interprets no axis — Partition and all ids are opaque.
type Service struct {
	store    Store
	provider PaymentProvider
	enc      Encryptor
	now      func() time.Time
}

// Config wires a Service. Store, Provider, and Encryptor are all required — ledger charge/secret must be
// encrypted at rest, so a nil Encryptor is rejected.
type Config struct {
	Store     Store
	Provider  PaymentProvider
	Encryptor Encryptor
	Now       func() time.Time
}

// NewService validates the config and returns the Service.
func NewService(c Config) (*Service, error) {
	if c.Store == nil {
		return nil, &configError{"billing: Config.Store is required"}
	}
	if c.Provider == nil {
		return nil, &configError{"billing: Config.Provider is required"}
	}
	if c.Encryptor == nil {
		return nil, &configError{"billing: Config.Encryptor is required (ledger charge id / client secret must be sealed at rest)"}
	}
	s := &Service{store: c.Store, provider: c.Provider, enc: c.Encryptor, now: c.Now}
	if s.now == nil {
		s.now = time.Now
	}
	return s, nil
}

// EnsureCustomer is the get-or-create of the customer keyed by (Partition, UserID):
// it answers the partition's stored customer for the subject when one exists, else
// stores c as that customer, and in either case makes sure the row carries a
// provider customer reference, asking the provider only while the row has none, so
// the pair ends with one row and one provider customer across any number of calls.
// It consumes a Customer with a Partition (ErrNoPartition otherwise) and a subject,
// empty for a partition's subjectless customer, reads the pair through
// Store.GetCustomerByUser, and takes a Customer carrying an ID as the stored row.
// The billing host's EnsureCustomer handler is its caller, with the sealed
// partition and the request's subject; every caller of that operation dials it
// before each read of the partition's subscription, so a call that stored a new
// customer each time would answer a customer holding no subscription and create a
// provider customer to match. A create refused with ErrCustomerExists is a
// concurrent caller's row landing first: the pair is re-read and that row answered.
// TODO: two concurrent first calls that both find the row without a provider
// reference each ask the provider and the reference written last wins; the rail
// sets no idempotency key on the provider's customer create.
func (s *Service) EnsureCustomer(ctx context.Context, c *Customer) (*Customer, error) {
	if c.Partition == "" {
		return nil, ErrNoPartition
	}
	if c.ID == "" {
		stored, err := s.store.GetCustomerByUser(ctx, c.Partition, c.UserID)
		switch {
		case err == nil:
			c = stored
		case errors.Is(err, ErrNotFound):
			if cerr := s.store.CreateCustomer(ctx, c); errors.Is(cerr, ErrCustomerExists) {
				if c, err = s.store.GetCustomerByUser(ctx, c.Partition, c.UserID); err != nil {
					return nil, err
				}
			} else if cerr != nil {
				return nil, cerr
			}
		default:
			return nil, err
		}
	}
	if c.ProviderRef == "" {
		ref, err := s.provider.EnsureCustomer(ctx, c)
		if err != nil {
			return nil, err
		}
		if err := s.store.SetCustomerProviderRef(ctx, c.Partition, c.ID, ref); err != nil {
			return nil, err
		}
		c.ProviderRef = ref
	}
	return c, nil
}

// ChargeRequest describes a charge to create against a customer.
type ChargeRequest struct {
	Partition          string
	CustomerID     string
	CustomerRef    string // opaque provider customer id
	AmountCents    int64
	Currency       string
	IdempotencyKey string
	Metadata       map[string]string
}

// ErrAmountInvalid is returned when a charge amount is not positive.
var ErrAmountInvalid = errors.New("billing: charge amount must be positive")

// Charge creates a provider payment intent and records it on the ledger exactly once (keyed by
// IdempotencyKey), sealing the client secret and any charge ref at rest. It returns the ledger row and
// the PLAINTEXT client secret for the client SDK (returned once; the store holds only the sealed form).
// A retry with the same IdempotencyKey returns the existing row and its revealed client secret without
// creating a second provider intent.
func (s *Service) Charge(ctx context.Context, req ChargeRequest) (row *PaymentLedgerRow, clientSecret string, err error) {
	if req.AmountCents <= 0 {
		return nil, "", ErrAmountInvalid
	}
	// Idempotency: an existing ledger row short-circuits the provider call.
	if existing, gerr := s.store.GetPaymentByKey(ctx, req.IdempotencyKey); gerr == nil {
		secret, derr := s.reveal(existing.ClientSecretSealed)
		if derr != nil {
			return nil, "", derr
		}
		return existing, secret, nil
	} else if !errors.Is(gerr, ErrNotFound) {
		return nil, "", gerr
	}

	res, err := s.provider.CreatePaymentIntent(ctx, PaymentIntentInput{
		CustomerRef:    req.CustomerRef,
		AmountCents:    req.AmountCents,
		Currency:       req.Currency,
		IdempotencyKey: req.IdempotencyKey,
		Metadata:       req.Metadata,
	})
	if err != nil {
		return nil, "", err
	}
	sealedSecret, err := s.seal(res.ClientSecret)
	if err != nil {
		return nil, "", err
	}
	sealedCharge, err := s.seal(res.ChargeRef)
	if err != nil {
		return nil, "", err
	}
	row = &PaymentLedgerRow{
		IdempotencyKey:     req.IdempotencyKey,
		Partition:              req.Partition,
		CustomerID:         req.CustomerID,
		PaymentIntentRef:   res.IntentRef,
		Status:             res.Status,
		AmountCents:        req.AmountCents,
		Currency:           req.Currency,
		ChargeRefSealed:    sealedCharge,
		ClientSecretSealed: sealedSecret,
		CreatedAt:          s.now(),
	}
	if existing, rerr := s.store.RecordPayment(ctx, row); rerr != nil {
		return nil, "", rerr
	} else if existing != nil {
		// A concurrent writer won the race; return their row + revealed secret.
		secret, derr := s.reveal(existing.ClientSecretSealed)
		if derr != nil {
			return nil, "", derr
		}
		return existing, secret, nil
	}
	return row, res.ClientSecret, nil
}

// RevealChargeRef decrypts a ledger row's sealed charge ref just-in-time (e.g. to reconcile a refund).
func (s *Service) RevealChargeRef(row *PaymentLedgerRow) (string, error) {
	return s.reveal(row.ChargeRefSealed)
}

// Subscribe creates a provider subscription and persists the local Subscription
// record from the result. billingProfileID is handed to the provider as
// SubscriptionInput.BillingProfileID and stored nowhere here: the local row
// carries no profile, the provider's subscription record does.
func (s *Service) Subscribe(ctx context.Context, partition, customerID, customerRef, priceID, priceRef string, quantity int64, billingProfileID string) (*Subscription, error) {
	res, err := s.provider.CreateSubscription(ctx, SubscriptionInput{CustomerRef: customerRef, PriceRef: priceRef, Quantity: quantity, BillingProfileID: billingProfileID})
	if err != nil {
		return nil, err
	}
	if res.Status != "" && !ValidSubscriptionStatus(res.Status) {
		return nil, ErrInvalidSubscriptionStatus
	}
	sub := &Subscription{
		Partition:              partition,
		CustomerID:         customerID,
		PriceID:            priceID,
		ProviderRef:        res.ProviderRef,
		Status:             res.Status,
		Quantity:           quantity,
		CurrentPeriodStart: res.CurrentPeriodStart,
		CurrentPeriodEnd:   res.CurrentPeriodEnd,
	}
	if err := s.store.CreateSubscription(ctx, sub); err != nil {
		return nil, err
	}
	return sub, nil
}

// seal encrypts a value, returning "" for an empty input (nothing to seal).
func (s *Service) seal(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	return s.enc.Encrypt(plaintext)
}

// reveal decrypts a sealed value, returning "" for an empty input.
func (s *Service) reveal(ciphertext string) (string, error) {
	if ciphertext == "" {
		return "", nil
	}
	return s.enc.Decrypt(ciphertext)
}
