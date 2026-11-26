package billing

import (
	"context"
	"database/sql"
	"errors"
)

// SubscriptionItem is one line of a subscription: a catalog price and the
// quantity sold at it. PriceID is this package's price id (Price.ID); PriceRef
// is the provider's id for the same price (Price.ProviderRef). The provider seam
// (SubscriptionUpdater) reads and answers PriceRef and Quantity; the store
// (Store.UpdateSubscriptionItems, Store.ListSubscriptionItems) holds PriceID and
// Quantity, because the provider reference of a price is the catalog row's
// (billing_prices.provider_ref) and is not stored twice. Service.UpdateSubscription
// carries an item between the two: it fills PriceRef from the catalog before the
// provider is asked and PriceID from the catalog after it has answered.
type SubscriptionItem struct {
	PriceID  string
	PriceRef string
	Quantity int64
}

// SubscriptionUpdater is an OPTIONAL PaymentProvider extension that sets a
// provider subscription's items to the list given, with or without proration,
// and answers the subscription as the provider holds it afterwards, every item
// in SubscriptionResult.Items. Service.UpdateSubscription type-asserts on it and
// answers ErrProviderUnsupported when the wired provider does not implement it,
// so a deployment without a provider-side update fails closed rather than
// writing a price the provider never confirmed; the core PaymentProvider stays
// unchanged.
type SubscriptionUpdater interface {
	UpdateSubscription(ctx context.Context, providerRef string, items []SubscriptionItem, prorate bool) (SubscriptionResult, error)
}

// ErrNoSubscriptionItems is answered by Service.SubscribeItems,
// Service.UpdateSubscription, Store.CreateSubscriptionWithItems and
// Store.UpdateSubscriptionItems for an empty item list: the list is the
// subscription's whole item set, and a subscription with no item has no price
// for its row to carry.
var ErrNoSubscriptionItems = errors.New("billing: a subscription create or update names no items")

// ErrInvalidSubscriptionItem is answered by Service.SubscribeItems and
// Service.UpdateSubscription before the provider is asked, and by
// Store.CreateSubscriptionWithItems before it writes, for an item with no price
// id, a quantity below one, or a price the list names twice: the provider would
// refuse each, and the item rows are keyed by (subscription, price).
var ErrInvalidSubscriptionItem = errors.New("billing: subscription item needs a price id, a quantity of at least one, and a price named once")

// ErrNoProviderPrice is answered by Service.SubscribeItems and
// Service.UpdateSubscription for a catalog price carrying no provider price
// reference: the provider cannot sell it, and a price that cannot be set at the
// provider is not written locally.
var ErrNoProviderPrice = errors.New("billing: price has no provider price to sell")

// ErrProviderItemsUnresolved is answered by Service.UpdateSubscription when the
// provider's answer cannot be stored as it stands: it names no items, or an item
// whose provider price is not the provider reference of any catalog price in the
// partition; and by Service.SubscribeItems for an answer naming a provider price
// the create did not ask for. Nothing is written, so the local rows never carry
// an item the catalog cannot name.
// TODO: the provider has applied the update by then; the subscription's items at
// the provider and the local rows differ until a caller repeats the update with
// the catalog holding the price.
var ErrProviderItemsUnresolved = errors.New("billing: the provider answered items the catalog cannot name")

// UpdateSubscription sets the partition's subscription to the items given at
// the provider first, then stores what the provider answers, and answers the row
// and its items as stored. It consumes catalog price ids and quantities; each
// price's provider reference is read from Store.GetPrice in the partition, so a
// price outside the partition or without a provider price is refused before the
// provider is asked. The provider's answer is mapped back to catalog price ids
// through the prices asked for, the subscription's stored items and the row's
// own price, and stored whole through Store.UpdateSubscriptionItems; a provider
// refusal or an answer the catalog cannot name writes nothing. The write carries
// no status: the provider reports the status that results through its event,
// which the billing host's webhook applier writes. A subscription with no
// provider reference is refused (ErrNoProviderSubscription): the operation is
// named for a change at the provider and makes no local-only one. The billing
// host's UpdateSubscription handler is its caller, with the sealed partition and
// the request's whole item list.
func (s *Service) UpdateSubscription(ctx context.Context, partition, id string, items []SubscriptionItem, prorate bool) (*Subscription, []SubscriptionItem, error) {
	sub, err := s.store.GetSubscription(ctx, partition, id)
	if err != nil {
		return nil, nil, err
	}
	if sub.ProviderRef == "" {
		return nil, nil, ErrNoProviderSubscription
	}
	if len(items) == 0 {
		return nil, nil, ErrNoSubscriptionItems
	}
	updater, ok := s.provider.(SubscriptionUpdater)
	if !ok {
		return nil, nil, ErrProviderUnsupported
	}
	ask, byRef, err := s.catalogItems(ctx, items)
	if err != nil {
		return nil, nil, err
	}
	res, err := updater.UpdateSubscription(ctx, sub.ProviderRef, ask, prorate)
	if err != nil {
		return nil, nil, err
	}
	if len(res.Items) == 0 {
		return nil, nil, ErrProviderItemsUnresolved
	}
	stored := make([]SubscriptionItem, 0, len(res.Items))
	for _, it := range res.Items {
		priceID, ok := byRef[it.PriceRef]
		if !ok {
			if priceID, err = s.priceIDOfProviderRef(ctx, partition, sub, it.PriceRef); err != nil {
				return nil, nil, err
			}
			byRef[it.PriceRef] = priceID
		}
		stored = append(stored, SubscriptionItem{PriceID: priceID, Quantity: it.Quantity})
	}
	if err := s.store.UpdateSubscriptionItems(ctx, partition, id, stored); err != nil {
		return nil, nil, err
	}
	row, err := s.store.GetSubscription(ctx, partition, id)
	if err != nil {
		return nil, nil, err
	}
	return row, stored, nil
}

// priceIDOfProviderRef answers the catalog price whose provider
// reference is ref, looking among the prices of the subscription's stored items
// and the row's own price: the prices a provider answer can name that the
// request did not. ErrProviderItemsUnresolved when none of them is it.
func (s *Service) priceIDOfProviderRef(ctx context.Context, partition string, sub *Subscription, ref string) (string, error) {
	if ref == "" {
		return "", ErrProviderItemsUnresolved
	}
	held, err := s.store.ListSubscriptionItems(ctx, partition, sub.ID)
	if err != nil {
		return "", err
	}
	candidates := make([]string, 0, len(held)+1)
	for _, it := range held {
		candidates = append(candidates, it.PriceID)
	}
	if sub.PriceID != "" {
		candidates = append(candidates, sub.PriceID)
	}
	for _, priceID := range candidates {
		price, err := s.store.GetPrice(ctx, CatalogPartition, priceID)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return "", err
		}
		if price.ProviderRef == ref {
			return price.ID, nil
		}
	}
	return "", ErrProviderItemsUnresolved
}

// subscriptionItemCreateSQL is the statement InitSchema creates the item table
// with. Items are keyed by (subscription, price) and ordered by position, the
// provider's order, so the first row is the one the subscription row carries as
// its price and quantity. The table has no partition column: every read and
// write reaches it through the subscription row's partition predicate, so an
// empty partition reaches no item.
const subscriptionItemCreateSQL = `CREATE TABLE IF NOT EXISTS billing_subscription_items (
			subscription_id TEXT NOT NULL, price_id TEXT NOT NULL DEFAULT '',
			quantity INTEGER NOT NULL DEFAULT 0, position INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (subscription_id, price_id)
		)`

// UpdateSubscriptionItems replaces one subscription's item rows with items, in
// the order given, and writes the first item's price_id and quantity and
// updated_at onto the subscription row, no other column, so a status the
// provider's event wrote is never put back by an item change. The row update
// carries the partition predicate and admits the item writes: a row the
// partition does not hold answers ErrNotFound and nothing is written. An empty
// list is refused (ErrNoSubscriptionItems). Service.UpdateSubscription is its
// caller, with the items the provider answered.
func (s *SQLStore) UpdateSubscriptionItems(ctx context.Context, partition, id string, items []SubscriptionItem) error {
	if len(items) == 0 {
		return ErrNoSubscriptionItems
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollbackUnlessCommitted(tx)
	now := s.now().UnixNano()
	res, err := tx.ExecContext(ctx, `UPDATE billing_subscriptions SET price_id=?, quantity=?, updated_at=?
		WHERE id = ? AND (? <> '' AND partition = ?)`,
		items[0].PriceID, items[0].Quantity, now, id, partition, partition)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM billing_subscription_items WHERE subscription_id = ?`, id); err != nil {
		return err
	}
	if err := insertSubscriptionItems(ctx, tx, id, items, now); err != nil {
		return err
	}
	return tx.Commit()
}

// insertSubscriptionItems writes items as the rows of one subscription, at
// their positions, stamped with the instant given. CreateSubscription and
// UpdateSubscriptionItems call it inside their own transactions.
func insertSubscriptionItems(ctx context.Context, exec interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, id string, items []SubscriptionItem, now int64) error {
	for i, it := range items {
		if _, err := exec.ExecContext(ctx, `INSERT INTO billing_subscription_items
			(subscription_id, price_id, quantity, position, created_at, updated_at) VALUES (?,?,?,?,?,?)`,
			id, it.PriceID, it.Quantity, i, now, now); err != nil {
			return err
		}
	}
	return nil
}

// ListSubscriptionItems answers one subscription's items in position order,
// price id and quantity, and no rows for a subscription the partition does not
// hold: the selector joins the subscription row under its partition predicate.
// A row that carries a price and has no item row answers that price and
// quantity as its one item (rowItemOf): the row's price is the item the store
// holds for it, and a reader that copies the items and sends the list back
// must be answered the price the subscription has. Service.UpdateSubscription
// reads it to name the prices a provider answer carries that the request did
// not; the billing host's reads answer it beside the row.
func (s *SQLStore) ListSubscriptionItems(ctx context.Context, partition, id string) ([]SubscriptionItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT i.price_id, i.quantity FROM billing_subscription_items i
		JOIN billing_subscriptions s ON s.id = i.subscription_id
		WHERE s.id = ? AND (? <> '' AND s.partition = ?) ORDER BY i.position ASC`, id, partition, partition)
	if err != nil {
		return nil, err
	}
	defer closeRows(rows)
	var out []SubscriptionItem
	for rows.Next() {
		var it SubscriptionItem
		if err := rows.Scan(&it.PriceID, &it.Quantity); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) > 0 {
		return out, nil
	}
	row, err := s.GetSubscription(ctx, partition, id)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return rowItemOf(row), nil
}

// rowItemOf is the one item a subscription row carries in its own columns,
// price_id and quantity, and none when the row names no price. Both stores'
// ListSubscriptionItems answer it for a subscription with no item row.
func rowItemOf(row *Subscription) []SubscriptionItem {
	if row == nil || row.PriceID == "" {
		return nil
	}
	return []SubscriptionItem{{PriceID: row.PriceID, Quantity: row.Quantity}}
}

// UpdateSubscriptionItems is the reference behaviour SQLStore's method is held
// to: the item set becomes the list given, the row's price and quantity become
// the first item's, UpdatedAt moves, every other field stays as stored.
func (m *MemStore) UpdateSubscriptionItems(_ context.Context, partition, id string, items []SubscriptionItem) error {
	if len(items) == 0 {
		return ErrNoSubscriptionItems
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.subscriptions[id]
	if !ok || !partitionMatch(partition, e.Partition) {
		return ErrNotFound
	}
	upd := cloneSubscription(e)
	upd.PriceID = items[0].PriceID
	upd.Quantity = items[0].Quantity
	upd.UpdatedAt = m.now()
	m.subscriptions[id] = upd
	m.subscriptionItems[id] = storedItems(items)
	return nil
}

// ListSubscriptionItems is the reference behaviour SQLStore's read is held to:
// the items in the order written, price id and quantity, the row's own price
// and quantity for a subscription with no item (rowItemOf), none for a
// subscription the partition does not hold.
func (m *MemStore) ListSubscriptionItems(_ context.Context, partition, id string) ([]SubscriptionItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.subscriptions[id]
	if !ok || !partitionMatch(partition, e.Partition) {
		return nil, nil
	}
	if items := m.subscriptionItems[id]; len(items) > 0 {
		return append([]SubscriptionItem(nil), items...), nil
	}
	return rowItemOf(e), nil
}

// storedItems is the shape an item row holds: price id and quantity. The
// provider reference is the catalog price's and is not stored on the item.
func storedItems(items []SubscriptionItem) []SubscriptionItem {
	out := make([]SubscriptionItem, len(items))
	for i, it := range items {
		out[i] = SubscriptionItem{PriceID: it.PriceID, Quantity: it.Quantity}
	}
	return out
}
