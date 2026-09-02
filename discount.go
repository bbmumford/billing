package billing

import (
	"context"
	"errors"
)

// Discount is a catalogue entry that mirrors a provider coupon / promotion code.
// DiscountID doubles as the provider coupon id — the apply path attaches it
// directly. Value is a percent (5.0 = 5%) for Type "percentage" or an amount in
// cents for "fixed_amount". Partition is the opaque scope the store filters on;
// "" is a global/all-scope discount. The catalogue is populated out of band
// (mirrored from the provider); billing only reads it here.
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

// ComputeDiscountAmountCents applies a discount's value to a subtotal and returns
// the cents saved. A percentage value is a percent (5.0 = 5%); a fixed_amount
// value is cents. Both are clamped to the subtotal so a mis-seeded discount can
// never exceed the bill. Returns 0 for a nil discount or non-positive subtotal.
func ComputeDiscountAmountCents(d *Discount, subtotalCents int32) int32 {
	if d == nil || subtotalCents <= 0 {
		return 0
	}
	switch d.Type {
	case "percentage":
		amount := int32((d.Value / 100.0) * float64(subtotalCents))
		if amount > subtotalCents {
			amount = subtotalCents
		}
		return amount
	case "fixed_amount":
		amount := int32(d.Value)
		if amount > subtotalCents {
			amount = subtotalCents
		}
		return amount
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

// ApplyDiscount validates the catalogue discount, enforces subscription-scoped
// idempotency, attaches the provider coupon, and records the application. It is
// idempotent: a repeat for an already-applied (subscription, discount) returns
// AlreadyApplied without a second provider attach. Fails closed when no
// DiscountApplier is wired.
func (s *Service) ApplyDiscount(ctx context.Context, partition, customerID, subscriptionRef, discountID, idempotencyKey string) (ApplyDiscountResult, error) {
	d, err := s.store.GetDiscount(ctx, partition, discountID)
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
	// Subscription-scoped idempotency probe: skip a second attach on a retry.
	if subscriptionRef != "" {
		already, err := s.store.IsDiscountApplied(ctx, partition, subscriptionRef, discountID)
		if err != nil {
			return ApplyDiscountResult{}, err
		}
		if already {
			return ApplyDiscountResult{AlreadyApplied: true}, nil
		}
	}
	cust, err := s.store.GetCustomer(ctx, partition, customerID)
	if err != nil {
		return ApplyDiscountResult{}, err
	}
	if subscriptionRef == "" && cust.ProviderRef == "" {
		return ApplyDiscountResult{}, errors.New("billing: customer has no provider account for a customer-wide discount")
	}
	if _, err := da.AttachDiscount(ctx, DiscountAttachInput{
		CustomerRef:     cust.ProviderRef,
		SubscriptionRef: subscriptionRef,
		CouponRef:       discountID,
		IdempotencyKey:  idempotencyKey,
	}); err != nil {
		return ApplyDiscountResult{}, err
	}
	if subscriptionRef != "" {
		if _, err := s.store.RecordAppliedDiscount(ctx, partition, subscriptionRef, discountID); err != nil {
			if errors.Is(err, ErrDiscountAlreadyApplied) {
				return ApplyDiscountResult{AlreadyApplied: true}, nil
			}
			return ApplyDiscountResult{}, err
		}
	}
	return ApplyDiscountResult{}, nil
}
