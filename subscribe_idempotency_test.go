package billing

import (
	"context"
	"fmt"
	"testing"
)

// keyedSubscribeProvider answers a create the way the provider answers a keyed retry: a key it
// has already been handed is answered the subscription it created for that key, and any other
// create, an unkeyed one among them, makes a new subscription. It records every key it is handed.
type keyedSubscribeProvider struct {
	fakeProvider
	created int
	byKey   map[string]SubscriptionResult
	keys    []string
}

func (p *keyedSubscribeProvider) create(in SubscriptionInput, items []SubscriptionItem) SubscriptionResult {
	p.keys = append(p.keys, in.IdempotencyKey)
	if r, ok := p.byKey[in.IdempotencyKey]; ok && in.IdempotencyKey != "" {
		return r
	}
	p.created++
	r := SubscriptionResult{ProviderRef: fmt.Sprintf("sub_%d", p.created), Status: "active"}
	for _, it := range items {
		r.Items = append(r.Items, SubscriptionItem{PriceRef: it.PriceRef, Quantity: it.Quantity})
	}
	if p.byKey == nil {
		p.byKey = map[string]SubscriptionResult{}
	}
	if in.IdempotencyKey != "" {
		p.byKey[in.IdempotencyKey] = r
	}
	return r
}

func (p *keyedSubscribeProvider) CreateSubscription(_ context.Context, in SubscriptionInput) (SubscriptionResult, error) {
	return p.create(in, nil), nil
}

func (p *keyedSubscribeProvider) CreateSubscriptionWithItems(_ context.Context, in SubscriptionInput, items []SubscriptionItem) (SubscriptionResult, error) {
	return p.create(in, items), nil
}

// A retried Subscribe with the caller's key hands the provider the same derived key, so the
// provider creates one subscription, and is answered the row the first call stored rather than
// storing a second.
func TestSubscribe_ARetryWithTheSameKeyMakesOneSubscription(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		prov := &keyedSubscribeProvider{}
		svc := newService(t, store, prov)
		seedCatalogPrice(t, store, "price1", "price_ref_1")
		first, err := svc.Subscribe(ctx, "acme", "c1", "cus_1", "price1", "price_ref_1", 2, "", "signup-1")
		if err != nil {
			t.Fatal(err)
		}
		again, err := svc.Subscribe(ctx, "acme", "c1", "cus_1", "price1", "price_ref_1", 2, "", "signup-1")
		if err != nil {
			t.Fatal(err)
		}
		want := providerIdempotencyKey("subscribe", "acme", "signup-1")
		if len(prov.keys) != 2 || prov.keys[0] != want || prov.keys[1] != want {
			t.Fatalf("provider keys = %q, want %q twice", prov.keys, want)
		}
		if prov.created != 1 {
			t.Fatalf("provider subscriptions created = %d, want 1", prov.created)
		}
		if again.ID != first.ID || again.ProviderRef != first.ProviderRef {
			t.Fatalf("retry answered %+v, want the first call's row %+v", again, first)
		}
		if rows, _ := store.ListSubscriptionsByCustomer(ctx, "acme", "c1"); len(rows) != 1 {
			t.Fatalf("local rows = %d, want 1", len(rows))
		}
		// Another key is another create: its own provider subscription and its own row.
		other, err := svc.Subscribe(ctx, "acme", "c1", "cus_1", "price1", "price_ref_1", 2, "", "signup-2")
		if err != nil || other.ID == first.ID || other.ProviderRef == first.ProviderRef || prov.created != 2 {
			t.Fatalf("a second key answered %+v, %v with %d created; want its own subscription", other, err, prov.created)
		}
		if rows, _ := store.ListSubscriptionsByCustomer(ctx, "acme", "c1"); len(rows) != 2 {
			t.Fatalf("local rows after a second key = %d, want 2", len(rows))
		}
	})
}

// The derived key belongs to the partition: another partition's Subscribe with the same caller
// key hands the provider a different key and gets its own subscription.
func TestSubscribe_TheKeyIsThePartitions(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		prov := &keyedSubscribeProvider{}
		svc := newService(t, store, prov)
		seedCatalogPrice(t, store, "price1", "price_ref_1")
		a, err := svc.Subscribe(ctx, "acme", "c1", "cus_1", "price1", "price_ref_1", 1, "", "signup-1")
		if err != nil {
			t.Fatal(err)
		}
		b, err := svc.Subscribe(ctx, "globex", "c1", "cus_1", "price1", "price_ref_1", 1, "", "signup-1")
		if err != nil {
			t.Fatal(err)
		}
		if prov.keys[0] == prov.keys[1] || prov.created != 2 || a.ProviderRef == b.ProviderRef || b.Partition != "globex" {
			t.Fatalf("keys %q, created %d, rows %+v %+v; want two partitions' own subscriptions", prov.keys, prov.created, a, b)
		}
	})
}

// A caller with no key hands the provider no key: the provider dedupes nothing, so each call is
// its own subscription, and no two unkeyed creates of a partition share a derived key.
func TestSubscribe_AnUnkeyedCallHandsTheProviderNoKey(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		prov := &keyedSubscribeProvider{}
		svc := newService(t, store, prov)
		seedCatalogPrice(t, store, "price1", "price_ref_1")
		for i := 0; i < 2; i++ {
			if _, err := svc.Subscribe(ctx, "acme", "c1", "cus_1", "price1", "price_ref_1", 1, "", ""); err != nil {
				t.Fatal(err)
			}
		}
		if len(prov.keys) != 2 || prov.keys[0] != "" || prov.keys[1] != "" || prov.created != 2 {
			t.Fatalf("keys %q, created %d; want two unkeyed creates", prov.keys, prov.created)
		}
	})
}

// SubscribeItems holds the same rule for a subscription of several items: one provider
// subscription for a retried key, the first call's row and items answered to the retry.
func TestSubscribeItems_ARetryWithTheSameKeyMakesOneSubscription(t *testing.T) {
	eachStore(t, func(t *testing.T, store Store) {
		catalogWithTwoPrices(t, store)
		prov := &keyedSubscribeProvider{}
		svc := newService(t, store, prov)
		seedCatalogPrice(t, store, "price1", "price_ref_1")
		asked := []SubscriptionItem{{PriceID: "p_plan", Quantity: 1}, {PriceID: "p_addon", Quantity: 3}}
		first, firstItems, err := svc.SubscribeItems(ctx, "acme", "c2", "cus_2", asked, "bp_1", "plan-change-7")
		if err != nil {
			t.Fatal(err)
		}
		again, againItems, err := svc.SubscribeItems(ctx, "acme", "c2", "cus_2", asked, "bp_1", "plan-change-7")
		if err != nil {
			t.Fatal(err)
		}
		want := providerIdempotencyKey("subscribe", "acme", "plan-change-7")
		if prov.created != 1 || len(prov.keys) != 2 || prov.keys[0] != want || prov.keys[1] != want {
			t.Fatalf("created %d, keys %q; want one subscription under %q", prov.created, prov.keys, want)
		}
		if again.ID != first.ID || !itemsEqual(againItems, firstItems) || len(againItems) != 2 {
			t.Fatalf("retry answered %+v %+v, want the first row %+v %+v", again, againItems, first, firstItems)
		}
		if rows, _ := store.ListSubscriptionsByCustomer(ctx, "acme", "c2"); len(rows) != 1 {
			t.Fatalf("local rows = %d, want 1", len(rows))
		}
		single, _, err := svc.SubscribeItems(ctx, "acme", "c3", "cus_3", asked[:1], "", "plan-change-8")
		if err != nil || prov.keys[2] != providerIdempotencyKey("subscribe", "acme", "plan-change-8") || single.ProviderRef == first.ProviderRef {
			t.Fatalf("one-item create = %+v %v, key %q; want its own keyed subscription", single, err, prov.keys[2])
		}
	})
}
