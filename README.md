# billing

billing is a billing system: customers, a product and price catalogue, subscriptions and their items, invoices, an idempotency-keyed payment ledger, metered usage, credits, payment methods and discounts, kept in a store you supply under an opaque partition.

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

## Dependencies

No other package of this estate, and no external module in its production
code: `SQLStore` runs on any `database/sql` driver the application opens.
Tests only: `modernc.org/sqlite`, the pure-Go SQLite driver.

## Used by

Importers in the ORBTR estate:

- `github.com/ORBTR/Library/domain/billinghost` (with `kvencryptor` and
  `stripeprovider`) and `github.com/ORBTR/Library/billinghold`: the billing
  host that serves the billing RPCs, its Stripe provider and its encryptor.
- `github.com/ORBTR/cortex/billing/billing_profile/bridge` and
  `billing/planprice`.
- `github.com/ORBTR/api.orbtr.ai`, `github.com/ORBTR/app.orbtr.io/handlers` and
  `github.com/ORBTR/tools/schemaref/manifest`.

## Platforms and build

Pure Go, no cgo and no build tags. The module's `go` directive is 1.25.0.

## Licence

MIT. Copyright (c) 2026 Brandon Miller-Mumford. The full text is in
[LICENSE](LICENSE).

## Contributing

[AGENTS.md](AGENTS.md) holds the repository's two rules, for people and coding
agents alike: no temporary documents in the repository, and conventional commit
messages that stand on their own. [CLAUDE.md](CLAUDE.md) imports it for Claude
Code.
