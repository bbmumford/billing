package billing

import "context"

// SubscriptionItemsCreator is an OPTIONAL PaymentProvider extension that
// creates one provider subscription holding every item given, each at its
// provider price and quantity, in one provider call, and answers the
// subscription as the provider holds it afterwards, every item in
// SubscriptionResult.Items. in carries the customer and the billing profile as
// it does for CreateSubscription, with PriceRef and Quantity set to the first
// item's. Service.SubscribeItems type-asserts on it for a list of two or more
// items and answers ErrProviderUnsupported when the wired provider does not
// implement it, so a provider that can sell only one price is never asked to
// create a subscription the caller named several items for; the core
// PaymentProvider stays unchanged. One call, not a create followed by an item
// update, because the provider bills a subscription's first period at its
// creation and a second call that failed would leave the provider holding a
// subscription with only the first item.
type SubscriptionItemsCreator interface {
	CreateSubscriptionWithItems(ctx context.Context, in SubscriptionInput, items []SubscriptionItem) (SubscriptionResult, error)
}

// SubscribeItems creates the partition's subscription for the customer with
// every item given at the provider first, then stores what the provider answers,
// and answers the row and its items as stored. It consumes catalog price ids and
// quantities; each price's provider reference is read from Store.GetPrice in the
// partition, so a price outside the partition or without a provider price is
// refused before the provider is asked (catalogItems), as is an empty list. One
// item is asked of the core CreateSubscription; two or more of
// SubscriptionItemsCreator. billingProfileID is handed to the provider as
// SubscriptionInput.BillingProfileID and stored nowhere here, as Subscribe does.
// The provider's answer is mapped back to catalog price ids through the prices
// asked for and stored whole through Store.CreateSubscriptionWithItems, the
// first item as the row's price and quantity; a provider answering no item list
// confirms the items it is asked, which are then the rows. A provider refusal,
// a status outside ValidSubscriptionStatus, or an answer naming a price the
// request did not ask for writes nothing. idempotencyKey is the caller's key for
// the create, handled as Subscribe handles it: the provider is handed the key
// derived from it and the partition, and a retry the provider answers with the
// subscription the first call made is answered the row and items the first call
// stored. Its caller is the billing host's CreateSubscription handler, with every
// item its request names.
// TODO: an answer refused after the provider created the subscription leaves
// that provider subscription with no local row.
func (s *Service) SubscribeItems(ctx context.Context, partition, customerID, customerRef string, items []SubscriptionItem, billingProfileID, idempotencyKey string) (*Subscription, []SubscriptionItem, error) {
	if len(items) == 0 {
		return nil, nil, ErrNoSubscriptionItems
	}
	ask, byRef, err := s.catalogItems(ctx, partition, items)
	if err != nil {
		return nil, nil, err
	}
	in := SubscriptionInput{CustomerRef: customerRef, PriceRef: ask[0].PriceRef, Quantity: ask[0].Quantity,
		BillingProfileID: billingProfileID, IdempotencyKey: providerIdempotencyKey("subscribe", partition, idempotencyKey)}
	var res SubscriptionResult
	if len(ask) == 1 {
		res, err = s.provider.CreateSubscription(ctx, in)
	} else {
		creator, ok := s.provider.(SubscriptionItemsCreator)
		if !ok {
			return nil, nil, ErrProviderUnsupported
		}
		res, err = creator.CreateSubscriptionWithItems(ctx, in, ask)
	}
	if err != nil {
		return nil, nil, err
	}
	if res.Status != "" && !ValidSubscriptionStatus(res.Status) {
		return nil, nil, ErrInvalidSubscriptionStatus
	}
	row, err := s.storedSubscription(ctx, partition, customerID, res.ProviderRef)
	if err != nil {
		return nil, nil, err
	}
	if row != nil {
		rowItems, err := s.store.ListSubscriptionItems(ctx, partition, row.ID)
		if err != nil {
			return nil, nil, err
		}
		return row, rowItems, nil
	}
	stored := storedItems(ask)
	if len(res.Items) > 0 {
		stored = make([]SubscriptionItem, 0, len(res.Items))
		for _, it := range res.Items {
			priceID, ok := byRef[it.PriceRef]
			if !ok {
				return nil, nil, ErrProviderItemsUnresolved
			}
			stored = append(stored, SubscriptionItem{PriceID: priceID, Quantity: it.Quantity})
		}
	}
	sub := &Subscription{
		Partition:          partition,
		CustomerID:         customerID,
		ProviderRef:        res.ProviderRef,
		Status:             res.Status,
		CurrentPeriodStart: res.CurrentPeriodStart,
		CurrentPeriodEnd:   res.CurrentPeriodEnd,
	}
	if err := s.store.CreateSubscriptionWithItems(ctx, sub, stored); err != nil {
		return nil, nil, err
	}
	return sub, stored, nil
}

// catalogItems checks every item a caller names and reads its provider price
// from the partition's catalog: an item with no price id, a quantity below one
// or a price named twice is ErrInvalidSubscriptionItem, a price the partition
// does not hold the store's ErrNotFound, a price with no provider price
// ErrNoProviderPrice. It answers the items with PriceRef filled, in the order
// given, and the catalog price id of each provider price asked for, which maps
// the provider's answer back. Service.SubscribeItems and
// Service.UpdateSubscription read it before the provider is asked, so neither
// asks the provider for a price the partition cannot sell.
func (s *Service) catalogItems(ctx context.Context, partition string, items []SubscriptionItem) ([]SubscriptionItem, map[string]string, error) {
	ask := make([]SubscriptionItem, 0, len(items))
	byRef := map[string]string{}
	seen := map[string]bool{}
	for _, it := range items {
		if it.PriceID == "" || it.Quantity < 1 || seen[it.PriceID] {
			return nil, nil, ErrInvalidSubscriptionItem
		}
		seen[it.PriceID] = true
		price, err := s.store.GetPrice(ctx, partition, it.PriceID)
		if err != nil {
			return nil, nil, err
		}
		if price.ProviderRef == "" {
			return nil, nil, ErrNoProviderPrice
		}
		byRef[price.ProviderRef] = price.ID
		ask = append(ask, SubscriptionItem{PriceID: price.ID, PriceRef: price.ProviderRef, Quantity: it.Quantity})
	}
	return ask, byRef, nil
}

// checkCreateItems is the rule both stores' CreateSubscriptionWithItems hold a
// list to before writing anything: at least one item
// (ErrNoSubscriptionItems), and each with a price id, a quantity of at least
// one, and a price named once (ErrInvalidSubscriptionItem). The item rows are
// keyed by (subscription, price), so SQLStore would refuse a repeated price
// part-way; checking first keeps the two stores' answers one answer.
func checkCreateItems(items []SubscriptionItem) error {
	if len(items) == 0 {
		return ErrNoSubscriptionItems
	}
	seen := map[string]bool{}
	for _, it := range items {
		if it.PriceID == "" || it.Quantity < 1 || seen[it.PriceID] {
			return ErrInvalidSubscriptionItem
		}
		seen[it.PriceID] = true
	}
	return nil
}

// CreateSubscriptionWithItems inserts the row with the first item's price and
// quantity, whatever sub carries in those two fields, and every item as a row
// of billing_subscription_items in the order given, in one transaction, so a
// refused or failed write leaves neither the row nor any item. The list is
// checked first (checkCreateItems). Service.SubscribeItems is its caller, with
// the items the provider answered.
func (s *SQLStore) CreateSubscriptionWithItems(ctx context.Context, sub *Subscription, items []SubscriptionItem) error {
	if err := checkCreateItems(items); err != nil {
		return err
	}
	sub.PriceID, sub.Quantity = items[0].PriceID, items[0].Quantity
	return s.insertSubscription(ctx, sub, items)
}

// CreateSubscriptionWithItems is the reference behaviour SQLStore's write is
// held to: the list checked first, the row stored with the first item's price
// and quantity and its status instants from the store's clock, and the items in
// the order given.
func (m *MemStore) CreateSubscriptionWithItems(_ context.Context, s *Subscription, items []SubscriptionItem) error {
	if err := checkCreateItems(items); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s.PriceID, s.Quantity = items[0].PriceID, items[0].Quantity
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
	m.subscriptionItems[s.ID] = storedItems(items)
	return nil
}
