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
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"sync"
	"time"

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
	// uniqueNames / notNullNames carry the user-defined constraint name per
	// (label, prop) key, tracked separately per kind because a UNIQUE and a
	// NOT NULL constraint may coexist on the same key. The name is needed so a
	// constraint round-trips durably through the WAL / snapshot with the name
	// the client declared. An entry is absent for a constraint registered
	// without a name (the legacy [RegisterUnique] / [RegisterNotNull] path).
	uniqueNames  map[string]string // "label.prop" → constraint name (UNIQUE)
	notNullNames map[string]string // "label.prop" → constraint name (NOT NULL)
}

// NewConstraintRegistry creates an empty ConstraintRegistry.
func NewConstraintRegistry() *ConstraintRegistry {
	return &ConstraintRegistry{
		unique:       make(map[string]string),
		notNull:      make(map[string]bool),
		valueSets:    make(map[string]map[string]struct{}),
		uniqueNames:  make(map[string]string),
		notNullNames: make(map[string]string),
	}
}

// ConstraintInfo is a structured description of one registered constraint,
// used to persist the constraint set durably and to re-register it on
// recovery. KindUnique distinguishes UNIQUE (true) from NOT NULL (false).
type ConstraintInfo struct {
	// KindUnique is true for a UNIQUE constraint, false for NOT NULL.
	KindUnique bool
	// Label is the constrained node label.
	Label string
	// Property is the constrained property key.
	Property string
	// Name is the user-defined constraint name (may be empty for a constraint
	// registered without one).
	Name string
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

// HasUnique reports whether a unique constraint exists for (label, prop).
func (r *ConstraintRegistry) HasUnique(label, prop string) bool {
	r.mu.RLock()
	_, ok := r.unique[constraintKey(label, prop)]
	r.mu.RUnlock()
	return ok
}

// SetConstraintName records the user-defined name of the constraint of the
// given kind on (label, prop), so the constraint round-trips durably with the
// name the client declared. kindUnique selects UNIQUE (true) vs NOT NULL. A
// later [UnregisterUnique] / [UnregisterNotNull] clears the matching name.
func (r *ConstraintRegistry) SetConstraintName(kindUnique bool, label, prop, name string) {
	if name == "" {
		return
	}
	r.mu.Lock()
	if kindUnique {
		r.uniqueNames[constraintKey(label, prop)] = name
	} else {
		r.notNullNames[constraintKey(label, prop)] = name
	}
	r.mu.Unlock()
}

// Constraints returns a structured snapshot of every registered constraint, in
// deterministic order (UNIQUE before NOT NULL, then by label, property, name).
// It is used to persist the constraint set into a snapshot and to compare the
// recovered set against the live one.
//
// Constraints is safe for concurrent use.
func (r *ConstraintRegistry) Constraints() []ConstraintInfo {
	r.mu.RLock()
	out := make([]ConstraintInfo, 0, len(r.unique)+len(r.notNull))
	for key := range r.unique {
		label, prop := splitConstraintKey(key)
		out = append(out, ConstraintInfo{KindUnique: true, Label: label, Property: prop, Name: r.uniqueNames[key]})
	}
	for key := range r.notNull {
		label, prop := splitConstraintKey(key)
		out = append(out, ConstraintInfo{KindUnique: false, Label: label, Property: prop, Name: r.notNullNames[key]})
	}
	r.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.KindUnique != b.KindUnique {
			return a.KindUnique // UNIQUE (true) sorts first
		}
		if a.Label != b.Label {
			return a.Label < b.Label
		}
		if a.Property != b.Property {
			return a.Property < b.Property
		}
		return a.Name < b.Name
	})
	return out
}

// SeedUniqueValues populates the value-set of an already-registered UNIQUE
// constraint on (label, prop) from the property values of the nodes that
// currently carry the label. It is the post-creation seed that makes a
// constraint added to a non-empty dataset functional: without it the value-set
// starts empty and pre-existing duplicates (or duplicates of a pre-existing
// value) are accepted on the next write.
//
// It also enforces the at-creation invariant (Neo4j semantics, audit gap H2):
// if two of the supplied values are equal it returns a
// *ConstraintViolationError wrapping [ErrConstraintViolation] and seeds
// nothing, so the caller can reject CREATE CONSTRAINT over already-duplicated
// data. Null values (the zero PropertyValue) are ignored by a UNIQUE
// constraint and are skipped.
//
// SeedUniqueValues is a no-op (and returns nil) when no UNIQUE constraint is
// registered for (label, prop).
func (r *ConstraintRegistry) SeedUniqueValues(label, prop string, values []lpg.PropertyValue) error {
	key := constraintKey(label, prop)

	// Build the seed set outside the lock so the duplicate check does not hold
	// the registry write lock during the O(n) scan of the candidate values.
	seed := make(map[string]struct{}, len(values))
	for i := range values {
		strVal, ok := propertyValueToString(values[i])
		if !ok {
			continue // null: not constrained by UNIQUE.
		}
		if _, dup := seed[strVal]; dup {
			return &ConstraintViolationError{
				Label:    label,
				Property: prop,
				Kind:     "UNIQUE",
				Detail:   fmt.Sprintf("pre-existing data contains duplicate value %q", strVal),
			}
		}
		seed[strVal] = struct{}{}
	}

	r.mu.Lock()
	r.mergeSeed(key, seed)
	r.mu.Unlock()
	return nil
}

// SeedUniqueValuesIgnoringDuplicates seeds the value-set of an
// already-registered UNIQUE constraint on (label, prop) from values WITHOUT
// rejecting pre-existing duplicates. It is the recovery seed: recovery must
// always succeed so the store is serviceable, and a duplicate that predates the
// constraint is a historical artefact the live enforcement path still rejects
// on the next write. Null values are skipped. No-op when no UNIQUE constraint
// is registered for (label, prop).
func (r *ConstraintRegistry) SeedUniqueValuesIgnoringDuplicates(label, prop string, values []lpg.PropertyValue) {
	key := constraintKey(label, prop)
	seed := make(map[string]struct{}, len(values))
	for i := range values {
		if strVal, ok := propertyValueToString(values[i]); ok {
			seed[strVal] = struct{}{}
		}
	}
	r.mu.Lock()
	r.mergeSeed(key, seed)
	r.mu.Unlock()
}

// mergeSeed merges seed into the value-set for key, creating the value-set when
// the key names a registered UNIQUE constraint that has none yet. It is a no-op
// when key is not a registered UNIQUE constraint. Callers hold r.mu.
func (r *ConstraintRegistry) mergeSeed(key string, seed map[string]struct{}) {
	vs := r.valueSets[key]
	if vs == nil {
		if _, ok := r.unique[key]; !ok {
			return // not a registered UNIQUE constraint
		}
		vs = make(map[string]struct{}, len(seed))
		r.valueSets[key] = vs
	}
	for v := range seed {
		vs[v] = struct{}{}
	}
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
	delete(r.uniqueNames, key)
	r.mu.Unlock()
}

// UnregisterNotNull removes the not-null constraint for (label, prop). No-op
// if absent.
func (r *ConstraintRegistry) UnregisterNotNull(label, prop string) {
	r.mu.Lock()
	key := constraintKey(label, prop)
	delete(r.notNull, key)
	delete(r.notNullNames, key)
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
				if strVal, ok := propertyValueToString(value); ok {
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
	strVal, ok := propertyValueToString(value)
	if !ok {
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

// propertyValueToString converts a PropertyValue to a canonical string key
// for use in a unique value-set. The second return is false when the value
// carries no enforceable identity — the zero PropertyValue (null), which a
// UNIQUE constraint does not constrain (null-handling is the NOT NULL
// constraint's job) — so callers skip the uniqueness check for it.
//
// Every non-null kind produces a key, and the key is namespaced by a
// kind-specific tag so two values of different kinds never collide (e.g. the
// string "1", the integer 1, and the float 1.0 map to three distinct keys).
// The encoding is injective within a kind: integers and times use their exact
// integral representation, floats use their IEEE-754 bit pattern (so +0/-0 and
// every NaN payload are distinguished, matching value identity rather than
// numeric equality), bytes use base64. This mirrors the canonical property
// encoding the snapshot layer uses (store/snapshot/properties.go), so a
// constraint enforced in memory and one re-seeded from a recovered graph agree
// on what counts as a duplicate.
func propertyValueToString(value lpg.PropertyValue) (string, bool) {
	switch value.Kind() {
	case lpg.PropString:
		s, _ := value.String()
		return "\x00s\x00" + s, true
	case lpg.PropInt64:
		i, _ := value.Int64()
		return "\x00i\x00" + strconv.FormatInt(i, 10), true
	case lpg.PropFloat64:
		f, _ := value.Float64()
		// IEEE-754 bit pattern: injective over all float64 values (including
		// the sign of zero and every NaN payload), unlike %g which collapses
		// them.
		return "\x00f\x00" + strconv.FormatUint(math.Float64bits(f), 16), true
	case lpg.PropBool:
		b, _ := value.Bool()
		if b {
			return "\x00b\x001", true
		}
		return "\x00b\x000", true
	case lpg.PropTime:
		t, _ := value.Time()
		// RFC3339Nano is injective for the wall-clock instants the engine
		// stores and is independent of the *Location pointer identity, so two
		// equal instants compare equal regardless of how they were parsed.
		return "\x00t\x00" + t.UTC().Format(time.RFC3339Nano), true
	case lpg.PropBytes:
		raw, _ := value.Bytes()
		return "\x00x\x00" + base64.StdEncoding.EncodeToString(raw), true
	case lpg.PropList:
		elems, _ := value.List()
		return encodeListKey(elems), true
	}
	// Zero PropertyValue (Kind == 0): null. Not subject to a UNIQUE check.
	return "", false
}

// encodeListKey builds an injective canonical key for a PropList by joining
// its elements' own canonical keys with a separator the element encoding
// cannot itself produce, so two distinct lists never share a key. A null
// element (the zero PropertyValue) is encoded with a dedicated marker so a
// list containing a null is distinguished from a shorter list.
func encodeListKey(elems []lpg.PropertyValue) string {
	var b []byte
	b = append(b, "\x00l\x00"...)
	b = strconv.AppendInt(b, int64(len(elems)), 10)
	for _, e := range elems {
		b = append(b, 0x1f) // unit separator: not produced by the element keys
		if k, ok := propertyValueToString(e); ok {
			b = append(b, k...)
		} else {
			b = append(b, "\x00n\x00"...) // explicit null-element marker
		}
	}
	return string(b)
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
//
// This is a SECONDARY check: the backing hash index only ever carries string
// or int64 keys (the kinds [graph/index/hash] indexes), so only those two
// cases can match here. For every other [lpg.PropertyKind] — float, bool,
// time, bytes, list — the registry's own value-set ([CheckSetProperty]'s
// primary check) is the sole authority, and it covers all kinds via
// [propertyValueToString]. Returning false for the non-indexed kinds is
// therefore correct, not a gap: the primary check has already caught (or
// cleared) the value before this secondary lookup runs.
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
