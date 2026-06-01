package exec

// constraints.go — ConstraintRegistry and enforcement helpers (task-296).
//
// ConstraintRegistry holds the active set of UNIQUE and NOT NULL constraints.
// It is consulted by write operators before every mutation to detect violations
// early. The registry is thread-safe: concurrent reads (CheckSetProperty) are
// non-blocking; writes (Register/Unregister) acquire a write lock.
//
// # Unique constraint backing
//
// A UNIQUE constraint is backed by a hash.Index[string] registered in the
// index.Manager under the synthetic name "__uniq__<label>.<prop>". The hash
// index is also used directly from the registry's own value-set to track
// which values are in use. The value-set is updated via RecordPropertySet
// after a successful write; CheckSetProperty consults it before writing.
//
// The hash.Index[V] in the standard library does not implement Apply (it is a
// no-op), so constraint tracking does NOT rely on the Change-event fanout
// path. Instead the registry maintains its own string value-set per constraint
// key, keeping the unique check accurate across RunInTx calls.
//
// # NOT NULL enforcement
//
// A NOT NULL constraint is stored as a boolean flag keyed by "label.prop".
// Before a property write the registry checks whether the proposed value is
// the zero PropertyValue (Kind == 0), which represents null in the lpg type
// system.
//
// # Concurrency
//
// ConstraintRegistry is safe for concurrent use.

import (
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// ConstraintKind distinguishes UNIQUE from NOT_NULL constraints.
type ConstraintKind uint8

const (
	// ConstraintUnique requires that at most one node with a given label has a
	// particular value for the constrained property.
	ConstraintUnique ConstraintKind = iota
	// ConstraintNotNull requires that every node with a given label has a
	// non-null value for the constrained property.
	ConstraintNotNull
)

// ErrConstraintViolation is the sentinel returned (wrapped) by
// CheckSetProperty when a write would violate a constraint.
var ErrConstraintViolation = errors.New("exec: constraint violation")

// ConstraintViolationError carries structured context about which constraint
// was violated.
type ConstraintViolationError struct {
	// Label is the node label the constraint is defined on.
	Label string
	// Property is the constrained property key.
	Property string
	// Kind describes the type of constraint: "UNIQUE" or "NOT NULL".
	Kind string
	// Detail is an optional human-readable explanation.
	Detail string
}

// Error implements the error interface.
func (e *ConstraintViolationError) Error() string {
	return fmt.Sprintf("exec: constraint violation: %s constraint on (%s).%s: %s",
		e.Kind, e.Label, e.Property, e.Detail)
}

// Unwrap chains to ErrConstraintViolation so callers can use errors.Is.
func (e *ConstraintViolationError) Unwrap() error { return ErrConstraintViolation }

// ─────────────────────────────────────────────────────────────────────────────
// ConstraintRegistry
// ─────────────────────────────────────────────────────────────────────────────

// ConstraintRegistry is a thread-safe registry of active constraints. It
// stores unique and not-null constraints keyed by "label.prop".
//
// ConstraintRegistry is safe for concurrent use.
type ConstraintRegistry struct {
	mu        sync.RWMutex
	unique    map[string]string              // "label.prop" → index name
	notNull   map[string]bool                // "label.prop" → true
	valueSets map[string]map[string]struct{} // "label.prop" → set of string values in use
}

// NewConstraintRegistry creates an empty ConstraintRegistry.
func NewConstraintRegistry() *ConstraintRegistry {
	return &ConstraintRegistry{
		unique:    make(map[string]string),
		notNull:   make(map[string]bool),
		valueSets: make(map[string]map[string]struct{}),
	}
}

// constraintKey returns the canonical map key for (label, prop).
func constraintKey(label, prop string) string { return label + "." + prop }

// RegisterUnique adds a unique constraint for (label, prop) backed by
// indexName in the index.Manager.
func (r *ConstraintRegistry) RegisterUnique(label, prop, indexName string) {
	r.mu.Lock()
	key := constraintKey(label, prop)
	r.unique[key] = indexName
	if r.valueSets[key] == nil {
		r.valueSets[key] = make(map[string]struct{})
	}
	r.mu.Unlock()
}

// RegisterNotNull adds a not-null constraint for (label, prop).
func (r *ConstraintRegistry) RegisterNotNull(label, prop string) {
	r.mu.Lock()
	r.notNull[constraintKey(label, prop)] = true
	r.mu.Unlock()
}

// UnregisterUnique removes the unique constraint for (label, prop). No-op if
// absent.
func (r *ConstraintRegistry) UnregisterUnique(label, prop string) {
	r.mu.Lock()
	key := constraintKey(label, prop)
	delete(r.unique, key)
	delete(r.valueSets, key)
	r.mu.Unlock()
}

// UnregisterNotNull removes the not-null constraint for (label, prop). No-op
// if absent.
func (r *ConstraintRegistry) UnregisterNotNull(label, prop string) {
	r.mu.Lock()
	delete(r.notNull, constraintKey(label, prop))
	r.mu.Unlock()
}

// UniqueIndexName returns the backing index name for a unique constraint on
// (label, prop), or ("", false) if none exists.
func (r *ConstraintRegistry) UniqueIndexName(label, prop string) (string, bool) {
	r.mu.RLock()
	name, ok := r.unique[constraintKey(label, prop)]
	r.mu.RUnlock()
	return name, ok
}

// HasNotNull reports whether a not-null constraint exists for (label, prop).
func (r *ConstraintRegistry) HasNotNull(label, prop string) bool {
	r.mu.RLock()
	ok := r.notNull[constraintKey(label, prop)]
	r.mu.RUnlock()
	return ok
}

// CheckSetProperty validates that setting prop = value on a node with the
// given labels does not violate any registered constraint. mgr is used for
// unique-constraint index lookups (hash index Cardinality check) as a
// secondary source; the primary source is the registry's own value set.
//
// Returns *ConstraintViolationError (which wraps ErrConstraintViolation) on
// the first violation found; nil when all constraints pass.
func (r *ConstraintRegistry) CheckSetProperty(labels []string, prop string, value lpg.PropertyValue, mgr *index.Manager) error {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, label := range labels {
		key := constraintKey(label, prop)

		// NOT NULL check: zero PropertyValue (Kind == 0) is null.
		if r.notNull[key] && value.Kind() == 0 {
			return &ConstraintViolationError{
				Label:    label,
				Property: prop,
				Kind:     "NOT NULL",
				Detail:   "value is null",
			}
		}

		// UNIQUE check: consult the registry's own value set first (always
		// up-to-date), then the backing hash index as a secondary source.
		if indexName, ok := r.unique[key]; ok {
			_ = indexName // used for the secondary check below

			// Primary: check the in-memory value set.
			if vs := r.valueSets[key]; vs != nil {
				strVal := propertyValueToString(value)
				if strVal != "" {
					if _, exists := vs[strVal]; exists {
						return &ConstraintViolationError{
							Label:    label,
							Property: prop,
							Kind:     "UNIQUE",
							Detail:   fmt.Sprintf("value %q already exists", strVal),
						}
					}
				}
			}

			// Secondary: also check via the hash index cardinality (covers
			// values that were inserted before this Engine instance started, e.g.
			// via direct index manipulation in tests).
			if mgr != nil {
				sub, err := mgr.GetIndex(indexName)
				if err == nil {
					if checkUniqueViolation(sub, value) {
						return &ConstraintViolationError{
							Label:    label,
							Property: prop,
							Kind:     "UNIQUE",
							Detail:   fmt.Sprintf("value already exists in index %q", indexName),
						}
					}
				}
			}
		}
	}
	return nil
}

// RecordPropertySet records that a property value has been successfully
// written to a node with the given labels. This keeps the unique value sets
// up-to-date so that subsequent CheckSetProperty calls detect violations.
// It is a no-op when no unique constraint exists for (label, prop).
func (r *ConstraintRegistry) RecordPropertySet(labels []string, prop string, value lpg.PropertyValue) {
	strVal := propertyValueToString(value)
	if strVal == "" {
		return
	}
	r.mu.Lock()
	for _, label := range labels {
		key := constraintKey(label, prop)
		if vs := r.valueSets[key]; vs != nil {
			vs[strVal] = struct{}{}
		}
	}
	r.mu.Unlock()
}

// propertyValueToString converts a PropertyValue to its canonical string
// representation for use as a value-set key. Returns "" for unsupported kinds.
func propertyValueToString(value lpg.PropertyValue) string {
	switch value.Kind() {
	case lpg.PropString:
		s, _ := value.String()
		return s
	case lpg.PropInt64:
		i, _ := value.Int64()
		return fmt.Sprintf("\x00int\x00%d", i) // namespace to avoid string/int collision
	}
	return ""
}

// ListConstraintRows returns a [][]expr.Value where each inner slice has four
// elements: [name, type, label, property]. The name column uses the canonical
// "label.prop" key; type is "UNIQUE" or "NOT_NULL". Rows are returned in
// deterministic lexicographic order.
//
// ListConstraintRows is safe for concurrent use.
func (r *ConstraintRegistry) ListConstraintRows() [][]expr.Value {
	r.mu.RLock()
	rows := make([][]expr.Value, 0, len(r.unique)+len(r.notNull))
	for key := range r.unique {
		label, prop := splitConstraintKey(key)
		rows = append(rows, []expr.Value{
			expr.StringValue(key),
			expr.StringValue("UNIQUE"),
			expr.StringValue(label),
			expr.StringValue(prop),
		})
	}
	for key := range r.notNull {
		label, prop := splitConstraintKey(key)
		rows = append(rows, []expr.Value{
			expr.StringValue(key),
			expr.StringValue("NOT_NULL"),
			expr.StringValue(label),
			expr.StringValue(prop),
		})
	}
	r.mu.RUnlock()

	// Sort for deterministic output.
	sort.Slice(rows, func(i, j int) bool {
		ki := rows[i][0].(expr.StringValue)
		kj := rows[j][0].(expr.StringValue)
		if ki != kj {
			return string(ki) < string(kj)
		}
		ti := rows[i][1].(expr.StringValue)
		tj := rows[j][1].(expr.StringValue)
		return string(ti) < string(tj)
	})
	return rows
}

// splitConstraintKey splits a "label.prop" key into its two parts.
// If there is no dot, label is the full key and prop is "".
func splitConstraintKey(key string) (label, prop string) {
	for i := len(key) - 1; i >= 0; i-- {
		if key[i] == '.' {
			return key[:i], key[i+1:]
		}
	}
	return key, ""
}

// ─────────────────────────────────────────────────────────────────────────────
// unique index lookup helpers
// ─────────────────────────────────────────────────────────────────────────────

// hashStringCardinality is satisfied by hash.Index[string]: it reports the
// number of nodes that carry a given string value. Using Cardinality avoids
// a bitmap clone allocation compared to Lookup.
type hashStringCardinality interface {
	Cardinality(value string) uint64
}

// hashInt64Cardinality is satisfied by hash.Index[int64].
type hashInt64Cardinality interface {
	Cardinality(value int64) uint64
}

// checkUniqueViolation returns true when the hash index subscriber already
// contains value (i.e. at least one node holds that property value).
func checkUniqueViolation(sub index.Subscriber, value lpg.PropertyValue) bool {
	switch value.Kind() {
	case lpg.PropString:
		s, _ := value.String()
		if sc, ok := sub.(hashStringCardinality); ok {
			return sc.Cardinality(s) > 0
		}
	case lpg.PropInt64:
		i, _ := value.Int64()
		if ic, ok := sub.(hashInt64Cardinality); ok {
			return ic.Cardinality(i) > 0
		}
	}
	return false
}
