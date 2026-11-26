package billing

import "errors"

// ErrNoPartition is every Store write's refusal of a row carrying no partition
// (customers, catalog rows, subscriptions, invoices, payments, usage, credits,
// payment methods), and Service.EnsureCustomer's of a customer with none: every
// read refuses the empty partition before comparing, so a row written there could
// never be read, listed, corrected or removed through the store. Nothing is
// written. The billing host refuses an empty sealed partition before calling the
// Service, so the refusal here guards a caller holding the Service or the store
// directly.
var ErrNoPartition = errors.New("billing: no partition")

// ErrCustomerExists is answered by Store.CreateCustomer when the partition already
// holds a customer for the subject: one customer per (partition, subject) is the
// store's rule, and Service.EnsureCustomer reads it as a concurrent caller's create
// having landed first, re-reads the pair and carries on with the stored row.
var ErrCustomerExists = errors.New("billing: the partition already holds a customer for the subject")
