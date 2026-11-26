// Package billing is the scope-agnostic billing system (extracted from the HSTLES billing domain). It
// owns the neutral money-state — customers, a product/price catalog, subscriptions, invoices, an
// idempotency-keyed payment ledger, metered usage, and credit grants — over an injected store. The
// primary axis (a baked per-tenant billing DB) becomes an opaque Partition the system never interprets; the
// customer/user/org ids are opaque; and every payment-PROVIDER identifier (a Stripe customer/price/
// intent id) is a single opaque ProviderRef, because the provider itself is a pluggable seam a consumer
// injects — the package holds neutral state and pulls no payment-SDK dependency. Money is always int64
// minor units (cents) plus a currency, never a float. Sensitive ledger fields (a charge id, a client
// secret) are sealed at rest behind an injected Encryptor. Times are time.Time; a nil *time.Time means
// "not yet".
package billing

import "time"

// Customer is a billing relationship. Partition is the scope it belongs to and
// this package never interprets it: a deployment may partition by team, family,
// project or anything else, and the one field carries all of them. UserID is the
// principal GetCustomerByUser looks a customer up by, within a partition.
// ProviderRef is the payment provider's own id
// for this customer, opaque here because the provider mints it. Currency is the
// ISO-4217 code billed in.
type Customer struct {
	ID           string
	Partition    string
	UserID       string
	ProviderRef  string
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
	Partition   string
	ServiceID   string
	Name        string
	Description string
	Active      bool
	// Metadata is opaque consumer key/values carried with the product. The store
	// never reads it; a consumer stamps identifiers a payment provider echoes back
	// on its own records, so a webhook naming only the provider's id can still be
	// matched to this row. Dropping it breaks that match silently — nothing here
	// fails, the reconciliation upstream stops finding anything.
	Metadata  map[string]string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Price is a purchasable price for a product. AmountCents is the unit price in minor units; Interval is
// the billing cadence ("month"/"year"/"" for one-off); PricingModel is opaque ("flat"/"per_unit"/
// "tiered"/"percentage"); TierConfig holds opaque tier JSON for a tiered model; PercentageRate applies
// to a percentage model.
type Price struct {
	ID             string
	Partition      string
	ProductID      string
	ProviderRef    string // opaque provider price id
	Currency       string
	AmountCents    int64
	Interval       string
	PricingModel   string
	TierConfig     string // opaque JSON (volume/graduated tiers)
	PercentageRate float64
	// Metadata is opaque consumer key/values, carried for the same reason as
	// Product.Metadata.
	Metadata  map[string]string
	CreatedAt time.Time
}

// Subscription is a customer's subscription to a price. Status is opaque to the store but the service
// validates it against the allowed set (see subscription.go). Cancel-at-period-end defers cancellation.
type Subscription struct {
	ID                 string
	Partition          string
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
	// StatusChangedAt is the instant Status took its present value: the creation
	// write, then each UpdateSubscription whose Status differs from the stored one.
	// UpdatedAt cannot stand in for it, because UpdateSubscription and
	// UpdateSubscriptionItem rewrite UpdatedAt on every write, a period or price
	// change included.
	StatusChangedAt time.Time
	// EnteredUnpaidAt is the instant the subscription entered the unpaid statuses
	// (see unpaid.go) from any other status. It holds while the status moves
	// between them and is nil outside them, so it is the start of the present
	// unpaid run; UnpaidSince reads it.
	//
	// The store writes both instants from its own clock and the stored row, and
	// ignores the values a caller hands to CreateSubscription or UpdateSubscription:
	// the billing host's webhook applier writes back a whole row it read, so a value
	// taken from the caller would be whatever that read held, and ListUnpaidSince
	// selects by this clock.
	EnteredUnpaidAt *time.Time
	// EventCreatedAt is the created instant of the provider event last applied to
	// the row; nil on a row no event has touched. The billing host's webhook
	// applier sets it from the event it applies, and both stores hold the rule on
	// the write: UpdateSubscription stores the caller's instant, keeps the stored
	// one when the caller hands nil (an item or cancel write reads the row back
	// whole and is no event), and refuses with ErrStaleEvent an instant earlier
	// than the stored one, writing nothing. The provider does not order its
	// deliveries, so a row carrying only the status would take a redelivered older
	// event's status over a newer one's. An equal instant is applied: the
	// provider's instant has one-second grain and two distinct events may share it.
	// CreateSubscription stores the value as handed.
	EventCreatedAt *time.Time
}

// Invoice is an issued invoice. AmountDueCents/AmountPaidCents are minor units; Status is opaque
// ("draft"/"open"/"paid"/"void"/"uncollectible"). Line items live in InvoiceItem.
type Invoice struct {
	ID              string
	Partition       string
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
// and its lifecycle status. IdempotencyKey is the key a retry reuses so a payment is recorded exactly
// once; it is unique within the row's Partition, and another partition's row may carry the same key.
// ChargeRefSealed and ClientSecretSealed are the provider charge id and client secret
// encrypted at rest (an injected Encryptor); they never reach the store in the clear.
type PaymentLedgerRow struct {
	IdempotencyKey     string
	Partition          string
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
// period. Metadata is opaque. IdempotencyKey is the reporter's key for the event: Store.RecordUsage
// derives the record's ID from the partition and it, so a retried report is stored once. A record
// carries a key or an ID of its own; one with neither is refused (ErrIdempotencyKeyRequired).
type UsageRecord struct {
	ID             string
	IdempotencyKey string
	Partition      string
	CustomerID     string
	ProductID      string
	Quantity       int64
	Metadata       map[string]string
	RecordedAt     time.Time
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
	Partition   string
	CustomerID  string
	AmountCents int64
	Currency    string
	Reason      string
	CreatedAt   time.Time
	ExpiresAt   *time.Time
}
