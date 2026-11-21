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
// (*sql.DB) store must match. Thread-safe; clock and id generator injectable. Partition is opaque; "" on a
// partition-scoped read matches nothing — see partitionMatch, which the durable store's predicates
// are held to.
type MemStore struct {
	mu                sync.Mutex
	customers         map[string]*Customer
	products          map[string]*Product
	prices            map[string]*Price
	subscriptions     map[string]*Subscription
	subscriptionItems map[string][]SubscriptionItem // keyed by Subscription.ID; the row's items in order
	invoices          map[string]*Invoice
	invoiceItems      []*InvoiceItem
	payments          map[paymentKey]*PaymentLedgerRow
	usage             []*UsageRecord
	credits           []*Credit
	paymentMethods    map[string]*PaymentMethod // keyed by PaymentMethod.ID
	discounts         map[string]*Discount      // catalogue, keyed by DiscountID
	appliedDiscounts  []*AppliedDiscount        // idempotency ledger
	now               func() time.Time
	newID             func() string
}

// NewMemStore builds an empty billing store.
func NewMemStore() *MemStore {
	return &MemStore{
		customers:         map[string]*Customer{},
		products:          map[string]*Product{},
		prices:            map[string]*Price{},
		subscriptions:     map[string]*Subscription{},
		subscriptionItems: map[string][]SubscriptionItem{},
		invoices:          map[string]*Invoice{},
		payments:          map[paymentKey]*PaymentLedgerRow{},
		paymentMethods:    map[string]*PaymentMethod{},
		discounts:         map[string]*Discount{},
		now:               time.Now,
		newID:             randomID,
	}
}

func randomID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// partitionMatch reports whether a row held under partition `have` is visible to a
// caller asking for partition `want`. Every customer, subscription, product, price,
// payment, credit, usage and discount lookup in this store filters through it.
//
// An empty `want` names no partition and matches nothing. SQLStore answers the same
// way — its predicates reject an empty partition parameter before comparing — and the
// two have to agree,
// because a test written against this store is what certifies the other's behaviour.
// Rows here decide what is charged and to whom, so a `want` that matched everything
// would read and mutate another partition's money.
func partitionMatch(want, have string) bool { return want != "" && want == have }

// ── Customers ──

// CreateCustomer is the reference behaviour SQLStore's is held to: the partition's
// customer for the subject (UserID) is stored once, a second create for the pair
// answers ErrCustomerExists and changes nothing, and the id and instants are
// assigned to c only for a row that is stored.
func (m *MemStore) CreateCustomer(_ context.Context, c *Customer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.customers {
		if e.Partition == c.Partition && e.UserID == c.UserID {
			return ErrCustomerExists
		}
	}
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

func (m *MemStore) GetCustomer(_ context.Context, partition, id string) (*Customer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.customers[id]
	if !ok || !partitionMatch(partition, c.Partition) {
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

func (m *MemStore) GetCustomerByUser(_ context.Context, partition, userID string) (*Customer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.customers {
		if partitionMatch(partition, c.Partition) && c.UserID == userID {
			return cloneCustomer(c), nil
		}
	}
	return nil, ErrNotFound
}

func (m *MemStore) SetCustomerProviderRef(_ context.Context, partition, id, providerRef string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.customers[id]
	if !ok || !partitionMatch(partition, c.Partition) {
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
	if !ok || !partitionMatch(c.Partition, e.Partition) {
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

func (m *MemStore) GetProduct(_ context.Context, partition, id string) (*Product, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.products[id]
	if !ok || !partitionMatch(partition, p.Partition) {
		return nil, ErrNotFound
	}
	cp := *p
	return &cp, nil
}

func (m *MemStore) ListProducts(_ context.Context, partition string, activeOnly bool) ([]*Product, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Product
	for _, p := range m.products {
		if partitionMatch(partition, p.Partition) && (!activeOnly || p.Active) {
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

func (m *MemStore) GetPrice(_ context.Context, partition, id string) (*Price, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.prices[id]
	if !ok || !partitionMatch(partition, p.Partition) {
		return nil, ErrNotFound
	}
	cp := *p
	return &cp, nil
}

func (m *MemStore) ListPrices(_ context.Context, partition, productID string) ([]*Price, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Price
	for _, p := range m.prices {
		if partitionMatch(partition, p.Partition) && p.ProductID == productID {
			cp := *p
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AmountCents < out[j].AmountCents })
	return out, nil
}

func (m *MemStore) SetPriceProviderRef(_ context.Context, partition, id, providerRef string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.prices[id]
	if !ok || !partitionMatch(partition, p.Partition) {
		return ErrNotFound
	}
	p.ProviderRef = providerRef
	return nil
}

// ── Subscriptions ──

// CreateSubscription is the reference behaviour SQLStore's is held to: the row
// is stored and, when it names a price, that price and quantity become the
// subscription's one item.
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
	s.StatusChangedAt, s.EnteredUnpaidAt = statusClock(nil, s.Status, now)
	m.subscriptions[s.ID] = cloneSubscription(s)
	if s.PriceID != "" {
		m.subscriptionItems[s.ID] = []SubscriptionItem{{PriceID: s.PriceID, Quantity: s.Quantity}}
	}
	return nil
}

func (m *MemStore) GetSubscription(_ context.Context, partition, id string) (*Subscription, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.subscriptions[id]
	if !ok || !partitionMatch(partition, s.Partition) {
		return nil, ErrNotFound
	}
	return cloneSubscription(s), nil
}

func (m *MemStore) ListSubscriptionsByCustomer(_ context.Context, partition, customerID string) ([]*Subscription, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Subscription
	for _, s := range m.subscriptions {
		if partitionMatch(partition, s.Partition) && s.CustomerID == customerID {
			out = append(out, cloneSubscription(s))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// UpdateSubscription is the reference behaviour SQLStore's write is held to: the
// status instants move by statusClock, and the event instant is the caller's,
// the stored one on a nil caller instant, and refused with ErrStaleEvent when the
// caller's is earlier than the stored one, the row left unchanged.
func (m *MemStore) UpdateSubscription(_ context.Context, s *Subscription) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.subscriptions[s.ID]
	if !ok || !partitionMatch(s.Partition, e.Partition) {
		return ErrNotFound
	}
	if s.EventCreatedAt != nil && e.EventCreatedAt != nil && s.EventCreatedAt.Before(*e.EventCreatedAt) {
		return ErrStaleEvent
	}
	upd := cloneSubscription(s)
	upd.CreatedAt = e.CreatedAt
	upd.UpdatedAt = m.now()
	upd.StatusChangedAt, upd.EnteredUnpaidAt = statusClock(e, s.Status, upd.UpdatedAt)
	if upd.EventCreatedAt == nil {
		upd.EventCreatedAt = clonePtr(e.EventCreatedAt)
	}
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

func (m *MemStore) GetInvoice(_ context.Context, partition, id string) (*Invoice, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inv, ok := m.invoices[id]
	if !ok || !partitionMatch(partition, inv.Partition) {
		return nil, ErrNotFound
	}
	return cloneInvoice(inv), nil
}

func (m *MemStore) ListInvoicesByCustomer(_ context.Context, partition, customerID string) ([]*Invoice, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Invoice
	for _, inv := range m.invoices {
		if partitionMatch(partition, inv.Partition) && inv.CustomerID == customerID {
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

func (m *MemStore) UpdateInvoiceStatus(_ context.Context, partition, id, status string, amountPaidCents int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	inv, ok := m.invoices[id]
	if !ok || !partitionMatch(partition, inv.Partition) {
		return ErrNotFound
	}
	if !invoiceStatusOrder.follows(inv.Status, status) {
		return ErrStatusOutOfOrder
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

// paymentKey is the ledger's key, the partition and the idempotency key, as SQLStore's primary key
// on billing_payments is: one key names one row per partition.
type paymentKey struct{ partition, idempotencyKey string }

func (m *MemStore) RecordPayment(_ context.Context, row *PaymentLedgerRow) (*PaymentLedgerRow, error) {
	if row.Partition == "" {
		return nil, ErrNoPartition
	}
	if row.IdempotencyKey == "" {
		return nil, ErrIdempotencyKeyRequired
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	k := paymentKey{row.Partition, row.IdempotencyKey}
	if existing, ok := m.payments[k]; ok {
		return clonePayment(existing), nil // idempotent: a retry returns the existing row
	}
	now := m.now()
	if row.CreatedAt.IsZero() {
		row.CreatedAt = now
	}
	row.UpdatedAt = now
	m.payments[k] = clonePayment(row)
	return nil, nil
}

func (m *MemStore) GetPaymentByKey(_ context.Context, partition, idempotencyKey string) (*PaymentLedgerRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.payments[paymentKey{partition, idempotencyKey}]
	if !ok || !partitionMatch(partition, row.Partition) {
		return nil, ErrNotFound
	}
	return clonePayment(row), nil
}

func (m *MemStore) UpdatePaymentStatus(_ context.Context, partition, idempotencyKey, status, chargeRefSealed string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.payments[paymentKey{partition, idempotencyKey}]
	if !ok || !partitionMatch(partition, row.Partition) {
		return ErrNotFound
	}
	if !paymentStatusOrder.follows(row.Status, status) {
		return ErrStatusOutOfOrder
	}
	row.Status = status
	if chargeRefSealed != "" {
		row.ChargeRefSealed = chargeRefSealed
	}
	row.UpdatedAt = m.now()
	return nil
}

func (m *MemStore) ListPaymentsByCustomer(_ context.Context, partition, customerID string) ([]*PaymentLedgerRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*PaymentLedgerRow
	for _, row := range m.payments {
		if partitionMatch(partition, row.Partition) && row.CustomerID == customerID {
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

func (m *MemStore) SummarizeUsage(_ context.Context, partition, customerID string, from, to time.Time) ([]*UsageSummary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	byProduct := map[string]*UsageSummary{}
	for _, u := range m.usage {
		if !partitionMatch(partition, u.Partition) || u.CustomerID != customerID {
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

func (m *MemStore) ListCredits(_ context.Context, partition, customerID string) ([]*Credit, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Credit
	for _, c := range m.credits {
		if partitionMatch(partition, c.Partition) && c.CustomerID == customerID {
			out = append(out, cloneCredit(c))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (m *MemStore) CreditBalance(_ context.Context, partition, customerID string, now time.Time) (map[string]int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	bal := map[string]int64{}
	for _, c := range m.credits {
		if !partitionMatch(partition, c.Partition) || c.CustomerID != customerID {
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
	cp.EnteredUnpaidAt = clonePtr(s.EnteredUnpaidAt)
	cp.EventCreatedAt = clonePtr(s.EventCreatedAt)
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

func clonePaymentMethod(pm *PaymentMethod) *PaymentMethod { cp := *pm; return &cp }

// ── payment methods ──

func (m *MemStore) AddPaymentMethod(_ context.Context, pm *PaymentMethod) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if pm.ID == "" {
		pm.ID = m.newID()
	}
	now := m.now()
	if pm.CreatedAt.IsZero() {
		pm.CreatedAt = now
	}
	pm.UpdatedAt = now
	pm.Active = true
	if pm.IsDefault {
		for _, e := range m.paymentMethods {
			if e.CustomerID == pm.CustomerID && e.Active && partitionMatch(pm.Partition, e.Partition) {
				e.IsDefault = false
			}
		}
	}
	m.paymentMethods[pm.ID] = clonePaymentMethod(pm)
	return nil
}

func (m *MemStore) ListPaymentMethods(_ context.Context, partition, customerID string) ([]*PaymentMethod, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*PaymentMethod
	for _, pm := range m.paymentMethods {
		if pm.CustomerID == customerID && pm.Active && partitionMatch(partition, pm.Partition) {
			out = append(out, clonePaymentMethod(pm))
		}
	}
	return out, nil
}

func (m *MemStore) GetPaymentMethod(_ context.Context, partition, id string) (*PaymentMethod, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pm, ok := m.paymentMethods[id]
	if !ok || !partitionMatch(partition, pm.Partition) {
		return nil, ErrNotFound
	}
	return clonePaymentMethod(pm), nil
}

func (m *MemStore) SetDefaultPaymentMethod(_ context.Context, partition, customerID, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	target, ok := m.paymentMethods[id]
	if !ok || !target.Active || target.CustomerID != customerID || !partitionMatch(partition, target.Partition) {
		return ErrNotFound
	}
	now := m.now()
	for _, e := range m.paymentMethods {
		if e.CustomerID == customerID && e.Active && partitionMatch(partition, e.Partition) {
			e.IsDefault = false
			e.UpdatedAt = now
		}
	}
	target.IsDefault = true
	target.UpdatedAt = now
	return nil
}

func (m *MemStore) RemovePaymentMethod(_ context.Context, partition, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	pm, ok := m.paymentMethods[id]
	if !ok || !partitionMatch(partition, pm.Partition) {
		return ErrNotFound
	}
	pm.Active = false
	pm.IsDefault = false
	pm.UpdatedAt = m.now()
	return nil
}

// ── discounts ──

func (m *MemStore) UpsertDiscount(_ context.Context, d *Discount) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d.DiscountID == "" {
		d.DiscountID = m.newID()
	}
	cp := *d
	m.discounts[d.DiscountID] = &cp
	return nil
}

func (m *MemStore) GetDiscount(_ context.Context, partition, discountID string) (*Discount, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.discounts[discountID]
	if !ok || !partitionMatch(partition, d.Partition) {
		return nil, ErrDiscountNotFound
	}
	cp := *d
	return &cp, nil
}

func (m *MemStore) IsDiscountApplied(_ context.Context, partition, subscriptionRef, discountID string) (bool, error) {
	if subscriptionRef == "" || discountID == "" {
		return false, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.appliedDiscounts {
		if a.SubscriptionRef == subscriptionRef && a.DiscountID == discountID && partitionMatch(partition, a.Partition) {
			return true, nil
		}
	}
	return false, nil
}

func (m *MemStore) RecordAppliedDiscount(_ context.Context, partition, subscriptionRef, discountID string) (*AppliedDiscount, error) {
	if subscriptionRef == "" || discountID == "" {
		return nil, ErrNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.appliedDiscounts {
		if a.SubscriptionRef == subscriptionRef && a.DiscountID == discountID && partitionMatch(partition, a.Partition) {
			return nil, ErrDiscountAlreadyApplied
		}
	}
	rec := &AppliedDiscount{ID: m.newID(), Partition: partition, SubscriptionRef: subscriptionRef, DiscountID: discountID}
	m.appliedDiscounts = append(m.appliedDiscounts, rec)
	if d, ok := m.discounts[discountID]; ok {
		d.CurrentUses++
	}
	cp := *rec
	return &cp, nil
}

var _ Store = (*MemStore)(nil)
