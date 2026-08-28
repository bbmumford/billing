package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// SQLStore is the durable default Store over any database/sql driver — the shipped standalone store. The
// package imports only database/sql (stdlib); the APP supplies the opened *sql.DB and registers the
// driver, so billing pulls no driver dependency yet works out of the box. Owner is an opaque column the
// store filters on but never interprets ("" matches any owner). Money is INTEGER minor units; times
// persist as INTEGER Unix nanos with NULL for an unset *time.Time; metadata as JSON. Only sealed payment
// material reaches the ledger table. The Store interface stays the escape-hatch for a mesh gateway or
// MemStore.
type SQLStore struct{ db *sql.DB }

// NewSQLStore builds a store over an opened *sql.DB. Call InitSchema once before use.
func NewSQLStore(db *sql.DB) *SQLStore { return &SQLStore{db: db} }

var _ Store = (*SQLStore)(nil)

// InitSchema creates every table + index (idempotent); run once at boot.
func (s *SQLStore) InitSchema(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS billing_customers (
			id TEXT PRIMARY KEY, owner TEXT NOT NULL DEFAULT '', user_id TEXT NOT NULL DEFAULT '',
			org_id TEXT NOT NULL DEFAULT '', provider_ref TEXT NOT NULL DEFAULT '',
			billing_email TEXT NOT NULL DEFAULT '', billing_name TEXT NOT NULL DEFAULT '',
			currency TEXT NOT NULL DEFAULT '', tax_id TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_billing_customers_owner ON billing_customers(owner, user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_billing_customers_provref ON billing_customers(provider_ref)`,
		`CREATE TABLE IF NOT EXISTS billing_products (
			id TEXT PRIMARY KEY, owner TEXT NOT NULL DEFAULT '', service_id TEXT NOT NULL DEFAULT '',
			name TEXT NOT NULL DEFAULT '', description TEXT NOT NULL DEFAULT '',
			active INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_billing_products_owner ON billing_products(owner)`,
		`CREATE TABLE IF NOT EXISTS billing_prices (
			id TEXT PRIMARY KEY, owner TEXT NOT NULL DEFAULT '', product_id TEXT NOT NULL DEFAULT '',
			provider_ref TEXT NOT NULL DEFAULT '', currency TEXT NOT NULL DEFAULT '',
			amount_cents INTEGER NOT NULL DEFAULT 0, interval TEXT NOT NULL DEFAULT '',
			pricing_model TEXT NOT NULL DEFAULT '', tier_config TEXT NOT NULL DEFAULT '',
			percentage_rate REAL NOT NULL DEFAULT 0, created_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_billing_prices_product ON billing_prices(owner, product_id)`,
		`CREATE TABLE IF NOT EXISTS billing_subscriptions (
			id TEXT PRIMARY KEY, owner TEXT NOT NULL DEFAULT '', customer_id TEXT NOT NULL DEFAULT '',
			price_id TEXT NOT NULL DEFAULT '', provider_ref TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT '', quantity INTEGER NOT NULL DEFAULT 0,
			current_period_start INTEGER, current_period_end INTEGER,
			cancel_at_period_end INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL DEFAULT 0, canceled_at INTEGER
		)`,
		`CREATE INDEX IF NOT EXISTS idx_billing_subs_customer ON billing_subscriptions(owner, customer_id)`,
		`CREATE TABLE IF NOT EXISTS billing_invoices (
			id TEXT PRIMARY KEY, owner TEXT NOT NULL DEFAULT '', customer_id TEXT NOT NULL DEFAULT '',
			subscription_id TEXT NOT NULL DEFAULT '', provider_ref TEXT NOT NULL DEFAULT '',
			number TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT '', currency TEXT NOT NULL DEFAULT '',
			amount_due_cents INTEGER NOT NULL DEFAULT 0, amount_paid_cents INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL DEFAULT 0, due_at INTEGER, paid_at INTEGER
		)`,
		`CREATE INDEX IF NOT EXISTS idx_billing_invoices_customer ON billing_invoices(owner, customer_id)`,
		`CREATE TABLE IF NOT EXISTS billing_invoice_items (
			id TEXT PRIMARY KEY, invoice_id TEXT NOT NULL DEFAULT '', product_id TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT '', quantity INTEGER NOT NULL DEFAULT 0,
			unit_amount_cents INTEGER NOT NULL DEFAULT 0, amount_cents INTEGER NOT NULL DEFAULT 0,
			metadata TEXT NOT NULL DEFAULT '{}'
		)`,
		`CREATE INDEX IF NOT EXISTS idx_billing_items_invoice ON billing_invoice_items(invoice_id)`,
		`CREATE TABLE IF NOT EXISTS billing_payments (
			idempotency_key TEXT PRIMARY KEY, owner TEXT NOT NULL DEFAULT '', customer_id TEXT NOT NULL DEFAULT '',
			payment_intent_ref TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT '',
			amount_cents INTEGER NOT NULL DEFAULT 0, currency TEXT NOT NULL DEFAULT '',
			charge_ref_sealed TEXT NOT NULL DEFAULT '', client_secret_sealed TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_billing_payments_customer ON billing_payments(owner, customer_id)`,
		`CREATE TABLE IF NOT EXISTS billing_usage (
			id TEXT PRIMARY KEY, owner TEXT NOT NULL DEFAULT '', customer_id TEXT NOT NULL DEFAULT '',
			product_id TEXT NOT NULL DEFAULT '', quantity INTEGER NOT NULL DEFAULT 0,
			metadata TEXT NOT NULL DEFAULT '{}', recorded_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_billing_usage_customer ON billing_usage(owner, customer_id, recorded_at)`,
		`CREATE TABLE IF NOT EXISTS billing_credits (
			id TEXT PRIMARY KEY, owner TEXT NOT NULL DEFAULT '', customer_id TEXT NOT NULL DEFAULT '',
			amount_cents INTEGER NOT NULL DEFAULT 0, currency TEXT NOT NULL DEFAULT '',
			reason TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL DEFAULT 0, expires_at INTEGER
		)`,
		`CREATE INDEX IF NOT EXISTS idx_billing_credits_customer ON billing_credits(owner, customer_id)`,
	}
	for _, q := range stmts {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// ── codecs ──

func tnanos(t time.Time) int64    { return t.UnixNano() }
func fromNanos(n int64) time.Time { return time.Unix(0, n).UTC() }

func ptrNanos(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixNano()
}

func fromNullNanos(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := fromNanos(n.Int64)
	return &t
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func jsonMap(m map[string]string) string {
	if len(m) == 0 {
		return "{}"
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func readMap(s string) map[string]string {
	if s == "" || s == "{}" {
		return nil
	}
	var m map[string]string
	_ = json.Unmarshal([]byte(s), &m)
	return m
}

// ── Customers ──

const customerCols = `id, owner, user_id, org_id, provider_ref, billing_email, billing_name, currency, tax_id, created_at, updated_at`

func scanCustomer2(sc interface{ Scan(...any) error }) (*Customer, error) {
	var c Customer
	var created, updated int64
	if err := sc.Scan(&c.ID, &c.Owner, &c.UserID, &c.OrgID, &c.ProviderRef, &c.BillingEmail,
		&c.BillingName, &c.Currency, &c.TaxID, &created, &updated); err != nil {
		return nil, err
	}
	c.CreatedAt = fromNanos(created)
	c.UpdatedAt = fromNanos(updated)
	return &c, nil
}

func (s *SQLStore) CreateCustomer(ctx context.Context, c *Customer) error {
	if c.ID == "" {
		c.ID = randomID()
	}
	now := time.Now()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	c.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `INSERT INTO billing_customers (`+customerCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		c.ID, c.Owner, c.UserID, c.OrgID, c.ProviderRef, c.BillingEmail, c.BillingName, c.Currency,
		c.TaxID, tnanos(c.CreatedAt), tnanos(c.UpdatedAt))
	return err
}

func (s *SQLStore) GetCustomer(ctx context.Context, owner, id string) (*Customer, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+customerCols+` FROM billing_customers
		WHERE id = ? AND (? = '' OR owner = ?)`, id, owner, owner)
	c, err := scanCustomer2(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return c, err
}

func (s *SQLStore) GetCustomerByProviderRef(ctx context.Context, providerRef string) (*Customer, error) {
	if providerRef == "" {
		return nil, ErrNotFound
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+customerCols+` FROM billing_customers
		WHERE provider_ref = ? LIMIT 1`, providerRef)
	c, err := scanCustomer2(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return c, err
}

func (s *SQLStore) GetCustomerByUser(ctx context.Context, owner, userID string) (*Customer, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+customerCols+` FROM billing_customers
		WHERE (? = '' OR owner = ?) AND user_id = ? LIMIT 1`, owner, owner, userID)
	c, err := scanCustomer2(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return c, err
}

func (s *SQLStore) SetCustomerProviderRef(ctx context.Context, owner, id, providerRef string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE billing_customers SET provider_ref = ?, updated_at = ?
		WHERE id = ? AND (? = '' OR owner = ?)`, providerRef, time.Now().UnixNano(), id, owner, owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLStore) UpdateCustomer(ctx context.Context, c *Customer) error {
	res, err := s.db.ExecContext(ctx, `UPDATE billing_customers SET user_id=?, org_id=?, provider_ref=?,
		billing_email=?, billing_name=?, currency=?, tax_id=?, updated_at=?
		WHERE id = ? AND (? = '' OR owner = ?)`,
		c.UserID, c.OrgID, c.ProviderRef, c.BillingEmail, c.BillingName, c.Currency, c.TaxID,
		time.Now().UnixNano(), c.ID, c.Owner, c.Owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ── Products & prices ──

func (s *SQLStore) CreateProduct(ctx context.Context, p *Product) error {
	if p.ID == "" {
		p.ID = randomID()
	}
	now := time.Now()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `INSERT INTO billing_products
		(id, owner, service_id, name, description, active, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		p.ID, p.Owner, p.ServiceID, p.Name, p.Description, boolInt(p.Active), tnanos(p.CreatedAt), tnanos(p.UpdatedAt))
	return err
}

func scanProduct(sc interface{ Scan(...any) error }) (*Product, error) {
	var p Product
	var active int
	var created, updated int64
	if err := sc.Scan(&p.ID, &p.Owner, &p.ServiceID, &p.Name, &p.Description, &active, &created, &updated); err != nil {
		return nil, err
	}
	p.Active = active != 0
	p.CreatedAt = fromNanos(created)
	p.UpdatedAt = fromNanos(updated)
	return &p, nil
}

const productCols = `id, owner, service_id, name, description, active, created_at, updated_at`

func (s *SQLStore) GetProduct(ctx context.Context, owner, id string) (*Product, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+productCols+` FROM billing_products
		WHERE id = ? AND (? = '' OR owner = ?)`, id, owner, owner)
	p, err := scanProduct(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return p, err
}

func (s *SQLStore) ListProducts(ctx context.Context, owner string, activeOnly bool) ([]*Product, error) {
	where := `(? = '' OR owner = ?)`
	args := []any{owner, owner}
	if activeOnly {
		where += ` AND active = 1`
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+productCols+` FROM billing_products WHERE `+where+` ORDER BY name ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Product
	for rows.Next() {
		p, err := scanProduct(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

const priceCols = `id, owner, product_id, provider_ref, currency, amount_cents, interval, pricing_model, tier_config, percentage_rate, created_at`

func scanPrice(sc interface{ Scan(...any) error }) (*Price, error) {
	var p Price
	var created int64
	if err := sc.Scan(&p.ID, &p.Owner, &p.ProductID, &p.ProviderRef, &p.Currency, &p.AmountCents,
		&p.Interval, &p.PricingModel, &p.TierConfig, &p.PercentageRate, &created); err != nil {
		return nil, err
	}
	p.CreatedAt = fromNanos(created)
	return &p, nil
}

func (s *SQLStore) CreatePrice(ctx context.Context, p *Price) error {
	if p.ID == "" {
		p.ID = randomID()
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO billing_prices (`+priceCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		p.ID, p.Owner, p.ProductID, p.ProviderRef, p.Currency, p.AmountCents, p.Interval,
		p.PricingModel, p.TierConfig, p.PercentageRate, tnanos(p.CreatedAt))
	return err
}

func (s *SQLStore) GetPrice(ctx context.Context, owner, id string) (*Price, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+priceCols+` FROM billing_prices
		WHERE id = ? AND (? = '' OR owner = ?)`, id, owner, owner)
	p, err := scanPrice(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return p, err
}

func (s *SQLStore) ListPrices(ctx context.Context, owner, productID string) ([]*Price, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+priceCols+` FROM billing_prices
		WHERE (? = '' OR owner = ?) AND product_id = ? ORDER BY amount_cents ASC`, owner, owner, productID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Price
	for rows.Next() {
		p, err := scanPrice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *SQLStore) SetPriceProviderRef(ctx context.Context, owner, id, providerRef string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE billing_prices SET provider_ref = ?
		WHERE id = ? AND (? = '' OR owner = ?)`, providerRef, id, owner, owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ── Subscriptions ──

const subCols = `id, owner, customer_id, price_id, provider_ref, status, quantity,
	current_period_start, current_period_end, cancel_at_period_end, created_at, updated_at, canceled_at`

func scanSub(sc interface{ Scan(...any) error }) (*Subscription, error) {
	var s Subscription
	var cancel int
	var created, updated int64
	var cps, cpe, canceled sql.NullInt64
	if err := sc.Scan(&s.ID, &s.Owner, &s.CustomerID, &s.PriceID, &s.ProviderRef, &s.Status,
		&s.Quantity, &cps, &cpe, &cancel, &created, &updated, &canceled); err != nil {
		return nil, err
	}
	s.CancelAtPeriodEnd = cancel != 0
	s.CurrentPeriodStart = fromNullNanos(cps)
	s.CurrentPeriodEnd = fromNullNanos(cpe)
	s.CreatedAt = fromNanos(created)
	s.UpdatedAt = fromNanos(updated)
	s.CanceledAt = fromNullNanos(canceled)
	return &s, nil
}

func (s *SQLStore) CreateSubscription(ctx context.Context, sub *Subscription) error {
	if sub.ID == "" {
		sub.ID = randomID()
	}
	now := time.Now()
	if sub.CreatedAt.IsZero() {
		sub.CreatedAt = now
	}
	sub.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `INSERT INTO billing_subscriptions (`+subCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		sub.ID, sub.Owner, sub.CustomerID, sub.PriceID, sub.ProviderRef, sub.Status, sub.Quantity,
		ptrNanos(sub.CurrentPeriodStart), ptrNanos(sub.CurrentPeriodEnd), boolInt(sub.CancelAtPeriodEnd),
		tnanos(sub.CreatedAt), tnanos(sub.UpdatedAt), ptrNanos(sub.CanceledAt))
	return err
}

func (s *SQLStore) GetSubscription(ctx context.Context, owner, id string) (*Subscription, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+subCols+` FROM billing_subscriptions
		WHERE id = ? AND (? = '' OR owner = ?)`, id, owner, owner)
	sub, err := scanSub(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return sub, err
}

func (s *SQLStore) ListSubscriptionsByCustomer(ctx context.Context, owner, customerID string) ([]*Subscription, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+subCols+` FROM billing_subscriptions
		WHERE (? = '' OR owner = ?) AND customer_id = ? ORDER BY created_at DESC`, owner, owner, customerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Subscription
	for rows.Next() {
		sub, err := scanSub(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

func (s *SQLStore) UpdateSubscription(ctx context.Context, sub *Subscription) error {
	res, err := s.db.ExecContext(ctx, `UPDATE billing_subscriptions SET price_id=?, provider_ref=?,
		status=?, quantity=?, current_period_start=?, current_period_end=?, cancel_at_period_end=?,
		updated_at=?, canceled_at=? WHERE id = ? AND (? = '' OR owner = ?)`,
		sub.PriceID, sub.ProviderRef, sub.Status, sub.Quantity, ptrNanos(sub.CurrentPeriodStart),
		ptrNanos(sub.CurrentPeriodEnd), boolInt(sub.CancelAtPeriodEnd), time.Now().UnixNano(),
		ptrNanos(sub.CanceledAt), sub.ID, sub.Owner, sub.Owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
