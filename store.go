package billing

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned by single-record reads when nothing matches.
var ErrNotFound = errors.New("billing: not found")

// Store is the persistence contract for the billing money-state. The Owner is opaque and the store
// filters on it, never interprets it; an empty owner ("") on an owner-scoped read means "any owner"
// (the admin/all-scope view). Only sealed material reaches the store for the payment ledger (encrypted
// charge id / client secret). A consumer supplies a *sql.DB store (the shipped default), a mesh-RPC
// gateway, or the reference MemStore. The payment PROVIDER (Stripe/etc.) is a separate injected seam,
// not part of this contract.
type Store interface {
	// ── Customers ──
	CreateCustomer(ctx context.Context, c *Customer) error
	GetCustomer(ctx context.Context, owner, id string) (*Customer, error)
	GetCustomerByProviderRef(ctx context.Context, providerRef string) (*Customer, error)
	GetCustomerByUser(ctx context.Context, owner, userID string) (*Customer, error)
	SetCustomerProviderRef(ctx context.Context, owner, id, providerRef string) error
	UpdateCustomer(ctx context.Context, c *Customer) error

	// ── Products & prices ──
	CreateProduct(ctx context.Context, p *Product) error
	GetProduct(ctx context.Context, owner, id string) (*Product, error)
	ListProducts(ctx context.Context, owner string, activeOnly bool) ([]*Product, error)
	CreatePrice(ctx context.Context, p *Price) error
	GetPrice(ctx context.Context, owner, id string) (*Price, error)
	ListPrices(ctx context.Context, owner, productID string) ([]*Price, error)
	SetPriceProviderRef(ctx context.Context, owner, id, providerRef string) error

	// ── Subscriptions ──
	CreateSubscription(ctx context.Context, s *Subscription) error
	GetSubscription(ctx context.Context, owner, id string) (*Subscription, error)
	ListSubscriptionsByCustomer(ctx context.Context, owner, customerID string) ([]*Subscription, error)
	UpdateSubscription(ctx context.Context, s *Subscription) error

	// ── Invoices ──
	CreateInvoice(ctx context.Context, inv *Invoice, items []*InvoiceItem) error
	GetInvoice(ctx context.Context, owner, id string) (*Invoice, error)
	ListInvoicesByCustomer(ctx context.Context, owner, customerID string) ([]*Invoice, error)
	ListInvoiceItems(ctx context.Context, invoiceID string) ([]*InvoiceItem, error)
	UpdateInvoiceStatus(ctx context.Context, owner, id, status string, amountPaidCents int64) error

	// ── Payment ledger (idempotency-keyed, append-only status advance) ──
	// RecordPayment inserts a ledger row keyed by IdempotencyKey; a duplicate key returns the existing
	// row without inserting (idempotent), so a retry never double-records a payment.
	RecordPayment(ctx context.Context, row *PaymentLedgerRow) (existing *PaymentLedgerRow, err error)
	GetPaymentByKey(ctx context.Context, idempotencyKey string) (*PaymentLedgerRow, error)
	UpdatePaymentStatus(ctx context.Context, idempotencyKey, status, chargeRefSealed string) error
	ListPaymentsByCustomer(ctx context.Context, owner, customerID string) ([]*PaymentLedgerRow, error)

	// ── Usage ──
	RecordUsage(ctx context.Context, u *UsageRecord) error
	SummarizeUsage(ctx context.Context, owner, customerID string, from, to time.Time) ([]*UsageSummary, error)

	// ── Credits ──
	GrantCredit(ctx context.Context, c *Credit) error
	ListCredits(ctx context.Context, owner, customerID string) ([]*Credit, error)
	// CreditBalance sums non-expired credits (as of `now`) for a customer, returning the balance per currency.
	CreditBalance(ctx context.Context, owner, customerID string, now time.Time) (map[string]int64, error)
}
