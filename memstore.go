package billing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sort"
	"sync"
	"time"
)

// MemStore is the in-memory reference Store — the test/dev backend and the semantic reference a durable
// (*sql.DB) store must match. Thread-safe; clock and id generator injectable. Owner is opaque; "" on an
// owner-scoped read means any owner.
type MemStore struct {
	mu            sync.Mutex
	customers     map[string]*Customer
	products      map[string]*Product
	prices        map[string]*Price
	subscriptions map[string]*Subscription
	invoices      map[string]*Invoice
	invoiceItems  []*InvoiceItem
	payments      map[string]*PaymentLedgerRow // keyed by IdempotencyKey
	usage         []*UsageRecord
	credits       []*Credit
	now           func() time.Time
	newID         func() string
}

// NewMemStore builds an empty billing store.
func NewMemStore() *MemStore {
	return &MemStore{
		customers:     map[string]*Customer{},
		products:      map[string]*Product{},
		prices:        map[string]*Price{},
		subscriptions: map[string]*Subscription{},
		invoices:      map[string]*Invoice{},
		payments:      map[string]*PaymentLedgerRow{},
		now:           time.Now,
		newID:         randomID,
	}
}

func randomID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func ownerMatch(want, have string) bool { return want == "" || want == have }

// ── Customers ──

func (m *MemStore) CreateCustomer(_ context.Context, c *Customer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c.ID == "" {
		c.ID = m.newID()
	}
	now := m.now()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	c.UpdatedAt = now
	m.customers[c.ID] = cloneCustomer(c)
	return nil
}

func (m *MemStore) GetCustomer(_ context.Context, owner, id string) (*Customer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.customers[id]
	if !ok || !ownerMatch(owner, c.Owner) {
		return nil, ErrNotFound
	}
	return cloneCustomer(c), nil
}

func (m *MemStore) GetCustomerByProviderRef(_ context.Context, providerRef string) (*Customer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if providerRef == "" {
		return nil, ErrNotFound
	}
	for _, c := range m.customers {
		if c.ProviderRef == providerRef {
			return cloneCustomer(c), nil
		}
	}
	return nil, ErrNotFound
}

func (m *MemStore) GetCustomerByUser(_ context.Context, owner, userID string) (*Customer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.customers {
		if ownerMatch(owner, c.Owner) && c.UserID == userID {
			return cloneCustomer(c), nil
		}
	}
	return nil, ErrNotFound
}

func (m *MemStore) SetCustomerProviderRef(_ context.Context, owner, id, providerRef string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.customers[id]
	if !ok || !ownerMatch(owner, c.Owner) {
		return ErrNotFound
	}
	c.ProviderRef = providerRef
	c.UpdatedAt = m.now()
	return nil
}

func (m *MemStore) UpdateCustomer(_ context.Context, c *Customer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.customers[c.ID]
	if !ok || !ownerMatch(c.Owner, e.Owner) {
		return ErrNotFound
	}
	upd := cloneCustomer(c)
	upd.CreatedAt = e.CreatedAt
	upd.UpdatedAt = m.now()
	m.customers[c.ID] = upd
	return nil
}

// ── Products & prices ──

func (m *MemStore) CreateProduct(_ context.Context, p *Product) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p.ID == "" {
		p.ID = m.newID()
	}
	now := m.now()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	cp := *p
	m.products[p.ID] = &cp
	return nil
}

func (m *MemStore) GetProduct(_ context.Context, owner, id string) (*Product, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.products[id]
	if !ok || !ownerMatch(owner, p.Owner) {
		return nil, ErrNotFound
	}
	cp := *p
	return &cp, nil
}

func (m *MemStore) ListProducts(_ context.Context, owner string, activeOnly bool) ([]*Product, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Product
	for _, p := range m.products {
		if ownerMatch(owner, p.Owner) && (!activeOnly || p.Active) {
			cp := *p
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *MemStore) CreatePrice(_ context.Context, p *Price) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p.ID == "" {
		p.ID = m.newID()
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = m.now()
	}
	cp := *p
	m.prices[p.ID] = &cp
	return nil
}

func (m *MemStore) GetPrice(_ context.Context, owner, id string) (*Price, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.prices[id]
	if !ok || !ownerMatch(owner, p.Owner) {
		return nil, ErrNotFound
	}
	cp := *p
	return &cp, nil
}

func (m *MemStore) ListPrices(_ context.Context, owner, productID string) ([]*Price, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Price
	for _, p := range m.prices {
		if ownerMatch(owner, p.Owner) && p.ProductID == productID {
			cp := *p
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AmountCents < out[j].AmountCents })
	return out, nil
}

func (m *MemStore) SetPriceProviderRef(_ context.Context, owner, id, providerRef string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.prices[id]
	if !ok || !ownerMatch(owner, p.Owner) {
		return ErrNotFound
	}
	p.ProviderRef = providerRef
	return nil
}

// ── Subscriptions ──

func (m *MemStore) CreateSubscription(_ context.Context, s *Subscription) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.ID == "" {
		s.ID = m.newID()
	}
	now := m.now()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}
	s.UpdatedAt = now
	m.subscriptions[s.ID] = cloneSubscription(s)
	return nil
}

func (m *MemStore) GetSubscription(_ context.Context, owner, id string) (*Subscription, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.subscriptions[id]
	if !ok || !ownerMatch(owner, s.Owner) {
		return nil, ErrNotFound
	}
	return cloneSubscription(s), nil
}

func (m *MemStore) ListSubscriptionsByCustomer(_ context.Context, owner, customerID string) ([]*Subscription, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Subscription
	for _, s := range m.subscriptions {
		if ownerMatch(owner, s.Owner) && s.CustomerID == customerID {
			out = append(out, cloneSubscription(s))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (m *MemStore) UpdateSubscription(_ context.Context, s *Subscription) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.subscriptions[s.ID]
	if !ok || !ownerMatch(s.Owner, e.Owner) {
		return ErrNotFound
	}
	upd := cloneSubscription(s)
	upd.CreatedAt = e.CreatedAt
	upd.UpdatedAt = m.now()
	m.subscriptions[s.ID] = upd
	return nil
}

// ── Invoices ──

func (m *MemStore) CreateInvoice(_ context.Context, inv *Invoice, items []*InvoiceItem) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if inv.ID == "" {
		inv.ID = m.newID()
	}
	if inv.CreatedAt.IsZero() {
		inv.CreatedAt = m.now()
	}
	m.invoices[inv.ID] = cloneInvoice(inv)
	for _, it := range items {
		if it.ID == "" {
			it.ID = m.newID()
		}
		it.InvoiceID = inv.ID
		m.invoiceItems = append(m.invoiceItems, cloneItem(it))
	}
	return nil
}

func (m *MemStore) GetInvoice(_ context.Context, owner, id string) (*Invoice, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inv, ok := m.invoices[id]
	if !ok || !ownerMatch(owner, inv.Owner) {
		return nil, ErrNotFound
	}
	return cloneInvoice(inv), nil
}

func (m *MemStore) ListInvoicesByCustomer(_ context.Context, owner, customerID string) ([]*Invoice, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Invoice
	for _, inv := range m.invoices {
		if ownerMatch(owner, inv.Owner) && inv.CustomerID == customerID {
			out = append(out, cloneInvoice(inv))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (m *MemStore) ListInvoiceItems(_ context.Context, invoiceID string) ([]*InvoiceItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*InvoiceItem
	for _, it := range m.invoiceItems {
		if it.InvoiceID == invoiceID {
			out = append(out, cloneItem(it))
		}
	}
	return out, nil
}

func (m *MemStore) UpdateInvoiceStatus(_ context.Context, owner, id, status string, amountPaidCents int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	inv, ok := m.invoices[id]
	if !ok || !ownerMatch(owner, inv.Owner) {
		return ErrNotFound
	}
	inv.Status = status
	inv.AmountPaidCents = amountPaidCents
	if status == "paid" && inv.PaidAt == nil {
		t := m.now()
		inv.PaidAt = &t
	}
	return nil
}

// ── Payment ledger ──

func (m *MemStore) RecordPayment(_ context.Context, row *PaymentLedgerRow) (*PaymentLedgerRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.payments[row.IdempotencyKey]; ok {
		return clonePayment(existing), nil // idempotent: a retry returns the existing row
	}
	now := m.now()
	if row.CreatedAt.IsZero() {
		row.CreatedAt = now
	}
	row.UpdatedAt = now
	m.payments[row.IdempotencyKey] = clonePayment(row)
	return nil, nil
}

func (m *MemStore) GetPaymentByKey(_ context.Context, idempotencyKey string) (*PaymentLedgerRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.payments[idempotencyKey]
	if !ok {
		return nil, ErrNotFound
	}
	return clonePayment(row), nil
}

func (m *MemStore) UpdatePaymentStatus(_ context.Context, idempotencyKey, status, chargeRefSealed string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.payments[idempotencyKey]
	if !ok {
		return ErrNotFound
	}
	row.Status = status
	if chargeRefSealed != "" {
		row.ChargeRefSealed = chargeRefSealed
	}
	row.UpdatedAt = m.now()
	return nil
}

func (m *MemStore) ListPaymentsByCustomer(_ context.Context, owner, customerID string) ([]*PaymentLedgerRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*PaymentLedgerRow
	for _, row := range m.payments {
		if ownerMatch(owner, row.Owner) && row.CustomerID == customerID {
			out = append(out, clonePayment(row))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// ── Usage ──

func (m *MemStore) RecordUsage(_ context.Context, u *UsageRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if u.ID == "" {
		u.ID = m.newID()
	}
	if u.RecordedAt.IsZero() {
		u.RecordedAt = m.now()
	}
	m.usage = append(m.usage, cloneUsage(u))
	return nil
}

func (m *MemStore) SummarizeUsage(_ context.Context, owner, customerID string, from, to time.Time) ([]*UsageSummary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	byProduct := map[string]*UsageSummary{}
	for _, u := range m.usage {
		if !ownerMatch(owner, u.Owner) || u.CustomerID != customerID {
			continue
		}
		if u.RecordedAt.Before(from) || !u.RecordedAt.Before(to) {
			continue // [from, to)
		}
		s := byProduct[u.ProductID]
		if s == nil {
			s = &UsageSummary{ProductID: u.ProductID}
			byProduct[u.ProductID] = s
		}
		s.TotalQuantity += u.Quantity
		if u.Quantity > s.PeakQuantity {
			s.PeakQuantity = u.Quantity
		}
	}
	out := make([]*UsageSummary, 0, len(byProduct))
	for _, s := range byProduct {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProductID < out[j].ProductID })
	return out, nil
}

// ── Credits ──

func (m *MemStore) GrantCredit(_ context.Context, c *Credit) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c.ID == "" {
		c.ID = m.newID()
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = m.now()
	}
	m.credits = append(m.credits, cloneCredit(c))
	return nil
}

func (m *MemStore) ListCredits(_ context.Context, owner, customerID string) ([]*Credit, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Credit
	for _, c := range m.credits {
		if ownerMatch(owner, c.Owner) && c.CustomerID == customerID {
			out = append(out, cloneCredit(c))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (m *MemStore) CreditBalance(_ context.Context, owner, customerID string, now time.Time) (map[string]int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	bal := map[string]int64{}
	for _, c := range m.credits {
		if !ownerMatch(owner, c.Owner) || c.CustomerID != customerID {
			continue
		}
		if c.ExpiresAt != nil && !now.Before(*c.ExpiresAt) {
			continue // expired
		}
		bal[c.Currency] += c.AmountCents
	}
	return bal, nil
}

// ── clone helpers ──

func clonePtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}

func cloneStrMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	c := make(map[string]string, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

func cloneCustomer(c *Customer) *Customer { cp := *c; return &cp }

func cloneSubscription(s *Subscription) *Subscription {
	cp := *s
	cp.CurrentPeriodStart = clonePtr(s.CurrentPeriodStart)
	cp.CurrentPeriodEnd = clonePtr(s.CurrentPeriodEnd)
	cp.CanceledAt = clonePtr(s.CanceledAt)
	return &cp
}

func cloneInvoice(inv *Invoice) *Invoice {
	cp := *inv
	cp.DueAt = clonePtr(inv.DueAt)
	cp.PaidAt = clonePtr(inv.PaidAt)
	return &cp
}

func cloneItem(it *InvoiceItem) *InvoiceItem {
	cp := *it
	cp.Metadata = cloneStrMap(it.Metadata)
	return &cp
}

func clonePayment(row *PaymentLedgerRow) *PaymentLedgerRow { cp := *row; return &cp }

func cloneUsage(u *UsageRecord) *UsageRecord {
	cp := *u
	cp.Metadata = cloneStrMap(u.Metadata)
	return &cp
}

func cloneCredit(c *Credit) *Credit {
	cp := *c
	cp.ExpiresAt = clonePtr(c.ExpiresAt)
	return &cp
}

var _ Store = (*MemStore)(nil)
