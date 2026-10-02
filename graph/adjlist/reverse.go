package adjlist

import (
	"slices"
	"sync"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// revIndex is the live in-edge index: for every node it records which nodes
// hold an edge INTO it, one entry per edge slot, so parallel edges are
// represented with their multiplicity exactly as the forward adjacency is.
//
// # Why it exists
//
// The forward adjacency answers "what does src point at" in O(1) and "what
// points at dst" not at all. Before rmp #2400 the only answer to the second
// question was [graph.Mapper.Walk] — a scan of every interned node, loading
// every adjacency entry, per question asked. The Cypher delete path asks it
// once per node deleted (the DELETE guard "does this node still have
// relationships", and DETACH DELETE's removal of the incoming edges), so
// deleting k nodes from a graph of n cost O(k·n) and the measured wipe time
// grew linearly with every node the graph had ever interned. That is the
// defect the 2026-08-11 concurrency assessment recorded as F1, and this is its
// real mechanism: in a CPU profile of the reproduction, Mapper.Walk is 78.77%
// of samples, while the tombstone-bitmap clone the assessment named as the
// root cause is 0.99% and the whole of removeNodeInfo is 1.72%.
//
// Neo4j and Memgraph both store the incoming direction beside the outgoing one
// for the same reason. This index is the same decision, kept deliberately
// smaller: it carries NodeIDs only, with no weight, handle, label or property
// column, because the questions it exists to answer never need them.
//
// # Concurrency
//
// Sharded on the destination's own shard bits, exactly like the forward
// adjacency, so two writes to different destinations never contend. It is a
// LEAF in the lock order: a caller may hold any number of adjacency shard
// locks while calling into this index, and this index never acquires an
// adjacency lock, so the order adjacency→reverse can never invert.
//
// # It is a derived index, and it is maintained where membership changes
//
// Every edge-slot insertion funnels through [AdjList.upsertEdgeLocked] and
// every edge-slot removal through one of the four removal paths, all of which
// know (src, dst) at the point they publish. Nothing else changes membership:
// the label, property and aux mutators republish an entry carrying
// `current.neighbours` unchanged. Rollback needs no special handling because
// the undo log replays the ordinary add and remove operations rather than
// restoring entry snapshots.
// The gate that lets a graph with no edges pay nothing for this index is
// [AdjList.Size], the edge counter the forward path already maintains — NOT a
// counter of its own. An earlier draft kept its own atomic.Int64 here and
// incremented it on every insertion, which put a second globally shared cache
// line on the write path for a number the AdjList was already tracking. That is
// precisely the shape graph/mvcc/horizon.go exists to avoid, and measuring it
// against a counter that had to exist anyway is not a trade worth making.
//
// # The index reads the PRESENT; a transaction reads its SNAPSHOT (rmp #2884)
//
// The membership lists record the newest stored state, which includes other
// in-flight transactions' arcs and omits arcs they have removed. A write
// transaction deciding "does d still have an incoming relationship" must answer
// from its own snapshot instead, or it refuses a DELETE over an arc it cannot
// see, and — worse — deletes a node whose incoming arc it CAN see because a
// concurrent removal already took the arc out of the list, so a rollback of that
// removal leaves a committed arc into a deleted node.
//
// So a versioned removal leaves a GHOST: the source it took out, stamped with the
// removal's own commit record. The live list and the ghosts together are the
// CANDIDATES for any snapshot a reader may hold, and
// [AdjList.InNeighbourIDsVisible] confirms each candidate against the versioned
// forward adjacency as that reader resolves it. A ghost is retired by
// [AdjList.Reclaim] once its removal is at or below the reclamation watermark —
// the instant from which no reader can resolve the forward entry it removed — or
// once the removal aborted. That is the same rule that frees the forward
// version, so the ghosts cost memory for exactly as long as the versions do.
type revIndex struct {
	shards [shardCount]revShard
}

// revShard holds the in-neighbour lists of every node whose NodeID selects this
// shard, indexed by the intra-shard component of the DESTINATION's NodeID.
type revShard struct {
	mu sync.RWMutex
	// srcs[intra] holds one entry per edge slot pointing INTO the node whose
	// intra-shard index is intra. Nil until this shard records its first
	// in-edge, so a shard no edge ever reaches costs one mutex and one nil
	// slice header.
	srcs [][]graph.NodeID
	// ghosts[intra] holds one record per edge slot a VERSIONED removal took out
	// of srcs[intra], until [revIndex.sweepGhosts] retires it. Nil until the
	// shard's first versioned removal, so an unversioned graph and a shard no
	// removal has reached pay one nil map header. See the "PRESENT and
	// SNAPSHOT" note on [revIndex].
	ghosts map[uint64][]revGhost
}

// revGhost is one removed edge slot from src, with the instant of its removal:
// the removing transaction's shared commit record, or — for an untransacted
// write — the commit timestamp it was stamped with. Exactly the pair the
// forward version record carries, so the ghost and the version resolve alike.
type revGhost struct {
	info *mvcc.CommitInfo
	ts   uint64
	src  graph.NodeID
}

// at returns the ghost's effective removal instant.
func (gh revGhost) at() uint64 {
	if gh.info != nil {
		return gh.info.TS()
	}
	return gh.ts
}

// add records one edge slot from src into dst.
func (r *revIndex) add(dst, src graph.NodeID) {
	sh := &r.shards[dst&shardMask]
	intra := uint64(dst) >> shardBits
	sh.mu.Lock()
	if intra >= uint64(len(sh.srcs)) {
		// GEOMETRIC, and the first version of this was not — it grew to exactly
		// intra+1, which reallocates and copies the whole array every time a new
		// destination appears. On BenchmarkHub_AddEdge_100k, where every edge
		// lands on a fresh destination, that made the index O(n²): +296% time and
		// +1057% allocation (45.48MiB to 526.10MiB) against the same benchmark
		// without the index at all. Doubling amortises the copy to O(1) per node.
		grown := make([][]graph.NodeID, max(intra+1, 2*uint64(len(sh.srcs))))
		copy(grown, sh.srcs)
		sh.srcs = grown
	}
	sh.srcs[intra] = append(sh.srcs[intra], src)
	sh.mu.Unlock()
}

// remove drops ONE recorded edge slot from src into dst, mirroring the forward
// removal paths, which each excise a single slot. It is a no-op when no such
// slot is recorded, so a double removal cannot drive the count negative.
//
// info and ts are the instant of the forward removal as its version record
// carries it, or both zero when the removal recorded no version. A versioned
// removal leaves a ghost of the slot behind; see [revIndex].
func (r *revIndex) remove(dst, src graph.NodeID, info *mvcc.CommitInfo, ts uint64) {
	sh := &r.shards[dst&shardMask]
	intra := uint64(dst) >> shardBits
	sh.mu.Lock()
	if intra >= uint64(len(sh.srcs)) {
		sh.mu.Unlock()
		return
	}
	list := sh.srcs[intra]
	idx := slices.Index(list, src)
	if idx < 0 {
		sh.mu.Unlock()
		return
	}
	if info != nil || ts != 0 {
		if sh.ghosts == nil {
			sh.ghosts = make(map[uint64][]revGhost, 8)
		}
		sh.ghosts[intra] = append(sh.ghosts[intra], revGhost{info: info, ts: ts, src: src})
	}
	// Order within a destination's list carries no meaning — sources reorders
	// what it returns — so excise by swapping the tail element down, which
	// keeps the removal O(1) instead of O(degree).
	last := len(list) - 1
	list[idx] = list[last]
	list[last] = 0
	if last == 0 {
		sh.srcs[intra] = nil
	} else {
		sh.srcs[intra] = list[:last]
	}
	sh.mu.Unlock()
}

// sources returns the DISTINCT nodes holding an edge into dst, excluding dst
// itself, ordered as [graph.Mapper.Walk] would have yielded them.
//
// Both the deduplication and the exclusion of dst reproduce the contract of the
// full scan this index replaced: that scan appended each source key at most
// once however many parallel edges it held, and skipped the destination itself,
// so a self-loop never made a node its own in-neighbour. The Walk ordering is
// reproduced rather than replaced because it is observable — a caller iterating
// in-neighbours to match a pattern sees them in this order — and changing it
// silently would be a behaviour change smuggled in with a performance fix.
func (r *revIndex) sources(dst graph.NodeID) []graph.NodeID {
	sh := &r.shards[dst&shardMask]
	intra := uint64(dst) >> shardBits

	sh.mu.RLock()
	var out []graph.NodeID
	if intra < uint64(len(sh.srcs)) {
		if list := sh.srcs[intra]; len(list) > 0 {
			out = make([]graph.NodeID, len(list))
			copy(out, list)
		}
	}
	sh.mu.RUnlock()

	if len(out) == 0 {
		return nil
	}
	// Mapper.Walk yields shard 0 first and, within a shard, ascending
	// intra-shard index — which is NOT ascending NodeID, because the shard
	// occupies the low bits. Sort on the same pair Walk iterates.
	slices.SortFunc(out, walkOrder)
	out = slices.Compact(out)
	if i := slices.Index(out, dst); i >= 0 {
		out = slices.Delete(out, i, i+1)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// walkOrder orders two NodeIDs as [graph.Mapper.Walk] yields them: shard first,
// then ascending intra-shard index.
func walkOrder(x, y graph.NodeID) int {
	if sx, sy := x&shardMask, y&shardMask; sx != sy {
		return int(sx) - int(sy)
	}
	switch ix, iy := uint64(x)>>shardBits, uint64(y)>>shardBits; {
	case ix < iy:
		return -1
	case ix > iy:
		return 1
	default:
		return 0
	}
}

// revCandidate is one source a snapshot reader must consider for dst: live
// reports whether the present membership list holds it, as opposed to a ghost.
type revCandidate struct {
	src  graph.NodeID
	live bool
}

// candidates returns every source a reader holding ANY snapshot may resolve an
// arc into dst from — the live membership list and the ghosts — deduplicated,
// in [graph.Mapper.Walk] order, with dst itself excluded. A source present in
// both is reported once, as live.
func (r *revIndex) candidates(dst graph.NodeID) []revCandidate {
	sh := &r.shards[dst&shardMask]
	intra := uint64(dst) >> shardBits

	sh.mu.RLock()
	var live []graph.NodeID
	if intra < uint64(len(sh.srcs)) {
		live = sh.srcs[intra]
	}
	ghosts := sh.ghosts[intra]
	if len(live) == 0 && len(ghosts) == 0 {
		sh.mu.RUnlock()
		return nil
	}
	out := make([]revCandidate, 0, len(live)+len(ghosts))
	for _, src := range live {
		out = append(out, revCandidate{src: src, live: true})
	}
	for _, gh := range ghosts {
		out = append(out, revCandidate{src: gh.src})
	}
	sh.mu.RUnlock()

	// Live before ghost for one source, so Compact keeps the live record: a live
	// candidate may take the fast path in [AdjList.InNeighbourIDsVisible].
	slices.SortFunc(out, func(x, y revCandidate) int {
		if c := walkOrder(x.src, y.src); c != 0 {
			return c
		}
		switch {
		case x.live == y.live:
			return 0
		case x.live:
			return -1
		default:
			return 1
		}
	})
	out = slices.CompactFunc(out, func(x, y revCandidate) bool { return x.src == y.src })
	if i := slices.IndexFunc(out, func(c revCandidate) bool { return c.src == dst }); i >= 0 {
		out = slices.Delete(out, i, i+1)
	}
	return out
}

// sweepGhosts retires every ghost no reader can need any more — one whose
// removal is at or below watermark, or aborted — and reports how many it
// retired. A watermark of zero retires only aborted ghosts, matching the
// "reclaim nothing" reading every other reclaimer gives it.
//
// An aborted removal's ghost is retired because nothing resolves the arc it
// names from it: the transaction's rollback re-adds the arc to the live list,
// and a transaction that aborted without a rollback leaves its adjacency claim
// on the destination, which refuses a concurrent delete of that node on its own
// (see lpg's adjacency conflict stamps).
//
// Safe for concurrent use; takes each shard's lock in turn.
func (r *revIndex) sweepGhosts(watermark uint64) (freed int) {
	for i := range r.shards {
		sh := &r.shards[i]
		sh.mu.Lock()
		for intra, list := range sh.ghosts {
			kept := list[:0]
			for _, gh := range list {
				at := gh.at()
				if at == mvcc.AbortedTS || (watermark != 0 && at <= watermark) {
					freed++
					continue
				}
				kept = append(kept, gh)
			}
			clear(list[len(kept):])
			if len(kept) == 0 {
				delete(sh.ghosts, intra)
			} else {
				sh.ghosts[intra] = kept
			}
		}
		if len(sh.ghosts) == 0 {
			sh.ghosts = nil
		}
		sh.mu.Unlock()
	}
	return freed
}

// removalStamp returns the instant the write just published into slot intraIdx
// of s superseded its predecessor — the pair the entry's own version record
// carries — or both zero when that write recorded no version. The caller must
// hold s.mu and must call it straight after the removal's [AdjList.storeEntry],
// so the current entry IS the one the removal published.
func (a *AdjList[N, W]) removalStamp(s *adjShard[W], intraIdx uint64) (*mvcc.CommitInfo, uint64) {
	if !a.versioning {
		return nil, 0
	}
	e := loadEntry(s, intraIdx)
	if e == nil {
		return nil, 0
	}
	v := e.ver.Load()
	if v == nil {
		return nil, 0
	}
	return v.info, v.ts
}

// InNeighbourIDsVisible returns the distinct NodeIDs whose adjacency entry, as a
// reader resolves it through visible, holds an arc into dst — excluding dst
// itself, in [graph.Mapper.Walk] order. It is [AdjList.InNeighbourIDs] answered
// from a SNAPSHOT rather than from the present (rmp #2884).
//
// visible is the reader's pinned verdict on a version stamp, the same function
// [AdjList.EntryViewAsOfVisible] takes. A reader that should see the present
// uses [AdjList.InNeighbourIDs] instead.
//
// # Cost
//
// The candidates are the destination's live membership list plus its ghosts
// (see [revIndex]), so the work is O(in-degree) candidates. A live candidate
// whose current entry the reader resolves to unchanged is confirmed by the
// membership list itself, in one atomic load: the list and that entry describe
// the same present, and no write the reader could not see has replaced the
// entry. Only a candidate whose entry the reader must step back through — a
// concurrent writer's work, or a removal after the reader's snapshot — pays a
// scan of that entry's neighbours, so the bulk delete of a hub's leaves stays
// linear in the hub's degree.
//
// Safe for concurrent use.
func (a *AdjList[N, W]) InNeighbourIDsVisible(dst graph.NodeID, visible func(*mvcc.CommitInfo, uint64) bool) []graph.NodeID {
	return a.inNeighbourIDsVisible(dst, visible, false)
}

// HasInNeighbourVisible reports whether [AdjList.InNeighbourIDsVisible] would
// return at least one NodeID, stopping at the first confirmed candidate.
//
// Safe for concurrent use.
func (a *AdjList[N, W]) HasInNeighbourVisible(dst graph.NodeID, visible func(*mvcc.CommitInfo, uint64) bool) bool {
	return len(a.inNeighbourIDsVisible(dst, visible, true)) > 0
}

func (a *AdjList[N, W]) inNeighbourIDsVisible(dst graph.NodeID, visible func(*mvcc.CommitInfo, uint64) bool, first bool) []graph.NodeID {
	// No edge and no version: every reader resolves every entry to the present,
	// and the present holds no arc. The bulk delete of unconnected nodes (#2400)
	// takes this branch without touching a shard.
	if a.size.Load() == 0 && a.versionActive.Load() == 0 {
		return nil
	}
	cands := a.rev.candidates(dst)
	if len(cands) == 0 {
		return nil
	}
	var out []graph.NodeID
	for _, c := range cands {
		if !a.arcVisible(c, dst, visible) {
			continue
		}
		out = append(out, c.src)
		if first {
			return out
		}
	}
	return out
}

// arcVisible reports whether candidate c's adjacency entry, as resolved through
// visible, holds an arc into dst.
func (a *AdjList[N, W]) arcVisible(c revCandidate, dst graph.NodeID, visible func(*mvcc.CommitInfo, uint64) bool) bool {
	s := &a.shards[c.src&shardMask]
	cur := loadEntry(s, uint64(c.src)>>shardBits)
	e := a.entryAsOfLoadedVisible(cur, visible)
	if e == nil {
		return false
	}
	if c.live && e == cur {
		return true
	}
	return slices.Contains(e.neighbours, dst)
}

// InNeighbourIDs returns the distinct NodeIDs holding an edge into dst,
// excluding dst itself, in [graph.Mapper.Walk] order. The result is a fresh
// slice the caller owns; a node with no incoming edge returns nil.
//
// For an undirected graph every edge is stored in both directions, so the
// answer is the node's neighbour set.
//
// InNeighbourIDs is safe for concurrent use, and takes only the destination's
// own reverse shard lock: it neither blocks nor is blocked by adjacency
// operations on other nodes.
//
// It reads the newest COMMITTED state: an entry a transaction has written and
// not published is stepped back over to the one it replaced (rmp #2965, round
// 5; see committedEntry). A transaction reads its own writes through its own
// snapshot.
func (a *AdjList[N, W]) InNeighbourIDs(dst graph.NodeID) []graph.NodeID {
	if a.versioning && a.versionActive.Load() != 0 {
		// Committed only (rmp #2965, round 5): an arc whose entry an uncommitted
		// transaction wrote resolves to the entry it replaced; see
		// [AdjList.committedEntry]. With no live version the stored state is
		// the committed state, and the reverse index answers directly.
		return a.inNeighbourIDsVisible(dst, committedVisible, false)
	}
	// A graph with no edge has no in-neighbour, so the edge counter the forward
	// path already maintains answers without touching a shard. This is the case
	// a bulk delete of unconnected nodes takes — the shape that exposed #2400.
	if a.size.Load() == 0 {
		return nil
	}
	return a.rev.sources(dst)
}

// InNeighbours returns the keys of the distinct nodes holding an edge into dst,
// excluding dst itself. Keys that the Mapper can no longer resolve are skipped.
//
// InNeighbours is safe for concurrent use.
//
// It reads the newest COMMITTED state: an entry a transaction has written and
// not published is stepped back over to the one it replaced (rmp #2965, round
// 5; see committedEntry). A transaction reads its own writes through its own
// snapshot.
func (a *AdjList[N, W]) InNeighbours(dst N) []N {
	if a.size.Load() == 0 && a.versionActive.Load() == 0 {
		return nil
	}
	dstID, ok := a.mapper.Lookup(dst)
	if !ok {
		return nil
	}
	ids := a.InNeighbourIDs(dstID)
	if len(ids) == 0 {
		return nil
	}
	out := make([]N, 0, len(ids))
	for _, id := range ids {
		if key, ok := a.mapper.Resolve(id); ok {
			out = append(out, key)
		}
	}
	return out
}

// RecordedInEdges reports how many in-edge slots the reverse index currently
// holds, counted on demand across every shard. On a consistent graph it equals
// [AdjList.Size] for a directed graph, and twice it for an undirected one,
// since an undirected edge is stored in both directions.
//
// It exists so tests can assert the index has not drifted from the forward
// adjacency — the failure mode that would matter, because an index missing an
// edge would let DETACH DELETE leave that edge behind. It is O(nodes) and takes
// every shard lock in turn, so it belongs in a test or a diagnostic, never on a
// hot path; that is also why it is not maintained as a counter, which would put
// a shared cache line on the write path to serve an assertion.
func (a *AdjList[N, W]) RecordedInEdges() int64 {
	var total int64
	for i := range a.rev.shards {
		sh := &a.rev.shards[i]
		sh.mu.RLock()
		for _, list := range sh.srcs {
			total += int64(len(list))
		}
		sh.mu.RUnlock()
	}
	return total
}

// RecordedInEdgeGhosts reports how many removed edge slots the reverse index
// still keeps as snapshot candidates (see [revIndex]), counted on demand across
// every shard. It returns to zero once [AdjList.Reclaim] has run at a watermark
// past every removal, which is what tests assert to prove the ghosts are
// bounded. Like [AdjList.RecordedInEdges] it is O(shards) and takes every shard
// lock in turn, so it belongs in a test or a diagnostic, never on a hot path.
func (a *AdjList[N, W]) RecordedInEdgeGhosts() int64 {
	var total int64
	for i := range a.rev.shards {
		sh := &a.rev.shards[i]
		sh.mu.RLock()
		for _, list := range sh.ghosts {
			total += int64(len(list))
		}
		sh.mu.RUnlock()
	}
	return total
}

// committedVisible is the visibility rule of the present-state readers: a
// change is visible once its transaction has committed. See
// [AdjList.committedEntry].
func committedVisible(info *mvcc.CommitInfo, ts uint64) bool {
	if info != nil {
		ts = info.TS()
	}
	return mvcc.Visible(ts, committedStartTS, 0)
}

// InNeighbourIDsStored is [AdjList.InNeighbourIDs] over the STORED adjacency:
// it includes arcs written by transactions that have not committed. It is the
// read a nil snapshot resolves to in the graph layer, for a writer that must
// see its own uncommitted arcs; every other reader wants InNeighbourIDs, which
// returns the newest committed state (rmp #2965, round 5).
//
// Safe for concurrent use.
func (a *AdjList[N, W]) InNeighbourIDsStored(dst graph.NodeID) []graph.NodeID {
	if a.size.Load() == 0 {
		return nil
	}
	return a.rev.sources(dst)
}
