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
// Its ROWS and TIME are therefore real measurements of the whole phase, and its
// DB-HITS are not measured at all: the leaf claims neither [StorageRecordScan] nor
// [storageAccessCounter], so the cell reads 0 for a full scan of the graph. The
// same query below the parallel threshold plans a [NodeByLabelScan] and reports
// one db-hit per node, so the identical work reports 0 or N according to a
// threshold the reader did not set. The leaves' [PlanDetail] states this next to
// the number, which is the only place the current column shape allows it to be
// said (rmp #2720).
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
	// planStats returns the rows emitted, the time attributed to the operator, and
	// the logical storage accesses attributed to it.
	planStats() (int64, time.Duration, int64)
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

// planStats reports the measured rows and time, plus the db-hits.
//
// Db-hits come from one of two places, in this order:
//
//   - a MEASURED count, when the operator implements [storageAccessCounter] and
//     can report the records it actually read at no cost to a non-PROFILE run; or
//   - a count DERIVED from the emitted rows, when the operator implements
//     [StorageRecordScan] — a marker that asserts one record read per row emitted.
//
// Every other operator reports zero, which is the honest answer for a pure row
// transformer and an UNDER-REPORT for an operator that reads storage without
// claiming either interface. [StorageRecordScan] enumerates which is which, and
// names the operators whose true count this file cannot reach.
//
// Deriving rather than threading is what keeps the cost-when-off property absolute
// for the derived set: no counter is passed through any storage accessor, so a
// non-PROFILE Run executes not just no counting but no counting CODE — there is no
// branch to skip. [storageAccessCounter] is admitted only where the operator
// ALREADY maintains the counter for its own reasons, so it costs a non-PROFILE run
// nothing either.
func (p *profiledOp) planStats() (int64, time.Duration, int64) {
	return p.rows, p.elapsed, p.dbHits()
}

// dbHits returns the storage accesses attributable to the wrapped operator.
//
// The measured counter wins over the derived one wherever both are available: a
// figure the operator counted is never worse than a figure inferred from its
// boundary.
func (p *profiledOp) dbHits() int64 {
	if c, ok := p.inner.(storageAccessCounter); ok {
		return c.storageAccesses()
	}
	if _, ok := p.inner.(StorageRecordScan); ok {
		return p.rows
	}
	return 0
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
//     posting-list entry it emits;
//   - a single-hop expand yields one relationship slot per EMITTED neighbour.
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
//   - [ShortestPath] and [AllShortestPaths] read relationship records and carry
//     NEITHER interface, so they report 0. Their own totalEdgesTraversed counter
//     covers only the exhaustive path-predicate search and not the bidirectional
//     BFS, so wiring it would report an authoritative-looking zero for the common
//     path; reporting 0 with this note is the lesser misstatement of the two.
//   - A single-hop [Expand] with a relationship-type filter reads every slot of
//     the source's adjacency run and emits only the admitted ones (the edgeSkip
//     branch), so its figure counts EMITTED edges, not slots read. Measured: an
//     out-degree-100 node with one :KNOWS edge reports 1 db-hit for the same
//     100-slot CSR walk that `-->` reports 100 for. Correcting it needs a counter
//     the operator does not have, whose per-slot increment a non-PROFILE run would
//     pay — the trade this marker exists to avoid — so it is recorded, not fixed.
//   - The morsel-parallel leaves ([ParallelScanProject], [ParallelAggregateScan],
//     [ParallelCountScan]) carry neither interface and report 0 for a full scan.
//     Their [PlanDetail] says so in the rendered plan.
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
// An operator that implements neither this interface nor [storageAccessCounter]
// reports 0 db-hits, which is the honest answer for a pure row transformer and an
// under-report for the operators named above.
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
func (*Expand) storageRecordPerRow()               {}
func (*OptionalExpand) storageRecordPerRow()       {}

// ─────────────────────────────────────────────────────────────────────────────
// storageAccessCounter — operators that COUNT their own storage accesses
// ─────────────────────────────────────────────────────────────────────────────

// storageAccessCounter is implemented by an operator that can report the number
// of storage records it actually read, rather than having that number inferred
// from the rows it emitted ([StorageRecordScan]).
//
// It is admitted only where the operator ALREADY maintains the counter for its
// own reasons — a traversal budget, a safety cap — so implementing it costs a
// non-PROFILE run nothing, and the cost-when-off guarantee this file's Profiler
// documents is untouched. An operator that would have to add a per-record
// increment to satisfy it must NOT implement it: paying every ordinary query for
// a diagnostic is the trade [StorageRecordScan] exists to refuse.
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
