package billing

import (
	"context"
	"errors"
	"time"
)

// CancelResult is what a PaymentProvider answers to CancelSubscription: whether the
// subscription is set to end at its period end, and the instant the provider recorded
// the cancellation, nil when it recorded none. Service.CancelSubscription stores both
// through Store.UpdateSubscriptionCancel. The status is not part of it: the provider
// reports the status that results through its event, and the billing host's webhook
// applier is that column's writer.
type CancelResult struct {
	CancelAtPeriodEnd bool
	CanceledAt        *time.Time
}

// ErrNoProviderSubscription is answered by Service.CancelSubscription and
// Service.UpdateSubscription for a stored subscription carrying no provider
// reference: there is no provider subscription to cancel or update, and a change
// that reaches nothing at the provider is refused rather than written locally.
var ErrNoProviderSubscription = errors.New("billing: subscription has no provider subscription to cancel or update")

// CancelSubscription cancels the partition's subscription at the provider, at once or
// at period end, and then writes the cancel fields the provider answers through
// Store.UpdateSubscriptionCancel; it answers the row as stored after that write. The
// provider is called first so a provider refusal leaves the row unchanged, and the
// store write carries no status: the provider reports the resulting status (canceled
// at once, or the present one until the period ends) through its event, which the
// billing host's webhook applier writes, so a past_due subscription's unpaid clock
// stands until that event. The billing host's CancelSubscription handler is its
// caller.
func (s *Service) CancelSubscription(ctx context.Context, partition, id string, atPeriodEnd bool) (*Subscription, error) {
	sub, err := s.store.GetSubscription(ctx, partition, id)
	if err != nil {
		return nil, err
	}
	if sub.ProviderRef == "" {
		return nil, ErrNoProviderSubscription
	}
	res, err := s.provider.CancelSubscription(ctx, sub.ProviderRef, atPeriodEnd)
	if err != nil {
		return nil, err
	}
	if err := s.store.UpdateSubscriptionCancel(ctx, partition, id, res.CancelAtPeriodEnd, res.CanceledAt); err != nil {
		return nil, err
	}
	return s.store.GetSubscription(ctx, partition, id)
}

// UpdateSubscriptionCancel writes cancel_at_period_end, canceled_at and updated_at for
// one subscription in the partition, and no other column, so a status the provider's
// event wrote and the instants beside it are never put back or moved by a cancel. An
// empty partition matches no row, as in every other selector here.
func (s *SQLStore) UpdateSubscriptionCancel(ctx context.Context, partition, id string, cancelAtPeriodEnd bool, canceledAt *time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE billing_subscriptions SET cancel_at_period_end=?, canceled_at=?, updated_at=?
		WHERE id = ? AND (? <> '' AND partition = ?)`,
		boolInt(cancelAtPeriodEnd), ptrNanos(canceledAt), s.now().UnixNano(), id, partition, partition)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateSubscriptionCancel is the reference behaviour SQLStore's method is held to:
// the two cancel fields change, UpdatedAt moves, every other field stays as stored.
func (m *MemStore) UpdateSubscriptionCancel(_ context.Context, partition, id string, cancelAtPeriodEnd bool, canceledAt *time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.subscriptions[id]
	if !ok || !partitionMatch(partition, e.Partition) {
		return ErrNotFound
	}
	upd := cloneSubscription(e)
	upd.CancelAtPeriodEnd = cancelAtPeriodEnd
	upd.CanceledAt = clonePtr(canceledAt)
	upd.UpdatedAt = m.now()
	m.subscriptions[id] = upd
	return nil
}
