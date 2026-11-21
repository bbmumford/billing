package billing

import (
	"errors"
	"sort"
	"strings"
)

// ErrStatusOutOfOrder is UpdateInvoiceStatus's and UpdatePaymentStatus's refusal of a status that
// does not follow the one the row holds (statusOrder.follows); nothing is written. The provider
// does not order its deliveries, so an event can arrive after a later one: a payment_failed
// redelivered after paid would turn a paid invoice back to failed. The billing host's invoice
// applier acknowledges the event on it and logs it, so the provider stops redelivering an event
// that is not to be applied.
var ErrStatusOutOfOrder = errors.New("billing: status does not follow the status the row holds")

// statusOrder is the order a row's status moves in: each status has a rank, a status outside the
// table ranks as unknown, and a status of rank final or above is an outcome. A status follows the
// stored one when it is the same status, or when the stored one is not an outcome and the new one
// ranks at or above it. Statuses of one rank move among themselves both ways, as the provider moves
// them while a payment is attempted again, and an outcome is never replaced.
type statusOrder struct {
	rank    map[string]int
	unknown int
	final   int
}

// invoiceStatusOrder is the provider's invoice lifecycle: a draft is finalised to open; an open
// invoice (payment_failed is an open invoice whose attempt failed) is paid, voided or marked
// uncollectible; an uncollectible invoice can still be paid or voided; paid and void are outcomes.
// UpdateInvoiceStatus holds it.
var invoiceStatusOrder = statusOrder{
	rank:    map[string]int{"draft": 0, "open": 1, "payment_failed": 1, "uncollectible": 2, "paid": 3, "void": 3},
	unknown: 1,
	final:   3,
}

// paymentStatusOrder is the provider's payment-intent lifecycle: every status short of an outcome
// is one rank, since a failed attempt returns the intent to requires_payment_method; succeeded and
// canceled are outcomes. UpdatePaymentStatus holds it.
var paymentStatusOrder = statusOrder{
	rank: map[string]int{
		"requires_payment_method": 1, "requires_confirmation": 1, "requires_action": 1,
		"processing": 1, "requires_capture": 1, "succeeded": 2, "canceled": 2,
	},
	unknown: 1,
	final:   2,
}

func (o statusOrder) of(status string) int {
	if r, ok := o.rank[status]; ok {
		return r
	}
	return o.unknown
}

// follows reports whether a row holding stored may take next. MemStore's status writes call it and
// SQLStore's write predicate (guard) is built from it, so the two stores hold one order.
func (o statusOrder) follows(stored, next string) bool {
	if stored == next {
		return true
	}
	if o.of(stored) >= o.final {
		return false
	}
	return o.of(next) >= o.of(stored)
}

// guard is the SQL predicate over the status column that holds exactly when next follows the
// stored status, with its arguments: the stored statuses of the table next follows (status IN) or
// the ones it does not (status NOT IN) when next also follows a status outside the table. It lets
// SQLStore's status write test and write the row in one statement.
func (o statusOrder) guard(next string) (string, []any) {
	known := make([]string, 0, len(o.rank))
	for st := range o.rank {
		known = append(known, st)
	}
	sort.Strings(known)
	var allowed, refused []any
	for _, st := range known {
		if o.follows(st, next) {
			allowed = append(allowed, st)
		} else {
			refused = append(refused, st)
		}
	}
	// A stored status outside the table ranks unknown, which is never an outcome, and is next
	// itself when next is outside the table too.
	if _, inTable := o.rank[next]; !inTable || o.of(next) >= o.unknown {
		if len(refused) == 0 {
			return "1 = 1", nil
		}
		return "status NOT IN (" + placeholders(len(refused)) + ")", refused
	}
	if len(allowed) == 0 {
		return "1 = 0", nil
	}
	return "status IN (" + placeholders(len(allowed)) + ")", allowed
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
