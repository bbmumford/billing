# billing

A scope-agnostic billing system: customers, a product and price catalogue, subscriptions and their items, invoices, an idempotency-keyed payment ledger, metered usage, credits, payment methods and discounts, kept in a store you supply under an opaque partition.

- **Provider first.** A `Service` puts a pluggable `PaymentProvider` in front of the store: each payment effect happens at the provider, and the provider's answer is what gets stored. The package pulls in no payment SDK; every provider identifier is one opaque `ProviderRef`.
- **Money is integers.** Amounts are `int64` minor units (cents) plus a currency, never a float.
- **Sealed at rest.** Sensitive ledger fields (a charge reference, a client secret) are sealed through an `Encryptor` you supply.
- **Partition is opaque.** The store filters on it and never interprets it; an empty partition matches nothing.

## Use

```sh
go get github.com/bbmumford/billing
```

```go
store := billing.NewSQLStore(db) // any database/sql driver; the application registers it
if err := store.InitSchema(ctx); err != nil { /* ... */ } // idempotent; once at boot

svc, err := billing.NewService(billing.Config{
	Store:     store,     // or billing.NewMemStore()
	Provider:  provider,  // your PaymentProvider, e.g. a Stripe adapter
	Encryptor: encryptor, // seals ledger secrets
})

row, clientSecret, err := svc.Charge(ctx, billing.ChargeRequest{
	Partition:      partition,
	CustomerID:     customerID,
	CustomerRef:    customerRef,
	AmountCents:    1500,
	Currency:       "aud",
	IdempotencyKey: key,
})
```

A `PaymentProvider` creates customers, payment intents and subscriptions and cancels subscriptions. Payment methods, multi-item subscriptions, subscription updates and discounts are separate optional interfaces (`PaymentMethodProvider`, `SubscriptionItemsCreator`, `SubscriptionUpdater`, `DiscountApplier`) a provider implements when it supports them.

## Consumers

ORBTR library (its billing host serves the billing RPCs), ai cortex and the api.orbtr.ai endpoint, and io's app.orbtr.io endpoint and tools.
