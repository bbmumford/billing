package billing

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned by single-record reads when nothing matches.
var ErrNotFound = errors.New("billing: not found")

// ErrStaleEvent is UpdateSubscription's refusal of a write whose
// Subscription.EventCreatedAt is earlier than the instant the stored row carries;
// nothing is written. billinghost's webhook applier acknowledges the event on it,
// as it does for an instant it compared itself before writing.
var ErrStaleEvent = errors.New("billing: event created earlier than the one applied")

// Store is the persistence contract for the billing money-state. The Partition is opaque and the store
// filters on it, never interprets it; an empty partition on a partition-scoped read matches NOTHING —
// every predicate rejects an empty parameter before comparing, so an unscoped read returns no rows rather
// than every partition's. These rows are money state: a blank partition that matched everything would read and
// mutate another partition's customers, subscriptions and ledger. Every write of a row with no partition
// answers ErrNoPartition and stores nothing, so no row is kept that no read can reach. There is no unscoped read; a caller
// that must cross partitions names the partition it means. Only sealed material reaches the store for the payment ledger (encrypted
// charge id / client secret). A consumer supplies a *sql.DB store (the shipped default), a mesh-RPC
// gateway, or the reference MemStore. The payment PROVIDER (Stripe/etc.) is a separate injected seam,
// not part of this contract.
type Store interface {
	// ── Customers ──
	// CreateCustomer stores c as the partition's customer for its subject (UserID).
	// One customer per (partition, subject): a second create for the pair answers
	// ErrCustomerExists and writes nothing. Service.EnsureCustomer is its caller, so
	// the partition's callers that name no subject share one customer.
	CreateCustomer(ctx context.Context, c *Customer) error
	GetCustomer(ctx context.Context, partition, id string) (*Customer, error)
	GetCustomerByProviderRef(ctx context.Context, providerRef string) (*Customer, error)
	GetCustomerByUser(ctx context.Context, partition, userID string) (*Customer, error)
	SetCustomerProviderRef(ctx context.Context, partition, id, providerRef string) error
	UpdateCustomer(ctx context.Context, c *Customer) error

	// ── Products & prices ──
	// The catalog (products, prices and discounts) is platform-wide: its rows carry CatalogPartition,
	// every partition reads it through CatalogPartition, and CreateProduct, CreatePrice,
	// SetPriceProviderRef and UpsertDiscount refuse any other partition with ErrCatalogPartition.
	CreateProduct(ctx context.Context, p *Product) error
	GetProduct(ctx context.Context, partition, id string) (*Product, error)
	ListProducts(ctx context.Context, partition string, activeOnly bool) ([]*Product, error)
	CreatePrice(ctx context.Context, p *Price) error
	GetPrice(ctx context.Context, partition, id string) (*Price, error)
	ListPrices(ctx context.Context, partition, productID string) ([]*Price, error)
	SetPriceProviderRef(ctx context.Context, partition, id, providerRef string) error

	// ── Subscriptions ──
	CreateSubscription(ctx context.Context, s *Subscription) error
	// CreateSubscriptionWithItems stores s with the first item's price and quantity on the row and
	// every item as a row of billing_subscription_items, in the order given, in one write; a list
	// with no item answers ErrNoSubscriptionItems and one with an item lacking a price, a quantity
	// below one or a price named twice ErrInvalidSubscriptionItem, and either writes nothing. It is
	// the write Service.SubscribeItems makes after the provider has created the subscription, with
	// the items the provider answered, so a subscription created with several items never stands
	// with only the first.
	CreateSubscriptionWithItems(ctx context.Context, s *Subscription, items []SubscriptionItem) error
	GetSubscription(ctx context.Context, partition, id string) (*Subscription, error)
	ListSubscriptionsByCustomer(ctx context.Context, partition, customerID string) ([]*Subscription, error)
	// UpdateSubscription writes the whole row, the status among it, within the partition; ErrNotFound
	// when the partition holds no subscription with that id. It is the provider event's write
	// (billinghost's webhook applier is its caller): it stores s.EventCreatedAt, keeps the stored
	// instant when s.EventCreatedAt is nil, and answers ErrStaleEvent, writing nothing, when
	// s.EventCreatedAt is earlier than the stored instant. StatusChangedAt and EnteredUnpaidAt are the
	// store's to move (see Subscription).
	UpdateSubscription(ctx context.Context, s *Subscription) error
	// UpdateSubscriptionItem writes a subscription's price and quantity within the partition and no
	// other column; ErrNotFound when the partition holds no subscription with that id. A caller
	// changing the row's one item uses it rather than UpdateSubscription, whose row write carries the
	// status: a row read before the provider's event and written back after it would put back the
	// status the event replaced. It leaves the item rows alone; UpdateSubscriptionItems writes both.
	// TODO: no caller in ai, io or library dials it.
	UpdateSubscriptionItem(ctx context.Context, partition, id, priceID string, quantity int64) error
	// UpdateSubscriptionItems replaces a subscription's item rows (billing_subscription_items) with
	// items, in the order given, and writes the first item's price and quantity onto the row, no
	// other column; ErrNotFound when the partition holds no subscription with that id,
	// ErrNoSubscriptionItems for an empty list. It is the write Service.UpdateSubscription makes
	// after the provider has answered, with the items the provider answered, for the reason
	// UpdateSubscriptionItem exists: the whole-row UpdateSubscription carries the status, and the
	// status is the provider event's to write.
	UpdateSubscriptionItems(ctx context.Context, partition, id string, items []SubscriptionItem) error
	// ListSubscriptionItems answers a subscription's item rows in order, price id and quantity each,
	// and none for a subscription the partition does not hold. CreateSubscription writes the row's
	// price and quantity as the first item, so every subscription's items are rows.
	ListSubscriptionItems(ctx context.Context, partition, id string) ([]SubscriptionItem, error)
	// UpdateSubscriptionCancel writes a subscription's cancel_at_period_end and canceled_at within the
	// partition and no other column; ErrNotFound when the partition holds no subscription with that
	// id. It is the write Service.CancelSubscription makes after the provider has cancelled, with the
	// two values the provider answered, for the same reason UpdateSubscriptionItem exists: the whole-row
	// UpdateSubscription carries the status, and the status is the provider event's to write.
	UpdateSubscriptionCancel(ctx context.Context, partition, id string, cancelAtPeriodEnd bool, canceledAt *time.Time) error

	// ── Invoices ──
	CreateInvoice(ctx context.Context, inv *Invoice, items []*InvoiceItem) error
	GetInvoice(ctx context.Context, partition, id string) (*Invoice, error)
	ListInvoicesByCustomer(ctx context.Context, partition, customerID string) ([]*Invoice, error)
	// ListInvoiceItems answers the lines of the partition's invoice, none for an invoice the partition
	// does not hold: the lines carry no partition of their own, so the invoice they belong to decides.
	ListInvoiceItems(ctx context.Context, partition, invoiceID string) ([]*InvoiceItem, error)
	// UpdateInvoiceStatus writes the partition's invoice's status and amount paid, and its paid
	// instant once when the status is paid; ErrNotFound when the partition holds no invoice with
	// that id. A status the stored one does not lead to in the invoice lifecycle
	// (invoiceStatusOrder) answers ErrStatusOutOfOrder and writes nothing, so a paid or void
	// invoice keeps its outcome whatever event arrives after it. The billing host's webhook
	// applier is its caller.
	UpdateInvoiceStatus(ctx context.Context, partition, id, status string, amountPaidCents int64) error

	// ── Payment ledger (keyed by partition and idempotency key, append-only status advance) ──
	// RecordPayment inserts a ledger row keyed by (row.Partition, row.IdempotencyKey); a key the
	// partition already holds returns the existing row without inserting (idempotent), so a retry never
	// double-records a payment, and another partition's row under the same key is a different row. A
	// row with no partition answers ErrNoPartition and one with no key ErrIdempotencyKeyRequired, and
	// neither is written.
	RecordPayment(ctx context.Context, row *PaymentLedgerRow) (existing *PaymentLedgerRow, err error)
	// GetPaymentByKey reads the partition's ledger row for a key; ErrNotFound when the partition holds
	// none, whatever another partition holds under the key.
	GetPaymentByKey(ctx context.Context, partition, idempotencyKey string) (*PaymentLedgerRow, error)
	// UpdatePaymentStatus writes the status, and the sealed charge ref when one is given, of the
	// partition's ledger row for a key; ErrNotFound when the partition holds none. A status the stored
	// one does not lead to in the payment-intent lifecycle (paymentStatusOrder) answers
	// ErrStatusOutOfOrder and writes nothing, so a succeeded or canceled payment keeps its outcome.
	UpdatePaymentStatus(ctx context.Context, partition, idempotencyKey, status, chargeRefSealed string) error
	ListPaymentsByCustomer(ctx context.Context, partition, customerID string) ([]*PaymentLedgerRow, error)

	// ── Usage ──
	RecordUsage(ctx context.Context, u *UsageRecord) error
	SummarizeUsage(ctx context.Context, partition, customerID string, from, to time.Time) ([]*UsageSummary, error)

	// ── Credits ──
	GrantCredit(ctx context.Context, c *Credit) error
	ListCredits(ctx context.Context, partition, customerID string) ([]*Credit, error)
	// CreditBalance sums non-expired credits (as of `now`) for a customer, returning the balance per currency.
	CreditBalance(ctx context.Context, partition, customerID string, now time.Time) (map[string]int64, error)

	// ── Payment methods ──
	// AddPaymentMethod inserts a customer's stored payment instrument; when pm.IsDefault is set it also
	// clears the default flag on the customer's other methods (at most one default per customer).
	AddPaymentMethod(ctx context.Context, pm *PaymentMethod) error
	// ListPaymentMethods returns the customer's Active methods, newest first.
	ListPaymentMethods(ctx context.Context, partition, customerID string) ([]*PaymentMethod, error)
	// GetPaymentMethod reads one method by id within the partition.
	GetPaymentMethod(ctx context.Context, partition, id string) (*PaymentMethod, error)
	// SetDefaultPaymentMethod makes id the customer's sole default (clearing the others); it errors
	// ErrNotFound when id is not an Active method of customerID in the partition.
	SetDefaultPaymentMethod(ctx context.Context, partition, customerID, id string) error
	// RemovePaymentMethod soft-deletes a method (Active=false) within the partition.
	RemovePaymentMethod(ctx context.Context, partition, id string) error

	// ── Discounts ──
	// UpsertDiscount inserts a catalogue discount or rewrites the one its partition holds under the id
	// (the out-of-band sync/seed path that mirrors a provider coupon into the local catalogue). An id
	// another partition holds answers ErrDiscountPartition and changes nothing, so a write never moves a
	// discount, its uses or its limit to another partition. A rewrite keeps the uses taken (current_uses
	// is the apply path's to move); an insert stores d.CurrentUses.
	UpsertDiscount(ctx context.Context, d *Discount) error
	// GetDiscount reads a catalogue discount by id within the partition (CatalogPartition for the
	// catalog); ErrDiscountNotFound when absent.
	GetDiscount(ctx context.Context, partition, discountID string) (*Discount, error)
	// IsDiscountApplied reports whether (subscriptionRef, discountID) is already recorded in the partition.
	IsDiscountApplied(ctx context.Context, partition, subscriptionRef, discountID string) (bool, error)
	// RecordAppliedDiscount takes one use of the catalog's discount and records the partition's
	// (subscriptionRef, discountID) application, in one write: ErrNoPartition for an empty partition,
	// ErrDiscountAlreadyApplied when the pair already exists, ErrDiscountNotFound when the catalog
	// holds no such discount, and ErrDiscountExhausted when the
	// discount's MaxUses is above zero and current_uses has reached it; a refusal takes no use and
	// records nothing, so concurrent applications never pass the limit. Every application is recorded
	// under its reference: a subscription's provider reference, or for a customer-wide application the
	// reference Service.ApplyDiscount names the customer by (customerWideRef). An empty subscriptionRef
	// or discountID answers ErrNotFound and takes nothing. Service.ApplyDiscount calls it before the provider attach, so a discount at its
	// limit never reaches the provider.
	RecordAppliedDiscount(ctx context.Context, partition, subscriptionRef, discountID string) (*AppliedDiscount, error)
	// ReleaseAppliedDiscount gives back the use RecordAppliedDiscount took and removes its pair, in one
	// write, current_uses never going below zero: ErrNotFound when the partition holds no such pair
	// (an empty subscriptionRef included), and then nothing is given back, so no use is returned
	// without the record that it was taken. Service.ApplyDiscount calls it when the provider refuses the
	// attach, so a failed attach neither spends a use nor leaves a pair that would answer a retry
	// AlreadyApplied.
	ReleaseAppliedDiscount(ctx context.Context, partition, subscriptionRef, discountID string) error
}
