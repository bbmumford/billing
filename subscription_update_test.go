package billing

import (
	"context"
	"errors"
	"testing"
	"time"
)

// updatingProvider is the fakeProvider with the optional SubscriptionUpdater
// surface: it records every update it is asked for and answers the result or
// error a test sets. The plain fakeProvider stays without the surface so a test
// can show what a provider lacking it is answered.
type updatingProvider struct {
	fakeProvider
	updates []updateCall
	answer  SubscriptionResult
	err     error
}

// updateCall is one UpdateSubscription the updatingProvider is asked for.
type updateCall struct {
	providerRef string
	items       []SubscriptionItem
	prorate     bool
}

func (p *updatingProvider) UpdateSubscription(_ context.Context, providerRef string, items []SubscriptionItem, prorate bool) (SubscriptionResult, error) {
	p.updates = append(p.updates, updateCall{providerRef: providerRef, items: append([]SubscriptionItem(nil), items...), prorate: prorate})
	if p.err != nil {
		return SubscriptionResult{}, p.err
	}
	return p.answer, nil
}

// itemWritingStore is the reference MemStore with its item write counted and
// hooked, so a Service test can tell whether the store is written and in what
// order against the provider call.
type itemWritingStore struct {
	*MemStore
	itemWrites int
	onItems    func()
}

func (s *itemWritingStore) UpdateSubscriptionItems(c context.Context, partition, id string, items []SubscriptionItem) error {
	s.itemWrites++
	if s.onItems != nil {
		s.onItems()
	}
	return s.MemStore.UpdateSubscriptionItems(c, partition, id, items)
}

// catalogWithTwoPrices stores the partition's plan price p_plan (provider
// price_plan) and add-on price p_addon (provider price_addon), and a customer c1
// with an active subscription on p_plan, quantity 1, provider ref sub_9.
func catalogWithTwoPrices(t *testing.T, s Store) *Subscription {
	t.Helper()
	for _, p := range []*Price{
		{ID: "p_plan", Partition: "acme", ProductID: "prod_plan", ProviderRef: "price_plan", Currency: "usd", AmountCents: 1000},
		{ID: "p_addon", Partition: "acme", ProductID: "prod_addon", ProviderRef: "price_addon", Currency: "usd", AmountCents: 500},
		{ID: "p_unsold", Partition: "acme", ProductID: "prod_unsold", Currency: "usd", AmountCents: 1},
	} {
		if err := s.CreatePrice(ctx, p); err != nil {
			t.Fatalf("CreatePrice(%s): %v", p.ID, err)
		}
	}
	sub := &Subscription{Partition: "acme", CustomerID: "c1", PriceID: "p_plan", ProviderRef: "sub_9", Status: "active", Quantity: 1}
	if err := s.CreateSubscription(ctx, sub); err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}
	return sub
}

func itemsEqual(a, b []SubscriptionItem) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Service.UpdateSubscription asks the provider first, with every item's provider
// price reference resolved from the catalog and the proration flag as given, and
// stores what the provider answers: every answered item is a row, the first is
// the row's price and quantity, and the status and its instants stay what the
// provider's event last wrote. The row and items answered are the stored ones.
func TestService_UpdateSubscriptionCallsTheProviderFirstAndStoresEveryItemItAnswers(t *testing.T) {
	for _, prorate := range []bool{true, false} {
		t.Run(map[bool]string{true: "prorated", false: "not prorated"}[prorate], func(t *testing.T) {
			eachStore(t, func(t *testing.T, store Store) {
				sub := catalogWithTwoPrices(t, store)
				setStatus(t, store, "acme", sub.ID, "past_due")
				before := mustGet(t, store, "acme", sub.ID)
				since, ok := before.UnpaidSince()
				if !ok {
					t.Fatal("the past_due row answers no unpaid clock")
				}
				prov := &updatingProvider{answer: SubscriptionResult{
					ProviderRef: "sub_9", Status: "active",
					Items: []SubscriptionItem{{PriceRef: "price_plan", Quantity: 5}, {PriceRef: "price_addon", Quantity: 2}},
				}}
				svc := newService(t, store, prov)

				row, items, err := svc.UpdateSubscription(ctx, "acme", sub.ID,
					[]SubscriptionItem{{PriceID: "p_plan", Quantity: 5}, {PriceID: "p_addon", Quantity: 2}}, prorate)
				if err != nil {
					t.Fatalf("UpdateSubscription: %v", err)
				}
				wantAsked := []SubscriptionItem{{PriceID: "p_plan", PriceRef: "price_plan", Quantity: 5}, {PriceID: "p_addon", PriceRef: "price_addon", Quantity: 2}}
				if len(prov.updates) != 1 || prov.updates[0].providerRef != "sub_9" || prov.updates[0].prorate != prorate ||
					!itemsEqual(prov.updates[0].items, wantAsked) {
					t.Fatalf("provider updates = %+v, want one of sub_9, prorate=%v, items %+v", prov.updates, prorate, wantAsked)
				}
				wantStored := []SubscriptionItem{{PriceID: "p_plan", Quantity: 5}, {PriceID: "p_addon", Quantity: 2}}
				if !itemsEqual(items, wantStored) {
					t.Fatalf("answered items = %+v, want the provider's %+v as catalog price ids and quantities", items, wantStored)
				}
				stored, err := store.ListSubscriptionItems(ctx, "acme", sub.ID)
				if err != nil {
					t.Fatalf("ListSubscriptionItems: %v", err)
				}
				if !itemsEqual(stored, wantStored) {
					t.Fatalf("stored items = %+v, want %+v: not every item is a row", stored, wantStored)
				}
				if row.PriceID != "p_plan" || row.Quantity != 5 {
					t.Fatalf("row item = (%q, %d), want the first answered item (p_plan, 5)", row.PriceID, row.Quantity)
				}
				if row.Status != "past_due" || !row.StatusChangedAt.Equal(before.StatusChangedAt) {
					t.Fatalf("status %q changed at %v, want past_due at %v: the item update wrote a status", row.Status, row.StatusChangedAt, before.StatusChangedAt)
				}
				wantUnpaid(t, row, since)
				got := mustGet(t, store, "acme", sub.ID)
				if got.PriceID != row.PriceID || got.Quantity != row.Quantity || got.Status != row.Status || !got.UpdatedAt.Equal(row.UpdatedAt) {
					t.Fatalf("answered %+v, stored %+v: the answer is not the stored row", row, got)
				}
			})
		})
	}
}

// The provider's answer is what is stored, not the request: an item the
// provider answers with another quantity, or keeps that the request did not
// name, is stored as answered, its catalog price found from the rows the
// subscription already holds.
func TestService_UpdateSubscriptionStoresTheProvidersAnswerNotTheRequest(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		sub := catalogWithTwoPrices(t, store)
		prov := &updatingProvider{answer: SubscriptionResult{
			ProviderRef: "sub_9",
			Items:       []SubscriptionItem{{PriceRef: "price_plan", Quantity: 1}, {PriceRef: "price_addon", Quantity: 3}},
		}}
		svc := newService(t, store, prov)
		_, items, err := svc.UpdateSubscription(ctx, "acme", sub.ID, []SubscriptionItem{{PriceID: "p_addon", Quantity: 7}}, false)
		if err != nil {
			t.Fatalf("UpdateSubscription: %v", err)
		}
		want := []SubscriptionItem{{PriceID: "p_plan", Quantity: 1}, {PriceID: "p_addon", Quantity: 3}}
		if !itemsEqual(items, want) {
			t.Fatalf("answered items = %+v, want the provider's %+v", items, want)
		}
		stored, err := store.ListSubscriptionItems(ctx, "acme", sub.ID)
		if err != nil || !itemsEqual(stored, want) {
			t.Fatalf("stored items = %+v (%v), want %+v", stored, err, want)
		}
	})
}

// A provider that refuses the update leaves the subscription unchanged: no
// store write of any shape is made, so the local price is never one the
// provider did not confirm.
func TestService_UpdateSubscriptionWritesNothingWhenTheProviderRefuses(t *testing.T) {
	store := &itemWritingStore{MemStore: NewMemStore()}
	sub := catalogWithTwoPrices(t, store)
	refusal := errors.New("provider says no")
	prov := &updatingProvider{err: refusal}
	svc := newService(t, store, prov)
	_, _, err := svc.UpdateSubscription(ctx, "acme", sub.ID, []SubscriptionItem{{PriceID: "p_addon", Quantity: 1}}, true)
	if !errors.Is(err, refusal) {
		t.Fatalf("UpdateSubscription = %v, want the provider's refusal", err)
	}
	if len(prov.updates) != 1 {
		t.Fatalf("provider updates = %d, want 1", len(prov.updates))
	}
	if store.itemWrites != 0 {
		t.Fatalf("the store was written %d times after a provider refusal, want 0", store.itemWrites)
	}
	got := mustGet(t, store, "acme", sub.ID)
	if got.PriceID != "p_plan" || got.Quantity != 1 || got.Status != "active" {
		t.Fatalf("row changed after a refused update: %+v", got)
	}
	items, err := store.ListSubscriptionItems(ctx, "acme", sub.ID)
	if err != nil || !itemsEqual(items, []SubscriptionItem{{PriceID: "p_plan", Quantity: 1}}) {
		t.Fatalf("items after a refused update = %+v (%v), want the created item alone", items, err)
	}
}

// The store write comes after the provider has answered, never before.
func TestService_UpdateSubscriptionWritesTheStoreOnlyAfterTheProviderAnswered(t *testing.T) {
	store := &itemWritingStore{MemStore: NewMemStore()}
	sub := catalogWithTwoPrices(t, store)
	prov := &updatingProvider{answer: SubscriptionResult{Items: []SubscriptionItem{{PriceRef: "price_addon", Quantity: 1}}}}
	store.onItems = func() {
		if len(prov.updates) != 1 {
			t.Fatalf("the store was written with %d provider updates made, want 1 made before the write", len(prov.updates))
		}
	}
	svc := newService(t, store, prov)
	if _, _, err := svc.UpdateSubscription(ctx, "acme", sub.ID, []SubscriptionItem{{PriceID: "p_addon", Quantity: 1}}, true); err != nil {
		t.Fatalf("UpdateSubscription: %v", err)
	}
	if store.itemWrites != 1 {
		t.Fatalf("item writes = %d, want 1", store.itemWrites)
	}
}

// A provider without the SubscriptionUpdater surface is answered
// ErrProviderUnsupported and nothing is written: the operation is named for a
// change at the provider and makes no local-only one.
func TestService_UpdateSubscriptionRefusesAProviderWithoutTheSurface(t *testing.T) {
	store := &itemWritingStore{MemStore: NewMemStore()}
	sub := catalogWithTwoPrices(t, store)
	svc := newService(t, store, &fakeProvider{})
	_, _, err := svc.UpdateSubscription(ctx, "acme", sub.ID, []SubscriptionItem{{PriceID: "p_addon", Quantity: 1}}, true)
	if !errors.Is(err, ErrProviderUnsupported) {
		t.Fatalf("UpdateSubscription = %v, want ErrProviderUnsupported", err)
	}
	if store.itemWrites != 0 {
		t.Fatalf("the store was written %d times by a provider without the surface, want 0", store.itemWrites)
	}
	if got := mustGet(t, store, "acme", sub.ID); got.PriceID != "p_plan" || got.Quantity != 1 {
		t.Fatalf("row changed: %+v", got)
	}
}

// What is refused before the provider is asked, each writing nothing: a
// subscription with no provider subscription, no items, an item with no price
// or a quantity below one, a price named twice, a price the catalog does not
// hold in the partition, and a price with no provider price.
func TestService_UpdateSubscriptionRefusesWhatTheProviderCannotBeAsked(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		sub := catalogWithTwoPrices(t, store)
		local := &Subscription{Partition: "acme", CustomerID: "c1", PriceID: "p_plan", Status: "active", Quantity: 1}
		if err := store.CreateSubscription(ctx, local); err != nil {
			t.Fatalf("CreateSubscription: %v", err)
		}
		for _, tc := range []struct {
			name  string
			id    string
			items []SubscriptionItem
			want  error
		}{
			{"no provider subscription", local.ID, []SubscriptionItem{{PriceID: "p_addon", Quantity: 1}}, ErrNoProviderSubscription},
			{"no items", sub.ID, nil, ErrNoSubscriptionItems},
			{"empty price", sub.ID, []SubscriptionItem{{Quantity: 1}}, ErrInvalidSubscriptionItem},
			{"zero quantity", sub.ID, []SubscriptionItem{{PriceID: "p_addon"}}, ErrInvalidSubscriptionItem},
			{"price named twice", sub.ID, []SubscriptionItem{{PriceID: "p_addon", Quantity: 1}, {PriceID: "p_addon", Quantity: 2}}, ErrInvalidSubscriptionItem},
			{"unknown price", sub.ID, []SubscriptionItem{{PriceID: "p_missing", Quantity: 1}}, ErrNotFound},
			{"price with no provider price", sub.ID, []SubscriptionItem{{PriceID: "p_unsold", Quantity: 1}}, ErrNoProviderPrice},
		} {
			t.Run(tc.name, func(t *testing.T) {
				prov := &updatingProvider{}
				svc := newService(t, store, prov)
				if _, _, err := svc.UpdateSubscription(ctx, "acme", tc.id, tc.items, true); !errors.Is(err, tc.want) {
					t.Fatalf("UpdateSubscription = %v, want %v", err, tc.want)
				}
				if len(prov.updates) != 0 {
					t.Fatalf("the provider was asked %d times", len(prov.updates))
				}
			})
		}
		got := mustGet(t, store, "acme", sub.ID)
		if got.PriceID != "p_plan" || got.Quantity != 1 {
			t.Fatalf("row changed by a refused update: %+v", got)
		}
	})
}

// An answer the store cannot hold is refused and nothing is written: no items
// at all, or an item whose provider price is not a catalog price of the
// partition.
func TestService_UpdateSubscriptionRefusesAnAnswerTheCatalogCannotName(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer []SubscriptionItem
	}{
		{"no items answered", nil},
		{"an item of a price outside the catalog", []SubscriptionItem{{PriceRef: "price_elsewhere", Quantity: 1}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &itemWritingStore{MemStore: NewMemStore()}
			sub := catalogWithTwoPrices(t, store)
			prov := &updatingProvider{answer: SubscriptionResult{Items: tc.answer}}
			svc := newService(t, store, prov)
			_, _, err := svc.UpdateSubscription(ctx, "acme", sub.ID, []SubscriptionItem{{PriceID: "p_addon", Quantity: 1}}, true)
			if !errors.Is(err, ErrProviderItemsUnresolved) {
				t.Fatalf("UpdateSubscription = %v, want ErrProviderItemsUnresolved", err)
			}
			if store.itemWrites != 0 {
				t.Fatalf("the store was written %d times for an answer it cannot hold", store.itemWrites)
			}
		})
	}
}

// The read is inside the partition named: another partition's subscription, an
// empty partition and an unknown id answer ErrNotFound, and the provider is not
// asked.
func TestService_UpdateSubscriptionReadsOnlyInsideThePartition(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		sub := catalogWithTwoPrices(t, store)
		prov := &updatingProvider{}
		svc := newService(t, store, prov)
		for _, tc := range []struct{ partition, id string }{{"globex", sub.ID}, {"", sub.ID}, {"acme", "missing"}} {
			if _, _, err := svc.UpdateSubscription(ctx, tc.partition, tc.id, []SubscriptionItem{{PriceID: "p_addon", Quantity: 1}}, true); !errors.Is(err, ErrNotFound) {
				t.Fatalf("UpdateSubscription(%q, %q) = %v, want ErrNotFound", tc.partition, tc.id, err)
			}
		}
		if len(prov.updates) != 0 {
			t.Fatalf("the provider was asked %d times from outside the partition", len(prov.updates))
		}
	})
}

// UpdateSubscriptionItems replaces the subscription's item rows with the list
// given, writes the first item onto the row as its price and quantity, moves
// updated_at, and leaves every other column as stored: the status and the two
// instants it carries stay what the provider's event wrote.
func TestUpdateSubscriptionItems_WritesEveryItemAndNeverTheStatus(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		start, end := at, at.Add(30*24*time.Hour)
		sub := &Subscription{Partition: "acme", CustomerID: "c1", PriceID: "p1", ProviderRef: "sub_1",
			Status: "active", Quantity: 1, CurrentPeriodStart: &start, CurrentPeriodEnd: &end, CreatedAt: at}
		if err := s.CreateSubscription(ctx, sub); err != nil {
			t.Fatalf("CreateSubscription: %v", err)
		}
		created, err := s.ListSubscriptionItems(ctx, "acme", sub.ID)
		if err != nil || !itemsEqual(created, []SubscriptionItem{{PriceID: "p1", Quantity: 1}}) {
			t.Fatalf("items after creation = %+v (%v), want the created item (p1, 1)", created, err)
		}
		setStatus(t, s, "acme", sub.ID, "past_due")
		before := mustGet(t, s, "acme", sub.ID)

		items := []SubscriptionItem{{PriceID: "p2", Quantity: 3}, {PriceID: "p3", Quantity: 1}}
		if err := s.UpdateSubscriptionItems(ctx, "acme", sub.ID, items); err != nil {
			t.Fatalf("UpdateSubscriptionItems: %v", err)
		}
		stored, err := s.ListSubscriptionItems(ctx, "acme", sub.ID)
		if err != nil {
			t.Fatalf("ListSubscriptionItems: %v", err)
		}
		if !itemsEqual(stored, items) {
			t.Fatalf("stored items = %+v, want %+v in the order given", stored, items)
		}
		got := mustGet(t, s, "acme", sub.ID)
		if got.PriceID != "p2" || got.Quantity != 3 {
			t.Fatalf("row item = (%q, %d), want the first item (p2, 3)", got.PriceID, got.Quantity)
		}
		if got.Status != "past_due" || !got.StatusChangedAt.Equal(before.StatusChangedAt) {
			t.Fatalf("status %q changed at %v, want past_due at %v", got.Status, got.StatusChangedAt, before.StatusChangedAt)
		}
		if since, ok := before.UnpaidSince(); ok {
			wantUnpaid(t, got, since)
		}
		if got.ProviderRef != "sub_1" || got.CustomerID != "c1" || got.CurrentPeriodStart == nil || !got.CurrentPeriodStart.Equal(start) ||
			got.CurrentPeriodEnd == nil || !got.CurrentPeriodEnd.Equal(end) {
			t.Fatalf("other columns moved: %+v", got)
		}
		if !got.CreatedAt.Equal(before.CreatedAt) || got.UpdatedAt.Before(before.UpdatedAt) {
			t.Fatalf("times: created %v (was %v), updated %v (was %v)", got.CreatedAt, before.CreatedAt, got.UpdatedAt, before.UpdatedAt)
		}

		// A second write with one item leaves one row: the list given is the whole set.
		if err := s.UpdateSubscriptionItems(ctx, "acme", sub.ID, items[1:]); err != nil {
			t.Fatalf("UpdateSubscriptionItems(second): %v", err)
		}
		stored, _ = s.ListSubscriptionItems(ctx, "acme", sub.ID)
		if !itemsEqual(stored, items[1:]) {
			t.Fatalf("stored items after the second write = %+v, want %+v", stored, items[1:])
		}
		if got = mustGet(t, s, "acme", sub.ID); got.PriceID != "p3" || got.Quantity != 1 {
			t.Fatalf("row item = (%q, %d), want (p3, 1)", got.PriceID, got.Quantity)
		}
	})
}

// The write and the read are partition-scoped like every other: another
// partition, an empty partition and an unknown id answer ErrNotFound on the
// write and no rows on the read, and change nothing; an empty item list is
// refused, because the row's price has to be one of its items.
func TestUpdateSubscriptionItems_ReachesOnlyItsPartition(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		sub := &Subscription{Partition: "acme", CustomerID: "c1", PriceID: "p1", Status: "active", Quantity: 1}
		if err := s.CreateSubscription(ctx, sub); err != nil {
			t.Fatalf("CreateSubscription: %v", err)
		}
		for _, tc := range []struct{ partition, id string }{{"globex", sub.ID}, {"", sub.ID}, {"acme", "missing"}} {
			if err := s.UpdateSubscriptionItems(ctx, tc.partition, tc.id, []SubscriptionItem{{PriceID: "p9", Quantity: 9}}); !errors.Is(err, ErrNotFound) {
				t.Fatalf("UpdateSubscriptionItems(%q, %q) = %v, want ErrNotFound", tc.partition, tc.id, err)
			}
			if items, err := s.ListSubscriptionItems(ctx, tc.partition, tc.id); err != nil || len(items) != 0 {
				t.Fatalf("ListSubscriptionItems(%q, %q) = %+v, %v, want no rows", tc.partition, tc.id, items, err)
			}
		}
		if err := s.UpdateSubscriptionItems(ctx, "acme", sub.ID, nil); !errors.Is(err, ErrNoSubscriptionItems) {
			t.Fatalf("UpdateSubscriptionItems(no items) = %v, want ErrNoSubscriptionItems", err)
		}
		got := mustGet(t, s, "acme", sub.ID)
		items, _ := s.ListSubscriptionItems(ctx, "acme", sub.ID)
		if got.PriceID != "p1" || got.Quantity != 1 || !itemsEqual(items, []SubscriptionItem{{PriceID: "p1", Quantity: 1}}) {
			t.Fatalf("a refused write changed the subscription: row %+v items %+v", got, items)
		}
	})
}

// A subscription row that carries a price but has no item rows answers that
// price and quantity as its one item: the row's price is the item the store
// held for it, so a caller that copies the items and sends the list back sends
// the price the subscription has, and a row with no price answers no item.
func TestListSubscriptionItems_AnswersTheRowsPriceWhenNoItemRowExists(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		sub := &Subscription{Partition: "acme", CustomerID: "c1", ProviderRef: "sub_1", Status: "active"}
		if err := s.CreateSubscription(ctx, sub); err != nil {
			t.Fatalf("CreateSubscription: %v", err)
		}
		if items, err := s.ListSubscriptionItems(ctx, "acme", sub.ID); err != nil || len(items) != 0 {
			t.Fatalf("items of a subscription with no price = %+v (%v), want none", items, err)
		}
		row := mustGet(t, s, "acme", sub.ID)
		row.PriceID, row.Quantity = "p_old", 3
		if err := s.UpdateSubscription(ctx, row); err != nil {
			t.Fatalf("UpdateSubscription: %v", err)
		}
		items, err := s.ListSubscriptionItems(ctx, "acme", sub.ID)
		if err != nil || !itemsEqual(items, []SubscriptionItem{{PriceID: "p_old", Quantity: 3}}) {
			t.Fatalf("items of a row with a price and no item rows = %+v (%v), want the row's (p_old, 3)", items, err)
		}
		if items, err := s.ListSubscriptionItems(ctx, "globex", sub.ID); err != nil || len(items) != 0 {
			t.Fatalf("items from another partition = %+v (%v), want none", items, err)
		}
	})
}

// CreateSubscription writes the row's price and quantity as the subscription's
// item row itself, observed at the table and the map and not through
// ListSubscriptionItems, whose row fallback would answer the same list for a
// subscription with no item row.
func TestCreateSubscription_WritesTheItemRow(t *testing.T) {
	t.Run("sql", func(t *testing.T) {
		db := openDB(t)
		s := NewSQLStore(db)
		if err := s.InitSchema(ctx); err != nil {
			t.Fatalf("InitSchema: %v", err)
		}
		sub := &Subscription{Partition: "acme", CustomerID: "c1", PriceID: "p1", Status: "active", Quantity: 2}
		if err := s.CreateSubscription(ctx, sub); err != nil {
			t.Fatalf("CreateSubscription: %v", err)
		}
		var n int
		var priceID string
		var quantity int64
		if err := db.QueryRow(`SELECT count(*), max(price_id), max(quantity) FROM billing_subscription_items WHERE subscription_id = ?`, sub.ID).Scan(&n, &priceID, &quantity); err != nil {
			t.Fatalf("count item rows: %v", err)
		}
		if n != 1 || priceID != "p1" || quantity != 2 {
			t.Fatalf("item rows after creation = %d (%q, %d), want one row (p1, 2)", n, priceID, quantity)
		}
	})
	t.Run("mem", func(t *testing.T) {
		m := NewMemStore()
		sub := &Subscription{Partition: "acme", CustomerID: "c1", PriceID: "p1", Status: "active", Quantity: 2}
		if err := m.CreateSubscription(ctx, sub); err != nil {
			t.Fatalf("CreateSubscription: %v", err)
		}
		if got := m.subscriptionItems[sub.ID]; !itemsEqual(got, []SubscriptionItem{{PriceID: "p1", Quantity: 2}}) {
			t.Fatalf("items held after creation = %+v, want (p1, 2)", got)
		}
	})
}
