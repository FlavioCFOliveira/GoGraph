package exec

import (
	"context"
	"time"
)

// Profiler captures per-operator measurements for one query execution.
//
// # Cost when off
//
// A Profiler is opt-in and there is exactly one place that installs it: the
// recursive plan builder wraps each operator it returns, and only when a
// Profiler is present. With none, no wrapper is built and no operator executes
// any instrumentation — the normal path runs byte-identical code to a build that
// never had profiling. That is the acceptance condition (rmp #2222 AC 3) and the
// reason the counters live in a wrapper rather than in the operators: an
// `if p != nil` inside 55 Next implementations would be a cost on every row of
// every query forever.
//
// # Why the wrapper must be transparent
//
// The builder wraps on the way OUT of its recursion, so a parent is constructed
// with its child ALREADY wrapped and runs its capability type-assertions against
// the wrapper. A wrapper that hid [ChunkProducer] would make the parent build a
// row-mode operator instead of a columnar one, so profiling would change the very
// plan it exists to observe. Wrap therefore returns a variant matching what the
// wrapped operator exposes:
//
//   - plain — [Operator] only
//   - chunk — also [ChunkProducer]
//   - nodeID — also [NodeIDColumnProducer]
//
// All variants forward rowCountHint, which is likewise asserted on children (to
// bound allocation) and whose contract already covers "no bound known".
// TestProfile_PlanShapeIsIdenticalProfiledOrNot is the gate on that
// transparency: it compares the rendered tree with and without a Profiler.
//
// # Why this is not cypher/explain.ProfiledOperator
//
// The cypher/explain package already had a ProfiledOperator recording rows and
// elapsed time (plus a DbHits counter), written before any engine surface existed
// to wire it to. It could not be the wiring, for a reason that is structural
// rather than stylistic: transparency requires the wrapper to re-implement
// NodeIDColumnProducer, whose identifying method nodeIDColumnProducer() is
// UNEXPORTED to this package. A wrapper declared anywhere else cannot satisfy that
// interface, so wrapping from cypher/explain would strip the marker and silently
// downgrade a columnar plan to row mode.
//
// Every measurement therefore lives here, db-hits included: rmp #2238 folded them
// into this Profiler by deriving the count at the wrapper from the rows a marked
// operator emits ([StorageRecordScan]), rather than threading a counter through the
// storage accessors, which is what keeps the cost-when-off guarantee absolute.
// cypher/explain's own DbHitsCounter and InstrumentedScan are superseded by that
// and have no caller.
//
// That derivation is a MODEL, and rmp #2720 measured where it breaks: it holds for
// the scan and seek leaves, and fails for a traversal and for a filtered expand.
// [StorageRecordScan] enumerates every case and says what was done about each; an
// operator that can report its own count implements [storageAccessCounter] and its
// figure is measured rather than inferred.
//
// What cypher/explain still contributes is PRESENTATION. Its FormatReport renders
// a plan and its measurements as a fixed-width columnar table where this package's
// [RenderPlanNode] renders an indented tree, and rmp #2701 wired it to
// cypher.Engine.ProfileTable, which flattens the [PlanNode] tree this Profiler
// produced into that table. Both renderings describe one run of one plan.
//
// # Concurrency
//
// A Profiler carries no mutable state: Wrap allocates a wrapper, returns it and
// retains nothing, so Wrap itself is safe to call from any goroutine. The
// WRAPPERS are the part that is not: each accumulates its row count and elapsed
// time with plain non-atomic adds, so every wrapper must be driven by exactly one
// goroutine — which is what a single-goroutine Volcano pipeline gives it.
//
// # The parallel tier is measured as ONE node, by construction
//
// A morsel-parallel leaf ([ParallelScanProject], [ParallelAggregateScan],
// [ParallelCountScan]) builds and drives a private sub-plan per morsel on a
// worker goroutine. Those sub-plans are neither instrumented nor rendered, and
// both halves are ENFORCED rather than assumed:
//
//   - the builder clears the profiler from the per-worker build options
//     (cypher's buildOpts.forWorker), so no worker allocates a wrapper or times a
//     row; and
//   - a morsel-parallel leaf implements no [PlanChildren], so [PlanTree] stops at
//     it and a measurement taken below it would be unreachable anyway.
//
// The leaf therefore reports the rows it emitted and the time its own Next and
// FillChunk calls took on the driving goroutine: the whole parallel phase
// attributed to one node. That is a deliberate contract, not a limitation of the
// wrapper. Showing the inside would mean rendering one sub-tree per morsel, or
// merging N morsel sub-trees into one synthetic tree; both change what PROFILE
// reports, so neither is done here.
//
// Its ROWS, TIME and — since rmp #2762 — DB-HITS are all real measurements of the
// whole phase. Each leaf implements [storageAccessCounter] and reports the node
// references its workers consumed, so the identical query now reports the SAME
// figure on either side of the parallel threshold: a scan of 2000 :B nodes reports
// 2000 whether it planned a [NodeByLabelScan] or a [ParallelScanProject]. Before
// #2762 it reported N below the threshold and "not counted" above it, according to
// a threshold the reader did not set; before rmp #2760 the cell simply read 0,
// indistinguishable from an operator that read nothing
// (docs/explain-profile-honesty-audit-2026-09-03.md §3 refutation 3).
//
// The counting obeys the same one-node contract. A worker accumulates in a LOCAL
// and folds into the leaf's counter with one atomic add per MORSEL — never per
// node: a per-record atomic on a morsel-parallel scan is a contention defect
// against CLAUDE.md's mandate 3, and this very tier measured that shape at 18.9%
// of flat CPU with scaling that stopped at four workers (rmp #2649). Each leaf's
// storageAccesses godoc states what its figure counts, and that a node reference
// is charged ONCE even though Init's walk and the morsel scan both touch it —
// charging both would report 2N for work the serial plan reports as N.
//
// # One stated limit: a parent that never pulls a row
//
// cypher's profileMaterialised captures the tree after the drain and BEFORE any
// Close, on the reasoning that materialisation has driven every operator to
// exhaustion. A morsel-parallel leaf is joined by its own first Next (wg.Wait), so
// that reasoning holds for every plan whose parent pulls at least one row. It does
// NOT hold when the parent pulls none: `RETURN ... LIMIT 0` builds the leaf,
// Init launches its workers, [Limit] then returns false without ever calling the
// leaf's Next, and the capture happens with the workers still in flight. The
// figure is then whatever they had completed at that instant. MEASURED over 40
// consecutive PROFILEs of `MATCH (n:B) RETURN n.v LIMIT 0` on 20 000 nodes: 0
// every time, alongside rows=0 and time=0s — the workers do not complete a morsel
// in the microseconds before the capture — but 0 is what was observed, not what is
// guaranteed. This is a property of WHEN the tree is captured, shared with the rows
// and time columns, and not of the counter; it is stated here rather than left for
// a reader of an unexpected figure to discover.
//
// # Why not PostgreSQL's per-worker breakdown
//
// PostgreSQL takes the other route, and GoGraph deliberately does not follow it.
// Under ANALYZE it keeps a per-worker Instrumentation array (execParallel.c:1275,
// :1297 — each worker calls InstrEndLoop then InstrAggNode into its own slot;
// :1039-1043 — the leader folds them into the node's own figure and :1057-1058
// keeps the per-worker copies), and under ANALYZE plus VERBOSE explain.c:1893-1941
// emits a separate "Worker N:" sub-entry per worker (explain.c:4544-4551,
// flushed by ExplainFlushWorkersState at :4595-4614). Its headline figure is then
// a PER-WORKER AVERAGE rather than a total: explain.c:1841-1847 computes
// `rows = instrument->ntuples / nloops`, and instrument.c:184-186 has summed BOTH
// ntuples and nloops across participants, so a `Parallel Seq Scan` printing
// `rows=2000 loops=15` scanned 30 000 (regress/expected/select_parallel.out:589-620;
// 15 = 3 rescans x (4 workers + leader)). Read at REL_17_STABLE, commit
// 018bfcfd9fa4e520970ba3bda370f78bb473c365.
//
// Neither half fits here. The per-worker sub-entries have nothing to report: the
// builder clears the profiler from the per-worker build options, so no worker
// measures anything, and reinstating one would reintroduce the shared-wrapper data
// race rmp #2664 removed. And an averaged headline would defeat the point of this
// task — the figure exists so a reader can compare the parallel plan with the
// serial one, and a per-worker average is not comparable with anything. Neo4j
// 5.26.16 (commit 679feffbfb7a9189aba360ea98eef7fc3371e275) sums instead, which is
// the model followed here: ProfilingTracerData.java:33-41 accumulates `dbHits +=`
// per operator id, PlanDescriptionBuilder.scala:123-141 attaches exactly one
// DbHits argument per plan node, and ProfileDbHitsTestBase.scala:172-178 asserts a
// parallel all-nodes scan reports one total equal to the node count. (That clone
// is community-only, so the enterprise parallel runtime's own accumulator was not
// read; the conclusion rests on the QueryProfile contract, the plan-description
// builder and the runtime spec-suite assertion, all of which are in it.)
//
// Sharing one Profiler between two concurrently executing queries is meaningless
// rather than unsafe — the measurements would belong to two unrelated trees — so
// Engine.Profile allocates one per call.
type Profiler struct {
	// The type deliberately carries NO state. It is the marker that instrumentation
	// is on, and nothing more: Wrap returns the wrapper it builds, and the wrapper's
	// lifetime is already tied to the operator tree that [PlanTree] walks, so
	// retaining a second reference here would keep every wrapper (and, from a
	// morsel-parallel build, every per-morsel sub-plan) alive for no reader's
	// benefit. A field that only ever gets appended to is not a design, it is a leak
	// with a race attached (rmp #2664).
}

// NewProfiler returns a Profiler ready to instrument one query build.
func NewProfiler() *Profiler { return &Profiler{} }

// Wrap returns op instrumented to record the rows it emits and the time its own
// Next/FillChunk calls take, preserving every capability op exposes. It returns
// op unchanged when op is nil or already wrapped, so a double-wrap cannot
// double-count.
func (p *Profiler) Wrap(op Operator) Operator {
	if p == nil || op == nil {
		return op
	}
	if _, already := op.(profiledNode); already {
		return op
	}
	if cp, columnar := op.(ChunkProducer); columnar {
		return p.wrapChunkProducer(cp)
	}
	return &profiledOp{inner: op}
}

// WrapChunk is [Profiler.Wrap] for a caller that holds a [ChunkProducer] and needs
// one back.
//
// It exists so that a plan-shape recogniser which SUBSTITUTES a columnar operator
// can re-instrument it without a type assertion. Wrap returns an [Operator], and a
// caller feeding [NewColumnarFilter] would have to assert its result back to
// ChunkProducer — an assertion that cannot fail, but that the compiler forces the
// caller to handle anyway, so the code grows an unreachable branch whose behaviour
// nobody can test. Returning the interface the caller needs removes the branch
// instead of documenting it.
//
// Like Wrap it returns cp unchanged when the profiler is nil, when cp is nil, or
// when cp is already wrapped.
func (p *Profiler) WrapChunk(cp ChunkProducer) ChunkProducer {
	if p == nil || cp == nil {
		return cp
	}
	if _, already := cp.(profiledNode); already {
		return cp
	}
	return p.wrapChunkProducer(cp)
}

// wrapChunkProducer builds the wrapper variant matching what cp exposes. The two
// return types are the only variants that satisfy [ChunkProducer], which the
// compile-time assertions at the foot of this file enforce, so the result is a
// ChunkProducer by construction rather than by assertion.
func (p *Profiler) wrapChunkProducer(cp ChunkProducer) ChunkProducer {
	base := profiledChunkOp{profiledOp: profiledOp{inner: cp}, chunk: cp}
	if _, nodeIDs := cp.(NodeIDColumnProducer); nodeIDs {
		return &profiledNodeIDOp{base}
	}
	return &base
}

// UnwrapProfiled returns the operator op measures when op is a profiling wrapper,
// and op itself otherwise. It sees through exactly one wrapper, which is all there
// ever is: [Profiler.Wrap] returns op unchanged when op is already wrapped.
//
// # Why the builder needs this
//
// [Profiler.Wrap] preserves every INTERFACE an operator exposes, which is what
// keeps a capability type-assertion — `child.(ChunkProducer)` — answering the same
// under PROFILE as without it. It cannot preserve a CONCRETE type: no wrapper is
// an *Expand. A plan-shape recogniser that asserts on a concrete operator type
// therefore stops recognising its own shape the moment a Profiler is installed,
// and PROFILE renders a plan the user never runs (rmp #2665).
//
// A recogniser in that position asks for the operator itself here, builds what it
// meant to build, and puts the result back through the profiler
// ([Profiler.Wrap] or [Profiler.WrapChunk]) so the node it substituted is still
// measured. The wrapper it discards was allocated by the build and never driven,
// so no measurement is lost with it.
//
// Reaching for this to escape a CAPABILITY assertion would be a defect: those the
// wrapper already satisfies, and unwrapping one would silently drop the node from
// the profile. Use it only where a concrete type is genuinely required.
func UnwrapProfiled(op Operator) Operator {
	if p, ok := op.(profiledNode); ok {
		return p.planUnwrap()
	}
	return op
}

// profiledNode is the behaviour [PlanTree] needs from any wrapper variant: the
// operator it hides, and what that operator did.
type profiledNode interface {
	Operator
	// planUnwrap returns the measured operator, so a node is named after the
	// operator that ran rather than after the wrapper.
	planUnwrap() Operator
	// planStats returns the rows emitted, the time attributed to the operator, the
	// logical storage accesses attributed to it, and whether that last figure is
	// KNOWN. A false there means nothing counted this operator's accesses; the
	// accompanying int64 is then meaningless and must not be rendered as a zero
	// (rmp #2760).
	planStats() (int64, time.Duration, int64, bool)
}

// profiledOp measures one operator: the rows it emits and the wall-clock time
// spent inside its own Next.
//
// The time is inclusive: a pipelined operator's Next pulls from its children, so
// their cost is inside its measurement. The children are wrapped too, so a
// reader obtains an operator's exclusive cost by subtracting its children's —
// the same arithmetic a reader of Neo4j's PROFILE performs, and the reason each
// node reports its own total rather than a pre-computed exclusive figure that
// would hide the nesting.
type profiledOp struct {
	inner   Operator
	elapsed time.Duration
	rows    int64
}

// Init delegates. It is not timed: it runs once per query, and folding it into a
// per-row measurement would distort what the profile is for.
func (p *profiledOp) Init(ctx context.Context) error { return p.inner.Init(ctx) }

// Next delegates, counting the row and accumulating the elapsed time.
func (p *profiledOp) Next(out *Row) (bool, error) {
	start := time.Now()
	ok, err := p.inner.Next(out)
	p.elapsed += time.Since(start)
	if ok {
		p.rows++
	}
	return ok, err
}

// Close delegates to the wrapped operator.
func (p *profiledOp) Close() error { return p.inner.Close() }

// PlanChildren reports the wrapped operator's inputs, so a profiled tree walks
// exactly like an unprofiled one. Those inputs are themselves wrappers, which is
// what lets [PlanTree] attribute measurements at every level.
func (p *profiledOp) PlanChildren() []Operator {
	if kids, ok := p.inner.(PlanChildren); ok {
		return kids.PlanChildren()
	}
	return nil
}

// rowCountHint forwards the wrapped operator's upper-bound hint, reporting
// ok=false when it has none — which is exactly the interface's own contract for
// "no sound upper bound is known". Forwarding unconditionally keeps the hint
// reachable through the wrapper, so an allocation bound derived from it is not
// silently lost to profiling.
func (p *profiledOp) rowCountHint() (int, bool) {
	if h, ok := p.inner.(rowCountHinter); ok {
		return h.rowCountHint()
	}
	return 0, false
}

func (p *profiledOp) planUnwrap() Operator { return p.inner }

// planStats reports the measured rows and time, plus the db-hits and whether
// that last figure is a figure AT ALL.
//
// Db-hits come from one of three places, in this order:
//
//   - a MEASURED count, when the operator implements [storageAccessCounter] and
//     can report the records it actually read at no cost to a non-PROFILE run;
//   - a count DERIVED from the emitted rows, when the operator implements
//     [StorageRecordScan] — a marker that asserts one record read per row emitted;
//   - a KNOWN ZERO, when the operator implements [noStorageAccess] — a marker that
//     asserts the operator opens no access path at all.
//
// Every other operator reports known=false, which is the honest report of a
// figure nobody counted. It is NOT rendered as 0: rmp #2760 separated the two
// because a column that prints 0 for both cannot be read, and every renderer
// carries the flag through (see [PlanNode.DbHits]).
//
// Deriving rather than threading is what keeps the cost-when-off property absolute
// for the derived set: no counter is passed through any storage accessor, so a
// non-PROFILE Run executes not just no counting but no counting CODE — there is no
// branch to skip. [storageAccessCounter] is admitted only where the operator
// ALREADY maintains the counter for its own reasons, so it costs a non-PROFILE run
// nothing either. [noStorageAccess] is a marker method with an empty body, so it
// costs nothing anywhere; and all three are consulted HERE, in the wrapper, which
// only a PROFILE run allocates.
func (p *profiledOp) planStats() (int64, time.Duration, int64, bool) {
	hits, known := p.dbHits()
	return p.rows, p.elapsed, hits, known
}

// dbHits returns the storage accesses attributable to the wrapped operator, and
// whether that number was established at all.
//
// The measured counter wins over the derived one wherever both are available: a
// figure the operator counted is never worse than a figure inferred from its
// boundary. known=false is returned for an operator claiming none of the three
// markers, which is the honest answer and never a zero.
func (p *profiledOp) dbHits() (int64, bool) {
	if c, ok := p.inner.(storageAccessCounter); ok {
		return c.storageAccesses(), true
	}
	if _, ok := p.inner.(StorageRecordScan); ok {
		return p.rows, true
	}
	if _, ok := p.inner.(noStorageAccess); ok {
		return 0, true
	}
	return 0, false
}

// profiledChunkOp is the wrapper for an operator that also produces chunks. It
// preserves [ChunkProducer] so a columnar parent still recognises its child as
// columnar, and measures the columnar path as well as the row path.
type profiledChunkOp struct {
	profiledOp
	chunk ChunkProducer
}

// NewOutputChunk forwards to the wrapped producer.
func (p *profiledChunkOp) NewOutputChunk(capacity int) *Chunk {
	return p.chunk.NewOutputChunk(capacity)
}

// FillChunk forwards, counting the rows appended and the time taken. The
// columnar drain never calls Next, so without this a columnar operator would
// report zero rows.
func (p *profiledChunkOp) FillChunk(dst *Chunk, maxRows int) (int, error) {
	start := time.Now()
	n, err := p.chunk.FillChunk(dst, maxRows)
	p.elapsed += time.Since(start)
	p.rows += int64(n)
	return n, err
}

// profiledNodeIDOp is the wrapper for a [NodeIDColumnProducer]. The marker
// method is what the interface is identified by, and it is unexported to this
// package, so only a wrapper declared here can preserve the claim.
type profiledNodeIDOp struct {
	profiledChunkOp
}

func (p *profiledNodeIDOp) nodeIDColumnProducer() {}

// Every variant Wrap can return must satisfy [profiledNode], or [PlanTree] would
// walk past a wrapper without unwrapping it and the node would render unmeasured.
// Wrap used to enforce that with a `w.(profiledNode)` type assertion on the value
// it appended to a retained slice; the slice was write-only and the append was a
// data race (rmp #2664), so both are gone and the invariant is stated here — at
// compile time, where it belongs, rather than as a panic on the build path.
var (
	_ profiledNode         = (*profiledOp)(nil)
	_ profiledNode         = (*profiledChunkOp)(nil)
	_ profiledNode         = (*profiledNodeIDOp)(nil)
	_ ChunkProducer        = (*profiledChunkOp)(nil)
	_ NodeIDColumnProducer = (*profiledNodeIDOp)(nil)
	_ rowCountHinter       = (*profiledOp)(nil)
)

// ─────────────────────────────────────────────────────────────────────────────
// StorageRecordScan — which operators have db-hits at all
// ─────────────────────────────────────────────────────────────────────────────

// StorageRecordScan marks an operator whose emitted row count IS its logical
// storage-access count: it reads exactly one record per row it emits, so the
// boundary count the profiling wrapper already has is the access count
// (rmp #2238).
//
// # Why a marker rather than a counter
//
// Db-hits exist to distinguish a selective seek from a scan that filtered
// afterwards: both can emit the same handful of rows while touching wildly
// different amounts of storage.
//
// For the operators listed below, "records read" and "rows emitted" are the same
// number by construction:
//
//   - a label or all-nodes scan yields one node reference per emitted row;
//   - an index seek, seek-set or range scan yields one node reference per
//     posting-list entry it emits.
//
// [Expand] used to be in that list, on the strength of "one relationship slot per
// EMITTED neighbour". It is not any more: emitted is not read, and rmp #2761 moved
// it to [storageAccessCounter]. What remains here is the set of access-path LEAVES,
// for which the boundary count and the access count coincide because nothing inside
// the operator filters.
//
// So the count is available at the operator boundary, where the profiling wrapper
// already sits, and needs no counter threaded through any accessor. That is not a
// shortcut but the point: with nothing threaded, a non-PROFILE Run executes no
// counting CODE AT ALL — there is not even a nil check to skip on the hot path.
//
// # Where the identity does NOT hold, and what is done about it
//
// The identity is a property of these particular operators, not a law of access
// paths, and rmp #2720 measured three places where it fails. Each is stated here
// rather than left for a reader of a `dbhits=` figure to discover:
//
//   - A TRAVERSAL operator reads many relationship records per emitted row.
//     [VarLengthExpand] therefore does NOT carry this marker; it implements
//     [storageAccessCounter] instead and reports the count it already maintains
//     for its traversal budget, so its figure is MEASURED. Measured on a 200-way
//     fan with one 3-hop chain, `-[*3..3]->` emitted one row for 202 relationship
//     slots read — a 202x under-report before the counter was wired.
//   - [ShortestPath] and [AllShortestPaths] read relationship records across a
//     BFS and emit one row per input row (or one per shortest path), so no
//     derivation from rows could describe the walk: a 100-way fan with one
//     continuation to dst reads 101 slots and emits ONE row. Until rmp #2763 they
//     carried none of the three markers and their cell rendered UNKNOWN, because
//     the only counter they had — totalEdgesTraversed — covers just the
//     exhaustive path-predicate search and would have reported an
//     authoritative-looking 0 for the common path. Since #2763 both implement
//     [storageAccessCounter] and report the adjacency slots every one of their
//     searches read, charged one add per RUN rather than per slot: the scan loop's
//     own bound IS the charge, because no scan in either operator breaks out of a
//     run early ([ShortestPath.scanRun]). totalEdgesTraversed stays a resource
//     budget and is deliberately NOT added in — it counts ADMITTED ARCS over the
//     same runs, so summing the two would count one walk twice under two
//     definitions ([ShortestPath.storageAccesses]).
//   - A single-hop [Expand] with a relationship-type filter reads every slot of
//     the source's adjacency run and emits only the admitted ones (the edgeSkip
//     branch), so a derived figure counts EMITTED edges, not slots read. Measured
//     before the correction: an out-degree-100 node with one :KNOWS edge reported
//     1 db-hit for the same 100-slot CSR walk that `-->` reported 100 for. [Expand]
//     and [OptionalExpand] therefore do NOT carry this marker either; since
//     rmp #2761 they implement [storageAccessCounter] and both arms report 100.
//     The counter that made it possible is not a per-slot increment — the cursors
//     already advance one position per slot, so the count is recovered in O(1) per
//     INPUT ROW ([Expand.closeSlotWindow]). That is why it could be
//     admitted under the paragraph below without paying the cost this marker exists
//     to refuse.
//   - The morsel-parallel leaves ([ParallelScanProject], [ParallelAggregateScan],
//     [ParallelCountScan]) each walk a whole node source across worker goroutines
//     while emitting the admitted rows, one row per group, or a single row. For all
//     three the derivation is not merely imprecise but unrelated to the work: a
//     count leaf reports 1 for a 2000-node walk. Since rmp #2762 they implement
//     [storageAccessCounter] and report the node references their workers consumed,
//     folded from a worker-local by ONE atomic add per morsel — never per node,
//     which is the same refusal the paragraph above makes, at morsel granularity
//     rather than at row granularity.
//
// # What this deliberately does not count
//
// PROPERTY READS. Neo4j charges a db-hit per property access, so its numbers for a
// filter-heavy plan are larger than GoGraph's. Counting them here would mean
// threading a counter into the property accessors — precisely the hot path the
// paragraph above protects — and would make every ordinary query pay for a
// diagnostic. The divergence is documented in docs/cypher.md rather than papered
// over with an estimate, because a db-hits figure that silently blends measured
// leaf reads with guessed property reads would be less useful than one whose
// meaning is exact.
//
// An operator that implements none of this interface, [storageAccessCounter] and
// [noStorageAccess] reports db-hits UNKNOWN. That is the honest default and the
// one rmp #2760 chose deliberately: a pure row transformer opts IN to a known
// zero by claiming [noStorageAccess], rather than every uncounted operator being
// silently defaulted to a zero it never earned.
type StorageRecordScan interface {
	// storageRecordPerRow is a marker. It is unexported so only operators in this
	// package can claim to read storage, which keeps the guarantee auditable: the
	// set of db-hit sources is the set of implementations in this file.
	storageRecordPerRow()
}

func (*AllNodesScan) storageRecordPerRow()         {}
func (*NodeByLabelScan) storageRecordPerRow()      {}
func (*NodeByIndexSeek) storageRecordPerRow()      {}
func (*NodeByIndexSeekSet) storageRecordPerRow()   {}
func (*NodeByIndexRangeScan) storageRecordPerRow() {}

// ─────────────────────────────────────────────────────────────────────────────
// storageAccessCounter — operators that COUNT their own storage accesses
// ─────────────────────────────────────────────────────────────────────────────

// storageAccessCounter is implemented by an operator that can report the number
// of storage records it actually read, rather than having that number inferred
// from the rows it emitted ([StorageRecordScan]).
//
// # The admission rule: never one increment per record
//
// The rule this interface enforces is not "the counter must already exist" but
// "the counter must never cost a per-RECORD increment", which is the trade
// [StorageRecordScan] exists to refuse. Three shapes satisfy it, and all three are
// in use:
//
//   - The counter ALREADY EXISTS for the operator's own reasons — a traversal
//     budget, a safety cap. [VarLengthExpand] reports the figure its budget
//     maintains, so it costs a non-PROFILE run nothing at all.
//   - The count is RECOVERED from state that already advances with the work.
//     [Expand] reads it off its adjacency cursors, which already move one position
//     per slot consumed, so [Expand.closeSlotWindow] recovers it in O(1) per INPUT
//     ROW (rmp #2761).
//   - The count is charged ONCE PER BATCH the operator already processes as a unit.
//     The morsel-parallel leaves charge each morsel's length once, when its scan
//     leaf has read it — one add per [DefaultMorselSize] node references, on the
//     goroutine that read them (rmp #2762). [ShortestPath] and [AllShortestPaths]
//     apply the same shape at a finer unit: the batch is one node's ADJACENCY RUN,
//     whose length is the scan loop's own bound, so the charge is one add per run
//     against a loop of deg(node) iterations (rmp #2763). That the batch charge
//     equals a per-record one is not an argument in either case — it holds here
//     because no scan in either operator leaves a run early, and it was verified
//     with a temporary per-slot probe that panicked on disagreement.
//
// An operator that could satisfy the interface only by incrementing per record
// must NOT implement it: paying every ordinary query for a diagnostic is the
// trade this whole design refuses. For a CONCURRENT operator the bar is higher
// still — a shared atomic per record is a contention defect against CLAUDE.md's
// mandate 3 and not merely a cost, which is why the third shape above accumulates
// in a worker LOCAL and publishes once per morsel.
//
// The method is unexported so only operators in this package can claim to have
// measured their accesses, which keeps the set of measured sources auditable.
//
// A figure reported here is MEASURED. A figure from [StorageRecordScan] is
// DERIVED. The rendered plan does not distinguish them, which rmp #2720 records
// as a known limitation of the output rather than of the accounting.
type storageAccessCounter interface {
	// storageAccesses returns the storage records this operator has read over its
	// whole lifetime, including any Init it has been restarted by.
	storageAccesses() int64
}

// The census of MEASURED operators. Like the [noStorageAccess] block below, this
// exists so the claim is checked at compile time and readable in one place: an
// unexported marker method that does not actually satisfy the interface would
// leave the operator reporting UNKNOWN while its own godoc said it was measured.
// TestDbHitsClassification_EveryOperatorIsClassified is the drift gate that keeps
// this list and the hand-written census in
// cypher/exec/dbhits_classification_test.go from parting company.
var (
	_ storageAccessCounter = (*VarLengthExpand)(nil)
	_ storageAccessCounter = (*Expand)(nil)
	_ storageAccessCounter = (*OptionalExpand)(nil)
	_ storageAccessCounter = (*columnarExpand)(nil) // promoted from the embedded *Expand
	_ storageAccessCounter = (*ParallelScanProject)(nil)
	_ storageAccessCounter = (*ParallelAggregateScan)(nil)
	_ storageAccessCounter = (*ParallelCountScan)(nil)
	_ storageAccessCounter = (*ShortestPath)(nil)
	_ storageAccessCounter = (*AllShortestPaths)(nil)
)

// ─────────────────────────────────────────────────────────────────────────────
// noStorageAccess — operators whose db-hits are a KNOWN zero
// ─────────────────────────────────────────────────────────────────────────────

// noStorageAccess marks an operator that opens NO access path: it reads no node
// or relationship record, seeks no index, and consults no maintained counter, so
// its db-hits figure is a measured, meaningful ZERO rather than a figure nobody
// counted (rmp #2760).
//
// # Why the claim has to be explicit
//
// Before rmp #2760 the zero was a DEFAULT: an operator implementing neither
// [StorageRecordScan] nor [storageAccessCounter] rendered `dbhits=0`, and the
// column could not tell "counted, and it is zero" from "not counted at all".
// [ParallelScanProject] printed the same 0 for a full scan of 2000 nodes that a
// pure projection prints for reading nothing. (Since rmp #2762 that leaf counts,
// and reports 2000; the point the example makes about the COLUMN stands.) Making
// the zero an explicit claim
// inverts that: the honest default became UNKNOWN, and only an operator that can
// stand behind a zero says so here.
//
// The method is unexported for the same reason [StorageRecordScan]'s is: the set
// of operators claiming to read nothing is the set of implementations in this
// file, so the classification is auditable in one place and at compile time.
// TestDbHitsClassification_EveryOperatorIsClassified is the drift gate on it —
// it derives the operator set from the package source and fails when an operator
// is added, renamed, or reclassified without the census below being updated.
//
// # The bar for claiming it
//
// An operator may claim noStorageAccess only when NEITHER its Next/FillChunk nor
// anything it calls from them can reach the graph. In practice that rules out
// every operator holding a caller-supplied expression closure, because such a
// closure is evaluated through cypher's evalRow bridge, which passes the
// [github.com/FlavioCFOliveira/GoGraph/cypher/expr.PatternEvaluator] — and that
// evaluator WALKS ADJACENCY. Measured on a 100-way fan (rmp #2760):
//
//	MATCH (r:Root) WHERE (r)-[:LIKES]->() RETURN r.k
//	  ColumnarProject → Filter → NodeByLabelScan     total db-hits reported: 1
//	MATCH (r:Root)-[:LIKES]->(m) RETURN DISTINCT r.k
//	  Distinct → ColumnarProject → Expand → scan     Expand alone: 100
//
// Both arms must read the Root's relationship slots to answer; the Filter arm
// reports none of them. A pattern comprehension inside a projection behaves the
// same way — `RETURN size([(r)-[:LIKES]->(x) | 1])` is evaluated INSIDE Project
// (it is not lowered to a RollUpApply) and walks all 100 slots while the plan
// reports 1 db-hit in total. So [Filter], [Project] and their columnar forms,
// [Sort], [Top], [Unwind], [HashJoin], [ColumnarHashJoin], [RollUpApply] and
// [ProcedureCallOp] are all UNKNOWN, not zero: each holds such a closure.
//
// This corrects docs/explain-profile-honesty-audit-2026-09-03.md, which recorded
// Project as "a pure row transformer [that] reports dbhits=0, honestly". It is a
// row transformer, but its projection can reach the graph, so its zero was an
// under-report of exactly the kind the audit set out to expose.
//
// # What claiming it does NOT assert
//
// It says nothing about PROPERTY reads, which this column does not count at all
// for any operator — a documented divergence from Neo4j, stated on
// [StorageRecordScan] and in docs/cypher.md. An operator that reads a property
// off a node already bound in its input row therefore still qualifies.
type noStorageAccess interface {
	// readsNoStorage is a marker. Like [StorageRecordScan]'s it is unexported, so
	// only operators declared in this package can make the claim.
	readsNoStorage()
}

// The census. Each entry is a claim that the named operator opens no access path,
// with the reason it can be made. An operator absent from this list reports
// UNKNOWN db-hits, which is the honest default.

// --- row sources that produce rows without reading the graph ---------------

// Argument re-emits the outer row its Apply driver set on it.
func (*Argument) readsNoStorage() {}

// SingleRow emits one empty row and nothing else.
func (*SingleRow) readsNoStorage() {}

// singleRow emits one caller-supplied row, already materialised.
func (*singleRow) readsNoStorage() {}

// StaticRows emits rows built before execution began.
func (*StaticRows) readsNoStorage() {}

// --- row transformers that hold no caller-supplied expression --------------

// Limit forwards its child's rows and stops at a count fixed when it was built.
// ColumnarLimit embeds Limit and inherits this claim, correctly: its columnar
// path likewise only counts and forwards.
func (*Limit) readsNoStorage() {}

// Skip forwards its child's rows after discarding a count fixed at build time.
func (*Skip) readsNoStorage() {}

// Eager buffers its child's rows and re-emits them.
func (*Eager) readsNoStorage() {}

// Distinct hashes and compares values already bound in the row.
func (*Distinct) readsNoStorage() {}

// CountRows counts its child's rows and emits the count.
func (*CountRows) readsNoStorage() {}

// UnionAll concatenates two inputs' rows.
func (*UnionAll) readsNoStorage() {}

// Union deduplicates a UnionAll through an embedded Distinct.
func (*Union) readsNoStorage() {}

// EagerAggregation groups on COLUMN INDICES (keyCols) and feeds each aggregate
// from a column of the input row; it evaluates no expression of its own, so the
// projection that produced those columns is where any graph access is attributed.
func (*EagerAggregation) readsNoStorage() {}

// GlobalAggregateAdapter forwards its child's rows, and on an empty child emits
// one row of aggregator neutral values — constants, computed from nothing.
func (*GlobalAggregateAdapter) readsNoStorage() {}

// --- drivers whose work is entirely in their children ----------------------
//
// Each of these re-drives an inner sub-plan per outer row. The inner operators
// are wrapped and measured in their own right, and appear as this operator's
// children in the rendered tree, so attributing anything to the driver itself
// would double-count what its children already report.

// Apply drives its inner plan once per outer row.
func (*Apply) readsNoStorage() {}

// CorrelatedApply drives its inner plan once per outer row.
func (*CorrelatedApply) readsNoStorage() {}

// OptionalApply drives its inner plan once per outer row, padding when it is empty.
func (*OptionalApply) readsNoStorage() {}

// SemiApply forwards an outer row when its inner plan yields at least one.
func (*SemiApply) readsNoStorage() {}

// AntiSemiApply forwards an outer row when its inner plan yields none.
func (*AntiSemiApply) readsNoStorage() {}

// Foreach drives its inner plan once per outer row and emits the outer row.
func (*Foreach) readsNoStorage() {}

// Every marker method above must be reachable through the interface, or the claim
// would be silently inert: the wrapper's type assertion would simply not match and
// the operator would report UNKNOWN while its documentation said otherwise.
var (
	_ noStorageAccess = (*Argument)(nil)
	_ noStorageAccess = (*SingleRow)(nil)
	_ noStorageAccess = (*singleRow)(nil)
	_ noStorageAccess = (*StaticRows)(nil)
	_ noStorageAccess = (*Limit)(nil)
	_ noStorageAccess = (*ColumnarLimit)(nil) // promoted from the embedded Limit
	_ noStorageAccess = (*Skip)(nil)
	_ noStorageAccess = (*Eager)(nil)
	_ noStorageAccess = (*Distinct)(nil)
	_ noStorageAccess = (*CountRows)(nil)
	_ noStorageAccess = (*UnionAll)(nil)
	_ noStorageAccess = (*Union)(nil)
	_ noStorageAccess = (*EagerAggregation)(nil)
	_ noStorageAccess = (*GlobalAggregateAdapter)(nil)
	_ noStorageAccess = (*Apply)(nil)
	_ noStorageAccess = (*CorrelatedApply)(nil)
	_ noStorageAccess = (*OptionalApply)(nil)
	_ noStorageAccess = (*SemiApply)(nil)
	_ noStorageAccess = (*AntiSemiApply)(nil)
	_ noStorageAccess = (*Foreach)(nil)
)
