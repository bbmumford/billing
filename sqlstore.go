package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// SQLStore is the durable default Store over any database/sql driver — the shipped standalone store. The
// package imports only database/sql (stdlib); the APP supplies the opened *sql.DB and registers the
// driver, so billing pulls no driver dependency yet works out of the box. Partition is an opaque column the
// store filters on but never interprets; an empty partition matches nothing, because every
// partition-scoped predicate rejects an empty parameter before comparing. Money is INTEGER minor units; times
// persist as INTEGER Unix nanos with NULL for an unset *time.Time; metadata as JSON. Only sealed payment
// material reaches the ledger table. The Store interface stays the escape-hatch for a mesh gateway or
// MemStore.
type SQLStore struct {
	db *sql.DB
	// now is the clock CreateSubscription and UpdateSubscription stamp a
	// subscription's status instants with; NewSQLStore sets time.Now, and the
	// package's tests set a fixed clock so both stores write the same instants.
	now func() time.Time
}

// NewSQLStore builds a store over an opened *sql.DB. Call InitSchema once before use.
func NewSQLStore(db *sql.DB) *SQLStore { return &SQLStore{db: db, now: time.Now} }

var _ Store = (*SQLStore)(nil)

// InitSchema creates every table + index (idempotent); run once at boot.
func (s *SQLStore) InitSchema(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS billing_customers (
			id TEXT PRIMARY KEY, partition TEXT NOT NULL DEFAULT '', user_id TEXT NOT NULL DEFAULT '',
			provider_ref TEXT NOT NULL DEFAULT '',
			billing_email TEXT NOT NULL DEFAULT '', billing_name TEXT NOT NULL DEFAULT '',
			currency TEXT NOT NULL DEFAULT '', tax_id TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL DEFAULT 0,
			UNIQUE(partition, user_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_billing_customers_partition ON billing_customers(partition, user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_billing_customers_provref ON billing_customers(provider_ref)`,
		`CREATE TABLE IF NOT EXISTS billing_products (
			id TEXT PRIMARY KEY, partition TEXT NOT NULL DEFAULT '', service_id TEXT NOT NULL DEFAULT '',
			name TEXT NOT NULL DEFAULT '', description TEXT NOT NULL DEFAULT '',
			active INTEGER NOT NULL DEFAULT 0, metadata TEXT NOT NULL DEFAULT '{}',
			created_at INTEGER NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_billing_products_partition ON billing_products(partition)`,
		`CREATE TABLE IF NOT EXISTS billing_prices (
			id TEXT PRIMARY KEY, partition TEXT NOT NULL DEFAULT '', product_id TEXT NOT NULL DEFAULT '',
			provider_ref TEXT NOT NULL DEFAULT '', currency TEXT NOT NULL DEFAULT '',
			amount_cents INTEGER NOT NULL DEFAULT 0, interval TEXT NOT NULL DEFAULT '',
			pricing_model TEXT NOT NULL DEFAULT '', tier_config TEXT NOT NULL DEFAULT '',
			percentage_rate REAL NOT NULL DEFAULT 0, metadata TEXT NOT NULL DEFAULT '{}',
			created_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_billing_prices_product ON billing_prices(partition, product_id)`,
		`CREATE TABLE IF NOT EXISTS billing_subscriptions (
			id TEXT PRIMARY KEY, partition TEXT NOT NULL DEFAULT '', customer_id TEXT NOT NULL DEFAULT '',
			price_id TEXT NOT NULL DEFAULT '', provider_ref TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT '', quantity INTEGER NOT NULL DEFAULT 0,
			current_period_start INTEGER, current_period_end INTEGER,
			cancel_at_period_end INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL DEFAULT 0, canceled_at INTEGER,
			status_changed_at INTEGER NOT NULL DEFAULT 0, unpaid_since INTEGER,
			event_created_at INTEGER
		)`,
		`CREATE INDEX IF NOT EXISTS idx_billing_subs_customer ON billing_subscriptions(partition, customer_id)`,
		`CREATE INDEX IF NOT EXISTS idx_billing_subs_unpaid ON billing_subscriptions(unpaid_since)`,
		subscriptionItemCreateSQL,
		`CREATE TABLE IF NOT EXISTS billing_invoices (
			id TEXT PRIMARY KEY, partition TEXT NOT NULL DEFAULT '', customer_id TEXT NOT NULL DEFAULT '',
			subscription_id TEXT NOT NULL DEFAULT '', provider_ref TEXT NOT NULL DEFAULT '',
			number TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT '', currency TEXT NOT NULL DEFAULT '',
			amount_due_cents INTEGER NOT NULL DEFAULT 0, amount_paid_cents INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL DEFAULT 0, due_at INTEGER, paid_at INTEGER
		)`,
		`CREATE INDEX IF NOT EXISTS idx_billing_invoices_customer ON billing_invoices(partition, customer_id)`,
		`CREATE TABLE IF NOT EXISTS billing_invoice_items (
			id TEXT PRIMARY KEY, invoice_id TEXT NOT NULL DEFAULT '', product_id TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT '', quantity INTEGER NOT NULL DEFAULT 0,
			unit_amount_cents INTEGER NOT NULL DEFAULT 0, amount_cents INTEGER NOT NULL DEFAULT 0,
			metadata TEXT NOT NULL DEFAULT '{}'
		)`,
		`CREATE INDEX IF NOT EXISTS idx_billing_items_invoice ON billing_invoice_items(invoice_id)`,
		`CREATE TABLE IF NOT EXISTS billing_payments (
			idempotency_key TEXT PRIMARY KEY, partition TEXT NOT NULL DEFAULT '', customer_id TEXT NOT NULL DEFAULT '',
			payment_intent_ref TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT '',
			amount_cents INTEGER NOT NULL DEFAULT 0, currency TEXT NOT NULL DEFAULT '',
			charge_ref_sealed TEXT NOT NULL DEFAULT '', client_secret_sealed TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_billing_payments_customer ON billing_payments(partition, customer_id)`,
		`CREATE TABLE IF NOT EXISTS billing_usage (
			id TEXT PRIMARY KEY, partition TEXT NOT NULL DEFAULT '', customer_id TEXT NOT NULL DEFAULT '',
			product_id TEXT NOT NULL DEFAULT '', quantity INTEGER NOT NULL DEFAULT 0,
			metadata TEXT NOT NULL DEFAULT '{}', recorded_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_billing_usage_customer ON billing_usage(partition, customer_id, recorded_at)`,
		`CREATE TABLE IF NOT EXISTS billing_credits (
			id TEXT PRIMARY KEY, partition TEXT NOT NULL DEFAULT '', customer_id TEXT NOT NULL DEFAULT '',
			amount_cents INTEGER NOT NULL DEFAULT 0, currency TEXT NOT NULL DEFAULT '',
			reason TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL DEFAULT 0, expires_at INTEGER
		)`,
		`CREATE INDEX IF NOT EXISTS idx_billing_credits_customer ON billing_credits(partition, customer_id)`,
		`CREATE TABLE IF NOT EXISTS billing_payment_methods (
			id TEXT PRIMARY KEY, partition TEXT NOT NULL DEFAULT '', customer_id TEXT NOT NULL DEFAULT '',
			type TEXT NOT NULL DEFAULT '', is_default INTEGER NOT NULL DEFAULT 0,
			provider_ref TEXT NOT NULL DEFAULT '', card_brand TEXT NOT NULL DEFAULT '',
			card_last4 TEXT NOT NULL DEFAULT '', card_exp_month INTEGER NOT NULL DEFAULT 0,
			card_exp_year INTEGER NOT NULL DEFAULT 0, crypto_currency TEXT NOT NULL DEFAULT '',
			active INTEGER NOT NULL DEFAULT 1, created_at INTEGER NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_billing_pm_customer ON billing_payment_methods(partition, customer_id)`,
		`CREATE TABLE IF NOT EXISTS billing_discounts (
			discount_id TEXT PRIMARY KEY, partition TEXT NOT NULL DEFAULT '', code TEXT NOT NULL DEFAULT '',
			name TEXT NOT NULL DEFAULT '', type TEXT NOT NULL DEFAULT '', value REAL NOT NULL DEFAULT 0,
			applies_to TEXT NOT NULL DEFAULT '', max_uses INTEGER NOT NULL DEFAULT 0,
			current_uses INTEGER NOT NULL DEFAULT 0, active INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS billing_applied_discounts (
			id TEXT PRIMARY KEY, partition TEXT NOT NULL DEFAULT '', subscription_ref TEXT NOT NULL DEFAULT '',
			discount_id TEXT NOT NULL DEFAULT '', applied_at INTEGER NOT NULL DEFAULT 0,
			UNIQUE(partition, subscription_ref, discount_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_billing_applied_disc ON billing_applied_discounts(partition, subscription_ref, discount_id)`,
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

const customerCols = `id, partition, user_id, provider_ref, billing_email, billing_name, currency, tax_id, created_at, updated_at`

func scanCustomer2(sc interface{ Scan(...any) error }) (*Customer, error) {
	var c Customer
	var created, updated int64
	if err := sc.Scan(&c.ID, &c.Partition, &c.UserID, &c.ProviderRef, &c.BillingEmail,
		&c.BillingName, &c.Currency, &c.TaxID, &created, &updated); err != nil {
		return nil, err
	}
	c.CreatedAt = fromNanos(created)
	c.UpdatedAt = fromNanos(updated)
	return &c, nil
}

// CreateCustomer stores c as the partition's customer for its subject (user_id) and
// answers ErrCustomerExists, writing nothing, when the partition already holds one
// for the pair. The insert is conditioned on the pair's absence in the one
// statement, so two creates racing for the pair cannot both insert, and the fresh
// schema's UNIQUE(partition, user_id) holds a row written past this method to the
// same rule. The id and the instants are assigned to c only once the row is
// stored. Service.EnsureCustomer is its caller and reads ErrCustomerExists as the
// concurrent caller's row to answer.
func (s *SQLStore) CreateCustomer(ctx context.Context, c *Customer) error {
	id := c.ID
	if id == "" {
		id = randomID()
	}
	now := time.Now()
	created := c.CreatedAt
	if created.IsZero() {
		created = now
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO billing_customers (`+customerCols+`)
		SELECT ?,?,?,?,?,?,?,?,?,?
		WHERE NOT EXISTS (SELECT 1 FROM billing_customers WHERE partition = ? AND user_id = ?)`,
		id, c.Partition, c.UserID, c.ProviderRef, c.BillingEmail, c.BillingName, c.Currency,
		c.TaxID, tnanos(created), tnanos(now), c.Partition, c.UserID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrCustomerExists
	}
	c.ID, c.CreatedAt, c.UpdatedAt = id, created, now
	return nil
}

func (s *SQLStore) GetCustomer(ctx context.Context, partition, id string) (*Customer, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+customerCols+` FROM billing_customers
		WHERE id = ? AND (? <> '' AND partition = ?)`, id, partition, partition)
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

func (s *SQLStore) GetCustomerByUser(ctx context.Context, partition, userID string) (*Customer, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+customerCols+` FROM billing_customers
		WHERE (? <> '' AND partition = ?) AND user_id = ? LIMIT 1`, partition, partition, userID)
	c, err := scanCustomer2(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return c, err
}

func (s *SQLStore) SetCustomerProviderRef(ctx context.Context, partition, id, providerRef string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE billing_customers SET provider_ref = ?, updated_at = ?
		WHERE id = ? AND (? <> '' AND partition = ?)`, providerRef, time.Now().UnixNano(), id, partition, partition)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLStore) UpdateCustomer(ctx context.Context, c *Customer) error {
	res, err := s.db.ExecContext(ctx, `UPDATE billing_customers SET user_id=?, provider_ref=?,
		billing_email=?, billing_name=?, currency=?, tax_id=?, updated_at=?
		WHERE id = ? AND (? <> '' AND partition = ?)`,
		c.UserID, c.ProviderRef, c.BillingEmail, c.BillingName, c.Currency, c.TaxID,
		time.Now().UnixNano(), c.ID, c.Partition, c.Partition)
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
	_, err := s.db.ExecContext(ctx, `INSERT INTO billing_products (`+productCols+`)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		p.ID, p.Partition, p.ServiceID, p.Name, p.Description, boolInt(p.Active), jsonMap(p.Metadata),
		tnanos(p.CreatedAt), tnanos(p.UpdatedAt))
	return err
}

func scanProduct(sc interface{ Scan(...any) error }) (*Product, error) {
	var p Product
	var active int
	var meta string
	var created, updated int64
	if err := sc.Scan(&p.ID, &p.Partition, &p.ServiceID, &p.Name, &p.Description, &active, &meta, &created, &updated); err != nil {
		return nil, err
	}
	p.Active = active != 0
	p.Metadata = readMap(meta)
	p.CreatedAt = fromNanos(created)
	p.UpdatedAt = fromNanos(updated)
	return &p, nil
}

const productCols = `id, partition, service_id, name, description, active, metadata, created_at, updated_at`

func (s *SQLStore) GetProduct(ctx context.Context, partition, id string) (*Product, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+productCols+` FROM billing_products
		WHERE id = ? AND (? <> '' AND partition = ?)`, id, partition, partition)
	p, err := scanProduct(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return p, err
}

func (s *SQLStore) ListProducts(ctx context.Context, partition string, activeOnly bool) ([]*Product, error) {
	where := `(? <> '' AND partition = ?)`
	args := []any{partition, partition}
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

const priceCols = `id, partition, product_id, provider_ref, currency, amount_cents, interval, pricing_model, tier_config, percentage_rate, metadata, created_at`

func scanPrice(sc interface{ Scan(...any) error }) (*Price, error) {
	var p Price
	var meta string
	var created int64
	if err := sc.Scan(&p.ID, &p.Partition, &p.ProductID, &p.ProviderRef, &p.Currency, &p.AmountCents,
		&p.Interval, &p.PricingModel, &p.TierConfig, &p.PercentageRate, &meta, &created); err != nil {
		return nil, err
	}
	p.Metadata = readMap(meta)
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
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.ID, p.Partition, p.ProductID, p.ProviderRef, p.Currency, p.AmountCents, p.Interval,
		p.PricingModel, p.TierConfig, p.PercentageRate, jsonMap(p.Metadata), tnanos(p.CreatedAt))
	return err
}

func (s *SQLStore) GetPrice(ctx context.Context, partition, id string) (*Price, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+priceCols+` FROM billing_prices
		WHERE id = ? AND (? <> '' AND partition = ?)`, id, partition, partition)
	p, err := scanPrice(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return p, err
}

func (s *SQLStore) ListPrices(ctx context.Context, partition, productID string) ([]*Price, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+priceCols+` FROM billing_prices
		WHERE (? <> '' AND partition = ?) AND product_id = ? ORDER BY amount_cents ASC`, partition, partition, productID)
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

func (s *SQLStore) SetPriceProviderRef(ctx context.Context, partition, id, providerRef string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE billing_prices SET provider_ref = ?
		WHERE id = ? AND (? <> '' AND partition = ?)`, providerRef, id, partition, partition)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ── Subscriptions ──

const subCols = `id, partition, customer_id, price_id, provider_ref, status, quantity,
	current_period_start, current_period_end, cancel_at_period_end, created_at, updated_at, canceled_at,
	status_changed_at, unpaid_since, event_created_at`

func scanSub(sc interface{ Scan(...any) error }) (*Subscription, error) {
	var s Subscription
	var cancel int
	var created, updated, statusChanged int64
	var cps, cpe, canceled, unpaidSince, eventCreated sql.NullInt64
	if err := sc.Scan(&s.ID, &s.Partition, &s.CustomerID, &s.PriceID, &s.ProviderRef, &s.Status,
		&s.Quantity, &cps, &cpe, &cancel, &created, &updated, &canceled, &statusChanged, &unpaidSince,
		&eventCreated); err != nil {
		return nil, err
	}
	s.StatusChangedAt = fromNanos(statusChanged)
	s.EnteredUnpaidAt = fromNullNanos(unpaidSince)
	s.EventCreatedAt = fromNullNanos(eventCreated)
	s.CancelAtPeriodEnd = cancel != 0
	s.CurrentPeriodStart = fromNullNanos(cps)
	s.CurrentPeriodEnd = fromNullNanos(cpe)
	s.CreatedAt = fromNanos(created)
	s.UpdatedAt = fromNanos(updated)
	s.CanceledAt = fromNullNanos(canceled)
	return &s, nil
}

// CreateSubscription inserts the row and, when it names a price, that price and
// quantity as the subscription's one item row, in the same transaction, so the
// item table holds every subscription's items from creation and
// ListSubscriptionItems answers a created subscription's item as it answers an
// updated one's.
func (s *SQLStore) CreateSubscription(ctx context.Context, sub *Subscription) error {
	var items []SubscriptionItem
	if sub.PriceID != "" {
		items = []SubscriptionItem{{PriceID: sub.PriceID, Quantity: sub.Quantity}}
	}
	return s.insertSubscription(ctx, sub, items)
}

// insertSubscription inserts the row, its id, instants and status clock
// assigned here, and items as its item rows, in one transaction.
// CreateSubscription hands it the row's own price as the one item;
// CreateSubscriptionWithItems hands it every item the provider answered.
func (s *SQLStore) insertSubscription(ctx context.Context, sub *Subscription, items []SubscriptionItem) error {
	if sub.ID == "" {
		sub.ID = randomID()
	}
	now := s.now()
	if sub.CreatedAt.IsZero() {
		sub.CreatedAt = now
	}
	sub.UpdatedAt = now
	sub.StatusChangedAt, sub.EnteredUnpaidAt = statusClock(nil, sub.Status, now)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO billing_subscriptions (`+subCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		sub.ID, sub.Partition, sub.CustomerID, sub.PriceID, sub.ProviderRef, sub.Status, sub.Quantity,
		ptrNanos(sub.CurrentPeriodStart), ptrNanos(sub.CurrentPeriodEnd), boolInt(sub.CancelAtPeriodEnd),
		tnanos(sub.CreatedAt), tnanos(sub.UpdatedAt), ptrNanos(sub.CanceledAt),
		tnanos(sub.StatusChangedAt), ptrNanos(sub.EnteredUnpaidAt), ptrNanos(sub.EventCreatedAt)); err != nil {
		return err
	}
	if err := insertSubscriptionItems(ctx, tx, sub.ID, items, now.UnixNano()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLStore) GetSubscription(ctx context.Context, partition, id string) (*Subscription, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+subCols+` FROM billing_subscriptions
		WHERE id = ? AND (? <> '' AND partition = ?)`, id, partition, partition)
	sub, err := scanSub(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return sub, err
}

func (s *SQLStore) ListSubscriptionsByCustomer(ctx context.Context, partition, customerID string) ([]*Subscription, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+subCols+` FROM billing_subscriptions
		WHERE (? <> '' AND partition = ?) AND customer_id = ? ORDER BY created_at DESC`, partition, partition, customerID)
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

// UpdateSubscription writes the row and moves its status instants by statusClock's
// rule, stated here as SQL over the stored columns: every right-hand side of a SET
// reads the row as stored before this statement, so the stored status is compared
// and the stored unpaid_since carried in the same statement that writes the new
// status, and an event landing between a caller's read and this write is what the
// comparison sees. The caller's StatusChangedAt and EnteredUnpaidAt are not read.
//
// The event instant is held the same way: event_created_at takes the caller's
// instant and keeps the stored one on a nil caller instant, and the WHERE admits
// the row only while the stored instant is null or at or before the caller's, so
// an older event delivered between the caller's read and this write finds the
// newer instant and writes nothing. A row the WHERE refused is then read once more
// to tell the absent row (ErrNotFound) from the refused instant (ErrStaleEvent).
func (s *SQLStore) UpdateSubscription(ctx context.Context, sub *Subscription) error {
	now := s.now().UnixNano()
	eventAt := ptrNanos(sub.EventCreatedAt)
	res, err := s.db.ExecContext(ctx, `UPDATE billing_subscriptions SET price_id=?, provider_ref=?,
		status_changed_at = CASE WHEN status = ? THEN status_changed_at ELSE ? END,
		unpaid_since = CASE
			WHEN ? NOT IN (`+unpaidStatusSQL+`) THEN NULL
			WHEN status IN (`+unpaidStatusSQL+`) AND unpaid_since IS NOT NULL THEN unpaid_since
			ELSE ? END,
		event_created_at = COALESCE(?, event_created_at),
		status=?, quantity=?, current_period_start=?, current_period_end=?, cancel_at_period_end=?,
		updated_at=?, canceled_at=? WHERE id = ? AND (? <> '' AND partition = ?)
		AND (? IS NULL OR event_created_at IS NULL OR event_created_at <= ?)`,
		sub.PriceID, sub.ProviderRef,
		sub.Status, now,
		sub.Status, now,
		eventAt,
		sub.Status, sub.Quantity, ptrNanos(sub.CurrentPeriodStart),
		ptrNanos(sub.CurrentPeriodEnd), boolInt(sub.CancelAtPeriodEnd), now,
		ptrNanos(sub.CanceledAt), sub.ID, sub.Partition, sub.Partition,
		eventAt, eventAt)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if eventAt == nil {
			return ErrNotFound
		}
		if _, err := s.GetSubscription(ctx, sub.Partition, sub.ID); err != nil {
			return err
		}
		return ErrStaleEvent
	}
	return nil
}

// ── payment methods ──

const pmCols = `id, partition, customer_id, type, is_default, provider_ref, card_brand,
	card_last4, card_exp_month, card_exp_year, crypto_currency, active, created_at, updated_at`

func scanPM(sc interface{ Scan(...any) error }) (*PaymentMethod, error) {
	var p PaymentMethod
	var isDefault, active int
	var created, updated int64
	if err := sc.Scan(&p.ID, &p.Partition, &p.CustomerID, &p.Type, &isDefault, &p.ProviderRef,
		&p.CardBrand, &p.CardLast4, &p.CardExpMonth, &p.CardExpYear, &p.CryptoCurrency, &active,
		&created, &updated); err != nil {
		return nil, err
	}
	p.IsDefault = isDefault != 0
	p.Active = active != 0
	p.CreatedAt = fromNanos(created)
	p.UpdatedAt = fromNanos(updated)
	return &p, nil
}

func (s *SQLStore) AddPaymentMethod(ctx context.Context, pm *PaymentMethod) error {
	if pm.ID == "" {
		pm.ID = randomID()
	}
	now := time.Now()
	if pm.CreatedAt.IsZero() {
		pm.CreatedAt = now
	}
	pm.UpdatedAt = now
	pm.Active = true
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// One default per customer: clear the others before this method claims it.
	if pm.IsDefault {
		if _, err := tx.ExecContext(ctx, `UPDATE billing_payment_methods SET is_default=0, updated_at=?
			WHERE (? <> '' AND partition = ?) AND customer_id = ? AND active = 1`,
			now.UnixNano(), pm.Partition, pm.Partition, pm.CustomerID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO billing_payment_methods (`+pmCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		pm.ID, pm.Partition, pm.CustomerID, pm.Type, boolInt(pm.IsDefault), pm.ProviderRef,
		pm.CardBrand, pm.CardLast4, pm.CardExpMonth, pm.CardExpYear, pm.CryptoCurrency,
		boolInt(pm.Active), tnanos(pm.CreatedAt), tnanos(pm.UpdatedAt)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLStore) ListPaymentMethods(ctx context.Context, partition, customerID string) ([]*PaymentMethod, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+pmCols+` FROM billing_payment_methods
		WHERE (? <> '' AND partition = ?) AND customer_id = ? AND active = 1 ORDER BY created_at DESC`,
		partition, partition, customerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PaymentMethod
	for rows.Next() {
		pm, err := scanPM(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, pm)
	}
	return out, rows.Err()
}

func (s *SQLStore) GetPaymentMethod(ctx context.Context, partition, id string) (*PaymentMethod, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+pmCols+` FROM billing_payment_methods
		WHERE id = ? AND (? <> '' AND partition = ?)`, id, partition, partition)
	pm, err := scanPM(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return pm, err
}

func (s *SQLStore) SetDefaultPaymentMethod(ctx context.Context, partition, customerID, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UnixNano()
	if _, err := tx.ExecContext(ctx, `UPDATE billing_payment_methods SET is_default=0, updated_at=?
		WHERE (? <> '' AND partition = ?) AND customer_id = ? AND active = 1`,
		now, partition, partition, customerID); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE billing_payment_methods SET is_default=1, updated_at=?
		WHERE id = ? AND (? <> '' AND partition = ?) AND customer_id = ? AND active = 1`,
		now, id, partition, partition, customerID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

func (s *SQLStore) RemovePaymentMethod(ctx context.Context, partition, id string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE billing_payment_methods SET active=0, is_default=0, updated_at=?
		WHERE id = ? AND (? <> '' AND partition = ?)`, time.Now().UnixNano(), id, partition, partition)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ── discounts ──

func (s *SQLStore) UpsertDiscount(ctx context.Context, d *Discount) error {
	if d.DiscountID == "" {
		d.DiscountID = randomID()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO billing_discounts
		(discount_id, partition, code, name, type, value, applies_to, max_uses, current_uses, active)
		VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(discount_id) DO UPDATE SET partition=excluded.partition, code=excluded.code,
		name=excluded.name, type=excluded.type, value=excluded.value, applies_to=excluded.applies_to,
		max_uses=excluded.max_uses, active=excluded.active`,
		d.DiscountID, d.Partition, d.Code, d.Name, d.Type, d.Value, d.AppliesTo, d.MaxUses, d.CurrentUses, boolInt(d.Active))
	return err
}

func (s *SQLStore) GetDiscount(ctx context.Context, partition, discountID string) (*Discount, error) {
	row := s.db.QueryRowContext(ctx, `SELECT discount_id, partition, code, name, type, value,
		applies_to, max_uses, current_uses, active FROM billing_discounts
		WHERE discount_id = ? AND (? <> '' AND partition = ?)`, discountID, partition, partition)
	var d Discount
	var active int
	if err := row.Scan(&d.DiscountID, &d.Partition, &d.Code, &d.Name, &d.Type, &d.Value,
		&d.AppliesTo, &d.MaxUses, &d.CurrentUses, &active); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrDiscountNotFound
		}
		return nil, err
	}
	d.Active = active != 0
	return &d, nil
}

func (s *SQLStore) IsDiscountApplied(ctx context.Context, partition, subscriptionRef, discountID string) (bool, error) {
	if subscriptionRef == "" || discountID == "" {
		return false, nil
	}
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM billing_applied_discounts
		WHERE (? <> '' AND partition = ?) AND subscription_ref = ? AND discount_id = ? LIMIT 1`,
		partition, partition, subscriptionRef, discountID).Scan(&id)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *SQLStore) RecordAppliedDiscount(ctx context.Context, partition, subscriptionRef, discountID string) (*AppliedDiscount, error) {
	if subscriptionRef == "" || discountID == "" {
		return nil, ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT id FROM billing_applied_discounts
		WHERE (? <> '' AND partition = ?) AND subscription_ref = ? AND discount_id = ? LIMIT 1`,
		partition, partition, subscriptionRef, discountID).Scan(&existing)
	if err == nil {
		return nil, ErrDiscountAlreadyApplied
	}
	if err != sql.ErrNoRows {
		return nil, err
	}
	id := randomID()
	if _, err := tx.ExecContext(ctx, `INSERT INTO billing_applied_discounts (id, partition, subscription_ref, discount_id, applied_at)
		VALUES (?,?,?,?,?)`, id, partition, subscriptionRef, discountID, time.Now().UnixNano()); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE billing_discounts SET current_uses = current_uses + 1
		WHERE discount_id = ? AND (? <> '' AND partition = ?)`, discountID, partition, partition); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &AppliedDiscount{ID: id, Partition: partition, SubscriptionRef: subscriptionRef, DiscountID: discountID}, nil
}
