package billing

import (
	"context"
	"errors"
	"math"
)

// Discount is a catalogue entry that mirrors a provider coupon / promotion code.
// DiscountID doubles as the provider coupon id — the apply path attaches it
// directly. Value is a percent (5.0 = 5%) for Type "percentage" or an amount in
// cents for "fixed_amount". Partition is CatalogPartition: a discount is a
// platform-wide catalog row every partition applies, and the stores refuse one
// under any other partition (ErrCatalogPartition). The catalogue is populated
// out of band (mirrored from the provider); billing only reads it here.
type Discount struct {
	DiscountID  string
	Partition   string
	Code        string
	Name        string
	Type        string // "percentage" | "fixed_amount"
	Value       float64
	AppliesTo   string
	MaxUses     int64 // 0 = unlimited
	CurrentUses int64
	Active      bool
}

// AppliedDiscount records that a discount was attached to a subscription — the
// idempotency anchor for ApplyDiscount. SubscriptionRef is the opaque provider
// subscription reference the caller supplied.
type AppliedDiscount struct {
	ID              string
	Partition       string
	SubscriptionRef string
	DiscountID      string
}

// ErrDiscountNotFound is returned when a discount id is not in the catalogue (a
// local-catalogue miss; the provider coupon may still exist).
var ErrDiscountNotFound = errors.New("billing: discount not found")

// ErrDiscountInactive is returned when a discount exists but is not active.
var ErrDiscountInactive = errors.New("billing: discount is inactive")

// ErrDiscountAlreadyApplied indicates the (subscription, discount) pair is
// already recorded — callers map it to an idempotent success.
var ErrDiscountAlreadyApplied = errors.New("billing: discount already applied")

// ErrDiscountPartition is Store.UpsertDiscount's refusal of a discount id
// another partition holds; nothing is written. The id is the provider coupon
// reference and the table's key, so an upsert that took the row over would move
// the discount, its uses and its limit out of the partition that holds them.
var ErrDiscountPartition = errors.New("billing: the discount id is held by another partition")

// ErrDiscountSubscription is ApplyDiscount's refusal of a subscription
// reference that is not the provider reference of a subscription the customer
// holds in the partition; no use is taken and the provider is not asked.
var ErrDiscountSubscription = errors.New("billing: the subscription is not the customer's in the partition")

// ErrDiscountExhausted is RecordAppliedDiscount's and ApplyDiscount's refusal
// of a use past a discount's MaxUses; nothing is recorded and the provider is
// not asked to attach the coupon.
var ErrDiscountExhausted = errors.New("billing: discount has reached its use limit")

// DiscountApplier is an OPTIONAL PaymentProvider extension that attaches a
// provider coupon to a subscription (or, when no subscription is given, to the
// customer). Service type-asserts on it and fails closed (ErrProviderUnsupported)
// when the wired provider does not implement it, so the core PaymentProvider is
// unchanged.
type DiscountApplier interface {
	AttachDiscount(ctx context.Context, in DiscountAttachInput) (appliedRef string, err error)
}

// DiscountAttachInput carries the neutral fields of a coupon attach. When
// SubscriptionRef is set the coupon attaches to that subscription; otherwise it
// attaches customer-wide to CustomerRef. IdempotencyKey dedupes a provider retry.
type DiscountAttachInput struct {
	CustomerRef     string
	SubscriptionRef string
	CouponRef       string
	IdempotencyKey  string
}

// ComputeDiscountAmountCents answers the minor units a discount takes off a
// subtotal of subtotalCents, in int64 minor units like every amount here. A
// percentage value is a percent (5.0 = 5%), read as whole basis points (33.3 =
// 3330), and the subtotal's share is computed in integers and rounded to the
// nearest unit, a half up; a fixed_amount value is minor units, rounded the same
// way. The result is clamped to [0, subtotalCents]: a value that is negative or
// not a number takes nothing off, and one past the subtotal takes the whole
// subtotal, so a mis-seeded discount never adds to the bill or exceeds it.
// Returns 0 for a nil discount, an unknown type or a subtotal below one.
func ComputeDiscountAmountCents(d *Discount, subtotalCents int64) int64 {
	if d == nil || subtotalCents <= 0 || !(d.Value > 0) {
		return 0
	}
	switch d.Type {
	case "percentage":
		if d.Value >= 100 {
			return subtotalCents
		}
		bp := int64(math.Round(d.Value * 100))
		// subtotal × bp / 10000 split at 10000 so no product passes int64: the
		// quotient's share is exact and the remainder's is rounded.
		q, r := subtotalCents/10000, subtotalCents%10000
		return q*bp + (r*bp+5000)/10000
	case "fixed_amount":
		if d.Value >= float64(subtotalCents) {
			return subtotalCents
		}
		return int64(math.Round(d.Value))
	default:
		return 0
	}
}

// ApplyDiscountResult reports the outcome of an apply. AlreadyApplied is true when
// the (subscription, discount) pair was already recorded. The amount fields are
// populated only when a subtotal is supplied to a future amount-aware caller;
// today's caller reads success/idempotency alone, so they default to zero.
type ApplyDiscountResult struct {
	AlreadyApplied     bool
	AppliedAmountCents int32
	NewSubtotalCents   int32
}

// ApplyDiscount validates the discount, read from the platform-wide catalog
// (CatalogPartition) whatever the partition, enforces subscription-scoped
// idempotency, takes a use of the discount and records the application
// (Store.RecordAppliedDiscount), then attaches the provider coupon. It is
// idempotent: a repeat for an already-applied (subscription, discount) returns
// AlreadyApplied without a second provider attach. The use is taken before the
// attach so a discount at its MaxUses is refused with ErrDiscountExhausted
// without asking the provider, a customer-wide application counting as a use
// as a subscription's does; an attach the provider refuses gives the use and
// the pair back (Store.ReleaseAppliedDiscount), so the next attempt starts
// from the limit as it was. A subscriptionRef must be the provider reference of
// a subscription the customer holds in the partition (ErrDiscountSubscription
// otherwise, before any use is taken), so a caller cannot attach its discount
// to another customer's or another partition's subscription. The provider is
// handed the key derived from the partition and the caller's key
// (providerIdempotencyKey), as Charge and Subscribe hand it theirs, so two
// partitions' applications under one key never meet at the provider's one
// account. Fails closed when no DiscountApplier is wired.
// TODO: a release that fails after a refused attach keeps the use and the
// pair, so a retry answers AlreadyApplied with no coupon at the provider.
func (s *Service) ApplyDiscount(ctx context.Context, partition, customerID, subscriptionRef, discountID, idempotencyKey string) (ApplyDiscountResult, error) {
	d, err := s.store.GetDiscount(ctx, CatalogPartition, discountID)
	if err != nil {
		return ApplyDiscountResult{}, err
	}
	if !d.Active {
		return ApplyDiscountResult{}, ErrDiscountInactive
	}
	da, ok := s.provider.(DiscountApplier)
	if !ok {
		return ApplyDiscountResult{}, ErrProviderUnsupported
	}
	cust, err := s.store.GetCustomer(ctx, partition, customerID)
	if err != nil {
		return ApplyDiscountResult{}, err
	}
	if subscriptionRef != "" {
		// The provider attaches to whatever subscription it is named, so the reference is
		// held to one the customer has in the partition before anything is recorded.
		sub, err := s.storedSubscription(ctx, partition, customerID, subscriptionRef)
		if err != nil {
			return ApplyDiscountResult{}, err
		}
		if sub == nil {
			return ApplyDiscountResult{}, ErrDiscountSubscription
		}
		// Subscription-scoped idempotency probe: skip a second attach on a retry.
		already, err := s.store.IsDiscountApplied(ctx, partition, subscriptionRef, discountID)
		if err != nil {
			return ApplyDiscountResult{}, err
		}
		if already {
			return ApplyDiscountResult{AlreadyApplied: true}, nil
		}
	}
	if subscriptionRef == "" && cust.ProviderRef == "" {
		return ApplyDiscountResult{}, errors.New("billing: customer has no provider account for a customer-wide discount")
	}
	if _, err := s.store.RecordAppliedDiscount(ctx, partition, subscriptionRef, discountID); err != nil {
		if errors.Is(err, ErrDiscountAlreadyApplied) {
			return ApplyDiscountResult{AlreadyApplied: true}, nil
		}
		return ApplyDiscountResult{}, err
	}
	if _, err := da.AttachDiscount(ctx, DiscountAttachInput{
		CustomerRef:     cust.ProviderRef,
		SubscriptionRef: subscriptionRef,
		CouponRef:       discountID,
		IdempotencyKey:  providerIdempotencyKey("discount", partition, idempotencyKey),
	}); err != nil {
		return ApplyDiscountResult{}, errors.Join(err, s.store.ReleaseAppliedDiscount(ctx, partition, subscriptionRef, discountID))
	}
	return ApplyDiscountResult{}, nil
}
