package lpg

import "sync/atomic"

// SchemaValidator is the interface that schema enforcement hooks implement.
// It is satisfied by *schema.Schema[…] after properties have been registered.
//
// Validate receives the property name and the value about to be written.
// A nil return allows the write; a non-nil return rejects it with the
// returned error, leaving the graph state unchanged.
//
// Implementations must be safe for concurrent use.
type SchemaValidator interface {
	Validate(propertyName string, value PropertyValue) error
}

// validatorHolder wraps a SchemaValidator pointer for atomic swap.
// Using an interface value inside atomic.Pointer avoids the need for
// a separate indirection struct.
type validatorHolder struct {
	v SchemaValidator
}

// atomicValidator provides lock-free get/set for an optional SchemaValidator.
type atomicValidator struct {
	p atomic.Pointer[validatorHolder]
}

// load returns the current validator, or nil when none is set.
func (a *atomicValidator) load() SchemaValidator {
	h := a.p.Load()
	if h == nil {
		return nil
	}
	return h.v
}

// store installs v as the current validator. Passing nil clears it.
func (a *atomicValidator) store(v SchemaValidator) {
	if v == nil {
		a.p.Store(nil)
		return
	}
	a.p.Store(&validatorHolder{v: v})
}
