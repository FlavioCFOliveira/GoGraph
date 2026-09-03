package cypher

// plan_prefix.go — execution of a statement written with an EXPLAIN or PROFILE
// prefix (rmp #2721).
//
// The prefix is part of the GRAMMAR (see `script` in
// cypher/parser/grammar/CypherParser.g4), so it is the parser — not a textual
// scan ahead of it — that decides whether a statement carries one, and
// [parser.ParseStatement] reports which. This file is what the engine then does
// with that answer.
//
// # The one property that matters
//
// EXPLAIN plans the statement and executes NOTHING. PROFILE executes it and
// reports what each operator cost. That is the distinction the pair exists for,
// and it is a safety property before it is a diagnostic one: a user reaching for
// EXPLAIN on a DETACH DELETE must not lose their graph. Every route into this
// file therefore diverts BEFORE the write path opens a transaction — see the
// call sites in [Engine.runRead], [Engine.runInTxSession] and
// [ExplicitTx.Exec], each placed immediately after the semantic check and
// before any lock, transaction or build.
//
// # Result shape, and why it is Neo4j's
//
// The plan does NOT come back as rows. An EXPLAIN produces the statement's own
// column signature with ZERO rows; a PROFILE produces the statement's real rows,
// unchanged; and in both cases the plan travels beside the result, reachable as
// [Result.Plan] / [Result.Profile] and published by the Bolt server in the
// terminal SUCCESS metadata (`plan` / `profile`), which is exactly where the
// Neo4j drivers look for it (ResultSummary.Plan()/Profile()).
//
// The alternative — returning the rendered plan as a one-column result set —
// was rejected because it makes the plan invisible to every driver: a driver
// consuming `EXPLAIN MATCH (n) RETURN n` would receive a column named `plan`
// where it expects the query's own signature, and ResultSummary.Plan() would
// stay nil, which is the very gap this work closes.
//
// # Renderings
//
// Nothing here renders. The captured tree is an [exec.PlanNode], the same value
// [Engine.Profile] and [Engine.ProfileTable] render, so `exec.RenderPlanNode` on
// a [Result.Plan] prints byte for byte what [Engine.Explain] prints for the same
// statement. No second derivation of the plan exists in this file.

import (
	"context"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/cypher/parser"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// runPlanPrefixed executes a statement written with an EXPLAIN or PROFILE
// prefix and returns a [Result] carrying the plan.
//
// at is the caller's pinned read view when one exists (a statement of an
// explicit READ transaction, which must observe that transaction's instant) and
// nil otherwise, in which case this opens and releases its own snapshot exactly
// as [Engine.Run] does.
//
// It is called from every entry point that parses a statement, immediately after
// the semantic check, so a prefixed statement never reaches the write path.
func (e *Engine) runPlanPrefixed(
	ctx context.Context,
	entry *planCacheEntry,
	params map[string]expr.Value,
	at *pinnedView,
) (*Result, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	var (
		r   *Result
		err error
	)
	switch entry.planMode {
	case parser.PlanModeExplain:
		r, err = e.runExplainPrefixed(ctx, entry, params, at)
	case parser.PlanModeProfile:
		r, err = e.runProfilePrefixed(ctx, entry, params, at)
	default:
		// Unreachable: callers gate on planMode != PlanModeNone. Returned as an
		// error rather than panicking, per the module's fail-stop contract.
		return nil, fmt.Errorf("cypher: internal: runPlanPrefixed called with mode %d", entry.planMode)
	}
	if err != nil {
		return nil, err
	}
	// The plan-time advisories belong on a prefixed statement above all others:
	// seeing the Cartesian-product warning WITHOUT running the query is one of
	// the things EXPLAIN is for. They are attached here, once, so neither branch
	// can forget them — EXPLAIN did, and reported zero notifications for a query
	// whose un-prefixed run reported one.
	r.notifications = entry.notifications
	return r, nil
}

// runExplainPrefixed plans the statement and executes nothing.
//
// For a READING statement it builds the PHYSICAL operator tree the read path
// would build — [Engine.buildReadPhysical], the single build path, so the
// reported plan cannot disagree with the plan that runs — and captures it with
// [exec.PlanTree]. The tree is discarded without Init or Close: no operator
// acquires anything at construction time, and Init, which is what starts
// goroutines and allocates working buffers, is never called. This is what
// [Engine.explainPhysical] does, and for the same reasons.
//
// For a WRITING statement it captures the LOGICAL plan instead. A write's
// physical tree cannot be built without a live mutator (its operators bind to an
// open transaction), so there is nothing faithful to walk outside one — and
// opening one is precisely what EXPLAIN must not do. The captured tree is the
// same walk [Engine.ExplainLogical] and [Engine.ExplainTable] render, so all
// three agree.
//
// The Result carries the statement's own column signature and zero rows.
//
// Parameters are NOT required to be supplied. Planning reads a parameter's VALUE
// only where an access-path gate needs it, and a plan is a useful answer to
// "what would this run?" before the caller has bound anything — which is what
// [Engine.Explain] already does, and what Neo4j does. PROFILE, which executes,
// requires them like any other execution.
//
// The cost of that leniency, measured under rmp #2720 and pinned by
// TestExplainFidelity_UnboundParameterDivergesFromTheRun: where an access-path
// gate DOES need the value, an EXPLAIN without it renders a plan the run will not
// build. `EXPLAIN MATCH (n:P) WHERE n.age = $a RETURN n.name` with no parameters
// renders a NodeByLabelScan; the same statement run with $a bound builds a
// NodeByIndexRangeScan. Nothing in the rendered plan says the value was missing,
// so a reader is shown a full scan for a query that seeks. Every other condition
// tested — cold, on a plan-cache hit, and with the parameter bound — reproduces
// the executed tree exactly.
func (e *Engine) runExplainPrefixed(
	ctx context.Context,
	entry *planCacheEntry,
	params map[string]expr.Value,
	at *pinnedView,
) (*Result, error) {
	// A writing statement has no physical tree outside a transaction. Decided from
	// the plan the cache holds (entry.containsWrite), not by re-scanning the query
	// text, so the answer is the structural one.
	if entry.containsWrite {
		node := e.logicalPlanNode(entry, params)
		return newPlanResult(planColumns(entry.plan), &node, parser.PlanModeExplain), nil
	}

	queryReg := newNowAwareRegistry(e.reg, time.Now())
	// A SNAPSHOT, not the visibility barrier — the same choice
	// [Engine.explainPhysical] makes (rmp #2304): EXPLAIN reads the same label
	// cardinalities and index metadata Run reads to choose a plan, and Run takes
	// no lock. An explicit read transaction supplies its own instant instead.
	var snap *lpg.Snapshot
	if at != nil {
		snap = at.snap
	} else {
		snap = e.g.BeginRead()
		defer e.g.EndRead(snap)
	}
	op, cols, err := e.buildReadPhysical(ctx, entry, entry.plan, params, queryReg, nil, snap)
	if err != nil {
		return nil, fmt.Errorf("cypher: build plan: %w", err)
	}
	node := exec.PlanTree(op)
	return newPlanResult(cols, &node, parser.PlanModeExplain), nil
}

// runProfilePrefixed executes the statement with the profiling instrumentation
// installed and returns its rows together with the measured plan.
//
// It shares [Engine.profileMaterialised] with [Engine.Profile] and
// [Engine.ProfileTable], so all three describe one run of one plan built by one
// builder. The difference is only what happens to the rows: Profile drains and
// discards them because the measurements are its product, whereas here they ARE
// the result — Neo4j's PROFILE returns the query's rows, and a client that
// prefixed a statement with PROFILE still asked for its answer.
//
// A WRITING statement is refused rather than executed. This is the same refusal
// [Engine.Profile] documents and applies, kept identical on purpose so the
// Cypher surface and the Go surface cannot disagree: the profiling wrapper is
// installed by the READ builder, and a write's operators bind to a live mutator
// that builder does not create. Use EXPLAIN for a writing statement's plan, or
// run it without a prefix to execute it.
func (e *Engine) runProfilePrefixed(
	ctx context.Context,
	entry *planCacheEntry,
	params map[string]expr.Value,
	at *pinnedView,
) (*Result, error) {
	if entry.containsWrite {
		return nil, fmt.Errorf("cypher: PROFILE: refusing to execute a writing statement; " +
			"use EXPLAIN for its plan, or run the statement without a prefix to execute it")
	}
	if err := checkParamPresence(entry.paramRefs, params); err != nil {
		return nil, err
	}
	if err := checkParamTypesCached(entry, params); err != nil {
		return nil, err
	}
	r, node, err := e.profileMaterialised(ctx, entry, params, at)
	if err != nil {
		return nil, err
	}
	r.planNode = &node
	r.planMode = parser.PlanModeProfile
	return r, nil
}

// logicalPlanNode captures the LOGICAL plan as an [exec.PlanNode] tree.
//
// It drives the SAME walk [Engine.ExplainLogical] and [Engine.ExplainTable]
// drive — one [explainInputs.walk], with the index-seek substitutions, the
// count-store-gated reorderings and the cardinality estimates already applied —
// and rebuilds the tree structure from the depth each emitted line carries. It
// does NOT re-derive the plan from the IR: a second derivation is exactly the
// class of defect rmp #2222 removed, where a rendered plan named an access path
// the engine did not take.
func (e *Engine) logicalPlanNode(entry *planCacheEntry, params map[string]expr.Value) exec.PlanNode {
	var (
		root  exec.PlanNode
		have  bool
		stack []*exec.PlanNode // stack[d] is the node most recently opened at depth d
	)
	e.explainInputsFor(entry).walk(func(l planLine) {
		n := exec.PlanNode{Name: l.text}
		d := l.depth()
		if d == 0 || !have {
			root = n
			have = true
			stack = append(stack[:0], &root)
			return
		}
		// Guard against a depth that skips a level: attach to the deepest open
		// node rather than indexing out of range. The walk never produces one —
		// every line is emitted by a recursion that descends one level at a time —
		// but a renderer must not panic on a plan shape it did not expect.
		if d > len(stack) {
			d = len(stack)
		}
		parent := stack[d-1]
		parent.Children = append(parent.Children, n)
		child := &parent.Children[len(parent.Children)-1]
		stack = append(stack[:d], child)
	}, params)
	if !have {
		return exec.PlanNode{Name: "(empty)"}
	}
	return root
}

// depth reports how deep in the plan tree the line sits, with the root at 0.
//
// It is DERIVED from the two fields the walk already sets, rather than carried
// as a third: [explainWithIndexesNode] builds a child's prefix by appending its
// parent's continuation, which is "" at the root and exactly three runes
// ("   " or "│  ") at every other level, and gives the root — and only the
// root — an empty connector. So a line with an empty connector is the root, and
// any other line sits one level below the number of three-rune groups its
// prefix carries.
//
// Deriving it keeps the walk untouched: the tree and table renderers are
// unaffected by this file existing.
// TestPlanLineDepth_ReconstructsTheWalksOwnNesting pins the derivation against
// real plans of known shape, and TestPlanLineDepth_Formula against the rule
// itself, so a change to the walk's prefixes fails a test rather than silently
// mis-nesting a plan.
func (l *planLine) depth() int {
	if l.connector == "" {
		return 0
	}
	return utf8.RuneCountInString(l.prefix)/3 + 1
}

// planColumns returns the output column names of a logical plan.
//
// Every plan [ir.FromAST] produces is rooted at an [ir.ProduceResults], which
// names them. A plan that is not (none is today) yields no columns rather than a
// fabricated signature.
func planColumns(plan ir.LogicalPlan) []string {
	if pr, ok := plan.(*ir.ProduceResults); ok {
		return pr.Columns
	}
	return nil
}

// newPlanResult builds the zero-row [Result] an EXPLAIN returns: the statement's
// column signature, no rows, and the captured plan.
//
// It is materialised-and-empty rather than backed by a live operator, so
// [Result.Next] returns false without touching the graph — which is the
// no-execution guarantee expressed in the Result itself and not merely upheld by
// the code above.
func newPlanResult(cols []string, node *exec.PlanNode, mode parser.PlanMode) *Result {
	r := newResult(exec.Run(context.Background(), exec.NewArgument(), nil), cols, nil, nil, nil)
	r.matOn = true
	r.matRowLen = len(cols)
	r.matRecords = 0
	r.planNode = node
	r.planMode = mode
	return r
}

// Plan returns the query plan captured for a statement written with the EXPLAIN
// prefix, and nil for every other statement — including a PROFILE, whose
// measured plan is [Result.Profile].
//
// The split mirrors the Bolt summary a driver consumes: an EXPLAIN populates
// `plan` and a PROFILE populates `profile`, never both, so a caller reading
// Plan() knows the figures it holds are ESTIMATES the planner derived and not
// measurements of a run that happened.
//
// The returned tree is owned by the Result and must not be mutated. Render it
// with [exec.RenderPlanNode], which prints exactly what [Engine.Explain] prints
// for the same statement.
func (r *Result) Plan() *exec.PlanNode {
	if r.planMode != parser.PlanModeExplain {
		return nil
	}
	return r.planNode
}

// Profile returns the MEASURED query plan captured for a statement written with
// the PROFILE prefix, and nil for every other statement.
//
// Call it after the result has been fully consumed. The measurements are final
// as soon as the Result exists — the rows are materialised eagerly, under the
// read instant, before this method can be reached — so the tree is complete from
// the first call; the ordering advice is about the Bolt server, which publishes
// this tree in the SUCCESS that terminates the stream.
//
// Each node's Rows and Time are measured; Time is INCLUSIVE of the node's
// children. DbHits is derived at the access-path boundary. See [Engine.Profile]
// for the full contract, which this shares exactly.
//
// The returned tree is owned by the Result and must not be mutated.
func (r *Result) Profile() *exec.PlanNode {
	if r.planMode != parser.PlanModeProfile {
		return nil
	}
	return r.planNode
}
