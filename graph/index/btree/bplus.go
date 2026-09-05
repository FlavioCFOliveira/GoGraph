package btree

// bplus.go — the cache-friendly, IMMUTABLE in-memory B+ tree that backs
// [Index] (task #1514; converted to a copy-on-write snapshot with lock-free
// readers, task #2683).
//
// # Structure
//
// A classic B+ tree: all (key, payload) data lives in the LEAVES; internal
// nodes hold only separator keys and child pointers. Each leaf slot addresses
// its payload by POINTER (an [entry]), never by value, so copying a leaf
// duplicates only the pointer and every copy of the tree shares ONE payload
// and ONE lock per key.
//
// There is NO leaf chain. Leaves used to be singly linked low→high, and every
// forward scan walked that chain. Under copy-on-write that link is
// unmaintainable: path-copying one leaf leaves its PREDECESSOR's next pointer
// aimed at the stale copy, and the predecessor is not on the copy path, so
// repairing it would cost O(n) per structural write. Forward iteration is a
// [cursor] instead — a small stack of (internal node, child index) frames, one
// per level, built by the lower-bound descent. Running off the end of a leaf
// pops to the deepest frame with an unvisited sibling and re-descends leftmost
// from there, which is O(1) amortised because a boundary is crossed only once
// per leaf-full of keys. This is the standard persistent/COW B+ tree cursor
// (LMDB uses the same shape).
//
// # The immutability rule
//
// Once a *bplus[V] has been stored into [Index.tree], every [leaf] and [inode]
// reachable from it is IMMUTABLE: keys, children, sets, count, height and root
// are NEVER written again, and neither are the backing arrays of those slices.
// Only [entry] contents mutate, and only under the entry's own mutex.
//
// A structural change therefore builds a NEW spine — it copies every node on
// the root-to-leaf path, leaving every off-path node shared with the previous
// snapshot — and publishes it with a single atomic store. Two consequences the
// code below relies on:
//
//   - A slice a copied node does not modify may be SHARED with the node it was
//     copied from (see cloneInsertInto, which shares an untouched keys slice).
//     Every helper that changes a slice allocates a fresh one, so sharing can
//     never be observed as a mutation.
//   - A reader that has loaded a snapshot may keep traversing it for as long
//     as it likes: nothing it can reach will ever change shape underneath it.
//
// # Key ordering and NaN
//
// Every comparison — descent, leaf search, split, bulk-pack — goes through the
// single total-order comparator [keyCompare]/[keyLess] (cmp.Compare / cmp.Less
// wrappers). The total order differs from raw < only at IEEE 754 NaN: a NaN
// key sorts before every other value (including -Inf) and all NaN bit patterns
// are one key. NaN is NEVER special-cased in the tree mechanics; the rule lives
// entirely inside the comparator, so the leading-NaN entry simply falls out as
// the leftmost key. This preserves the v1 sorted-array semantics exactly
// (package doc; task #1354).
//
// # Complexity (n = distinct keys, k = keys in a range)
//
//	Insert (existing key) O(log n)   descend + entry.set.Add, no tree change
//	Insert (new key)      O(log n)   descend + path copy + split propagation
//	Delete (set not empty)O(log n)   descend + entry.set.Remove, no tree change
//	Delete (key emptied)  O(log n)   descend + path copy - empty-node removal
//	point (Lookup/Card.)  O(log n)   descend + leaf binary search
//	Range / RangeCount    O(log n+k) lower-bound descend + cursor walk
//	RangeFirst            O(log n)   lower-bound descend
//	DistinctValues        O(1)       maintained running count
//	bulkPack (sorted in)  O(n)       bottom-up level-by-level packing
//	full in-order scan    O(n)       leftmost descent + cursor walk
//
// A structural write copies height nodes rather than shifting one slice in
// place. That is the price of a wait-free reader: the copy is O(fanout * height)
// bytes, bounded and independent of n, and it is paid ONLY when a key is created
// or destroyed. Adding or removing a node id under an existing key touches no
// tree node at all.
//
// # Delete policy
//
// Removing the last node id from a key drops the key from its leaf; when the
// copied leaf becomes ENTIRELY empty it is dropped from its copied parent, and
// the removal recurses upward through the copy path. It does NOT borrow from or
// merge partially-full siblings: for an in-memory index the textbook rebalance
// buys nothing but a large, bug-prone code surface. If churn ever degrades
// density, rebuild via [Index.BulkLoad] (the O(n) bottom-up packer). This is a
// deliberate, sanctioned simplification (graph-theory-expert, #1514).
//
// Two O(n) warts of the pre-COW deleter are gone with the leaf chain: the
// predecessor chain-walk that repaired the forward link, and the contains()
// subtree search that routed the parent removal (the source of the height-4
// corruption of #2037). Structural removal now happens along the copy path,
// which is known exactly by construction.
//
// # Concurrency
//
// A *bplus[V] is IMMUTABLE once published, so an unbounded number of readers
// may traverse one concurrently with no synchronisation whatsoever — no lock,
// no atomic, nothing but ordinary loads. The atomic load of the snapshot
// pointer in [Index] is the only synchronisation a read path performs on the
// structure.
//
// Mutation of a key's node-set is serialised by that key's own [entry.mu], so
// two writers touching different keys never interact. Structural change is
// serialised by [Index.mu], which no reader ever acquires. Lock order is always
// Index.mu → entry.mu, and no read path holds two entry locks at once, so the
// package is deadlock-free by construction.

import (
	"cmp"
	"sync"
	"unsafe"

	"github.com/FlavioCFOliveira/GoGraph/graph/index"
)

// bplusFanout is the maximum number of keys in a leaf node and the maximum
// number of children in an internal node. A node splits when it would exceed
// this. The value was chosen empirically (#1514): a 32/64/128/256 sweep with
// benchstat on a 1M-key int64 index showed 128 gives the fastest point lookup
// while keeping insert/delete of a distinct key at ~−95% versus the old
// sorted-array O(n) shift (insert ~30ms vs ~610ms for 100k distinct keys) and
// range scans unchanged. 128 keeps the tree shallow (~3 levels for 1M keys at
// the ~66% bulk-load fill) while keeping the per-split slice shift cheap. The
// residual point-lookup cost vs a flat array (+~9%, descent pointer-chase, an
// allocation-dominated absolute cost via the Lookup bitmap clone) is the
// principled, irreducible cost of a tree (rust-perf-engineer, #1514).
const bplusFanout = 128

// bplusFillNum / bplusFillDen set the bulk-load fill factor (2/3 ≈ 66%): freshly
// packed nodes are left with headroom so the first inserts after a load do not
// trigger an immediate split storm on a write-heavy index.
const (
	bplusFillNum = 2
	bplusFillDen = 3
)

// keyCompare is the total order used everywhere in the tree: cmp.Compare,
// which orders NaN before every other float and treats all NaN bit patterns
// (and ±0.0) as equal. For non-float V it is the ordinary comparison.
func keyCompare[V cmp.Ordered](a, b V) int { return cmp.Compare(a, b) }

// keyLess reports a < b under the total order (cmp.Less).
func keyLess[V cmp.Ordered](a, b V) bool { return cmp.Less(a, b) }

// entry is the MUTABLE per-key payload: the set of NodeIDs that carry the key,
// plus the lock that serialises access to it. Every leaf slot addresses an
// entry by pointer, so a copy-on-write path copy duplicates only the pointer:
// the old snapshot and the new one share ONE entry, ONE node-set and ONE lock
// for that key. That sharing is what lets a writer add a node id to an existing
// key without building a new spine at all, and what makes the detach protocol
// in [Index.Delete] work — see the crux comment there.
//
// entry is deliberately NOT generic: the key type lives in the leaf, the
// payload does not, so one non-generic type is instantiated once for every V.
//
// The zero value is a live, empty entry.
type entry struct {
	// mu guards both dead and set. It is taken by READERS as well as
	// writers, because [index.NodeSet] is not safe for concurrent use and
	// [index.NodeSet.Bitmap] can hand back the live bitmap.
	mu sync.Mutex

	// dead marks an entry that has been DETACHED from the published tree: its
	// key was removed because its set emptied. A dead entry is never reachable
	// from the current snapshot, only from an older one a reader or writer is
	// still holding. Writers must never add to a dead entry — the add would be
	// invisible to every future reader — so every insert path re-checks it
	// under mu. Once true it never becomes false again: resurrection allocates
	// a brand-new entry.
	dead bool

	// set is the node-set for the key. Read and written only under mu.
	set index.NodeSet
}

// entrySlabLen is the number of [entry] values [entryArena] carves out of ONE
// heap object.
//
// # Why four
//
// Measured on this module against one heap object per entry, 10M distinct keys
// each carrying one node (the case most adverse to a per-key payload object),
// arms interleaved within each campaign, minimum of nine forced collections per
// run, every delta taken against the baseline arm of its OWN campaign. The
// same-vs-same noise floor, measured by running the baseline arm twice inside
// the same window, was 1.6% on wall clock and 1.7% on mark CPU (task #2684,
// bench/entryheap):
//
//	slab   objects/key   GC wall   mark CPU   resident/key   worst-case pin
//	   1       1.04767    (base)     (base)         (base)          0 B/key
//	   2       0.54766   -29.85%    -39.78%         -0.000%         32 B/key
//	   4       0.29767   -32.79%    -41.73%         +0.000%         96 B/key
//	   8       0.17267   -32.90%    -42.98%         +0.000%        224 B/key
//	  16       0.11017   -30.81%    -37.26%         +0.000%        480 B/key
//
// Rows 1-8 are one campaign of six repetitions; row 16 is a second campaign of
// five, whose own baseline arm landed within 0.5% of the first one's. They are
// not one run and are not quoted as one.
//
// The GC saving PLATEAUS at four: eight is 0.11 percentage points better on
// wall clock and 1.25 on mark CPU, both inside the noise floor, while it
// doubles the retention exposure documented on [entryArena]. Two gives up about
// three points, outside the floor. Four is therefore the whole win at the
// smallest exposure that buys it, and the last column — the bytes a lone
// surviving key can pin, (entrySlabLen-1)*32 — is why the tie is broken
// downward rather than upward.
//
// # Why never more than sixteen
//
// There is a hard cliff above, and it is not a matter of taste. An object that
// CONTAINS POINTERS and is LARGER than 512 bytes carries an 8-byte malloc
// header naming its type (internal/runtime/gc.MinSizeForMallocHeader =
// PtrSize*PtrBits = 512, gc.MallocHeaderSize = 8; go1.27.1
// internal/runtime/gc/malloc.go). The header is added to the requested size
// BEFORE the size class is chosen, so it costs far more than its eight bytes: a
// 17-entry slab asks for 544, becomes 552, and lands in the 576-byte class.
// Measured on an isolated allocator probe holding the same object graph, one
// entry past the cliff inverts the result outright:
//
//	slab   bytes   resident/key   GC wall   mark CPU
//	  16     512         +0.00%   -13.21%    -15.33%
//	  17     544         +3.82%    +7.17%    +29.45%
//	  32    1024         +8.12%   +11.39%    +29.91%
//
// A 13% win becomes a 7% loss, mark CPU rises by 30%, and 3.8% of resident
// memory is spent on padding. The guard below makes that structural rather than
// hopeful.
const entrySlabLen = 4

// A slab must never cross the malloc-header cutover; see [entrySlabLen]. This
// is a constant expression, so a slab that grew past 512 bytes — because
// entrySlabLen rose, or because [entry] gained a field — fails to COMPILE here
// rather than silently costing 30% of GC mark time and 4% of resident memory.
const _ = uint(512 - entrySlabLen*unsafe.Sizeof(entry{}))

// entryArena hands out [entry] values [entrySlabLen] at a time from a shared
// heap object, instead of allocating each one on its own.
//
// # Why
//
// The per-key payload object is what makes a shared lock possible: a path copy
// duplicates only the pointer, so a snapshot and its predecessor address ONE
// entry, ONE node-set and ONE lock for the key. That is load-bearing and is not
// in question here. What it costs is one heap OBJECT per distinct key, and GC
// mark time is sensitive to object count in a way it is not sensitive to the
// bytes those objects hold: slabbing leaves the pointer count and the resident
// bytes untouched, RAISES scannable bytes by ~23% (the trailing scalar half of
// every non-final entry now falls inside the scanned range), and still cuts
// mark cost by ~15%. The saving is per-object bookkeeping, not scanning.
//
// The pointer identity every correctness argument in this file rests on is
// UNCHANGED: &slab[i] is a stable, unique address for the life of the entry,
// and the concurrency protocol never learns where it came from.
//
// # What it costs: retention
//
// A slab is reclaimed only when EVERY entry in it is unreachable, so a lone
// surviving key pins its whole slab. The worst case is entrySlabLen-1 dead
// entries retained per survivor — 96 bytes at entrySlabLen = 4 — and it is
// reached when deletion leaves exactly one survivor per slab. That is why the
// constant is small: the exposure is linear in it while the GC saving plateaus
// by four (see [entrySlabLen]).
//
// The bound is a bound, not a leak. Every live key pins at most one slab, so
// retained dead-entry bytes never exceed (entrySlabLen-1)*32 per LIVE key —
// 4x amplification of a 32-byte-per-key component, and never a function of how
// many keys the index once held beyond that. Deleting keys therefore never
// RAISES the footprint; it only stops lowering it once a slab has one survivor
// left. Measured at 2M keys pruned to every KEEP-th insertion, the pinned bytes
// per survivor land on the model exactly (task #2684):
//
//	keep    survivors   pinned/survivor   amplification   pruned heap
//	   2    1,000,000            32.00 B           2.00x       82.63 MB
//	   4      500,000            95.99 B           4.00x       74.63 MB
//	   8      250,000            96.00 B           4.00x       38.63 MB
//	  16      125,000            96.01 B           4.00x       20.64 MB
//	  64       31,250            95.86 B           4.00x        7.14 MB
//
// The overhead saturates at (entrySlabLen-1)*32 and the amplification at
// entrySlabLen, exactly as the model says; and every pruned heap is far below
// the 98.63 MB the same index occupied before any key was deleted, which is
// what "never raises the footprint" means concretely.
//
// An index whose churn has actually reached that state has the same remedy the
// delete policy already prescribes for leaf density: rebuild through
// [Index.BulkLoad], whose packer uses a private arena and therefore compacts
// the payloads as well as the leaves.
//
// Slots are NEVER reused. A detached entry can still be reachable from an older
// snapshot a reader is traversing, and from a writer that resolved it before
// the detach; handing its slot to a different key would resurrect it under that
// key. The garbage collector is the grace period, exactly as it is for the
// entry pointers themselves, and a free list would be a second, unsound
// reclamation scheme racing it.
//
// # Concurrency
//
// entryArena is NOT safe for concurrent use. The only arena reached by a live
// index is [Index.arena], and every call to alloc happens while its owner holds
// [Index.mu]; [bplus.bulkPack] uses a private arena on a tree no other
// goroutine can see yet.
type entryArena struct {
	// free is the unhanded tail of the current slab. Reslicing it forward keeps
	// the slab reachable through the arena until it is exhausted, after which
	// only the entries still referenced by a tree keep it alive.
	free []entry
}

// alloc returns a pointer to a fresh, zero-valued entry. The zero value is a
// live, empty entry (see [entry]), which is what makes handing out a slot of a
// freshly made slab equivalent to allocating one.
func (a *entryArena) alloc() *entry {
	if len(a.free) == 0 {
		a.free = make([]entry, entrySlabLen)
	}
	e := &a.free[0]
	a.free = a.free[1:]
	return e
}

// leaf is a B+ tree leaf: parallel slices of keys and pointers to their
// payloads, in ascending key order. Both slices are IMMUTABLE once the tree
// holding the leaf is published (see the immutability rule above); only the
// contents of the pointed-to entries ever change.
type leaf[V cmp.Ordered] struct {
	keys []V
	sets []*entry
}

// inode is an internal node: separator keys and child pointers. For m keys
// there are m+1 children; child[i] holds keys k with keys[i-1] <= k < keys[i]
// (with the usual sentinels). Each separator keys[i] equals the smallest key
// of the subtree rooted at child[i+1] AT THE TIME the separator was created;
// deleting that key does not rewrite the separator, which stays a valid split
// point because every key of the right subtree remains >= it.
//
// Both slices are IMMUTABLE once the tree holding the node is published.
type inode[V cmp.Ordered] struct {
	keys     []V
	children []any // each element is *leaf[V] or *inode[V]
}

// bplus is one IMMUTABLE snapshot of the B+ tree. root is *leaf[V] (possibly
// empty) or *inode[V]; height is 1 when root is a leaf. Every field is written
// exactly once, before the snapshot is published.
type bplus[V cmp.Ordered] struct {
	root   any
	count  int // number of distinct keys (DistinctValues)
	height int // 1 == root is a leaf
}

// newBplus returns an empty tree (one empty leaf root).
func newBplus[V cmp.Ordered]() *bplus[V] {
	return &bplus[V]{root: &leaf[V]{}, height: 1}
}

// leafSearch returns the index of the first key in l with key >= target under
// the total order (the lower-bound position), in [0, len(l.keys)].
func leafSearch[V cmp.Ordered](l *leaf[V], target V) int {
	lo, hi := 0, len(l.keys)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if keyLess(l.keys[mid], target) {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// inodeChild returns the index of the child to descend into for target, using
// the right-biased convention (key >= separator goes right). For separators
// s[0..m-1] and children c[0..m], it returns the smallest i in [0,m] such that
// target < s[i], or m when target >= every separator.
func inodeChild[V cmp.Ordered](n *inode[V], target V) int {
	lo, hi := 0, len(n.keys)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		// descend right of separator mid when target >= s[mid].
		if keyLess(target, n.keys[mid]) {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo
}

// findLeaf descends from the root to the leaf that would contain target. The
// tree is perfectly balanced (height levels), so the descent is exactly
// height-1 internal hops followed by the leaf — no per-level leaf/internal
// type test in the loop. The common single-leaf-root case (height 1) returns
// immediately.
func (t *bplus[V]) findLeaf(target V) *leaf[V] {
	if t.height == 1 {
		//nolint:forcetypeassert // guarded by t.height == 1 at bplus.go:394; height 1 means the root IS the leaf, and root and height are written in lockstep by th
		return t.root.(*leaf[V])
	}
	//nolint:forcetypeassert // reached only when t.height >= 2, the height==1 branch having returned at bplus.go:396; height rises past 1 only in cloneInsert's root split (bplus.go:589-595) and in bulkPack (bplus.go:855-858), both of which set root to an *inode[V] in the same statement
	n := t.root.(*inode[V])
	for h := t.height; h > 2; h-- {
		//nolint:forcetypeassert // the loop at bplus.go:400 runs while h > 2, so n is at level >= 3 and the child taken is at level >= 2, which the level invariant types *inode[V]
		n = n.children[inodeChild(n, target)].(*inode[V])
	}
	//nolint:forcetypeassert // after the loop at bplus.go:400 exits, n is at level 2 exactly, so its child is at level 1, which the level invariant types *leaf[V]
	return n.children[inodeChild(n, target)].(*leaf[V])
}

// get returns the entry stored for value under the total order, or nil when
// the key is absent in this snapshot. The entry is SHARED with every other
// snapshot that holds the same key, so the caller must take entry.mu before
// reading or writing its node-set, and must honour the dead flag before
// writing (see [entry]).
func (t *bplus[V]) get(value V) *entry {
	l := t.findLeaf(value)
	off := leafSearch(l, value)
	if off < len(l.keys) && keyCompare(l.keys[off], value) == 0 {
		return l.sets[off]
	}
	return nil
}

// maxCursorDepth is the number of internal levels a [cursor] can record. It is
// a hard bound, and it is chosen so that no tree this package can build can
// reach it:
//
//   - By insertion, height rises ONLY when the root overflows, which needs
//     bplusFanout+1 = 129 children. Each of those children was produced by a
//     split one level down, and so on, so reaching height h costs on the order
//     of 128^(h-2) structural writes over the index's whole lifetime — a
//     figure independent of how many keys are live at any instant, since
//     deletes never raise height. Depth 16 (height 17) would need ~128^15 =
//     2^105 writes; a machine doing 10^9 of them a second for a century
//     manages ~10^18.
//   - By bulk load, height h needs at least (bplusFanout*2/3)^(h-1) = 85^(h-1)
//     keys. Height 17 would need ~10^32 of them.
//
// The bound is what lets the frame stack be a plain array indexed by depth
// rather than a slice: a slice field pointing into the cursor's own array is
// self-referential, which defeats escape analysis and puts the whole cursor on
// the heap — measured at 320 B and one allocation per [Index.RangeFirst],
// against a documented allocation-free contract.
const maxCursorDepth = 16

// cursorFrame records, for one internal level of the descent, the node the
// cursor passed through and which child it took.
type cursorFrame[V cmp.Ordered] struct {
	n  *inode[V]
	ci int
}

// cursor is a forward, in-order iterator over one IMMUTABLE snapshot. It
// replaces the leaf chain the pre-COW tree maintained: the descent path it
// records is exactly what a leaf boundary needs in order to step to the next
// leaf without a sibling pointer.
//
// A cursor is a value, created on the caller's stack and never shared. It
// borrows the snapshot it was seeked into and must not outlive the caller's
// reference to it — which is automatic, since a snapshot is immutable and
// stays reachable through the cursor itself.
type cursor[V cmp.Ordered] struct {
	// stack holds one frame per INTERNAL level of the descent, indexed
	// directly by depth. It is deliberately an array and not a slice: see
	// maxCursorDepth.
	stack  [maxCursorDepth]cursorFrame[V]
	depth  int // number of frames in use == height-1
	leaf   *leaf[V]
	off    int
	height int
}

// seek positions c at the first key >= target under the total order, or leaves
// it exhausted when the snapshot holds no such key.
func (c *cursor[V]) seek(t *bplus[V], target V) {
	c.height = t.height
	c.depth = 0
	cur := t.root
	for d := 0; d <= t.height-2; d++ {
		//nolint:forcetypeassert // the loop at bplus.go:477 bounds d <= t.height-2, so cur is at level >= 2, which the level invariant types *inode[V]
		n := cur.(*inode[V])
		ci := inodeChild(n, target)
		c.stack[d] = cursorFrame[V]{n: n, ci: ci}
		c.depth = d + 1
		cur = n.children[ci]
	}
	//nolint:forcetypeassert // after the loop at bplus.go:477, d == height-1 so cur is at level 1; when height == 1 the body never runs and cur is the leaf root
	c.leaf = cur.(*leaf[V])
	c.off = leafSearch(c.leaf, target)
	c.settle()
}

// seekFirst positions c at the smallest key in the snapshot, or leaves it
// exhausted when the snapshot is empty.
func (c *cursor[V]) seekFirst(t *bplus[V]) {
	c.height = t.height
	c.depth = 0
	cur := t.root
	for d := 0; d <= t.height-2; d++ {
		//nolint:forcetypeassert // the loop at bplus.go:497 bounds d <= t.height-2, so cur is at level >= 2, which the level invariant types *inode[V]
		n := cur.(*inode[V])
		c.stack[d] = cursorFrame[V]{n: n, ci: 0}
		c.depth = d + 1
		cur = n.children[0]
	}
	//nolint:forcetypeassert // after the loop at bplus.go:497, d == height-1 so cur is at level 1; when height == 1 the body never runs and cur is the leaf root
	c.leaf = cur.(*leaf[V])
	c.off = 0
	c.settle()
}

// valid reports whether the cursor is positioned on a key.
func (c *cursor[V]) valid() bool { return c.leaf != nil }

// key returns the key the cursor is positioned on. Only valid when valid().
func (c *cursor[V]) key() V { return c.leaf.keys[c.off] }

// entry returns the payload the cursor is positioned on. Only valid when
// valid(). The caller must take entry.mu before touching its node-set.
func (c *cursor[V]) entry() *entry { return c.leaf.sets[c.off] }

// next advances the cursor to the following key in ascending order.
func (c *cursor[V]) next() {
	c.off++
	c.settle()
}

// settle steps the cursor forward across leaf boundaries until it is either
// positioned on a key or exhausted. The loop tolerates a zero-key leaf; it
// terminates because every iteration of nextLeaf either exhausts the cursor or
// moves it to a strictly later leaf, and a snapshot holds finitely many.
func (c *cursor[V]) settle() {
	for c.leaf != nil && c.off >= len(c.leaf.keys) {
		c.nextLeaf()
	}
}

// nextLeaf re-descends to the leaf that follows the current one, or exhausts
// the cursor when there is none. It pops to the deepest recorded frame that
// still has an unvisited sibling and descends leftmost from it, so the cost is
// O(height) per leaf boundary — O(1) amortised over a leaf-full of keys.
func (c *cursor[V]) nextLeaf() {
	for d := c.depth - 1; d >= 0; d-- {
		f := &c.stack[d]
		if f.ci+1 >= len(f.n.children) {
			continue
		}
		f.ci++
		cur := f.n.children[f.ci]
		c.depth = d + 1
		// The child sits at depth d+1; internal levels run to height-2.
		for nd := d + 1; nd <= c.height-2; nd++ {
			//nolint:forcetypeassert // the loop at bplus.go:550 bounds nd <= c.height-2, so cur is at level >= 2; c.height is read from the same immutable snapshot captured at bplus.go:474/494
			n := cur.(*inode[V])
			c.stack[nd] = cursorFrame[V]{n: n, ci: 0}
			c.depth = nd + 1
			cur = n.children[0]
		}
		//nolint:forcetypeassert // after the loop at bplus.go:550, nd == height-1 so cur is at level 1, which the level invariant types *leaf[V]
		c.leaf = cur.(*leaf[V])
		c.off = 0
		return
	}
	c.leaf = nil
	c.off = 0
}

// insertNode is the result of a recursive insert that split: it carries the
// separator key to promote and the new right sibling to attach in the parent.
type insertNode[V cmp.Ordered] struct {
	key   V
	right any
}

// cloneInsert returns a NEW snapshot holding every key of t plus value, mapped
// to a fresh entry, drawn from a, containing node. t is left untouched.
//
// PRECONDITION: value is absent from t. [Index.insertStructural] establishes it
// while holding Index.mu, against this very snapshot, which is also what makes
// the fresh entry safe to publish — no concurrent writer can be holding a live
// entry for the same key. Holding Index.mu is equally what makes the arena
// unshared for the duration of the call, which it must be: [entryArena] is not
// safe for concurrent use.
func (t *bplus[V]) cloneInsert(value V, node uint64, a *entryArena) *bplus[V] {
	e := a.alloc()
	e.set.Add(node)
	root, promoted := cloneInsertInto(t.root, t.height, value, e)
	height := t.height
	if promoted != nil {
		// Root split: build a new root one level up.
		root = &inode[V]{
			keys:     []V{promoted.key},
			children: []any{root, promoted.right},
		}
		height++
	}
	return &bplus[V]{root: root, count: t.count + 1, height: height}
}

// cloneInsertInto copies the root-to-leaf path for value inside the subtree
// rooted at cur (which sits at the given level, 1 == leaf), inserting the key
// into the copied leaf. It returns the replacement subtree and, when the copy
// split, the separator + right sibling to promote into the parent.
func cloneInsertInto[V cmp.Ordered](cur any, level int, value V, e *entry) (any, *insertNode[V]) {
	if level == 1 {
		//nolint:forcetypeassert // guarded by level == 1 at bplus.go:603; level starts at t.height (bplus.go:585) and drops by one per recursion (bplus.go:610), and level 1 is a *leaf[V]
		return cloneInsertLeaf(cur.(*leaf[V]), value, e)
	}
	//nolint:forcetypeassert // reached only when level >= 2, the level==1 branch having returned at bplus.go:605; level >= 2 is an *inode[V] by the level invariant
	n := cur.(*inode[V])
	ci := inodeChild(n, value)
	child, promoted := cloneInsertInto(n.children[ci], level-1, value, e)
	if promoted == nil {
		// Only the child pointer changes: SHARE the separator slice with the
		// node we copied. Both are immutable once published, and every helper
		// that alters a slice allocates a fresh one, so the sharing is invisible.
		nn := &inode[V]{keys: n.keys, children: copySlice(n.children)}
		nn.children[ci] = child
		return nn, nil
	}
	// Splice the promoted separator + right child in at position ci. Inserting
	// the right child at ci+1 leaves index ci addressing the copied child.
	nn := &inode[V]{
		keys:     copyInsertAt(n.keys, ci, promoted.key),
		children: copyInsertAt(n.children, ci+1, promoted.right),
	}
	nn.children[ci] = child
	if len(nn.children) <= bplusFanout {
		return nn, nil
	}
	return splitInode(nn)
}

// cloneInsertLeaf returns a copy of l with (value, e) spliced in at its sorted
// position, split into two fresh leaves when the copy overflows.
func cloneInsertLeaf[V cmp.Ordered](l *leaf[V], value V, e *entry) (any, *insertNode[V]) {
	off := leafSearch(l, value)
	keys := copyInsertAt(l.keys, off, value)
	sets := copyInsertAt(l.sets, off, e)
	if len(keys) <= bplusFanout {
		return &leaf[V]{keys: keys, sets: sets}, nil
	}
	// Split. The B+ rule: COPY the right half's smallest key up as the
	// separator; the key itself stays in the right leaf (all data lives in
	// leaves). Both halves get exact-capacity slices so neither retains the
	// oversized backing array of the merged copy.
	mid := len(keys) / 2
	left := &leaf[V]{keys: copySlice(keys[:mid]), sets: copySlice(sets[:mid])}
	right := &leaf[V]{keys: copySlice(keys[mid:]), sets: copySlice(sets[mid:])}
	return left, &insertNode[V]{key: right.keys[0], right: right}
}

// splitInode splits an over-full, freshly copied internal node in two. The B+
// rule: MOVE the median separator up (internal nodes hold only separators, so
// it need not stay below). n is never a published node — cloneInsertInto builds
// it — so rewriting its slices is safe.
func splitInode[V cmp.Ordered](n *inode[V]) (any, *insertNode[V]) {
	// children has len(keys)+1 entries. Pick the median key to promote.
	mid := len(n.keys) / 2
	promoteKey := n.keys[mid]
	left := &inode[V]{
		keys:     copySlice(n.keys[:mid]),
		children: copySlice(n.children[:mid+1]),
	}
	right := &inode[V]{
		keys:     copySlice(n.keys[mid+1:]),
		children: copySlice(n.children[mid+1:]),
	}
	return left, &insertNode[V]{key: promoteKey, right: right}
}

// cloneRemove returns a NEW snapshot holding every key of t except value, or t
// itself when value is absent (the snapshot is immutable, so returning it is
// safe and allocation-free). t is left untouched.
//
// A copied leaf that ends up empty is dropped from its copied parent, and the
// removal recurses upward along the copy path; a root internal node that shrinks
// to a single child is collapsed. Partially-full siblings are never merged or
// borrowed from — see the delete policy above.
func (t *bplus[V]) cloneRemove(value V) *bplus[V] {
	root, gone, removed := cloneRemoveFrom(t.root, t.height, value)
	if !removed {
		return t
	}
	if gone {
		// Every key is gone: the canonical empty tree is a lone empty leaf.
		return newBplus[V]()
	}
	height := t.height
	// Collapse every root internal node that has shrunk to one child. The
	// pre-COW deleter collapsed at most one level per delete, so a non-root
	// single-child node could surface as the root and stay; looping keeps the
	// tree as shallow as its key count allows, which never changes a query
	// result and only shortens every descent.
	for height > 1 {
		in, ok := root.(*inode[V])
		if !ok || len(in.children) != 1 {
			break
		}
		root = in.children[0]
		height--
	}
	return &bplus[V]{root: root, count: t.count - 1, height: height}
}

// cloneRemoveFrom copies the root-to-leaf path for value inside the subtree
// rooted at cur (level 1 == leaf), dropping the key from the copied leaf.
//
// removed reports whether the key was found at all; when it is false nothing
// was copied and next is cur itself. gone reports that the copied subtree holds
// no keys and must be dropped from its parent.
func cloneRemoveFrom[V cmp.Ordered](cur any, level int, value V) (next any, gone, removed bool) {
	if level == 1 {
		//nolint:forcetypeassert // guarded by level == 1 at bplus.go:711; level starts at t.height (bplus.go:679) and drops by one per recursion (bplus.go:727), and level 1 is a *leaf[V]
		l := cur.(*leaf[V])
		off := leafSearch(l, value)
		if off >= len(l.keys) || keyCompare(l.keys[off], value) != 0 {
			return cur, false, false
		}
		nl := &leaf[V]{
			keys: copyRemoveAt(l.keys, off),
			sets: copyRemoveAt(l.sets, off),
		}
		return nl, len(nl.keys) == 0, true
	}
	//nolint:forcetypeassert // reached only when level >= 2, the level==1 branch having returned at bplus.go:713; level >= 2 is an *inode[V] by the level invariant
	n := cur.(*inode[V])
	ci := inodeChild(n, value)
	child, childGone, removed := cloneRemoveFrom(n.children[ci], level-1, value)
	if !removed {
		return cur, false, false
	}
	if !childGone {
		// Only the child pointer changes: share the separators (see the
		// immutability rule).
		nn := &inode[V]{keys: n.keys, children: copySlice(n.children)}
		nn.children[ci] = child
		return nn, false, true
	}
	// Drop child ci together with the separator that bordered it: for child ci
	// that is keys[ci-1], or keys[0] for the leftmost child. Either choice
	// leaves the surviving children correctly separated, because the dropped
	// child's key band collapses into the neighbour that keeps the boundary.
	children := copyRemoveAt(n.children, ci)
	keys := n.keys
	if len(keys) > 0 {
		si := ci - 1
		if si < 0 {
			si = 0
		}
		keys = copyRemoveAt(keys, si)
	}
	if len(children) == 0 {
		return nil, true, true
	}
	return &inode[V]{keys: keys, children: children}, false, true
}

// bulkPack builds the tree bottom-up from already-sorted, deduplicated
// (key, node-set) pairs in O(n): it packs leaves to the fill factor, then packs
// each parent level from the children's first-keys until a single root remains.
// The caller guarantees keys are strictly ascending under the total order (the
// v1 contract and the Deserialize precondition) and hands off ownership of both
// slices.
func (t *bplus[V]) bulkPack(keys []V, sets []index.NodeSet) {
	t.count = len(keys)
	if len(keys) == 0 {
		t.root, t.height = &leaf[V]{}, 1
		return
	}

	leafCap := bplusFanout * bplusFillNum / bplusFillDen
	if leafCap < 1 {
		leafCap = 1
	}
	// Payloads come from ONE rolling arena rather than one block per leaf. Per
	// key would cost n allocations on the recovery path, and a block for the
	// whole load would keep the entire array alive for as long as a single key
	// of it survived; a per-LEAF block avoided both, but it sized the object at
	// leafCap entries — 85 * 32 = 2720 bytes, five times past the malloc-header
	// cutover documented on [entrySlabLen]. That block therefore asked for 2728
	// bytes and landed in the 3072-byte class, wasting 352 bytes per 85 keys,
	// and it was scanned in the expensive regime.
	//
	// Moving it to the arena's sub-cutover slabs RAISES the object count (one
	// per four keys instead of one per 85) and still measures, at 10M keys,
	// eight interleaved repetitions, noise floor 0.22% (task #2684):
	//
	//	                per-leaf block   arena slabs    delta
	//	resident/key          53.3201 B     49.1786 B   -7.77%
	//	mark CPU              181.07 ms     133.37 ms  -26.34%
	//	GC wall                20.441 ms     19.698 ms   -3.63%
	//
	// More objects, less mark time: the saving is the cutover, not the count.
	// The 4.14 bytes per key recovered is exactly the 352-byte size-class
	// remainder amortised over 85 keys. A bulk-loaded index now has the same
	// payload economics as an incrementally built one, and its retention
	// granularity improves from a leaf to a slab.
	var arena entryArena
	leaves := make([]*leaf[V], 0, (len(keys)+leafCap-1)/leafCap)
	for i := 0; i < len(keys); i += leafCap {
		end := i + leafCap
		if end > len(keys) {
			end = len(keys)
		}
		ptrs := make([]*entry, end-i)
		for j := range ptrs {
			e := arena.alloc()
			e.set = sets[i+j]
			ptrs[j] = e
		}
		// Adopt each leaf's key window directly via a three-index reslice
		// (cap == len) instead of copying it into a fresh slice. bulkPack owns
		// keys (the caller hands it off and never reads it again), and the
		// windows are disjoint, so the leaves can share the backing array. The
		// capped cap forces any later append to reallocate, so a leaf can never
		// grow into its neighbour's window.
		leaves = append(leaves, &leaf[V]{keys: keys[i:end:end], sets: ptrs})
	}

	if len(leaves) == 1 {
		t.root, t.height = leaves[0], 1
		return
	}

	// Build internal levels. Each node's children are a slice of the lower
	// level; its separators are the first-keys of children[1:].
	level := make([]any, len(leaves))
	firstKeys := make([]V, len(leaves))
	for i, l := range leaves {
		level[i] = l
		firstKeys[i] = l.keys[0]
	}
	height := 1
	childCap := bplusFanout * bplusFillNum / bplusFillDen
	if childCap < 2 {
		childCap = 2
	}
	for len(level) > 1 {
		parents := make([]any, 0, (len(level)+childCap-1)/childCap)
		parentFirst := make([]V, 0, cap(parents))
		for i := 0; i < len(level); i += childCap {
			end := i + childCap
			if end > len(level) {
				end = len(level)
			}
			in := &inode[V]{
				children: copySlice(level[i:end]),
				// Separators: first-key of each child except the first.
				keys: copySlice(firstKeys[i+1 : end]),
			}
			parents = append(parents, in)
			parentFirst = append(parentFirst, firstKeys[i])
		}
		level = parents
		firstKeys = parentFirst
		height++
	}
	t.root = level[0]
	t.height = height
}

// copySlice returns a fresh, exact-capacity copy of s. A nil or empty s yields
// a nil slice, which every consumer here treats identically to an empty one.
func copySlice[T any](s []T) []T {
	if len(s) == 0 {
		return nil
	}
	out := make([]T, len(s))
	copy(out, s)
	return out
}

// copyInsertAt returns a fresh slice holding s with v inserted at index i.
// s is never modified — the immutability rule forbids it.
func copyInsertAt[T any](s []T, i int, v T) []T {
	out := make([]T, len(s)+1)
	copy(out, s[:i])
	out[i] = v
	copy(out[i+1:], s[i:])
	return out
}

// copyRemoveAt returns a fresh slice holding s without the element at index i.
// s is never modified — the immutability rule forbids it.
func copyRemoveAt[T any](s []T, i int) []T {
	if len(s) == 1 {
		return nil
	}
	out := make([]T, len(s)-1)
	copy(out, s[:i])
	copy(out[i:], s[i+1:])
	return out
}
