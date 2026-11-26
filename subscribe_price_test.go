package billing

import (
	"context"
	"errors"
	"testing"
)

// seedCatalogPrice stores the catalog price id selling provider price ref.
func seedCatalogPrice(t *testing.T, s Store, id, ref string) {
	t.Helper()
	if err := s.CreatePrice(context.Background(), &Price{ID: id, Partition: CatalogPartition, ProductID: "prod", ProviderRef: ref, Currency: "aud", AmountCents: 100}); err != nil {
		t.Fatalf("CreatePrice(%s): %v", id, err)
	}
}

// subscribeCountingProvider is the fakeProvider counting the subscriptions it
// is asked to create and keeping the price each names.
type subscribeCountingProvider struct {
	fakeProvider
	asked []string
}

func (p *subscribeCountingProvider) CreateSubscription(ctx context.Context, in SubscriptionInput) (SubscriptionResult, error) {
	p.asked = append(p.asked, in.PriceRef)
	return p.fakeProvider.CreateSubscription(ctx, in)
}

// Subscribe reads its price from the catalog before the provider is asked: a
// price the catalog does not hold, one with no provider price and a provider
// price that is not the catalog price's are refused without asking the
// provider, and an empty provider price asks for the catalog price's own.
func TestSubscribeResolvesItsPriceThroughTheCatalog(t *testing.T) {
	ctx := context.Background()
	eachStore(t, func(t *testing.T, s Store) {
		seedCatalogPrice(t, s, "pro", "price_pro")
		seedCatalogPrice(t, s, "unsold", "")
		prov := &subscribeCountingProvider{}
		svc := newService(t, s, prov)
		for _, tc := range []struct {
			priceID, priceRef string
			want              error
		}{
			{"absent", "price_x", ErrNotFound},
			{"unsold", "", ErrNoProviderPrice},
			{"pro", "price_enterprise", ErrPriceRefMismatch},
		} {
			if _, err := svc.Subscribe(ctx, "pa", "c1", "cus_1", tc.priceID, tc.priceRef, 1, "", "k-"+tc.priceID); !errors.Is(err, tc.want) {
				t.Errorf("Subscribe(%s, %q) = %v, want %v", tc.priceID, tc.priceRef, err, tc.want)
			}
		}
		if len(prov.asked) != 0 {
			t.Fatalf("the provider was asked for %q", prov.asked)
		}
		if _, err := svc.Subscribe(ctx, "pa", "c1", "cus_1", "pro", "", 1, "", "k-pro"); err != nil {
			t.Fatalf("Subscribe(pro) = %v", err)
		}
		if len(prov.asked) != 1 || prov.asked[0] != "price_pro" {
			t.Fatalf("provider asked for %q, want the catalog price's price_pro", prov.asked)
		}
	})
}
