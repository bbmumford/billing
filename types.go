// Package billing is the scope-agnostic billing system (extracted from the HSTLES billing domain). It
// owns the neutral money-state — customers, a product/price catalog, subscriptions, invoices, an
// idempotency-keyed payment ledger, metered usage, and credit grants — over an injected store. The
// primary axis (a baked per-tenant billing DB) becomes an opaque Owner the system never interprets; the
// customer/user/org ids are opaque; and every payment-PROVIDER identifier (a Stripe customer/price/
// intent id) is a single opaque ProviderRef, because the provider itself is a pluggable seam a consumer
// injects — the package holds neutral state and pulls no payment-SDK dependency. Money is always int64
// minor units (cents) plus a currency, never a float. Sensitive ledger fields (a charge id, a client
// secret) are sealed at rest behind an injected Encryptor. Times are time.Time; a nil *time.Time means
// "not yet".
package billing

import "time"

// Customer is a billing customer. Owner is the opaque owner-scope; UserID/OrgID are opaque grouping
// ids; ProviderRef is the opaque payment-provider customer id (e.g. a Stripe customer). Currency is the
// ISO-4217 code the customer is billed in.
type Customer struct {
	ID           string
	Owner        string // opaque owner-scope (was the per-tenant billing DB)
	UserID       string
	OrgID        string
	ProviderRef  string // opaque provider customer id
	BillingEmail string
	BillingName  string
	Currency     string
	TaxID        string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Product is a sellable product in the catalog. ServiceID is an opaque grouping a consumer may use to
// tie products to a service/feature.
type Product struct {
	ID          string
	Owner       string
	ServiceID   string
	Name        string
	Description string
	Active      bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Price is a purchasable price for a product. AmountCents is the unit price in minor units; Interval is
// the billing cadence ("month"/"year"/"" for one-off); PricingModel is opaque ("flat"/"per_unit"/
// "tiered"/"percentage"); TierConfig holds opaque tier JSON for a tiered model; PercentageRate applies
// to a percentage model.
type Price struct {
	ID             string
	Owner          string
	ProductID      string
	ProviderRef    string // opaque provider price id
	Currency       string
	AmountCents    int64
	Interval       string
	PricingModel   string
	TierConfig     string // opaque JSON (volume/graduated tiers)
	PercentageRate float64
	CreatedAt      time.Time
}

// Subscription is a customer's subscription to a price. Status is opaque to the store but the service
// validates it against the allowed set (see subscription.go). Cancel-at-period-end defers cancellation.
type Subscription struct {
	ID                 string
	Owner              string
	CustomerID         string
	PriceID            string
	ProviderRef        string // opaque provider subscription id
	Status             string
	Quantity           int64
	CurrentPeriodStart *time.Time
	CurrentPeriodEnd   *time.Time
	CancelAtPeriodEnd  bool
	CreatedAt          time.Time
	UpdatedAt          time.Time
	CanceledAt         *time.Time
}

// Invoice is an issued invoice. AmountDueCents/AmountPaidCents are minor units; Status is opaque
// ("draft"/"open"/"paid"/"void"/"uncollectible"). Line items live in InvoiceItem.
type Invoice struct {
	ID              string
	Owner           string
	CustomerID      string
	SubscriptionID  string
	ProviderRef     string // opaque provider invoice id
	Number          string
	Status          string
	Currency        string
	AmountDueCents  int64
	AmountPaidCents int64
	CreatedAt       time.Time
	DueAt           *time.Time
	PaidAt          *time.Time
}

// InvoiceItem is one line on an invoice. AmountCents is Quantity × UnitAmountCents (the writer computes
// it); Metadata is opaque.
type InvoiceItem struct {
	ID              string
	InvoiceID       string
	ProductID       string
	Description     string
	Quantity        int64
	UnitAmountCents int64
	AmountCents     int64
	Metadata        map[string]string
}

// PaymentLedgerRow is one append-only, idempotency-keyed row of the payment ledger — a money movement
// and its lifecycle status. IdempotencyKey is the unique key a retry reuses so a payment is recorded
// exactly once. ChargeRefSealed and ClientSecretSealed are the provider charge id and client secret
// encrypted at rest (an injected Encryptor); they never reach the store in the clear.
type PaymentLedgerRow struct {
	IdempotencyKey     string
	Owner              string
	CustomerID         string
	PaymentIntentRef   string // opaque provider payment-intent id
	Status             string
	AmountCents        int64
	Currency           string
	ChargeRefSealed    string
	ClientSecretSealed string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// UsageRecord is a metered-usage event for a customer + product; usage-based billing sums these over a
// period. Metadata is opaque.
type UsageRecord struct {
	ID         string
	Owner      string
	CustomerID string
	ProductID  string
	Quantity   int64
	Metadata   map[string]string
	RecordedAt time.Time
}

// UsageSummary aggregates a customer+product's usage over a queried window.
type UsageSummary struct {
	ProductID     string
	TotalQuantity int64
	PeakQuantity  int64
}

// Credit is a credit grant against a customer's balance (a positive AmountCents adds credit). Reason is
// opaque; an expired credit (ExpiresAt in the past) no longer counts toward the balance.
type Credit struct {
	ID          string
	Owner       string
	CustomerID  string
	AmountCents int64
	Currency    string
	Reason      string
	CreatedAt   time.Time
	ExpiresAt   *time.Time
}
