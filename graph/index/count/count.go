// Package count holds the derived, non-durable relationship count-store that
// backs exact cardinality estimates for the Cypher planner (design
// docs/count-store-design.md, task #2082). It maintains three relationship
// statistics keyed by the stable interned ids of the graph's single
// label/relationship-type registry:
//
//	E(relType)            — live edges of a relationship type
//	D(label, relType, dir)— degree-sum: edge endpoints of relType in a direction
//	                        whose this-end node carries label
//	T(labelA, relType, labelB) — live edges (:labelA)-[:relType]->(:labelB)
//
// The node statistic N(label) is NOT stored here; it is read from the existing
// label index (see cypher/api.go ResolveLabelCount).
//
// # Structure
//
// Each cell is an [atomic.Int64] held in one of a fixed number of shards; a
// cell is created on first observation of a combination and DELETED when its
// counter returns to zero, so the store's footprint is bounded by the number of
// currently-observed schema combinations — a function of schema cardinality,
// never of |V| or |E| (design §2.3). Keys are the registry's uint32 ids, so no
// string touches the hot path.
//
// # Concurrency contract
//
// The Store is safe for concurrent use, and it no longer rests on any exclusion
// the engine provides. This contract used to say that all MUTATIONS were
// serialised by the engine's write barrier (visMu.Lock in commitUnderBarrier) and
// that all READS ran under a read barrier (visMu.RLock in Graph.View). BOTH HALVES
// ARE FALSE, and have been since sprint 334 made MVCC the module's concurrency
// control: commitUnderBarrier now runs inside a SHARED hold, so two writers mutate
// this store concurrently, and an ordinary query read takes no barrier at all —
// Graph.View survives only for DDL-adjacent scans.
//
// What makes it safe is therefore the structure itself, not exclusion:
//
//   - the per-shard [sync.RWMutex] serialises the insert-add-delete-on-zero
//     sequence in [Store.add], which is the only sequence that is not a single
//     atomic operation. It is genuinely contended now rather than defence-in-depth;
//   - the atomic cells make an individual counter read lock-free regardless;
//   - the aggregate is ORDER-INSENSITIVE (rmp #2303): a cell is deleted at exactly
//     zero rather than at zero-or-below, so concurrent partial sums that transit a
//     negative value do not lose a decrement. That property is what replaced writer
//     exclusion, and [Store.add] documents the failure it fixes.
//
// The store spawns no goroutines.
package count

import (
	"sync"
	"sync/atomic"
)

// numShards is the fixed shard fan-out for the cell maps. It MUST be a power of
// two (the shard index is a hash masked by numShards-1). Sixty-four mirrors the
// per-vertex/per-edge striping the lpg property and label stores use and keeps
// key-insertion contention off the counter path; the empty shards cost only a
// few KB per graph.
const numShards = 64

const shardMask = numShards - 1

// Direction selects which end of a relationship a [D] degree-sum counts.
type Direction uint8

const (
	// Out counts the source endpoint's label (n)-[rt]->().
	Out Direction = iota
	// In counts the destination endpoint's label ()-[rt]->(n).
	In
)

// Kind selects which count family a [Delta] targets.
type Kind uint8

const (
	// KindE targets E(relType); only RT and Delta are read.
	KindE Kind = iota
	// KindD targets D(label, relType, dir); A (the label), RT, Dir and Delta are read.
	KindD
	// KindT targets T(labelA, relType, labelB); A, RT, B and Delta are read.
	KindT
)

// Delta is one buffered increment to a single count cell. It is a small
// value carried by copy; a transaction accumulates a slice of them in the
// engine's CountBuffer and applies them at commit via [Store.Apply].
type Delta struct {
	A     uint32    // KindD: label; KindT: labelA; KindE: unused.
	RT    uint32    // relationship-type id.
	B     uint32    // KindT: labelB; otherwise unused.
	Delta int64     // signed increment (+1 on create, -1 on remove).
	Kind  Kind      // which family this delta targets.
	Dir   Direction // KindD only.
}

// EDelta builds an E(relType) increment.
func EDelta(rt uint32, sign int64) Delta { return Delta{Kind: KindE, RT: rt, Delta: sign} }

// DDelta builds a D(label, relType, dir) increment.
func DDelta(label, rt uint32, dir Direction, sign int64) Delta {
	return Delta{Kind: KindD, A: label, RT: rt, Dir: dir, Delta: sign}
}

// TDelta builds a T(labelA, relType, labelB) increment.
func TDelta(a, rt, b uint32, sign int64) Delta {
	return Delta{Kind: KindT, A: a, RT: rt, B: b, Delta: sign}
}

// DirtyScope selects which X-scoped exactness set a [DirtyMark] toggles off.
type DirtyScope uint8

const (
	// DirtyDOut marks D(label, *, OUT) untrustworthy for a label.
	DirtyDOut DirtyScope = iota
	// DirtyDIn marks D(label, *, IN) untrustworthy for a label.
	DirtyDIn
	// DirtyTA marks T(label, *, *) untrustworthy (the a-position).
	DirtyTA
	// DirtyTB marks T(*, *, label) untrustworthy (the b-position).
	DirtyTB
)

// DirtyMark records that a family becomes non-exact for one label id, buffered
// alongside deltas and applied at commit via [Store.MarkDirty]. See design
// §3.3.1: a relabel whose IN-side cannot be enumerated in O(delta) marks the
// minimal X-scoped IN cells dirty rather than writing a wrong exact.
type DirtyMark struct {
	Label uint32
	Scope DirtyScope
}

// triKey is the comparable map key for a T cell. It hashes without allocation.
type triKey struct{ a, rt, b uint32 }

// dkey packs a (label, relType) pair into the D map key.
func dkey(label, rt uint32) uint64 { return uint64(label)<<32 | uint64(rt) }

// shard is one stripe of the cell maps. Its RWMutex guards the three maps'
// structure (key insert/delete); the *atomic.Int64 values are read and written
// lock-free.
type shard struct {
	e    map[uint32]*atomic.Int64
	dOut map[uint64]*atomic.Int64
	dIn  map[uint64]*atomic.Int64
	t    map[triKey]*atomic.Int64
	mu   sync.RWMutex
}

// Store is the sharded relationship count-store. Its zero value is not usable;
// construct one with [New].
type Store struct {
	shards [numShards]shard

	// budget is the per-relabel out-degree fan-out ceiling (design §3.3.1,
	// EngineOptions.MaxLabelRecountEdges). A relabel of a node with more than
	// budget out-edges dirties the OUT X-scoped cells instead of recounting them.
	// Zero or negative means no ceiling (always recount the OUT side exactly).
	budget int

	// dmu guards the four X-scoped dirty sets. They only ever grow within a
	// session (until RecomputeReset), bounded by the number of distinct labels
	// ever relabelled — a schema-cardinality bound, data-size-independent.
	dmu       sync.RWMutex
	dDirtyOut map[uint32]struct{}
	dDirtyIn  map[uint32]struct{}
	tDirtyA   map[uint32]struct{}
	tDirtyB   map[uint32]struct{}
}

// New returns an empty, ready-to-use Store whose per-relabel OUT-side recount
// ceiling is maxRecountEdges (design §3.3.1). A maxRecountEdges of 0 or less
// disables the ceiling (the OUT side is always recounted exactly).
func New(maxRecountEdges int) *Store {
	s := &Store{
		budget:    maxRecountEdges,
		dDirtyOut: make(map[uint32]struct{}),
		dDirtyIn:  make(map[uint32]struct{}),
		tDirtyA:   make(map[uint32]struct{}),
		tDirtyB:   make(map[uint32]struct{}),
	}
	for i := range s.shards {
		sh := &s.shards[i]
		sh.e = make(map[uint32]*atomic.Int64)
		sh.dOut = make(map[uint64]*atomic.Int64)
		sh.dIn = make(map[uint64]*atomic.Int64)
		sh.t = make(map[triKey]*atomic.Int64)
	}
	return s
}

// MaxRecountEdges reports the per-relabel OUT-side recount ceiling (0 or less
// means unbounded). The relabel maintenance consults it to decide between an
// exact OUT-side recount and an X-scoped OUT dirty marking (design §3.3.1).
func (s *Store) MaxRecountEdges() int { return s.budget }

// mix32 is a cheap integer bit-finaliser (an xorshift-multiply) that spreads the
// low, densely-packed registry ids across the shard space so a small vocabulary
// does not cluster on a few shards.
func mix32(x uint32) uint32 {
	x *= 0x9E3779B1
	x ^= x >> 16
	return x
}

func (s *Store) eShardOf(rt uint32) *shard { return &s.shards[mix32(rt)&shardMask] }
func (s *Store) dShardOf(k uint64) *shard  { return &s.shards[mix32(uint32(k)^uint32(k>>32))&shardMask] }
func (s *Store) tShardOf(k triKey) *shard {
	return &s.shards[(mix32(k.a)^mix32(k.rt)^mix32(k.b))&shardMask]
}

// Apply applies one buffered delta to its cell. A key is created on first
// observation and deleted when its counter returns to zero (bounded growth). A
// zero delta is a no-op.
//
// It needs NO serialisation from the caller (rmp #2345). Every cell it touches goes
// through [Store.add], whose aggregate is ORDER-INSENSITIVE — a cell is deleted at
// exactly zero and a negative cell is retained, so addition commutes — and each
// touch is made under that cell's own per-shard lock. Two writers applying
// concurrently therefore reach the same totals in any interleaving. The package
// contract states this at the top; it is restated here because this doc used to
// require the engine's write barrier, which has not serialised writers since
// rmp #2320.
func (s *Store) Apply(d Delta) {
	if d.Delta == 0 {
		return
	}
	switch d.Kind {
	case KindE:
		s.add(s.eShardOf(d.RT), func(sh *shard) *atomic.Int64 { return sh.e[d.RT] },
			func(sh *shard, c *atomic.Int64) { sh.e[d.RT] = c },
			func(sh *shard) { delete(sh.e, d.RT) }, d.Delta)
	case KindD:
		k := dkey(d.A, d.RT)
		m := func(sh *shard) map[uint64]*atomic.Int64 { return sh.dOut }
		if d.Dir == In {
			m = func(sh *shard) map[uint64]*atomic.Int64 { return sh.dIn }
		}
		s.add(s.dShardOf(k), func(sh *shard) *atomic.Int64 { return m(sh)[k] },
			func(sh *shard, c *atomic.Int64) { m(sh)[k] = c },
			func(sh *shard) { delete(m(sh), k) }, d.Delta)
	case KindT:
		tk := triKey{a: d.A, rt: d.RT, b: d.B}
		s.add(s.tShardOf(tk), func(sh *shard) *atomic.Int64 { return sh.t[tk] },
			func(sh *shard, c *atomic.Int64) { sh.t[tk] = c },
			func(sh *shard) { delete(sh.t, tk) }, d.Delta)
	}
}

// add is the shared insert-add-delete-on-zero routine. It runs entirely under
// the shard write lock; the closures read/insert/delete the family-specific map
// entry. The lock is REAL contention, not defence-in-depth: this comment used to
// say writes are serialised by the engine barrier so the lock is uncontended in
// production, and that has been false since sprint 334 let two writers commit at
// once under a shared hold. Sizing the shard count is therefore a live
// performance question rather than a settled one.
func (s *Store) add(sh *shard, get func(*shard) *atomic.Int64, put func(*shard, *atomic.Int64), del func(*shard), delta int64) {
	sh.mu.Lock()
	cell := get(sh)
	if cell == nil {
		cell = new(atomic.Int64)
		put(sh, cell)
	}
	// Deleted at EXACTLY zero, not at zero-or-below (rmp #2303, MVCC B1).
	//
	// `<= 0` clamped: a cell driven negative was deleted, the negative value was
	// discarded, and the next increment recreated the cell from zero — so the
	// decrement was permanently lost. That made the aggregate ORDER-SENSITIVE, and
	// therefore dependent on writer exclusion: applying -1 then +1 to an empty
	// cell read 1, where +1 then -1 read 0. Under the visibility barrier the base
	// was always correct so no partial sum could go negative and the clamp was
	// unreachable; the moment writers commit concurrently, one transaction's
	// decrements can land before another's increments and the clamp silently eats
	// them. TestCountStore_ConcurrentDeltasReachZeroFromEitherOrder fails against
	// the `<= 0` form.
	//
	// Retaining a negative cell is what makes addition commute here, which is this
	// store's whole ordering basis. It costs nothing in the steady state: a
	// negative cell is transient under a correct workload — the matching increment
	// takes it to exactly zero, where it is deleted — so the bounded-growth
	// property the delete exists for is unchanged.
	if cell.Add(delta) == 0 {
		del(sh)
	}
	sh.mu.Unlock()
}

// CountE returns the live edge count of relationship type rt (0 when absent).
func (s *Store) CountE(rt uint32) int64 {
	sh := s.eShardOf(rt)
	sh.mu.RLock()
	cell := sh.e[rt]
	sh.mu.RUnlock()
	if cell == nil {
		return 0
	}
	return cell.Load()
}

// CountD returns the degree-sum D(label, rt, dir) (0 when absent). It ignores
// the dirty flag; callers that need the exactness verdict consult [Store.DDirty].
func (s *Store) CountD(label, rt uint32, dir Direction) int64 {
	k := dkey(label, rt)
	sh := s.dShardOf(k)
	sh.mu.RLock()
	var cell *atomic.Int64
	if dir == In {
		cell = sh.dIn[k]
	} else {
		cell = sh.dOut[k]
	}
	sh.mu.RUnlock()
	if cell == nil {
		return 0
	}
	return cell.Load()
}

// CountT returns the triple count T(a, rt, b) (0 when absent). It ignores the
// dirty flag; callers that need the exactness verdict consult [Store.TDirty].
func (s *Store) CountT(a, rt, b uint32) int64 {
	tk := triKey{a: a, rt: rt, b: b}
	sh := s.tShardOf(tk)
	sh.mu.RLock()
	cell := sh.t[tk]
	sh.mu.RUnlock()
	if cell == nil {
		return 0
	}
	return cell.Load()
}

// MarkDirty toggles off the exactness of one X-scoped family set. It is a
// mutation. It needs no caller serialisation, for the order-insensitivity reason
// given on [Store.Apply].
func (s *Store) MarkDirty(m DirtyMark) {
	s.dmu.Lock()
	switch m.Scope {
	case DirtyDOut:
		s.dDirtyOut[m.Label] = struct{}{}
	case DirtyDIn:
		s.dDirtyIn[m.Label] = struct{}{}
	case DirtyTA:
		s.tDirtyA[m.Label] = struct{}{}
	case DirtyTB:
		s.tDirtyB[m.Label] = struct{}{}
	}
	s.dmu.Unlock()
}

// DDirty reports whether D(label, *, dir) is currently non-exact.
func (s *Store) DDirty(label uint32, dir Direction) bool {
	s.dmu.RLock()
	defer s.dmu.RUnlock()
	if dir == In {
		_, ok := s.dDirtyIn[label]
		return ok
	}
	_, ok := s.dDirtyOut[label]
	return ok
}

// TDirty reports whether T(a, *, b) is currently non-exact — true when either
// the a-position label or the b-position label has been marked dirty.
func (s *Store) TDirty(a, b uint32) bool {
	s.dmu.RLock()
	defer s.dmu.RUnlock()
	if _, ok := s.tDirtyA[a]; ok {
		return true
	}
	_, ok := s.tDirtyB[b]
	return ok
}

// Snapshot is a point-in-time copy of every live cell and dirty marking, for
// observability and differential testing. The D keys are dkey(label, relType)
// = label<<32|relType; the T keys are [3]uint32{labelA, relType, labelB}. The
// dirty slices list the label ids currently marked non-exact in each family.
type Snapshot struct {
	E         map[uint32]int64
	DOut      map[uint64]int64
	DIn       map[uint64]int64
	T         map[[3]uint32]int64
	DirtyDOut []uint32
	DirtyDIn  []uint32
	DirtyTA   []uint32
	DirtyTB   []uint32
}

// Snapshot returns a copy of every live cell (value > 0) and every dirty marking.
// It is a read taken under the shard and dirty read locks, so it is safe to call
// concurrently with writers, which are NOT serialised against each other.
func (s *Store) Snapshot() Snapshot {
	snap := Snapshot{
		E:    make(map[uint32]int64),
		DOut: make(map[uint64]int64),
		DIn:  make(map[uint64]int64),
		T:    make(map[[3]uint32]int64),
	}
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.RLock()
		for rt, c := range sh.e {
			if v := c.Load(); v != 0 {
				snap.E[rt] = v
			}
		}
		for k, c := range sh.dOut {
			if v := c.Load(); v != 0 {
				snap.DOut[k] = v
			}
		}
		for k, c := range sh.dIn {
			if v := c.Load(); v != 0 {
				snap.DIn[k] = v
			}
		}
		for k, c := range sh.t {
			if v := c.Load(); v != 0 {
				snap.T[[3]uint32{k.a, k.rt, k.b}] = v
			}
		}
		sh.mu.RUnlock()
	}
	s.dmu.RLock()
	snap.DirtyDOut = keysOf(s.dDirtyOut)
	snap.DirtyDIn = keysOf(s.dDirtyIn)
	snap.DirtyTA = keysOf(s.tDirtyA)
	snap.DirtyTB = keysOf(s.tDirtyB)
	s.dmu.RUnlock()
	return snap
}

// Cells reports the number of distinct live count cells currently held — the
// sum over every shard of the E, D(out), D(in) and T map sizes. Because a cell
// is deleted the moment its counter returns to zero ([Store.add]), every map
// entry is a live combination, so this is an exact, allocation-free size
// indicator for observability: it is bounded by the number of currently-observed
// schema combinations (design §2.3), never by |V| or |E|. It is a read taken
// under the shard read locks and is safe to call concurrently with writers, which
// are NOT serialised against each other. The metrics [Backend] exposes no gauge, so this is
// the accessor an observer reads to surface the store's footprint (task #2087).
func (s *Store) Cells() int {
	n := 0
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.RLock()
		n += len(sh.e) + len(sh.dOut) + len(sh.dIn) + len(sh.t)
		sh.mu.RUnlock()
	}
	return n
}

// keysOf returns the keys of a label set as a slice (order unspecified).
func keysOf(m map[uint32]struct{}) []uint32 {
	if len(m) == 0 {
		return nil
	}
	out := make([]uint32, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// RecomputeReset clears every cell and every dirty flag, returning the store to
// its empty state. It is the seam an O(V+E) recompute-from-graph (task #2084)
// resets before replaying the create-deltas of every live edge; clearing the
// dirty sets restores full exactness. It is a mutation, and needs no caller
// serialisation for the order-insensitivity reason given on [Store.Apply].
func (s *Store) RecomputeReset() {
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.Lock()
		clear(sh.e)
		clear(sh.dOut)
		clear(sh.dIn)
		clear(sh.t)
		sh.mu.Unlock()
	}
	s.dmu.Lock()
	clear(s.dDirtyOut)
	clear(s.dDirtyIn)
	clear(s.tDirtyA)
	clear(s.tDirtyB)
	s.dmu.Unlock()
}
