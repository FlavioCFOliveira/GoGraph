package exec

// shortest_path_bidir.go — bidirectional BFS for the single-path
// shortestPath() operator (rmp #2220).
//
// # Why
//
// shortest_path.go's own algorithm note set the exit condition for this change:
// "Forward-only BFS is kept until a measured benchmark shows the doubling pays
// off in this codebase." The round-3 head-to-head produced that benchmark —
// shortestPath bounded at 6 hops cost 23.797 ms against Neo4j's 1.089 ms and
// Memgraph's 395 µs, the worst non-triangle ratio in the whole matrix, on a
// graph of average out-degree 10 — and nobody connected the two.
//
// The note is right that the complexity CLASS is unchanged and wrong that this
// makes the technique unimportant. Forward-only BFS explores Θ(b^d);
// bidirectional explores Θ(b^⌈d/2⌉). At b = 10 and d = 6 that is 10^6 against
// 2 × 10^3. Halving the exponent is where the three orders of magnitude live.
//
// # What this is NOT
//
// It is not [search.BiBFS], which already exists and is already tested. That
// runs over an immutable [csr.CSR]; this operator works over a CSR snapshot but
// must additionally honour the relationship-type filter, the direction mode,
// hop bounds, and relationship-uniqueness, and must emit the flat alternating
// hop encoding with handle-disambiguated forward positions. The TECHNIQUE
// transfers; the function does not.
//
// It is also deliberately NOT applied to allShortestPaths: reconstructing the
// multi-predecessor DAG across a meeting point is materially harder, and that
// operator must stay level-synchronous.
//
// # The two searches, and why the backward one is the exact dual
//
// The forward search expands in the query's own direction. The backward search
// starts at dst and walks AGAINST it, so for each CSR it uses the opposite one:
//
//	path edge x→y traversed FORWARD  (allowed unless dir == DirIn)
//	  forward search:  scan fwd CSR of x, find y
//	  backward search: scan REV CSR of y, find x   → hop{reversed: false}
//
//	path edge x→y traversed REVERSE  (allowed unless dir == DirOut)
//	  forward search:  scan rev CSR of x, find y
//	  backward search: scan FWD CSR of y, find x   → hop{reversed: true}
//
// So a backward predecessor entry resolves to a hop with the SAME forward
// position rule as a forward one and the OPPOSITE reversed flag — see
// [ShortestPath.resolveBackwardHop], which is [ShortestPath.resolveFwdPos]
// with that one boolean inverted. Getting this wrong produces a path whose hops
// hydrate to the wrong relationship instances rather than an obviously broken
// answer, which is why the differential test compares the full emitted list and
// not merely the path LENGTH.
//
// # Termination, and why it is exact
//
// Expand whole levels, always the smaller frontier. After each level, best is
// the least distF(u) + distB(u) over nodes seen by both. Stop once
// best ≤ levelF + levelB.
//
// That condition is exact, not a heuristic. Take any src→dst path P of length
// L ≤ levelF + levelB. If L ≤ levelF the forward search has already reached dst
// and best ≤ L. Otherwise let w be P's vertex at position levelF: distF(w) ≤
// levelF so the forward search knows it, and w's distance to dst along P is
// L − levelF ≤ levelB so the backward search knows it too — hence best ≤
// distF(w) + distB(w) ≤ L. Either way best ≤ L, so no path shorter than best
// remains undiscovered.
//
// # Relationship-uniqueness at the join
//
// A joined walk of length best cannot repeat a vertex: a repeat would contain a
// cycle, and excising it would give a strictly shorter src→dst walk,
// contradicting best being minimal. No repeated vertex means no repeated edge,
// so the openCypher relationship-uniqueness requirement holds by construction.
//
// The code does not rely on that argument alone. Every candidate meeting point
// is checked for a repeated relationship handle, and a candidate that fails is
// skipped in favour of the next one at the same distance. If every candidate
// failed — which the argument says cannot happen — the operator falls back to
// the forward-only search rather than returning a wrong path or a spurious
// no-path. The fallback makes the worst case slow, never incorrect.

import (
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// biMeet records one node reached by both searches, with the distance of the
// joined path through it.
type biMeet struct {
	node uint64
	dist int
}

// canBidirectional reports whether the two-sided search is usable for this
// operator's configuration. Whichever direction mode is in force, exactly one of
// the two searches walks against the query direction and therefore reads the
// reverse adjacency, so a usable reverse CSR is the precondition.
//
// # Why the shape check, and why it is a gate rather than an assertion
//
// Before #2220 a DirOut operator never read the reverse CSR, so a caller was
// free to hand it a placeholder. Several in-tree tests did exactly that —
// buildCSR(n, nil), an empty reverse adjacency — and enabling the two-sided
// search over one of those would report "no path" for a pair that is plainly
// connected. That is the worst possible failure: silent and wrong.
//
// A genuine reverse CSR has one entry per forward entry and one vertex slot per
// forward vertex slot. Checking that is O(1) and turns the placeholder case into
// a fall back to the forward-only walk, which is always correct. It is a
// necessary condition, not a proof of well-formedness — a reverse CSR with the
// right counts but wrong contents would still be wrong, but that would already
// break every DirIn and DirBoth traversal today, so it is part of the caller
// contract rather than something this search newly depends on.
//
// # Every direction, and why the two halves need no direction-specific code
//
// #2220 admitted DirOut alone; rmp #2236 widened the gate to DirIn and DirBoth
// as well. No search code changed to do it, because [ShortestPath.biExpand] was
// always written from the duality above rather than from the DirOut case: it
// scans forward-traversed path edges unless dir is DirIn and reverse-traversed
// ones unless dir is DirOut, and each search picks its CSR by the `forward` flag.
// For DirIn that means the forward search reads the reverse CSR from src and the
// backward search reads the forward CSR from dst — the exact dual — and for
// DirBoth both searches read both. Hop resolution is likewise keyed off the
// recorded entry's own CSR (spPredEntry.fwd), not off op.dir.
//
// What blocked the widening was a correctness question, not the algorithm.
// #2220's differential suite, once given a type filter, showed the FORWARD-ONLY
// reference returning a path whose hops use an excluded edge — both algorithms
// agreeing on a path the filter should reject, which implicated the shared
// reverse-slot type check rather than the new code. It was a defect in that
// shared check: the resolution signalled failure by returning its own input, so a
// slot whose mapping legitimately equalled its reverse position skipped the
// filter entirely. Fixed first and separately (see
// [ShortestPath.resolveFwdPosKnown] and
// docs/benchmarks/shortest-path-type-filter-2026-07-28.md), which is what made
// this widening safe to build on.
//
// # A typed search, and why the gate is exactness rather than cost
//
// A relationship-type filter is keyed by FORWARD edge position, so the half that
// scans reverse slots must decide admission for a reverse slot. #2220 measured two
// ways and rejected both: the prebuilt revToFwd table, which cost more than the
// search saved (277.7 ms → 351.1 ms end to end at N=20000, a 26% regression,
// while the search itself got 17.9× faster), and resolving each scanned slot
// against the forward CSR, which left an unresolvable case that had to be answered
// permissively.
//
// Only ONE of those two objections survived measurement. The table was not
// expensive — it was being rebuilt once per outer ROW (see
// [ShortestPath.revPrepared]); built once it costs under a millisecond at
// E=200k. The permissiveness was the real obstacle, and it is not a cost problem
// at all: an admission answer that may be wrong cannot be traded against speed.
//
// So the gate is exactness. [revTypeAdmitSet] builds a filter keyed by REVERSE
// position directly, by replaying the counting-sort transpose that produced the
// reverse CSR — a pairing that validates every slot or declines outright, with no
// unresolvable case to answer permissively — and marks the admitted slots in a
// bitset. The per-slot test is one word read and shift. op.revAdmit != nil is
// precisely "that structure was built and validated", so it is the gate for a
// typed search.
//
// # Why the shape check is still needed
//
// [revTypeAdmitSet] validates the reverse CSR, but only for a TYPED search. An
// untyped one never builds it, so the O(1) shape check below remains the guard
// that turns a placeholder reverse CSR into a fallback rather than a silent
// "no path".
func (op *ShortestPath) canBidirectional() bool {
	if op.revVerts == nil ||
		len(op.revVerts) != len(op.fwdVerts) ||
		len(op.revEdges) != len(op.fwdEdges) {
		return false
	}
	// A type filter is admitted only with the exact reverse-position bitset. The
	// revToFwd table would also answer, but it answers permissively where a
	// mapping is unknown, and the two searches scan different slots — so the
	// two-sided half could admit an edge the forward-only walk never reaches and
	// return a path the filter excludes. That is the #2220 symptom; do not
	// reintroduce it for a fallback that is no faster.
	return op.edgeType == "" || op.revAdmit != nil
}

// biBFSShortestPath finds a shortest src→dst path with two-sided BFS. It
// returns (path, true, nil) on success, (Null, false, nil) when no path
// satisfies the hop bounds, and delegates to the forward-only search when the
// configuration or an exhausted uniqueness check makes the two-sided answer
// unavailable.
//
// src != dst is a precondition: the equal case is a cycle search and is handled
// by the caller.
func (op *ShortestPath) biBFSShortestPath(src, dst uint64) (expr.Value, bool, error) {
	distF := map[uint64]int{src: 0}
	distB := map[uint64]int{dst: 0}
	predF := map[uint64]spPredEntry{src: {parent: src}}
	predB := map[uint64]spPredEntry{dst: {parent: dst}}

	frontierF := []uint64{src}
	frontierB := []uint64{dst}
	levelF, levelB := 0, 0

	best := -1
	var meets []biMeet

	for len(frontierF) > 0 && len(frontierB) > 0 {
		if err := op.ctx.Err(); err != nil {
			return nil, false, err
		}
		// Exactness condition, proved in the file comment.
		if best >= 0 && best <= levelF+levelB {
			break
		}
		// One more level costs one more hop on whichever side expands; when even
		// that cannot fit under the bound there is nothing left to find.
		if op.maxHops != shortestNoMaxHops && levelF+levelB+1 > op.maxHops {
			break
		}

		// Always expand the smaller frontier: that is what turns Θ(b^d) into
		// Θ(b^⌈d/2⌉) on an irregular graph, where one side may branch far faster
		// than the other.
		var discovered []uint64
		if len(frontierF) <= len(frontierB) {
			discovered = op.biExpand(frontierF, predF, distF, levelF+1, true)
			frontierF = discovered
			levelF++
		} else {
			discovered = op.biExpand(frontierB, predB, distB, levelB+1, false)
			frontierB = discovered
			levelB++
		}

		// A meeting can only involve a node discovered in this level.
		for _, u := range discovered {
			df, okF := distF[u]
			db, okB := distB[u]
			if !okF || !okB {
				continue
			}
			d := df + db
			if best < 0 || d < best {
				best, meets = d, meets[:0]
			}
			if d == best {
				meets = append(meets, biMeet{node: u, dist: d})
			}
		}
	}

	if best < 0 {
		return expr.Null, false, nil
	}
	if best < op.minHops {
		return expr.Null, false, nil
	}
	if op.maxHops != shortestNoMaxHops && best > op.maxHops {
		return expr.Null, false, nil
	}

	for _, m := range meets {
		hops, ok := op.biJoin(predF, predB, src, dst, m.node)
		if ok {
			return buildHopList(src, hops), true, nil
		}
	}
	// Every candidate meeting point repeated a relationship. The argument in the
	// file comment says this is unreachable; rather than trust it at runtime,
	// hand the query to the forward-only search, which enumerates differently
	// and cannot return a non-simple path.
	return op.bfsShortestPathForward(src, dst)
}

// biExpand advances one BFS level. forward selects which search is advancing;
// the CSR each one reads is chosen by the duality documented in the file
// comment. Nodes already known to this search are skipped, so dist records the
// exact BFS distance.
func (op *ShortestPath) biExpand(frontier []uint64, pred map[uint64]spPredEntry, dist map[uint64]int, level int, forward bool) []uint64 {
	var next []uint64
	for _, node := range frontier {
		// Path edges traversed FORWARD. The forward search reads the forward CSR;
		// the backward search reads the reverse CSR to walk the same edge the
		// other way.
		if op.dir != DirIn {
			next = op.biScan(node, pred, dist, level, next, forward /* useFwdCSR */)
		}
		// Path edges traversed REVERSE. Mirror image of the above.
		if op.dir != DirOut {
			next = op.biScan(node, pred, dist, level, next, !forward /* useFwdCSR */)
		}
	}
	return next
}

// biScan scans one CSR of node, recording every undiscovered neighbour into pred
// and dist at the given level, and appends them to next.
func (op *ShortestPath) biScan(node uint64, pred map[uint64]spPredEntry, dist map[uint64]int, level int, next []uint64, useFwdCSR bool) []uint64 {
	verts, edges := op.revVerts, op.revEdges
	if useFwdCSR {
		verts, edges = op.fwdVerts, op.fwdEdges
	}
	if verts == nil || node+1 >= uint64(len(verts)) {
		return next
	}
	for pos := verts[node]; pos < verts[node+1]; pos++ {
		// The gate admits only an untyped search, so this is a constant true
		// today; it is kept rather than elided so that widening the gate cannot
		// silently drop the check.
		if !op.passesTypeFilter(pos, useFwdCSR) {
			continue
		}
		neighbour := uint64(edges[pos])
		if _, seen := pred[neighbour]; seen {
			continue
		}
		pred[neighbour] = spPredEntry{parent: node, rawPos: pos, fwd: useFwdCSR}
		dist[neighbour] = level
		next = append(next, neighbour)
	}
	return next
}

// biJoin assembles the src→dst hop sequence through the meeting node and reports
// whether it satisfies relationship-uniqueness. A false result means this
// meeting point must be abandoned, not that no path exists.
func (op *ShortestPath) biJoin(predF, predB map[uint64]spPredEntry, src, dst, meet uint64) ([]hop, bool) {
	// Forward half, meet→src, then reversed into src→meet order.
	var hops []hop
	for cur := meet; cur != src; {
		e := predF[cur]
		fwdPos, reversed := op.resolveFwdPos(e)
		hops = append(hops, hop{dstID: cur, fwdPos: fwdPos, reversed: reversed})
		cur = e.parent
	}
	for i, j := 0, len(hops)-1; i < j; i, j = i+1, j-1 {
		hops[i], hops[j] = hops[j], hops[i]
	}
	// Backward half, meet→dst, already in path order: each entry's parent is the
	// next node towards dst.
	for cur := meet; cur != dst; {
		e := predB[cur]
		fwdPos, reversed := op.resolveBackwardHop(cur, e)
		hops = append(hops, hop{dstID: e.parent, fwdPos: fwdPos, reversed: reversed})
		cur = e.parent
	}

	// Relationship-uniqueness across the WHOLE joined path. Each half is
	// individually simple because BFS never revisits a node, so only a collision
	// BETWEEN the halves can occur.
	seen := make(map[uint64]struct{}, len(hops))
	for _, h := range hops {
		// The handle is looked up from the forward position, which both halves
		// have already resolved to, so an edge reached from either side yields the
		// same handle and a collision is actually detected.
		handle := op.relHandle(h.fwdPos, true)
		if _, dup := seen[handle]; dup {
			return nil, false
		}
		seen[handle] = struct{}{}
	}
	return hops, true
}

// resolveBackwardHop resolves one hop recorded by the BACKWARD search into its
// handle-disambiguated forward position and traversal orientation. from is the
// node the hop leaves (the entry's own key); e.parent is the node it reaches,
// one step closer to dst, and e.rawPos indexes e.parent's CSR.
//
// The orientation is inverted relative to [ShortestPath.resolveFwdPos] because
// the backward search reads the opposite CSR from the one the path hop runs
// along — see the file comment's table. Getting it wrong yields a path of the
// right length whose hops hydrate to the wrong relationship instances, which is
// what TestBiBFS_DifferentialAgainstForwardOnly's per-hop validation catches.
func (op *ShortestPath) resolveBackwardHop(from uint64, e spPredEntry) (fwdPos uint64, reversed bool) {
	if e.fwd {
		// Reached over e.parent's FORWARD CSR while walking backwards, so the CSR
		// edge already runs e.parent → from and the path hop from → e.parent runs
		// against it. The position is the forward one directly; no mapping needed.
		return e.rawPos, true
	}
	// Reached over e.parent's REVERSE CSR, so the CSR edge runs from → e.parent
	// and the path hop is forward. The forward position is not the reverse one, so
	// it has to be resolved.
	//
	// Prefer the prebuilt table when this operator has one. When it does not — an
	// untyped DirOut search, where building it would cost more than the whole
	// search saves — resolve this ONE hop by scanning from's own forward slots.
	// That is O(deg(from)) for each of the path's ≤ d hops, against O(E·d) to
	// build a table whose only consumer here is this loop.
	if op.revToFwd != nil {
		return op.resolvedFwdPosOrSelf(e.rawPos), false
	}
	return op.scanFwdPos(from, e.parent, e.rawPos), false
}

// scanFwdPos finds the forward-CSR position of the edge from→to that the reverse
// slot revPos denotes. When both CSRs carry handles the match is by handle, so a
// parallel edge resolves to its OWN forward slot rather than to an arbitrary one
// between the same pair — the same disambiguation buildRevToFwd performs. It
// falls back to the reverse position when no forward slot matches, mirroring
// [ShortestPath.resolvedFwdPosOrSelf]'s unresolved case.
func (op *ShortestPath) scanFwdPos(from, to, revPos uint64) uint64 {
	if from+1 >= uint64(len(op.fwdVerts)) {
		return revPos
	}
	var wantHandle uint64
	haveHandles := op.fwdHandles != nil && op.revHandles != nil && revPos < uint64(len(op.revHandles))
	if haveHandles {
		wantHandle = op.revHandles[revPos]
	}
	for fp := op.fwdVerts[from]; fp < op.fwdVerts[from+1]; fp++ {
		if uint64(op.fwdEdges[fp]) != to {
			continue
		}
		if !haveHandles {
			return fp
		}
		if op.fwdHandles[fp] == wantHandle {
			return fp
		}
	}
	return revPos
}
