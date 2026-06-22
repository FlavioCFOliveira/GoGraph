package btree

// bplus.go — the cache-friendly in-memory B+ tree that backs [Index]
// (task #1514, replacing the sorted-array v1).
//
// # Structure
//
// A classic B+ tree: all (key, [index.NodeSet]) data lives in the LEAVES;
// internal nodes hold only separator keys and child pointers. Leaves are
// SINGLY linked low→high via leaf.next, because every scan in this package
// (Range, RangeCount, Serialize) walks forward from a lower bound only. The
// tree never scans backward, so a prev pointer would be dead weight and extra
// split/unlink maintenance — if a future feature needs descending iteration,
// add the back-link deliberately rather than re-walking O(n) (graph-theory-
// expert, #1514).
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
//	Insert (new key)      O(log n)   descend + leaf insert + split propagation
//	Insert (existing key) O(log n)   descend + set.Add
//	Delete                O(log n)   descend + set.Remove (+ empty-leaf unlink)
//	point (Lookup/Card.)  O(log n)   descend + leaf binary search
//	Range / RangeCount    O(log n+k) lower-bound descend + forward leaf walk
//	RangeFirst            O(log n)   lower-bound descend
//	DistinctValues        O(1)       maintained running count
//	bulkPack (sorted in)  O(n)       bottom-up level-by-level packing
//	full in-order scan    O(n)       leftmost leaf + next chain
//
// # Delete policy
//
// Delete removes an emptied key from its leaf and, when a leaf becomes
// ENTIRELY empty, unlinks it (repairing the predecessor's next link and the
// parent separator, recursing the empty-removal upward). It does NOT borrow
// from or merge partially-full siblings: for an in-memory index the textbook
// rebalance buys nothing but a large, bug-prone code surface, while leaving an
// empty node in the scan chain or a dangling parent pointer would be a real
// defect. If churn ever degrades density, rebuild via [Index.BulkLoad] (the
// O(n) bottom-up packer). This is a deliberate, sanctioned simplification
// (graph-theory-expert, #1514).
//
// # Concurrency
//
// This type is NOT safe for concurrent use on its own; [Index] guards every
// operation with a single sync.RWMutex held for the WHOLE operation (including
// the full forward leaf walk on reads). Because the RWMutex fully excludes a
// writer's split/unlink from any in-flight reader, a reader can never observe a
// half-applied split or a dangling/unlinked leaf — the only B+-tree-specific
// isolation hazard (graph-theory-expert, #1514).

import (
	"cmp"

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

// leaf is a B+ tree leaf: parallel slices of keys and their node-sets in
// ascending key order, plus a forward link to the next leaf. The node-set
// is stored BY VALUE (see [index.NodeSet]) so a leaf of singleton-keyed
// entries carries no per-key heap object.
type leaf[V cmp.Ordered] struct {
	keys []V
	sets []index.NodeSet
	next *leaf[V]
}

// inode is an internal node: separator keys and child pointers. For m keys
// there are m+1 children; child[i] holds keys k with keys[i-1] <= k < keys[i]
// (with the usual sentinels). Each separator keys[i] equals the smallest key
// of the subtree rooted at child[i+1].
type inode[V cmp.Ordered] struct {
	keys     []V
	children []any // each element is *leaf[V] or *inode[V]
}

// bplus is the B+ tree itself. root is *leaf[V] (possibly empty) or *inode[V].
// first always points at the leftmost leaf so an in-order scan starts in O(1)
// after the O(log n) initial descent is amortised by the cached pointer.
type bplus[V cmp.Ordered] struct {
	root   any
	first  *leaf[V]
	count  int // number of distinct keys (DistinctValues)
	height int // 1 == root is a leaf
}

// newBplus returns an empty tree (one empty leaf root).
func newBplus[V cmp.Ordered]() *bplus[V] {
	l := &leaf[V]{}
	return &bplus[V]{root: l, first: l, height: 1}
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
		return t.root.(*leaf[V])
	}
	n := t.root.(*inode[V])
	for h := t.height; h > 2; h-- {
		n = n.children[inodeChild(n, target)].(*inode[V])
	}
	return n.children[inodeChild(n, target)].(*leaf[V])
}

// lowerBound returns the (leaf, offset) of the first key >= target, or
// (nil, 0) when no such key exists. It crosses the leaf link once when target
// is greater than every key in the landed leaf (the separator routed us to a
// leaf whose keys are all < target).
func (t *bplus[V]) lowerBound(target V) (*leaf[V], int) {
	l := t.findLeaf(target)
	off := leafSearch(l, target)
	if off < len(l.keys) {
		return l, off
	}
	if l.next != nil {
		return l.next, 0
	}
	return nil, 0
}

// get returns a pointer to the node-set stored for value under the total
// order, or nil when the key is absent. The pointer aliases the leaf's
// by-value set, so the caller must hold the index lock for the duration
// of any read or mutation through it.
func (t *bplus[V]) get(value V) *index.NodeSet {
	l := t.findLeaf(value)
	off := leafSearch(l, value)
	if off < len(l.keys) && keyCompare(l.keys[off], value) == 0 {
		return &l.sets[off]
	}
	return nil
}

// insertNode is the result of a recursive insert that split: it carries the
// separator key to promote and the new right sibling to attach in the parent.
type insertNode[V cmp.Ordered] struct {
	key   V
	right any
}

// insert adds node to value's bitmap, creating the key when absent. It returns
// true when a NEW distinct key was created (so the caller can bump the count).
func (t *bplus[V]) insert(value V, node uint64) bool {
	created, promoted := t.insertInto(t.root, value, node)
	if promoted != nil {
		// Root split: build a new root one level up.
		nr := &inode[V]{
			keys:     []V{promoted.key},
			children: []any{t.root, promoted.right},
		}
		t.root = nr
		t.height++
	}
	if created {
		t.count++
	}
	return created
}

// insertInto inserts (value,node) into the subtree rooted at cur. It returns
// whether a new key was created and, when the node split, the separator+right
// sibling to promote into the parent.
func (t *bplus[V]) insertInto(cur any, value V, node uint64) (created bool, promoted *insertNode[V]) {
	switch n := cur.(type) {
	case *leaf[V]:
		return t.insertLeaf(n, value, node)
	case *inode[V]:
		ci := inodeChild(n, value)
		created, childPromoted := t.insertInto(n.children[ci], value, node)
		if childPromoted == nil {
			return created, nil
		}
		// Insert the promoted separator + right child at position ci.
		n.keys = insertAt(n.keys, ci, childPromoted.key)
		n.children = insertAt(n.children, ci+1, childPromoted.right)
		if len(n.children) <= bplusFanout {
			return created, nil
		}
		return created, splitInode(n)
	}
	return false, nil
}

// insertLeaf inserts (value,node) into leaf l.
func (t *bplus[V]) insertLeaf(l *leaf[V], value V, node uint64) (created bool, promoted *insertNode[V]) {
	off := leafSearch(l, value)
	if off < len(l.keys) && keyCompare(l.keys[off], value) == 0 {
		l.sets[off].Add(node)
		return false, nil
	}
	var set index.NodeSet
	set.Add(node)
	l.keys = insertAt(l.keys, off, value)
	l.sets = insertAt(l.sets, off, set)
	if len(l.keys) <= bplusFanout {
		return true, nil
	}
	return true, splitLeaf(l)
}

// splitLeaf splits an over-full leaf into l (left half) and a new right leaf,
// fixing the forward link. The B+ rule: COPY the right half's smallest key up
// as the separator; the key itself stays in the right leaf (all data lives in
// leaves).
func splitLeaf[V cmp.Ordered](l *leaf[V]) *insertNode[V] {
	mid := len(l.keys) / 2
	right := &leaf[V]{
		keys: append([]V(nil), l.keys[mid:]...),
		sets: append([]index.NodeSet(nil), l.sets[mid:]...),
		next: l.next,
	}
	l.keys = l.keys[:mid:mid]
	l.sets = l.sets[:mid:mid]
	l.next = right
	return &insertNode[V]{key: right.keys[0], right: right}
}

// splitInode splits an over-full internal node. The B+ rule: MOVE the median
// separator up (internal nodes hold only separators, so it need not stay
// below).
func splitInode[V cmp.Ordered](n *inode[V]) *insertNode[V] {
	// children has len(keys)+1 entries. Pick the median key to promote.
	mid := len(n.keys) / 2
	promoteKey := n.keys[mid]
	right := &inode[V]{
		keys:     append([]V(nil), n.keys[mid+1:]...),
		children: append([]any(nil), n.children[mid+1:]...),
	}
	n.keys = n.keys[:mid:mid]
	n.children = n.children[: mid+1 : mid+1]
	return &insertNode[V]{key: promoteKey, right: right}
}

// remove deletes node from value's bitmap; when the bitmap empties, the key is
// removed from its leaf and a fully-empty leaf is unlinked. It returns true
// when a distinct key was removed (so the caller can decrement the count).
func (t *bplus[V]) remove(value V, node uint64) bool {
	l := t.findLeaf(value)
	off := leafSearch(l, value)
	if off >= len(l.keys) || keyCompare(l.keys[off], value) != 0 {
		return false
	}
	if !l.sets[off].Remove(node) {
		return false
	}
	// Drop the emptied key from the leaf.
	l.keys = removeAt(l.keys, off)
	l.sets = removeAt(l.sets, off)
	t.count--
	if len(l.keys) == 0 {
		t.unlinkEmptyLeaf(l)
	}
	return true
}

// unlinkEmptyLeaf removes a now-empty leaf from the tree: it repairs the
// predecessor's forward link (and the first pointer) and removes the leaf's
// child pointer + separator from its parent, recursing the empty-node removal
// upward. A leaf is only unlinked once it holds zero keys (delete policy).
func (t *bplus[V]) unlinkEmptyLeaf(l *leaf[V]) {
	// Repair the forward link chain. The leftmost leaf has no predecessor.
	if t.first == l {
		if l.next != nil {
			t.first = l.next
		}
		// else: the tree is now empty; keep l as the lone (empty) root below.
	} else {
		// Find the predecessor leaf via the chain (cheap: leaves are few
		// relative to keys, and this only runs on full-leaf-emptying deletes).
		for p := t.first; p != nil; p = p.next {
			if p.next == l {
				p.next = l.next
				break
			}
		}
	}
	// Remove l from the tree structure (parent separator + child pointer),
	// recursing upward if a parent becomes empty.
	if _, ok := t.root.(*leaf[V]); ok {
		// Root is the only leaf; an empty root leaf is the canonical empty
		// tree — leave it in place and reset first to it.
		t.first = l
		return
	}
	t.removeChild(t.root, nil, l)
	// Collapse a root inode that has shrunk to a single child.
	if in, ok := t.root.(*inode[V]); ok && len(in.children) == 1 {
		t.root = in.children[0]
		t.height--
	}
}

// removeChild descends to the parent of target and removes target's child
// pointer + the matching separator. It recurses, removing an internal node
// that becomes child-less from its own parent. parent is cur's parent
// (nil for the root), used only to escalate the removal when cur itself
// becomes child-less.
func (t *bplus[V]) removeChild(cur any, parent *inode[V], target any) {
	n, ok := cur.(*inode[V])
	if !ok {
		return
	}
	for ci := range n.children {
		if n.children[ci] == target {
			n.children = removeAt(n.children, ci)
			// Drop the separator that bordered the removed child. For child ci
			// the bordering separator is keys[ci-1] when ci>0 else keys[0].
			si := ci - 1
			if si < 0 {
				si = 0
			}
			if len(n.keys) > 0 {
				n.keys = removeAt(n.keys, si)
			}
			if len(n.children) == 0 && parent != nil {
				t.removeChild(parent, nil, n)
			}
			return
		}
		if t.contains(n.children[ci], target) {
			t.removeChild(n.children[ci], n, target)
			return
		}
	}
}

// contains reports whether the subtree rooted at cur holds the exact node
// pointer target (used to route removeChild without re-deriving keys).
func (t *bplus[V]) contains(cur, target any) bool {
	switch n := cur.(type) {
	case *leaf[V]:
		return any(n) == target
	case *inode[V]:
		if any(n) == target {
			return true
		}
		for _, c := range n.children {
			if c == target {
				return true
			}
		}
		return false
	}
	return false
}

// bulkPack builds the tree bottom-up from already-sorted, deduplicated
// (key, node-set) pairs in O(n): it packs leaves to the fill factor, links
// them, then packs each parent level from the children's first-keys until a
// single root remains. The caller guarantees keys are strictly ascending under
// the total order (the v1 contract and the Deserialize precondition).
func (t *bplus[V]) bulkPack(keys []V, sets []index.NodeSet) {
	t.count = len(keys)
	if len(keys) == 0 {
		l := &leaf[V]{}
		t.root, t.first, t.height = l, l, 1
		return
	}

	leafCap := bplusFanout * bplusFillNum / bplusFillDen
	if leafCap < 1 {
		leafCap = 1
	}
	var leaves []*leaf[V]
	for i := 0; i < len(keys); i += leafCap {
		end := i + leafCap
		if end > len(keys) {
			end = len(keys)
		}
		// Adopt each leaf's slice window directly via a three-index reslice
		// (cap == len) instead of copying it into a fresh slice. bulkPack owns
		// keys/sets (the caller hands them off and never reads them again), and
		// the windows are disjoint, so the leaves can share the backing arrays.
		// The capped cap forces any later in-leaf append to reallocate, so a
		// leaf can never grow into its neighbour's window.
		l := &leaf[V]{
			keys: keys[i:end:end],
			sets: sets[i:end:end],
		}
		if len(leaves) > 0 {
			leaves[len(leaves)-1].next = l
		}
		leaves = append(leaves, l)
	}
	t.first = leaves[0]

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
		var parents []any
		var parentFirst []V
		for i := 0; i < len(level); i += childCap {
			end := i + childCap
			if end > len(level) {
				end = len(level)
			}
			in := &inode[V]{
				children: append([]any(nil), level[i:end]...),
			}
			// Separators: first-key of each child except the first.
			for j := i + 1; j < end; j++ {
				in.keys = append(in.keys, firstKeys[j])
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

// insertAt inserts v at index i in s, returning the grown slice.
func insertAt[T any](s []T, i int, v T) []T {
	var zero T
	s = append(s, zero)
	copy(s[i+1:], s[i:])
	s[i] = v
	return s
}

// removeAt removes the element at index i from s, returning the shrunk slice.
func removeAt[T any](s []T, i int) []T {
	return append(s[:i], s[i+1:]...)
}
