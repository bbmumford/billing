package billing

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned by single-record reads when nothing matches.
var ErrNotFound = errors.New("billing: not found")

// Store is the persistence contract for the billing money-state. The Partition is opaque and the store
// filters on it, never interprets it; an empty partition ("") on a partition-scoped read matches NOTHING —
// the predicates read (? <> '' AND partition = ?), so an unscoped read returns no rows rather than every
// partition's. These rows are money state: a blank partition that matched everything would read and
// mutate another partition's customers, subscriptions and ledger. There is no unscoped read; a caller
// that must cross partitions names the partition it means. Only sealed material reaches the store for the payment ledger (encrypted
// charge id / client secret). A consumer supplies a *sql.DB store (the shipped default), a mesh-RPC
// gateway, or the reference MemStore. The payment PROVIDER (Stripe/etc.) is a separate injected seam,
// not part of this contract.
type Store interface {
	// ── Customers ──
	CreateCustomer(ctx context.Context, c *Customer) error
	GetCustomer(ctx context.Context, partition, id string) (*Customer, error)
	GetCustomerByProviderRef(ctx context.Context, providerRef string) (*Customer, error)
	GetCustomerByUser(ctx context.Context, partition, userID string) (*Customer, error)
	SetCustomerProviderRef(ctx context.Context, partition, id, providerRef string) error
	UpdateCustomer(ctx context.Context, c *Customer) error

	// ── Products & prices ──
	CreateProduct(ctx context.Context, p *Product) error
	GetProduct(ctx context.Context, partition, id string) (*Product, error)
	ListProducts(ctx context.Context, partition string, activeOnly bool) ([]*Product, error)
	CreatePrice(ctx context.Context, p *Price) error
	GetPrice(ctx context.Context, partition, id string) (*Price, error)
	ListPrices(ctx context.Context, partition, productID string) ([]*Price, error)
	SetPriceProviderRef(ctx context.Context, partition, id, providerRef string) error

	// ── Subscriptions ──
	CreateSubscription(ctx context.Context, s *Subscription) error
	GetSubscription(ctx context.Context, partition, id string) (*Subscription, error)
	ListSubscriptionsByCustomer(ctx context.Context, partition, customerID string) ([]*Subscription, error)
	UpdateSubscription(ctx context.Context, s *Subscription) error
	// UpdateSubscriptionItem writes a subscription's price and quantity within the partition and no
	// other column; ErrNotFound when the partition holds no subscription with that id. A caller
	// changing the item uses it rather than UpdateSubscription, whose row write carries the status:
	// a row read before the provider's event and written back after it would put back the status the
	// event replaced. billinghost's UpdateSubscription is its caller.
	UpdateSubscriptionItem(ctx context.Context, partition, id, priceID string, quantity int64) error
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
	ListInvoiceItems(ctx context.Context, invoiceID string) ([]*InvoiceItem, error)
	UpdateInvoiceStatus(ctx context.Context, partition, id, status string, amountPaidCents int64) error

	// ── Payment ledger (idempotency-keyed, append-only status advance) ──
	// RecordPayment inserts a ledger row keyed by IdempotencyKey; a duplicate key returns the existing
	// row without inserting (idempotent), so a retry never double-records a payment.
	RecordPayment(ctx context.Context, row *PaymentLedgerRow) (existing *PaymentLedgerRow, err error)
	GetPaymentByKey(ctx context.Context, idempotencyKey string) (*PaymentLedgerRow, error)
	UpdatePaymentStatus(ctx context.Context, idempotencyKey, status, chargeRefSealed string) error
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
	// UpsertDiscount inserts or replaces a catalogue discount (the out-of-band sync/seed path that mirrors
	// a provider coupon into the local catalogue). current_uses is preserved by the apply path, not this.
	UpsertDiscount(ctx context.Context, d *Discount) error
	// GetDiscount reads a catalogue discount by id within the partition; ErrDiscountNotFound when absent.
	GetDiscount(ctx context.Context, partition, discountID string) (*Discount, error)
	// IsDiscountApplied reports whether (subscriptionRef, discountID) is already recorded in the partition.
	IsDiscountApplied(ctx context.Context, partition, subscriptionRef, discountID string) (bool, error)
	// RecordAppliedDiscount records a (subscriptionRef, discountID) application and bumps the discount's
	// current_uses in one transaction; ErrDiscountAlreadyApplied when the pair already exists.
	RecordAppliedDiscount(ctx context.Context, partition, subscriptionRef, discountID string) (*AppliedDiscount, error)
}
