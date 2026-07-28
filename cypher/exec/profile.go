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
// The measurements therefore live here; cypher/explain keeps its DbHits counter
// and report formatter, and folding db-hits into this Profiler is tracked
// separately (rmp #2238) since it needs a counter threaded through the storage
// accessors rather than a wrapper.
//
// # Concurrency
//
// A Profiler is NOT safe for concurrent use and must not be shared between
// queries. One query's pipeline is driven by one goroutine, so the wrappers need
// no synchronisation. An operator that fans work out internally (the parallel
// tier) is measured as one node: the wrapper times the calls the driving
// goroutine makes, which is the honest attribution — it cannot see inside.
type Profiler struct {
	// wrapped retains every wrapper in build order, tying their lifetime to the
	// Profiler and keeping them reachable without a tree walk.
	wrapped []profiledNode
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

	base := profiledOp{inner: op}
	var w Operator
	switch cp := op.(type) {
	case NodeIDColumnProducer:
		w = &profiledNodeIDOp{profiledChunkOp{profiledOp: base, chunk: cp}}
	case ChunkProducer:
		w = &profiledChunkOp{profiledOp: base, chunk: cp}
	default:
		w = &profiledOp{inner: op}
	}
	p.wrapped = append(p.wrapped, w.(profiledNode))
	return w
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

// planStats reports the measured rows and time, plus the db-hits DERIVED from
// them.
//
// Db-hits are not accumulated in the hot path, and that is the whole design. The
// task's own framing was that a wrapper cannot count them because the accesses
// happen inside an operator rather than at its boundary — true in general, and
// false for the operators where the count is actually defined. An operator that
// reads records from storage reads exactly one per row it emits ([StorageRecordScan]
// documents why), so the boundary row count IS the access count; an operator that
// only transforms its children's rows touches no storage and its count is zero.
//
// Deriving rather than threading is what keeps the cost-when-off property absolute:
// no counter is passed through any storage accessor, so a non-PROFILE Run executes
// not just no counting but no counting CODE — there is no branch to skip.
func (p *profiledOp) planStats() (int64, time.Duration, int64) {
	return p.rows, p.elapsed, p.dbHits()
}

// dbHits returns the storage accesses attributable to the wrapped operator.
func (p *profiledOp) dbHits() int64 {
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

var (
	_ profiledNode         = (*profiledOp)(nil)
	_ ChunkProducer        = (*profiledChunkOp)(nil)
	_ NodeIDColumnProducer = (*profiledNodeIDOp)(nil)
	_ rowCountHinter       = (*profiledOp)(nil)
)

// ─────────────────────────────────────────────────────────────────────────────
// StorageRecordScan — which operators have db-hits at all
// ─────────────────────────────────────────────────────────────────────────────

// StorageRecordScan marks an operator that READS RECORDS FROM STORAGE, one record
// per row it emits, so its logical storage-access count (its db-hits) equals its
// emitted row count (rmp #2238).
//
// # Why a marker rather than a counter
//
// Db-hits exist to distinguish a selective seek from a scan that filtered
// afterwards: both can emit the same handful of rows while touching wildly
// different amounts of storage. That distinction lives entirely in the LEAVES —
// which records were read — and every operator above them consumes rows its
// children already produced, touching no storage of its own.
//
// For a leaf, "records read" and "rows emitted" are the same number by
// construction:
//
//   - a label or all-nodes scan yields one node record per emitted row;
//   - an index seek, seek-set or range scan yields one node record per posting-list
//     entry it emits;
//   - an expand yields one relationship record per emitted neighbour.
//
// So the count is available at the operator boundary, where the profiling wrapper
// already sits, and needs no counter threaded through any accessor. That is not a
// shortcut but the point: with nothing threaded, a non-PROFILE Run executes no
// counting CODE AT ALL — there is not even a nil check to skip on the hot path.
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
// An operator that does not implement this interface reports 0 db-hits, which is
// the honest answer for a pure row transformer.
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
func (*VarLengthExpand) storageRecordPerRow()      {}
