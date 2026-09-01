// Package btree provides an order-preserving property index over a
// constraints.Ordered value type, answering range predicates against
// the NodeIDs that carry each value.
//
// The implementation is a cache-friendly in-memory B+ tree (task
// #1514): all (value, NodeID-set) data lives in the leaves and internal
// nodes hold separator keys + child pointers. Insert and Delete of a
// distinct key are O(log n); point reads (Lookup, Cardinality) are
// O(log n); a range scan is O(log n + k) over the k keys it spans; and
// [Index.BulkLoad] builds the tree bottom-up in O(n) from sorted input.
// This replaces the original sorted-array index, whose per-key Insert
// and Delete were O(n) (an array shift) — the win is on write-heavy
// indexed workloads while every read path keeps its prior complexity.
// The tree internals live in bplus.go.
//
// # Concurrency
//
// Every operation is safe for concurrent use by any number of
// goroutines, and NO read path takes a lock on the tree (task #2683).
//
// The tree is an IMMUTABLE snapshot published through an
// [atomic.Pointer]. A read performs one atomic load and then traverses
// the snapshot with no synchronisation at all; nothing it can reach
// will ever change shape underneath it. A structural write — creating
// or destroying a key — copies the root-to-leaf path, leaves every
// off-path node shared, and publishes the new spine with one atomic
// store, serialised against other structural writes by a mutex that no
// reader ever acquires. Adding or removing a NodeID under an EXISTING
// key touches no tree node at all: it takes only that key's own lock,
// so two writers on different keys never interact and a writer never
// blocks a reader of another key.
//
// This is the Lehman & Yao premise restored. PostgreSQL's own B-tree
// README (REL_17_2, src/backend/access/nbtree/README lines 63-68)
// records why it could not use it: "Lehman and Yao don't require read
// locks, but assume that in-memory copies of tree pages are unshared.
// Postgres shares in-memory buffers among backends. As a result, we do
// page-level read locking on btree pages in order to guarantee that no
// record is modified while we are examining it. This reduces
// concurrency but guarantees correct behavior." A copy-on-write
// snapshot makes the pages a reader examines genuinely unshared with
// any writer, so the read lock that assumption forced is not needed
// here.
//
// What a scan guarantees:
//
//   - The KEY SET a scan observes is snapshot-atomic. It is exactly the
//     set of keys the index held at the instant of the scan's atomic
//     load: a concurrent structural write can neither add a key to a
//     scan already in flight nor take one away from it, and a scan can
//     never observe a half-applied split or a detached node.
//   - Each key's NodeID set is read at the instant the scan REACHES
//     that key, not all at one instant. A multi-key scan
//     ([Index.Range], [Index.RangeFrom], [Index.RangeCount],
//     [Index.RangeCountFrom], [Index.Serialize]) is therefore not a
//     point-in-time image of the node sets, and may mix a node added
//     under an early key before the scan started with one added under a
//     late key after it started.
//   - SINGLE-key operations ([Index.Lookup], [Index.LookupAppend],
//     [Index.Cardinality], [Index.RangeFirst]) ARE atomic with respect
//     to that key's node set: they hold the key's lock while reading it.
//
// The per-key relaxation is sound for both consumers in this module,
// and both were verified rather than assumed:
//
//   - cypher/exec/scan_index_btree.go documents the range result as an
//     inclusive SUPERSET and stacks a MANDATORY residual predicate
//     Filter that re-checks the property against the live node, so a
//     stale or early NodeID is rejected there, exactly as an
//     out-of-interval one already is.
//   - [Index.Serialize]'s only production caller is the checkpointer,
//     which drains writers to zero before capture and refuses a capture
//     instant taken while a write transaction is open
//     (store/snapshot/capture.go, ErrCaptureNotQuiesced). Serializing a
//     concurrently mutating index is outside that contract.
//
// The index provides index-internal isolation only; transaction
// isolation across multiple calls is the engine's responsibility.
//
// [Index.BulkLoad], [Index.BulkLoadSorted] and [Index.Deserialize]
// REPLACE the whole index rather than editing it. They publish a tree
// of brand-new entries, so a concurrent Insert or Delete that had
// already resolved a key against the outgoing tree writes to an entry
// the new tree does not contain and is lost. They are load-time
// operations — backfill before registration, snapshot recovery — and
// every caller in this module runs them before the index is shared;
// callers must not race them against index maintenance.
//
// # Key ordering
//
// Keys are ordered by the TOTAL order of [cmp.Compare] / [cmp.Less],
// not by the raw < operator. The two orders agree everywhere except
// IEEE 754 NaN: under the total order a floating-point NaN key is
// less than every other value (including math.Inf(-1)), every NaN bit
// pattern compares equal to every other NaN, and ±0.0 are one key.
// Raw < is only a partial order over floats — every comparison with
// NaN is false — so a single NaN insert used to break the monotone
// predicate that [sort.Search] requires and silently corrupted the
// index for ordinary keys (task #1354). With the total order the
// sorted invariant holds for every representable input: NaN is a
// regular, deduplicated key that Lookup/Delete address and that no
// range with a non-NaN lower bound ever returns.
package btree

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/RoaringBitmap/roaring/v2/roaring64"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
)

// ErrMismatchedLengths is returned by [Index.BulkLoad] when the
// values and nodes slices supplied to it do not share a common
// length. Before sprint 21 this condition panicked; the error
// returned here lets callers handle it as a recoverable input
// validation failure.
var ErrMismatchedLengths = errors.New("btree: values and nodes slices must have the same length")

// ErrNotSorted is returned by [Index.BulkLoadSorted] when the values slice is
// not in ascending total order, so the caller's pre-sorted precondition is
// violated. It is a recoverable input-validation failure; the index is left
// untouched.
var ErrNotSorted = errors.New("btree: values must be in ascending order for BulkLoadSorted")

// Index is an order-preserving property index keyed by V, backed by an
// in-memory B+ tree (see bplus.go). It is safe for concurrent use by any
// number of goroutines; see the Concurrency section of the package
// documentation for exactly what a concurrent scan observes.
type Index[V cmp.Ordered] struct {
	// tree is the published, IMMUTABLE snapshot. Every read path resolves it
	// with a single atomic load and then traverses it lock-free.
	tree atomic.Pointer[bplus[V]]

	// mu serialises STRUCTURAL writers only — the path copies that create or
	// destroy a key, and the wholesale replacements. No read path ever takes
	// it, and neither does a write that only adds or removes a NodeID under an
	// existing key. Lock order is mu → entry.mu, never the reverse.
	mu sync.Mutex

	// binding, when non-nil, ties the index to one (label, property) pair of
	// a live node graph so [Index.Apply] can translate [index.Change] events
	// into typed Insert / Delete calls. It is set once by [NewBound] before
	// the index is shared and never mutated afterwards, so Apply reads it
	// without synchronisation. See bound.go.
	binding *Binding[V]
}

// New returns an empty index.
func New[V cmp.Ordered]() *Index[V] {
	i := &Index[V]{}
	i.tree.Store(newBplus[V]())
	return i
}

// BulkLoad replaces the contents of the index with the given
// (value, node) pairs in O(n log n) time. The pairs slice is left
// untouched. Calling BulkLoad on a non-empty index discards previous
// data. Returns [ErrMismatchedLengths] when len(values) != len(nodes).
// Values are sorted and deduplicated under the total order described
// in the package documentation, so NaN inputs collapse into one
// leading entry instead of corrupting (or, before task #1354, hanging)
// the load.
func (i *Index[V]) BulkLoad(values []V, nodes []graph.NodeID) error {
	if len(values) != len(nodes) {
		return ErrMismatchedLengths
	}
	type pair struct {
		v V
		n graph.NodeID
	}
	pairs := make([]pair, len(values))
	for k := range values {
		pairs[k] = pair{v: values[k], n: nodes[k]}
	}
	// cmp.Less / cmp.Compare (not raw < / ==): the sort comparator must
	// be a total order or NaN inputs land in unspecified positions, and
	// the grouping loop below would never advance past a NaN pair
	// (NaN == NaN is false), appending empty entries forever.
	sort.Slice(pairs, func(a, b int) bool { return cmp.Less(pairs[a].v, pairs[b].v) })
	keys := make([]V, 0, len(pairs))
	sets := make([]index.NodeSet, 0, len(pairs))
	for k := 0; k < len(pairs); {
		j := k
		var set index.NodeSet
		for j < len(pairs) && equalKeys(pairs[j].v, pairs[k].v) {
			set.Add(uint64(pairs[j].n))
			j++
		}
		keys = append(keys, pairs[k].v)
		sets = append(sets, set)
		k = j
	}
	tree := newBplus[V]()
	tree.bulkPack(keys, sets)
	// Publish under mu so the replacement cannot interleave with a structural
	// writer's read-modify-publish. See the package doc for why a wholesale
	// replacement is not linearisable against a concurrent Insert or Delete.
	i.mu.Lock()
	i.tree.Store(tree)
	i.mu.Unlock()
	return nil
}

// BulkLoadSorted is [Index.BulkLoad] for input already in ascending total
// order — the order [Index.Serialize] emits and the snapshot stores. It skips
// the copy-into-pairs and the sort that BulkLoad performs, so a pre-sorted
// load (e.g. snapshot recovery) avoids that throwaway O(n) materialization.
// Equal keys must be adjacent (guaranteed by ascending order) and their nodes
// are unioned into one entry, exactly as BulkLoad does, so for the same data
// the resulting tree is identical.
//
// It returns [ErrMismatchedLengths] when the lengths differ and [ErrNotSorted]
// when values is not ascending. The order precondition is checked by a cheap
// allocation-free scan, so a mis-sorted input is rejected rather than silently
// building a corrupt tree.
func (i *Index[V]) BulkLoadSorted(values []V, nodes []graph.NodeID) error {
	if len(values) != len(nodes) {
		return ErrMismatchedLengths
	}
	for k := 1; k < len(values); k++ {
		if keyLess(values[k], values[k-1]) {
			return ErrNotSorted
		}
	}
	keys := make([]V, 0, len(values))
	sets := make([]index.NodeSet, 0, len(values))
	for k := 0; k < len(values); {
		j := k
		var set index.NodeSet
		for j < len(values) && equalKeys(values[j], values[k]) {
			set.Add(uint64(nodes[j]))
			j++
		}
		keys = append(keys, values[k])
		sets = append(sets, set)
		k = j
	}
	tree := newBplus[V]()
	tree.bulkPack(keys, sets)
	// Publish under mu so the replacement cannot interleave with a structural
	// writer's read-modify-publish. See the package doc for why a wholesale
	// replacement is not linearisable against a concurrent Insert or Delete.
	i.mu.Lock()
	i.tree.Store(tree)
	i.mu.Unlock()
	return nil
}

// isNaN reports whether v is an IEEE 754 NaN — the only value that
// differs from itself. For non-floating-point instantiations the
// comparison is constant-false and the compiler eliminates it.
//
//nolint:gocritic // dupSubExpr: v != v is the canonical generic NaN test (mirrors stdlib cmp.isNaN).
func isNaN[V cmp.Ordered](v V) bool { return v != v }

// equalKeys reports whether two keys are equal under the total order:
// IEEE == everywhere, except that any two NaNs are equal.
func equalKeys[V cmp.Ordered](a, b V) bool {
	if isNaN(a) || isNaN(b) {
		return isNaN(a) && isNaN(b)
	}
	return a == b
}

// Lower-bound search strategy (shared by every method below): the B+
// tree is ordered under the [cmp.Compare] total order, and every
// descent and leaf search goes through that same comparator (keyCompare
// / keyLess in bplus.go). The total order places the single deduplicated
// NaN key before every other value — including -Inf — so it falls out as
// the leftmost key with no special-casing in the tree mechanics: a NaN
// probe lands on it like any other key, and any non-NaN lower bound
// excludes it because NaN < every real key. The NaN rule lives entirely
// inside the comparator (task #1354).

// Insert records that node carries value. Keys follow the total
// order described in the package documentation, so a floating-point
// NaN is a valid key: it sorts before every other value and all NaN
// bit patterns share one entry. Inserting a new distinct key is
// O(log n).
//
// Adding a node to a key the index ALREADY holds takes no lock but that
// key's own: it does not touch the tree and does not exclude a writer
// on any other key. Creating a new key falls through to the structural
// path, which is serialised across the whole index.
func (i *Index[V]) Insert(value V, node graph.NodeID) {
	if e := i.tree.Load().get(value); e != nil {
		e.mu.Lock()
		if !e.dead {
			e.set.Add(uint64(node))
			e.mu.Unlock()
			return
		}
		// The entry we resolved has been detached: the snapshot we read it
		// from is stale. Adding to it would be invisible to every future
		// reader, so re-resolve against the published tree instead.
		e.mu.Unlock()
	}
	i.insertStructural(value, uint64(node))
}

// insertStructural creates value as a new key, or joins a key another writer
// created first. It is the slow half of [Index.Insert].
func (i *Index[V]) insertStructural(value V, node uint64) {
	i.mu.Lock()
	defer i.mu.Unlock()
	// Re-read under mu: the tree may have moved on since the fast path looked,
	// and a racing structural insert may already have created the key.
	t := i.tree.Load()
	if e := t.get(value); e != nil {
		// The entry cannot be dead: only the detach in [Index.Delete] sets
		// that flag, it does so while holding mu, and it publishes a tree
		// without the key before releasing mu.
		e.mu.Lock()
		e.set.Add(node)
		e.mu.Unlock()
		return
	}
	i.tree.Store(t.cloneInsert(value, node))
}

// Delete removes node from the set associated with value. No-op when
// absent. The (value, node-set) entry is removed when its set becomes
// empty, and a leaf that becomes entirely empty is dropped from the
// tree (see the delete policy in bplus.go). Like [Index.Insert], value
// is matched under the total order, so Delete addresses a NaN-keyed
// entry.
//
// Removing a node that does not empty the key's set takes no lock but
// that key's own; emptying the set falls through to the structural
// detach, which is serialised across the whole index.
func (i *Index[V]) Delete(value V, node graph.NodeID) {
	e := i.tree.Load().get(value)
	if e == nil {
		return
	}
	e.mu.Lock()
	if e.dead {
		// The key was detached after we loaded the snapshot, so as of that
		// load the index did not hold this node under this value and the
		// delete is a no-op. It is a legitimate serialisation: observing
		// dead == true proves the detach had not yet published when we
		// loaded, hence any re-creation of the key published later still,
		// hence it did not complete before this call began.
		e.mu.Unlock()
		return
	}
	nowEmpty := e.set.Remove(uint64(node)) && e.set.IsEmpty()
	e.mu.Unlock()
	if !nowEmpty {
		return
	}
	i.detach(value)
}

// detach removes an emptied key from the tree. It is the slow half of
// [Index.Delete].
//
// The crux of the protocol, and the reason both re-checks below exist: the
// fast-path Insert's !e.dead test and the !e.set.IsEmpty() test here happen
// under the SAME entry.mu, and that is the only thing that orders them.
//
//   - Without the emptiness re-check, an Insert that resurrected the key
//     between [Index.Delete]'s Remove and this detach would be silently lost:
//     the detach would drop a key that had just become non-empty again.
//   - Without the dead flag the fast-path Insert consults, an Insert that
//     resolved the entry before the detach and added to it afterwards would be
//     equally lost: it would write into an entry no published tree contains.
//
// Together they leave exactly two outcomes for that race, both correct. The
// inserter wins the entry lock first, the set is non-empty here and the detach
// ABORTS; or the detach wins, the inserter sees dead and re-resolves against
// the tree this call publishes.
func (i *Index[V]) detach(value V) {
	i.mu.Lock()
	defer i.mu.Unlock()
	t := i.tree.Load()
	e := t.get(value)
	if e == nil {
		// Another writer detached the same emptied key first.
		return
	}
	e.mu.Lock()
	// e.dead cannot be true here: a dead entry is unreachable from the tree
	// published before its detach released mu, and we hold mu. It is re-tested
	// so the invariant is enforced rather than assumed.
	if e.dead || !e.set.IsEmpty() {
		e.mu.Unlock()
		return
	}
	e.dead = true
	e.mu.Unlock()
	i.tree.Store(t.cloneRemove(value))
}

// RangeFirst returns the first NodeID in the smallest indexed value
// not less than lo and not greater than hi, plus that value. The
// second return value reports whether any match exists. It is the
// allocation-free way to peek the first row of a range scan; the
// full union of matches is available via [Index.Range]. Bounds
// compare under the total order, so lo = NaN admits a NaN key while
// any non-NaN lo excludes it.
//
// The value and NodeID returned are read together under that key's own
// lock, so they are a consistent pair even under concurrent writes.
func (i *Index[V]) RangeFirst(lo, hi V) (V, graph.NodeID, bool) {
	var zeroV V
	if cmp.Less(hi, lo) {
		return zeroV, 0, false
	}
	var c cursor[V]
	for c.seek(i.tree.Load(), lo); c.valid(); c.next() {
		k := c.key()
		if cmp.Less(hi, k) {
			return zeroV, 0, false
		}
		e := c.entry()
		e.mu.Lock()
		empty := e.set.IsEmpty()
		var first uint64
		if !empty {
			first = e.set.Minimum()
		}
		e.mu.Unlock()
		if !empty {
			return k, graph.NodeID(first), true
		}
		// A concurrent Delete emptied this key's set but has not yet published
		// the tree without it. The key carries no NodeID, so it is not the
		// first row of anything: skip it exactly as an absent key would be.
		// Only a concurrent writer can produce this state — a Delete that
		// empties a set always detaches the key before it returns.
	}
	return zeroV, 0, false
}

// Range returns a Roaring bitmap that is the union of the per-value
// bitmaps for every key v with lo <= v <= hi under the total order.
// The returned bitmap is freshly allocated; the caller owns it. A NaN
// key is below every other value, so any range with a non-NaN lo —
// including Range(math.Inf(-1), math.Inf(1)) — never returns it.
func (i *Index[V]) Range(lo, hi V) *roaring64.Bitmap {
	out := roaring64.New()
	if cmp.Less(hi, lo) {
		return out
	}
	var c cursor[V]
	for c.seek(i.tree.Load(), lo); c.valid(); c.next() {
		if cmp.Less(hi, c.key()) {
			break
		}
		e := c.entry()
		e.mu.Lock()
		e.set.OrInto(out)
		e.mu.Unlock()
	}
	return out
}

// RangeFrom returns a Roaring bitmap that is the union of the per-value
// bitmaps for every key v with lo <= v under the total order, with NO upper
// bound — it scans from lo to the largest key present. It is the open-ended
// counterpart of [Index.Range] for an unbounded-above predicate (e.g. a string
// range n.name >= 'A'), where no finite sentinel key is a true maximum: a
// variable-length key type such as string has no representable greatest value,
// so capping the scan at any fixed key would silently exclude every key sorting
// above it. Scanning to the last leaf is the only superset-complete way to
// serve an unbounded-above range (#F-CY1). A NaN key is below every other value
// (see [Index.Range]); a non-NaN lo therefore never returns it.
//
// The returned bitmap is freshly allocated; the caller owns it.
func (i *Index[V]) RangeFrom(lo V) *roaring64.Bitmap {
	out := roaring64.New()
	var c cursor[V]
	for c.seek(i.tree.Load(), lo); c.valid(); c.next() {
		e := c.entry()
		e.mu.Lock()
		e.set.OrInto(out)
		e.mu.Unlock()
	}
	return out
}

// RangeCountFrom returns the exact number of NodeIDs whose value is >= lo under
// the total order (no upper bound), with the same early-exit-at-budget contract
// as [Index.RangeCount]. It is the open-ended counterpart used by the
// unbounded-above selectivity gate so the count and the executed [Index.RangeFrom]
// scan agree on the same key space (#F-CY1).
func (i *Index[V]) RangeCountFrom(lo V, budget uint64) (count uint64, exact bool) {
	var total uint64
	var c cursor[V]
	for c.seek(i.tree.Load(), lo); c.valid(); c.next() {
		e := c.entry()
		e.mu.Lock()
		total += e.set.Cardinality()
		e.mu.Unlock()
		if total > budget {
			return budget + 1, false
		}
	}
	return total, true
}

// Lookup returns a clone of the bitmap associated with value, or an
// empty bitmap when value is unknown. Matching uses the total order,
// so Lookup(NaN) returns the NaN entry when one exists.
func (i *Index[V]) Lookup(value V) *roaring64.Bitmap {
	e := i.tree.Load().get(value)
	if e == nil {
		return roaring64.New()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	bm, shared := e.set.Bitmap()
	if shared {
		// The set is in the bitmap state and the returned bitmap aliases
		// the live one; clone so the caller owns an independent copy that
		// a later writer cannot mutate (the pre-refactor Lookup contract).
		return bm.Clone()
	}
	// Inline state: Bitmap already materialised a fresh, caller-owned
	// bitmap — no clone needed.
	return bm
}

// LookupAppend appends the NodeIDs associated with value to dst and returns the
// extended slice — the allocation-light equivalent of [Index.Lookup] for
// callers that iterate the result once, the dominant equality-seek shape. A
// singleton or small posting list appends straight from the set's inline fields
// with no heap allocation when dst has spare capacity (e.g. a reused seek
// buffer), where Lookup clones (or materialises) a full roaring bitmap. Only a
// promoted bitmap entry allocates a single iterator.
//
// Matching uses the total order, so LookupAppend(NaN) appends the NaN entry
// when one exists, exactly as [Index.Lookup] returns it. An unknown value
// appends nothing. The appended ids are an independent snapshot the caller may
// iterate after the call returns, with the same ownership as Lookup's clone.
func (i *Index[V]) LookupAppend(value V, dst []uint64) []uint64 {
	e := i.tree.Load().get(value)
	if e == nil {
		return dst
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.set.AppendTo(dst)
}

// Cardinality returns the number of NodeIDs associated with value,
// matched under the total order (see [Index.Lookup]).
func (i *Index[V]) Cardinality(value V) uint64 {
	e := i.tree.Load().get(value)
	if e == nil {
		return 0
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.set.Cardinality()
}

// DistinctValues returns the number of distinct values currently
// indexed. It is O(1) and takes no lock at all: the count is a field of
// the published snapshot, which the tree maintains as it is built.
func (i *Index[V]) DistinctValues() int {
	return i.tree.Load().count
}

// Kind returns "btree" — satisfies [index.Subscriber].
func (*Index[V]) Kind() string { return "btree" }

// Apply maintains a bound index (see [NewBound]) from the [index.Manager]
// change fan-out; it is a no-op for an unbound index (see [New]), which cannot
// reliably interpret arbitrary [index.Change] values without the
// caller-supplied binding (property key + value-type coercion). The bound
// rules live in [Index.applyBound] (bound.go).
func (i *Index[V]) Apply(c index.Change) {
	if i.binding == nil {
		return
	}
	i.applyBound(c)
}

// RangeCount returns the exact number of NodeIDs whose value falls within the
// inclusive interval [lo, hi] under the total order, but stops accumulating as
// soon as the running total exceeds budget and returns (budget+1, false) — the
// caller learns only that the count is "more than budget" without paying to
// walk the whole range. When the full count is ≤ budget it is returned with
// exact == true.
//
// The entries are pairwise-disjoint node-sets (each node carries exactly one
// value for the property), so the sum of per-entry cardinalities equals the
// union cardinality exactly, with no allocation and no union materialisation
// (graph-theory-expert, #1505). The early-exit bounds the gate cost to
// O(budget) cardinality probes regardless of how many distinct values the
// range spans, which keeps a non-selective range cheap to reject.
func (i *Index[V]) RangeCount(lo, hi V, budget uint64) (count uint64, exact bool) {
	if cmp.Less(hi, lo) {
		return 0, true
	}
	var total uint64
	var c cursor[V]
	for c.seek(i.tree.Load(), lo); c.valid(); c.next() {
		if cmp.Less(hi, c.key()) {
			return total, true
		}
		e := c.entry()
		e.mu.Lock()
		total += e.set.Cardinality()
		e.mu.Unlock()
		if total > budget {
			return budget + 1, false
		}
	}
	return total, true
}

// btreeMagic is the four-byte magic at the head of a serialised
// btree index ('SBTR' little-endian — 0x52544253).
const btreeMagic uint32 = 0x52544253

// btreeFormatVersion is the on-disk format version of a serialised
// btree index.
const btreeFormatVersion uint32 = 1

// btreeCapHintMax caps the eager slice reservation in Deserialize so a
// hostile entryCount (up to the 1<<40 implausibility ceiling) cannot drive
// a multi-terabyte allocation before the per-entry reads have a chance to
// fail on a truncated file. It mirrors the safe sibling ceiling used by
// store/snapshot/tombstones.bin and constraints.bin (1<<20). A legitimately
// large index is unaffected: the slice grows via append.
const btreeCapHintMax uint64 = 1 << 20

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// encodeOrdered serialises one cmp.Ordered value to bytes. The
// supported set matches [hash.Index] for the types that are both
// comparable and ordered.
//
//nolint:gocyclo // type switch over supported ordered kinds
func encodeOrdered[V cmp.Ordered](v V) ([]byte, error) {
	switch x := any(v).(type) {
	case string:
		return []byte(x), nil
	case int64:
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], uint64(x))
		return buf[:], nil
	case int32:
		var buf [4]byte
		binary.LittleEndian.PutUint32(buf[:], uint32(x))
		return buf[:], nil
	case int:
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], uint64(x))
		return buf[:], nil
	case uint64:
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], x)
		return buf[:], nil
	case uint32:
		var buf [4]byte
		binary.LittleEndian.PutUint32(buf[:], x)
		return buf[:], nil
	case uint:
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], uint64(x))
		return buf[:], nil
	case float64:
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], math.Float64bits(x))
		return buf[:], nil
	}
	return nil, fmt.Errorf("%w: %T", index.ErrIndexValueTypeUnsupported, v)
}

// decodeOrdered is the inverse of [encodeOrdered].
//
//nolint:gocyclo // type switch over supported ordered kinds
func decodeOrdered[V cmp.Ordered](b []byte) (V, error) {
	var zero V
	switch any(zero).(type) {
	case string:
		var out V
		assignAny(&out, string(b))
		return out, nil
	case int64:
		if len(b) != 8 {
			return zero, fmt.Errorf("%w: int64 wants 8 bytes, got %d",
				index.ErrIndexCorrupted, len(b))
		}
		var out V
		assignAny(&out, int64(binary.LittleEndian.Uint64(b)))
		return out, nil
	case int32:
		if len(b) != 4 {
			return zero, fmt.Errorf("%w: int32 wants 4 bytes, got %d",
				index.ErrIndexCorrupted, len(b))
		}
		var out V
		assignAny(&out, int32(binary.LittleEndian.Uint32(b)))
		return out, nil
	case int:
		if len(b) != 8 {
			return zero, fmt.Errorf("%w: int wants 8 bytes, got %d",
				index.ErrIndexCorrupted, len(b))
		}
		var out V
		assignAny(&out, int(int64(binary.LittleEndian.Uint64(b))))
		return out, nil
	case uint64:
		if len(b) != 8 {
			return zero, fmt.Errorf("%w: uint64 wants 8 bytes, got %d",
				index.ErrIndexCorrupted, len(b))
		}
		var out V
		assignAny(&out, binary.LittleEndian.Uint64(b))
		return out, nil
	case uint32:
		if len(b) != 4 {
			return zero, fmt.Errorf("%w: uint32 wants 4 bytes, got %d",
				index.ErrIndexCorrupted, len(b))
		}
		var out V
		assignAny(&out, binary.LittleEndian.Uint32(b))
		return out, nil
	case uint:
		if len(b) != 8 {
			return zero, fmt.Errorf("%w: uint wants 8 bytes, got %d",
				index.ErrIndexCorrupted, len(b))
		}
		var out V
		assignAny(&out, uint(binary.LittleEndian.Uint64(b)))
		return out, nil
	case float64:
		if len(b) != 8 {
			return zero, fmt.Errorf("%w: float64 wants 8 bytes, got %d",
				index.ErrIndexCorrupted, len(b))
		}
		var out V
		assignAny(&out, math.Float64frombits(binary.LittleEndian.Uint64(b)))
		return out, nil
	}
	return zero, fmt.Errorf("%w: %T", index.ErrIndexValueTypeUnsupported, zero)
}

// assignAny copies src into *dst, treating dst as an any.
func assignAny[V any](dst *V, src any) {
	*dst = src.(V)
}

// Serialize writes every (value, NodeID-set) pair in key order to w.
// The on-disk layout is:
//
//	uint32 magic ('SBTR')
//	uint32 formatVersion
//	uint64 entryCount
//	repeat entryCount times:
//	  uint32 keyLen
//	  [keyLen]byte key (kind-specific encoding)
//	  uint64 idCount
//	  [idCount]uint64 NodeIDs (sorted ascending)
//	uint32 crc32c (little-endian)
//
// Writing in key order lets [Deserialize] use [Index.BulkLoad]
// indirectly: the reader appends one entry at a time and the sorted
// order is preserved.
func (i *Index[V]) Serialize(w io.Writer) error {
	bw := bufio.NewWriterSize(w, 1<<16)
	hasher := crc32.New(castagnoli)
	tee := io.MultiWriter(bw, hasher)

	if err := binary.Write(tee, binary.LittleEndian, btreeMagic); err != nil {
		return err
	}
	if err := binary.Write(tee, binary.LittleEndian, btreeFormatVersion); err != nil {
		return err
	}

	// One snapshot for the header count AND the body, so the declared entry
	// count always matches the number of entries written.
	t := i.tree.Load()
	if err := binary.Write(tee, binary.LittleEndian, uint64(t.count)); err != nil {
		return err
	}
	// Walk the snapshot in ascending key order — the byte-identical layout the
	// v1 sorted-array writer produced (the wire format is a logical key→nodes
	// mapping; the tree shape is not serialised). storage-engine-auditor
	// #1514: formatVersion stays 1.
	var c cursor[V]
	for c.seekFirst(t); c.valid(); c.next() {
		key, err := encodeOrdered(c.key())
		if err != nil {
			return err
		}
		if uint64(len(key)) > uint64(^uint32(0)) {
			return fmt.Errorf("btree: key too long to serialize: %d", len(key))
		}
		if err := binary.Write(tee, binary.LittleEndian, uint32(len(key))); err != nil {
			return err
		}
		if _, err := tee.Write(key); err != nil {
			return err
		}
		e := c.entry()
		e.mu.Lock()
		ids := e.set.ToArray()
		e.mu.Unlock()
		if err := binary.Write(tee, binary.LittleEndian, uint64(len(ids))); err != nil {
			return err
		}
		if err := binary.Write(tee, binary.LittleEndian, ids); err != nil {
			return err
		}
	}

	if err := binary.Write(bw, binary.LittleEndian, hasher.Sum32()); err != nil {
		return err
	}
	return bw.Flush()
}

// Deserialize replaces the receiver's state with the contents of r.
// Because the writer dumps entries in ascending key order, the
// reader can build the sorted entries slice directly without an
// extra sort pass; the loader is therefore O(n) instead of
// [Index.BulkLoad]'s O(n log n).
//
// Keys must be STRICTLY ascending under the [cmp.Compare] total order
// — the only shape [Index.Serialize] produces. A payload that
// violates it fails fail-stop with [index.ErrIndexCorrupted] rather
// than load an index whose binary searches would silently miss live
// keys. In particular, a float64 payload written before the
// total-order fix (task #1354) that carries a NaN key after a real
// key, or duplicate NaN entries, is rejected; the index is derived
// data, so the caller recovers by rebuilding it from the primary
// graph. A single NaN entry in the leading position is the legitimate
// post-fix encoding and loads normally.
//
//nolint:gocyclo // index deserialize: header + per-entry decode + per-step bounds checks
func (i *Index[V]) Deserialize(r io.Reader) error {
	all, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("%w: read: %w", index.ErrIndexCorrupted, err)
	}
	if len(all) < 4 {
		return fmt.Errorf("%w: short payload", index.ErrIndexCorrupted)
	}
	body := all[:len(all)-4]
	trailer := binary.LittleEndian.Uint32(all[len(all)-4:])
	if got := crc32.Checksum(body, castagnoli); got != trailer {
		return fmt.Errorf("%w: crc32c mismatch: got %d, want %d",
			index.ErrIndexCorrupted, got, trailer)
	}

	br := bufio.NewReader(bytes.NewReader(body))
	var magic, version uint32
	if err := binary.Read(br, binary.LittleEndian, &magic); err != nil {
		return fmt.Errorf("%w: magic: %w", index.ErrIndexCorrupted, err)
	}
	if magic != btreeMagic {
		return fmt.Errorf("%w: bad magic %#x", index.ErrIndexCorrupted, magic)
	}
	if err := binary.Read(br, binary.LittleEndian, &version); err != nil {
		return fmt.Errorf("%w: version: %w", index.ErrIndexCorrupted, err)
	}
	if version != btreeFormatVersion {
		return fmt.Errorf("%w: unsupported format version %d",
			index.ErrIndexCorrupted, version)
	}
	var entryCount uint64
	if err := binary.Read(br, binary.LittleEndian, &entryCount); err != nil {
		return fmt.Errorf("%w: entryCount: %w", index.ErrIndexCorrupted, err)
	}
	if entryCount > 1<<40 {
		return fmt.Errorf("%w: implausible entryCount %d",
			index.ErrIndexCorrupted, entryCount)
	}

	// Clamp the eager reservation: a hostile entryCount (up to the 1<<40
	// implausibility ceiling) must not pre-allocate a multi-terabyte buffer
	// before the per-entry reads fail on a truncated file (storage-engine-
	// auditor #1514, mirroring #1480). The transient slices grow via append;
	// the tree is built bottom-up from them only AFTER validation succeeds.
	hint := entryCount
	if hint > btreeCapHintMax {
		hint = btreeCapHintMax
	}
	keys := make([]V, 0, hint)
	sets := make([]index.NodeSet, 0, hint)
	var prev V
	hasPrev := false
	// scratch de-reflects the per-key fixed headers (keyLen, idCount): the
	// previous binary.Read(br, LE, &scalar) boxed the destination pointer into
	// `any`, costing one allocation per scalar per key (~2 per key). io.ReadFull
	// into a reused 8-byte buffer + binary.LittleEndian is byte-identical and
	// allocation-free. The recovery path decodes one of these per index entry.
	var scratch [8]byte
	for e := uint64(0); e < entryCount; e++ {
		if _, err := io.ReadFull(br, scratch[:4]); err != nil {
			return fmt.Errorf("%w: keyLen: %w", index.ErrIndexCorrupted, err)
		}
		keyLen := binary.LittleEndian.Uint32(scratch[:4])
		if uint64(keyLen) > uint64(len(body)) {
			return fmt.Errorf("%w: implausible keyLen %d",
				index.ErrIndexCorrupted, keyLen)
		}
		kbuf := make([]byte, keyLen)
		if _, err := io.ReadFull(br, kbuf); err != nil {
			return fmt.Errorf("%w: key bytes: %w", index.ErrIndexCorrupted, err)
		}
		v, derr := decodeOrdered[V](kbuf)
		if derr != nil {
			return derr
		}
		if hasPrev && cmp.Compare(v, prev) <= 0 {
			return fmt.Errorf("%w: keys not in strictly ascending order",
				index.ErrIndexCorrupted)
		}
		prev = v
		hasPrev = true
		if _, err := io.ReadFull(br, scratch[:8]); err != nil {
			return fmt.Errorf("%w: idCount: %w", index.ErrIndexCorrupted, err)
		}
		idCount := binary.LittleEndian.Uint64(scratch[:8])
		// Each id occupies 8 wire bytes, so len(body)/8 is the true ceiling on
		// how many ids the component's bytes could hold. The prior len(body)
		// bound admitted an ~8x over-declaration, forcing make([]uint64, idCount)
		// (plus binary.Read's transient buffer, ~16x total) to allocate before
		// the short read failed. Bounded amplification, tightened for
		// defense-in-depth to match the len/elemSize cap-hint pattern elsewhere.
		if idCount > uint64(len(body))/8 {
			return fmt.Errorf("%w: implausible idCount %d",
				index.ErrIndexCorrupted, idCount)
		}
		ids := make([]uint64, idCount)
		if err := binary.Read(br, binary.LittleEndian, ids); err != nil {
			return fmt.Errorf("%w: ids: %w", index.ErrIndexCorrupted, err)
		}
		// ids is strictly ascending (the writer emits ToArray order and the
		// reader does not reorder), so NodeSetFromSorted picks the cheapest
		// representation without a re-sort. Ownership of ids transfers.
		keys = append(keys, v)
		sets = append(sets, index.NodeSetFromSorted(ids))
	}

	// Build the tree bottom-up from the validated, strictly-ascending entries
	// in O(n). The strict-ascending check above gated admission, so a corrupt
	// non-ascending payload never reaches this point (auditor condition C4a).
	tree := newBplus[V]()
	tree.bulkPack(keys, sets)
	// Publish under mu so the replacement cannot interleave with a structural
	// writer's read-modify-publish. See the package doc for why a wholesale
	// replacement is not linearisable against a concurrent Insert or Delete.
	i.mu.Lock()
	i.tree.Store(tree)
	i.mu.Unlock()
	return nil
}
