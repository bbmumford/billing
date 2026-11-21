package billing

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
)

// ErrIdempotencyKeyRequired is Charge's refusal of a request with no idempotency key, and the
// stores' refusal of a ledger row with none; nothing is written and the provider is not asked.
// The key is what lets a retried charge find the one already made, so without it every retry
// would create another provider payment intent. The billing host's CreatePaymentIntent answers
// the same refusal before it reads anything.
var ErrIdempotencyKeyRequired = errors.New("billing: idempotency key required")

// ErrIdempotencyConflict is Charge's refusal of a request whose key the partition's ledger
// already holds for a charge to another customer, of another amount or in another currency.
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
// Charge and Subscribe hand it to the provider.
func providerIdempotencyKey(operation, partition, key string) string {
	if key == "" {
		return ""
	}
	h := sha256.New()
	for _, field := range []string{operation, partition, key} {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(field)))
		h.Write(n[:])
		h.Write([]byte(field))
	}
	return "billing-" + operation + "-" + hex.EncodeToString(h.Sum(nil))
}

// sameCharge reports whether a stored ledger row records the charge req asks for: the same
// customer, amount and currency, the currency compared without case because the provider reads
// it without case. Charge answers a stored row for a retried key only when it does, and
// ErrIdempotencyConflict otherwise.
func sameCharge(row *PaymentLedgerRow, req ChargeRequest) bool {
	return row.CustomerID == req.CustomerID && row.AmountCents == req.AmountCents &&
		strings.EqualFold(row.Currency, req.Currency)
}
