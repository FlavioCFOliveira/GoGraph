package exec

// constraints.go — ConstraintRegistry and enforcement helpers (task-296).
//
// ConstraintRegistry holds the active set of UNIQUE and NOT NULL constraints.
// It is consulted by write operators before every mutation to detect violations
// early. The registry is thread-safe via a single RWMutex: reads
// (CheckSetProperty, HasUnique, …) take the read lock and run concurrently with
// each other; writes (Register/Unregister) take the write lock and exclude both.
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
	"sync/atomic"
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

// ErrConstraintNotFound is the sentinel returned (wrapped) when a
// DROP CONSTRAINT names a constraint that does not exist and IF EXISTS was not
// given. It is the fail-stop analogue of Neo4j's ConstraintDropFailed / "No
// such constraint": the drop reports a typed error rather than fail-silently
// claiming success.
var ErrConstraintNotFound = errors.New("exec: constraint not found")

// ErrConstraintAlreadyExists is the sentinel returned (wrapped) when CREATE
// CONSTRAINT (without IF NOT EXISTS) names a constraint whose (kind, label,
// property) identity is already registered. It is the analogue of Neo4j's
// EquivalentSchemaRuleAlreadyExists / ConstraintAlreadyExists — a constraint-
// level fault surfaced instead of leaking the synthetic backing-index name.
var ErrConstraintAlreadyExists = errors.New("exec: constraint already exists")

// ErrConstraintNameConflict is the sentinel returned (wrapped) when CREATE
// CONSTRAINT requests a name already held by a DIFFERENT constraint (a distinct
// kind/label/property). Neo4j requires constraint names to be unique across the
// database; this is the analogue of its ConstraintWithNameAlreadyExists.
var ErrConstraintNameConflict = errors.New("exec: constraint name already in use")

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
// ConstraintTxn
// ─────────────────────────────────────────────────────────────────────────────

// ckeyval identifies one (label, property, value) reservation.
type ckeyval struct {
	key ckey
	val string
}

// ConstraintTxn is one transaction's UNCOMMITTED contribution to the UNIQUE
// value-sets: the values it has RELEASED and not yet committed.
//
// # Why a release is deferred and a reservation is not (rmp #2366)
//
// The two directions are not symmetric, and treating them as if they were is the
// defect. A RESERVATION is taken eagerly into the shared value-set, and that is
// correct: it must block every peer immediately, and rolling it back — deleting a
// value no committed peer can have taken, because it was reserved throughout —
// cannot disturb anyone. A RELEASE is different. Applied eagerly it hands the value
// to any peer that asks, and if the releasing transaction then rolls back the value
// has two holders; so the old code applied it eagerly and journaled the RE-RESERVE
// as the rollback inverse, which put the value back into SHARED state judged
// against the rolling-back transaction's OWN view:
//
//	T1  REMOVE b:Person          releases 'old' (eagerly, shared)
//	T2  SET b.email = 'new'      releases 'old', reserves 'new'
//	T2  ROLLBACK                 replays: releases 'new', RE-RESERVES 'old'
//	T1  COMMIT                   the label removal stands — no live :Person holds 'old'
//	    CREATE (y:Person {email:'old'})  -> REFUSED, for ever
//
// Both transactions behaved correctly in isolation; the merged state is wrong
// because a rollback wrote to state a peer's COMMITTED release had already changed.
// Deferring the release removes the write: a rollback drops a private mark and
// touches nothing shared, so there is nothing to order against a peer's commit.
//
// The transaction still sees its own release — that is what the mark is for, and it
// is what lets one transaction free a value and take it again on another node.
//
// The zero value is ready to use and allocates nothing until the first release, so
// a statement under a schema with no UNIQUE constraint costs nothing at all.
//
// Not safe for concurrent use: it belongs to one transaction, which is driven by
// one goroutine at a time, exactly like the undo log it travels beside.
type ConstraintTxn struct {
	// inline holds the first few releases without allocating, and n is how many of
	// its slots are in use. spill takes the rest, and is nil for every transaction
	// that stays within the inline capacity.
	//
	// # Why an inline array and not a map
	//
	// The first version held a map, and `make(map[ckeyval]struct{}, 2)` on the first
	// release of every transaction is one allocation per commit. That was the leading
	// SUSPECT for a measured write-throughput regression on the constrained path, and
	// replacing it with this array did NOT move the number — so the map was not the
	// cause, and this comment says so rather than claiming a diagnosis the measurement
	// refuted.
	//
	// It is kept because it is better regardless: it allocates NOTHING for the shape
	// every statement has. A statement releases one or two constrained values — a SET
	// replaces one value on one node, and a node carries one or two constrained labels —
	// so four slots cover it, and a linear scan over four entries beats a map lookup
	// outright. It is the same trade [undoLog.inline] and NodeByIndexSeek's id buffer
	// already make.
	inline [4]ckeyval
	n      int
	spill  map[ckeyval]struct{}
}

// markReleased records that this transaction has released val under key.
//
// Idempotent: a repeated release of the same value records one mark, so a statement
// that releases the same value twice does not consume two slots.
func (t *ConstraintTxn) markReleased(key ckey, val string) {
	if t == nil {
		return
	}
	kv := ckeyval{key: key, val: val}
	for i := 0; i < t.n; i++ {
		if t.inline[i] == kv {
			return
		}
	}
	if t.n < len(t.inline) {
		t.inline[t.n] = kv
		t.n++
		return
	}
	if t.spill == nil {
		t.spill = make(map[ckeyval]struct{}, 4)
	}
	t.spill[kv] = struct{}{}
}

// unmarkReleased withdraws a release this transaction recorded. It is the inverse a
// rolled-back STATEMENT replays: it touches only this transaction's own mark, never
// the shared value-set, which is the whole point (see the type comment).
//
// The inline slot is closed over by moving the last entry into it, which is safe
// because the order of the marks carries no meaning — they are a set.
func (t *ConstraintTxn) unmarkReleased(key ckey, val string) {
	if t == nil {
		return
	}
	kv := ckeyval{key: key, val: val}
	for i := 0; i < t.n; i++ {
		if t.inline[i] == kv {
			t.inline[i] = t.inline[t.n-1]
			t.inline[t.n-1] = ckeyval{}
			t.n--
			return
		}
	}
	if t.spill != nil {
		delete(t.spill, kv)
	}
}

// releasedHere reports whether this transaction has released val under key, so its
// own subsequent write of that value is not refused by a reservation it has itself
// given up.
func (t *ConstraintTxn) releasedHere(key ckey, val string) bool {
	if t == nil {
		return false
	}
	kv := ckeyval{key: key, val: val}
	for i := 0; i < t.n; i++ {
		if t.inline[i] == kv {
			return true
		}
	}
	if t.spill == nil {
		return false
	}
	_, ok := t.spill[kv]
	return ok
}

// forEachReleased calls fn for every value this transaction has released.
func (t *ConstraintTxn) forEachReleased(fn func(ckeyval)) {
	if t == nil {
		return
	}
	for i := 0; i < t.n; i++ {
		fn(t.inline[i])
	}
	for kv := range t.spill {
		fn(kv)
	}
}

// empty reports whether this transaction has released nothing.
func (t *ConstraintTxn) empty() bool { return t == nil || (t.n == 0 && len(t.spill) == 0) }

// Reset drops every mark, which is what a ROLLBACK does. Nothing shared was
// touched, so there is nothing to undo.
//
// The inline slots are zeroed rather than merely counted out, so a released value's
// string does not stay reachable through a recycled adapter; the spill map is kept for
// reuse, exactly as the undo log keeps its slice.
func (t *ConstraintTxn) Reset() {
	if t == nil {
		return
	}
	for i := 0; i < t.n; i++ {
		t.inline[i] = ckeyval{}
	}
	t.n = 0
	clear(t.spill)
}

// ─────────────────────────────────────────────────────────────────────────────
// ConstraintRegistry
// ─────────────────────────────────────────────────────────────────────────────

// ConstraintRegistry is a thread-safe registry of active constraints. It
// stores unique and not-null constraints keyed by "label.prop".
//
// ConstraintRegistry is safe for concurrent use.
type ConstraintRegistry struct {
	unique    map[ckey]string              // (label, prop) → index name
	notNull   map[ckey]bool                // (label, prop) → true
	valueSets map[ckey]map[string]struct{} // (label, prop) → set of string values in use
	// uniqueNames / notNullNames carry the user-defined constraint name per
	// (label, prop) key, tracked separately per kind because a UNIQUE and a
	// NOT NULL constraint may coexist on the same key. The name is needed so a
	// constraint round-trips durably through the WAL / snapshot with the name
	// the client declared. An entry is absent for a constraint registered
	// without a name (the legacy [RegisterUnique] / [RegisterNotNull] path).
	uniqueNames  map[ckey]string // (label, prop) → constraint name (UNIQUE)
	notNullNames map[ckey]string // (label, prop) → constraint name (NOT NULL)
	// notNullByLabel is a label → NOT-NULL-property-keys index maintained
	// alongside notNull so the commit-time existence check does an O(1) lookup
	// per touched label instead of scanning the whole notNull map and splitting
	// every key (#1911). Its slices are copy-on-write: RegisterNotNull /
	// UnregisterNotNull replace the whole slice rather than mutate it in place,
	// so [NotNullProperties] can hand back the internal slice with zero
	// allocation and a reader that already holds one is never affected by a
	// concurrent modification.
	notNullByLabel map[string][]string
	// uniqueByLabel is the same label → property-keys index for UNIQUE
	// constraints, maintained alongside unique by RegisterUnique /
	// UnregisterUnique under the identical copy-on-write discipline (see
	// [addLabelProp]), so [UniqueProperties] hands back the internal slice with
	// zero allocation.
	//
	// It exists because the LABEL-set path needs the inverse lookup the (label,
	// prop)-keyed `unique` map cannot answer: `SET n:Person` knows the label and
	// must discover which of that label's properties are constrained, whereas
	// every property-set caller already knows both halves of the key (rmp #2352).
	uniqueByLabel map[string][]string

	mu sync.RWMutex

	// uniqueActive is how many UNIQUE constraints are registered, maintained
	// alongside the `unique` map so the WRITE PATH can skip this registry's lock
	// entirely when there are none (rmp #2306).
	//
	// # Why an atomic counter and not len(unique) under the RLock
	//
	// Because taking the lock is the cost. Every constrained-or-not property write
	// called [ConstraintRegistry.ReserveSetProperty], which takes mu EXCLUSIVELY, so
	// one global mutex sat on the write hot path whether or not the graph had a single
	// constraint. With writers serialised that was invisible; once rmp #2306 retired
	// the engine's writer mutex it became THE ceiling. Measured by mutex profile at
	// sixteen concurrent writers on `CREATE (n:Account {id: $id})` — a statement
	// against a schema with NO constraints at all — ReserveSetProperty accounted for
	// 57 % of all lock delay and RecordPropertySet for a further 41 %, together
	// essentially the whole of it.
	//
	// # Why reading it without the lock is sound
	//
	// A registration can only happen under [Engine.ddlMu] AND the graph's schema
	// barrier held EXCLUSIVELY, while an ordinary write holds that barrier SHARED for
	// its whole bracket — and these methods are called from inside that bracket. So a
	// write that reads zero here cannot have a constraint appear underneath it: the
	// DDL cannot acquire the barrier until the write's bracket closes. The counter is
	// atomic rather than plain only because the DDL writing it and the write reading
	// it are different goroutines.
	uniqueActive atomic.Int64
	// notNullActive is the same counter for NOT NULL constraints.
	//
	// It is separate because [ConstraintRegistry.ReserveSetProperty] enforces BOTH
	// kinds and must consult both, while the value-set methods
	// ([ConstraintRegistry.RecordPropertySet],
	// [ConstraintRegistry.ReleasePropertyValue]) touch only the UNIQUE value sets and
	// gate on uniqueActive alone.
	notNullActive atomic.Int64
}

// NewConstraintRegistry creates an empty ConstraintRegistry.
func NewConstraintRegistry() *ConstraintRegistry {
	return &ConstraintRegistry{
		unique:         make(map[ckey]string),
		notNull:        make(map[ckey]bool),
		valueSets:      make(map[ckey]map[string]struct{}),
		uniqueNames:    make(map[ckey]string),
		notNullNames:   make(map[ckey]string),
		notNullByLabel: make(map[string][]string),
		uniqueByLabel:  make(map[string][]string),
	}
}

// ConstraintInfo is a structured description of one registered constraint,
// used to persist the constraint set durably and to re-register it on
// recovery. KindUnique distinguishes UNIQUE (true) from NOT NULL (false).
type ConstraintInfo struct {
	// Label is the constrained node label.
	Label string
	// Property is the constrained property key.
	Property string
	// Name is the user-defined constraint name (may be empty for a constraint
	// registered without one).
	Name string
	// KindUnique is true for a UNIQUE constraint, false for NOT NULL.
	KindUnique bool
}

// ckey is the registry map key: the (label, property) pair kept as distinct
// fields. Keeping them structurally separate — rather than joining them into a
// "label.prop" string and splitting on a dot — eliminates the aliasing where a
// dotted label or property made ("A.b","c") and ("A","b.c") collide on the same
// key, which could mis-attribute a constraint (#1916). ckey is comparable, so
// it is a valid Go map key directly.
type ckey struct{ label, prop string }

// constraintKey returns the canonical map key for (label, prop). Call sites are
// unchanged from when the key was a string; only the key type is now unambiguous.
func constraintKey(label, prop string) ckey { return ckey{label: label, prop: prop} }

// RegisterUnique adds a unique constraint for (label, prop) backed by
// indexName in the index.Manager.
func (r *ConstraintRegistry) RegisterUnique(label, prop, indexName string) {
	r.mu.Lock()
	key := constraintKey(label, prop)
	_, existed := r.unique[key]
	r.unique[key] = indexName
	if r.valueSets[key] == nil {
		r.valueSets[key] = make(map[string]struct{})
	}
	addLabelProp(r.uniqueByLabel, label, prop)
	if !existed {
		r.uniqueActive.Add(1)
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
		out = append(out, ConstraintInfo{KindUnique: true, Label: key.label, Property: key.prop, Name: r.uniqueNames[key]})
	}
	for key := range r.notNull {
		out = append(out, ConstraintInfo{KindUnique: false, Label: key.label, Property: key.prop, Name: r.notNullNames[key]})
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

// Count returns the number of registered constraints (UNIQUE + NOT NULL). It is
// a cheap, allocation-free alternative to len(Constraints()) that the engine
// uses to mirror the count onto the graph for the checkpointer (#1464).
//
// Count is safe for concurrent use.
func (r *ConstraintRegistry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.unique) + len(r.notNull)
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
				Detail:   fmt.Sprintf("pre-existing data contains duplicate value %s", humanConstraintValue(values[i])),
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
func (r *ConstraintRegistry) mergeSeed(key ckey, seed map[string]struct{}) {
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
	key := constraintKey(label, prop)
	if !r.notNull[key] {
		r.notNullActive.Add(1)
	}
	r.notNull[key] = true
	addLabelProp(r.notNullByLabel, label, prop)
	r.mu.Unlock()
}

// addLabelProp adds prop to idx[label] in the copy-on-write label index,
// deduplicating. Callers hold r.mu.
//
// The slice is REPLACED rather than appended to in place. That is the property
// [NotNullProperties] and [UniqueProperties] rest on: they return the registry's
// own slice with zero allocation, so a reader already holding one must never see
// it change underneath. `append` onto a slice with spare capacity would mutate
// the shared backing array and break exactly that.
//
// One helper serves both the NOT NULL and the UNIQUE index because the discipline
// is identical and a second copy would be free to drift — and a drift here is a
// label whose constraint the write path stops finding.
func addLabelProp(idx map[string][]string, label, prop string) {
	old := idx[label]
	for _, p := range old {
		if p == prop {
			return // already present
		}
	}
	next := make([]string, len(old)+1)
	copy(next, old)
	next[len(old)] = prop
	idx[label] = next
}

// removeLabelProp removes prop from idx[label] in the copy-on-write label index,
// dropping the label entry when it becomes empty. Callers hold r.mu. See
// [addLabelProp] for why the slice is replaced rather than mutated.
func removeLabelProp(idx map[string][]string, label, prop string) {
	old := idx[label]
	if len(old) == 0 {
		return
	}
	next := make([]string, 0, len(old))
	for _, p := range old {
		if p != prop {
			next = append(next, p)
		}
	}
	if len(next) == 0 {
		delete(idx, label)
		return
	}
	idx[label] = next
}

// UnregisterUnique removes the unique constraint for (label, prop). No-op if
// absent.
func (r *ConstraintRegistry) UnregisterUnique(label, prop string) {
	r.mu.Lock()
	key := constraintKey(label, prop)
	if _, existed := r.unique[key]; existed {
		r.uniqueActive.Add(-1)
	}
	delete(r.unique, key)
	delete(r.valueSets, key)
	delete(r.uniqueNames, key)
	removeLabelProp(r.uniqueByLabel, label, prop)
	r.mu.Unlock()
}

// HasAnyUnique reports whether any UNIQUE constraint is registered, WITHOUT taking
// the registry lock.
//
// It is the write path's gate: with no UNIQUE constraint there is nothing to check
// and nothing to reserve, so the three value-set methods return before touching mu.
// See [ConstraintRegistry.uniqueActive] for the measurement that motivated it and
// for why the lock-free read is sound.
func (r *ConstraintRegistry) HasAnyUnique() bool { return r.uniqueActive.Load() > 0 }

// UnregisterNotNull removes the not-null constraint for (label, prop). No-op
// if absent.
func (r *ConstraintRegistry) UnregisterNotNull(label, prop string) {
	r.mu.Lock()
	key := constraintKey(label, prop)
	if r.notNull[key] {
		r.notNullActive.Add(-1)
	}
	delete(r.notNull, key)
	delete(r.notNullNames, key)
	removeLabelProp(r.notNullByLabel, label, prop)
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

// ResolveByName resolves a user-defined constraint name to its (kind, label,
// property) identity, so a DROP CONSTRAINT <name> can locate the constraint to
// remove. It searches the UNIQUE names first, then the NOT NULL names, and
// returns found=false when no constraint carries that name.
//
// Only constraints registered WITH a name (via [SetConstraintName], which the
// CREATE CONSTRAINT executor calls) are resolvable; an anonymous constraint
// registered through the legacy [RegisterUnique] / [RegisterNotNull] path has
// no name to match.
//
// ResolveByName is safe for concurrent use. It is deterministic: it scans the
// UNIQUE names then the NOT NULL names, each in sorted key order, and returns
// the first match, so a lookup never depends on Go map iteration order. Once
// constraint names are enforced unique at CREATE time ([ErrConstraintNameConflict])
// at most one constraint can match, so the sorted scan is belt-and-braces.
func (r *ConstraintRegistry) ResolveByName(name string) (kind ConstraintKind, label, prop string, found bool) {
	if name == "" {
		return 0, "", "", false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if key, ok := firstKeyByName(r.uniqueNames, name); ok {
		return ConstraintUnique, key.label, key.prop, true
	}
	if key, ok := firstKeyByName(r.notNullNames, name); ok {
		return ConstraintNotNull, key.label, key.prop, true
	}
	return 0, "", "", false
}

// firstKeyByName returns the smallest key (ordered by label then property) in
// names whose value equals name, for deterministic resolution. Callers hold r.mu.
func firstKeyByName(names map[ckey]string, name string) (ckey, bool) {
	var best ckey
	found := false
	for key, n := range names {
		if n == name && (!found || key.label < best.label || (key.label == best.label && key.prop < best.prop)) {
			best, found = key, true
		}
	}
	return best, found
}

// NameInUse reports the identity of the constraint currently holding name, or
// found=false when no constraint carries it. It is the CREATE-time lookup used
// to reject a name already used by a different constraint. NameInUse is safe
// for concurrent use and deterministic (see [ResolveByName]).
func (r *ConstraintRegistry) NameInUse(name string) (kind ConstraintKind, label, prop string, found bool) {
	return r.ResolveByName(name)
}

// HasNotNull reports whether a not-null constraint exists for (label, prop).
func (r *ConstraintRegistry) HasNotNull(label, prop string) bool {
	r.mu.RLock()
	ok := r.notNull[constraintKey(label, prop)]
	r.mu.RUnlock()
	return ok
}

// HasAnyNotNull reports whether at least one NOT NULL (property-existence)
// constraint is registered, WITHOUT taking the registry lock.
//
// It is the cheap gate the engine consults on every statement before doing any
// touched-node existence-constraint work: when it returns false the commit-time check
// is a no-op and the per-transaction touched-node recording is skipped entirely.
//
// It used to take the read lock to measure `len(notNull)`, which put one lock
// acquisition per statement on the write path — cheap while writers were serialised
// and no longer free once rmp #2306 let them overlap, for the same reason
// [ConstraintRegistry.uniqueActive] gives: the acquisition IS the cost. It now reads
// the counter, which is sound for the same reason: a registration needs the schema
// barrier held exclusively, and an ordinary write holds it shared for its whole
// bracket.
//
// HasAnyNotNull is safe for concurrent use.
func (r *ConstraintRegistry) HasAnyNotNull() bool { return r.notNullActive.Load() > 0 }

// NotNullProperties returns the property keys for which a NOT NULL constraint is
// registered on label, or nil when label carries no existence constraint. It is
// the per-label lookup the commit-time existence check uses to test only the
// constrained properties of a touched node's labels.
//
// The lookup is O(1) via the notNullByLabel index and allocates nothing on this
// hot path: it returns the registry's own copy-on-write slice, which
// RegisterNotNull / UnregisterNotNull never mutate in place. The caller must
// treat the result as READ-ONLY (it is shared and must not be appended to or
// modified). Most labels carry zero or one existence constraint, so the common
// return is nil or a one-element slice. NotNullProperties is safe for
// concurrent use.
func (r *ConstraintRegistry) NotNullProperties(label string) []string {
	r.mu.RLock()
	out := r.notNullByLabel[label]
	r.mu.RUnlock()
	return out
}

// UniqueProperties returns the property keys for which a UNIQUE constraint is
// registered on label, or nil when label carries no uniqueness constraint. It is
// the per-label lookup the LABEL-set path uses to discover which of a node's
// properties attaching that label brings under a uniqueness constraint (rmp
// #2352) — the inverse of the (label, prop) lookup every property-set caller
// makes, which already knows both halves of the key.
//
// The lookup is O(1) via the uniqueByLabel index and allocates nothing: it
// returns the registry's own copy-on-write slice, which RegisterUnique /
// UnregisterUnique never mutate in place. The caller must treat the result as
// READ-ONLY (it is shared and must not be appended to or modified). Most labels
// carry zero or one uniqueness constraint, so the common return is nil or a
// one-element slice.
//
// Callers on the write path MUST gate this behind [HasAnyUnique], which is a
// lock-free atomic load: UniqueProperties itself takes the registry read lock,
// and putting that on every label write of an unconstrained schema is precisely
// the cost [ConstraintRegistry.uniqueActive] exists to avoid.
//
// UniqueProperties is safe for concurrent use.
func (r *ConstraintRegistry) UniqueProperties(label string) []string {
	r.mu.RLock()
	out := r.uniqueByLabel[label]
	r.mu.RUnlock()
	return out
}

// CheckSetProperty validates that setting prop = value on a node with the
// given labels does not violate any registered constraint. mgr is used for
// unique-constraint index lookups (hash index Cardinality check) as a
// secondary source; the primary source is the registry's own value set.
//
// The primary value-set is authoritative: when it is present (non-nil) and
// does not contain value, the secondary hash-index check is skipped. The
// value-set is seeded from the live graph at constraint creation and kept
// current by RecordPropertySet / ReleasePropertyValue / ReseedFromGraph, so
// an absent primary "not present" signal means the value is genuinely free.
// The secondary check is consulted only when the primary set has not been
// initialised (nil), covering the narrow window before SeedUniqueValues is
// called.
//
// Returns *ConstraintViolationError (which wraps ErrConstraintViolation) on
// the first violation found; nil when all constraints pass.
//
// # It CHECKS ONLY, which is not enough for a concurrent writer
//
// This is a read: it takes the read lock, consults the value-set, and returns.
// Reserving the value happens later and separately, in [RecordPropertySet], which
// takes the WRITE lock — so between the two, another writer can run the same check
// and reach the same conclusion. Two writers then both insert and the UNIQUE
// constraint is violated with both statements reporting success.
//
// That is unreachable while the engine serialises whole write statements on the
// exclusive visibility barrier, which is why this shape survived. It is NOT
// reachable-but-rare: with the concurrent write path enabled it was measured at 14
// of 15 runs (rmp #2321). A writer that can overlap another must therefore call
// [ConstraintRegistry.ReserveSetProperty], which does both halves under one lock.
//
// This entry point remains for callers that genuinely only want to ASK —
// pre-validation and diagnostics — and for the existing test surface.
//
// ct carries the caller's own uncommitted releases, so asking about a value the
// asking transaction has itself given up answers "free"; nil means "no
// transaction". See [ConstraintTxn].
func (r *ConstraintRegistry) CheckSetProperty(ct *ConstraintTxn, labels []string, prop string, value lpg.PropertyValue, mgr *index.Manager) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.checkSetPropertyLocked(ct, labels, prop, value, mgr)
}

// ReserveSetProperty validates that setting prop = value on a node with the given
// labels violates no registered constraint AND, in the same critical section,
// records the value as in use — so no concurrent writer can pass the same check.
//
// It is the enforcement entry point for every write path. On success the value is
// already reserved and the caller's later [ConstraintRegistry.RecordPropertySet] is
// a harmless idempotent no-op. On failure NOTHING is reserved, for any label.
//
// # Why atomic, and the prior art
//
// Splitting the test from the insert is a check-then-act race, and it is the defect
// [ConstraintRegistry.CheckSetProperty] documents above. PostgreSQL closes the same
// hole by holding the leaf page's buffer lock ACROSS the uniqueness check and the
// insertion: `_bt_doinsert` calls `_bt_check_unique` with the buffer locked and does
// not release it before inserting, and `_bt_stepright` states the reason in as many
// words — "We must write-lock the target page before releasing write lock on current
// page; else someone else's _bt_check_unique scan could fail to see our insertion"
// (postgres/postgres, master @ 36f7330, 2026-08-03,
// src/backend/access/nbtree/nbtinsert.c:1033). Memgraph validates unique constraints
// under the constraint structure's own lock for the same reason
// (memgraph/memgraph, branch master, src/storage/v2/constraints/unique_constraints.cpp).
//
// # Why a duplicate is a VIOLATION here and not a retriable conflict
//
// PostgreSQL distinguishes two cases: a duplicate committed by a finished
// transaction is a constraint violation, while a duplicate inserted by a
// STILL-RUNNING transaction makes the inserter release its lock, wait for that
// transaction (`XactLockTableWait`, or `SpeculativeInsertionWait` for
// `INSERT … ON CONFLICT`) and start over — so the outcome depends on whether the
// peer commits or aborts.
//
// GoGraph cannot make that distinction here, and the reason is structural rather
// than an oversight: PostgreSQL's index entry carries the inserting transaction id,
// while this value-set is a set of strings with no owner and the registry has no
// transaction-liveness oracle to consult even if it had one. So every duplicate is
// reported as what openCypher calls it — a constraint violation, a client error —
// which is always correct for a committed duplicate and conservative for an
// in-flight one (it rejects a statement that a retry might have satisfied, rather
// than admitting a statement that breaks the invariant). Refining the in-flight case
// to a retriable [mvcc.ErrSerializationConflict], which is what Memgraph reports,
// needs the value-set to carry its reserver's transaction id; that arrives with the
// transaction threading in rmp #2320.
//
// # Intra-statement duplicates are now caught too
//
// A statement that writes the same constrained value to two nodes —
// `SET a.email = 'x', b.email = 'x'` under a UNIQUE constraint on email — used to
// pass, because every check ran before any record. Reserving at check time rejects
// the second write. That is a fix, not a side effect: such a statement leaves the
// graph violating a declared invariant.
// ct carries the reservations this transaction has released but not committed, so a
// value it has itself given up is not refused as its own duplicate; see
// [ConstraintTxn]. It may be nil, which means "no transaction" — a caller with
// nothing to roll back.
func (r *ConstraintRegistry) ReserveSetProperty(ct *ConstraintTxn, labels []string, prop string, value lpg.PropertyValue, mgr *index.Manager) error {
	// No constraint of EITHER kind: nothing to check and nothing to reserve, so do not
	// take this registry's global lock — it would put one mutex on every property write
	// for no benefit. BOTH counters, because this method enforces UNIQUE and NOT NULL;
	// gating it on uniqueActive alone silently disabled NOT NULL enforcement and
	// TestReserveSetProperty_NotNullStillEnforced caught it. See
	// [ConstraintRegistry.notNullActive].
	if r.uniqueActive.Load() == 0 && r.notNullActive.Load() == 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	// Phase 1 — verify EVERY label before reserving for any of them, so a
	// violation on the second label cannot leave the first one reserved. A node
	// carries all its labels at once; a partial reservation would be a phantom that
	// only a whole-graph reseed could clear.
	if err := r.checkSetPropertyLocked(ct, labels, prop, value, mgr); err != nil {
		return err
	}

	// Phase 2 — reserve. Same critical section, so no other writer observed the gap.
	// A null value is not constrained by UNIQUE and has nothing to reserve.
	if strVal, ok := propertyValueToString(value); ok {
		for _, label := range labels {
			key := constraintKey(label, prop)
			if vs := r.valueSets[key]; vs != nil {
				vs[strVal] = struct{}{}
			}
			// This transaction is taking the value again, so its own pending release
			// of it is spent. Without this a transaction that released a value and
			// re-reserved it would, at commit, apply the release and delete the
			// reservation it had just taken.
			ct.unmarkReleased(key, strVal)
		}
	}
	return nil
}

// checkSetPropertyLocked is the constraint test shared by
// [ConstraintRegistry.CheckSetProperty] and
// [ConstraintRegistry.ReserveSetProperty]. The caller must hold r.mu in read mode
// (to ask) or write mode (to ask and then reserve).
//
// Extracting it is what lets the two entry points differ ONLY in atomicity rather
// than in what they consider a violation — a second copy of this logic would be
// free to drift, and a drift here is an unenforced constraint.
func (r *ConstraintRegistry) checkSetPropertyLocked(ct *ConstraintTxn, labels []string, prop string, value lpg.PropertyValue, mgr *index.Manager) error {
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
					// A reservation THIS transaction has released is not an obstacle to
					// its own write: the release is deferred to commit, so the value is
					// still in the shared set and would otherwise refuse the transaction
					// that gave it up (rmp #2366). A PEER's pending release is
					// deliberately NOT consulted — the value is still committed to that
					// peer's node until the peer commits, and handing it over before then
					// is how a rolled-back release ends up with two holders.
					if _, exists := vs[strVal]; exists && !ct.releasedHere(key, strVal) {
						return &ConstraintViolationError{
							Label:    label,
							Property: prop,
							Kind:     "UNIQUE",
							Detail:   fmt.Sprintf("value %s already exists", humanConstraintValue(value)),
						}
					}
					// Primary value-set is present and definitively reports the
					// value as absent — it is maintained by RecordPropertySet /
					// ReleasePropertyValue / ReseedFromGraph and is seeded from
					// the live graph, so its "not present" signal is
					// authoritative. Skip the secondary (hash-index cardinality)
					// check for this label: the backing UNIQUE index is an
					// unbound hash index that does not self-maintain from the
					// change fan-out, so its cardinality can carry stale ghost
					// entries for values that were deleted, rolled back, or
					// overwritten (#1342).
					continue
				}
				// Null value: UNIQUE does not constrain nulls; fall through.
			}

			// Secondary: also check via the hash index cardinality. Only
			// reached when the primary value-set is nil (not yet seeded).
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
// # DO NOT CALL THIS FROM A WRITE PATH (rmp #2357/#2358)
//
// Its only legitimate caller is [releaseConstraintValue], where it IS the journaled
// inverse of a release: a rolled-back release must put the value back.
//
// Every write path must reserve through [reserveConstraintValue] and nothing else.
// Eleven write sites used to call this immediately AFTER their reserve, and every
// one of those calls was dead weight: the body below is the SAME set insert as
// [ConstraintRegistry.ReserveSetProperty]'s phase 2, over the same (labels, prop,
// value), and a set insert is idempotent — so the value was already present. What it
// was not free of is r.mu: each call took this registry's WRITE lock a second time
// per constrained property write, and
// [nodeStateReader] records that this lock measured 57 % of ALL lock delay at sixteen
// writers on a schema with no constraints at all. Verified site by site before the
// calls were removed, including the three whose reserve sits more than a dozen lines
// above it.
//
// A new write path that "records" what it has already reserved therefore buys
// nothing and pays a lock acquisition. Reserve, and stop.
func (r *ConstraintRegistry) RecordPropertySet(labels []string, prop string, value lpg.PropertyValue) {
	// No UNIQUE constraint anywhere: nothing to record, and taking this registry's
	// global lock would put one mutex on every property write for no benefit. See
	// [ConstraintRegistry.uniqueActive].
	if r.uniqueActive.Load() == 0 {
		return
	}
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

// ReleasePropertyValue removes a value from the unique value-set so it is no
// longer treated as "in use" by the constraint. It must be called whenever a
// previously recorded value is no longer present in the graph: on node
// deletion, on REMOVE of a constrained property, and when SET replaces an
// existing constrained value with a new one (releasing the old value).
//
// ReleasePropertyValue is a no-op when the value was not present in the
// value-set, or when no unique constraint exists for (label, prop). It is
// safe for concurrent use.
//
// # It is DEFERRED when a transaction owns it (rmp #2366)
//
// With ct non-nil the release is recorded as that transaction's own pending
// contribution and the shared value-set is left alone until
// [ConstraintRegistry.CommitTxn]. That is what makes a rollback free: it drops a
// private mark instead of writing a re-reservation into shared state that a peer's
// COMMITTED release may already have changed. [ConstraintTxn] carries the
// interleaving this closes.
//
// With ct nil there is no transaction and therefore nothing to roll back, so the
// release applies immediately — the correct reading of a change that is committed
// the instant it is made.
func (r *ConstraintRegistry) ReleasePropertyValue(ct *ConstraintTxn, labels []string, prop string, value lpg.PropertyValue) {
	// No UNIQUE constraint anywhere: nothing to release, and taking this registry's
	// global lock would put one mutex on every property write for no benefit. See
	// [ConstraintRegistry.uniqueActive].
	if r.uniqueActive.Load() == 0 {
		return
	}
	strVal, ok := propertyValueToString(value)
	if !ok {
		return
	}
	if ct != nil {
		// Deferred: no lock, because nothing shared is touched. That also removes one
		// acquisition of this registry's global mutex from the write path, which
		// [ConstraintRegistry.uniqueActive] records as having measured 57 % of all
		// lock delay at sixteen writers.
		for _, label := range labels {
			ct.markReleased(constraintKey(label, prop), strVal)
		}
		return
	}
	r.mu.Lock()
	for _, label := range labels {
		key := constraintKey(label, prop)
		if vs := r.valueSets[key]; vs != nil {
			delete(vs, strVal)
		}
	}
	r.mu.Unlock()
}

// CommitTxn applies ct's pending releases to the shared value-sets and clears it.
//
// It must be called exactly once per transaction that committed, from inside the
// same window that publishes the transaction's writes, so the value a committed
// release frees becomes available at the instant the graph stops holding it.
//
// A rolled-back transaction calls [ConstraintTxn.Reset] instead — there is nothing
// to undo, because a deferred release never touched anything shared.
//
// Safe for concurrent use.
func (r *ConstraintRegistry) CommitTxn(ct *ConstraintTxn) {
	// A nil registry is a query with no enforcement, and an empty contribution is the
	// common case: both are no-ops, so every caller can call this unconditionally.
	if r == nil || ct.empty() {
		return
	}
	r.mu.Lock()
	ct.forEachReleased(func(kv ckeyval) {
		if vs := r.valueSets[kv.key]; vs != nil {
			delete(vs, kv.val)
		}
	})
	r.mu.Unlock()
	ct.Reset()
}

// ReseedFromGraph clears every UNIQUE value-set and rebuilds it from the
// provided (label, prop) → values mapping. It is called after an in-memory
// transaction undo (rollback) so the registry reflects the restored graph
// state rather than the rolled-back writes.
//
// The caller supplies a scanFn that, given a label and property key, returns
// the property values currently carried by all live nodes with that label.
// ReseedFromGraph acquires the registry write lock only once per constraint,
// so the scan itself must NOT hold the registry lock.
func (r *ConstraintRegistry) ReseedFromGraph(scanFn func(label, prop string) []lpg.PropertyValue) {
	r.mu.RLock()
	// Snapshot the registered constraint keys so we can iterate them without
	// holding the write lock during the (potentially expensive) scan.
	keys := make([]ckey, 0, len(r.unique))
	for k := range r.unique {
		keys = append(keys, k)
	}
	r.mu.RUnlock()

	for _, k := range keys {
		values := scanFn(k.label, k.prop)
		seed := make(map[string]struct{}, len(values))
		for _, v := range values {
			if sv, ok := propertyValueToString(v); ok {
				seed[sv] = struct{}{}
			}
		}
		r.mu.Lock()
		if vs := r.valueSets[k]; vs != nil {
			// Replace the value-set contents in-place so we keep the map header
			// (avoids an extra allocation) and concurrent readers see the update
			// as a single atomic swap of the whole set's contents.
			for oldKey := range vs {
				delete(vs, oldKey)
			}
			for newKey := range seed {
				vs[newKey] = struct{}{}
			}
		}
		r.mu.Unlock()
	}
}

// humanConstraintValue renders a property value in its natural form for a
// client-facing constraint-violation message. It is display-only and never used
// as an identity key — unlike [propertyValueToString], whose kind-tagged output
// (e.g. "\x00s\x00a@b.com") must never reach a user-facing message (#1914).
func humanConstraintValue(v lpg.PropertyValue) string {
	switch v.Kind() {
	case lpg.PropString:
		s, _ := v.String()
		return strconv.Quote(s)
	case lpg.PropInt64:
		i, _ := v.Int64()
		return strconv.FormatInt(i, 10)
	case lpg.PropFloat64:
		f, _ := v.Float64()
		return strconv.FormatFloat(f, 'g', -1, 64)
	case lpg.PropBool:
		b, _ := v.Bool()
		return strconv.FormatBool(b)
	case lpg.PropTime:
		t, _ := v.Time()
		return t.UTC().Format(time.RFC3339Nano)
	case lpg.PropBytes:
		raw, _ := v.Bytes()
		return fmt.Sprintf("<%d bytes>", len(raw))
	case lpg.PropList:
		return "<list>"
	}
	return "<null>"
}

// propertyValueToString converts a PropertyValue to a canonical string key
// for use in a unique value-set. The second return is false when the value
// carries no enforceable identity — the zero PropertyValue (null), which a
// UNIQUE constraint does not constrain (null-handling is the NOT NULL
// constraint's job) — so callers skip the uniqueness check for it.
//
// Numbers use VALUE-EQUIVALENCE semantics so the UNIQUE check agrees with
// openCypher = and with MERGE (#1240): an integral float within int64 range
// folds onto the identical integer, so the integer 1 and the float 1.0 (and
// +0.0 / -0.0) map to ONE key, while a distinct kind such as the string "1"
// stays separate. All NaN values collapse to a single key (NaN ≡ NaN, matching
// value-equivalence). A non-integral or out-of-int64-range float keeps its
// IEEE-754 bits. Value-equivalence is exact-value-based and therefore transitive
// — the property a value-set map key requires — so two integers beyond 2^53 that
// share a float64 rounding are NOT folded together (they are distinct values).
// Times use an injective RFC3339Nano form, bytes base64. Both the live check and
// the recovery re-seed apply this same function to the lpg.PropertyValue, so
// they agree on what counts as a duplicate.
func propertyValueToString(value lpg.PropertyValue) (string, bool) {
	switch value.Kind() {
	case lpg.PropString:
		s, _ := value.String()
		return "\x00s\x00" + s, true
	case lpg.PropInt64:
		i, _ := value.Int64()
		return numericCanonicalKey(i), true
	case lpg.PropFloat64:
		f, _ := value.Float64()
		return floatCanonicalKey(f), true
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

// numericCanonicalKey is the value-set key for an integer: the shared numeric
// namespace an equal integral float folds into (see [floatCanonicalKey]).
func numericCanonicalKey(i int64) string {
	return "\x00#\x00" + strconv.FormatInt(i, 10)
}

// float64IntBound is 2^63 as a float64 (exactly representable). A finite float
// strictly below it in magnitude, and >= -2^63, converts to int64 without
// overflow; the guard keeps the int64(f) conversion well-defined.
const float64IntBound = 9223372036854775808.0 // 2^63

// floatCanonicalKey is the value-set key for a float. A finite, integral float
// within int64 range folds onto the identical integer key (so 1.0 ≡ 1, and
// +0.0 / -0.0 ≡ 0); every NaN collapses to one key (NaN ≡ NaN); any other float
// keeps its IEEE-754 bits (injective and distinct from every integer).
func floatCanonicalKey(f float64) string {
	if math.IsNaN(f) {
		return "\x00#nan\x00" // all NaN are value-equivalent
	}
	if !math.IsInf(f, 0) && f == math.Trunc(f) && f >= -float64IntBound && f < float64IntBound {
		if n := int64(f); float64(n) == f {
			return numericCanonicalKey(n) // integral: fold onto the integer
		}
	}
	return "\x00#f\x00" + strconv.FormatUint(math.Float64bits(f), 16)
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
// elements: [name, type, label, property]. The name column carries the
// constraint's declared (or auto-generated) name — the same name DROP
// CONSTRAINT resolves by — falling back to the canonical "label.prop" key only
// for a constraint registered without a name (the legacy anonymous path). type
// is "UNIQUE" or "NOT_NULL". Rows are returned in deterministic order (name,
// type, label, property).
//
// ListConstraintRows is safe for concurrent use.
func (r *ConstraintRegistry) ListConstraintRows() [][]expr.Value {
	// displayName is the declared name for key, or the "label.prop" key itself
	// when the constraint was registered anonymously — so create -> list -> drop
	// agrees on the name to use (#1909).
	displayName := func(names map[ckey]string, key ckey) string {
		if n := names[key]; n != "" {
			return n
		}
		return key.label + "." + key.prop
	}

	r.mu.RLock()
	rows := make([][]expr.Value, 0, len(r.unique)+len(r.notNull))
	for key := range r.unique {
		rows = append(rows, []expr.Value{
			expr.StringValue(displayName(r.uniqueNames, key)),
			expr.StringValue("UNIQUE"),
			expr.StringValue(key.label),
			expr.StringValue(key.prop),
		})
	}
	for key := range r.notNull {
		rows = append(rows, []expr.Value{
			expr.StringValue(displayName(r.notNullNames, key)),
			expr.StringValue("NOT_NULL"),
			expr.StringValue(key.label),
			expr.StringValue(key.prop),
		})
	}
	r.mu.RUnlock()

	// Sort for deterministic output: name, then type, then label, then property.
	sort.Slice(rows, func(i, j int) bool {
		for col := 0; col < 4; col++ {
			//nolint:forcetypeassert // the column was type-checked as expr.StringValue before this sort comparator was installed, so every row's value at col is a StringValue
			a := string(rows[i][col].(expr.StringValue))
			//nolint:forcetypeassert // the column was type-checked as expr.StringValue before this sort comparator was installed, so every row's value at col is a StringValue
			b := string(rows[j][col].(expr.StringValue))
			if a != b {
				return a < b
			}
		}
		return false
	})
	return rows
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
