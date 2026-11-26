package billing

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
)

// ErrIdempotencyKeyRequired is Charge's refusal of a request with no idempotency key, the
// stores' refusal of a ledger row with none, and Store.RecordUsage's of a usage record with
// neither a key nor an ID; nothing is written and the provider is not asked.
// The key is what lets a retried charge find the one already made, so without it every retry
// would create another provider payment intent. The billing host's CreatePaymentIntent answers
// the same refusal before it reads anything.
var ErrIdempotencyKeyRequired = errors.New("billing: idempotency key required")

// ErrIdempotencyConflict is Charge's refusal of a request whose key the partition's ledger
// already holds for a charge to another customer, of another amount or in another currency, and
// Store.RecordUsage's of a usage key the partition holds for another customer, product or quantity.
// Neither the stored row nor its client secret is answered: a key reused for a different
// charge would otherwise hand the caller the secret that confirms a payment it did not ask for.
var ErrIdempotencyConflict = errors.New("billing: idempotency key reused for a different charge")

// providerIdempotencyKey is the idempotency key a Service call hands the payment provider for
// the caller's key within a partition: "billing-<operation>-" and the hex SHA-256 of the
// operation, the partition and the key, each length-prefixed so no two triples share bytes. The
// provider dedupes by its key across the one account every partition shares, so the caller's
// key alone would let two partitions' requests carrying the same key meet at the provider, the
// second answered with the first's intent or refused; the derived key keeps each partition's
// retries to itself, keeps an operation's keys apart from another's, and names no partition to
// the provider. An empty key answers "", which asks the provider for no deduplication.
// Charge, Subscribe, SubscribeItems and ApplyDiscount hand it to the provider.
func providerIdempotencyKey(operation, partition, key string) string {
	if key == "" {
		return ""
	}
	return "billing-" + operation + "-" + keyDigest(operation, partition, key)
}

// usageRecordID is the ID Store.RecordUsage gives a usage record from its partition and
// idempotency key, "usage-" and the hex digest of both (keyDigest): one report and its retries
// share it, so the record's primary key holds a retry to one row, and two partitions' reports
// under one key are two rows.
func usageRecordID(partition, key string) string {
	return "usage-" + keyDigest("usage", partition, key)
}

// keyDigest is the hex SHA-256 of an operation, a partition and a caller's key, each
// length-prefixed so no two triples share bytes; providerIdempotencyKey and usageRecordID are its
// callers.
func keyDigest(operation, partition, key string) string {
	h := sha256.New()
	for _, field := range []string{operation, partition, key} {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(field)))
		h.Write(n[:])
		h.Write([]byte(field))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// sameUsage reports whether a stored usage record is the report u retries: the same customer,
// product and quantity. RecordUsage answers the stored record for a retried key only when it is,
// and ErrIdempotencyConflict otherwise.
func sameUsage(stored, u *UsageRecord) bool {
	return stored.CustomerID == u.CustomerID && stored.ProductID == u.ProductID && stored.Quantity == u.Quantity
}

// sameCharge reports whether a stored ledger row records the charge req asks for: the same
// customer, amount and currency, the currency compared without case because the provider reads
// it without case. Charge answers a stored row for a retried key only when it does, and
// ErrIdempotencyConflict otherwise.
func sameCharge(row *PaymentLedgerRow, req ChargeRequest) bool {
	return row.CustomerID == req.CustomerID && row.AmountCents == req.AmountCents &&
		strings.EqualFold(row.Currency, req.Currency)
}
