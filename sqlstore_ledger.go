package billing

import (
	"context"
	"database/sql"
	"time"
)

// ── Invoices ──

const invoiceCols = `id, partition, customer_id, subscription_id, provider_ref, number, status, currency,
	amount_due_cents, amount_paid_cents, created_at, due_at, paid_at`

func scanInvoice(sc interface{ Scan(...any) error }) (*Invoice, error) {
	var inv Invoice
	var created int64
	var due, paid sql.NullInt64
	if err := sc.Scan(&inv.ID, &inv.Partition, &inv.CustomerID, &inv.SubscriptionID, &inv.ProviderRef,
		&inv.Number, &inv.Status, &inv.Currency, &inv.AmountDueCents, &inv.AmountPaidCents,
		&created, &due, &paid); err != nil {
		return nil, err
	}
	inv.CreatedAt = fromNanos(created)
	inv.DueAt = fromNullNanos(due)
	inv.PaidAt = fromNullNanos(paid)
	return &inv, nil
}

func (s *SQLStore) CreateInvoice(ctx context.Context, inv *Invoice, items []*InvoiceItem) error {
	if inv.Partition == "" {
		return ErrNoPartition
	}
	if inv.ID == "" {
		inv.ID = randomID()
	}
	if inv.CreatedAt.IsZero() {
		inv.CreatedAt = time.Now()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollbackUnlessCommitted(tx)
	if err := insertedOne(tx.ExecContext(ctx, `INSERT INTO billing_invoices (`+invoiceCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`,
		inv.ID, inv.Partition, inv.CustomerID, inv.SubscriptionID, inv.ProviderRef, inv.Number, inv.Status,
		inv.Currency, inv.AmountDueCents, inv.AmountPaidCents, tnanos(inv.CreatedAt), ptrNanos(inv.DueAt),
		ptrNanos(inv.PaidAt))); err != nil {
		return err
	}
	for _, it := range items {
		if it.ID == "" {
			it.ID = randomID()
		}
		it.InvoiceID = inv.ID
		if _, err := tx.ExecContext(ctx, `INSERT INTO billing_invoice_items
			(id, invoice_id, product_id, description, quantity, unit_amount_cents, amount_cents, metadata)
			VALUES (?,?,?,?,?,?,?,?)`,
			it.ID, it.InvoiceID, it.ProductID, it.Description, it.Quantity, it.UnitAmountCents,
			it.AmountCents, jsonMap(it.Metadata)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *SQLStore) GetInvoice(ctx context.Context, partition, id string) (*Invoice, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+invoiceCols+` FROM billing_invoices
		WHERE id = ? AND (? <> '' AND partition = ?)`, id, partition, partition)
	inv, err := scanInvoice(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return inv, err
}

func (s *SQLStore) ListInvoicesByCustomer(ctx context.Context, partition, customerID string) ([]*Invoice, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+invoiceCols+` FROM billing_invoices
		WHERE (? <> '' AND partition = ?) AND customer_id = ? ORDER BY created_at DESC`, partition, partition, customerID)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	var out []*Invoice
	for rows.Next() {
		inv, err := scanInvoice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

func (s *SQLStore) ListInvoiceItems(ctx context.Context, partition, invoiceID string) ([]*InvoiceItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT it.id, it.invoice_id, it.product_id, it.description, it.quantity,
		it.unit_amount_cents, it.amount_cents, it.metadata FROM billing_invoice_items it
		JOIN billing_invoices i ON i.id = it.invoice_id
		WHERE it.invoice_id = ? AND (? <> '' AND i.partition = ?)`, invoiceID, partition, partition)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	var out []*InvoiceItem
	for rows.Next() {
		var it InvoiceItem
		var meta string
		if err := rows.Scan(&it.ID, &it.InvoiceID, &it.ProductID, &it.Description, &it.Quantity,
			&it.UnitAmountCents, &it.AmountCents, &meta); err != nil {
			return nil, err
		}
		it.Metadata = readMap(meta)
		out = append(out, &it)
	}
	return out, rows.Err()
}

func (s *SQLStore) UpdateInvoiceStatus(ctx context.Context, partition, id, status string, amountPaidCents int64) error {
	set := `status = ?, amount_paid_cents = ?`
	args := []any{status, amountPaidCents}
	if status == "paid" {
		set += `, paid_at = COALESCE(paid_at, ?)`
		args = append(args, time.Now().UnixNano())
	}
	guard, guardArgs := invoiceStatusOrder.guard(status)
	args = append(args, id, partition, partition)
	args = append(args, guardArgs...)
	res, err := s.db.ExecContext(ctx, `UPDATE billing_invoices SET `+set+`
		WHERE id = ? AND (? <> '' AND partition = ?) AND `+guard, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return s.statusRefusal(ctx, `SELECT 1 FROM billing_invoices WHERE id = ? AND (? <> '' AND partition = ?)`,
			id, partition, partition)
	}
	return nil
}

// statusRefusal answers why a status write guarded by a statusOrder changed no row, from a query
// selecting the row the write named: ErrStatusOutOfOrder when the partition holds the row, so the
// guard refused the status, and ErrNotFound when it does not. UpdateInvoiceStatus and
// UpdatePaymentStatus call it, so their refusals match MemStore's.
func (s *SQLStore) statusRefusal(ctx context.Context, query string, args ...any) error {
	var one int
	switch err := s.db.QueryRowContext(ctx, query, args...).Scan(&one); {
	case err == sql.ErrNoRows:
		return ErrNotFound
	case err != nil:
		return err
	}
	return ErrStatusOutOfOrder
}

// ── Payment ledger ──

const paymentCols = `idempotency_key, partition, customer_id, payment_intent_ref, status, amount_cents,
	currency, charge_ref_sealed, client_secret_sealed, created_at, updated_at`

func scanPayment(sc interface{ Scan(...any) error }) (*PaymentLedgerRow, error) {
	var r PaymentLedgerRow
	var created, updated int64
	if err := sc.Scan(&r.IdempotencyKey, &r.Partition, &r.CustomerID, &r.PaymentIntentRef, &r.Status,
		&r.AmountCents, &r.Currency, &r.ChargeRefSealed, &r.ClientSecretSealed, &created, &updated); err != nil {
		return nil, err
	}
	r.CreatedAt = fromNanos(created)
	r.UpdatedAt = fromNanos(updated)
	return &r, nil
}

func (s *SQLStore) RecordPayment(ctx context.Context, row *PaymentLedgerRow) (*PaymentLedgerRow, error) {
	if row.Partition == "" {
		return nil, ErrNoPartition
	}
	if row.IdempotencyKey == "" {
		return nil, ErrIdempotencyKeyRequired
	}
	now := time.Now()
	if row.CreatedAt.IsZero() {
		row.CreatedAt = now
	}
	row.UpdatedAt = now
	// Idempotent insert: on a key the partition already holds, insert nothing and return the existing row
	// so a retry never double-records a payment. The check + insert run in one transaction (driver-agnostic).
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer rollbackUnlessCommitted(tx)
	existing, gerr := scanPayment(tx.QueryRowContext(ctx, `SELECT `+paymentCols+` FROM billing_payments
		WHERE partition = ? AND idempotency_key = ?`, row.Partition, row.IdempotencyKey))
	if gerr == nil {
		return existing, nil
	}
	if gerr != sql.ErrNoRows {
		return nil, gerr
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO billing_payments (`+paymentCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		row.IdempotencyKey, row.Partition, row.CustomerID, row.PaymentIntentRef, row.Status, row.AmountCents,
		row.Currency, row.ChargeRefSealed, row.ClientSecretSealed, tnanos(row.CreatedAt), tnanos(row.UpdatedAt)); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return nil, nil
}

func (s *SQLStore) GetPaymentByKey(ctx context.Context, partition, idempotencyKey string) (*PaymentLedgerRow, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+paymentCols+` FROM billing_payments
		WHERE (? <> '' AND partition = ?) AND idempotency_key = ?`, partition, partition, idempotencyKey)
	r, err := scanPayment(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return r, err
}

func (s *SQLStore) UpdatePaymentStatus(ctx context.Context, partition, idempotencyKey, status, chargeRefSealed string) error {
	set := `status = ?, updated_at = ?`
	args := []any{status, time.Now().UnixNano()}
	if chargeRefSealed != "" {
		set += `, charge_ref_sealed = ?`
		args = append(args, chargeRefSealed)
	}
	guard, guardArgs := paymentStatusOrder.guard(status)
	args = append(args, partition, partition, idempotencyKey)
	args = append(args, guardArgs...)
	res, err := s.db.ExecContext(ctx, `UPDATE billing_payments SET `+set+`
		WHERE (? <> '' AND partition = ?) AND idempotency_key = ? AND `+guard, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return s.statusRefusal(ctx, `SELECT 1 FROM billing_payments WHERE (? <> '' AND partition = ?) AND idempotency_key = ?`,
			partition, partition, idempotencyKey)
	}
	return nil
}

func (s *SQLStore) ListPaymentsByCustomer(ctx context.Context, partition, customerID string) ([]*PaymentLedgerRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+paymentCols+` FROM billing_payments
		WHERE (? <> '' AND partition = ?) AND customer_id = ? ORDER BY created_at DESC`, partition, partition, customerID)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	var out []*PaymentLedgerRow
	for rows.Next() {
		r, err := scanPayment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ── Usage ──

func (s *SQLStore) RecordUsage(ctx context.Context, u *UsageRecord) error {
	if u.Partition == "" {
		return ErrNoPartition
	}
	if u.IdempotencyKey != "" {
		u.ID = usageRecordID(u.Partition, u.IdempotencyKey)
	}
	if u.ID == "" {
		return ErrIdempotencyKeyRequired
	}
	if u.RecordedAt.IsZero() {
		u.RecordedAt = time.Now()
	}
	if u.IdempotencyKey == "" {
		return insertedOne(s.db.ExecContext(ctx, `INSERT INTO billing_usage
			(id, partition, customer_id, product_id, quantity, metadata, recorded_at) VALUES (?,?,?,?,?,?,?)
			ON CONFLICT(id) DO NOTHING`,
			u.ID, u.Partition, u.CustomerID, u.ProductID, u.Quantity, jsonMap(u.Metadata), tnanos(u.RecordedAt)))
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO billing_usage
		(id, partition, customer_id, product_id, quantity, metadata, recorded_at) VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(id) DO NOTHING`,
		u.ID, u.Partition, u.CustomerID, u.ProductID, u.Quantity, jsonMap(u.Metadata), tnanos(u.RecordedAt))
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 1 {
		return err
	}
	// The key's record is already stored: a retry is answered it, another report under the key refused.
	var stored UsageRecord
	var meta string
	var at int64
	if err := s.db.QueryRowContext(ctx, `SELECT customer_id, product_id, quantity, metadata, recorded_at
		FROM billing_usage WHERE id = ? AND partition = ?`, u.ID, u.Partition).Scan(
		&stored.CustomerID, &stored.ProductID, &stored.Quantity, &meta, &at); err != nil {
		return err
	}
	if !sameUsage(&stored, u) {
		return ErrIdempotencyConflict
	}
	u.Metadata, u.RecordedAt = readMap(meta), fromNanos(at)
	return nil
}

func (s *SQLStore) SummarizeUsage(ctx context.Context, partition, customerID string, from, to time.Time) ([]*UsageSummary, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT product_id, SUM(quantity), MAX(quantity)
		FROM billing_usage WHERE (? <> '' AND partition = ?) AND customer_id = ?
		AND recorded_at >= ? AND recorded_at < ? GROUP BY product_id ORDER BY product_id ASC`,
		partition, partition, customerID, from.UnixNano(), to.UnixNano())
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	var out []*UsageSummary
	for rows.Next() {
		var su UsageSummary
		if err := rows.Scan(&su.ProductID, &su.TotalQuantity, &su.PeakQuantity); err != nil {
			return nil, err
		}
		out = append(out, &su)
	}
	return out, rows.Err()
}

// ── Credits ──

func (s *SQLStore) GrantCredit(ctx context.Context, c *Credit) error {
	if c.Partition == "" {
		return ErrNoPartition
	}
	if c.ID == "" {
		c.ID = randomID()
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now()
	}
	return insertedOne(s.db.ExecContext(ctx, `INSERT INTO billing_credits
		(id, partition, customer_id, amount_cents, currency, reason, created_at, expires_at)
		VALUES (?,?,?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`,
		c.ID, c.Partition, c.CustomerID, c.AmountCents, c.Currency, c.Reason, tnanos(c.CreatedAt), ptrNanos(c.ExpiresAt)))
}

func (s *SQLStore) ListCredits(ctx context.Context, partition, customerID string) ([]*Credit, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, partition, customer_id, amount_cents, currency, reason,
		created_at, expires_at FROM billing_credits WHERE (? <> '' AND partition = ?) AND customer_id = ?
		ORDER BY created_at DESC`, partition, partition, customerID)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	var out []*Credit
	for rows.Next() {
		var c Credit
		var created int64
		var expires sql.NullInt64
		if err := rows.Scan(&c.ID, &c.Partition, &c.CustomerID, &c.AmountCents, &c.Currency, &c.Reason,
			&created, &expires); err != nil {
			return nil, err
		}
		c.CreatedAt = fromNanos(created)
		c.ExpiresAt = fromNullNanos(expires)
		out = append(out, &c)
	}
	return out, rows.Err()
}

func (s *SQLStore) CreditBalance(ctx context.Context, partition, customerID string, now time.Time) (map[string]int64, error) {
	// A credit counts unless it has expired (expires_at <= now). NULL expiry never expires.
	rows, err := s.db.QueryContext(ctx, `SELECT currency, SUM(amount_cents) FROM billing_credits
		WHERE (? <> '' AND partition = ?) AND customer_id = ? AND (expires_at IS NULL OR expires_at > ?)
		GROUP BY currency`, partition, partition, customerID, now.UnixNano())
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	bal := map[string]int64{}
	for rows.Next() {
		var cur string
		var sum int64
		if err := rows.Scan(&cur, &sum); err != nil {
			return nil, err
		}
		bal[cur] = sum
	}
	return bal, rows.Err()
}
