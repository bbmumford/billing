package billing

import "errors"

// ErrNoPartition is answered by Service.EnsureCustomer for a customer carrying no
// partition: the pair (partition, subject) that keys a customer has no first half,
// and every selector reads no row under the empty partition, so a row written there
// could never be found again or carry the provider reference set on it. The
// billing host refuses an empty sealed partition before calling the Service, so the
// refusal here guards a caller holding the Service directly.
var ErrNoPartition = errors.New("billing: no partition")

// ErrCustomerExists is answered by Store.CreateCustomer when the partition already
// holds a customer for the subject: one customer per (partition, subject) is the
// store's rule, and Service.EnsureCustomer reads it as a concurrent caller's create
// having landed first, re-reads the pair and carries on with the stored row.
var ErrCustomerExists = errors.New("billing: the partition already holds a customer for the subject")
