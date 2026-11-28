# Changelog

## [0.2.0](https://github.com/bbmumford/billing/compare/v0.1.0...v0.2.0) (2026-11-28)


### ⚠ BREAKING CHANGES

* RecordUsage refuses a record with neither an IdempotencyKey nor an ID (ErrIdempotencyKeyRequired).
* Store.ListInvoiceItems takes the partition.
* CreateProduct, CreatePrice, SetPriceProviderRef and UpsertDiscount refuse any partition but CatalogPartition (ErrCatalogPartition), and the Service reads prices and discounts under CatalogPartition, so a caller writes its catalog rows there.

### Bug Fixes

* a discount upsert never moves a discount another partition holds, and a rewrite keeps the uses already taken ([635b58a](https://github.com/bbmumford/billing/commit/635b58a21fe6ecf6e4ada8d3ddc7688bb315ba53))
* a discount use is given back only against the record of its taking, and a customer-wide use is recorded under the customer ([642f107](https://github.com/bbmumford/billing/commit/642f1077627beb50aa8d90d22703e23e1449c8ac))
* a usage report is stored under an id derived from its partition and idempotency key, so a retried report counts once ([84c8e7e](https://github.com/bbmumford/billing/commit/84c8e7ee9a0263402251ab14763eefc59fe31d5c))
* ApplyDiscount hands the provider a key derived from the partition and the caller's key, and attaches only to a subscription the customer holds in the partition ([726afaf](https://github.com/bbmumford/billing/commit/726afafd5fe2e62331f11457929ea226118bba86))
* both stores refuse a product, price, invoice, usage record or credit whose id they already hold, with ErrDuplicateID ([e4ea228](https://github.com/bbmumford/billing/commit/e4ea228fa5ab32146754df7d46e6da651a5e4f02))
* every store write refuses a row with no partition with ErrNoPartition and stores nothing, since no read can reach a row kept under the empty partition ([9253f3b](https://github.com/bbmumford/billing/commit/9253f3b5b5872d990d685279182b6932332b00f2))
* invoice lines are listed only within the partition that holds the invoice ([8e909d5](https://github.com/bbmumford/billing/commit/8e909d5772aa9630c8ceee16c41039436022de5d))
* Subscribe reads its price from the catalog before asking the provider, and refuses a provider price that is not the catalog price's ([cc6474d](https://github.com/bbmumford/billing/commit/cc6474dd8bb07489be174a5fa9a528ecec293166))
* the catalog is platform-wide: products, prices and discounts are stored and read under CatalogPartition, which every partition reads ([bd57667](https://github.com/bbmumford/billing/commit/bd5766726e9d668951671a8d0954a3611f3f4d71))
