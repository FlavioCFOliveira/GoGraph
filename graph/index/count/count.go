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
// Within a shard each family's cell map is IMMUTABLE ONCE PUBLISHED and swapped
// as a whole through an [atomic.Pointer] (copy-on-write). Only the map's
// STRUCTURE — which combinations exist — is versioned that way; a cell's VALUE
// lives behind the pointer the map holds and changes in place, so the ordinary
// increment never rebuilds a map. See [addCell] for why that split is what
// makes the store scale.
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
//   - [Store.CountE], [Store.CountD] and [Store.CountT] take NO LOCK AT ALL. They
//     load the shard's published cell map through an [atomic.Pointer] and read the
//     cell. The map they observe is immutable, so a concurrent structural change
//     cannot mutate it under them;
//   - an increment to an ALREADY-PRESENT cell runs under the shard's SHARED hold
//     and is a single [atomic.Int64.Add]. The shared hold is not protecting the
//     arithmetic — the arithmetic is already atomic. It is what stops a cell being
//     unlinked out from under an in-flight increment, which would silently discard
//     the delta. Since rmp #2696 that hold is taken on an [rbMutex], whose shared
//     side is striped per-P, rather than on a [sync.RWMutex] whose shared side was
//     one contended word;
//   - creating a cell, and deleting one that has returned to zero, take the shard's
//     EXCLUSIVE lock. Those are the only two operations that change a map's
//     structure, and their frequency is schema-cardinality-bounded rather than
//     data-size-bounded;
//   - the aggregate is ORDER-INSENSITIVE (rmp #2303): a cell is deleted at exactly
//     zero rather than at zero-or-below, so concurrent partial sums that transit a
//     negative value do not lose a decrement. That property is what replaced writer
//     exclusion, and [addCell] documents the failure it fixes.
//
// The store spawns no goroutines.
//
// # Why the WRITE path's shared hold is striped (rmp #2696)
//
// rmp #2682 removed the read lock and fixed the SPREAD case. It did not fix the
// single-hot-type case, which was unchanged at 0.319x from 1 to 8 goroutines, and
// the reason is that the remaining cost was never the counter.
//
// MEASURED at HEAD 42a27558, one hot relationship type at 8 goroutines:
// sync/atomic.(*Int32).Add was 48.85% of ALL CPU, and pprof -peek attributes
// 100% of it to [sync.RWMutex.RLock] (53.17%) and [sync.RWMutex.RUnlock]
// (46.83%). The count cell those calls exist to protect — an [atomic.Int64] —
// was 12.60%. The lock word was the contended object; the counter was not.
//
// That distinction decided the design, and it was settled by measurement rather
// than by argument. Two candidates were built and benchmarked against the same
// 90%-read/10%-write shape:
//
//   - a STRIPED COUNTER summed on read, in the shape of Java's LongAdder: each
//     cell becomes eight per-line counters, a write picks one, a read sums them.
//     It bought 1.227x, made an uncontended read 1.92x SLOWER (5.198ns against
//     2.702ns) and an uncontended write 3.4x slower, because the delete-on-zero
//     test must sum every stripe on every increment. It loses, and it loses
//     because it unshares the object that was not shared enough to matter;
//   - a STRIPED LOCK with the counter left alone — the shape adopted here. It
//     bought 2.018x on the same benchmark and left the read path exactly as it
//     was, O(1) and exact, at 0.994x.
//
// The generalisable lesson is that on this store the FREQUENT hold is the shared
// one and the RARE hold is the exclusive one, so the object worth striping is the
// LOCK. ClickHouse draws the same line explicitly between its two counter
// families: ProfileEvents is striped per-CPU and summed on read, while
// CurrentMetrics is a single unstriped atomic precisely because it must be
// exactly readable at a point in time (src/Common/CurrentMetrics.h). This store
// is the second kind — the planner reads an exact cardinality, and delete-on-zero
// needs an exact zero — so striping its counter was the wrong transplant.
//
// # The lock-free increment that does not work
//
// Recorded because it is the obvious next idea and it is WRONG. Replacing the
// shared hold with a dead-flag handshake — the writer adds and then checks a flag,
// the unlinker sets the flag and then re-reads the counter — appears sound and is
// not. After a SUCCESSFUL unlink the flag stays set, so a writer whose delta was
// already counted into the zero that justified the unlink also observes it, undoes
// its delta and re-applies it to the replacement cell. The delta lands twice and
// the aggregate drifts. MEASURED: with the unlink ablated the same increment is
// exact over 8x50000 oscillations through zero, and with the unlink restored the
// same test read 13 against an expected 24. Safe reclamation of a cell that
// lock-free writers may still hold a pointer to needs epochs or hazard pointers,
// which is a far larger change than this store's contention warrants.
//
// # Why the read path holds no lock (rmp #2682)
//
// It used to take the shard's read lock, and that single [sync.RWMutex] reader
// counter was measured to be the whole of the store's contention: with one hot
// relationship type — every cell of which lands on one shard, because
// [Store.eShardOf] keys on the relationship type alone — throughput FELL to 0.391x
// going from 1 to 8 goroutines on a 10-core host, and 98.15% of the module's mutex
// delay sat on this package's increment.
//
// That is the same failure rmp #2203 measured elsewhere in this module: a bare
// [sync.RWMutex] degrading 17.6x from 1 to 10 cores purely because of its one
// shared reader counter, the counter and not the code around it being the
// bottleneck. A reader-side RLock is two atomic read-modify-writes on ONE cache
// line shared by every core, so it serialises at the cache-coherence level however
// little work the critical section does. Copy-on-write removes the read-side
// read-modify-write entirely: a reader performs a plain atomic LOAD, which leaves
// the line in shared state on every core at once.
package count

import (
	"math/rand/v2"
	"runtime"
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

// cells is one family's published cell map. A value of this type is IMMUTABLE
// once it has been stored into a [table]: the only legal way to change which
// keys it holds is to build a fresh map and swap the whole thing, which is what
// [insertCell] and [removeCellIfZero] do under the shard's exclusive lock.
//
// The *values* are pointers, and the counters they address are mutated in place.
// That is the point of the split: an increment to an existing combination — the
// overwhelmingly common case, and the one that runs at |E| frequency — never
// touches the map at all, while a change of structure runs at schema-cardinality
// frequency and can afford to copy.
type cells[K comparable] map[K]*atomic.Int64

// table is one family's copy-on-write cell map inside a shard.
//
// It is a distinct type rather than a bare [atomic.Pointer] so the load helper
// can hide the double indirection ([atomic.Pointer] cannot hold a map directly,
// because a map is not a pointer type as far as the type system is concerned).
type table[K comparable] struct {
	p atomic.Pointer[cells[K]]
}

// load returns the currently published cell map. It is a plain atomic load: no
// read-modify-write, so the cache line stays shared across every reading core.
// The returned map must never be written to.
//
// It PANICS on a table that [table.init] has never been called on, because it
// dereferences the published pointer unconditionally. That is reachable only from
// a zero-value [Store], which [Store] already documents as unusable; [New]
// initialises every table of every shard.
func (t *table[K]) load() cells[K] { return *t.p.Load() }

// store publishes m as the family's cell map. The caller must hold the shard's
// exclusive lock, and must not retain any writable reference to m afterwards.
func (t *table[K]) store(m cells[K]) { t.p.Store(&m) }

// init publishes an empty map. It is called once per shard by [New] and again by
// [Store.RecomputeReset], which holds the exclusive lock.
func (t *table[K]) init() { t.store(make(cells[K])) }

// shard is one stripe of the cell maps.
//
// Its lock does NOT guard reads. It guards the two structural operations —
// creating a cell and deleting a cell that has reached zero — against the
// in-flight increments that could otherwise be discarded by them:
//
//   - an increment to an existing cell holds mu SHARED. Several increments to the
//     same or different cells therefore proceed in parallel, each finishing with a
//     single [atomic.Int64.Add];
//   - a structural change holds mu EXCLUSIVE, which by construction cannot overlap
//     any increment. That is what lets [removeCellIfZero] re-read a counter and
//     trust the value it sees;
//   - a read takes mu not at all. See the package documentation.
//
// # Layout
//
// The four published-map pointers are read by every [Store.CountE] and written
// only by a structural change, so they are read-mostly and share a line happily.
// They are padded away from the lock deliberately: when the lock word sat in the
// same line, every shared acquire's read-modify-write invalidated the line that
// every concurrent reader was loading the map pointer from. That is false sharing
// between the lock and the data it does not even protect, and it was measured —
// moving the lock to its own line, changing nothing else, was worth 1.22x on the
// hot mixed workload on its own.
type shard struct {
	e    table[uint32]
	dOut table[uint64]
	dIn  table[uint64]
	t    table[triKey]
	_    [cacheLine - 4*8]byte

	mu rbMutex
}

// cacheLine is the padding target. 128 rather than 64 because Go's own runtime
// uses 128 for arm64 — internal/cpu.CacheLinePadSize, whose comment names Apple
// silicon as the reason — and because macOS reports hw.cachelinesize = 128 on
// Apple silicon. It is also a multiple of the 64-byte line x86 and Neoverse use,
// so one padded slot never straddles two lines there either.
//
// The literature does not agree on this number and the disagreement is worth
// recording: ClickHouse pads AArch64 to 64 (src/Common/CacheLine.h) because it
// targets Neoverse and says so explicitly; LLVM reports 256 for AArch64. On this
// host the CPU's own L1 line is reported as 64 by CTR_EL0 on Linux while macOS
// reports 128 for the system-level granule (Wrenger et al., Journal of Systems
// Architecture, 2024, measured on M1 Ultra). 128 is the conservative choice and
// the one Go itself makes.
const cacheLine = 128

// rbSlot is one core's share of a shard's shared-hold counter. It owns a whole
// cache line so that two cores taking the shared hold never touch the same one.
type rbSlot struct {
	n atomic.Int32
	_ [cacheLine - 4]byte
}

// rbToken carries a goroutine's preferred slot between operations. It lives in a
// [sync.Pool], whose per-P private slot is what gives the affinity: a goroutine
// running on one P tends to draw back the same token and therefore the same
// slot. That is the closest an ordinary package can get to a per-P counter —
// runtime.procPin is not exported, and reaching it needs //go:linkname, which
// needs unsafe.
//
// The affinity is a best-effort hint and never a correctness input: a token that
// migrates, or is dropped by a GC and redrawn, costs at most one extra slot
// collision. Measured against drawing a slot at random on every call, the token
// was worth 4% (1.937x -> 2.018x on the hot mixed benchmark), which is above
// this harness's measured 1.6% noise floor.
type rbToken struct {
	idx int
	_   [cacheLine - 8]byte
}

// rbMutex is a reader-writer lock whose SHARED side scales with the cores.
//
// # Why not sync.RWMutex
//
// On this store the FREQUENT hold is the shared one — every increment takes it —
// and the rare hold is the exclusive one, taken only when a cell is created or
// deleted, which is schema-cardinality-bounded. A [sync.RWMutex]'s shared acquire
// is two read-modify-writes on ONE int32 that every core must own exclusively in
// turn, so it serialises at the cache-coherence level however short the critical
// section is. MEASURED at HEAD: sync/atomic.(*Int32).Add was 48.85% of all CPU in
// the one-hot-type workload at 8 goroutines, and 100% of it came from RWMutex's
// RLock and RUnlock — against 12.60% for the count cell those calls protect. The
// lock word, not the counter, was the contended object.
//
// # The structure
//
// It is the readers-biased (big-reader) shape: a reader registers in a per-P slot
// on its own cache line, and a writer announces itself and then waits for every
// slot to drain. Prior art: Linux's brlock, and puzpuzpuz/xsync's RBMutex, which
// selects its slot the same way, from a token held in a sync.Pool.
//
// # Why it is correct
//
// A reader increments its slot and then re-reads the writer flag; a writer sets
// the flag and then reads the slots. Go's memory model specifies that sync/atomic
// behaves as sequentially consistent, so in the single total order at least one of
// the pair observes the other: either the writer sees the reader's registration
// and waits, or the reader sees the flag, stands down, and takes the fallback
// [sync.RWMutex] — which the writer also holds exclusively. The fallback is what
// makes the losing side SAFE rather than merely unlikely, and it is also what
// keeps a writer from starving, since Go's RWMutex blocks readers arriving behind
// a waiting writer.
//
// The zero value is not usable; construct with [newRBMutex].
type rbMutex struct {
	// pending is read by every shared acquire and written only by a writer, so
	// it is a read-mostly line that stays Shared on every core between the rare
	// structural changes.
	pending atomic.Bool
	_       [cacheLine - 1]byte

	slots  []rbSlot
	tokens *sync.Pool

	// fallback carries any shared hold that raced a writer, and the writer's own
	// exclusive hold. w serialises writers so only one drains the slots at a time.
	fallback sync.RWMutex
	w        sync.Mutex

	// tail rounds rbMutex to a whole number of cache lines so that [shard],
	// which embeds it after its own line of map pointers, keeps a stride that is
	// a multiple of the line size. Without it the stride was 328 bytes and
	// consecutive shards' hot fields drifted across line boundaries.
	//
	// The terms are pending plus its pad (128), the slots slice header (24), the
	// pool pointer (8), fallback (24) and w (16) — 200, rounded up to 256. It is
	// a literal restatement of those widths and CANNOT notice a field being
	// added, exactly as the shard padding it replaces could not;
	// TestShard_LayoutSeparatesReadersFromTheLock is the guard that can, and the
	// two must be kept in step by hand.
	_ [56]byte
}

// newRBMutex returns a lock with n per-P slots drawing tokens from pool.
func newRBMutex(n int, pool *sync.Pool) rbMutex {
	return rbMutex{slots: make([]rbSlot, n), tokens: pool}
}

// rlock takes the shared hold and returns the slot [rbMutex.runlock] must
// release, or -1 when the caller fell back to the plain reader lock.
func (m *rbMutex) rlock() int {
	if m.pending.Load() {
		m.fallback.RLock()
		return -1
	}
	t, _ := m.tokens.Get().(*rbToken)
	if t == nil {
		t = new(rbToken)
	}
	if t.idx == 0 {
		// Slots are numbered from 1 in the token so that a zero value means
		// "never assigned" rather than "slot 0".
		t.idx = 1 + int(rand.UintN(uint(len(m.slots)))) //nolint:gosec // G115: len(slots) >= 1 by construction.
	}
	slot := (t.idx - 1) % len(m.slots)
	m.tokens.Put(t)

	m.slots[slot].n.Add(1)
	if m.pending.Load() {
		// A writer announced itself between the two checks. Stand down and take
		// the fallback, which the writer does hold against.
		m.slots[slot].n.Add(-1)
		m.fallback.RLock()
		return -1
	}
	return slot
}

// runlock releases the hold rlock returned.
func (m *rbMutex) runlock(slot int) {
	if slot < 0 {
		m.fallback.RUnlock()
		return
	}
	m.slots[slot].n.Add(-1)
}

// lock takes the exclusive hold, excluding every shared holder on either path.
func (m *rbMutex) lock() {
	m.w.Lock()
	m.pending.Store(true)
	for i := range m.slots {
		for m.slots[i].n.Load() != 0 {
			runtime.Gosched()
		}
	}
	m.fallback.Lock()
}

// unlock releases the exclusive hold.
func (m *rbMutex) unlock() {
	m.fallback.Unlock()
	m.pending.Store(false)
	m.w.Unlock()
}

// Store is the sharded relationship count-store. Its zero value is not usable;
// construct one with [New].
//
// A Store is SAFE FOR CONCURRENT USE by any number of goroutines, in every
// combination of its operations: [Store.CountE], [Store.CountD], [Store.CountT],
// [Store.DDirty] and [Store.TDirty] are concurrent reads; [Store.Apply],
// [Store.MarkDirty] and [Store.RecomputeReset] are concurrent mutations and need
// no serialisation from the caller; [Store.Snapshot] and [Store.Cells] are
// observability reads safe to take against a live workload. The package
// documentation gives the structural reason each of those holds.
type Store struct {
	shards [numShards]shard

	// tokens hands out the per-P slot hints every shard's shared hold uses. It
	// is per-Store rather than package-level: a package-level pool would be
	// hidden global mutable state, which this module forbids, and a Store-scoped
	// one costs only the pool's own per-P array.
	tokens sync.Pool

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
	s.tokens.New = func() any { return new(rbToken) }
	// One slot per schedulable core. GOMAXPROCS is read once here rather than
	// per operation: it is dynamic in go1.27, but the slot array only needs to
	// be wide enough to spread the cores that actually run concurrently, and a
	// stale width costs at most an extra collision, never correctness.
	//
	// This sizing is what the lock costs in memory, and the cost is large:
	// MEASURED, an empty Store went from 24,000 to 123,712 bytes, +4.15x, of
	// which 64 shards x GOMAXPROCS slots x 128 bytes is the dominant term. On a
	// host with far more cores it grows linearly with them.
	//
	// Four slots measured indistinguishably from ten on this 10-core host
	// (0.718x against 0.729x scaling at 8 goroutines, inside the harness's
	// measured noise), so the array is over-provisioned HERE. It is nevertheless
	// sized by GOMAXPROCS rather than capped at a literal, because the number a
	// cap should take is unknown without measuring a many-core host, and the
	// module's mandate is to scale with the hardware it is given. Allocating a
	// shard's slots lazily, on its first contended acquire, would remove the
	// waste without the guess; that is left for a task that can measure it.
	slots := runtime.GOMAXPROCS(0)
	if slots < 1 {
		slots = 1
	}
	for i := range s.shards {
		sh := &s.shards[i]
		sh.e.init()
		sh.dOut.init()
		sh.dIn.init()
		sh.t.init()
		sh.mu = newRBMutex(slots, &s.tokens)
	}
	return s
}

// MaxRecountEdges reports the per-relabel OUT-side recount ceiling (0 or less
// means unbounded). The relabel maintenance consults it to decide between an
// exact OUT-side recount and an X-scoped OUT dirty marking (design §3.3.1).
func (s *Store) MaxRecountEdges() int { return s.budget }

// mix32 is a cheap integer bit-finaliser (a multiply then an xorshift) that spreads the
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
// through [addCell], whose aggregate is ORDER-INSENSITIVE — a cell is deleted at
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
		sh := s.eShardOf(d.RT)
		addCell(sh, &sh.e, d.RT, d.Delta)
	case KindD:
		k := dkey(d.A, d.RT)
		sh := s.dShardOf(k)
		if d.Dir == In {
			addCell(sh, &sh.dIn, k, d.Delta)
		} else {
			addCell(sh, &sh.dOut, k, d.Delta)
		}
	case KindT:
		tk := triKey{a: d.A, rt: d.RT, b: d.B}
		sh := s.tShardOf(tk)
		addCell(sh, &sh.t, tk, d.Delta)
	}
}

// addCell is the shared insert-add-delete-on-zero routine, in three tiers of
// decreasing frequency and increasing cost.
//
// # Tier 1 — the cell exists and stays non-zero (the |E|-frequency case)
//
// Shared lock, one map lookup, one [atomic.Int64.Add]. Concurrent increments —
// to this cell or any other in the shard — run in parallel; nothing here is
// serialised but the atomic itself.
//
// The shared lock IS load-bearing, and it is the only thing standing between this
// routine and a silently lost delta. Without it the sequence is: writer A loads the
// published map and takes the pointer to cell C; writer B drives C to zero and
// unlinks it; A adds its delta to C, which is now an orphan no reader can reach.
// The delta is gone. Holding mu shared across BOTH the lookup and the Add makes
// that impossible, because the unlink needs the exclusive lock and therefore cannot
// run while any increment is in flight.
//
// # Tier 2 — the counter reaches exactly zero
//
// The shared lock is dropped and the exclusive lock taken, and the counter is
// RE-READ before anything is unlinked. It must be, because the value can have
// moved between the two holds: another writer's increment may have taken the cell
// off zero, in which case the cell must stay. Under the exclusive lock no
// increment can be in flight, so the re-read is stable and the decision it drives
// is final.
//
// Two writers can both observe zero and both arrive here. The first unlinks; the
// second finds the key gone and does nothing. Neither outcome depends on which
// arrives first, which is the ordering property this store rests on.
//
// # Tier 3 — the cell does not exist
//
// The exclusive lock is taken and the key looked up AGAIN, because another writer
// may have created it while this one was waiting; only if it is still absent is a
// fresh map published with the new cell in it.
//
// # Deleted at EXACTLY zero, not at zero-or-below (rmp #2303, MVCC B1)
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
func addCell[K comparable](sh *shard, tab *table[K], k K, delta int64) {
	slot := sh.mu.rlock()
	if cell := tab.load()[k]; cell != nil {
		if cell.Add(delta) != 0 {
			sh.mu.runlock(slot) // Tier 1.
			return
		}
		sh.mu.runlock(slot)
		removeCellIfZero(sh, tab, k) // Tier 2.
		return
	}
	sh.mu.runlock(slot)
	insertCell(sh, tab, k, delta) // Tier 3.
}

// removeCellIfZero unlinks k from the family's map if — and only if — its
// counter is still EXACTLY zero once the exclusive lock is held. See tier 2 of
// [addCell] for why the re-read is mandatory and why a concurrent second caller
// is harmless.
func removeCellIfZero[K comparable](sh *shard, tab *table[K], k K) {
	sh.mu.lock()
	defer sh.mu.unlock()

	old := tab.load()
	cell := old[k]
	if cell == nil || cell.Load() != 0 {
		// Already unlinked by the other writer that saw the same zero, or taken
		// off zero by an increment that landed between the two lock holds. A
		// non-zero cell — negative included — is retained.
		return
	}
	next := make(cells[K], len(old))
	for kk, vv := range old {
		if kk != k {
			next[kk] = vv
		}
	}
	tab.store(next)
}

// insertCell creates k's cell and applies delta to it. It re-checks under the
// exclusive lock, because another writer may have created the cell while this one
// waited for the lock; see tier 3 of [addCell].
func insertCell[K comparable](sh *shard, tab *table[K], k K, delta int64) {
	sh.mu.lock()
	defer sh.mu.unlock()

	old := tab.load()
	if cell := old[k]; cell != nil {
		// Created while we waited. Add here rather than restarting: we hold the
		// exclusive lock, so this is the strictly stronger of the two paths.
		if cell.Add(delta) == 0 {
			next := make(cells[K], len(old))
			for kk, vv := range old {
				if kk != k {
					next[kk] = vv
				}
			}
			tab.store(next)
		}
		return
	}
	cell := new(atomic.Int64)
	if cell.Add(delta) == 0 {
		// Unreachable from Apply, which rejects a zero delta before reaching here.
		// Publishing nothing is nevertheless the correct answer: an absent key
		// reads as zero, which is the value the cell would have held.
		return
	}
	next := make(cells[K], len(old)+1)
	for kk, vv := range old {
		next[kk] = vv
	}
	next[k] = cell
	tab.store(next)
}

// CountE returns the live edge count of relationship type rt (0 when absent).
//
// It takes NO LOCK: the published cell map is immutable, so loading it is a plain
// atomic load and reading it is an ordinary map lookup. See the package
// documentation for the measurement that removed the read lock (rmp #2682).
//
// A cell a concurrent writer unlinks between the map load and the counter read
// still reads as the value it held, which is the value that made it eligible for
// unlinking: exactly zero. Either answer is a legal snapshot read, and this one
// cannot be wrong.
func (s *Store) CountE(rt uint32) int64 {
	cell := s.eShardOf(rt).e.load()[rt]
	if cell == nil {
		return 0
	}
	return cell.Load()
}

// CountD returns the degree-sum D(label, rt, dir) (0 when absent). It ignores
// the dirty flag; callers that need the exactness verdict consult [Store.DDirty].
//
// It takes no lock, for the reason given on [Store.CountE].
func (s *Store) CountD(label, rt uint32, dir Direction) int64 {
	k := dkey(label, rt)
	sh := s.dShardOf(k)
	var cell *atomic.Int64
	if dir == In {
		cell = sh.dIn.load()[k]
	} else {
		cell = sh.dOut.load()[k]
	}
	if cell == nil {
		return 0
	}
	return cell.Load()
}

// CountT returns the triple count T(a, rt, b) (0 when absent). It ignores the
// dirty flag; callers that need the exactness verdict consult [Store.TDirty].
//
// It takes no lock, for the reason given on [Store.CountE].
func (s *Store) CountT(a, rt, b uint32) int64 {
	tk := triKey{a: a, rt: rt, b: b}
	cell := s.tShardOf(tk).t.load()[tk]
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

// Snapshot returns a copy of every cell whose counter is currently NON-ZERO, and
// every dirty marking. It is safe to call concurrently with writers, which are NOT
// serialised against each other.
//
// It takes each shard's EXCLUSIVE lock, which is what freezes that shard for the
// duration of its scan: increments hold the SHARED lock (see [addCell]), so a
// shared hold here would no longer exclude them and the per-shard scan would no
// longer be atomic. The exclusive hold restores exactly the property the read hold
// used to give when increments were exclusive. It is not on any request path — the
// engine calls it for observability and the simulator for parity checking — so the
// cost of blocking a shard's writers for the length of one shard's scan is paid by
// the observer, never by the workload.
//
// NEGATIVE cells are included. This doc used to say "every live cell (value > 0)",
// which the code has never done: the predicate is `v != 0`, and it must be, because
// [addCell] deliberately RETAINS a cell driven negative rather than clamping it —
// that retention is what makes the aggregate order-insensitive (rmp #2303). A
// negative cell is reachable from ordinary Cypher, not only from concurrent
// writers: MEASURED, `SET a:X` then `SET b:X` then `REMOVE a:X` over an edge
// `a -> b` leaves T(X, rel, X) at -1, because the +1 was never applied (b had no
// out-edge, so the relabel's OUT recount returned early) while the -1 was, b having
// acquired X by then. Such a cell is always covered by the [DirtyMark] the same
// relabel raised, so it is non-exact rather than wrong; a consumer that treats an
// absent key as zero and assumes every present value is positive will nonetheless
// mis-read it.
func (s *Store) Snapshot() Snapshot {
	snap := Snapshot{
		E:    make(map[uint32]int64),
		DOut: make(map[uint64]int64),
		DIn:  make(map[uint64]int64),
		T:    make(map[[3]uint32]int64),
	}
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.lock()
		for rt, c := range sh.e.load() {
			if v := c.Load(); v != 0 {
				snap.E[rt] = v
			}
		}
		for k, c := range sh.dOut.load() {
			if v := c.Load(); v != 0 {
				snap.DOut[k] = v
			}
		}
		for k, c := range sh.dIn.load() {
			if v := c.Load(); v != 0 {
				snap.DIn[k] = v
			}
		}
		for k, c := range sh.t.load() {
			if v := c.Load(); v != 0 {
				snap.T[[3]uint32{k.a, k.rt, k.b}] = v
			}
		}
		sh.mu.unlock()
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
// is deleted the moment its counter returns to zero ([addCell]), every map
// entry is a live combination, so this is an exact, allocation-free size
// indicator for observability: it is bounded by the number of currently-observed
// schema combinations (design §2.3), never by |V| or |E|.
//
// "The moment" is now a two-step moment, and this is the one place it shows. The
// writer whose increment lands on zero drops the shared lock and re-takes the
// exclusive one to unlink the key, so a Cells call racing that writer can count a
// cell that is one instruction from removal. The over-count is bounded by the
// number of writers concurrently crossing zero, it is transient — the crossing
// writer always completes the unlink, and at quiescence no zero-valued cell
// survives — and it errs HIGH, so it can never hide a footprint the bound exists
// to catch. Every quiescent reading is exact, which is what the simulator's
// cells-bound invariant asserts against.
//
// It takes each shard's EXCLUSIVE lock, for the reason given on [Store.Snapshot],
// and is safe to call concurrently with writers. The metrics [Backend] exposes no
// gauge, so this is the accessor an observer reads to surface the store's
// footprint (task #2087).
func (s *Store) Cells() int {
	n := 0
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.lock()
		n += len(sh.e.load()) + len(sh.dOut.load()) + len(sh.dIn.load()) + len(sh.t.load())
		sh.mu.unlock()
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
//
// It publishes a FRESH empty map per family rather than clearing the published
// one, because the published map is immutable: a concurrent lock-free reader may
// be walking it, and clearing it under that reader would be a data race as well
// as a torn answer.
func (s *Store) RecomputeReset() {
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.lock()
		sh.e.init()
		sh.dOut.init()
		sh.dIn.init()
		sh.t.init()
		sh.mu.unlock()
	}
	s.dmu.Lock()
	clear(s.dDirtyOut)
	clear(s.dDirtyIn)
	clear(s.tDirtyA)
	clear(s.tDirtyB)
	s.dmu.Unlock()
}
