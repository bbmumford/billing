package billing

import (
	"context"
	"sort"
	"strings"
	"time"
)

// unpaidStatuses are the provider statuses a subscription is unpaid in: past_due,
// which the provider reports at the first failed payment, and unpaid, which it
// reports once its retries are spent and it keeps the subscription. The clock the
// suspension rule counts runs while the status is either of them; a move between
// the two is the same unpaid run.
var unpaidStatuses = []string{"past_due", "unpaid"}

func unpaidStatus(status string) bool {
	for _, s := range unpaidStatuses {
		if s == status {
			return true
		}
	}
	return false
}

// unpaidStatusSQL is unpaidStatuses as a SQL value list, spliced into SQLStore's
// UpdateSubscription and ListUnpaidSince so the SQL and memory stores read one set.
// The values are the constants above, never caller input.
var unpaidStatusSQL = func() string {
	quoted := make([]string, len(unpaidStatuses))
	for i, s := range unpaidStatuses {
		quoted[i] = "'" + s + "'"
	}
	return strings.Join(quoted, ",")
}()

// UnpaidSince answers the instant the subscription entered the unpaid statuses and
// true when its status is one of them; the zero time and false otherwise.
// ListUnpaidSince selects by it. A status in the set with no stored instant answers
// false, so a row no store write produced is never read as unpaid.
func (s *Subscription) UnpaidSince() (time.Time, bool) {
	if s == nil || !unpaidStatus(s.Status) || s.EnteredUnpaidAt == nil {
		return time.Time{}, false
	}
	return *s.EnteredUnpaidAt, true
}

// statusClock answers the StatusChangedAt and EnteredUnpaidAt a write of status at
// now leaves on a stored row: StatusChangedAt moves only when status differs from
// the stored one; EnteredUnpaidAt starts on entering the unpaid statuses, keeps the
// stored instant while the row stays in them, and clears on leaving them. A nil
// stored row is the creation write. MemStore applies it; SQLStore's
// UpdateSubscription states the same rule as SQL over the stored columns, so the
// read of the stored row and the write are one statement.
func statusClock(stored *Subscription, status string, now time.Time) (time.Time, *time.Time) {
	changed := now
	if stored != nil && stored.Status == status {
		changed = stored.StatusChangedAt
	}
	if !unpaidStatus(status) {
		return changed, nil
	}
	if stored != nil && unpaidStatus(stored.Status) && stored.EnteredUnpaidAt != nil {
		return changed, clonePtr(stored.EnteredUnpaidAt)
	}
	entered := now
	return changed, &entered
}

// UnpaidReader is the one billing read that crosses partitions: every partition's
// subscriptions whose unpaid clock (UnpaidSince) is at or before before, oldest
// clock first, a row held under the empty partition left out. SQLStore and MemStore
// carry it. It is kept off Store: the billing host holds a Store and serves every
// RPC operation through it, so no RPC operation reaches this read; only code that
// holds the concrete store, beside the billing database, reaches it.
//
// TODO: the suspension rule's sweep is its reader; it is not composed yet.
type UnpaidReader interface {
	ListUnpaidSince(ctx context.Context, before time.Time) ([]*Subscription, error)
}

var (
	_ UnpaidReader = (*SQLStore)(nil)
	_ UnpaidReader = (*MemStore)(nil)
)

// ListUnpaidSince answers every partition's subscriptions whose unpaid clock is at
// or before before; see UnpaidReader.
func (s *SQLStore) ListUnpaidSince(ctx context.Context, before time.Time) ([]*Subscription, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+subCols+` FROM billing_subscriptions
		WHERE partition <> '' AND status IN (`+unpaidStatusSQL+`)
		AND unpaid_since IS NOT NULL AND unpaid_since <= ?
		ORDER BY unpaid_since ASC, id ASC`, before.UnixNano())
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
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

// ListUnpaidSince is the reference behaviour SQLStore's read is held to.
func (m *MemStore) ListUnpaidSince(_ context.Context, before time.Time) ([]*Subscription, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Subscription
	for _, s := range m.subscriptions {
		if s.Partition == "" {
			continue
		}
		if since, ok := s.UnpaidSince(); ok && !since.After(before) {
			out = append(out, cloneSubscription(s))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].EnteredUnpaidAt.Equal(*out[j].EnteredUnpaidAt) {
			return out[i].EnteredUnpaidAt.Before(*out[j].EnteredUnpaidAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}
