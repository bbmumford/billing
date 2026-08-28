package billing

import "errors"

// ErrInvalidSubscriptionStatus is returned when a subscription status outside the allowed set is used.
var ErrInvalidSubscriptionStatus = errors.New("billing: invalid subscription status")

// validSubscriptionStatuses is the allowed lifecycle set (from the source domain). Kept as data, not an
// enum type, so the Subscription.Status field stays an opaque string the store never interprets while
// the service still rejects an unknown status.
var validSubscriptionStatuses = map[string]bool{
	"incomplete":        true,
	"trialing":          true,
	"active":            true,
	"past_due":          true,
	"downgrade_pending": true,
	"paused":            true,
	"canceled":          true,
}

// ValidSubscriptionStatus reports whether a status is in the allowed lifecycle set.
func ValidSubscriptionStatus(status string) bool { return validSubscriptionStatuses[status] }

// ComputeItemAmount returns the line total for a quantity at a unit price, in minor units. Kept as a
// named helper so money math is one reviewed place, never an inline float.
func ComputeItemAmount(quantity, unitAmountCents int64) int64 { return quantity * unitAmountCents }
