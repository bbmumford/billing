package billing

import (
	"context"
	"testing"
)

// Product and Price carry opaque consumer metadata because a payment provider
// echoes those key/values back on its own records, and a webhook naming only the
// provider's id is matched to a local row through them. A store that accepts the
// metadata and does not return it breaks that match with nothing failing here —
// so the round trip is asserted, not the write.
func TestProductAndPriceMetadataRoundTrip(t *testing.T) {
	eachStore(t, func(t *testing.T, st Store) {
		{
			ctx := context.Background()
			meta := map[string]string{"role": "worker_addon", "canonical_id": "prod_canonical", "adr": "0024"}

			p := &Product{ID: "prod1", Partition: CatalogPartition, ServiceID: "svc", Name: "Addon", Metadata: meta}
			if err := st.CreateProduct(ctx, p); err != nil {
				t.Fatalf("CreateProduct: %v", err)
			}
			got, err := st.GetProduct(ctx, CatalogPartition, "prod1")
			if err != nil {
				t.Fatalf("GetProduct: %v", err)
			}
			for k, want := range meta {
				if got.Metadata[k] != want {
					t.Errorf("product metadata %q = %q, want %q (whole map: %v)", k, got.Metadata[k], want, got.Metadata)
				}
			}

			pr := &Price{ID: "price1", Partition: CatalogPartition, ProductID: "prod1", Currency: "usd",
				AmountCents: 1000, Interval: "month", Metadata: map[string]string{"lookup": "worker_monthly"}}
			if err := st.CreatePrice(ctx, pr); err != nil {
				t.Fatalf("CreatePrice: %v", err)
			}
			gotPr, err := st.GetPrice(ctx, CatalogPartition, "price1")
			if err != nil {
				t.Fatalf("GetPrice: %v", err)
			}
			if gotPr.Metadata["lookup"] != "worker_monthly" {
				t.Errorf("price metadata lost: %v", gotPr.Metadata)
			}
		}
	})
}

// A product written with no metadata must read back usable, not nil-panic on
// index. The column defaults to '{}' for exactly this.
func TestAbsentMetadataIsSafeToRead(t *testing.T) {
	eachStore(t, func(t *testing.T, st Store) {
		ctx := context.Background()
		if err := st.CreateProduct(ctx, &Product{ID: "p", Partition: CatalogPartition, Name: "n"}); err != nil {
			t.Fatalf("CreateProduct: %v", err)
		}
		got, err := st.GetProduct(ctx, CatalogPartition, "p")
		if err != nil {
			t.Fatalf("GetProduct: %v", err)
		}
		if v := got.Metadata["anything"]; v != "" {
			t.Errorf("absent metadata key returned %q, want empty", v)
		}
	})
}
