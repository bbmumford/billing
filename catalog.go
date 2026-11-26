package billing

import "errors"

// CatalogPartition is the one partition the platform-wide catalog's rows carry:
// every product, price and discount is stored under it, and every partition
// reads the catalog through it. A partition's own rows never hold a catalog
// entry, so a partition resolves a plan's product, its prices and a plan
// discount with no copy of any of them. Service reads the catalog under it
// (SubscribeItems, Subscribe and UpdateSubscription for prices, ApplyDiscount
// for the discount), and the billing host's GetProduct, ListProducts and price
// lookups read under it whatever partition the caller is sealed to. The
// composition that seeds the catalog writes under it; the stores refuse a
// catalog row under any other partition (ErrCatalogPartition).
const CatalogPartition = "platform.catalog"

// ErrCatalogPartition is the stores' refusal of a catalog write (CreateProduct,
// CreatePrice, SetPriceProviderRef, UpsertDiscount) under any partition but
// CatalogPartition; nothing is written. Every read of the catalog is under
// CatalogPartition, so a row written under a partition would be a copy no read
// resolves and a partition could otherwise write the catalog every partition
// reads.
var ErrCatalogPartition = errors.New("billing: the catalog is written only under CatalogPartition")
