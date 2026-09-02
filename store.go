package billing

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned by single-record reads when nothing matches.
var ErrNotFound = errors.New("billing: not found")

// Store is the persistence contract for the billing money-state. The Partition is opaque and the store
// filters on it, never interprets it; an empty partition ("") on an partition-scoped read means "any partition"
// (the admin/all-scope view). Only sealed material reaches the store for the payment ledger (encrypted
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
}
