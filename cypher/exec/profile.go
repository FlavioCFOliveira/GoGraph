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
	// planStats returns the rows emitted and the time attributed to the operator.
	planStats() (int64, time.Duration)
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

func (p *profiledOp) planUnwrap() Operator              { return p.inner }
func (p *profiledOp) planStats() (int64, time.Duration) { return p.rows, p.elapsed }

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
