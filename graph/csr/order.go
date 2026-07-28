package csr

import "github.com/FlavioCFOliveira/GoGraph/graph"

// order.go — within-source neighbour ordering (rmp #2141).
//
// A CSR's edges array is grouped by source, but within a source the order was
// historically the insertion order of the underlying adjacency. Ordering each
// source's run by destination turns the executor's membership probes
// ([cypher/exec.Expand]'s forward-position lookups) from an O(d) scan into an
// O(log d) binary search, which is what unlocks Expand(Into) for a bound
// destination, the symmetric anchor swap, and the sorted-merge intersection
// primitive.
//
// # Why the ordering is UNCONDITIONAL rather than degree-adaptive
//
// The calibration in docs/design-degree-adaptive-adjacency.md measured the
// linear/binary crossover at degree ≈16 — not the degree ≈64 the motivating
// audit reported, whose linear-scan column was ~6.5x too fast and in fact faster
// than a Go scan can physically run. A per-source "is ordered" bit would buy a
// branch at every probe site, a correctness obligation that every site consult
// it, and a promotion rule that is incompatible with recovery determinism
// (store/snapshot.ApplyCSRToGraph replays in bulk csr.bin order, a different
// degree trajectory from the original interleaved writes, so a history-dependent
// representation lets a snapshot drift). Ordering every run removes all three:
// "runs are ordered" is a flat invariant of every CSR this package builds.
//
// # Why the key is (destination, handle) and why the sort must be STABLE
//
// Ordering by destination alone leaves parallel edges — several slots in one
// source's run sharing a destination — mutually unordered, and their ORDINAL
// within the run is load-bearing: cypher/api.go's buildEdgeTypeFilter resolves a
// parallel edge's relationship TYPE from it (it counts occurrences in run order
// and calls EdgeLabelsAt with that ordinal) on the handle-less path, and
// cypher/exec/revtofwd.go's buildRevToFwd has the same positional-ordinal
// fallback. An UNSTABLE reorder therefore silently mis-types parallel edges —
// and slices.Sort/sort.Slice are pdqsort, which is unstable, so reaching for
// either is the natural and silent mistake.
//
// Two defences are applied together rather than one:
//
//   - the handle is a TIEBREAKER in the key, so slots that carry distinct
//     handles are TOTALLY ordered and need no stability property at all. This
//     is the technique PostgreSQL's nbtree adopted by making the heap TID a
//     tiebreaker index column, and that RocksDB/LevelDB use by suffixing the
//     user key with a sequence number;
//   - the algorithms below are stable, which is what orders the residual the
//     key cannot separate: slots carrying handle 0 (the "no handle" sentinel,
//     which is what a MERGE-created slot carries) have no tiebreaker available,
//     so they keep their relative input order.
//
// The second point is an INVARIANT, not an implementation accident: the handle-0
// residual of a destination run must stay in its original relative order. Both
// [insertionSortRun] and [mergeSortRun] guarantee it, and it is asserted by the
// ordering tests.
//
// # The reverse CSR is not ordered here, deliberately
//
// [CSR.BuildReverse] scatters while walking sources ascending, so each reverse
// bucket is already ascending in the source at zero cost. It must NOT be
// re-ordered separately: its slot order is derived from the forward CSR's, and
// buildRevToFwd's ordinal fallback pairs the k-th reverse slot with the k-th
// forward slot. Because the transpose replays whatever forward order it is
// given, that correspondence survives this change automatically — but only so
// long as the reverse side is left alone.

// runOrderInsertionCutoff is the run length at or below which ordering uses an
// in-place insertion sort. Below the cutoff insertion sort is faster than a
// merge and, crucially, needs NO scratch buffer, so a graph whose every source
// is short orders with zero additional allocation. Property-graph average
// out-degree is 4-16, so the large majority of runs take this path.
const runOrderInsertionCutoff = 32

// OrderRuns stably orders every source's neighbour run of a CSR in place, by the
// total key (destination, handle), permuting the parallel weights and handles
// columns under the SAME permutation. vertices is the length V+1 offsets array;
// edges is the flat neighbour array; weights and handles are the parallel
// columns and may each be nil.
//
// It is exported because a caller that assembles CSR arrays itself and hands
// them to [FromArrays] must apply the same ordering to obtain a snapshot that
// carries this package's ordering invariant — store/bulk's counting-sort build
// does exactly that, and its byte-identity contract with [BuildFromAdjList]
// depends on using this same function.
//
// Complexity is O(V + Σ d log d), which for a property graph's degree
// distribution is dominated by the O(V + E) copy that produced the arrays.
// Allocation is at most one scratch buffer per column, sized to the LONGEST run
// and reused across every source; a CSR whose longest run is within
// [runOrderInsertionCutoff] allocates nothing at all.
func OrderRuns[W any](vertices []uint64, edges []graph.NodeID, weights []W, handles []uint64) {
	if len(vertices) < 2 {
		return
	}

	// Size the scratch from the longest run, once, so no per-source allocation
	// occurs. Runs at or below the cutoff never touch it.
	var longest int
	for i := 0; i+1 < len(vertices); i++ {
		if n := int(vertices[i+1] - vertices[i]); n > longest {
			longest = n
		}
	}
	if longest < 2 {
		return // every source has at most one neighbour: nothing to order
	}
	var scratchD []graph.NodeID
	var scratchW []W
	var scratchH []uint64
	if longest > runOrderInsertionCutoff {
		scratchD = make([]graph.NodeID, longest)
		if weights != nil {
			scratchW = make([]W, longest)
		}
		if handles != nil {
			scratchH = make([]uint64, longest)
		}
	}

	for i := 0; i+1 < len(vertices); i++ {
		lo, hi := vertices[i], vertices[i+1]
		if hi-lo < 2 {
			continue
		}
		e := edges[lo:hi]
		w := subSlice(weights, lo, hi)
		h := subSliceU64(handles, lo, hi)
		if runOrdered(e, h) {
			continue // already ordered: the common case for a bulk-loaded run
		}
		if len(e) <= runOrderInsertionCutoff {
			insertionSortRun(e, w, h)
			continue
		}
		mergeSortRun(e, w, h, scratchD, scratchW, scratchH)
	}
}

// RunsOrdered reports whether every source's neighbour run is ordered by the
// total key (destination, handle). It is a VERIFICATION helper for tests and
// diagnostics, not a hot-path predicate: probes rely on the invariant rather
// than re-checking it.
//
// Complexity is O(V + E). It does not allocate and never panics; a malformed
// offsets array yields false rather than an index panic.
func (c *CSR[W]) RunsOrdered() bool {
	verts := c.vertices
	for i := 0; i+1 < len(verts); i++ {
		lo, hi := verts[i], verts[i+1]
		if hi < lo || hi > uint64(len(c.edges)) {
			return false
		}
		if !runOrdered(c.edges[lo:hi], subSliceU64(c.handles, lo, hi)) {
			return false
		}
	}
	return true
}

// runOrdered reports whether one run is already in non-decreasing total-key
// order. Checking costs one linear pass and skips the sort entirely for a run
// that arrived ordered, which is common for bulk loads that stream edges in
// ascending destination order.
func runOrdered(edges []graph.NodeID, handles []uint64) bool {
	for i := 1; i < len(edges); i++ {
		if lessKeyAt(edges, handles, i, i-1) {
			return false
		}
	}
	return true
}

// lessKeyAt reports whether slot i sorts strictly before slot j under the total
// key (destination, handle). With no handle column the key degenerates to the
// destination, so equal destinations compare equal and the callers' stability
// preserves their relative order.
func lessKeyAt(edges []graph.NodeID, handles []uint64, i, j int) bool {
	di, dj := edges[i], edges[j]
	if di != dj {
		return di < dj
	}
	if handles == nil {
		return false
	}
	return handles[i] < handles[j]
}

// lessKey is [lessKeyAt] against a key held in registers, used by the insertion
// sort while the element being placed is out of the array.
func lessKey(d1 graph.NodeID, h1 uint64, d2 graph.NodeID, h2 uint64) bool {
	if d1 != d2 {
		return d1 < d2
	}
	return h1 < h2
}

// insertionSortRun orders one run in place, moving the parallel columns in
// lockstep. It is STABLE: the inner loop shifts only while the element being
// placed is STRICTLY less than its predecessor, so equal-key slots keep their
// relative order — which is what orders the handle-0 residual.
//
// It allocates nothing.
func insertionSortRun[W any](edges []graph.NodeID, weights []W, handles []uint64) {
	for i := 1; i < len(edges); i++ {
		d := edges[i]
		var w W
		if weights != nil {
			w = weights[i]
		}
		var h uint64
		if handles != nil {
			h = handles[i]
		}
		j := i - 1
		for j >= 0 && lessKey(d, h, edges[j], handleAt(handles, j)) {
			edges[j+1] = edges[j]
			if weights != nil {
				weights[j+1] = weights[j]
			}
			if handles != nil {
				handles[j+1] = handles[j]
			}
			j--
		}
		edges[j+1] = d
		if weights != nil {
			weights[j+1] = w
		}
		if handles != nil {
			handles[j+1] = h
		}
	}
}

// mergeSortRun orders one run in place with a top-down stable merge sort,
// falling back to [insertionSortRun] below the cutoff. The scratch buffers must
// each be at least len(edges) long (nil for a column that is absent).
//
// Passing the same scratch prefix down every recursion level is safe because
// each merge COPIES its subrange into the scratch before merging out of it, and
// the recursion is depth-first and sequential: a sibling's use of the scratch
// has completed before the parent's merge begins.
func mergeSortRun[W any](
	edges []graph.NodeID, weights []W, handles []uint64,
	scratchD []graph.NodeID, scratchW []W, scratchH []uint64,
) {
	n := len(edges)
	if n <= runOrderInsertionCutoff {
		insertionSortRun(edges, weights, handles)
		return
	}
	mid := n / 2
	mergeSortRun(edges[:mid], subSlice(weights, 0, uint64(mid)), subSliceU64(handles, 0, uint64(mid)),
		scratchD, scratchW, scratchH)
	mergeSortRun(edges[mid:], subSlice(weights, uint64(mid), uint64(n)), subSliceU64(handles, uint64(mid), uint64(n)),
		scratchD, scratchW, scratchH)
	if !lessKeyAt(edges, handles, mid, mid-1) {
		return // halves already in order end-to-end: the merge would be a copy
	}
	mergeRun(edges, weights, handles, mid, scratchD, scratchW, scratchH)
}

// mergeRun merges the two ordered halves edges[:mid] and edges[mid:] back into
// edges, in lockstep across the parallel columns. It is STABLE: on an equal key
// the LEFT half is taken first.
func mergeRun[W any](
	edges []graph.NodeID, weights []W, handles []uint64, mid int,
	scratchD []graph.NodeID, scratchW []W, scratchH []uint64,
) {
	n := len(edges)
	copy(scratchD[:n], edges)
	if weights != nil {
		copy(scratchW[:n], weights)
	}
	if handles != nil {
		copy(scratchH[:n], handles)
	}
	sh := subSliceU64(scratchH, 0, uint64(n))

	i, j, k := 0, mid, 0
	for i < mid && j < n {
		// Take the right element only when it is STRICTLY less than the left,
		// so equal keys resolve to the left half and the sort stays stable.
		if lessKeyAt(scratchD, sh, j, i) {
			edges[k] = scratchD[j]
			if weights != nil {
				weights[k] = scratchW[j]
			}
			if handles != nil {
				handles[k] = scratchH[j]
			}
			j++
		} else {
			edges[k] = scratchD[i]
			if weights != nil {
				weights[k] = scratchW[i]
			}
			if handles != nil {
				handles[k] = scratchH[i]
			}
			i++
		}
		k++
	}
	for ; i < mid; i, k = i+1, k+1 {
		edges[k] = scratchD[i]
		if weights != nil {
			weights[k] = scratchW[i]
		}
		if handles != nil {
			handles[k] = scratchH[i]
		}
	}
	for ; j < n; j, k = j+1, k+1 {
		edges[k] = scratchD[j]
		if weights != nil {
			weights[k] = scratchW[j]
		}
		if handles != nil {
			handles[k] = scratchH[j]
		}
	}
}

// handleAt returns the handle at slot i, or 0 when the column is absent.
func handleAt(handles []uint64, i int) uint64 {
	if handles == nil {
		return 0
	}
	return handles[i]
}

// subSlice slices a possibly-absent parallel column, preserving nil so an
// absent column stays absent instead of panicking on a non-empty range.
func subSlice[W any](s []W, lo, hi uint64) []W {
	if s == nil {
		return nil
	}
	return s[lo:hi]
}

// subSliceU64 is [subSlice] for the handle column.
func subSliceU64(s []uint64, lo, hi uint64) []uint64 {
	if s == nil {
		return nil
	}
	return s[lo:hi]
}
