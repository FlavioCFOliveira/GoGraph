package lpg

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// PropertyKind tags a [PropertyValue] with its underlying Go type.
type PropertyKind uint8

// The supported property kinds. They are stable across releases — new
// kinds extend this enum; existing values must not be reordered or
// reused.
const (
	PropString PropertyKind = iota + 1
	PropInt64
	PropFloat64
	PropBool
	PropTime
	PropBytes
	PropList // ordered list of PropertyValue elements; v is []PropertyValue
)

// PropertyValue is a tagged union of typed property values. It is
// laid out as a single (kind, any) pair, totalling 24 bytes on a
// 64-bit platform regardless of the inhabited variant. The zero
// value is invalid; values are constructed via the typed
// constructors ([StringValue], [Int64Value], etc.).
//
// A PropertyValue is immutable after construction and is copied by value, so
// it is safe for concurrent reads by multiple goroutines without external
// locking. The one caveat is the slice-bearing variants: [PropertyValue.Bytes]
// and [PropertyValue.List] return slices that alias the value's backing store,
// so callers must not mutate the returned slice (doing so would mutate the
// otherwise-immutable value and break the concurrency guarantee).
type PropertyValue struct {
	v    any
	kind PropertyKind
}

// Kind returns the underlying type tag.
func (p PropertyValue) Kind() PropertyKind { return p.kind }

// String returns the string value and true when v carries a string,
// the zero value and false otherwise.
func (p PropertyValue) String() (string, bool) {
	if p.kind != PropString {
		return "", false
	}
	s, _ := p.v.(string)
	return s, true
}

// Int64 returns the int64 value and true when v carries an int64.
func (p PropertyValue) Int64() (int64, bool) {
	if p.kind != PropInt64 {
		return 0, false
	}
	i, _ := p.v.(int64)
	return i, true
}

// Float64 returns the float64 value and true when v carries a float64.
func (p PropertyValue) Float64() (float64, bool) {
	if p.kind != PropFloat64 {
		return 0, false
	}
	f, _ := p.v.(float64)
	return f, true
}

// Bool returns the bool value and true when v carries a bool.
func (p PropertyValue) Bool() (val, ok bool) {
	if p.kind != PropBool {
		return false, false
	}
	b, _ := p.v.(bool)
	return b, true
}

// Time returns the time.Time value and true when v carries one.
func (p PropertyValue) Time() (time.Time, bool) {
	if p.kind != PropTime {
		return time.Time{}, false
	}
	t, _ := p.v.(time.Time)
	return t, true
}

// Bytes returns the []byte value and true when v carries one. The
// returned slice aliases the value held by v.
func (p PropertyValue) Bytes() ([]byte, bool) {
	if p.kind != PropBytes {
		return nil, false
	}
	b, _ := p.v.([]byte)
	return b, true
}

// List returns the []PropertyValue elements and true when v carries a
// PropList. The returned slice aliases the value held by v; callers
// must not modify it.
func (p PropertyValue) List() ([]PropertyValue, bool) {
	if p.kind != PropList {
		return nil, false
	}
	elems, _ := p.v.([]PropertyValue)
	return elems, true
}

// String constructors.

// StringValue builds a PropString.
func StringValue(s string) PropertyValue { return PropertyValue{kind: PropString, v: s} }

// Int64Value builds a PropInt64.
func Int64Value(i int64) PropertyValue { return PropertyValue{kind: PropInt64, v: i} }

// Float64Value builds a PropFloat64.
func Float64Value(f float64) PropertyValue { return PropertyValue{kind: PropFloat64, v: f} }

// BoolValue builds a PropBool.
func BoolValue(b bool) PropertyValue { return PropertyValue{kind: PropBool, v: b} }

// TimeValue builds a PropTime.
func TimeValue(t time.Time) PropertyValue { return PropertyValue{kind: PropTime, v: t} }

// DateValue builds a Cypher-visible Date property from t's calendar date — its
// year, month and day in t's location; any time-of-day and time zone are
// ignored. The value is the canonical SOH-tagged date string that the columnar
// storage tier folds into its compact int32 epoch-day column (~4 bytes/value)
// and that the Cypher read path decodes back to a native Date.
//
// Prefer DateValue over a hand-formatted ISO string ([StringValue]) for date
// properties written through the Go API: an untagged string stays in the
// 16-byte-header string column and reads back as a String, whereas a DateValue
// costs ~4 bytes/value and round-trips as a Date — the same on-disk and
// in-memory form a date written through Cypher produces. (Contrast [TimeValue]/
// PropTime, which is not Cypher-visible and reads back as Null.)
func DateValue(t time.Time) PropertyValue {
	y, m, d := t.Date()
	ed := daysFromCivil(y, int(m), d)
	if ed < minEpochDay || ed > maxEpochDay {
		// Calendar dates beyond the int32 epoch-day range (~±5.8M years): emit
		// the tagged canonical string directly — byte-identical to what the
		// Cypher write path produces for the same date — rather than folding it
		// into the compact int32 column. Such extreme dates are astronomically
		// outside any realistic dataset; this branch only guards the int32 cast
		// above from truncating.
		return PropertyValue{kind: PropString, v: string(rune(epochDayTag)) + formatCivil(y, int(m), d)}
	}
	return PropertyValue{kind: PropString, v: epochDayToString(int32(ed))}
}

// BytesValue builds a PropBytes wrapping b (no copy).
func BytesValue(b []byte) PropertyValue { return PropertyValue{kind: PropBytes, v: b} }

// ListValue builds a PropList from elems. The slice is stored directly
// (no copy); callers must not modify elems after calling ListValue.
func ListValue(elems []PropertyValue) PropertyValue {
	return PropertyValue{kind: PropList, v: elems}
}

// PropertyKeyID is the compact identifier of an interned property
// name.
type PropertyKeyID uint32

// propertyKeyNames is an immutable id→name table published by
// [PropertyKeyRegistry] via copy-on-write. Once stored it is never
// mutated; a new interning allocates a fresh table. Readers load the
// pointer once with zero synchronisation, so the read path (Resolve) is
// fully lock-free.
type propertyKeyNames struct {
	names []string
}

// forwardKeyName is the immutable name→id table published by
// [PropertyKeyRegistry] via copy-on-write. Once stored it is never
// mutated; interning a previously unseen name allocates a fresh map.
// Readers (Lookup) load the pointer once with zero synchronisation, so the
// name→id read path is fully lock-free — matching the id→name Resolve path.
type forwardKeyName struct {
	m map[string]PropertyKeyID
}

// PropertyKeyRegistry interns property names and assigns sequential
// PropertyKeyIDs. It is safe for concurrent use.
//
// Both read paths are fully lock-free: [PropertyKeyRegistry.Lookup]
// (name→id) loads the immutable forward table through an [atomic.Pointer]
// and [PropertyKeyRegistry.Resolve] (id→name) loads the immutable id→name
// snapshot, neither taking any lock. The write path
// ([PropertyKeyRegistry.Intern] of a previously unseen name) serialises
// under a mutex, builds fresh immutable tables extended by one entry, and
// publishes them — the id→name snapshot first, then the name→id table — so
// any reader that observes an id from Lookup can already Resolve it, and
// any reader that observes id in a property bag observes (by
// release/acquire ordering through that bag's own publication) tables at
// least as new as the ones Intern published. Lookup/Resolve therefore
// never miss a live id. Per-row property predicates hit Lookup once per
// access per reader; making it lock-free removes the RWMutex reader-count
// atomic that otherwise bounces across cores under concurrent scans. The
// O(n) copy on intern is a deliberate trade: the property-key vocabulary
// is append-mostly schema, interned at warm-up and read billions of times.
type PropertyKeyRegistry struct {
	// fwd holds the immutable name→id table. Loaded lock-free by Lookup;
	// swapped under mu by Intern.
	fwd atomic.Pointer[forwardKeyName]
	// snap holds the immutable id→name table. Loaded lock-free by
	// Resolve; swapped under mu by Intern.
	snap atomic.Pointer[propertyKeyNames]
	// mu serialises Intern (the write path) only; the read paths never take
	// it. The steady-state property vocabulary is small and stable, so
	// Intern is contended only while the vocabulary is first built up.
	mu sync.Mutex
}

// NewPropertyKeyRegistry returns an empty registry.
func NewPropertyKeyRegistry() *PropertyKeyRegistry {
	r := &PropertyKeyRegistry{}
	r.snap.Store(&propertyKeyNames{})
	r.fwd.Store(&forwardKeyName{m: make(map[string]PropertyKeyID)})
	return r
}

// Intern returns a stable PropertyKeyID for name. It runs on the write
// path only (property assignment). A lock-free fast path returns an
// already-interned id without taking the mutex; only the first interning
// of a previously unseen name serialises under mu to publish the extended
// tables. The steady-state property vocabulary is small and stable.
func (r *PropertyKeyRegistry) Intern(name string) PropertyKeyID {
	if id, ok := r.fwd.Load().m[name]; ok {
		return id
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := r.fwd.Load()
	if id, ok := cur.m[name]; ok { // re-check under the lock
		return id
	}
	names := r.snap.Load()
	id := PropertyKeyID(len(names.names))
	nextNames := &propertyKeyNames{names: make([]string, len(names.names)+1)}
	copy(nextNames.names, names.names)
	nextNames.names[id] = name
	nextFwd := &forwardKeyName{m: make(map[string]PropertyKeyID, len(cur.m)+1)}
	for k, v := range cur.m {
		nextFwd.m[k] = v
	}
	nextFwd.m[name] = id
	// Publish id→name before name→id so a reader that observes the new id
	// via Lookup can already Resolve it.
	r.snap.Store(nextNames)
	r.fwd.Store(nextFwd)
	return id
}

// Lookup returns the PropertyKeyID for name and true when known. It is
// lock-free: it loads the immutable name→id table once and reads it, so
// concurrent per-row property-predicate lookups never serialise nor bounce
// a shared reader-count cache line.
func (r *PropertyKeyRegistry) Lookup(name string) (PropertyKeyID, bool) {
	id, ok := r.fwd.Load().m[name]
	return id, ok
}

// Resolve returns the name interned under id. It is lock-free: it loads
// the immutable id→name snapshot once and indexes into it.
func (r *PropertyKeyRegistry) Resolve(id PropertyKeyID) (string, bool) {
	s := r.snap.Load()
	if uint64(id) >= uint64(len(s.names)) {
		return "", false
	}
	return s.names[id], true
}

// SetNodeProperty records the named property on n with the given
// value, inserting n into the graph if necessary. Returns the error
// from the underlying [adjlist.AdjList.AddNode] when present, or any
// error returned by the installed [SchemaValidator].
func (g *Graph[N, W]) SetNodeProperty(n N, key string, value PropertyValue) error {
	err := g.setNodePropertyInfo(n, key, value, nil)
	g.reclaimAfterDirectWrite(nil)
	return err
}

// setNodePropertyInfo is [Graph.SetNodeProperty] with an explicit commit
// record; info is nil for an autocommit write. See [Graph.setNodeLabelInfo].
func (g *Graph[N, W]) setNodePropertyInfo(n N, key string, value PropertyValue, tx *writeCtx) error {
	if v := g.validator.load(); v != nil {
		if err := v.Validate(key, value); err != nil {
			return err
		}
	}
	// ONE mapper shard acquisition, not two (rmp #2360). Mapper.Intern already
	// RETURNS the id it assigned, and [adjlist.AdjList.AddNode] is exactly
	// `mapper.Intern(n); return nil` — so the Lookup that used to follow it re-took
	// the same shard's lock to re-resolve a key the intern had just resolved. The id
	// is identical by construction: the mapper's slot assignment is permanent by
	// contract, so interning a key twice yields the same NodeID, and reading it from
	// the intern is the same value the Lookup returned.
	//
	// The reference engines do not pay this either: PostgreSQL and InnoDB resolve a
	// tuple's identity once per write, and Memgraph's accessor carries the vertex
	// pointer rather than re-looking it up per store.
	id := g.adj.Mapper().Intern(n)
	keyID := g.propKeys().Intern(key)
	s := g.nodePropShardFor(id)
	s.mu.Lock()
	// propBag is stored by value, so mutate a local copy and write it back.
	// In the small tier set may append to (and thus reallocate) the pairs
	// slice, and a first Set on a new node starts from the zero bag; the
	// write-back makes both visible.
	bag := s.m[id]
	// MVCC P2 (rmp #2279), inert unless armed. The undo depends on what was
	// there before: deleting the key when it was absent, restoring the
	// pre-image when it was not. A write that changes nothing records nothing,
	// for the same reason the label path guards on has().
	if g.propDeltasEnabled() {
		prev, had := bag.get(keyID)
		// The conflict test runs UNCONDITIONALLY, and that is the fix for rmp #2324.
		//
		// It used to sit behind a `record` guard — skipped when the value being
		// written already equalled the stored one — on the reasoning that "only a
		// write that RECORDS a version can conflict". That reasoning compares the
		// incoming value against the LIVE stored value, and a stale writer's value
		// can equal it by arithmetic coincidence, which is exactly what a lost
		// update looks like: A reads 1 and writes 2; B, whose snapshot also says 1,
		// computes 2 as well; B's write is judged a no-op against the now-stored 2,
		// the conflict test is skipped, no version is recorded, and B's statement
		// reports SUCCESS having applied nothing. Measured at 400 concurrent
		// increments producing a final value of 216, with one value written by five
		// different successful statements.
		//
		// Testing the head first cannot reintroduce the abort the guard was added to
		// prevent. MERGE's MATCH branch re-asserts a property it just read, so the
		// head it re-asserts over is visible to its own transaction and does not
		// conflict; a head that is NOT visible means another transaction changed the
		// object underneath, and refusing that is the correct answer rather than an
		// unwanted one.
		if head := s.headStamp(id); tx.conflicts(head) {
			s.mu.Unlock()
			return tx.conflictErr(mvcc.StoreNodeProperties, head)
		}
		switch {
		case !had:
			ci, ts := g.deltaStamp(tx.record())
			s.pushPropDelta(id, undoDelProp, keyID, PropertyValue{}, ci, ts, &g.propDeltaActive)
		case !propValuesDefinitelyEqual(prev, value):
			ci, ts := g.deltaStamp(tx.record())
			s.pushPropDelta(id, undoSetProp, keyID, prev, ci, ts, &g.propDeltaActive)
		}
	}
	bag.set(keyID, value)
	s.m[id] = bag
	s.mu.Unlock()
	// The write is CLAIMED in this store; now cross-check the existence store —
	// a pending DETACH DELETE of this node by another transaction refuses the
	// write, symmetrically with the delete's own property-head cross-check
	// (rmp #2444; ordering argument in mvcc_node_conflict.go).
	return g.crossCheckNodeLife(id, tx)
}

// GetNodeProperty returns the property value attached to n under
// key, and a bool reporting whether the property is set.
func (g *Graph[N, W]) GetNodeProperty(n N, key string) (PropertyValue, bool) {
	id, ok := g.adj.Mapper().Lookup(n)
	if !ok {
		return PropertyValue{}, false
	}
	keyID, ok := g.propKeys().Lookup(key)
	if !ok {
		return PropertyValue{}, false
	}
	s := g.nodePropShardFor(id)
	s.mu.RLock()
	defer s.mu.RUnlock()
	bag, ok := s.m[id]
	if !ok {
		return PropertyValue{}, false
	}
	return bag.get(keyID)
}

// DelNodeProperty removes the named property from n. No-op if absent.
func (g *Graph[N, W]) DelNodeProperty(n N, key string) {
	g.delNodePropertyInfo(n, key, nil)
	g.reclaimAfterDirectWrite(nil)
}

// delNodePropertyInfo is [Graph.DelNodeProperty] with an explicit commit
// record; info is nil for an autocommit write. See [Graph.setNodeLabelInfo].
func (g *Graph[N, W]) delNodePropertyInfo(n N, key string, tx *writeCtx) {
	id, ok := g.adj.Mapper().Lookup(n)
	if !ok {
		return
	}
	keyID, ok := g.propKeys().Lookup(key)
	if !ok {
		return
	}
	s := g.nodePropShardFor(id)
	// THE SHARED-LOCK PRE-PASS (rmp #2710).
	//
	// # What it is for
	//
	// Measured on dst-concurrent-bolt@64 (Apple M4, 10 cores, 2000 operations,
	// counters in the task record): of 3 190 784 calls that reached the
	// EXCLUSIVE shard lock, 451 024 (14.1%) found no bag for the node at all
	// and 1 204 394 (37.7%) found a bag without the key — 51.9% that mutated
	// nothing — and a further 1 466 514 (46.0%) were refused by the write-write
	// conflict test, which also mutates nothing. Only 68 852 (2.2%) removed a
	// property. So 97.8% of the exclusive acquisitions took a lock that
	// excludes every other writer on the shard in order to change no state.
	//
	// That is worth fixing here rather than by narrowing the critical section,
	// because the body is not what costs. In the same window 87% of this
	// function's CPU was the lock and unlock machinery itself and 9% was
	// everything else it does, so there is no expensive callee to hoist the way
	// rmp #2681 hoisted [label.Index.Add] out of the label shard's lock — the
	// analogous lever does not exist on this path. The number of EXCLUSIVE
	// acquisitions is the lever, and a question answered under a read lock
	// costs no writer anything.
	//
	// # Why a read lock is enough for these three outcomes
	//
	// The cross-store rule in mvcc_node_conflict.go is that each side CLAIMS in
	// its own store under that shard's lock BEFORE it cross-checks the others,
	// and that ordering is what makes the detection race-free without a
	// per-node lock. NONE of the three outcomes resolved here makes a claim:
	// the delta push and the bag mutation are both reached only when the key is
	// present and the conflict test passed, and that outcome is the one case
	// this pre-pass declines to answer. A no-op delete publishes nothing, so
	// there is no claim whose ordering could be disturbed.
	//
	// The read lock still excludes every writer, so the bag, the delta head and
	// the decision taken from them are one consistent observation — exactly the
	// guarantee the exclusive lock gave. What changes is only that the same
	// observation no longer serialises the other readers of this shard.
	//
	// A concurrent writer that adds the key between this pre-pass and the
	// caller's return is not a lost update and not a new window: the exclusive
	// form permits the identical interleaving, since a delete that arrives
	// before the write is a valid serial order in both.
	switch g.delNodePropertyShared(s, id, keyID, tx) {
	case delPropRefused:
		// The conflict path returns WITHOUT the existence cross-check, exactly
		// as the exclusive body below does — see its own early return.
		return
	case delPropNothingToRemove:
		_ = g.crossCheckNodeLife(id, tx)
		return
	}
	s.mu.Lock()
	if bag, ok2 := s.m[id]; ok2 {
		// MVCC P2 (rmp #2279), inert unless armed. Deleting a key that is not
		// there changes nothing and records nothing; deleting one that is there
		// records the pre-image so a reader can restore it.
		if g.propDeltasEnabled() {
			if prev, had := bag.get(keyID); had {
				// See setNodePropertyInfo; delNodePropertyInfo cannot return, so
				// the conflict is recorded on the transaction and commit
				// refuses it. See [writeCtx.conflictErr].
				if head := s.headStamp(id); tx.conflicts(head) {
					_ = tx.conflictErr(mvcc.StoreNodeProperties, head)
					s.mu.Unlock()
					return
				}
				ci, ts := g.deltaStamp(tx.record())
				s.pushPropDelta(id, undoSetProp, keyID, prev, ci, ts, &g.propDeltaActive)
			}
		}
		// propBag is stored by value; write the mutated copy back, dropping
		// the node entry entirely when the last property goes so an empty
		// bag never lingers (preserving the prior delete-when-empty contract).
		if bag.del(keyID) {
			delete(s.m, id)
		} else {
			s.m[id] = bag
		}
	}
	s.mu.Unlock()
	// Symmetric existence cross-check after the claim (rmp #2444); this path
	// cannot return an error, so the conflict is recorded on the transaction
	// and commit refuses it, exactly like the in-shard check above.
	_ = g.crossCheckNodeLife(id, tx)
}

// delPropOutcome is what the shared-lock pre-pass of
// [Graph.delNodePropertyInfo] was able to settle without an exclusive lock.
type delPropOutcome uint8

const (
	// delPropNeedsExclusive means the key is present and the write is not
	// refused, so the removal must be redone under the exclusive lock. The
	// pre-pass decides NOTHING in this case: everything it observed is re-read
	// there, because the shard is unlocked in between and a concurrent writer
	// may have changed the bag.
	delPropNeedsExclusive delPropOutcome = iota
	// delPropNothingToRemove means the node carries no bag, or a bag without
	// this key, so the removal has nothing to do. The caller still runs the
	// existence cross-check, exactly as the exclusive body does on this outcome.
	delPropNothingToRemove
	// delPropRefused means the write-write conflict test refused the write and
	// recorded it on the transaction. The caller returns immediately WITHOUT
	// the existence cross-check.
	//
	// THIS OUTCOME IS WHY THE PRE-PASS RETURNS THREE VALUES AND NOT A BOOL.
	// The exclusive body's conflict branch unlocks and returns without reaching
	// [Graph.crossCheckNodeLife], while every other path falls through to it. A
	// two-valued pre-pass would have collapsed "refused" into "nothing to
	// remove" and started cross-checking a node whose write was already
	// refused — a behaviour change on the cross-store seam of rmp #2444,
	// invisible to any test that does not race a DETACH DELETE against a
	// refused property delete.
	delPropRefused
)

// delNodePropertyShared answers, under the shard's READ lock, whether a delete
// of keyID from id needs the exclusive lock at all. See the pre-pass commentary
// in [Graph.delNodePropertyInfo] for why a read lock suffices for the two
// outcomes it settles, and for the measurement that motivates it.
//
// It never mutates the shard. The only state it can change is the
// TRANSACTION's, through [writeCtx.conflictErr] on the refused path, which
// records the conflict on tx and touches no shard at all.
func (g *Graph[N, W]) delNodePropertyShared(s *nodePropShard, id graph.NodeID, keyID PropertyKeyID, tx *writeCtx) delPropOutcome {
	s.mu.RLock()
	bag, ok := s.m[id]
	if !ok {
		s.mu.RUnlock()
		return delPropNothingToRemove
	}
	if _, had := bag.get(keyID); !had {
		// The key is absent, so [propBag.del] under the exclusive lock would
		// rescan the same buffer, remove nothing and write the identical bag
		// back. The ONE thing it would still do is drop an entry whose bag is
		// empty, preserving the delete-when-empty contract — so an empty bag is
		// handed to the exclusive path rather than settled here.
		//
		// No writer stores an empty bag today: [Graph.setNodePropertyInfo]
		// always stores at least the key it just set, and this function's
		// exclusive body and both abort reclaimers in mvcc_abort_reclaim.go
		// delete the entry instead when the bag empties. The guard is defence
		// in depth against a future writer that forgets, not a case reached now.
		empty := bag.empty()
		s.mu.RUnlock()
		if empty {
			return delPropNeedsExclusive
		}
		return delPropNothingToRemove
	}
	if g.propDeltasEnabled() {
		// The same unconditional head test the exclusive body runs, over the
		// same shard lock held in shared mode, so the head and the decision
		// taken from it are ONE consistent observation. A refusal claims
		// nothing, which is what lets it be settled without excluding writers.
		if head := s.headStamp(id); tx.conflicts(head) {
			_ = tx.conflictErr(mvcc.StoreNodeProperties, head)
			s.mu.RUnlock()
			return delPropRefused
		}
	}
	s.mu.RUnlock()
	return delPropNeedsExclusive
}

// NodeProperties returns a snapshot of every property currently
// attached to n.
func (g *Graph[N, W]) NodeProperties(n N) map[string]PropertyValue {
	id, ok := g.adj.Mapper().Lookup(n)
	if !ok {
		return nil
	}
	s := g.nodePropShardFor(id)
	s.mu.RLock()
	bag, ok := s.m[id]
	if !ok {
		s.mu.RUnlock()
		return nil
	}
	out := make(map[string]PropertyValue, bag.len())
	bag.forEach(func(k PropertyKeyID, v PropertyValue) {
		if name, ok := g.propKeys().Resolve(k); ok {
			out[name] = v
		}
	})
	s.mu.RUnlock()
	return out
}

// NodePropertiesByID is the NodeID-keyed counterpart of [Graph.NodeProperties].
// It skips the external-key → NodeID Mapper lookup, so callers that already
// hold the NodeID — chiefly the Cypher result-materialisation path, which
// resolves the NodeID once for identity and then needs both properties and
// labels — avoid a redundant Mapper round-trip per node. The returned map is a
// fresh copy owned by the caller; it is nil when id has no recorded
// properties. Concurrency-safe under the same contract as NodeProperties.
func (g *Graph[N, W]) NodePropertiesByID(id graph.NodeID) map[string]PropertyValue {
	s := g.nodePropShardFor(id)
	s.mu.RLock()
	bag, ok := s.m[id]
	if !ok {
		s.mu.RUnlock()
		return nil
	}
	out := make(map[string]PropertyValue, bag.len())
	bag.forEach(func(k PropertyKeyID, v PropertyValue) {
		if name, ok := g.propKeys().Resolve(k); ok {
			out[name] = v
		}
	})
	s.mu.RUnlock()
	return out
}

// NodePropertiesByIDFunc invokes visit once per property attached to the node
// identified by id, passing the resolved property name and a value copy of the
// PropertyValue. It is the allocation-fusing counterpart of
// [Graph.NodePropertiesByID]: callers that immediately re-key every property
// into a different map (chiefly the Cypher result-materialisation path, which
// converts each lpg.PropertyValue into a cypher/expr value) would otherwise
// allocate a throwaway intermediate map[string]PropertyValue only to range over
// it once. Streaming the bag through visit lets the caller build its target map
// directly, removing that intermediate allocation per returned node.
//
// visit is called zero times for a node with no recorded properties (and for an
// unknown id). The iteration order is unspecified, matching Go map iteration.
//
// Concurrency and isolation: visit runs while the property shard's read lock is
// held, so it observes a consistent snapshot of the node's properties relative
// to any concurrent writer holding the shard write lock — identical to the
// guarantee of [Graph.NodePropertiesByID]. visit therefore MUST NOT call back
// into any Graph method that takes a property-shard lock (it would deadlock) and
// MUST NOT retain the PropertyValue beyond the callback in a way that aliases
// graph-internal state; the PropertyValue passed in is a value copy, so copying
// it out (or deriving an independent value from it) is safe and is the intended
// use.
func (g *Graph[N, W]) NodePropertiesByIDFunc(id graph.NodeID, visit func(name string, pv PropertyValue)) {
	g.NodePropertiesByIDFuncAsOf(id, nil, visit)
}

// NodePropertiesByIDFuncAsOf is [Graph.NodePropertiesByIDFunc] as the node
// stood at snap. A nil snapshot reads the current value; see snapshot_read.go.
//
// Safe for concurrent use.
func (g *Graph[N, W]) NodePropertiesByIDFuncAsOf(id graph.NodeID, snap *Snapshot, visit func(name string, pv PropertyValue)) {
	g.withPropBag(id, snap, func(bag propBag) {
		bag.forEach(func(k PropertyKeyID, v PropertyValue) {
			if name, ok := g.propKeys().Resolve(k); ok {
				visit(name, v)
			}
		})
	})
}

// NodePropertyByID returns the single property keyed by name attached to the
// node identified by id, without materialising the node's full property map. It
// is the single-key counterpart of [Graph.NodePropertiesByID] and exists for
// the Cypher scalar-projection fast path: a predicate or projection that reads
// only n.name from a bound node fetches just that one value instead of copying
// every property into a fresh map per row.
//
// The boolean reports whether the property is present (false for both an
// unknown key name and a node that carries no such property), mirroring the
// missing-key-is-null semantics of openCypher property access. The returned
// PropertyValue is a value copy owned by the caller. Concurrency-safe under the
// same contract as [Graph.NodeProperties]: the read holds the property shard's
// read lock for the duration of the lookup, so it observes a consistent view of
// the node's properties relative to any concurrent writer holding the shard
// write lock.
func (g *Graph[N, W]) NodePropertyByID(id graph.NodeID, key string) (PropertyValue, bool) {
	// Resolve the key name to its interned id without interning a new one: an
	// unknown key cannot be present on any node, so a miss here is a definite
	// "absent" answer and avoids polluting the registry with query-time names.
	kid, ok := g.propKeys().Lookup(key)
	if !ok {
		return PropertyValue{}, false
	}
	s := g.nodePropShardFor(id)
	s.mu.RLock()
	bag, ok := s.m[id]
	if !ok {
		s.mu.RUnlock()
		return PropertyValue{}, false
	}
	v, ok := bag.get(kid)
	s.mu.RUnlock()
	return v, ok
}
