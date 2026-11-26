package billing

import (
	"context"
	"testing"
)

// inputRecordingProvider is a test PaymentProvider that keeps the
// SubscriptionInput it is handed, so a test can read what Subscribe tells the
// provider about the subscription.
type inputRecordingProvider struct {
	fakeProvider
	inputs []SubscriptionInput
}

func (p *inputRecordingProvider) CreateSubscription(ctx context.Context, in SubscriptionInput) (SubscriptionResult, error) {
	p.inputs = append(p.inputs, in)
	return p.fakeProvider.CreateSubscription(ctx, in)
}

// Subscribe hands the provider the billing profile the subscription pays for,
// so the rail can stamp it on the provider subscription; a subscription for no
// profile hands an empty value.
func TestSubscribe_HandsTheBillingProfileToTheProvider(t *testing.T) {
	for _, profile := range []string{"bp_1", ""} {
		t.Run("profile "+profile, func(t *testing.T) {
			p := &inputRecordingProvider{}
			store := NewMemStore()
			seedCatalogPrice(t, store, "price1", "price_ref_1")
			s := newService(t, store, p)
			sub, err := s.Subscribe(context.Background(), "acme", "c1", "cus_1", "price1", "price_ref_1", 2, profile, "")
			if err != nil {
				t.Fatalf("Subscribe: %v", err)
			}
			if len(p.inputs) != 1 {
				t.Fatalf("provider asked %d times, want once", len(p.inputs))
			}
			want := SubscriptionInput{CustomerRef: "cus_1", PriceRef: "price_ref_1", Quantity: 2, BillingProfileID: profile}
			if p.inputs[0] != want {
				t.Fatalf("provider input = %+v, want %+v", p.inputs[0], want)
			}
			if sub.ProviderRef != "sub_1" || sub.Quantity != 2 || sub.PriceID != "price1" {
				t.Fatalf("stored subscription = %+v, want the provider's ref with the price and quantity", sub)
			}
		})
	}
}
