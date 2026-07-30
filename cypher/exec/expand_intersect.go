package exec

// expand_intersect.go — the fused cyclic-pattern expand (rmp #2157).
//
// # What it replaces, and why a FUSION rather than a new strategy
//
// A directed triangle plans today as a left-deep chain whose last two hops are:
//
//	Expand [ExpandInto seek]     ← seeks c→a, discarding every c that does not close
//	└─ Expand                    ← OPEN: materialises every c ∈ N_out(b)
//
// The open hop materialises the whole of b's neighbourhood and the seek above it
// throws away all but the closing ones. Together they compute
// `N_out(b) ∩ N_in(a)` — by materialising the left operand in full first. This
// operator computes the same set directly, so a non-closing candidate is never
// built into a row.
//
// SPIKE #2155 (docs/design-wcoj-cyclic-patterns.md) established that this — not a
// general worst-case-optimal join — is the shape worth building. Every simple
// cycle admits exactly ONE intersection, at the vertex the ExpandInto seek already
// occupies, so Leapfrog Triejoin and Generic Join both degenerate to the same
// 2-way leapfrog on a binary relation, and genuine multi-way intersection needs
// K4 or denser. Framing it as a fusion of two adjacent existing operators is what
// keeps it small enough to verify.
//
// # No type check inside the merge — this is the load-bearing design decision
//
// The SPIKE's first measurements reported a REGRESSION for a typed pattern,
// because they type-checked the reverse side while merging: the reverse CSR
// carries no relationship type, so each reverse slot needed its forward position
// recovered before the type map could be probed.
//
// This operator never does that. It intersects the RAW ordered runs — no type
// filter, no morphism check, no map probe in the merge at all — and only then, for
// each surviving candidate, materialises the two edge identities with FORWARD
// `dstRun` lookups. Both r2 (in b's forward run) and r3 (in c's forward run) are
// forward positions, so every type check is a plain forward map probe on a
// candidate that already closed the cycle.
//
// The intersection therefore runs over a SUPERSET of the admissible candidates
// (it cannot see types), and the per-candidate lookups reject the rest. That is
// strictly cheaper than filtering during the merge, because the merge visits many
// more slots than it yields.
//
// # Semantic obligations, all verified against the engine during the SPIKE
//
//   - EDGE IDENTITY IS PUBLISHED. The intersection knows destination identity, not
//     edge identity, and destination identity is NOT sufficient: openCypher's
//     relationship-uniqueness scope is the whole MATCH clause including sibling
//     patterns, and with two parallel a→b edges a sibling leg legitimately
//     consumes one while the cycle uses the other. So both handles are
//     re-materialised and emitted as row columns, exactly as the two Expands
//     would have.
//   - PARALLEL-EDGE MULTIPLICITY is the cross-product of the two legs' contiguous
//     handle runs, which the total-key `(destination, handle)` ordering makes a
//     single `dstRun` each.
//   - RELATIONSHIP ISOMORPHISM is enforced twice: against the input row's sibling
//     columns (as Expand does), and between r2 and r3 themselves — the check the
//     closing Expand used to perform via its RelCols.
//   - EMISSION ORDER is preserved exactly. The two Expands emit c ascending, then
//     r2 ascending within c, then r3 ascending. The Intersector yields candidates
//     strictly ascending and the two `dstRun` walks are position-ordered, so the
//     nesting here is identical and no order-safety predicate applies.
//
// # Scope
//
// Both legs must be single-hop, DirOut, with statically-known types. DirBoth is
// vetoed: an undirected neighbourhood is `out ∪ in`, which is not one contiguous
// ordered run, so the primitive does not apply to it. Variable-length legs and
// CREATE-multiplicity are vetoed too — see [ExpandIntersectConfig].
//
// Not safe for concurrent use. The CSR snapshots it reads are immutable.

import (
	"context"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	cmetrics "github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// MetricExpandIntersectEngaged counts how many times a fused cyclic expand was
// actually initialised for execution.
//
// This is the white-box engagement counter, and it is not optional observability:
// SPIKE #2155 verified that the openCypher TCK contains NO directed cycle over
// three or more distinct node variables, so the 3897/3897 gate stays green whether
// this operator is correct, wrong, or never runs at all. A differential comparing
// the flag on against the flag off is likewise blind to an operator that silently
// declined to engage — both arms would simply run today's plan and agree. Only a
// counter distinguishes "identical because it is correct" from "identical because
// it never fired".
const MetricExpandIntersectEngaged = "cypher.expand_intersect.engaged"

// ExpandIntersectConfig configures a fused cyclic expand.
type ExpandIntersectConfig struct {
	// MidEdgeTypeFilter and EndEdgeTypeFilter map forward edge positions to the
	// accepted type set for the middle (b→c) and closing (c→a) legs. Required
	// whenever the corresponding EdgeType is non-empty.
	MidEdgeTypeFilter map[uint64]string
	EndEdgeTypeFilter map[uint64]string
	// MidEdgeType and EndEdgeType, when non-empty, restrict each leg to edges
	// present in the corresponding filter.
	MidEdgeType string
	EndEdgeType string
	// RelCols lists input-row columns holding edge IDs already traversed by
	// sibling relationship patterns in the same MATCH clause. Both emitted edges
	// must avoid all of them (relationship isomorphism). Empty disables the check.
	RelCols []int
	// MidCol is the input-row column holding b, the middle hop's source.
	MidCol int
	// EndCol is the input-row column holding a, the node the cycle closes on.
	EndCol int
}

// ExpandIntersect fuses a cycle's open middle hop and its closing seek into one
// operator driven by a sorted-set intersection.
type ExpandIntersect struct {
	input Operator
	ctx   context.Context

	fwd, rev csrAdjacency
	fwdVerts []uint64
	fwdEdges []graph.NodeID
	revVerts []uint64
	revEdges []graph.NodeID

	midType, endType     string
	midFilter, endFilter map[uint64]string
	relCols              []int
	midCol, endCol       int

	it csr.Intersector
	// ranges is the reusable two-element backing array handed to Intersector.Init,
	// so no slice is allocated per input row.
	ranges [2]csr.Range

	inputRow Row
	outBuf   []expr.Value

	bID, aID int64

	// Cursors. haveInput/haveC/haveR2 say which of the nested levels is live.
	haveInput, haveC, haveR2 bool
	cID                      int64
	r2Pos, r2End             uint64
	r2Cur                    uint64
	r3Lo, r3Hi, r3Pos        uint64

	emitCount int
	done      bool
}

// NewExpandIntersect creates a fused cyclic expand over the forward and reverse
// CSRs. rev is required: the candidate set is N_in(a).
//
// cfg is taken by pointer because it is 88 bytes and this is a plan-build call, so
// there is nothing to gain from copying it. A nil cfg is treated as the zero value,
// which is an untyped fusion reading b from column 0 and a from column 0 — valid
// but not useful, so callers always pass one.
func NewExpandIntersect(input Operator, fwd, rev csrAdjacency, cfg *ExpandIntersectConfig) *ExpandIntersect {
	if cfg == nil {
		cfg = &ExpandIntersectConfig{}
	}
	return &ExpandIntersect{
		input:     input,
		fwd:       fwd,
		rev:       rev,
		midType:   cfg.MidEdgeType,
		endType:   cfg.EndEdgeType,
		midFilter: cfg.MidEdgeTypeFilter,
		endFilter: cfg.EndEdgeTypeFilter,
		relCols:   cfg.RelCols,
		midCol:    cfg.MidCol,
		endCol:    cfg.EndCol,
	}
}

// Init initialises the child, snapshots both CSR directions, and RESETS every
// cursor so the operator can be re-executed.
//
// Resetting is not housekeeping — omitting it was a real defect, caught by the
// OPTIONAL MATCH case in cypher/cyclic_intersect_diff_test.go. Under a correlated
// Apply, Init runs once per OUTER ROW, not once per query. Without the reset the
// first outer row ran the operator to exhaustion, left done=true, and every
// subsequent outer row silently produced nothing — so an OPTIONAL MATCH over a
// cyclic pattern returned a null row for every input except at most the first. It
// failed silently and returned WRONG RESULTS rather than an error, which is why the
// reset is stated here explicitly rather than left implicit.
func (op *ExpandIntersect) Init(ctx context.Context) error {
	op.ctx = ctx
	if err := op.input.Init(ctx); err != nil {
		return err
	}
	cmetrics.IncCounter(MetricExpandIntersectEngaged, 1)
	op.fwdVerts = op.fwd.VerticesSlice()
	op.fwdEdges = op.fwd.EdgesSlice()
	op.revVerts = op.rev.VerticesSlice()
	op.revEdges = op.rev.EdgesSlice()
	op.done = false
	op.haveInput, op.haveC, op.haveR2 = false, false, false
	op.inputRow = nil
	op.emitCount = 0
	return nil
}

// PlanChildren reports the input whose rows drive the intersection.
func (op *ExpandIntersect) PlanChildren() []Operator { return []Operator{op.input} }

// PlanDetail names the two legs' types, which is the part of the physical
// decision the operator's own name cannot carry.
func (op *ExpandIntersect) PlanDetail() string {
	mid, end := op.midType, op.endType
	if mid == "" {
		mid = "*"
	}
	if end == "" {
		end = "*"
	}
	return "mid=" + mid + " close=" + end
}

// Close releases the child and drops the snapshots.
func (op *ExpandIntersect) Close() error {
	op.fwdVerts, op.fwdEdges, op.revVerts, op.revEdges = nil, nil, nil, nil
	op.inputRow = nil
	return op.input.Close()
}

// outRange returns the forward slot window for v, or an empty window when v is
// outside the snapshot's vertex space. A cached CSR pair can legitimately be
// narrower than the live node space, so this must be guarded.
func (op *ExpandIntersect) outRange(v int64) (uint64, uint64) {
	if v < 0 || uint64(v)+1 >= uint64(len(op.fwdVerts)) {
		return 0, 0
	}
	return op.fwdVerts[v], op.fwdVerts[v+1]
}

// inRange returns the reverse slot window for v, guarded the same way.
func (op *ExpandIntersect) inRange(v int64) (uint64, uint64) {
	if v < 0 || uint64(v)+1 >= uint64(len(op.revVerts)) {
		return 0, 0
	}
	return op.revVerts[v], op.revVerts[v+1]
}

// Next advances by one row.
func (op *ExpandIntersect) Next(out *Row) (bool, error) {
	if op.done {
		return false, nil
	}
	for {
		if op.emitCount&4095 == 0 && op.ctx != nil {
			if err := op.ctx.Err(); err != nil {
				return false, err
			}
		}
		if !op.haveInput {
			ok, err := op.loadInput()
			if err != nil {
				return false, err
			}
			if !ok {
				op.done = true
				return false, nil
			}
		}
		if !op.haveR2 {
			if !op.advanceR2() {
				// This input row is exhausted at every level.
				op.haveInput = false
				continue
			}
		}
		pos, ok := op.advanceR3()
		if !ok {
			op.haveR2 = false
			continue
		}
		op.buildRow(out, op.bID, int64(op.r2Cur), op.cID, int64(pos), op.aID)
		op.emitCount++
		return true, nil
	}
}

// loadInput pulls the next input row and arms the intersection for it. It skips
// rows that cannot produce anything — a malformed column, or an empty leg — which
// is where this operator is strictly cheaper than the plan it replaces: the open
// Expand would still scan b's whole run before the seek rejected everything.
func (op *ExpandIntersect) loadInput() (bool, error) {
	for {
		var row Row
		ok, err := op.input.Next(&row)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
		op.inputRow = row
		b, okB := op.nodeAt(row, op.midCol)
		a, okA := op.nodeAt(row, op.endCol)
		if !okB || !okA {
			continue
		}
		bs, be := op.outRange(b)
		as, ae := op.inRange(a)
		if bs == be || as == ae {
			continue
		}
		op.bID, op.aID = b, a
		op.ranges[0] = csr.Range{Edges: op.fwdEdges, Start: bs, End: be}
		op.ranges[1] = csr.Range{Edges: op.revEdges, Start: as, End: ae}
		if !op.it.Init(op.ranges[:]) {
			continue
		}
		op.haveInput = true
		op.haveC = false
		op.haveR2 = false
		return true, nil
	}
}

// nodeAt reads a NodeID from a row column, accepting EVERY representation the
// engine can legitimately place in a node column.
//
// # Why both forms have to be accepted (#2267)
//
// A node column carries a raw [expr.IntegerValue] — the canonical in-pipeline
// encoding — when it was produced by a scan or by an Expand, but a full
// [expr.NodeValue] when the variable last flowed through a PROJECTION. The second
// form is not exotic: any `WITH` between the anchor and the cycle produces it, so
//
//	MATCH (a) WITH a MATCH (a)-[:K]->(b)-[:K]->(c)-[:K]->(a) RETURN count(*)
//
// hands this operator a boxed `a`. Asserting only to IntegerValue classified that
// cell as a malformed column and SKIPPED the input row, and skipping every row is
// indistinguishable from a graph with no cycles: the query returned 0 where it must
// return 18. It failed silently, returned WRONG RESULTS rather than an error, and
// left the engagement counter ticking the whole time — so neither the TCK, nor a
// flag-on/flag-off differential, nor the counter alone could have caught it.
//
// # Why the accepted set is delegated rather than restated
//
// The set lives in [nodeIDFromValue], which is what the rest of the engine's row
// readers use. Restating it here is precisely how the two drifted apart in the
// first place, and a third representation added later would drift them apart
// again — silently, because the failure mode is a skipped row and not an error.
// Delegating makes that impossible by construction, and
// TestExpandIntersect_NodeCellAcceptanceMatchesExpand pins the remaining half of
// the invariant: that this operator and [Expand] accept the SAME set, so the fused
// plan and the two-Expand plan it replaces skip exactly the same rows.
//
// ok == false therefore now means one thing only: the column is out of range, or
// the cell is not node-typed at all (a null from an OPTIONAL MATCH that did not
// bind). That is the case [Expand.advanceInput] also skips.
func (op *ExpandIntersect) nodeAt(row Row, col int) (int64, bool) {
	if col < 0 || col >= len(row) {
		return 0, false
	}
	id, ok := nodeIDFromValue(row[col])
	if !ok {
		return 0, false
	}
	return int64(id), true
}

// advanceR2 moves to the next admissible middle-leg edge, pulling a new candidate
// c from the intersection whenever the current one is used up. It returns false
// once the current input row can yield nothing further.
func (op *ExpandIntersect) advanceR2() bool {
	for {
		if !op.haveC {
			c, ok := op.it.Next()
			if !ok {
				return false
			}
			op.cID = int64(c)
			bs, be := op.outRange(op.bID)
			op.r2Pos, op.r2End = dstRun(op.fwdEdges, bs, be, uint64(c))
			// The closing leg's run depends only on (c, a), so it is located once
			// per candidate and merely rewound for each r2.
			cs, ce := op.outRange(op.cID)
			op.r3Lo, op.r3Hi = dstRun(op.fwdEdges, cs, ce, uint64(op.aID))
			op.haveC = true
			if op.r3Lo == op.r3Hi {
				// The intersection saw c in N_in(a), but no c→a edge survives as a
				// FORWARD slot for this pattern — possible when the reverse run
				// carried a different logical edge. Nothing to emit for this c.
				op.haveC = false
				continue
			}
		}
		for op.r2Pos < op.r2End {
			pos := op.r2Pos
			op.r2Pos++
			if !passesTypeFilter(op.midType, op.midFilter, pos) {
				continue
			}
			if !op.passesRelMorphism(int64(pos)) {
				continue
			}
			op.r2Cur = pos
			op.r3Pos = op.r3Lo
			op.haveR2 = true
			return true
		}
		op.haveC = false
	}
}

// advanceR3 returns the next admissible closing-leg edge for the current r2.
//
// It enforces the isomorphism check that the closing Expand used to perform via
// its RelCols: r3 must differ from r2. Without it a single self-loop would be
// emitted as two distinct legs of the same cycle — the case that made a naive
// destination intersection report 4 triangles where the engine reports 3.
func (op *ExpandIntersect) advanceR3() (uint64, bool) {
	for op.r3Pos < op.r3Hi {
		pos := op.r3Pos
		op.r3Pos++
		if pos == op.r2Cur {
			continue
		}
		if !passesTypeFilter(op.endType, op.endFilter, pos) {
			continue
		}
		if !op.passesRelMorphism(int64(pos)) {
			continue
		}
		return pos, true
	}
	return 0, false
}

// passesRelMorphism reports whether edgeID is absent from every cyphermorphism
// column of the current input row. Same contract and same cost as
// [Expand.passesRelMorphism].
func (op *ExpandIntersect) passesRelMorphism(edgeID int64) bool {
	if len(op.relCols) == 0 {
		return true
	}
	for _, col := range op.relCols {
		if col < 0 || col >= len(op.inputRow) {
			continue
		}
		if iv, ok := op.inputRow[col].(expr.IntegerValue); ok && int64(iv) == edgeID {
			return false
		}
	}
	return true
}

// passesTypeFilter is the shared forward-position type test. Membership in the
// filter map is sufficient because the map holds only positions whose type is in
// the accepted set, which is what makes multi-type patterns ([r:A|B]) work.
func passesTypeFilter(edgeType string, filter map[uint64]string, pos uint64) bool {
	if edgeType == "" {
		return true
	}
	if filter == nil {
		return false
	}
	_, ok := filter[pos]
	return ok
}

// buildRow appends the SIX columns the two fused Expands would have appended, in
// their order: (b, r2, c) from the middle hop, then (c, r3, a) from the closing
// hop. The duplicated c column is not redundant — it is the closing hop's own
// source column, and every downstream column index depends on it being there.
func (op *ExpandIntersect) buildRow(out *Row, b, r2, c, r3, a int64) {
	need := len(op.inputRow) + 6
	if cap(op.outBuf) < need {
		op.outBuf = make([]expr.Value, need)
	}
	op.outBuf = op.outBuf[:need]
	copy(op.outBuf, op.inputRow)
	base := len(op.inputRow)
	op.outBuf[base] = expr.IntegerValue(b)
	op.outBuf[base+1] = expr.IntegerValue(r2)
	op.outBuf[base+2] = expr.IntegerValue(c)
	op.outBuf[base+3] = expr.IntegerValue(c)
	op.outBuf[base+4] = expr.IntegerValue(r3)
	op.outBuf[base+5] = expr.IntegerValue(a)
	*out = op.outBuf
}
