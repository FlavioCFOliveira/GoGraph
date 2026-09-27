package cypher

import (
	"sync"
	"sync/atomic"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// live_adjacency.go — forward expansion inside a write transaction over the
// transaction's own versioned adjacency, one source at a time, instead of a
// whole-graph CSR pair and relationship-type column (rmp #2883).
//
// # The cost it removes
//
// A write transaction's view may not share the engine's cached pair (rmp #2446),
// so every Init of a traversal in a write statement built one: O(V+E), plus the
// whole-graph type resolution when the pattern is typed. An anchored
// `MATCH (:Hub {id: 0})-[r:LINK]->(:Spoke {id: 7}) DELETE r` paid for the whole
// graph to read one node's neighbours. Measured at 127c012b on example 36 that
// build was 36.9 s and 30.2 GiB, and on example 31 it was 16.9 s of 23.2 s.
//
// # Scope: forward only, directed graphs only
//
// A forward run is one node's versioned adjacency entry, which answers exactly at
// the transaction's instant. The only incoming-edge structure is a present-state
// index with no versions, and writers are concurrent, so an incoming run cannot be
// answered at an instant from it. Every traversal that reads incoming edges keeps
// the whole-graph build; see [traversalAdjacencySource] for the list. An
// undirected graph stores each edge in both endpoints' entries, which would make
// every write a two-entry change; it keeps the whole-graph build too.
//
// # The instant, and why a journal is needed to keep it
//
// The whole-graph pair is resolved once per Init. That freezes the topology an
// Init traverses at the Init instant, including against the writes the SAME
// statement makes while the traversal is still open — a CREATE or DELETE
// pipelined above it. openCypher's clause semantics depend on that: in
// `MATCH (a)-[:T]->(b) CREATE (b)-[:T]->(:X)` a source visited after the first
// CREATE must not see the relationship that CREATE made.
//
// A per-source read taken later than the Init would see it. The transaction's
// own versions cannot recover the Init instant either: a transaction's second
// write to a node collapses into its first ([adjlist.AdjList] linkVersion). So the
// statement keeps a [liveTopoLog]. Before every write that changes a node's
// outgoing entry, its types, or a node's liveness, the mutator adapter reports
// it, and the log records the state it is about to change — once per node per
// Init, and only once some live Init has begun. A run is then served:
//
//   - live, when the statement has written nothing since the Init (the common
//     case, and the only case in which runs are read before any write);
//   - otherwise from the first capture taken after the Init, if the node was
//     written since; and live if it was not.
//
// Node liveness is journaled the same way and applied per slot, because the
// whole-graph build filters every arc by the liveness of both endpoints AT ITS
// INSTANT.
//
// # Why every write is seen
//
// Every statement write reaches the graph through exactly one of the two concrete
// [exec.GraphMutator] adapters, and within them through the fourteen methods that
// change adjacency, relationship types, per-pair CREATE counts or node liveness.
// Each of those calls into the log first. TestLiveTopo_EveryAdjacencyWriteIsJournaled
// drives every one of them and asserts the journal moved;
// TestLiveTopo_GraphMutatorMethodsAreClassified fails when a method is added to
// the interface without being classified, so a new write path cannot bypass it
// silently. A statement built with any other mutator does not use live runs.
//
// # Identity, order and types: the parity rules
//
// Each run holds exactly the arcs the whole-graph build would give the source,
// ordered by (destination, handle) through the same [csr.OrderRuns], typed by the
// same resolver ([resolveSourceSlotTypes]) and encoded by the same
// [encodeSlotTypes]. The relationship identity is the stable handle, as it is for
// the whole-graph adjacency; a slot without one is identified by the rule
// [exec] documents on liveEdgeIdentity, falling back to a whole-graph adjacency at
// the Init instant when only that can answer.

// liveTopoLog is one statement's journal of its own adjacency-affecting writes,
// shared by the statement's live traversal sources and its mutator adapter.
//
// # The capture rule
//
// seq counts journaled writes; an Init records the seq it began at. A run for node
// s at an Init that began at seq0 is the state s had at seq0. If s has been written
// since, that state is the capture taken just before its first write after seq0; a
// capture is taken for write w exactly when s has none newer than the latest Init,
// which is sufficient for every earlier Init too, because the state of s cannot
// have changed between an Init and the next write to s. Liveness events follow the
// same rule.
//
// It is safe for concurrent use: every method that reads or writes the journal
// holds mu, and the two atomics are read without it only as gates.
type liveTopoLog struct {
	// armed is set by the first live Init. Until then every hook returns after
	// one atomic load, so a statement with no live traversal pays nothing else.
	armed atomic.Bool
	// seq counts the journaled writes. Read without mu only to take the fast
	// path, which is sound because a statement's writes and its traversal run on
	// one goroutine.
	seq atomic.Uint64

	mu sync.Mutex
	// latest is seq at the most recent live Init.
	latest uint64
	// view is the write view every live source of the statement reads through,
	// fixed by the first [liveTopoLog.bind]; live is its liveness filter.
	view *lpg.ReadView[string, float64]
	live func(graph.NodeID) bool
	// caps and lives hold, per node, the captures and liveness events in seq
	// order. nil until the first one.
	caps  map[graph.NodeID][]liveCapture
	lives map[graph.NodeID][]liveEvent
	// sc is the resolver scratch, used under mu.
	sc *slotTypeScratch
	// vtmp is the two-entry offsets array [csr.OrderRuns] orders one run with.
	vtmp [2]uint64
	// last is the run most recently SERVED, resolved typed and unfiltered, still
	// sitting in the serving operator's buffers, and lastSeq the journal instant
	// it was resolved at. A capture of that node at that same instant — the
	// common `MATCH (a)-[r:T]->(b) DELETE r` shape, whose write lands on the node
	// just served — copies it instead of resolving the node again: nothing was
	// journaled in between, so the state is identical. It is invalidated by the
	// next serve, whose buffers it aliases.
	last      liveRun
	lastID    graph.NodeID
	lastSeq   uint64
	lastValid bool
}

// liveCapture is the state of one node's outgoing run just before the write
// numbered w.
type liveCapture struct {
	w   uint64
	run liveRun
}

// liveEvent is a node's liveness just before the liveness-changing write w.
type liveEvent struct {
	w       uint64
	wasLive bool
}

// liveRun is one node's outgoing run, ordered by (destination, handle) and NOT
// filtered by liveness: the liveness that applies depends on the reading Init, so
// it is applied when the run is served. Resolving types before filtering gives the
// same codes as resolving after it, because every resolution rule is per slot or
// per destination pair, and the liveness filter removes whole pairs.
type liveRun struct {
	dsts      []graph.NodeID
	handles   []uint64 // nil when the entry carries no handle column
	codes     []uint32
	extra     map[uint64][]uint32
	handleCol bool
}

// bind fixes the view the statement's live sources read through, and reports
// whether g reads at the same instant as the view already fixed. A source whose
// view differs is not served live: its runs would be answered at another instant.
func (l *liveTopoLog) bind(g *lpg.ReadView[string, float64]) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.view == nil {
		l.view = g
		l.live = g.LiveNodeFilter()
		return true
	}
	return l.view == g || l.view.Snapshot() == g.Snapshot()
}

// begin records a live Init and returns the instant it reads at.
func (l *liveTopoLog) begin() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.seq.Load()
	l.latest = s
	l.armed.Store(true)
	return s
}

// isLiveLocked reports a node's liveness at the view's present, own writes
// included.
func (l *liveTopoLog) isLiveLocked(id graph.NodeID) bool {
	return l.live == nil || l.live(id)
}

// beforeAdjWrite journals a write about to change the outgoing entry of the node
// keyed srcKey, its relationship types or its per-pair CREATE counts. It must be
// called BEFORE the write.
func (l *liveTopoLog) beforeAdjWrite(srcKey string) {
	if l == nil || !l.armed.Load() {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.seq.Add(1)
	id, ok := l.view.AdjList().Mapper().Lookup(srcKey)
	if !ok {
		// No entry exists yet, so there is nothing to preserve: at every earlier
		// instant this node had no outgoing arcs, which is what an absent node
		// reads as. Its liveness is journaled by the node hooks.
		return
	}
	caps := l.caps[id]
	if n := len(caps); n > 0 && caps[n-1].w > l.latest {
		return // an Init-instant state newer than every Init is already held
	}
	if l.caps == nil {
		l.caps = make(map[graph.NodeID][]liveCapture)
	}
	// A capture is resolved typed whatever the statement's patterns are: a
	// typed live source may be bound after it, by a subquery planned at
	// execution time, and the types it would need are about to change.
	var run liveRun
	if l.lastValid && l.lastID == id && l.lastSeq == w-1 {
		run = liveRun{
			dsts:      append([]graph.NodeID(nil), l.last.dsts...),
			handles:   append([]uint64(nil), l.last.handles...),
			codes:     append([]uint32(nil), l.last.codes...),
			extra:     l.last.extra, // never written after resolution
			handleCol: l.last.handleCol,
		}
		liveCaptureReuses.Add(1)
	} else {
		l.resolveLocked(id, &run, true)
	}
	l.caps[id] = append(caps, liveCapture{w: w, run: run})
}

// liveNodePre is a node's liveness captured before a write that may change it.
type liveNodePre struct {
	armed   bool
	wasLive bool
}

// nodePre captures the liveness of the node keyed key before a write that may
// create, revive or remove it. A key that is not interned reads as not live.
func (l *liveTopoLog) nodePre(key string) liveNodePre {
	if l == nil || !l.armed.Load() {
		return liveNodePre{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	id, ok := l.view.AdjList().Mapper().Lookup(key)
	return liveNodePre{armed: true, wasLive: ok && l.isLiveLocked(id)}
}

// nodePost journals the liveness pre captured for the node keyed key, once the
// write has happened and the key is certain to be interned. Journaling a liveness
// that did not change is harmless: the recorded value is the state at every Init
// the event can serve.
func (l *liveTopoLog) nodePost(key string, pre liveNodePre) {
	if l == nil || !pre.armed {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.seq.Add(1)
	id, ok := l.view.AdjList().Mapper().Lookup(key)
	if !ok {
		return // the write did not intern the node, so it is still not live
	}
	l.noteLivenessLocked(id, w, pre.wasLive)
}

// beforeNodeRemove journals the liveness of the node keyed key before a write
// that may remove it.
func (l *liveTopoLog) beforeNodeRemove(key string) {
	if l == nil || !l.armed.Load() {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.seq.Add(1)
	id, ok := l.view.AdjList().Mapper().Lookup(key)
	if !ok {
		return
	}
	l.noteLivenessLocked(id, w, l.isLiveLocked(id))
}

func (l *liveTopoLog) noteLivenessLocked(id graph.NodeID, w uint64, wasLive bool) {
	evs := l.lives[id]
	if n := len(evs); n > 0 && evs[n-1].w > l.latest {
		return
	}
	if l.lives == nil {
		l.lives = make(map[graph.NodeID][]liveEvent)
	}
	l.lives[id] = append(evs, liveEvent{w: w, wasLive: wasLive})
}

// liveAtLocked reports node id's liveness at the Init that began at seq0.
func (l *liveTopoLog) liveAtLocked(id graph.NodeID, seq0 uint64) bool {
	for _, ev := range l.lives[id] {
		if ev.w > seq0 {
			return ev.wasLive
		}
	}
	return l.isLiveLocked(id)
}

// captureAtLocked returns the capture holding node id's run at the Init that
// began at seq0, or nil when the node has not been written since.
func (l *liveTopoLog) captureAtLocked(id graph.NodeID, seq0 uint64) *liveRun {
	caps := l.caps[id]
	for i := range caps {
		if caps[i].w > seq0 {
			return &caps[i].run
		}
	}
	return nil
}

// resolveLocked reads node id's outgoing entry at the view's present — the
// transaction's instant, own writes included — and orders it, appending into
// run's slices. With typed set it also resolves every slot's relationship types;
// otherwise the codes are zero, which only an untyped traversal reads, and never.
//
// run.handles keeps its capacity when the entry has no handle column; handleCol,
// not the slice, says whether one exists.
func (l *liveTopoLog) resolveLocked(id graph.NodeID, run *liveRun, typed bool) {
	g := l.view
	adj := g.AdjList()
	snap := g.Snapshot()
	v := adj.EntryViewAsOf(id, snap.StartTS(), snap.TxID())
	run.dsts = append(run.dsts[:0], v.Neighbours...)
	run.handleCol = v.Handles != nil
	run.handles = append(run.handles[:0], v.Handles...)
	var hs []uint64
	if run.handleCol {
		hs = run.handles
	}
	n := len(run.dsts)
	l.vtmp = [2]uint64{0, uint64(n)}
	csr.OrderRuns(l.vtmp[:], run.dsts, []float64(nil), hs)
	if cap(run.codes) < n {
		run.codes = make([]uint32, n)
	} else {
		run.codes = run.codes[:n]
		clear(run.codes)
	}
	run.extra = nil
	if n == 0 || !typed {
		return
	}
	if l.sc == nil {
		l.sc = newSlotTypeScratch()
	}
	reg := g.Registry()
	codes := run.codes
	var extra map[uint64][]uint32
	resolveSourceSlotTypes(g, adj.Mapper(), l.sc, id, run.dsts, hs, 0,
		func(pos uint64, types []string) { extra = encodeSlotTypes(reg, codes, extra, pos, types) })
	run.extra = extra
}

// runAtLocked writes into out node id's run at the Init that began at seq0,
// filtered by liveness at that instant, exactly as the whole-graph build would
// have given it.
func (l *liveTopoLog) runAtLocked(id graph.NodeID, seq0 uint64, out *liveRun, typed bool) {
	// out's buffers are about to be overwritten, and they may be what last
	// aliases.
	l.lastValid = false
	fast := l.seq.Load() == seq0
	var src *liveRun
	if !fast {
		src = l.captureAtLocked(id, seq0)
	}
	if src == nil {
		l.resolveLocked(id, out, typed)
	} else {
		out.dsts = append(out.dsts[:0], src.dsts...)
		out.handleCol = src.handleCol
		out.handles = append(out.handles[:0], src.handles...)
		out.codes = append(out.codes[:0], src.codes...)
		out.extra = src.extra
	}
	liveAt := l.isLiveLocked
	if !fast {
		liveAt = func(x graph.NodeID) bool { return l.liveAtLocked(x, seq0) }
	}
	if !liveAt(id) {
		// A source that was not live contributes no arc and does not count
		// towards the build's handle-column criterion.
		out.dsts, out.handles, out.codes = out.dsts[:0], out.handles[:0], out.codes[:0]
		out.extra, out.handleCol = nil, false
		return
	}
	// Keep only arcs whose destination was live, compacting every column under
	// the same permutation. Filtering an ordered run keeps it ordered. The capture
	// a run was copied from is never written: a surviving multi-type slot is
	// re-keyed into a fresh map.
	k := 0
	var extra map[uint64][]uint32
	for i, d := range out.dsts {
		if !liveAt(d) {
			continue
		}
		out.dsts[k] = d
		if out.handleCol {
			out.handles[k] = out.handles[i]
		}
		out.codes[k] = out.codes[i]
		if e, ok := out.extra[uint64(i)]; ok {
			if extra == nil {
				extra = make(map[uint64][]uint32, len(out.extra))
			}
			extra[uint64(k)] = e
		}
		k++
	}
	unfiltered := k == len(out.dsts)
	out.dsts, out.codes = out.dsts[:k], out.codes[:k]
	if !unfiltered {
		out.extra = extra
	}
	if out.handleCol {
		out.handles = out.handles[:k]
	} else {
		out.handles = out.handles[:0]
	}
	// Remember a run served at the present, typed and with nothing filtered: it
	// is then exactly what a capture of this node at this instant would resolve.
	if fast && src == nil && typed && unfiltered {
		l.last, l.lastID, l.lastSeq, l.lastValid = *out, id, l.seq.Load(), true
	}
}

// liveCaptureReuses counts captures served from the run just served rather than
// resolved again. Process-global and monotonic; tests bracket a drive.
var liveCaptureReuses atomic.Uint64

// liveOutSource is the forward adjacency a live traversal reads through. It
// implements exec's unexported liveOutAdjacency protocol; its CSRAdjacency
// methods describe an empty graph, so an operator that did not recognise the
// protocol would see nothing rather than something wrong.
//
// Safe for concurrent use: all state it reads lives in its log, under the log's
// mutex, or is immutable.
type liveOutSource struct {
	log      *liveTopoLog
	bopts    *buildOpts
	g        *lpg.ReadView[string, float64]
	relTypes []string
}

// VerticesSlice implements [exec.CSRAdjacency]; a live source has no whole-graph
// arrays.
func (s *liveOutSource) VerticesSlice() []uint64 { return nil }

// EdgesSlice implements [exec.CSRAdjacency].
func (s *liveOutSource) EdgesSlice() []graph.NodeID { return nil }

// HandlesSlice implements [exec.CSRAdjacency].
func (s *liveOutSource) HandlesSlice() []uint64 { return nil }

// LiveBegin records an Init and resolves the accepted types at its instant, as
// the whole-graph source resolves them in the same place.
func (s *liveOutSource) LiveBegin() (uint64, []uint32) {
	return s.log.begin(), relTypeCodesFor(s.g, s.relTypes)
}

// LiveOutRun serves src's run at the Init that began at instant.
func (s *liveOutSource) LiveOutRun(
	src graph.NodeID, instant uint64, dsts []graph.NodeID, handles []uint64, codes []uint32,
) ([]graph.NodeID, []uint64, []uint32, map[uint64][]uint32, bool) {
	s.log.mu.Lock()
	defer s.log.mu.Unlock()
	run := liveRun{dsts: dsts, handles: handles, codes: codes}
	s.log.runAtLocked(src, instant, &run, len(s.relTypes) > 0)
	return run.dsts, run.handles, run.codes, run.extra, run.handleCol
}

// LiveFallback returns the whole-graph forward adjacency at the Init that began at
// instant. With no write since the Init the present IS that instant, and the
// ordinary build answers exactly as it would have at the Init. Otherwise the
// adjacency is assembled from the same per-node runs the live path serves.
func (s *liveOutSource) LiveFallback(instant uint64) (exec.CSRAdjacency, exec.RelTypeAdmit) {
	typed := len(s.relTypes) > 0
	if s.log.seq.Load() == instant {
		if typed {
			f, _, col := csrPairAndColumnCachedFor(s.bopts, s.g)
			return f, col.Admit(relTypeCodesFor(s.g, s.relTypes))
		}
		f, _ := csrPairCachedFor(s.bopts, s.g)
		return f, exec.RelTypeAdmit{}
	}
	fwd, codes, extra := s.assembleAt(instant)
	if !typed {
		return fwd, exec.RelTypeAdmit{}
	}
	return fwd, exec.NewRelTypeColumn(codes, nil, extra, nil).Admit(relTypeCodesFor(s.g, s.relTypes))
}

// assembleAt builds the whole forward adjacency at the Init that began at seq0
// from per-node runs, with the handle column the whole-graph build would give it.
func (s *liveOutSource) assembleAt(seq0 uint64) (*liveFwdCSR, []uint32, map[uint64][]uint32) {
	l := s.log
	l.mu.Lock()
	defer l.mu.Unlock()
	maxID := uint64(l.view.AdjList().MaxNodeID())
	out := &liveFwdCSR{vertices: make([]uint64, maxID+1)}
	var handles []uint64
	var codes []uint32
	var extra map[uint64][]uint32
	anyHandles := false
	var run liveRun
	for id := uint64(0); id < maxID; id++ {
		out.vertices[id] = uint64(len(out.edges))
		l.runAtLocked(graph.NodeID(id), seq0, &run, len(s.relTypes) > 0)
		base := uint64(len(out.edges))
		out.edges = append(out.edges, run.dsts...)
		codes = append(codes, run.codes...)
		if run.handleCol {
			anyHandles = true
			handles = append(handles, run.handles...)
		} else {
			handles = append(handles, make([]uint64, len(run.dsts))...)
		}
		for i, e := range run.extra {
			if extra == nil {
				extra = make(map[uint64][]uint32)
			}
			extra[base+i] = e
		}
	}
	out.vertices[maxID] = uint64(len(out.edges))
	if anyHandles {
		out.handles = handles
	}
	return out, codes, extra
}

// liveFwdCSR is a forward adjacency assembled by [liveOutSource.assembleAt].
// Immutable once built and safe for concurrent reads.
type liveFwdCSR struct {
	vertices []uint64
	edges    []graph.NodeID
	handles  []uint64
}

// VerticesSlice implements [exec.CSRAdjacency].
func (c *liveFwdCSR) VerticesSlice() []uint64 { return c.vertices }

// EdgesSlice implements [exec.CSRAdjacency].
func (c *liveFwdCSR) EdgesSlice() []graph.NodeID { return c.edges }

// HandlesSlice implements [exec.CSRAdjacency].
func (c *liveFwdCSR) HandlesSlice() []uint64 { return c.handles }

// traversalAdjacencySource is [expandAdjacencySource] for the three operators that
// serve a live forward run: [exec.Expand], [exec.OptionalExpand] (through its inner
// Expand) and [exec.VarLengthExpand]. It returns a live source when:
//
//   - the traversal reads outgoing edges only (dir is [exec.DirOut]);
//   - g is a write transaction's view ([viewCarriesOwnWrites]) — a pure reader
//     keeps the engine's cached pair, which is cheaper still once warm;
//   - the statement was built with a concrete mutator adapter, which is what
//     sets bopts.liveTopo and journals the statement's writes;
//   - the graph is directed; and
//   - g reads at the instant the statement's other live sources read at.
//
// Every other traversal keeps the whole-graph build: an incoming or undirected
// Expand, OptionalExpand or VarLengthExpand; shortestPath and allShortestPaths,
// whose bidirectional search reads the reverse frontier; the fused cyclic expand
// (ExpandIntersect), whose second leg reads incoming edges; and the
// relationship-reconstruction helpers ([ensureEdgeIDResolver], [ensureFwdCSR]),
// which index positions of a whole-graph adjacency.
func traversalAdjacencySource(
	bopts *buildOpts, g *lpg.ReadView[string, float64], relTypes []string, dir exec.Direction,
) exec.AdjacencySource {
	if dir == exec.DirOut && bopts != nil && bopts.liveTopo != nil && g != nil &&
		viewCarriesOwnWrites(g) && g.AdjList().Directed() && bopts.liveTopo.bind(g) {
		src := &liveOutSource{log: bopts.liveTopo, bopts: bopts, g: g, relTypes: relTypes}
		return func() (exec.CSRAdjacency, exec.CSRAdjacency, exec.RelTypeAdmit) {
			return src, nil, exec.RelTypeAdmit{}
		}
	}
	return expandAdjacencySource(bopts, g, relTypes)
}
