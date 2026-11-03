package billing

import (
	"context"
	"errors"
	"testing"
)

// itemsCreatingProvider is the fakeProvider with the optional
// SubscriptionItemsCreator surface: it records every multi-item create it is
// asked for and answers the result or error a test sets. The plain fakeProvider
// stays without the surface so a test can show what a provider lacking it is
// answered.
type itemsCreatingProvider struct {
	fakeProvider
	creates []itemsCreateCall
	answer  SubscriptionResult
	err     error
}

// itemsCreateCall is one CreateSubscriptionWithItems the provider is asked for.
type itemsCreateCall struct {
	in    SubscriptionInput
	items []SubscriptionItem
}

func (p *itemsCreatingProvider) CreateSubscriptionWithItems(_ context.Context, in SubscriptionInput, items []SubscriptionItem) (SubscriptionResult, error) {
	p.creates = append(p.creates, itemsCreateCall{in: in, items: append([]SubscriptionItem(nil), items...)})
	if p.err != nil {
		return SubscriptionResult{}, p.err
	}
	return p.answer, nil
}

// noSubscriptionFor fails the test when the partition holds any subscription
// for the customer: a refused create writes no row.
func noSubscriptionFor(t *testing.T, s Store, partition, customerID string) {
	t.Helper()
	rows, err := s.ListSubscriptionsByCustomer(ctx, partition, customerID)
	if err != nil {
		t.Fatalf("ListSubscriptionsByCustomer: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("%d subscriptions stored for %s, want none: a refused create wrote a row", len(rows), customerID)
	}
}

// Service.SubscribeItems asks the provider for one subscription holding every
// item the caller names, each price's provider reference read from the
// partition's catalog, and stores every item the provider answers as a row,
// the first as the row's price and quantity. The profile travels to the
// provider on the same call.
func TestService_SubscribeItemsCreatesEveryItemAtTheProviderAndStoresEachAsARow(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		catalogWithTwoPrices(t, store)
		prov := &itemsCreatingProvider{answer: SubscriptionResult{
			ProviderRef: "sub_new", Status: "active",
			Items: []SubscriptionItem{{PriceRef: "price_plan", Quantity: 1}, {PriceRef: "price_addon", Quantity: 3}},
		}}
		svc := newService(t, store, prov)

		row, items, err := svc.SubscribeItems(ctx, "acme", "c2", "cus_2",
			[]SubscriptionItem{{PriceID: "p_plan", Quantity: 1}, {PriceID: "p_addon", Quantity: 3}}, "bp_1")
		if err != nil {
			t.Fatalf("SubscribeItems: %v", err)
		}
		wantIn := SubscriptionInput{CustomerRef: "cus_2", PriceRef: "price_plan", Quantity: 1, BillingProfileID: "bp_1"}
		wantAsked := []SubscriptionItem{{PriceID: "p_plan", PriceRef: "price_plan", Quantity: 1}, {PriceID: "p_addon", PriceRef: "price_addon", Quantity: 3}}
		if len(prov.creates) != 1 || prov.creates[0].in != wantIn || !itemsEqual(prov.creates[0].items, wantAsked) {
			t.Fatalf("provider creates = %+v, want one of %+v with items %+v", prov.creates, wantIn, wantAsked)
		}
		if prov.subs != 0 {
			t.Fatalf("the one-price create was asked %d times beside the multi-item create", prov.subs)
		}
		wantStored := []SubscriptionItem{{PriceID: "p_plan", Quantity: 1}, {PriceID: "p_addon", Quantity: 3}}
		if !itemsEqual(items, wantStored) {
			t.Fatalf("answered items = %+v, want %+v", items, wantStored)
		}
		stored, err := store.ListSubscriptionItems(ctx, "acme", row.ID)
		if err != nil {
			t.Fatalf("ListSubscriptionItems: %v", err)
		}
		if !itemsEqual(stored, wantStored) {
			t.Fatalf("stored items = %+v, want %+v: an item after the first reached no row", stored, wantStored)
		}
		got := mustGet(t, store, "acme", row.ID)
		if got.PriceID != "p_plan" || got.Quantity != 1 || got.ProviderRef != "sub_new" || got.Status != "active" || got.CustomerID != "c2" {
			t.Fatalf("stored row = %+v, want c2's sub_new, active, on (p_plan, 1)", got)
		}
		if got.StatusChangedAt.IsZero() {
			t.Fatal("the created row carries no status instant")
		}
		if row.ID != got.ID || row.PriceID != got.PriceID || row.Status != got.Status {
			t.Fatalf("answered %+v, stored %+v: the answer is not the stored row", row, got)
		}
	})
}

// One item needs no extension: the core CreateSubscription is the provider
// call, with the catalog's provider price, and the item is the row's.
func TestService_SubscribeItemsWithOneItemUsesTheCoreCreate(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		catalogWithTwoPrices(t, store)
		prov := &inputRecordingProvider{}
		svc := newService(t, store, prov)
		row, items, err := svc.SubscribeItems(ctx, "acme", "c2", "cus_2", []SubscriptionItem{{PriceID: "p_plan", Quantity: 2}}, "")
		if err != nil {
			t.Fatalf("SubscribeItems: %v", err)
		}
		want := SubscriptionInput{CustomerRef: "cus_2", PriceRef: "price_plan", Quantity: 2}
		if len(prov.inputs) != 1 || prov.inputs[0] != want {
			t.Fatalf("provider inputs = %+v, want one of %+v", prov.inputs, want)
		}
		wantStored := []SubscriptionItem{{PriceID: "p_plan", Quantity: 2}}
		stored, err := store.ListSubscriptionItems(ctx, "acme", row.ID)
		if err != nil {
			t.Fatalf("ListSubscriptionItems: %v", err)
		}
		if !itemsEqual(items, wantStored) || !itemsEqual(stored, wantStored) {
			t.Fatalf("answered %+v, stored %+v, want %+v", items, stored, wantStored)
		}
		if row.ProviderRef != "sub_1" || row.PriceID != "p_plan" || row.Quantity != 2 {
			t.Fatalf("row = %+v, want the provider's sub_1 on (p_plan, 2)", row)
		}
	})
}

// A provider without the multi-item surface is refused before it is asked
// anything, so a subscription the request names two items for is never
// created at the provider with one of them.
func TestService_SubscribeItemsRefusesManyItemsToAProviderWithoutTheSurface(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		catalogWithTwoPrices(t, store)
		prov := &fakeProvider{}
		svc := newService(t, store, prov)
		_, _, err := svc.SubscribeItems(ctx, "acme", "c2", "cus_2",
			[]SubscriptionItem{{PriceID: "p_plan", Quantity: 1}, {PriceID: "p_addon", Quantity: 3}}, "")
		if !errors.Is(err, ErrProviderUnsupported) {
			t.Fatalf("SubscribeItems = %v, want ErrProviderUnsupported", err)
		}
		if prov.subs != 0 {
			t.Fatalf("the provider was asked to create %d subscriptions", prov.subs)
		}
		noSubscriptionFor(t, store, "acme", "c2")
	})
}

// Every item is checked before the provider is asked: a list with no item, an
// item with no price, a quantity below one, a price named twice, a price the
// partition's catalog does not hold, and a price with no provider price are
// each refused, and the provider and the store are left untouched.
func TestService_SubscribeItemsRefusesWhatTheProviderCannotBeAsked(t *testing.T) {
	for _, c := range []struct {
		name      string
		partition string
		items     []SubscriptionItem
		want      error
	}{
		{"no items", "acme", nil, ErrNoSubscriptionItems},
		{"no price id", "acme", []SubscriptionItem{{PriceID: "p_plan", Quantity: 1}, {Quantity: 1}}, ErrInvalidSubscriptionItem},
		{"quantity zero", "acme", []SubscriptionItem{{PriceID: "p_plan", Quantity: 1}, {PriceID: "p_addon", Quantity: 0}}, ErrInvalidSubscriptionItem},
		{"price twice", "acme", []SubscriptionItem{{PriceID: "p_plan", Quantity: 1}, {PriceID: "p_plan", Quantity: 2}}, ErrInvalidSubscriptionItem},
		{"price outside the catalog", "acme", []SubscriptionItem{{PriceID: "p_plan", Quantity: 1}, {PriceID: "p_nowhere", Quantity: 1}}, ErrNotFound},
		{"another partition", "other", []SubscriptionItem{{PriceID: "p_plan", Quantity: 1}, {PriceID: "p_addon", Quantity: 1}}, ErrNotFound},
		{"no provider price", "acme", []SubscriptionItem{{PriceID: "p_plan", Quantity: 1}, {PriceID: "p_unsold", Quantity: 1}}, ErrNoProviderPrice},
	} {
		t.Run(c.name, func(t *testing.T) {
			eachStore(t, func(t *testing.T, store Store) {
				catalogWithTwoPrices(t, store)
				prov := &itemsCreatingProvider{answer: SubscriptionResult{ProviderRef: "sub_new", Status: "active"}}
				svc := newService(t, store, prov)
				_, _, err := svc.SubscribeItems(ctx, c.partition, "c2", "cus_2", c.items, "")
				if !errors.Is(err, c.want) {
					t.Fatalf("SubscribeItems = %v, want %v", err, c.want)
				}
				if len(prov.creates) != 0 || prov.subs != 0 {
					t.Fatalf("the provider was asked (%d multi-item, %d one-price) for a list refused before it", len(prov.creates), prov.subs)
				}
				noSubscriptionFor(t, store, c.partition, "c2")
				noSubscriptionFor(t, store, "acme", "c2")
			})
		})
	}
}

// A provider refusal writes nothing.
func TestService_SubscribeItemsWritesNothingWhenTheProviderRefuses(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		catalogWithTwoPrices(t, store)
		refused := errors.New("card declined")
		prov := &itemsCreatingProvider{err: refused}
		svc := newService(t, store, prov)
		_, _, err := svc.SubscribeItems(ctx, "acme", "c2", "cus_2",
			[]SubscriptionItem{{PriceID: "p_plan", Quantity: 1}, {PriceID: "p_addon", Quantity: 3}}, "")
		if !errors.Is(err, refused) {
			t.Fatalf("SubscribeItems = %v, want the provider's refusal", err)
		}
		noSubscriptionFor(t, store, "acme", "c2")
	})
}

// The rows carry what the provider answered, in its order and at its
// quantities, not what the caller asked for.
func TestService_SubscribeItemsStoresTheProvidersAnswerNotTheRequest(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		catalogWithTwoPrices(t, store)
		prov := &itemsCreatingProvider{answer: SubscriptionResult{
			ProviderRef: "sub_new", Status: "trialing",
			Items: []SubscriptionItem{{PriceRef: "price_plan", Quantity: 1}, {PriceRef: "price_addon", Quantity: 4}},
		}}
		svc := newService(t, store, prov)
		row, _, err := svc.SubscribeItems(ctx, "acme", "c2", "cus_2",
			[]SubscriptionItem{{PriceID: "p_plan", Quantity: 1}, {PriceID: "p_addon", Quantity: 3}}, "")
		if err != nil {
			t.Fatalf("SubscribeItems: %v", err)
		}
		stored, err := store.ListSubscriptionItems(ctx, "acme", row.ID)
		if err != nil {
			t.Fatalf("ListSubscriptionItems: %v", err)
		}
		want := []SubscriptionItem{{PriceID: "p_plan", Quantity: 1}, {PriceID: "p_addon", Quantity: 4}}
		if !itemsEqual(stored, want) {
			t.Fatalf("stored items = %+v, want the provider's %+v", stored, want)
		}
		if got := mustGet(t, store, "acme", row.ID); got.Status != "trialing" {
			t.Fatalf("stored status %q, want the provider's trialing", got.Status)
		}
	})
}

// A provider answer naming a price the request did not ask for is not stored:
// the rows never carry an item the catalog cannot name. A provider answering
// no item list confirms the create of what it is asked, and those items are
// the rows. A status outside the provider's set is refused as Subscribe
// refuses it.
func TestService_SubscribeItemsReadsTheProvidersAnswerByTheCatalog(t *testing.T) {
	asked := []SubscriptionItem{{PriceID: "p_plan", Quantity: 1}, {PriceID: "p_addon", Quantity: 3}}
	t.Run("a price nobody asked for", func(t *testing.T) {
		eachStore(t, func(t *testing.T, store Store) {
			catalogWithTwoPrices(t, store)
			prov := &itemsCreatingProvider{answer: SubscriptionResult{ProviderRef: "sub_new", Status: "active",
				Items: []SubscriptionItem{{PriceRef: "price_plan", Quantity: 1}, {PriceRef: "price_other", Quantity: 3}}}}
			_, _, err := newService(t, store, prov).SubscribeItems(ctx, "acme", "c2", "cus_2", asked, "")
			if !errors.Is(err, ErrProviderItemsUnresolved) {
				t.Fatalf("SubscribeItems = %v, want ErrProviderItemsUnresolved", err)
			}
			noSubscriptionFor(t, store, "acme", "c2")
		})
	})
	t.Run("no item list", func(t *testing.T) {
		eachStore(t, func(t *testing.T, store Store) {
			catalogWithTwoPrices(t, store)
			prov := &itemsCreatingProvider{answer: SubscriptionResult{ProviderRef: "sub_new", Status: "active"}}
			row, items, err := newService(t, store, prov).SubscribeItems(ctx, "acme", "c2", "cus_2", asked, "")
			if err != nil {
				t.Fatalf("SubscribeItems: %v", err)
			}
			stored, err := store.ListSubscriptionItems(ctx, "acme", row.ID)
			if err != nil {
				t.Fatalf("ListSubscriptionItems: %v", err)
			}
			if !itemsEqual(items, asked) || !itemsEqual(stored, asked) {
				t.Fatalf("answered %+v, stored %+v, want the items asked %+v", items, stored, asked)
			}
		})
	})
	t.Run("a status outside the set", func(t *testing.T) {
		eachStore(t, func(t *testing.T, store Store) {
			catalogWithTwoPrices(t, store)
			prov := &itemsCreatingProvider{answer: SubscriptionResult{ProviderRef: "sub_new", Status: "suspended"}}
			_, _, err := newService(t, store, prov).SubscribeItems(ctx, "acme", "c2", "cus_2", asked, "")
			if !errors.Is(err, ErrInvalidSubscriptionStatus) {
				t.Fatalf("SubscribeItems = %v, want ErrInvalidSubscriptionStatus", err)
			}
			noSubscriptionFor(t, store, "acme", "c2")
		})
	})
}

// The store writes the row and every item in one write: the row carries the
// first item's price and quantity whatever the caller set on it, every item is
// a row in the order given, and a list with no item or with a price named twice
// writes nothing at all, row included.
func TestCreateSubscriptionWithItems_WritesTheRowAndEveryItemInOneWrite(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		sub := &Subscription{Partition: "acme", CustomerID: "c1", PriceID: "stale", Quantity: 9, ProviderRef: "sub_1", Status: "active"}
		items := []SubscriptionItem{{PriceID: "p_plan", Quantity: 2}, {PriceID: "p_addon", Quantity: 5}}
		if err := s.CreateSubscriptionWithItems(ctx, sub, items); err != nil {
			t.Fatalf("CreateSubscriptionWithItems: %v", err)
		}
		got := mustGet(t, s, "acme", sub.ID)
		if got.PriceID != "p_plan" || got.Quantity != 2 || got.Status != "active" || got.StatusChangedAt.IsZero() {
			t.Fatalf("row = %+v, want active on the first item (p_plan, 2) with its status instant", got)
		}
		stored, err := s.ListSubscriptionItems(ctx, "acme", sub.ID)
		if err != nil {
			t.Fatalf("ListSubscriptionItems: %v", err)
		}
		if !itemsEqual(stored, items) {
			t.Fatalf("stored items = %+v, want %+v", stored, items)
		}

		for name, c := range map[string]struct {
			items []SubscriptionItem
			want  error
		}{
			"no items":    {nil, ErrNoSubscriptionItems},
			"price twice": {[]SubscriptionItem{{PriceID: "p_plan", Quantity: 1}, {PriceID: "p_plan", Quantity: 1}}, ErrInvalidSubscriptionItem},
		} {
			bad := &Subscription{Partition: "acme", CustomerID: "c9", ProviderRef: "sub_9", Status: "active"}
			if err := s.CreateSubscriptionWithItems(ctx, bad, c.items); !errors.Is(err, c.want) {
				t.Fatalf("%s: CreateSubscriptionWithItems = %v, want %v", name, err, c.want)
			}
			noSubscriptionFor(t, s, "acme", "c9")
		}
	})
}
