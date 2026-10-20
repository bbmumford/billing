package billing

import (
	"context"
	"time"
)

// UpdateSubscriptionItem writes price_id, quantity and updated_at for one subscription in the
// partition, and no other column, so a status the provider's event wrote is never put back by an
// item change. An empty partition matches no row, as in every other selector here.
func (s *SQLStore) UpdateSubscriptionItem(ctx context.Context, partition, id, priceID string, quantity int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE billing_subscriptions SET price_id=?, quantity=?, updated_at=?
		WHERE id = ? AND (? <> '' AND partition = ?)`,
		priceID, quantity, time.Now().UnixNano(), id, partition, partition)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateSubscriptionItem is the reference behaviour SQLStore's method is held to: price and
// quantity change, UpdatedAt moves, every other field stays as stored.
func (m *MemStore) UpdateSubscriptionItem(_ context.Context, partition, id, priceID string, quantity int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.subscriptions[id]
	if !ok || !partitionMatch(partition, e.Partition) {
		return ErrNotFound
	}
	upd := cloneSubscription(e)
	upd.PriceID = priceID
	upd.Quantity = quantity
	upd.UpdatedAt = m.now()
	m.subscriptions[id] = upd
	return nil
}
