// Package cypher provides the public query engine API for the GoGraph Cypher
// executor.
//
// # Usage
//
//	g := lpg.New[string, float64](adjlist.Config{})
//	// ... populate graph ...
//
//	engine := cypher.NewEngine(g)
//	result, err := engine.Run(ctx, "MATCH (n) RETURN n", nil)
//	if err != nil { ... }
//	defer result.Close()
//	for result.Next() {
//	    rec := result.Record()
//	    _ = rec
//	}
//
// # Plan cache
//
// Engine caches parsed and translated logical plans together with the
// semantic-analysis verdict in a bounded LRU keyed by the query string. The
// cached entry is a *planCacheEntry; the physical build step runs per
// Engine.Run call so that per-call executor state is fresh. Semantically
// invalid queries are also cached (with the typed error) so that repeated
// runs of the same bad query short-circuit without re-parsing.
//
// The default capacity is [DefaultPlanCacheCapacity] (1024 entries). Configure
// a different bound via [EngineOptions.PlanCacheCapacity] and the [NewEngineWithOptions]
// constructor. Eviction is least-recently-used and emits the
// cypher.plan_cache.evictions counter on the global metrics surface; hits and
// misses are reported under cypher.plan_cache.hits and
// cypher.plan_cache.misses.
//
// # Concurrency
//
// Engine is safe for concurrent use. Each Run call creates an independent
// physical operator tree. The plan cache itself serialises its structural
// updates on a single sync.Mutex; the cached *planCacheEntry is immutable
// once published, so callers operate on the returned pointer without further
// synchronisation.
package cypher

import (
	"context"
	"fmt"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/RoaringBitmap/roaring/v2/roaring64"

	"gograph/cypher/ast"
	"gograph/cypher/exec"
	"gograph/cypher/expr"
	"gograph/cypher/funcs"
	"gograph/cypher/ir"
	"gograph/cypher/parser"
	"gograph/cypher/procs"
	"gograph/cypher/sema"
	"gograph/graph"
	"gograph/graph/csr"
	"gograph/graph/index"
	"gograph/graph/lpg"
	cmetrics "gograph/internal/metrics"
	"gograph/store/txn"
)

// buildOpts carries the query-scope, optional state threaded through
// [buildOperator] alongside the per-call positional arguments. A nil
// *buildOpts is equivalent to the legacy build path: no SubqueryEvaluator,
// background context. Each Engine.Run / Engine.RunInTx invocation allocates
// its own *buildOpts so closures created during the build observe a stable
// per-run snapshot.
// edgeVarInfo records the schema columns emitted by an Expand operator for a
// named relationship variable. The triple (srcCol, edgeCol, dstCol) allows
// buildIRProjection to reconstruct a RelationshipValue from the raw
// IntegerValue columns in the executor row.
type edgeVarInfo struct {
	srcCol   int
	edgeCol  int
	dstCol   int
	edgeType string // first element of RelTypes, or empty
}

// pathVarInfo records the schema column that holds the flat alternating path
// list emitted by a VarLengthExpand operator for a named path variable. The
// listCol column contains an expr.ListValue of the form
//
//	[srcNodeID, edgePos0, dstNode0, edgePos1, dstNode1, ...]
//
// buildIRProjection uses this to reconstruct an expr.PathValue.
type pathVarInfo struct {
	listCol  int    // schema column holding the flat alternating ListValue
	edgeType string // first element of RelTypes, or empty
}

// pathChainStep describes one (relationship, destination-node) hop of a
// fixed-length named path. The (srcCol, edgeCol, dstCol) triplet matches the
// layout emitted by [exec.Expand]; edgeType is the relationship-type filter
// declared in the AST and acts as a fallback when the live-graph lookup
// cannot resolve one.
type pathChainStep struct {
	srcCol   int
	edgeCol  int
	dstCol   int
	edgeType string
}

// pathChainInfo describes a named path bound by a zero- or fixed-length
// (possibly chained) pattern. leadingCol is the schema column that carries
// the IntegerValue of the path's leading node. Each step extends the path by
// one relationship and one destination node, in document order.
// buildIRProjection consumes this to reconstruct an [expr.PathValue].
type pathChainInfo struct {
	leadingCol int
	steps      []pathChainStep
}

// vleRelInfo describes the schema column and edge-type filter for a
// VarLengthExpand relationship variable. The column holds a flat
// alternating ListValue [srcID, edgePos0, dst0, edgePos1, dst1, …];
// buildIRProjection extracts each (edgePos, dst) pair and reconstructs
// a RelationshipValue per hop so `RETURN r` for variable-length r
// projects [[:T], [:T], …] instead of the raw integer list.
type vleRelInfo struct {
	listCol  int
	edgeType string
}

type buildOpts struct {
	// subEval handles [ast.ExistsSubquery] and [ast.CountSubquery] expressions
	// encountered inside Filter/Project closures. May be nil; in that case
	// subquery expressions surface as [expr.EvalError] at runtime.
	subEval expr.SubqueryEvaluator
	// patEval handles [ast.PathPattern] existential predicates inside WHERE
	// clauses. May be nil; in that case pattern predicates surface as
	// [expr.EvalError] at runtime.
	patEval expr.PatternEvaluator
	// queryCtx is the context.Context attached to the enclosing Engine.Run
	// call. It is threaded into [expr.EvalWith] so subquery drives observe
	// cancellation and deadlines from the outer query.
	queryCtx context.Context //nolint:containedctx // per-query state owned by the buildOpts holder, not a long-lived field
	// writeFallback, when non-nil, is invoked by [buildOperator]'s default
	// branch on encountering a write IR node ([ir.CreateNode],
	// [ir.SetProperty], …). [buildPlanWithMutatorFull] sets it so that a
	// read wrapper such as [ir.Projection] above a write subtree (the
	// canonical lowering for `CREATE … RETURN`) recurses through the
	// write-aware planner rather than failing with
	// "unsupported IR node *ir.CreateNode". Leaving the field nil
	// preserves the original error for read-only call sites.
	writeFallback func(ir.LogicalPlan) (exec.Operator, error)
	// edgeVarMeta maps a relationship variable name (e.g. "r" from
	// `MATCH (a)-[r:R]->(b)`) to the triplet of schema columns that the Expand
	// operator places in each output row. Used by buildIRProjection to
	// reconstruct a RelationshipValue when the variable is projected directly.
	edgeVarMeta map[string]edgeVarInfo
	// pathVarMeta maps a named path variable (e.g. "p" from `MATCH p=(a)-[*]->(b)`)
	// to the schema column that the VarLengthExpand operator populates with a flat
	// alternating ListValue. Used by buildIRProjection to reconstruct a PathValue.
	pathVarMeta map[string]pathVarInfo
	// pathVarChain maps a named path variable bound by a zero- or
	// fixed-length pattern (no variable-length expansion) to the explicit
	// alternating triplet description emitted by the underlying Expand
	// chain. Used by buildIRProjection to reconstruct an expr.PathValue
	// without the flat ListValue encoding used by VarLengthExpand.
	pathVarChain map[string]pathChainInfo
	// expandTripletSeq is the ordered list of (srcCol, edgeCol, dstCol)
	// triplets registered by each [exec.Expand] operator as its physical
	// builder runs. A [*ir.NamedPath] wrapper above an Expand subtree
	// captures the slice length before recursing into its child, then
	// consumes the triplets appended during the child build to associate
	// IR chain elements with row slots.
	expandTripletSeq []pathChainStep
	// vleRelMeta maps a VarLengthExpand relationship variable name (e.g. "r"
	// from `MATCH (a)-[r*]->(b)`) to the flat-list column it occupies plus
	// the optional edge-type filter. Used by buildIRProjection to render the
	// variable as a list of RelationshipValues rather than the raw
	// alternating ListValue emitted by the VLE operator.
	vleRelMeta map[string]vleRelInfo
	// scalarCols is the set of schema variable names whose row values must NOT be
	// upgraded from IntegerValue(NodeID) to NodeValue. Aggregate output columns
	// (e.g. the output of count(*), sum, avg) are always scalars; their integer
	// values can coincide with real NodeIDs and must pass through as-is to prevent
	// mis-upgrading a count result into a graph node. buildEagerAggregation
	// populates this set for every aggregate output name it registers in the schema.
	scalarCols map[string]struct{}
}

// evalRow is the canonical bridge from a per-row closure to [expr.Eval] /
// [expr.EvalWith]. When bopts is non-nil and carries a SubqueryEvaluator or
// PatternEvaluator, [expr.EvalWith] is used so EXISTS/COUNT subquery dispatch
// and pattern predicate dispatch are enabled; otherwise the call degrades to
// [expr.Eval], preserving exact backward compatibility.
func evalRow(bopts *buildOpts, e ast.Expression, row expr.RowContext, params map[string]expr.Value, reg expr.FunctionRegistry) (expr.Value, error) {
	if bopts == nil || (bopts.subEval == nil && bopts.patEval == nil) {
		return expr.Eval(e, row, params, reg)
	}
	ctx := bopts.queryCtx
	if ctx == nil {
		ctx = context.Background()
	}
	return expr.EvalWith(ctx, e, row, params, reg, bopts.subEval, bopts.patEval)
}

// ─────────────────────────────────────────────────────────────────────────────
// Engine
// ─────────────────────────────────────────────────────────────────────────────

// EngineOptions configures an [Engine]. The zero value is valid: it selects
// the default function registry ([funcs.DefaultRegistry]), no WAL-backed
// store, and the default plan cache capacity ([DefaultPlanCacheCapacity]).
// Use [NewEngineWithOptions] to construct an Engine from this struct.
type EngineOptions struct {
	// Registry, when non-nil, overrides the default built-in function
	// registry used to resolve scalar function calls.
	Registry expr.FunctionRegistry

	// Store, when non-nil, binds the Engine to a WAL-enabled
	// [txn.Store]. The Engine's graph is taken from store.Graph()
	// when both Store and Graph fields are set; the explicit Graph
	// is then ignored. Run queries through [Engine.RunInTx] for
	// atomicity and WAL durability.
	Store *txn.Store[string, float64]

	// PlanCacheCapacity bounds the number of cached plans. Zero
	// selects [DefaultPlanCacheCapacity]; positive values override
	// it. A negative value is treated as misconfiguration and is
	// clamped to the default by the constructor.
	PlanCacheCapacity int
}

// Engine is the public query engine. It binds a graph, a function registry,
// and a plan cache, and exposes a single Run method for query execution.
//
// Engine is safe for concurrent use.
type Engine struct {
	g             *lpg.Graph[string, float64]
	store         *txn.Store[string, float64] // non-nil when WAL-backed
	reg           expr.FunctionRegistry
	constraintReg *exec.ConstraintRegistry
	procReg       *procs.Registry
	cache         *planCache
}

// NewEngine creates an Engine backed by g. The default built-in function
// registry ([funcs.DefaultRegistry]) and the default plan cache capacity
// ([DefaultPlanCacheCapacity]) are used. Use [NewEngineWithOptions] when a
// non-default function registry or plan cache capacity is required.
//
// If g has no [index.Manager] attached yet, NewEngine installs a new empty one
// so that DDL statements (CREATE INDEX / DROP INDEX) work out of the box.
func NewEngine(g *lpg.Graph[string, float64]) *Engine {
	return NewEngineWithOptions(g, EngineOptions{})
}

// NewEngineWithRegistry creates an Engine backed by g using a custom function
// registry and the default plan cache capacity.
//
// If g has no [index.Manager] attached yet, a new empty one is installed.
func NewEngineWithRegistry(g *lpg.Graph[string, float64], reg expr.FunctionRegistry) *Engine {
	return NewEngineWithOptions(g, EngineOptions{Registry: reg})
}

// NewEngineWithStore creates an Engine backed by a WAL-enabled [txn.Store]
// using the default plan cache capacity.
//
// All write queries routed through [Engine.RunInTx] use a single [txn.Tx] for
// atomicity and WAL durability: mutations are applied eagerly to the in-memory
// graph (so reads within the same transaction see the writes) and the WAL is
// fsynced on [Result.Close] when no pipeline error occurred.
//
// The underlying graph is taken from store.Graph(). If the graph has no
// [index.Manager] attached yet, a new empty one is installed.
func NewEngineWithStore(store *txn.Store[string, float64]) *Engine {
	return NewEngineWithOptions(store.Graph(), EngineOptions{Store: store})
}

// NewEngineWithOptions creates an Engine backed by g with explicit options.
// Zero-valued fields are filled with their documented defaults. When
// opts.Store is non-nil, the Engine is bound to that WAL-enabled
// [txn.Store] in addition to g.
//
// If g has no [index.Manager] attached yet, a new empty one is installed.
func NewEngineWithOptions(g *lpg.Graph[string, float64], opts EngineOptions) *Engine {
	ensureIndexManager(g)
	reg := opts.Registry
	if reg == nil {
		reg = funcs.DefaultRegistry
	}
	e := &Engine{
		g:             g,
		store:         opts.Store,
		reg:           reg,
		constraintReg: exec.NewConstraintRegistry(),
		procReg:       procs.NewRegistry(),
		cache:         newPlanCache(opts.PlanCacheCapacity),
	}
	procs.RegisterBuiltins(e.procReg, g.IndexManager(), func() [][]expr.Value {
		return e.constraintReg.ListConstraintRows()
	})
	return e
}

// ensureIndexManager installs a new empty index.Manager on g when none is
// present, so that DDL operators have a non-nil manager to work with.
func ensureIndexManager(g *lpg.Graph[string, float64]) {
	if g.IndexManager() == nil {
		g.SetIndexManager(index.NewManager())
	}
}

// ClearPlanCache drops every cached plan and increments the
// cypher.plan_cache.invalidations counter exactly once. It is the
// operator-facing invalidation hook installed on every DDL operator
// (CREATE/DROP INDEX, CREATE/DROP CONSTRAINT) — successful schema
// mutations call it so that subsequent queries re-plan against the
// new index / constraint topology rather than reusing stale plans
// built before the schema changed.
//
// ClearPlanCache is also safe to invoke directly as a user-facing
// manual reset (e.g. from operational tooling after an out-of-band
// index swap on the underlying graph).
//
// ClearPlanCache is idempotent and safe for concurrent use; each call
// emits exactly one invalidations counter increment regardless of the
// cache's prior size.
func (e *Engine) ClearPlanCache() {
	e.cache.clear()
}

// Procs returns the engine's procedure registry so callers can register
// custom procedures alongside the built-in db.* set. The returned
// [*procs.Registry] is the live, owning registry — mutations are
// observed immediately by every subsequent CALL <ns>.<name>() in any
// query parsed by this engine.
//
// Returned registry is non-nil. Safe for concurrent use; see
// [procs.Registry] for the concurrency contract.
func (e *Engine) Procs() *procs.Registry {
	return e.procReg
}

// Run executes query against the engine's graph and returns a streaming
// [Result]. The caller must call [Result.Close] when done.
//
// A wrapped [*parser.ParseError] is returned when the query has a syntax
// error; the error includes line and column information.
//
// Semantic violations detected by the scope analyser (undefined variables,
// variable-type conflicts, scope leaks) are returned as a [*sema.SemanticError]
// before plan execution begins — see [sema.MapToBolt] for the
// ErrorKind→Bolt mapping used. Callers may use [errors.As] to recover the
// typed error and inspect Category / SubType.
//
// Sprint 25 support: MATCH (full scan or label scan) + RETURN.
func (e *Engine) Run(ctx context.Context, query string, params map[string]expr.Value) (*Result, error) {
	// ── 0. DDL fast-path ─────────────────────────────────────────────────────
	if ir.IsDDL(query) {
		return e.runDDL(ctx, query)
	}

	// ── 1. Parse, analyse, and retrieve from plan cache ──────────────────────
	entry, err := e.parseAndAnalyse(query)
	if err != nil {
		return nil, err
	}

	// ── 1a. Sema fast-path: skip planning when scope violations were found ───
	if entry.semaErr != nil {
		return nil, entry.semaErr
	}
	plan := entry.plan

	// ── 1b. Parameter type check ─────────────────────────────────────────────
	if len(params) > 0 {
		if err := sema.CheckParams(sema.InferParamTypes(plan), params); err != nil {
			return nil, err
		}
	}

	// ── 2. Build physical operator tree ─────────────────────────────────────
	walker := &lpgNodeWalker{g: e.g}
	labelSrc := &lpgLabelResolver{g: e.g}
	// Allocate a per-run subquery evaluator so EXISTS { … } / COUNT { … }
	// expressions encountered inside Filter/Project closures can drive their
	// inner pipelines against the current outer row (task-396).
	subEval := newSubqueryEvaluator(walker, labelSrc, e.reg, e.g)
	// Allocate a per-run pattern evaluator so WHERE (a)-[:T]->(b) existential
	// predicates can be evaluated against the live graph (task-961).
	patEval := newPatternEvaluator(e.g)
	bopts := &buildOpts{subEval: subEval, patEval: patEval, queryCtx: ctx}
	op, cols, err := buildPlanEngine(plan, walker, labelSrc, e.reg, params, e.g.IndexManager(), e.procReg, bopts)
	if err != nil {
		return nil, fmt.Errorf("cypher: build plan: %w", err)
	}

	// ── 3. Wrap in streaming Result ──────────────────────────────────────────
	rs := exec.Run(ctx, op, cols)
	return newResult(rs, cols, nil, nil, nil), nil
}

// Explain returns a textual representation of the physical plan that would be
// chosen to execute query with the given params. The plan reflects current index
// availability: a hash index on the relevant (label, property) pair causes the
// relevant Selection+LabelScan subtree to appear as NodeByIndexSeek. No rows
// are produced; the graph is not modified.
//
// The format mirrors [ir.Explain] but annotates Selection→LabelScan pairs that
// would be rewritten to index seeks at execution time.
func (e *Engine) Explain(query string, params map[string]expr.Value) (string, error) {
	if ir.IsDDL(query) {
		return "(DDL — no query plan)", nil
	}
	plan, err := e.planFor(query)
	if err != nil {
		return "", err
	}
	return explainWithIndexes(plan, e.g.IndexManager(), params), nil
}

// explainWithIndexes walks plan and renders operator names, substituting
// Selection→{AllNodesScan|NodeByLabelScan} pairs with "NodeByIndexSeek" when
// tryBuildIndexSeekFromSelection would succeed given idxMgr and params.
func explainWithIndexes(plan ir.LogicalPlan, idxMgr *index.Manager, params map[string]expr.Value) string {
	var b strings.Builder
	explainWithIndexesNode(&b, plan, idxMgr, params, "", true, true)
	return b.String()
}

func explainWithIndexesNode(
	b *strings.Builder,
	plan ir.LogicalPlan,
	idxMgr *index.Manager,
	params map[string]expr.Value,
	prefix string,
	isRoot, isLast bool,
) {
	var connector, childCont string
	if isRoot {
		connector = ""
		childCont = ""
	} else if isLast {
		connector = "└─ "
		childCont = "   "
	} else {
		connector = "├─ "
		childCont = "│  "
	}

	// Check whether a Selection→scan rewrite would fire.
	opName := ir.OperatorName(plan)
	if sel, ok := plan.(*ir.Selection); ok && idxMgr != nil {
		schema := make(map[string]int)
		if op, fired, err := tryBuildIndexSeekFromSelection(sel, params, schema, idxMgr); err == nil && fired && op != nil {
			opName = "NodeByIndexSeek"
		}
	}

	b.WriteString(prefix)
	b.WriteString(connector)
	b.WriteString(opName)
	b.WriteByte('\n')

	// When a Selection was rewritten to an index seek, skip its scan child
	// (the child would be NodeByLabelScan which is subsumed by the seek).
	if opName == "NodeByIndexSeek" {
		return
	}

	children := plan.Children()
	nextPrefix := prefix + childCont
	for i, child := range children {
		explainWithIndexesNode(b, child, idxMgr, params, nextPrefix, false, i == len(children)-1)
	}
}

// runDDL executes a DDL statement (CREATE INDEX / DROP INDEX / …) eagerly.
// DDL operators emit no rows and are fully executed during runDDL — callers
// receive errors immediately rather than lazily during Result.Next.
func (e *Engine) runDDL(ctx context.Context, query string) (*Result, error) {
	ddlPlan, err := ir.ParseDDL(query)
	if err != nil {
		return nil, fmt.Errorf("cypher: DDL parse: %w", err)
	}
	idxMgr := e.g.IndexManager()
	var op exec.Operator
	switch p := ddlPlan.(type) {
	case *ir.CreateIndex:
		var kind exec.IndexKindExec
		switch p.Type {
		case ir.IndexTypeHash:
			kind = exec.ExecIndexHash
		case ir.IndexTypeBTree:
			kind = exec.ExecIndexBTree
		}
		op = exec.NewCreateIndexOp(p.Name, kind, p.IfNotExists, idxMgr, e.ClearPlanCache)
	case *ir.DropIndex:
		op = exec.NewDropIndexOp(p.Name, p.IfExists, idxMgr, e.ClearPlanCache)
	case *ir.CreateConstraint:
		var kind exec.ConstraintKind
		switch p.Kind {
		case ir.ConstraintUnique:
			kind = exec.ConstraintUnique
		case ir.ConstraintNotNull:
			kind = exec.ConstraintNotNull
		}
		op = exec.NewCreateConstraintOp(p.Name, p.Label, p.Property, kind, p.IfNotExists, idxMgr, e.constraintReg, e.ClearPlanCache)
	case *ir.DropConstraint:
		var kind exec.ConstraintKind
		switch p.Kind {
		case ir.ConstraintUnique:
			kind = exec.ConstraintUnique
		case ir.ConstraintNotNull:
			kind = exec.ConstraintNotNull
		}
		op = exec.NewDropConstraintOp(p.Name, p.Label, p.Property, kind, p.IfExists, idxMgr, e.constraintReg, e.ClearPlanCache)
	default:
		return nil, fmt.Errorf("cypher: unsupported DDL plan %T", ddlPlan)
	}
	// DDL operators emit zero rows; execute synchronously so errors surface at
	// Run time rather than lazily during Result.Next.
	if err := op.Init(ctx); err != nil {
		return nil, fmt.Errorf("cypher: DDL init: %w", err)
	}
	var dummy exec.Row
	if _, err := op.Next(&dummy); err != nil {
		_ = op.Close()
		return nil, err
	}
	if err := op.Close(); err != nil {
		return nil, fmt.Errorf("cypher: DDL close: %w", err)
	}
	return newResult(exec.Run(ctx, exec.NewArgument(), nil), nil, nil, nil, nil), nil
}

// RunAny executes query with params expressed as map[string]any, automatically
// converting Go native types to [expr.Value]. See [BindParams] for the
// supported conversions.
//
// RunAny auto-detects whether the query contains writing clauses (CREATE,
// MERGE, SET, REMOVE, DELETE, DETACH DELETE) and routes through
// [Engine.RunInTx] when so, or [Engine.Run] otherwise. Callers that need
// an explicit choice should invoke [Engine.Run] / [Engine.RunInTx]
// directly.
func (e *Engine) RunAny(ctx context.Context, query string, params map[string]any) (*Result, error) {
	converted, err := BindParams(params)
	if err != nil {
		return nil, err
	}
	if queryHasWritingClause(query) {
		return e.RunInTx(ctx, query, converted)
	}
	return e.Run(ctx, query, converted)
}

// queryHasWritingClause reports whether the query string contains any
// writing keyword (CREATE, MERGE, SET, REMOVE, DELETE, DETACH) outside a
// DDL prefix. This is a textual heuristic: it avoids triggering the
// plan-cache machinery on a second pass, which would otherwise double-
// count hits and misses in concurrency tests.
//
// The heuristic is intentionally permissive — false positives (writing
// keywords inside string literals or backtick identifiers) merely cause a
// read-only query to be routed through RunInTx, which executes identical
// semantics with the same correctness guarantees, only with the cost of
// opening and committing a write transaction.
func queryHasWritingClause(query string) bool {
	if ir.IsDDL(query) {
		return false
	}
	return writingKeywordRE.MatchString(query)
}

// writingKeywordRE matches any writing-clause keyword as a standalone word.
// The pattern uses a case-insensitive flag and a word boundary anchor so
// fragments like "PRESET" or "NOMERGE" are not falsely classified.
//
//nolint:gochecknoglobals // singleton regex compiled once at init
var writingKeywordRE = regexp.MustCompile(`(?i)\b(CREATE|MERGE|SET|REMOVE|DELETE|DETACH)\b`)

// RunInTxAny executes a write query with params expressed as map[string]any,
// automatically converting Go native types to [expr.Value]. See [BindParams].
func (e *Engine) RunInTxAny(ctx context.Context, query string, params map[string]any) (*Result, error) {
	converted, err := BindParams(params)
	if err != nil {
		return nil, err
	}
	return e.RunInTx(ctx, query, converted)
}

// BindParams converts a map[string]any to map[string]expr.Value using the
// following type mapping:
//
//   - nil                       → expr.Null
//   - bool                      → expr.BoolValue
//   - int, int8, int16, int32, int64 → expr.IntegerValue
//   - uint, uint8, uint16, uint32, uint64 → expr.IntegerValue (truncated to int64)
//   - float32, float64          → expr.FloatValue
//   - string                    → expr.StringValue
//   - []any                     → expr.ListValue (recursively converted)
//   - map[string]any            → expr.MapValue (recursively converted)
//   - expr.Value                → passed through unchanged
//
// Returns an error for unsupported types.
func BindParams(params map[string]any) (map[string]expr.Value, error) {
	if len(params) == 0 {
		return nil, nil
	}
	out := make(map[string]expr.Value, len(params))
	for k, v := range params {
		converted, err := bindAny(v)
		if err != nil {
			return nil, fmt.Errorf("cypher: BindParams: key %q: %w", k, err)
		}
		out[k] = converted
	}
	return out, nil
}

// bindAny converts a single Go value to an expr.Value.
// Numeric types (int*, uint*, float*) are handled by bindNumeric.
func bindAny(v any) (expr.Value, error) {
	if v == nil {
		return expr.Null, nil
	}
	switch t := v.(type) {
	case expr.Value:
		return t, nil
	case bool:
		return expr.BoolValue(t), nil
	case string:
		return expr.StringValue(t), nil
	case []any:
		list := make(expr.ListValue, len(t))
		for i, elem := range t {
			converted, err := bindAny(elem)
			if err != nil {
				return nil, fmt.Errorf("list[%d]: %w", i, err)
			}
			list[i] = converted
		}
		return list, nil
	case map[string]any:
		m := make(expr.MapValue, len(t))
		for k, elem := range t {
			converted, err := bindAny(elem)
			if err != nil {
				return nil, fmt.Errorf("map[%q]: %w", k, err)
			}
			m[k] = converted
		}
		return m, nil
	default:
		if num, ok := bindNumeric(v); ok {
			return num, nil
		}
		return nil, fmt.Errorf("unsupported parameter type %T", v)
	}
}

// bindNumeric converts Go numeric primitives to expr.Value.
// Returns (value, true) on match, (nil, false) if v is not a numeric type.
func bindNumeric(v any) (expr.Value, bool) {
	switch t := v.(type) {
	case int:
		return expr.IntegerValue(int64(t)), true
	case int8:
		return expr.IntegerValue(int64(t)), true
	case int16:
		return expr.IntegerValue(int64(t)), true
	case int32:
		return expr.IntegerValue(int64(t)), true
	case int64:
		return expr.IntegerValue(t), true
	case uint:
		return expr.IntegerValue(int64(t)), true //nolint:gosec // intentional truncation documented
	case uint8:
		return expr.IntegerValue(int64(t)), true
	case uint16:
		return expr.IntegerValue(int64(t)), true
	case uint32:
		return expr.IntegerValue(int64(t)), true
	case uint64:
		return expr.IntegerValue(int64(t)), true //nolint:gosec // intentional truncation documented
	case float32:
		return expr.FloatValue(float64(t)), true
	case float64:
		return expr.FloatValue(t), true
	default:
		return nil, false
	}
}

// planCacheEntry is the value stored in [Engine.cache] for a successfully
// parsed query. It bundles the translated logical plan with the semantic
// analyser's verdict so that both lookups (planFor, semaCheckCached) hit a
// single cache slot.
//
// The semaErr field is non-nil when [sema.Analyse] reported violations;
// callers consult it before plan execution to short-circuit with a typed
// [*sema.SemanticError]. The logical plan is still cached even when semaErr
// is non-nil so that [Engine.Explain] can render the plan tree for
// diagnostic purposes without re-parsing.
type planCacheEntry struct {
	plan    ir.LogicalPlan
	semaErr *sema.SemanticError
}

// planFor returns the cached logical plan for query, or parses, translates,
// and caches it. Semantic-analysis failures are NOT surfaced from planFor:
// callers should invoke [Engine.semaCheckCached] separately so they can
// decide whether to fast-fail before plan execution (Run/RunInTx) or to
// still render an Explain tree.
func (e *Engine) planFor(query string) (ir.LogicalPlan, error) {
	entry, err := e.parseAndAnalyse(query)
	if err != nil {
		return nil, err
	}
	return entry.plan, nil
}

// parseAndAnalyse parses, runs the scope analyser, and translates query into
// a logical plan. The full result (plan + sema verdict) is cached so that
// subsequent calls with the same query string skip every stage above plan
// execution.
//
// A non-nil error is returned only for parse or translation failures; a
// semantically invalid (but parseable) query yields a cache entry whose
// semaErr field is set, and parseAndAnalyse returns (entry, nil).
func (e *Engine) parseAndAnalyse(query string) (*planCacheEntry, error) {
	if v, ok := e.cache.get(query); ok {
		return v, nil
	}
	astNode, err := parser.Parse(query)
	if err != nil {
		return nil, fmt.Errorf("cypher: parse: %w", err)
	}
	semaErr := sema.MapToBolt(sema.Analyse(astNode))
	plan, err := ir.FromAST(astNode)
	if err != nil {
		return nil, fmt.Errorf("cypher: translate: %w", err)
	}
	entry := &planCacheEntry{plan: plan, semaErr: semaErr}
	actual, _ := e.cache.loadOrStore(query, entry)
	return actual, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Result
// ─────────────────────────────────────────────────────────────────────────────

// Result is a forward-only streaming result set returned by [Engine.Run] /
// [Engine.RunInTx]. It wraps [exec.ResultSet] and exposes the same iterator
// contract.
//
// # Lifecycle contract
//
// Every Result returned from a successful Run/RunInTx call MUST be closed
// by the caller via [Result.Close], even if [Result.Err] is non-nil and even
// if the caller stops iterating before exhaustion. Close releases the
// physical operator tree, drains any goroutines spawned by parallel operators,
// commits or rolls back buffered index mutations for write queries, and (for
// WAL-backed engines) fsyncs the WAL or rolls the transaction back.
//
// The typical pattern is:
//
//	res, err := engine.Run(ctx, query, params)
//	if err != nil {
//	    return err
//	}
//	defer res.Close()
//	for res.Next() {
//	    rec := res.Record()
//	    // ... consume rec ...
//	}
//	return res.Err()
//
// # Safety net
//
// Result installs a [runtime.SetFinalizer] that detects callers who forget
// to Close. When the garbage collector reclaims an unclosed Result, the
// finalizer:
//
//  1. Increments the metric "cypher.result.leaked" so operators see the
//     incidence count in their monitoring; and
//  2. Best-effort closes the underlying resources to limit damage on a
//     long-running server.
//
// The finalizer is a fail-stop diagnostic, NOT a substitute for an explicit
// Close. In particular, the finalizer runs at an unpredictable time after
// the leak (it depends on the GC schedule) and CANNOT report errors back to
// the caller. WAL-backed write transactions held open until finalisation
// still commit lazily — a window during which other writers may be blocked
// on the store mutex. Callers that need predictable resource release MUST
// call Close themselves.
//
// Result is NOT safe for concurrent use.
type Result struct {
	rs     *exec.ResultSet
	cols   []string
	buf    *exec.IndexBuffer        // non-nil only for RunInTx results
	idxMgr *index.Manager           // non-nil only when buf != nil
	tx     *txn.Tx[string, float64] // non-nil only for WAL-backed RunInTx results

	closed atomic.Bool // tripped by Close; checked by the finalizer
}

// Next advances to the next result row. Returns true when a row is available.
func (r *Result) Next() bool { return r.rs.Next() }

// Record returns the current row as a map from column name to value.
// Must only be called after a successful [Next].
func (r *Result) Record() exec.Record { return r.rs.Record() }

// Err returns the first error encountered during iteration, or nil.
func (r *Result) Err() error { return r.rs.Err() }

// Columns returns the ordered list of output column names.
func (r *Result) Columns() []string { return r.cols }

// IsClosed reports whether Close has been called on this Result.
func (r *Result) IsClosed() bool { return r.closed.Load() }

// Close releases all resources held by the result set.
// When the result was created by [Engine.RunInTx], Close also:
//  1. Commits or rolls back buffered index changes (always).
//  2. When the engine is WAL-backed ([NewEngineWithStore]), WAL-syncs the
//     buffered ops via [txn.Tx.CommitWALOnly] on success, or calls
//     [txn.Tx.Rollback] on error. Mutations have already been applied to the
//     in-memory graph eagerly; CommitWALOnly only persists them to the WAL.
//
// Close is idempotent: a second invocation returns nil without re-entering
// the underlying ResultSet. The finalizer safety net also relies on this
// idempotence — see the type-level documentation.
func (r *Result) Close() error {
	if !r.closed.CompareAndSwap(false, true) {
		return nil
	}
	// Disarm the finalizer: we are about to release the resources ourselves
	// and there is no point in the GC calling us back later.
	runtime.SetFinalizer(r, nil)
	return r.closeLocked()
}

// closeLocked performs the actual resource release. Callers must hold the
// closed flag (set via CompareAndSwap by [Result.Close] or by the finalizer)
// to ensure exactly-once semantics across both call sites.
func (r *Result) closeLocked() error {
	err := r.rs.Close()
	if r.buf != nil {
		if err != nil || r.rs.Err() != nil {
			r.buf.Rollback()
		} else {
			r.buf.Commit(r.idxMgr)
		}
	}
	if r.tx != nil {
		if err != nil || r.rs.Err() != nil {
			_ = r.tx.Rollback() // release store mutex; in-memory state already dirty
		} else {
			if werr := r.tx.CommitWALOnly(); werr != nil {
				err = werr
			}
		}
	}
	return err
}

// newResult wraps the construction of every Result returned by Run/RunInTx,
// installing the leak-detection finalizer on the freshly built value. The
// finalizer is the only safety net against callers that forget Close; it
// emits cypher.result.leaked and performs a best-effort release.
//
// When cols is empty the query has no RETURN clause and the caller cares
// only about side effects (CREATE/SET/DELETE/MERGE/REMOVE without a
// trailing projection). In that case newResult drains the underlying
// [exec.ResultSet] eagerly so the writes execute and the iterator becomes
// immediately exhausted — TCK-conformant write-only semantics.
func newResult(rs *exec.ResultSet, cols []string, buf *exec.IndexBuffer, idxMgr *index.Manager, tx *txn.Tx[string, float64]) *Result {
	r := &Result{rs: rs, cols: cols, buf: buf, idxMgr: idxMgr, tx: tx}
	if len(cols) == 0 {
		for rs.Next() {
			// discard the row; write side effects execute as a side effect
		}
	}
	runtime.SetFinalizer(r, finalizeResult)
	return r
}

// finalizeResult is the runtime.SetFinalizer callback invoked by the GC
// when an unclosed Result becomes unreachable. It increments the leak
// counter and runs the same close path Close() would, ignoring its error
// (the caller is no longer there to receive it). See [Result] for the full
// contract.
func finalizeResult(r *Result) {
	if !r.closed.CompareAndSwap(false, true) {
		return
	}
	cmetrics.IncCounter("cypher.result.leaked", 1)
	_ = r.closeLocked()
}

// ─────────────────────────────────────────────────────────────────────────────
// Graph adapters
// ─────────────────────────────────────────────────────────────────────────────

// lpgNodeWalker adapts *lpg.Graph[string, float64] to the exec.nodeWalker
// interface expected by [exec.AllNodesScan].
type lpgNodeWalker struct {
	g *lpg.Graph[string, float64]
}

// WalkNodeIDs implements nodeWalkerIface.
func (w *lpgNodeWalker) WalkNodeIDs(fn func(graph.NodeID) bool) {
	w.g.AdjList().Mapper().Walk(func(id graph.NodeID, _ string) bool {
		return fn(id)
	})
}

// lpgLabelResolver adapts *lpg.Graph[string, float64] to the exec.labelResolver
// interface expected by [exec.NodeByLabelScan].
type lpgLabelResolver struct {
	g *lpg.Graph[string, float64]
}

// ResolveLabelBitmap implements exec.labelResolver.
func (s *lpgLabelResolver) ResolveLabelBitmap(name string) *roaring64.Bitmap {
	lid, ok := s.g.Registry().Lookup(name)
	if !ok {
		return roaring64.New()
	}
	return s.g.NodeIndex().Intersect(uint32(lid))
}

// ─────────────────────────────────────────────────────────────────────────────
// BuildPlan — IR → physical operator tree
// ─────────────────────────────────────────────────────────────────────────────

// nodeWalkerIface is the minimal interface needed from a node source.
type nodeWalkerIface interface {
	WalkNodeIDs(fn func(graph.NodeID) bool)
}

// labelResolverIface is the interface for label bitmap resolution, matching
// exec.labelResolver without importing the unexported type.
type labelResolverIface interface {
	ResolveLabelBitmap(name string) *roaring64.Bitmap
}

// BuildPlanWithMutator converts an IR [ir.LogicalPlan] tree into a physical
// [exec.Operator] tree, supporting both read and write IR operators. The
// mutator provides the write surface for CREATE, SET, REMOVE, DELETE, and
// MERGE operators.
//
// For read-only plans the behaviour is identical to [BuildPlan]; the mutator
// is only invoked when a write IR node is encountered.
func BuildPlanWithMutator(
	plan ir.LogicalPlan,
	walker nodeWalkerIface,
	labelSrc labelResolverIface,
	reg expr.FunctionRegistry,
	params map[string]expr.Value,
	mutator exec.GraphMutator,
) (op exec.Operator, cols []string, err error) {
	return buildPlanWithMutatorFull(plan, walker, labelSrc, reg, params, mutator, nil, nil)
}

// buildPlanWithMutatorFull is the engine-internal variant of
// BuildPlanWithMutator that also threads constraint enforcement through write
// operators. constraintReg and idxMgr may both be nil (no enforcement).
func buildPlanWithMutatorFull(
	plan ir.LogicalPlan,
	walker nodeWalkerIface,
	labelSrc labelResolverIface,
	reg expr.FunctionRegistry,
	params map[string]expr.Value,
	mutator exec.GraphMutator,
	constraintReg *exec.ConstraintRegistry,
	idxMgr *index.Manager,
) (op exec.Operator, cols []string, err error) {
	schema := make(map[string]int)
	argByTag := make(map[uint32]*exec.Argument)

	// bopts carries the writeFallback closure that lets read-side operator
	// builders (Projection / Selection / Sort / Limit / EagerAggregation
	// over a write subtree) recurse back into [buildOperatorWrite] when
	// they encounter a write IR node. Without this, the canonical lowering
	// of `CREATE (n) RETURN n.x` — ProduceResults → Projection → CreateNode
	// — falls through to [buildOperator]'s default branch and errors with
	// "unsupported IR node *ir.CreateNode".
	bopts := &buildOpts{}
	bopts.writeFallback = func(child ir.LogicalPlan) (exec.Operator, error) {
		return buildOperatorWrite(child, walker, labelSrc, reg, params, schema, mutator, constraintReg, idxMgr, argByTag, bopts)
	}

	// When the IR root is a ProduceResults, use its declared columns; otherwise
	// treat the plan as a write-only query with no output columns. A CREATE
	// without RETURN has a write operator as root.
	if pr, ok := plan.(*ir.ProduceResults); ok {
		cols = pr.Columns
		child, buildErr := buildOperatorWrite(pr.Child, walker, labelSrc, reg, params, schema, mutator, constraintReg, idxMgr, argByTag, bopts)
		if buildErr != nil {
			return nil, nil, buildErr
		}
		items := make([]exec.ProjectionItem, len(cols))
		for i, col := range cols {
			if colIdx, exists := schema[col]; exists {
				idx := colIdx
				items[i] = exec.ProjectionItem{
					Alias: col,
					Eval: func(row exec.Row) (expr.Value, error) {
						if idx < len(row) {
							return row[idx], nil
						}
						return expr.Null, nil
					},
				}
			} else {
				items[i] = exec.ProjectionItem{
					Alias: col,
					Eval:  func(_ exec.Row) (expr.Value, error) { return expr.Null, nil },
				}
			}
		}
		proj, projErr := exec.NewProject(child, items)
		if projErr != nil {
			return nil, nil, fmt.Errorf("cypher: build final projection: %w", projErr)
		}
		return proj, cols, nil
	}

	// Write-only query (no RETURN clause): build the write operator tree
	// directly and wrap in a single pass-through projection so the result
	// set can be drained to trigger side effects.
	child, buildErr := buildOperatorWrite(plan, walker, labelSrc, reg, params, schema, mutator, constraintReg, idxMgr, argByTag, bopts)
	if buildErr != nil {
		return nil, nil, buildErr
	}
	// Emit a single synthetic "__write__" column so the result set is non-empty
	// and can be drained. Callers that only care about side effects can ignore
	// the column.
	cols = nil // no output columns for write-only queries
	return child, cols, nil
}

// buildOperatorWrite extends buildOperator with handling for write IR nodes.
// When mutator is nil, write nodes fall through to the default error case.
// constraintReg and idxMgr are optional (nil means no enforcement).
//
// argByTag is forwarded to buildOperator for [*ir.Argument] resolution; pass
// nil when no Apply-family operator is in scope.
//
//nolint:gocyclo // large switch — one case per write IR node, no hidden branches
func buildOperatorWrite(
	plan ir.LogicalPlan,
	walker nodeWalkerIface,
	labelSrc labelResolverIface,
	reg expr.FunctionRegistry,
	params map[string]expr.Value,
	schema map[string]int,
	mutator exec.GraphMutator,
	constraintReg *exec.ConstraintRegistry,
	idxMgr *index.Manager,
	argByTag map[uint32]*exec.Argument,
	bopts *buildOpts,
) (exec.Operator, error) {
	if plan == nil {
		// A nil plan arises when a write clause has no driving subplan (e.g.
		// CREATE without a leading MATCH). Return a single-row operator that
		// drives the write operator exactly once.
		return exec.NewSingleRowOperator(), nil
	}

	switch p := plan.(type) {

	case *ir.CreateNode:
		child, err := buildOperatorWrite(p.Child, walker, labelSrc, reg, params, schema, mutator, constraintReg, idxMgr, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		// Snapshot schema before adding NodeVar: the propsExprFn must see the
		// input row layout (which does not include the newly created node).
		propsSchema := copySchema(schema)
		if p.NodeVar != "" {
			schema[p.NodeVar] = schemaWidth(schema)
		}
		cn, buildErr := exec.NewCreateNode(p.NodeVar, p.Labels, p.Properties, child, mutator)
		if buildErr != nil {
			return nil, buildErr
		}
		if len(params) > 0 {
			if cn, buildErr = cn.WithParams(params); buildErr != nil {
				return nil, buildErr
			}
		}
		if p.PropertiesExpr != nil {
			if ml, ok := p.PropertiesExpr.(*ast.MapLiteral); ok {
				if fn := buildPropsEvalFn(ml, propsSchema, params, reg, mutator); fn != nil {
					cn.WithPropsEvalFn(fn)
				}
			}
		}
		if constraintReg != nil {
			cn.WithConstraints(constraintReg, idxMgr)
		}
		return cn, nil

	case *ir.CreateRelationship:
		child, err := buildOperatorWrite(p.Child, walker, labelSrc, reg, params, schema, mutator, constraintReg, idxMgr, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		if p.RelVar != "" {
			schema[p.RelVar] = schemaWidth(schema)
		}
		// Pass a copy of schema so the operator captures the current state.
		schemaCopy := make(map[string]int, len(schema))
		for k, v := range schema {
			schemaCopy[k] = v
		}
		cr, buildErr := exec.NewCreateRelationship(p.StartVar, p.EndVar, p.RelVar, p.RelType, p.Properties, schemaCopy, child, mutator)
		if buildErr != nil {
			return nil, buildErr
		}
		if len(params) > 0 {
			if cr, buildErr = cr.WithParams(params); buildErr != nil {
				return nil, buildErr
			}
		}
		if p.PropertiesExpr != nil {
			if ml, ok := p.PropertiesExpr.(*ast.MapLiteral); ok {
				// Use schemaCopy (which captures relationship endpoints) for
				// property expression evaluation.
				relPropsSchema := copySchema(schemaCopy)
				if p.RelVar != "" {
					delete(relPropsSchema, p.RelVar)
				}
				if fn := buildPropsEvalFn(ml, relPropsSchema, params, reg, mutator); fn != nil {
					cr.WithPropsEvalFn(fn)
				}
			}
		}
		return cr, nil

	case *ir.SetProperty:
		child, err := buildOperatorWrite(p.Child, walker, labelSrc, reg, params, schema, mutator, constraintReg, idxMgr, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		schemaCopy := copySchema(schema)
		sp, buildErr := exec.NewSetProperty(p.EntityVar, p.PropertyKey, p.Value, schemaCopy, child, mutator)
		if buildErr != nil {
			return nil, buildErr
		}
		if len(params) > 0 {
			sp.WithParams(params)
		}
		if constraintReg != nil {
			sp.WithConstraints(constraintReg, idxMgr)
		}
		if bopts != nil {
			if info, isRel := bopts.edgeVarMeta[p.EntityVar]; isRel {
				sp.WithRelCols(exec.RelCols{SrcCol: info.srcCol, DstCol: info.dstCol})
			}
		}
		// AST-eval path for non-literal RHS expressions (SET n.num = n.num + 1,
		// SET n.name = a.name, etc.). When the IR carries a parsed expression,
		// build a per-row closure that evaluates it and converts the result to
		// an lpg.PropertyValue. The closure short-circuits to ('no value') for
		// expr.Eval errors or unsupported value kinds so the operator skips
		// the write without aborting the pipeline.
		if p.ValueExpr != nil && p.PropertyKey != "" {
			schemaSnap := schemaCopy
			capturedExpr := p.ValueExpr
			capturedParams := params
			capturedReg := reg
			capturedBopts := bopts
			var capturedG *lpg.Graph[string, float64]
			if lw, ok := walker.(*lpgNodeWalker); ok {
				capturedG = lw.g
			}
			sp.WithValueEvalFn(func(row exec.Row) (lpg.PropertyValue, bool, bool, error) {
				rowCtx := buildRowCtx(row, schemaSnap, capturedG, capturedBopts)
				v, evalErr := evalRow(capturedBopts, capturedExpr, rowCtx, capturedParams, capturedReg)
				if evalErr != nil {
					return lpg.PropertyValue{}, false, false, nil // surface as no-op
				}
				if v == nil || expr.IsNull(v) {
					return lpg.PropertyValue{}, true, false, nil
				}
				pv, ok := exprValueToLPGProp(v)
				if !ok {
					return lpg.PropertyValue{}, false, false, nil
				}
				return pv, false, true, nil
			})
		}
		return sp, nil

	case *ir.SetAllProperties:
		child, err := buildOperatorWrite(p.Child, walker, labelSrc, reg, params, schema, mutator, constraintReg, idxMgr, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		schemaCopy := copySchema(schema)
		var sap *exec.SetAllProperties
		switch {
		case p.SourceVar != "":
			sap = exec.NewSetAllPropertiesFromEntity(p.EntityVar, p.SourceVar, p.IsReplace, schemaCopy, child, mutator)
		case p.ParamName != "":
			sap = exec.NewSetAllPropertiesFromParam(p.EntityVar, p.ParamName, p.IsReplace, schemaCopy, child, mutator)
		default:
			var bErr error
			sap, bErr = exec.NewSetAllPropertiesFromMap(p.EntityVar, p.MapLiteral, p.IsReplace, schemaCopy, child, mutator)
			if bErr != nil {
				return nil, bErr
			}
		}
		if len(params) > 0 {
			var pErr error
			sap, pErr = sap.WithParams(params)
			if pErr != nil {
				return nil, pErr
			}
		}
		if constraintReg != nil {
			sap.WithConstraints(constraintReg, idxMgr)
		}
		if bopts != nil {
			if info, isRel := bopts.edgeVarMeta[p.EntityVar]; isRel {
				sap.WithRelCols(exec.RelCols{SrcCol: info.srcCol, DstCol: info.dstCol})
			}
			if p.SourceVar != "" {
				if info, isRel := bopts.edgeVarMeta[p.SourceVar]; isRel {
					sap.WithSourceRelCols(exec.RelCols{SrcCol: info.srcCol, DstCol: info.dstCol})
				}
			}
		}
		return sap, nil

	case *ir.SetLabels:
		child, err := buildOperatorWrite(p.Child, walker, labelSrc, reg, params, schema, mutator, constraintReg, idxMgr, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		schemaCopy := copySchema(schema)
		return exec.NewSetLabels(p.NodeVar, p.Labels, schemaCopy, child, mutator), nil

	case *ir.RemoveProperty:
		child, err := buildOperatorWrite(p.Child, walker, labelSrc, reg, params, schema, mutator, constraintReg, idxMgr, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		schemaCopy := copySchema(schema)
		rp := exec.NewRemoveProperty(p.EntityVar, p.PropertyKey, schemaCopy, child, mutator)
		if bopts != nil {
			if info, isRel := bopts.edgeVarMeta[p.EntityVar]; isRel {
				rp.WithRelCols(exec.RelCols{SrcCol: info.srcCol, DstCol: info.dstCol})
			}
		}
		return rp, nil

	case *ir.RemoveLabels:
		child, err := buildOperatorWrite(p.Child, walker, labelSrc, reg, params, schema, mutator, constraintReg, idxMgr, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		schemaCopy := copySchema(schema)
		return exec.NewRemoveLabels(p.NodeVar, p.Labels, schemaCopy, child, mutator), nil

	case *ir.DeleteNode:
		child, err := buildOperatorWrite(p.Child, walker, labelSrc, reg, params, schema, mutator, constraintReg, idxMgr, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		schemaCopy := copySchema(schema)
		return exec.NewDeleteNode(p.NodeVar, schemaCopy, child, mutator), nil

	case *ir.DeleteRelationship:
		child, err := buildOperatorWrite(p.Child, walker, labelSrc, reg, params, schema, mutator, constraintReg, idxMgr, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		schemaCopy := copySchema(schema)
		return exec.NewDeleteRelationship(p.RelVar, schemaCopy, child, mutator), nil

	case *ir.DetachDelete:
		child, err := buildOperatorWrite(p.Child, walker, labelSrc, reg, params, schema, mutator, constraintReg, idxMgr, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		schemaCopy := copySchema(schema)
		return exec.NewDetachDelete(p.NodeVar, schemaCopy, child, mutator), nil

	case *ir.Merge:
		child, err := buildOperatorWrite(p.Child, walker, labelSrc, reg, params, schema, mutator, constraintReg, idxMgr, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		// Extract labels and properties from the pattern string. For the
		// current IR the pattern is an opaque string; we surface the bound vars
		// as the output schema columns.
		for _, v := range p.BoundVars {
			if v != "" {
				schema[v] = schemaWidth(schema)
			}
		}
		schemaCopy := copySchema(schema)
		labels, props := parseNodePatternStr(p.Pattern)
		// Real MERGE search (T930): scan the live graph for a node whose
		// labels are a superset of `labels` AND whose properties equal every
		// (key, value) parsed from `props`. When matches are found the ON
		// MATCH branch fires; only zero matches drives the ON CREATE branch.
		// Single-writer serialisation in the engine keeps concurrent MERGE
		// callers from racing to a phantom zero-match result.
		searchFn, sfErr := exec.NewMergeSearchFnFromPattern(labels, props, params, mutator)
		if sfErr != nil {
			return nil, sfErr
		}
		m, buildErr := exec.NewMerge(
			firstVar(p.BoundVars),
			labels,
			props,
			p.OnCreate, p.OnMatch,
			searchFn,
			schemaCopy,
			child,
			mutator,
		)
		if buildErr != nil {
			return nil, buildErr
		}
		if len(params) > 0 {
			if m, buildErr = m.WithParams(params); buildErr != nil {
				return nil, buildErr
			}
		}
		if constraintReg != nil {
			m.WithConstraints(constraintReg, idxMgr)
		}
		return m, nil

	default:
		// Fall through to the read-operator builder.
		// procReg is nil here because buildOperatorWrite is only called from the
		// write path (buildPlanWithMutatorFull) which does not thread procReg.
		return buildOperator(plan, walker, labelSrc, reg, params, schema, idxMgr, nil, argByTag, bopts)
	}
}

// copySchema returns a shallow copy of the schema map.
func copySchema(schema map[string]int) map[string]int {
	cp := make(map[string]int, len(schema))
	for k, v := range schema {
		cp[k] = v
	}
	return cp
}

// schemaWidth returns the actual row width implied by schema: the maximum
// column index present plus one. This is the correct "next available column
// index" to use when appending a new column to the row.
//
// Using len(schema) directly is incorrect when buildIRProjection has registered
// secondary expression-string keys (e.g. schema["[1,2,3]"] == schema["lst"] ==
// 0 for "WITH [1,2,3] AS lst"), because those secondary entries inflate
// len(schema) without adding real row columns.
func schemaWidth(schema map[string]int) int {
	max := -1
	for _, idx := range schema {
		if idx > max {
			max = idx
		}
	}
	return max + 1
}

// buildPropsEvalFn constructs a [exec.PropsEvalFn] closure that evaluates the
// key→expression pairs in ml against each incoming row at runtime. It is used
// when the property map for a CreateNode or CreateRelationship contains
// non-literal values (variable references, property accesses, arithmetic) that
// cannot be resolved at plan-construction time.
//
// The closure:
//  1. Builds an [expr.RowContext] from the current row using the captured schema
//     and mutator (for upgrading IntegerValue(NodeID) → NodeValue with properties).
//  2. Calls [expr.Eval] on each value expression in ml.
//  3. Converts the resulting [expr.Value] to [lpg.PropertyValue]; entries that
//     evaluate to Null or to an unsupported type are silently omitted.
//
// A nil ml produces a nil closure (no-op).
func buildPropsEvalFn(
	ml *ast.MapLiteral,
	schemaCopy map[string]int,
	params map[string]expr.Value,
	reg expr.FunctionRegistry,
	mutator exec.GraphMutator,
) exec.PropsEvalFn {
	if ml == nil {
		return nil
	}
	// Snapshot keys and value expressions so the closure is self-contained.
	keys := make([]string, len(ml.Keys))
	copy(keys, ml.Keys)
	vals := make([]ast.Expression, len(ml.Values))
	copy(vals, ml.Values)

	return func(row exec.Row) []exec.PropEntry {
		// Build a RowContext that can resolve variable bindings and node
		// property accesses from the current row.
		rowCtx := buildRowCtxFromMutator(row, schemaCopy, mutator)

		var out []exec.PropEntry
		for i, k := range keys {
			v, evalErr := expr.Eval(vals[i], rowCtx, params, reg)
			if evalErr != nil || v == nil {
				continue // expression error or nil: skip
			}
			if expr.IsNull(v) {
				continue // openCypher: assigning null to a property is a no-op
			}
			pv, ok := exprValueToLPGProp(v)
			if !ok {
				continue // unsupported type (e.g. NodeValue): skip
			}
			out = append(out, exec.PropEntry{Key: k, Value: pv})
		}
		return out
	}
}

// buildRowCtxFromMutator builds an [expr.RowContext] from a row using the
// captured schema. For each column that holds an IntegerValue, it attempts to
// resolve the corresponding node from the mutator and upgrade it to a NodeValue
// carrying properties — enabling property-access expressions like `a.id` when
// `a` is a bound node variable in the row.
//
// When no mutator is available, or when the integer cannot be resolved to a
// node, the raw IntegerValue is kept.
func buildRowCtxFromMutator(row exec.Row, schema map[string]int, mutator exec.GraphMutator) expr.RowContext {
	ctx := make(expr.RowContext, len(schema))
	for varName, colIdx := range schema {
		if colIdx >= len(row) || row[colIdx] == nil {
			continue
		}
		v := row[colIdx]
		if mutator != nil {
			if iv, ok := v.(expr.IntegerValue); ok {
				nodeID := graph.NodeID(iv)
				if key, resolved := mutator.ResolveNodeLabel(nodeID); resolved {
					rawProps := mutator.NodeProperties(key)
					props := make(expr.MapValue, len(rawProps))
					for k, pv := range rawProps {
						props[k] = lpgPropToExpr(pv)
					}
					labels := mutator.NodeLabels(key)
					ctx[varName] = expr.NodeValue{
						ID:         uint64(nodeID),
						Labels:     labels,
						Properties: props,
					}
					continue
				}
			}
		}
		ctx[varName] = v
	}
	return ctx
}

// exprValueToLPGProp converts an [expr.Value] to an [lpg.PropertyValue].
// Returns (zero, false) when the value type has no natural property encoding
// (e.g. NodeValue, RelationshipValue, PathValue).
//
// Temporal values (Date, LocalDateTime, DateTime, LocalTime, Time,
// Duration) are encoded as PropString with a SOH-range tag byte
// (0x01..0x06) followed by the canonical openCypher textual form, the
// same scheme used by [cypher.exec.parseTemporalLiteral] on the
// literal-string write path. The decoder is [decodeTemporalString].
func exprValueToLPGProp(v expr.Value) (lpg.PropertyValue, bool) {
	switch val := v.(type) {
	case expr.StringValue:
		return lpg.StringValue(string(val)), true
	case expr.IntegerValue:
		return lpg.Int64Value(int64(val)), true
	case expr.FloatValue:
		return lpg.Float64Value(float64(val)), true
	case expr.BoolValue:
		return lpg.BoolValue(bool(val)), true
	case expr.DateValue:
		return lpg.StringValue("\x01" + val.String()), true
	case expr.LocalDateTimeValue:
		return lpg.StringValue("\x02" + val.String()), true
	case expr.DateTimeValue:
		return lpg.StringValue("\x03" + val.String()), true
	case expr.LocalTimeValue:
		return lpg.StringValue("\x04" + val.String()), true
	case expr.TimeValue:
		return lpg.StringValue("\x05" + val.String()), true
	case expr.DurationValue:
		return lpg.StringValue("\x06" + val.String()), true
	case expr.ListValue:
		elems := make([]lpg.PropertyValue, 0, len(val))
		for _, el := range val {
			pv, ok := exprValueToLPGProp(el)
			if !ok {
				continue
			}
			elems = append(elems, pv)
		}
		return lpg.ListValue(elems), true
	default:
		return lpg.PropertyValue{}, false
	}
}

// firstVar returns the first non-empty string from vars, or empty string.
func firstVar(vars []string) string {
	for _, v := range vars {
		if v != "" {
			return v
		}
	}
	return ""
}

// parseNodePatternStr extracts labels and property string from an opaque IR
// node-pattern string of the form "(var:Label1:Label2 {key:'val',...})".
// Both outputs may be empty when the pattern is absent or unparseable.
//
// When the source pattern is a relationship MERGE such as
// "(a)-[r:T {k:'v'}]->(b)", the head-node prefix has no labels or
// properties, the leading "(...)" closes before the relationship body,
// and the embedded "{...}" belongs to the relationship — not to the
// head node. Both cases are handled defensively: only the head "(...)"
// segment is scanned, and the property map is extracted by a
// brace-balanced cursor so trailing pattern syntax never bleeds into
// the props string.
func parseNodePatternStr(pattern string) (labels []string, props string) {
	s := strings.TrimSpace(pattern)
	s = leadingParenSegment(s)
	if len(s) >= 2 && s[0] == '(' && s[len(s)-1] == ')' {
		s = s[1 : len(s)-1]
	}
	s, props = extractBracedSuffix(s)

	// Remaining: "var:Label1:Label2" — split on ':' and discard first token (var).
	parts := strings.Split(s, ":")
	for _, p := range parts[1:] {
		lbl := strings.TrimSpace(p)
		if lbl != "" {
			labels = append(labels, lbl)
		}
	}
	return
}

// leadingParenSegment returns the leading "(...)" segment of s using
// paren-depth tracking so a relationship pattern "(a)-[...]->(b)"
// resolves to "(a)" instead of consuming the entire string. Strings
// that do not start with '(' or have unbalanced parens are returned
// unchanged.
func leadingParenSegment(s string) string {
	if s == "" || s[0] != '(' {
		return s
	}
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[:i+1]
			}
		}
	}
	return s
}

// extractBracedSuffix splits the first balanced "{...}" suffix out of
// s. Returns the head (everything before the opening brace) and the
// balanced "{...}" substring. When s contains no '{', or the braces
// are unbalanced, the head is s unchanged and props is empty.
func extractBracedSuffix(s string) (head, props string) {
	braceIdx := strings.IndexByte(s, '{')
	if braceIdx < 0 {
		return s, ""
	}
	depth := 0
	end := -1
	for i := braceIdx; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				end = i
			}
		}
		if end >= 0 {
			break
		}
	}
	if end > braceIdx {
		props = strings.TrimSpace(s[braceIdx : end+1])
	}
	return s[:braceIdx], props
}

// BuildPlan converts an IR [ir.LogicalPlan] tree into a physical [exec.Operator]
// tree together with the ordered output column names.
//
// walker provides node enumeration; labelSrc provides label-filtered scans;
// reg provides the built-in function registry; params are the query parameters.
//
// Sprint 25 support matrix:
//   - [ir.AllNodesScan]
//   - [ir.NodeByLabelScan]
//   - [ir.Selection] (predicate is an always-true stub)
//   - [ir.Projection]
//   - [ir.ProduceResults] (required as root)
//   - [ir.Expand] (stub; child rows pass through, rel/dst vars bound to NULL)
func BuildPlan(
	plan ir.LogicalPlan,
	walker nodeWalkerIface,
	labelSrc labelResolverIface,
	reg expr.FunctionRegistry,
	params map[string]expr.Value,
) (op exec.Operator, cols []string, err error) {
	pr, ok := plan.(*ir.ProduceResults)
	if !ok {
		return nil, nil, fmt.Errorf("cypher: plan root must be ProduceResults, got %T", plan)
	}
	cols = pr.Columns

	// schema maps variable name → column index in the current row.
	schema := make(map[string]int)
	argByTag := make(map[uint32]*exec.Argument)
	child, err := buildOperator(pr.Child, walker, labelSrc, reg, params, schema, nil, nil, argByTag, nil)
	if err != nil {
		return nil, nil, err
	}

	// Wrap in a final projection that maps the schema to the output column order.
	items := make([]exec.ProjectionItem, len(cols))
	for i, col := range cols {
		if colIdx, exists := schema[col]; exists {
			idx := colIdx
			items[i] = exec.ProjectionItem{
				Alias: col,
				Eval: func(row exec.Row) (expr.Value, error) {
					if idx < len(row) {
						return row[idx], nil
					}
					return expr.Null, nil
				},
			}
		} else {
			items[i] = exec.ProjectionItem{
				Alias: col,
				Eval:  func(_ exec.Row) (expr.Value, error) { return expr.Null, nil },
			}
		}
	}

	proj, err := exec.NewProject(child, items)
	if err != nil {
		return nil, nil, fmt.Errorf("cypher: build final projection: %w", err)
	}
	return proj, cols, nil
}

// buildPlanEngine is the Engine-internal variant of BuildPlan that threads the
// index manager and procedure registry through so that NodeByIndexSeek and
// ProcedureCall IR nodes can be resolved at build time. idxMgr and procReg
// may both be nil. bopts carries the query-scope SubqueryEvaluator and
// queryCtx for [ast.ExistsSubquery] / [ast.CountSubquery] expressions; pass
// nil when no subquery support is needed.
func buildPlanEngine(
	plan ir.LogicalPlan,
	walker nodeWalkerIface,
	labelSrc labelResolverIface,
	reg expr.FunctionRegistry,
	params map[string]expr.Value,
	idxMgr *index.Manager,
	procReg *procs.Registry,
	bopts *buildOpts,
) (op exec.Operator, cols []string, err error) {
	// Standalone CALL (root is *ir.ProcedureCall): treat YieldVars as columns.
	if p, ok := plan.(*ir.ProcedureCall); ok {
		schema := make(map[string]int)
		argByTag := make(map[uint32]*exec.Argument)
		child, buildErr := buildOperator(p, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if buildErr != nil {
			return nil, nil, buildErr
		}
		return child, p.YieldVars, nil
	}

	// UNION / UNION ALL: each branch is itself a top-level plan (typically
	// a ProduceResults). Recursively build each side, then concatenate via
	// exec.NewUnionAll (preserves duplicates) or exec.NewUnion (with a
	// Distinct cap to deduplicate). The left branch's column names are
	// returned as the union's output schema — openCypher requires every
	// branch of a UNION to expose the same column names in the same order.
	if u, ok := plan.(*ir.UnionAll); ok {
		leftOp, leftCols, lerr := buildPlanEngine(u.Left, walker, labelSrc, reg, params, idxMgr, procReg, bopts)
		if lerr != nil {
			return nil, nil, lerr
		}
		rightOp, _, rerr := buildPlanEngine(u.Right, walker, labelSrc, reg, params, idxMgr, procReg, bopts)
		if rerr != nil {
			return nil, nil, rerr
		}
		return exec.NewUnionAll(leftOp, rightOp), leftCols, nil
	}
	if u, ok := plan.(*ir.Union); ok {
		leftOp, leftCols, lerr := buildPlanEngine(u.Left, walker, labelSrc, reg, params, idxMgr, procReg, bopts)
		if lerr != nil {
			return nil, nil, lerr
		}
		rightOp, _, rerr := buildPlanEngine(u.Right, walker, labelSrc, reg, params, idxMgr, procReg, bopts)
		if rerr != nil {
			return nil, nil, rerr
		}
		return exec.NewUnion(leftOp, rightOp, 0), leftCols, nil
	}

	pr, ok := plan.(*ir.ProduceResults)
	if !ok {
		return nil, nil, fmt.Errorf("cypher: plan root must be ProduceResults, got %T", plan)
	}
	cols = pr.Columns
	schema := make(map[string]int)
	argByTag := make(map[uint32]*exec.Argument)
	child, err := buildOperator(pr.Child, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
	if err != nil {
		return nil, nil, err
	}
	items := make([]exec.ProjectionItem, len(cols))
	for i, col := range cols {
		if colIdx, exists := schema[col]; exists {
			idx := colIdx
			items[i] = exec.ProjectionItem{
				Alias: col,
				Eval: func(row exec.Row) (expr.Value, error) {
					if idx < len(row) {
						return row[idx], nil
					}
					return expr.Null, nil
				},
			}
		} else {
			items[i] = exec.ProjectionItem{
				Alias: col,
				Eval:  func(_ exec.Row) (expr.Value, error) { return expr.Null, nil },
			}
		}
	}
	proj, err := exec.NewProject(child, items)
	if err != nil {
		return nil, nil, fmt.Errorf("cypher: build final projection: %w", err)
	}
	return proj, cols, nil
}

// buildOperator recursively converts an IR plan node to a physical operator.
// schema accumulates variable→column-index bindings as operators are visited
// top-down (left-to-right for children). idxMgr and procReg may both be nil.
//
// argByTag routes [*ir.Argument] nodes to a specific [*exec.Argument] instance
// pre-allocated by an enclosing Apply-family operator (CorrelatedApply or
// OptionalApply). It may be nil when no Apply is in scope; missing tags fall
// back to a fresh exec.Argument so isolated Argument nodes still work.
//
//nolint:gocyclo // large switch — one case per read IR node type; no hidden branches
func buildOperator(
	plan ir.LogicalPlan,
	walker nodeWalkerIface,
	labelSrc labelResolverIface,
	reg expr.FunctionRegistry,
	params map[string]expr.Value,
	schema map[string]int,
	idxMgr *index.Manager,
	procReg *procs.Registry,
	argByTag map[uint32]*exec.Argument,
	bopts *buildOpts,
) (exec.Operator, error) {
	if plan == nil {
		// A nil read plan drives a single-row empty operator (e.g. bare RETURN
		// without a preceding MATCH).
		return exec.NewSingleRowOperator(), nil
	}
	switch p := plan.(type) {

	case *ir.AllNodesScan:
		schema[p.NodeVar] = schemaWidth(schema)
		return exec.NewAllNodesScan(walker), nil

	case *ir.NodeByLabelScan:
		schema[p.NodeVar] = schemaWidth(schema)
		// Adapt lpgLabelResolver to exec.labelResolver; both have the same
		// method signature so a direct wrapper suffices.
		src := &execLabelAdapter{labelSrc: labelSrc}
		return exec.NewNodeByLabelScan(p.Label, src), nil

	case *ir.NodeByIndexSeek:
		return buildIndexSeekOperator(p, params, schema, idxMgr)

	case *ir.Selection:
		// Opportunistic index-seek rewrite: if the predicate is a simple equality
		// n.prop = $name and a hash index is available, produce NodeByIndexSeek
		// directly without first building the scan child.
		if idxMgr != nil {
			if op, ok, err := tryBuildIndexSeekFromSelection(p, params, schema, idxMgr); err != nil {
				return nil, err
			} else if ok {
				return op, nil
			}
		}
		child, err := buildOperator(p.Child, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		if p.PredicateExpr != nil {
			var selG *lpg.Graph[string, float64]
			if lw, ok := walker.(*lpgNodeWalker); ok {
				selG = lw.g
			}
			if selG != nil {
				schemaSnap := copySchema(schema)
				predExpr := p.PredicateExpr
				capturedG := selG
				capturedParams := params
				capturedReg := reg
				capturedBopts := bopts
				return exec.NewFilter(child, func(row exec.Row) (expr.Value, error) {
					rowCtx := buildRowCtx(row, schemaSnap, capturedG, capturedBopts)
					return evalRow(capturedBopts, predExpr, rowCtx, capturedParams, capturedReg)
				}), nil
			}
		}
		// Fallback: no AST or no graph available — pass-through filter.
		return exec.NewFilter(child, func(_ exec.Row) (expr.Value, error) {
			return expr.BoolValue(true), nil
		}), nil

	case *ir.Projection:
		child, err := buildOperator(p.Child, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		var projG *lpg.Graph[string, float64]
		if lw, ok := walker.(*lpgNodeWalker); ok {
			projG = lw.g
		}
		return buildIRProjection(p.Items, child, schema, projG, params, reg, bopts)

	case *ir.Expand:
		child, err := buildOperator(p.Child, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}

		// Record the column index of the source var BEFORE we add new columns so
		// that inputCol correctly points at the source node in the child row.
		fromCol := 0
		if col, ok := schema[p.FromVar]; ok {
			fromCol = col
		}

		// Expand.buildRow emits: inputRow... || srcID || edgeID || dstID
		// The srcID duplicate occupies one slot with no variable bound (the source
		// is already in the schema via FromVar); RelVar maps to the edgeID slot;
		// ToVar maps to the dstID slot.
		//
		// We advance the schema counter by 3 for the (srcID, edgeID, dstID)
		// triplet emitted by Expand. Each slot gets a stable key in the schema
		// map even when no user-visible variable is bound, so schemaWidth(schema)
		// matches the actual row width — critical for downstream operators (e.g.
		// a sibling AllNodesScan inside an Apply) that allocate schema slots
		// based on the row width.
		schemaBase := schemaWidth(schema)
		schema[p.FromVar+"__dup"] = schemaBase // srcID dup — anonymous internal slot
		relKey := p.RelVar
		if relKey == "" {
			relKey = fmt.Sprintf("__anon_rel_%d", schemaBase+1)
		}
		schema[relKey] = schemaBase + 1
		toKey := p.ToVar
		if toKey == "" {
			toKey = fmt.Sprintf("__anon_to_%d", schemaBase+2)
		}
		schema[toKey] = schemaBase + 2

		// Record the triplet in chain order so a *ir.NamedPath wrapper above
		// this subtree can map its IR chain elements to the slots emitted by
		// this Expand. Done for both named and anonymous relationships — the
		// chain may include either.
		if bopts != nil {
			step := pathChainStep{
				srcCol:  schemaBase,
				edgeCol: schemaBase + 1,
				dstCol:  schemaBase + 2,
			}
			if len(p.RelTypes) > 0 {
				step.edgeType = p.RelTypes[0]
			}
			bopts.expandTripletSeq = append(bopts.expandTripletSeq, step)
		}

		// Record edge variable metadata so buildIRProjection can reconstruct
		// a RelationshipValue when the variable is projected directly.
		if p.RelVar != "" && bopts != nil {
			if bopts.edgeVarMeta == nil {
				bopts.edgeVarMeta = make(map[string]edgeVarInfo)
			}
			info := edgeVarInfo{
				srcCol:  schemaBase,     // srcID dup column
				edgeCol: schemaBase + 1, // edgeID column (= schema[relKey])
				dstCol:  schemaBase + 2, // dstID column  (= schema[toKey])
			}
			if len(p.RelTypes) > 0 {
				info.edgeType = p.RelTypes[0]
			}
			bopts.edgeVarMeta[p.RelVar] = info
		}

		var g *lpg.Graph[string, float64]
		if lw, ok := walker.(*lpgNodeWalker); ok {
			g = lw.g
		}
		if g == nil {
			// No graph available (e.g. schema-only planning) — return child with
			// NULL columns so downstream projections can reference the vars.
			return child, nil
		}

		fwd, rev := csrPairFromGraph(g)
		dir := irDirToExec(p.Direction)

		cfg := exec.ExpandConfig{
			Direction: dir,
			InputCol:  fromCol,
		}
		if len(p.RelTypes) > 0 {
			cfg.EdgeType = p.RelTypes[0]
			cfg.EdgeTypeFilter = buildEdgeTypeFilter(g, p.RelTypes)
		}
		return exec.NewExpand(child, fwd, rev, cfg), nil

	case *ir.Apply:
		// Build the outer plan first so its vars enter the schema.
		outer, err := buildOperator(p.Outer, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		// The inner subtree of a plain (non-correlated) Apply runs in
		// isolation — it does not consume the outer row, so inner rows are
		// inner-columns-only. Build the inner with a FRESH schema so its
		// operators index columns from 0. After the build, merge the inner
		// schema back into the shared schema with each column offset by the
		// outer's width so the post-Apply combined row layout (outer||inner)
		// stays addressable downstream.
		outerWidth := schemaWidth(schema)
		innerSchema := map[string]int{}
		arg := exec.NewArgument()
		inner, err := buildOperator(p.Inner, walker, labelSrc, reg, params, innerSchema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		for k, v := range innerSchema {
			schema[k] = v + outerWidth
		}
		return exec.NewApply(outer, inner, arg), nil

	case *ir.CorrelatedApply:
		// Build outer first; its vars enter the schema and define the outer width.
		outer, err := buildOperator(p.Outer, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		// Pre-allocate the exec.Argument and register it under the IR Argument's
		// tag so the inner subtree's matching Argument leaf resolves to this
		// instance.
		arg := exec.NewArgument()
		if argByTag != nil {
			argByTag[p.ArgTag] = arg
		}
		inner, err := buildOperator(p.Inner, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		if argByTag != nil {
			delete(argByTag, p.ArgTag)
		}
		return exec.NewCorrelatedApply(outer, inner, arg), nil

	case *ir.OptionalApply:
		// Build outer first; record its width so the NULL-extension row has the
		// correct padding when the inner pipeline emits zero rows.
		outer, err := buildOperator(p.Outer, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		outerWidth := schemaWidth(schema)
		arg := exec.NewArgument()
		if argByTag != nil {
			argByTag[p.ArgTag] = arg
		}
		inner, err := buildOperator(p.Inner, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		if argByTag != nil {
			delete(argByTag, p.ArgTag)
		}
		// paddedWidth = outerWidth + (inner-added columns). schemaWidth captures
		// the real row width (max index + 1) so secondary expression-string keys
		// (e.g. schema["date({…})"] sharing schema["x"]'s slot) do not inflate
		// the count.
		paddedWidth := schemaWidth(schema)
		if paddedWidth < outerWidth {
			paddedWidth = outerWidth
		}
		return exec.NewOptionalApply(outer, inner, arg, paddedWidth), nil

	case *ir.SemiApply:
		outer, err := buildOperator(p.Outer, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		// Pre-allocate the exec.Argument and register it under the IR
		// SemiApply's ArgTag so the inner subtree's matching Argument leaf
		// resolves to this instance and receives the outer row per iteration.
		arg := exec.NewArgument()
		if argByTag != nil {
			argByTag[p.ArgTag] = arg
		}
		inner, err := buildOperator(p.Inner, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		if argByTag != nil {
			delete(argByTag, p.ArgTag)
		}
		return exec.NewSemiApply(outer, inner, arg), nil

	case *ir.AntiSemiApply:
		outer, err := buildOperator(p.Outer, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		arg := exec.NewArgument()
		if argByTag != nil {
			argByTag[p.ArgTag] = arg
		}
		inner, err := buildOperator(p.Inner, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		if argByTag != nil {
			delete(argByTag, p.ArgTag)
		}
		return exec.NewAntiSemiApply(outer, inner, arg), nil

	case *ir.RollUpApply:
		// Pattern-comprehension execution. Build outer first; its
		// columns enter the schema and define the outer width that
		// the RollUpApply output preserves.
		outer, err := buildOperator(p.Outer, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		outerWidth := schemaWidth(schema)
		// Pre-allocate the exec.Argument and register it under the IR
		// RollUpApply's ArgTag so the inner subtree's matching
		// Argument leaf resolves to this instance and receives the
		// outer row per iteration.
		arg := exec.NewArgument()
		if argByTag != nil {
			argByTag[p.ArgTag] = arg
		}
		inner, err := buildOperator(p.Inner, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		if argByTag != nil {
			delete(argByTag, p.ArgTag)
		}
		// The RollUpApply output is (outer columns…, collected list).
		// Inner-built variables are not visible downstream — only the
		// list bound to CollectVar is. Drop schema entries that the
		// inner subplan added beyond outerWidth, then register
		// CollectVar at outerWidth so the final-projection lookup
		// resolves to the list column.
		for name, idx := range schema {
			if idx >= outerWidth {
				delete(schema, name)
			}
		}
		schema[p.CollectVar] = outerWidth
		// listEval is left nil — the inner subplan ends with a
		// Projection that puts the comprehension's projected value at
		// the row's first column, which is the default collection
		// extractor.
		return exec.NewRollUpApply(outer, inner, arg, nil), nil

	case *ir.Argument:
		// Argument is the leaf of an Apply-family inner plan. At runtime the exec
		// Argument operator re-emits the outer row that was injected by the Apply
		// loop. The IR vars are already in schema from the outer build; no new
		// column registrations are needed here.
		//
		// When p.Tag is registered in argByTag (by an enclosing CorrelatedApply
		// or OptionalApply), reuse the same exec.Argument instance so the Apply
		// driver and the leaf share state. Otherwise allocate a fresh, unbound
		// Argument for standalone use.
		if argByTag != nil {
			if a, ok := argByTag[p.Tag]; ok {
				return a, nil
			}
		}
		return exec.NewArgument(), nil

	case *ir.EagerAggregation:
		child, err := buildOperator(p.Child, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		var aggG *lpg.Graph[string, float64]
		if lw, ok := walker.(*lpgNodeWalker); ok {
			aggG = lw.g
		}
		return buildEagerAggregation(p, child, schema, aggG, params, reg, bopts)

	case *ir.Sort:
		child, err := buildOperator(p.Child, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		var sortG *lpg.Graph[string, float64]
		if lw, ok := walker.(*lpgNodeWalker); ok {
			sortG = lw.g
		}
		keys := irSortKeys(p.SortItems, schema, sortG, params, reg, bopts)
		if len(keys) == 0 {
			// No resolvable sort keys — pass through without sorting.
			return child, nil
		}
		return exec.NewSort(child, keys, 0)

	case *ir.Top:
		child, err := buildOperator(p.Child, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		var topG *lpg.Graph[string, float64]
		if lw, ok := walker.(*lpgNodeWalker); ok {
			topG = lw.g
		}
		keys := irSortKeys(p.SortItems, schema, topG, params, reg, bopts)
		if len(keys) == 0 || p.Limit <= 0 {
			// Degenerate: no sort keys or zero limit — return child unchanged.
			return child, nil
		}
		// exec.NewTop requires n ≥ 1 (int); ir.Top.Limit is int64.
		n := int(p.Limit)
		if int64(n) != p.Limit {
			// Limit overflows int — clamp to a large safe value.
			n = int(^uint(0) >> 1) // math.MaxInt
		}
		return exec.NewTop(child, keys, n)

	case *ir.Limit:
		child, err := buildOperator(p.Child, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		return exec.NewLimit(child, p.Count)

	case *ir.Skip:
		child, err := buildOperator(p.Child, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		return exec.NewSkip(child, p.Count)

	case *ir.Unwind:
		child, err := buildOperator(p.Child, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		// Use schemaWidth (max column index + 1) rather than len(schema) to
		// determine the element column index.  buildIRProjection registers
		// secondary expression-string keys that share an existing column index
		// (e.g. schema["[1,2,3]"] == schema["lst"] == 0), which inflates
		// len(schema) without widening the actual row.
		schema[p.ElementVar] = schemaWidth(schema)
		return buildUnwindOperator(p, child, schema, walker, params, reg, bopts)

	case *ir.Distinct:
		child, err := buildOperator(p.Child, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		return exec.NewDistinct(child, 0), nil

	case *ir.OptionalExpand:
		child, err := buildOperator(p.Child, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}

		fromCol := 0
		if col, ok := schema[p.FromVar]; ok {
			fromCol = col
		}

		// Same output layout as Expand: inputRow... || srcID || edgeID || dstID.
		// Always advance the schema by 3 (including anonymous slots) so that
		// schemaWidth(schema) tracks the actual row width — see the Expand case
		// for rationale.
		schemaBase := schemaWidth(schema)
		schema[p.FromVar+"__opt_dup"] = schemaBase
		relKey := p.RelVar
		if relKey == "" {
			relKey = fmt.Sprintf("__anon_opt_rel_%d", schemaBase+1)
		}
		schema[relKey] = schemaBase + 1
		toKey := p.ToVar
		if toKey == "" {
			toKey = fmt.Sprintf("__anon_opt_to_%d", schemaBase+2)
		}
		schema[toKey] = schemaBase + 2

		// Mirror the Expand triplet bookkeeping so a *ir.NamedPath wrapper can
		// reconstruct a PathValue across OPTIONAL hops as well. The PathValue
		// closure tolerates NULL slots (returns Null when any expected column
		// is missing).
		if bopts != nil {
			step := pathChainStep{
				srcCol:  schemaBase,
				edgeCol: schemaBase + 1,
				dstCol:  schemaBase + 2,
			}
			if len(p.RelTypes) > 0 {
				step.edgeType = p.RelTypes[0]
			}
			bopts.expandTripletSeq = append(bopts.expandTripletSeq, step)
		}

		var g *lpg.Graph[string, float64]
		if lw, ok := walker.(*lpgNodeWalker); ok {
			g = lw.g
		}
		if g == nil {
			return child, nil
		}

		fwd, rev := csrPairFromGraph(g)
		dir := irDirToExec(p.Direction)

		cfg := exec.ExpandConfig{
			Direction: dir,
			InputCol:  fromCol,
		}
		if len(p.RelTypes) > 0 {
			cfg.EdgeType = p.RelTypes[0]
			cfg.EdgeTypeFilter = buildEdgeTypeFilter(g, p.RelTypes)
		}
		return exec.NewOptionalExpand(child, fwd, rev, cfg), nil

	case *ir.VarLengthExpand:
		child, err := buildOperator(p.Child, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}

		fromCol := 0
		if col, ok := schema[p.FromVar]; ok {
			fromCol = col
		}

		// VarLengthExpand emits: inputRow... || pathList || dstNodeID.
		// pathList is a flat alternating ListValue: [srcID, edgePos0, dst0, ...].
		// Always advance schema by 2 — anonymous slots receive synthetic keys so
		// schemaWidth(schema) matches the actual row width.
		schemaBaseVLE := schemaWidth(schema)
		relKey := p.RelVar
		if relKey == "" {
			relKey = fmt.Sprintf("__anon_vlrel_%d", schemaBaseVLE)
		}
		schema[relKey] = schemaBaseVLE
		toKey := p.ToVar
		if toKey == "" {
			toKey = fmt.Sprintf("__anon_vlto_%d", schemaBaseVLE+1)
		}
		schema[toKey] = schemaBaseVLE + 1

		// Record path variable metadata for buildIRProjection.
		if p.PathVar != "" && bopts != nil {
			if bopts.pathVarMeta == nil {
				bopts.pathVarMeta = make(map[string]pathVarInfo)
			}
			info := pathVarInfo{listCol: schemaBaseVLE}
			if len(p.RelTypes) > 0 {
				info.edgeType = p.RelTypes[0]
			}
			bopts.pathVarMeta[p.PathVar] = info
			// Also register in schema so variable is accessible.
			schema[p.PathVar] = schemaBaseVLE
		}
		// Record the VLE relationship variable so the projection renders it
		// as a list of RelationshipValues instead of the raw flat list.
		if p.RelVar != "" && bopts != nil {
			if bopts.vleRelMeta == nil {
				bopts.vleRelMeta = make(map[string]vleRelInfo)
			}
			info := vleRelInfo{listCol: schemaBaseVLE}
			if len(p.RelTypes) > 0 {
				info.edgeType = p.RelTypes[0]
			}
			bopts.vleRelMeta[p.RelVar] = info
		}

		var g *lpg.Graph[string, float64]
		if lw, ok := walker.(*lpgNodeWalker); ok {
			g = lw.g
		}
		if g == nil {
			return child, nil
		}

		fwd, rev := csrPairFromGraph(g)
		dir := irDirToExec(p.Direction)
		minHops := p.MinDepth
		maxHops := p.MaxDepth
		// ir.VarLengthExpand.MaxDepth carries math.MaxInt for an unbounded
		// upper bound and a real integer otherwise (including 0 for the
		// degenerate "*0" quantifier). The exec.VarLengthExpand operator
		// honours MaxHops==0 as "no expansion beyond the source", which is
		// exactly the desired semantics for *0 / *0..0 / *N..M (N>M).

		var etFilter map[uint64]string
		edgeType := ""
		if len(p.RelTypes) > 0 {
			edgeType = p.RelTypes[0]
			etFilter = buildEdgeTypeFilter(g, p.RelTypes)
		}

		cfg := exec.VarLengthConfig{
			Direction:      dir,
			EdgeType:       edgeType,
			EdgeTypeFilter: etFilter,
			InputCol:       fromCol,
			MinHops:        minHops,
			MaxHops:        maxHops,
		}
		return exec.NewVarLengthExpand(child, fwd, rev, cfg), nil

	case *ir.NamedPath:
		// NamedPath is a pure pass-through: build the child, then register
		// the alternating-chain metadata so that buildIRProjection can
		// reconstruct an expr.PathValue for this path variable.
		//
		// Approach: each non-leading IR chain element corresponds to one
		// Expand operator emitted by the child subtree. Expand registers its
		// (srcCol, edgeCol, dstCol) triplet into bopts.expandTripletSeq in
		// build order, so we capture the slice length before recursing and
		// consume the entries appended during the child build.
		var startIdx int
		if bopts != nil {
			startIdx = len(bopts.expandTripletSeq)
		}
		child, err := buildOperator(p.Child, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
		if bopts == nil || p.PathName == "" {
			return child, nil
		}

		// Map IR chain to triplets. The leading element's NodeVar must be
		// resolvable in the current schema (an anonymous leading node would
		// have been registered under its synthetic key by AllNodesScan /
		// NodeByLabelScan); when the lookup fails we fall back to column 0,
		// which matches the legacy behaviour of the VLE pathway when the
		// FromVar lookup fails.
		var info pathChainInfo
		if leadingChain := p.Chain; len(leadingChain) > 0 && leadingChain[0].IsLeading {
			if col, ok := schema[leadingChain[0].NodeVar]; ok {
				info.leadingCol = col
			}
		}
		// Collect the triplets registered during the child build.
		added := bopts.expandTripletSeq[startIdx:]
		// Count non-leading IR chain elements to bound the iteration.
		nSteps := 0
		for i := range p.Chain {
			if !p.Chain[i].IsLeading {
				nSteps++
			}
		}
		if nSteps > len(added) {
			nSteps = len(added)
		}
		info.steps = make([]pathChainStep, 0, nSteps)
		chainIdx := 0
		for i := 0; i < nSteps && chainIdx < len(p.Chain); {
			if p.Chain[chainIdx].IsLeading {
				chainIdx++
				continue
			}
			step := added[i]
			// Prefer the AST-declared rel type when present; the live-graph
			// lookup in buildIRProjection takes precedence at row time.
			if len(p.Chain[chainIdx].RelTypes) > 0 && step.edgeType == "" {
				step.edgeType = p.Chain[chainIdx].RelTypes[0]
			}
			info.steps = append(info.steps, step)
			chainIdx++
			i++
		}

		if bopts.pathVarChain == nil {
			bopts.pathVarChain = make(map[string]pathChainInfo)
		}
		bopts.pathVarChain[p.PathName] = info
		// Register the path variable in the schema so downstream operators
		// that resolve "p" via column lookup (e.g. after a projection that
		// emits PathValue into a slot) keep working. We map it to the
		// leading-node column as a stable, harmless placeholder: the
		// projection fast path keys off pathVarChain before falling back to
		// schema, and any post-projection read of "p" looks up the slot
		// allocated by buildIRProjection itself rather than this one.
		if _, exists := schema[p.PathName]; !exists {
			schema[p.PathName] = info.leadingCol
		}
		return child, nil

	case *ir.ProcedureCall:
		return buildProcedureCallOperator(p, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)

	default:
		// When the engine is running inside a transactional write call
		// ([Engine.RunInTx]), buildPlanWithMutatorFull installs a
		// writeFallback closure on bopts so that write IR nodes
		// encountered as children of read wrappers (Projection over
		// CreateNode, Sort over SetProperty, …) recurse through the
		// write-aware planner instead of failing here. Read-only
		// callers leave the field nil and fall through to the original
		// "unsupported IR node" error.
		if bopts != nil && bopts.writeFallback != nil {
			return bopts.writeFallback(plan)
		}
		return nil, fmt.Errorf("cypher: unsupported IR node %T", plan)
	}
}

// buildEagerAggregation builds the physical EagerAggregation operator from the
// IR node. It wraps the child in a pre-projection so that the exec operator
// sees rows in the expected layout: [groupByKeys..., aggArgs...].
//
// Each pre-projection slot resolves its source value with the following
// priority order:
//
//  1. Parsed AST expression (when carried by the IR via GroupByExprs /
//     AggregateExpr.ArgumentExpr) — evaluated through [expr.Eval] against a
//     loaded RowContext so property accesses (n.prop), function calls, and
//     parameter references resolve correctly.
//  2. Schema lookup keyed on the textual group-by variable name or aggregate
//     argument string — preserves the legacy fast path for bare-variable
//     groupings (e.g. WITH n, count(*)).
//  3. Constant NULL — last-resort fallback so the pipeline keeps emitting
//     rows for malformed but non-fatal inputs.
//
// The Null fallback is openCypher-safe: count(NULL) does not increment,
// sum/avg/min/max ignore NULL, and collect skips NULL.
func buildEagerAggregation(
	p *ir.EagerAggregation,
	child exec.Operator,
	schema map[string]int,
	g *lpg.Graph[string, float64],
	params map[string]expr.Value,
	reg expr.FunctionRegistry,
	bopts *buildOpts,
) (exec.Operator, error) {
	// Snapshot the inbound schema once: every pre-projection eval needs the
	// pre-aggregation column layout, not the post-aggregation one (which we
	// overwrite below).
	schemaSnap := copySchema(schema)
	capturedG := g
	capturedParams := params
	capturedReg := reg
	capturedBopts := bopts

	// Build pre-projection items:
	//   positions 0..len(GroupBy)-1  → group-by key columns
	//   positions len(GroupBy)..end  → aggregate argument columns
	items := make([]exec.ProjectionItem, 0, len(p.GroupBy)+len(p.Aggregates))

	// Group-by key projections.
	keyCols := make([]int, len(p.GroupBy))
	for i, varName := range p.GroupBy {
		keyCols[i] = i // after pre-projection, key i is at position i

		var astExpr ast.Expression
		if i < len(p.GroupByExprs) {
			astExpr = p.GroupByExprs[i]
		}
		items = append(items, exec.ProjectionItem{
			Alias: varName,
			Eval:  newAggregationEval(astExpr, varName, schemaSnap, capturedG, capturedParams, capturedReg, capturedBopts),
		})
	}

	// Aggregate argument projections.
	aggFactories := make([]funcs.AggregatorFactory, 0, len(p.Aggregates))
	for _, aggExpr := range p.Aggregates {
		// Two-arg aggregates (percentileCont, percentileDisc) carry the
		// percentile parameter in SecondArgExpr. Evaluate it once at
		// build time so the factory bakes the value in. Single-arg
		// aggregates pass expr.Null which is ignored downstream.
		var secondArg expr.Value = expr.Null
		if aggExpr.SecondArgExpr != nil {
			v, evErr := expr.Eval(aggExpr.SecondArgExpr, expr.RowContext{}, params, reg)
			if evErr == nil {
				secondArg = v
			}
		}
		factory, ferr := aggregateFactory(aggExpr.Function, aggExpr.Argument, secondArg)
		if ferr != nil {
			return nil, fmt.Errorf("cypher: %w", ferr)
		}
		aggFactories = append(aggFactories, factory)

		// count(*) — argument is irrelevant; emit a constant non-null sentinel so
		// the aggregator's Step always increments. exec.NewCountStarAgg treats any
		// non-null value as a tick.
		if aggExpr.Argument == "" {
			items = append(items, exec.ProjectionItem{
				Alias: aggExpr.OutputName,
				Eval:  func(_ exec.Row) (expr.Value, error) { return expr.BoolValue(true), nil },
			})
			continue
		}

		items = append(items, exec.ProjectionItem{
			Alias: aggExpr.OutputName,
			Eval: newAggregationEval(
				aggExpr.ArgumentExpr, aggExpr.Argument,
				schemaSnap, capturedG, capturedParams, capturedReg, capturedBopts,
			),
		})
	}

	// Wrap child with the pre-projection.
	preProj, err := exec.NewProject(child, items)
	if err != nil {
		return nil, fmt.Errorf("cypher: EagerAggregation pre-projection: %w", err)
	}

	op, err := exec.NewEagerAggregation(preProj, keyCols, aggFactories, 0)
	if err != nil {
		return nil, fmt.Errorf("cypher: NewEagerAggregation: %w", err)
	}

	// When there are no group-by keys, openCypher semantics require a single
	// output row even when the input is empty — e.g.
	//
	//	MATCH (n:NeverExists) RETURN count(*)   --> 0
	//
	// EagerAggregation as a multiset operator emits zero rows in that case, so
	// wrap it in an adapter that synthesises the "empty-input → one-row of
	// neutral results" row.
	var topOp exec.Operator = op
	if len(p.GroupBy) == 0 {
		topOp = exec.NewGlobalAggregateAdapter(op, aggFactories)
	}

	// Replace schema with EagerAggregation output schema:
	//   positions 0..len(GroupBy)-1            → group-by variable names
	//   positions len(GroupBy)..len(Aggs)-1    → aggregate output names
	for k := range schema {
		delete(schema, k)
	}
	for i, varName := range p.GroupBy {
		schema[varName] = i
	}
	for i, aggExpr := range p.Aggregates {
		schema[aggExpr.OutputName] = len(p.GroupBy) + i
	}

	// Mark every aggregate output column as scalar so that buildIRProjection's
	// Variable fast-path does not mis-upgrade an integer count/sum/avg result into
	// a NodeValue when the integer coincides with a real NodeID.
	if bopts != nil {
		if bopts.scalarCols == nil {
			bopts.scalarCols = make(map[string]struct{})
		}
		for _, aggExpr := range p.Aggregates {
			bopts.scalarCols[aggExpr.OutputName] = struct{}{}
		}
	}

	return topOp, nil
}

// newAggregationEval returns a row-evaluator function suitable for an
// EagerAggregation pre-projection slot. The evaluator's resolution order is:
//
//  1. When astExpr is non-nil, evaluate it via [expr.Eval] against a RowContext
//     built from schemaSnap (which always reflects the pre-aggregation column
//     layout).
//  2. Otherwise, attempt a direct schema lookup keyed on varName.
//  3. Otherwise return [expr.Null].
func newAggregationEval(
	astExpr ast.Expression,
	varName string,
	schemaSnap map[string]int,
	g *lpg.Graph[string, float64],
	params map[string]expr.Value,
	reg expr.FunctionRegistry,
	bopts *buildOpts,
) func(exec.Row) (expr.Value, error) {
	// AST path — always preferred when present.
	if astExpr != nil {
		return func(row exec.Row) (expr.Value, error) {
			rowCtx := buildRowCtx(row, schemaSnap, g, bopts)
			return evalRow(bopts, astExpr, rowCtx, params, reg)
		}
	}
	// Legacy schema-lookup path.
	if col, ok := schemaSnap[varName]; ok {
		capturedCol := col
		return func(row exec.Row) (expr.Value, error) {
			if capturedCol < len(row) {
				return row[capturedCol], nil
			}
			return expr.Null, nil
		}
	}
	return func(_ exec.Row) (expr.Value, error) { return expr.Null, nil }
}

// aggregateFactory maps an IR aggregate function name and argument to a
// [funcs.AggregatorFactory]. An empty argument string means count(*).
// secondArg is the evaluated percentile parameter for two-arg
// aggregates; pass NULL for single-arg aggregates.
func aggregateFactory(fn, argument string, secondArg expr.Value) (funcs.AggregatorFactory, error) {
	lower := strings.ToLower(fn)
	switch lower {
	case "count":
		if argument == "" {
			return funcs.NewCountStarAgg(), nil
		}
		return funcs.NewCountAgg(), nil
	case "sum":
		return funcs.NewSumAgg(), nil
	case "avg":
		return funcs.NewAvgAgg(), nil
	case "min":
		return funcs.NewMinAgg(), nil
	case "max":
		return funcs.NewMaxAgg(), nil
	case "collect":
		return funcs.NewCollectAgg(), nil
	case "stdev":
		return funcs.NewStdDevAgg(), nil
	case "stdevp":
		return funcs.NewStdDevPAgg(), nil
	case "percentilecont":
		p, err := validPercentileParam(secondArg)
		if err != nil {
			return nil, err
		}
		return funcs.NewPercentileContAgg(p), nil
	case "percentiledisc":
		p, err := validPercentileParam(secondArg)
		if err != nil {
			return nil, err
		}
		return funcs.NewPercentileDiscAgg(p), nil
	default:
		return nil, fmt.Errorf("unknown aggregate function %q", fn)
	}
}

// percentileParam coerces the second argument of a percentile aggregate
// to a float64 in [0, 1]. Non-numeric inputs fall back to 0.5 (the
// median); the aggregator clamps the final value internally.
func percentileParam(v expr.Value) float64 {
	switch n := v.(type) {
	case expr.FloatValue:
		return float64(n)
	case expr.IntegerValue:
		return float64(int64(n))
	}
	return 0.5
}

// validPercentileParam coerces and validates the second argument of a
// percentile aggregate. Per openCypher, the percentile must be a number
// in [0.0, 1.0] (inclusive); values outside this range raise an
// ArgumentError(NumberOutOfRange) at plan-build time so the engine can
// surface it as a runtime error to the caller.
func validPercentileParam(v expr.Value) (float64, error) {
	switch n := v.(type) {
	case expr.FloatValue:
		f := float64(n)
		if f < 0.0 || f > 1.0 {
			return 0, fmt.Errorf("ArgumentError.NumberOutOfRange: percentile must be in [0.0, 1.0], got %g", f)
		}
		return f, nil
	case expr.IntegerValue:
		f := float64(int64(n))
		if f < 0.0 || f > 1.0 {
			return 0, fmt.Errorf("ArgumentError.NumberOutOfRange: percentile must be in [0.0, 1.0], got %g", f)
		}
		return f, nil
	}
	// Unset or non-numeric: default to median (matches percentileParam
	// fallback for the non-failing happy paths).
	return 0.5, nil
}

// irSortKeys converts a slice of ir.SortItem to exec.SortKey values.
//
// Resolution strategy (per item):
//  1. Direct schema lookup by expression string — covers the common case where
//     the sort key is a projected column (name or alias matches).
//  2. If (1) fails and the item carries an AST expression (si.Expr != nil),
//     compile an expression evaluator that derives the sort value from the row
//     context at runtime. This handles ORDER BY on expressions that are not
//     direct projection outputs (e.g. ORDER BY n.age after RETURN n).
//  3. If both fail, the item is skipped (callers treat empty result as
//     "no sort needed").
//
// The g, params, reg, and bopts arguments are used only when compiling an
// expression evaluator (case 2). Pass nil/zero values when the caller does
// not have access to them; in that case only direct schema lookups succeed.
func irSortKeys(
	items []ir.SortItem,
	schema map[string]int,
	g *lpg.Graph[string, float64],
	params map[string]expr.Value,
	reg expr.FunctionRegistry,
	bopts *buildOpts,
) []exec.SortKey {
	keys := make([]exec.SortKey, 0, len(items))
	for _, si := range items {
		// Case 1: direct lookup by expression string.
		if col, ok := schema[si.Expression]; ok {
			keys = append(keys, exec.SortKey{
				ColIdx:    col,
				Ascending: !si.Descending,
			})
			continue
		}

		// Case 2: compile an expression evaluator from the stored AST node.
		if si.Expr != nil {
			// Snapshot the schema and capture all dependencies so the closure
			// evaluates correctly against the row layout produced by the child.
			schemaCopy := make(map[string]int, len(schema))
			for k, v := range schema {
				schemaCopy[k] = v
			}
			capturedExpr := si.Expr
			capturedG := g
			capturedParams := params
			capturedReg := reg
			capturedBopts := bopts
			ascending := !si.Descending
			keys = append(keys, exec.SortKey{
				Ascending: ascending,
				Eval: func(row exec.Row) (expr.Value, error) {
					rowCtx := buildRowCtx(row, schemaCopy, capturedG, capturedBopts)
					return evalRow(capturedBopts, capturedExpr, rowCtx, capturedParams, capturedReg)
				},
			})
			continue
		}

		// Case 3: unresolvable — skip.
	}
	return keys
}

// buildProcedureCallOperator builds a ProcedureCallOp from an *ir.ProcedureCall node.
// When procReg is nil, it falls back to an empty registry which will return
// ErrProcNotFound at runtime.
func buildProcedureCallOperator(
	p *ir.ProcedureCall,
	walker nodeWalkerIface,
	labelSrc labelResolverIface,
	reg expr.FunctionRegistry,
	params map[string]expr.Value,
	schema map[string]int,
	idxMgr *index.Manager,
	procReg *procs.Registry,
	argByTag map[uint32]*exec.Argument,
	bopts *buildOpts,
) (exec.Operator, error) {
	// Build child if present.
	var child exec.Operator
	if p.Child != nil {
		var err error
		child, err = buildOperator(p.Child, walker, labelSrc, reg, params, schema, idxMgr, procReg, argByTag, bopts)
		if err != nil {
			return nil, err
		}
	}

	// Build argument evaluators. Each argument string is either a
	// variable reference resolved via the current schema, or a literal
	// (quoted string, integer, float, boolean, null) materialised once
	// at plan-build time. The latter is the common case for TCK
	// fixtures and any hand-written CALL with explicit constants.
	argEvals := make([]func(exec.Row) (expr.Value, error), len(p.Arguments))
	for i, argStr := range p.Arguments {
		argEvals[i] = buildProcArgEvaluator(argStr, schema)
	}

	// Resolve effective registry (never nil at runtime to avoid nil deref).
	effectiveProcReg := procReg
	if effectiveProcReg == nil {
		effectiveProcReg = procs.NewRegistry()
	}

	// Look up the procedure signature to determine YIELD columns.
	entry, err := effectiveProcReg.Lookup(p.Namespace, p.Name)
	if err != nil {
		return nil, fmt.Errorf("cypher: ProcedureCall %q: %w", p.Name, err)
	}

	// Determine yield variables: explicit YIELD wins; otherwise emit all output columns.
	yieldVars := p.YieldVars
	if len(yieldVars) == 0 {
		yieldVars = make([]string, len(entry.Sig.Outputs))
		for i, out := range entry.Sig.Outputs {
			yieldVars[i] = out.Name
		}
	}

	// Register output columns in the schema.
	for _, v := range yieldVars {
		schema[v] = schemaWidth(schema)
	}

	return exec.NewProcedureCallOp(p.Namespace, p.Name, argEvals, yieldVars, child, effectiveProcReg), nil
}

// buildProcArgEvaluator returns a row-evaluator for a single CALL
// argument string. Variable references that resolve in schema return
// row[idx]; primitive literals (quoted strings, integers, floats,
// booleans, null) are decoded once at plan-build time and the resulting
// [expr.Value] is captured by the closure. Unrecognised forms fall
// through to expr.Null so the procedure impl can still surface a
// typed-error message if the value is critical.
func buildProcArgEvaluator(argStr string, schema map[string]int) func(exec.Row) (expr.Value, error) {
	if colIdx, ok := schema[argStr]; ok {
		idx := colIdx
		return func(row exec.Row) (expr.Value, error) {
			if idx < len(row) {
				return row[idx], nil
			}
			return expr.Null, nil
		}
	}
	if lit, ok := parseProcArgLiteral(argStr); ok {
		return func(_ exec.Row) (expr.Value, error) { return lit, nil }
	}
	return func(_ exec.Row) (expr.Value, error) { return expr.Null, nil }
}

// parseProcArgLiteral recognises the primitive Cypher literal forms
// that may appear as a CALL argument: quoted single-/double-quoted
// strings, decimal integers, decimal floats, the boolean keywords and
// the null keyword. Returns (value, true) on recognition; (zero, false)
// when the string is not a primitive literal — the caller falls back
// to a variable lookup or a Null placeholder.
func parseProcArgLiteral(s string) (expr.Value, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, false
	}
	switch s {
	case "null", "NULL":
		return expr.Null, true
	case "true", "TRUE":
		return expr.BoolValue(true), true
	case "false", "FALSE":
		return expr.BoolValue(false), true
	}
	if len(s) >= 2 && (s[0] == '\'' || s[0] == '"') && s[len(s)-1] == s[0] {
		return expr.StringValue(s[1 : len(s)-1]), true
	}
	if strings.ContainsAny(s, ".eE") {
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return expr.FloatValue(f), true
		}
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return expr.IntegerValue(n), true
	}
	return nil, false
}

// buildIndexSeekOperator builds an exec.NodeByIndexSeek from an IR NodeByIndexSeek node.
// The Value field may be a parameter reference ($name) or a literal string/integer.
func buildIndexSeekOperator(
	p *ir.NodeByIndexSeek,
	params map[string]expr.Value,
	schema map[string]int,
	idxMgr *index.Manager,
) (exec.Operator, error) {
	seekVal, err := resolveSeekValue(p.Value, params)
	if err != nil {
		return nil, fmt.Errorf("cypher: NodeByIndexSeek %q: %w", p.NodeVar, err)
	}
	if idxMgr == nil {
		return nil, fmt.Errorf("cypher: NodeByIndexSeek requires an index manager")
	}
	names := idxMgr.ListIndexes()
	for _, name := range names {
		sub, err := idxMgr.GetIndex(name)
		if err != nil || sub.Kind() != "hash" {
			continue
		}
		if op, ok := tryNewHashSeek(sub, seekVal); ok {
			schema[p.NodeVar] = schemaWidth(schema)
			return op, nil
		}
	}
	return nil, fmt.Errorf("cypher: no hash index found for NodeByIndexSeek on %q.%q", p.Label, p.Property)
}

// tryBuildIndexSeekFromSelection inspects a Selection predicate for the pattern
// "n.prop = $name" or "$name = n.prop" over an AllNodesScan or NodeByLabelScan
// child, and when a hash index is available returns a NodeByIndexSeek operator.
// ok is false when the rewrite does not apply.
func tryBuildIndexSeekFromSelection(
	sel *ir.Selection,
	params map[string]expr.Value,
	schema map[string]int,
	idxMgr *index.Manager,
) (exec.Operator, bool, error) {
	nodeVar, label, ok := scanLeafNodeVar(sel.Child)
	if !ok {
		return nil, false, nil
	}
	seekVal, propKey, err := extractSeekFromSelection(sel, params, nodeVar)
	if err != nil || seekVal == nil {
		return nil, false, err
	}
	if op, ok := tryNamedHashSeek(idxMgr, label, propKey, seekVal); ok {
		schema[nodeVar] = schemaWidth(schema)
		return op, true, nil
	}
	if op, ok := tryAnyHashSeek(idxMgr, seekVal); ok {
		schema[nodeVar] = schemaWidth(schema)
		return op, true, nil
	}
	return nil, false, nil
}

// scanLeafNodeVar returns (nodeVar, label, true) when child is a
// bare scan leaf (AllNodesScan or NodeByLabelScan); (_, _, false)
// for any other operator type. Label is "" for the unlabelled
// AllNodesScan case.
func scanLeafNodeVar(child ir.LogicalPlan) (nodeVar, label string, ok bool) {
	switch c := child.(type) {
	case *ir.AllNodesScan:
		return c.NodeVar, "", true
	case *ir.NodeByLabelScan:
		return c.NodeVar, c.Label, true
	default:
		return "", "", false
	}
}

// extractSeekFromSelection walks the Selection's predicate in two
// passes — the parameterised n.prop = $param shorthand first, then
// a general AST-based extraction — and returns (seekVal, propKey,
// nil) on the first success. The seek value is nil when no
// suitable equality predicate is present; the caller treats that
// as "this Selection is not index-seekable" and falls back to the
// child scan.
func extractSeekFromSelection(
	sel *ir.Selection,
	params map[string]expr.Value,
	nodeVar string,
) (expr.Value, string, error) {
	if paramName, pk := extractEqParamFromPredicate(sel.Predicate, nodeVar); paramName != "" && pk != "" {
		sv, err := resolveSeekValue("$"+paramName, params)
		if err != nil {
			return nil, "", err
		}
		return sv, pk, nil
	}
	if sel.PredicateExpr != nil {
		if pk, sv, ok := extractEqFromAST(sel.PredicateExpr, nodeVar, params); ok {
			return sv, pk, nil
		}
	}
	return nil, "", nil
}

// tryNamedHashSeek looks up the auto-named hash index for a (label,
// propKey) pair and returns the seek operator + true when present
// and applicable to seekVal.
func tryNamedHashSeek(idxMgr *index.Manager, label, propKey string, seekVal expr.Value) (exec.Operator, bool) {
	if label == "" || propKey == "" {
		return nil, false
	}
	wantName := strings.ToLower(label) + "_" + strings.ToLower(propKey) + "_hash"
	sub, err := idxMgr.GetIndex(wantName)
	if err != nil || sub.Kind() != "hash" {
		return nil, false
	}
	return tryNewHashSeek(sub, seekVal)
}

// tryAnyHashSeek iterates every registered index and returns the
// first hash index that can serve seekVal. It is the fallback when
// the named-index lookup misses.
func tryAnyHashSeek(idxMgr *index.Manager, seekVal expr.Value) (exec.Operator, bool) {
	for _, name := range idxMgr.ListIndexes() {
		sub, err := idxMgr.GetIndex(name)
		if err != nil || sub.Kind() != "hash" {
			continue
		}
		if op, ok := tryNewHashSeek(sub, seekVal); ok {
			return op, true
		}
	}
	return nil, false
}

// extractEqFromAST extracts (propKey, seekVal, true) from a
// BinaryOp{=, Property{nodeVar, key}, literal/param} or its mirror form.
// params may be nil.
func extractEqFromAST(
	predExpr ast.Expression,
	nodeVar string,
	params map[string]expr.Value,
) (propKey string, seekVal expr.Value, ok bool) {
	binOp, ok2 := predExpr.(*ast.BinaryOp)
	if !ok2 || binOp.Operator != "=" {
		return "", nil, false
	}
	// Try left=Property, right=value.
	if prop, isP := binOp.Left.(*ast.Property); isP {
		if varNode, isV := prop.Receiver.(*ast.Variable); isV && varNode.Name == nodeVar {
			if v, err := astLiteralToValue(binOp.Right, params); err == nil {
				return prop.Key, v, true
			}
		}
	}
	// Try right=Property, left=value (mirror form).
	if prop, isP := binOp.Right.(*ast.Property); isP {
		if varNode, isV := prop.Receiver.(*ast.Variable); isV && varNode.Name == nodeVar {
			if v, err := astLiteralToValue(binOp.Left, params); err == nil {
				return prop.Key, v, true
			}
		}
	}
	return "", nil, false
}

// astLiteralToValue converts an AST leaf (string/int/float/bool literal or
// parameter reference) to an expr.Value. Returns a non-nil error for any other
// expression type.
func astLiteralToValue(e ast.Expression, params map[string]expr.Value) (expr.Value, error) {
	switch v := e.(type) {
	case *ast.StringLiteral:
		return expr.StringValue(v.Value), nil
	case *ast.IntLiteral:
		return expr.IntegerValue(v.Value), nil
	case *ast.FloatLiteral:
		return expr.FloatValue(v.Value), nil
	case *ast.BoolLiteral:
		return expr.BoolValue(v.Value), nil
	case *ast.Parameter:
		if params != nil {
			if pv, ok := params[v.Name]; ok {
				return pv, nil
			}
		}
		return expr.Null, nil
	}
	return nil, fmt.Errorf("not a literal: %T", e)
}

// extractEqParamFromPredicate parses the opaque predicate string to extract
// the parameter name and property key from an equality of form
// "(nodeVar.prop = $name)" or "($name = nodeVar.prop)".
// Returns ("", "") when the predicate does not match.
func extractEqParamFromPredicate(pred, nodeVar string) (paramName, propKey string) {
	pred = strings.TrimSpace(pred)
	if strings.HasPrefix(pred, "(") && strings.HasSuffix(pred, ")") {
		pred = pred[1 : len(pred)-1]
	}
	idx := strings.Index(pred, " = ")
	if idx < 0 {
		return "", ""
	}
	left := strings.TrimSpace(pred[:idx])
	right := strings.TrimSpace(pred[idx+3:])

	prefix := nodeVar + "."
	if strings.HasPrefix(left, prefix) && strings.HasPrefix(right, "$") {
		return right[1:], left[len(prefix):]
	}
	if strings.HasPrefix(right, prefix) && strings.HasPrefix(left, "$") {
		return left[1:], right[len(prefix):]
	}
	return "", ""
}

// resolveSeekValue resolves p.Value to an expr.Value. If value starts with "$",
// it is a parameter reference looked up in params. Otherwise it is parsed as a
// literal (string in single or double quotes, boolean, integer).
func resolveSeekValue(value string, params map[string]expr.Value) (expr.Value, error) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "$") {
		name := value[1:]
		if params != nil {
			if v, ok := params[name]; ok {
				return v, nil
			}
		}
		return expr.Null, nil // unbound parameter resolves to NULL
	}
	// Parse literal: delegate to exec's existing literal parser via a minimal
	// inline implementation (string, bool, integer).
	if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') {
		return expr.StringValue(value[1 : len(value)-1]), nil
	}
	if value == "true" {
		return expr.BoolValue(true), nil
	}
	if value == "false" {
		return expr.BoolValue(false), nil
	}
	// Try integer.
	var n int64
	if _, err := fmt.Sscan(value, &n); err == nil {
		return expr.IntegerValue(n), nil
	}
	return nil, fmt.Errorf("unsupported seek value %q", value)
}

// hashStringLookup is satisfied by hash.Index[string].
type hashStringLookup interface {
	Lookup(value string) *roaring64.Bitmap
}

// hashInt64Lookup is satisfied by hash.Index[int64].
type hashInt64Lookup interface {
	Lookup(value int64) *roaring64.Bitmap
}

// tryNewHashSeek attempts to build a NodeByIndexSeek operator using sub as the
// hash index. It returns (nil, false) when sub is not a supported hash type.
func tryNewHashSeek(sub index.Subscriber, seekVal expr.Value) (*exec.NodeByIndexSeek, bool) {
	if sl, ok := sub.(hashStringLookup); ok {
		return exec.NewNodeByIndexSeek(exec.NewStringHashIndex(sl), seekVal), true
	}
	if il, ok := sub.(hashInt64Lookup); ok {
		return exec.NewNodeByIndexSeek(exec.NewInt64HashIndex(il), seekVal), true
	}
	return nil, false
}

// lpgPropToExpr converts an lpg.PropertyValue to its expr.Value counterpart.
// Unsupported kinds (PropTime, PropBytes) fall through to expr.Null.
//
// PropString values that begin with a SOH-range byte (0x01..0x06) are decoded
// as temporal values, mirroring the encoding performed by
// [cypher/exec.parseTemporalLiteral]. The tag→kind mapping is:
//
//	0x01 → DateValue
//	0x02 → LocalDateTimeValue
//	0x03 → DateTimeValue
//	0x04 → LocalTimeValue
//	0x05 → TimeValue
//	0x06 → DurationValue
//
// When the body is malformed the value falls back to a plain StringValue
// carrying the raw bytes; this keeps reads forward-safe in the unlikely event
// that a legacy WAL contains a byte-collision payload.
func lpgPropToExpr(pv lpg.PropertyValue) expr.Value {
	switch pv.Kind() {
	case lpg.PropString:
		if s, ok := pv.String(); ok {
			if v, decoded := decodeTemporalString(s); decoded {
				return v
			}
			return expr.StringValue(s)
		}
	case lpg.PropInt64:
		if i, ok := pv.Int64(); ok {
			return expr.IntegerValue(i)
		}
	case lpg.PropFloat64:
		if f, ok := pv.Float64(); ok {
			return expr.FloatValue(f)
		}
	case lpg.PropBool:
		if b, ok := pv.Bool(); ok {
			return expr.BoolValue(b)
		}
	case lpg.PropList:
		if elems, ok := pv.List(); ok {
			lv := make(expr.ListValue, len(elems))
			for i, elem := range elems {
				lv[i] = lpgPropToExpr(elem)
			}
			return lv
		}
	}
	return expr.Null
}

// decodeTemporalString recognises the SOH-range tag introduced by
// cypher/exec/temporal_literal.go and returns the matching temporal Value.
// Returns (nil, false) when s does not start with a recognised tag byte.
func decodeTemporalString(s string) (expr.Value, bool) {
	if len(s) < 2 {
		return nil, false
	}
	body := s[1:]
	switch s[0] {
	case 0x01:
		if v, err := expr.ParseDate(body); err == nil {
			return v, true
		}
	case 0x02:
		if v, err := expr.ParseLocalDateTime(body); err == nil {
			return v, true
		}
	case 0x03:
		if v, err := expr.ParseDateTime(body); err == nil {
			return v, true
		}
	case 0x04:
		if v, err := expr.ParseLocalTime(body); err == nil {
			return v, true
		}
	case 0x05:
		if v, err := expr.ParseTime(body); err == nil {
			return v, true
		}
	case 0x06:
		if v, err := expr.ParseDuration(body); err == nil {
			return v, true
		}
	}
	return nil, false
}

// buildUnwindOperator builds the physical Unwind operator from the IR node.
//
// When the IR carries a parsed AST expression (p.ListExpr != nil), it is
// evaluated via [expr.Eval] against a row context derived from the current
// schema snapshot. When the evaluated value is a [expr.ListValue] the
// elements are expanded; when it is NULL or any non-list type no rows are
// emitted (openCypher semantics). When p.ListExpr is nil (expression could not
// be parsed), the operator always emits an empty expansion.
func buildUnwindOperator(
	p *ir.Unwind,
	child exec.Operator,
	schema map[string]int,
	walker nodeWalkerIface,
	params map[string]expr.Value,
	reg expr.FunctionRegistry,
	bopts *buildOpts,
) (exec.Operator, error) {
	if p.ListExpr == nil {
		// No AST available — emit nothing for every input row.
		return exec.NewUnwind(child, func(_ exec.Row) (expr.ListValue, error) {
			return nil, nil
		})
	}

	var g *lpg.Graph[string, float64]
	if lw, ok := walker.(*lpgNodeWalker); ok {
		g = lw.g
	}

	schemaSnap := copySchema(schema)
	listExpr := p.ListExpr
	capturedParams := params
	capturedReg := reg
	capturedG := g
	capturedBopts := bopts

	return exec.NewUnwind(child, func(row exec.Row) (expr.ListValue, error) {
		rowCtx := buildRowCtx(row, schemaSnap, capturedG, capturedBopts)
		v, err := evalRow(capturedBopts, listExpr, rowCtx, capturedParams, capturedReg)
		if err != nil {
			return nil, err
		}
		if v == expr.Null || v == nil {
			return nil, nil
		}
		lv, ok := v.(expr.ListValue)
		if !ok {
			// Per openCypher: UNWIND on a non-list scalar wraps it in a single-element list.
			return expr.ListValue{v}, nil
		}
		return lv, nil
	})
}

// upgradeNodeIDToValue upgrades a row cell from expr.IntegerValue(NodeID) to a
// full expr.NodeValue carrying labels and properties. The upgrade fires only
// when v is an expr.IntegerValue, g is non-nil, and the mapper resolves the
// integer to a known natural key — i.e. only when the row cell genuinely
// references a graph node. In every other case (nil graph, non-IntegerValue,
// IntegerValue that is not a NodeID such as a literal-integer projection or a
// relationship edge ID) the value is returned unchanged so callers can rely on
// "value-passthrough unless we can prove it's a node".
//
// Relationships are not upgraded here. The engine emits a relationship as three
// separate IntegerValue columns (srcID, edgeID, dstID) and the schema carries
// no per-column kind information, so RelationshipValue construction needs
// schema-level type metadata that this helper deliberately does not touch.
func upgradeNodeIDToValue(v expr.Value, g *lpg.Graph[string, float64]) expr.Value {
	if g == nil {
		return v
	}
	iv, ok := v.(expr.IntegerValue)
	if !ok {
		return v
	}
	id := graph.NodeID(iv)
	name, resolved := g.AdjList().Mapper().Resolve(id)
	if !resolved {
		return v
	}
	rawProps := g.NodeProperties(name)
	props := make(expr.MapValue, len(rawProps))
	for k, pv := range rawProps {
		props[k] = lpgPropToExpr(pv)
	}
	labels := g.NodeLabels(name)
	return expr.NodeValue{ID: uint64(id), Labels: labels, Properties: props}
}

// buildNodeValueFromID constructs an expr.NodeValue for a known graph NodeID,
// loading labels and properties from g. If the ID is not found in the mapper,
// an empty NodeValue with only the ID set is returned.
func buildNodeValueFromID(id graph.NodeID, g *lpg.Graph[string, float64]) expr.NodeValue {
	if g == nil {
		return expr.NodeValue{ID: uint64(id)}
	}
	name, resolved := g.AdjList().Mapper().Resolve(id)
	if !resolved {
		return expr.NodeValue{ID: uint64(id)}
	}
	rawProps := g.NodeProperties(name)
	props := make(expr.MapValue, len(rawProps))
	for k, pv := range rawProps {
		props[k] = lpgPropToExpr(pv)
	}
	labels := g.NodeLabels(name)
	return expr.NodeValue{ID: uint64(id), Labels: labels, Properties: props}
}

// buildRowCtx converts a row plus a schema snapshot into an expr.RowContext,
// upgrading IntegerValue(nodeID) entries to NodeValue with properties loaded
// from the graph. g may be nil when no graph is available (upgrade is
// skipped). When bopts carries edgeVarMeta entries (T937) the relationship
// variables they describe are reconstructed as full RelationshipValues with
// their typed properties loaded from the graph, so property-access
// expressions such as `r.since` resolve through the bound relationship.
func buildRowCtx(row exec.Row, schema map[string]int, g *lpg.Graph[string, float64], bopts *buildOpts) expr.RowContext {
	ctx := make(expr.RowContext, len(schema))
	for varName, colIdx := range schema {
		if colIdx >= len(row) || row[colIdx] == nil {
			continue
		}
		if bopts != nil && bopts.edgeVarMeta != nil {
			if meta, isEdge := bopts.edgeVarMeta[varName]; isEdge {
				if rv, ok := buildRelationshipValueFromRow(row, meta, g); ok {
					ctx[varName] = rv
					continue
				}
			}
		}
		ctx[varName] = upgradeNodeIDToValue(row[colIdx], g)
	}
	return ctx
}

// buildRelationshipValueFromRow reconstructs a [expr.RelationshipValue] from
// the (srcCol, edgeCol, dstCol) triplet emitted by the [exec.Expand]
// operator. The edge type and Properties are looked up on the live graph
// when both endpoints resolve. Returns (zero, false) when the row does not
// contain the expected columns or when the column types are not
// IntegerValue.
func buildRelationshipValueFromRow(row exec.Row, meta edgeVarInfo, g *lpg.Graph[string, float64]) (expr.RelationshipValue, bool) {
	if meta.edgeCol >= len(row) {
		return expr.RelationshipValue{}, false
	}
	edgeIDVal, ok := row[meta.edgeCol].(expr.IntegerValue)
	if !ok {
		return expr.RelationshipValue{}, false
	}
	var srcID, dstID uint64
	if meta.srcCol < len(row) {
		if iv, ok2 := row[meta.srcCol].(expr.IntegerValue); ok2 {
			srcID = uint64(iv)
		}
	}
	if meta.dstCol < len(row) {
		if iv, ok2 := row[meta.dstCol].(expr.IntegerValue); ok2 {
			dstID = uint64(iv)
		}
	}
	edgeType := meta.edgeType
	var edgeProps expr.MapValue
	if g != nil && srcID != 0 {
		srcKey, srcResolved := g.AdjList().Mapper().Resolve(graph.NodeID(srcID))
		dstKey, dstResolved := g.AdjList().Mapper().Resolve(graph.NodeID(dstID))
		if srcResolved && dstResolved {
			// Forward direction first: covers the common case of a directed
			// expansion or the forward pass of an undirected expansion.
			ets := g.EdgeLabels(srcKey, dstKey)
			rawEP := g.EdgeProperties(srcKey, dstKey)
			if len(ets) == 0 && len(rawEP) == 0 {
				// Reverse-edge pass of an undirected expansion: storage
				// holds the edge as (dstKey -> srcKey); look it up there
				// so the relationship's type and properties survive
				// reverse traversal (e.g. the b→a row of MATCH (a)-[r]-(b)
				// over a single (:A)-[:T]->(:B) edge).
				ets = g.EdgeLabels(dstKey, srcKey)
				rawEP = g.EdgeProperties(dstKey, srcKey)
			}
			if len(ets) > 0 {
				edgeType = ets[0]
			}
			edgeProps = make(expr.MapValue, len(rawEP))
			for k, pv := range rawEP {
				edgeProps[k] = lpgPropToExpr(pv)
			}
		}
	}
	return expr.RelationshipValue{
		ID:         uint64(edgeIDVal),
		StartID:    srcID,
		EndID:      dstID,
		Type:       edgeType,
		Properties: edgeProps,
	}, true
}

// buildIRProjection converts IR ProjectionItems to a physical Project operator.
// When an item carries a parsed AST expression (item.Expr != nil), the
// executor evaluates it via expr.Eval against a full RowContext — enabling
// property access (n.prop), function calls, and other non-trivial expressions.
// For simple variable references and string-only items the fast schema-lookup
// path is used. The variable fast-path handles plain nodes, relationship
// variables (RelationshipValue reconstruction from edge metadata), and named
// path variables (PathValue reconstruction from flat alternating encoding).
//
//nolint:gocyclo,cyclop // dispatches over every projection kind and variable type; splitting would obscure the data-flow
func buildIRProjection(
	items []ir.ProjectionItem,
	child exec.Operator,
	schema map[string]int,
	g *lpg.Graph[string, float64],
	params map[string]expr.Value,
	reg expr.FunctionRegistry,
	bopts *buildOpts,
) (*exec.Project, error) {
	projItems := make([]exec.ProjectionItem, len(items))
	for i, item := range items {
		name := item.Name
		exprStr := item.Expression

		var evalFn func(exec.Row) (expr.Value, error)
		if item.Expr != nil {
			if v, ok := item.Expr.(*ast.Variable); ok {
				// VLE relationship-list fast path: reconstruct a list of
				// RelationshipValues from the flat alternating ListValue
				// emitted by VarLengthExpand into the rel-variable column.
				if bopts != nil {
					if rmeta, isVLERel := bopts.vleRelMeta[v.Name]; isVLERel {
						capturedMeta := rmeta
						capturedG := g
						evalFn = func(row exec.Row) (expr.Value, error) {
							if capturedMeta.listCol >= len(row) {
								return expr.Null, nil
							}
							lv, ok := row[capturedMeta.listCol].(expr.ListValue)
							if !ok {
								return expr.Null, nil
							}
							if len(lv) == 0 {
								// Empty path (0 hops, possible with *0..0 patterns).
								return expr.ListValue{}, nil
							}
							// Flat format: [srcID, edgePos0, dst0, edgePos1, dst1, …].
							nHops := (len(lv) - 1) / 2
							rels := make(expr.ListValue, 0, nHops)
							srcID := uint64(0)
							if iv, ok2 := lv[0].(expr.IntegerValue); ok2 {
								srcID = uint64(iv)
							}
							for h := 0; h < nHops; h++ {
								edgeID, ok1 := lv[1+2*h].(expr.IntegerValue)
								dstIDVal, ok2 := lv[2+2*h].(expr.IntegerValue)
								if !ok1 || !ok2 {
									continue
								}
								dstID := uint64(dstIDVal)
								et := capturedMeta.edgeType
								var edgeProps expr.MapValue
								if capturedG != nil {
									srcKey, sOK := capturedG.AdjList().Mapper().Resolve(graph.NodeID(srcID))
									dstKey, dOK := capturedG.AdjList().Mapper().Resolve(graph.NodeID(dstID))
									if sOK && dOK {
										if ets := capturedG.EdgeLabels(srcKey, dstKey); len(ets) > 0 {
											et = ets[0]
										} else if ets := capturedG.EdgeLabels(dstKey, srcKey); len(ets) > 0 {
											// Reverse-edge fall-back.
											et = ets[0]
										}
										rawEP := capturedG.EdgeProperties(srcKey, dstKey)
										if len(rawEP) == 0 {
											rawEP = capturedG.EdgeProperties(dstKey, srcKey)
										}
										edgeProps = make(expr.MapValue, len(rawEP))
										for k, pv := range rawEP {
											edgeProps[k] = lpgPropToExpr(pv)
										}
									}
								}
								rels = append(rels, expr.RelationshipValue{
									ID:         uint64(edgeID),
									StartID:    srcID,
									EndID:      dstID,
									Type:       et,
									Properties: edgeProps,
								})
								srcID = dstID
							}
							return rels, nil
						}
					}
				}
				// Path variable fast path: reconstruct PathValue from the flat
				// alternating ListValue emitted by the VarLengthExpand operator.
				if bopts != nil && evalFn == nil {
					if pmeta, isPMeta := bopts.pathVarMeta[v.Name]; isPMeta {
						capturedMeta := pmeta
						capturedG := g
						evalFn = func(row exec.Row) (expr.Value, error) {
							if capturedMeta.listCol >= len(row) {
								return expr.Null, nil
							}
							lv, ok := row[capturedMeta.listCol].(expr.ListValue)
							if !ok || len(lv) == 0 {
								return expr.Null, nil
							}
							// Flat alternating format: [srcID, edgePos0, dst0, edgePos1, dst1, ...]
							// len = 1 + 2*N for N hops.
							nHops := (len(lv) - 1) / 2
							nodes := make([]expr.NodeValue, 0, nHops+1)
							rels := make([]expr.RelationshipValue, 0, nHops)
							if iv, ok2 := lv[0].(expr.IntegerValue); ok2 {
								nodes = append(nodes, buildNodeValueFromID(graph.NodeID(iv), capturedG))
							}
							edgeType := capturedMeta.edgeType
							for h := 0; h < nHops; h++ {
								edgePos, ok1 := lv[1+2*h].(expr.IntegerValue)
								dstIDVal, ok2 := lv[2+2*h].(expr.IntegerValue)
								if !ok1 || !ok2 {
									continue
								}
								dstNode := buildNodeValueFromID(graph.NodeID(dstIDVal), capturedG)
								nodes = append(nodes, dstNode)
								// Resolve edge type from graph if known.
								et := edgeType
								var edgeProps expr.MapValue
								if capturedG != nil && len(nodes) >= 2 {
									srcNodeID := nodes[h].ID
									dstNodeID := nodes[h+1].ID
									srcKey, sOK := capturedG.AdjList().Mapper().Resolve(graph.NodeID(srcNodeID))
									dstKey, dOK := capturedG.AdjList().Mapper().Resolve(graph.NodeID(dstNodeID))
									if sOK && dOK {
										if ets := capturedG.EdgeLabels(srcKey, dstKey); len(ets) > 0 {
											et = ets[0]
										}
										rawEP := capturedG.EdgeProperties(srcKey, dstKey)
										edgeProps = make(expr.MapValue, len(rawEP))
										for k, pv := range rawEP {
											edgeProps[k] = lpgPropToExpr(pv)
										}
									}
								}
								rels = append(rels, expr.RelationshipValue{
									ID:         uint64(edgePos),
									StartID:    nodes[h].ID,
									EndID:      dstNode.ID,
									Type:       et,
									Properties: edgeProps,
								})
							}
							return expr.PathValue{Nodes: nodes, Relationships: rels}, nil
						}
					}
				}
				// Named-path (chain) fast path: reconstruct PathValue from the
				// alternating (srcID, edgeID, dstID) triplets emitted by the
				// fixed-length Expand chain. This covers zero-length
				// (p = (a)), fixed-length (p = (a)-[r]->(b)) and chained
				// (p = (a)-[r1]->(b)-[r2]->(c)) named paths.
				//
				// The fast path is only valid for the FIRST projection above
				// the NamedPath wrapper, while the input row still carries
				// the chain's source columns. Once that projection emits a
				// real PathValue into a new schema slot the entry is removed
				// from pathVarChain so subsequent projections (e.g. RETURN
				// after a WITH) fall through to the regular schema-lookup
				// path.
				if bopts != nil && evalFn == nil {
					if cinfo, isChain := bopts.pathVarChain[v.Name]; isChain {
						capturedInfo := cinfo
						capturedG := g
						evalFn = func(row exec.Row) (expr.Value, error) {
							if capturedInfo.leadingCol >= len(row) {
								return expr.Null, nil
							}
							leadVal := row[capturedInfo.leadingCol]
							if leadVal == nil || expr.IsNull(leadVal) {
								return expr.Null, nil
							}
							// The leading slot may already carry an upgraded
							// NodeValue (when an earlier projection ran) or a
							// raw IntegerValue (from a scan). Both shapes are
							// accepted.
							var leadNode expr.NodeValue
							switch lv := leadVal.(type) {
							case expr.NodeValue:
								leadNode = lv
							case expr.IntegerValue:
								leadNode = buildNodeValueFromID(graph.NodeID(lv), capturedG)
							default:
								return expr.Null, nil
							}
							nodes := make([]expr.NodeValue, 0, len(capturedInfo.steps)+1)
							rels := make([]expr.RelationshipValue, 0, len(capturedInfo.steps))
							nodes = append(nodes, leadNode)
							for _, step := range capturedInfo.steps {
								if step.edgeCol >= len(row) || step.dstCol >= len(row) {
									return expr.Null, nil
								}
								edgeIDVal, ok1 := row[step.edgeCol].(expr.IntegerValue)
								dstIDVal, ok2 := row[step.dstCol].(expr.IntegerValue)
								if !ok1 || !ok2 {
									// OPTIONAL hops or otherwise-missing
									// columns collapse the path to NULL,
									// matching openCypher semantics.
									return expr.Null, nil
								}
								dstNode := buildNodeValueFromID(graph.NodeID(dstIDVal), capturedG)
								et := step.edgeType
								var edgeProps expr.MapValue
								// pathStart/pathEnd track the path's traversal
								// order (preceding node → current dst). The
								// edge's storage direction may run either way;
								// we probe EdgeLabels in both orientations and
								// record the real StartID/EndID so downstream
								// renderers can choose the correct arrow.
								pathStart := nodes[len(nodes)-1].ID
								pathEnd := dstNode.ID
								storageStart := pathStart
								storageEnd := pathEnd
								if capturedG != nil && pathStart != 0 {
									sKey, sOK := capturedG.AdjList().Mapper().Resolve(graph.NodeID(pathStart))
									dKey, dOK := capturedG.AdjList().Mapper().Resolve(graph.NodeID(pathEnd))
									if sOK && dOK {
										// Try forward (pathStart → pathEnd).
										if ets := capturedG.EdgeLabels(sKey, dKey); len(ets) > 0 {
											et = ets[0]
											rawEP := capturedG.EdgeProperties(sKey, dKey)
											edgeProps = make(expr.MapValue, len(rawEP))
											for k, pv := range rawEP {
												edgeProps[k] = lpgPropToExpr(pv)
											}
										} else if ets := capturedG.EdgeLabels(dKey, sKey); len(ets) > 0 {
											// Reverse storage: edge actually
											// stored pathEnd → pathStart.
											et = ets[0]
											rawEP := capturedG.EdgeProperties(dKey, sKey)
											edgeProps = make(expr.MapValue, len(rawEP))
											for k, pv := range rawEP {
												edgeProps[k] = lpgPropToExpr(pv)
											}
											storageStart = pathEnd
											storageEnd = pathStart
										}
									}
								}
								rels = append(rels, expr.RelationshipValue{
									ID:         uint64(edgeIDVal),
									StartID:    storageStart,
									EndID:      storageEnd,
									Type:       et,
									Properties: edgeProps,
								})
								nodes = append(nodes, dstNode)
							}
							return expr.PathValue{Nodes: nodes, Relationships: rels}, nil
						}
						// Mark this path variable as consumed so a subsequent
						// projection over the same name (e.g. RETURN p after a
						// WITH p) reads the freshly-projected PathValue
						// column directly instead of attempting to re-evaluate
						// the chain against the post-projection row layout.
						delete(bopts.pathVarChain, v.Name)
					}
				}
				// Edge variable fast path: reconstruct RelationshipValue from
				// the three-column triplet (srcID, edgeID, dstID) emitted by
				// the Expand operator.
				if bopts != nil && evalFn == nil {
					if meta, isMeta := bopts.edgeVarMeta[v.Name]; isMeta {
						capturedMeta := meta
						capturedG := g
						evalFn = func(row exec.Row) (expr.Value, error) {
							if capturedMeta.edgeCol >= len(row) {
								return expr.Null, nil
							}
							edgeIDVal, ok := row[capturedMeta.edgeCol].(expr.IntegerValue)
							if !ok {
								return expr.Null, nil
							}
							edgeID := uint64(edgeIDVal)
							var srcID, dstID uint64
							if capturedMeta.srcCol < len(row) {
								if iv, ok2 := row[capturedMeta.srcCol].(expr.IntegerValue); ok2 {
									srcID = uint64(iv)
								}
							}
							if capturedMeta.dstCol < len(row) {
								if iv, ok2 := row[capturedMeta.dstCol].(expr.IntegerValue); ok2 {
									dstID = uint64(iv)
								}
							}
							// Resolve edge type from the graph if not statically known.
							edgeType := capturedMeta.edgeType
							var edgeProps expr.MapValue
							if capturedG != nil && srcID != 0 {
								srcKey, srcResolved := capturedG.AdjList().Mapper().Resolve(graph.NodeID(srcID))
								dstKey, dstResolved := capturedG.AdjList().Mapper().Resolve(graph.NodeID(dstID))
								if srcResolved && dstResolved {
									if ets := capturedG.EdgeLabels(srcKey, dstKey); len(ets) > 0 {
										edgeType = ets[0]
									}
									rawEP := capturedG.EdgeProperties(srcKey, dstKey)
									edgeProps = make(expr.MapValue, len(rawEP))
									for k, pv := range rawEP {
										edgeProps[k] = lpgPropToExpr(pv)
									}
								}
							}
							return expr.RelationshipValue{
								ID:         edgeID,
								StartID:    srcID,
								EndID:      dstID,
								Type:       edgeType,
								Properties: edgeProps,
							}, nil
						}
					}
				}
				if evalFn == nil {
					// Node variable fast path: direct column lookup, with an in-line
					// IntegerValue(NodeID) → NodeValue upgrade so bare `RETURN u` for
					// a bound node produces the documented shape. Non-node
					// IntegerValues (literals, edge IDs) pass through unchanged.
					//
					// Exception: scalar columns (aggregate outputs such as count(*),
					// sum, avg) must NOT be upgraded — an integer count that
					// numerically equals a real NodeID would be mis-elevated into a
					// full graph node. bopts.scalarCols tracks which variable names
					// were produced by EagerAggregation and must pass through as-is.
					if colIdx, ok2 := schema[v.Name]; ok2 {
						idx := colIdx
						capturedG := g
						varIsScalar := bopts != nil && bopts.scalarCols != nil
						if varIsScalar {
							_, varIsScalar = bopts.scalarCols[v.Name]
						}
						if varIsScalar {
							// Scalar aggregate output: return the raw value without
							// upgrade. Integer counts/sums can numerically coincide with
							// a real NodeID and must not be elevated to NodeValue.
							evalFn = func(row exec.Row) (expr.Value, error) {
								if idx < len(row) {
									return row[idx], nil
								}
								return expr.Null, nil
							}
						} else {
							evalFn = func(row exec.Row) (expr.Value, error) {
								if idx < len(row) {
									return upgradeNodeIDToValue(row[idx], capturedG), nil
								}
								return expr.Null, nil
							}
						}
					}
				}
			}
			if evalFn == nil {
				// Schema-name fast path: when an upstream operator (e.g.
				// EagerAggregation) has pre-computed and named the output
				// column, prefer a direct index lookup over expression
				// re-evaluation. This avoids calling aggregate functions as
				// scalar functions. The IntegerValue(NodeID) → NodeValue
				// upgrade is deliberately NOT applied here: aggregate
				// results (count(*), sum(...), etc.) are scalar integers
				// that can numerically collide with a real NodeID and would
				// be mis-upgraded into a node row.
				//
				// Edge-variable carve-out: when the alias name maps to a
				// bound relationship variable (i.e. the schema slot carries
				// an Expand-emitted edge id rather than the projection's
				// evaluated value), the fast path is unsound. Without this
				// carve-out, `RETURN type(r) AS r` would bypass evaluation
				// of `type(r)` and return the raw IntegerValue edge id
				// instead of the relationship type label.
				aliasIsBoundRel := false
				if bopts != nil && bopts.edgeVarMeta != nil {
					_, aliasIsBoundRel = bopts.edgeVarMeta[name]
				}
				if !aliasIsBoundRel {
					if colIdx, ok2 := schema[name]; ok2 {
						idx := colIdx
						evalFn = func(row exec.Row) (expr.Value, error) {
							if idx < len(row) {
								return row[idx], nil
							}
							return expr.Null, nil
						}
					}
				}
			}
			if evalFn == nil {
				// General path: evaluate full AST expression with loaded RowContext.
				schemaSnap := copySchema(schema)
				capturedExpr := item.Expr
				capturedG := g
				capturedParams := params
				capturedReg := reg
				capturedBopts := bopts
				evalFn = func(row exec.Row) (expr.Value, error) {
					rowCtx := buildRowCtx(row, schemaSnap, capturedG, capturedBopts)
					return evalRow(capturedBopts, capturedExpr, rowCtx, capturedParams, capturedReg)
				}
			}
		} else if colIdx, ok := schema[exprStr]; ok {
			// String-only projection (no AST expression). Aggregate aliases
			// and pre-aggregated columns land here, so we cannot safely
			// upgrade IntegerValue → NodeValue (a scalar count that numerically
			// matches a NodeID would be mis-upgraded).
			idx := colIdx
			evalFn = func(row exec.Row) (expr.Value, error) {
				if idx < len(row) {
					return row[idx], nil
				}
				return expr.Null, nil
			}
		} else if colIdx, ok := schema[name]; ok {
			// Alias fallback. Same caveat as above — no kind information, no
			// upgrade.
			idx := colIdx
			evalFn = func(row exec.Row) (expr.Value, error) {
				if idx < len(row) {
					return row[idx], nil
				}
				return expr.Null, nil
			}
		} else {
			evalFn = func(_ exec.Row) (expr.Value, error) { return expr.Null, nil }
		}

		// Update schema: output variable maps to index i.
		schema[name] = i
		// Also register the expression string as a secondary key so that ORDER BY
		// clauses that reference the un-aliased expression (e.g. ORDER BY n.num
		// after RETURN n.num AS prop) can still resolve the correct column via
		// irSortKeys. Only added when exprStr differs from name to avoid redundant
		// writes; the primary alias always wins on any read.
		if exprStr != "" && exprStr != name {
			schema[exprStr] = i
		}
		projItems[i] = exec.ProjectionItem{Alias: name, Eval: evalFn}
	}
	return exec.NewProject(child, projItems)
}

// execLabelAdapter bridges labelResolverIface to the exec.labelResolver
// interface (which uses *roaring64.Bitmap). Both have identical method
// signatures; this thin wrapper avoids an import cycle.
type execLabelAdapter struct {
	labelSrc labelResolverIface
}

// ResolveLabelBitmap implements exec.labelResolver.
func (a *execLabelAdapter) ResolveLabelBitmap(name string) *roaring64.Bitmap {
	return a.labelSrc.ResolveLabelBitmap(name)
}

// ─────────────────────────────────────────────────────────────────────────────
// RunInTx — write-aware query execution
// ─────────────────────────────────────────────────────────────────────────────

// RunInTx executes a write query against the engine's graph and returns a
// streaming [Result]. Unlike [Run], RunInTx inspects the IR plan for write
// operators; when any write operator is present it builds a mutator adapter so
// that write operators can modify the graph.
//
// For the current in-memory implementation there is no external transaction
// manager (lpg.Graph does not support rollback). "Commit on success, rollback
// on error" means: the pipeline runs to completion with mutations applied
// eagerly; if any operator returns an error the pipeline is drained no further
// (standard Volcano error propagation) and the partial mutations remain in the
// graph. This matches the single-writer, in-memory contract documented in
// CLAUDE.md.
//
// RunInTx is safe for concurrent use (each call creates an independent
// operator tree), subject to the single-writer constraint on write queries.
func (e *Engine) RunInTx(ctx context.Context, query string, params map[string]expr.Value) (*Result, error) {
	// DDL queries don't require a write transaction.
	if ir.IsDDL(query) {
		return e.runDDL(ctx, query)
	}

	entry, err := e.parseAndAnalyse(query)
	if err != nil {
		return nil, err
	}
	// Sema fast-path: short-circuit scope violations before opening a tx.
	if entry.semaErr != nil {
		return nil, entry.semaErr
	}
	plan := entry.plan

	if len(params) > 0 {
		if err := sema.CheckParams(sema.InferParamTypes(plan), params); err != nil {
			return nil, err
		}
	}

	walker := &lpgNodeWalker{g: e.g}
	labelSrc := &lpgLabelResolver{g: e.g}
	buf := &exec.IndexBuffer{}

	var mutator exec.GraphMutator
	var walTx *txn.Tx[string, float64]
	if e.store != nil {
		walTx = e.store.Begin()
		mutator = &walMutatorAdapter{g: e.g, tx: walTx, buf: buf}
	} else {
		mutator = &lpgMutatorAdapter{g: e.g, buf: buf}
	}

	op, cols, err := buildPlanWithMutatorFull(plan, walker, labelSrc, e.reg, params, mutator, e.constraintReg, e.g.IndexManager())
	if err != nil {
		if walTx != nil {
			_ = walTx.Rollback()
		}
		return nil, fmt.Errorf("cypher: build plan: %w", err)
	}

	rs := exec.Run(ctx, op, cols)
	return newResult(rs, cols, buf, e.g.IndexManager(), walTx), nil
}

// ─────────────────────────────────────────────────────────────────────────────
// lpgMutatorAdapter — exec.graphMutator backed by *lpg.Graph[string,float64]
// ─────────────────────────────────────────────────────────────────────────────

// lpgMutatorAdapter adapts *lpg.Graph[string, float64] to the
// exec.graphMutator interface used by write operators.
//
// When buf is non-nil every mutation is also enqueued as an index.Change.
// buf is nil for read-only adapter instances.
type lpgMutatorAdapter struct {
	g   *lpg.Graph[string, float64]
	buf *exec.IndexBuffer // nil for read-only
}

// resolveID translates n to its stable NodeID, returning graph.NodeID(0)
// when the key has not been interned yet.
func (a *lpgMutatorAdapter) resolveID(n string) graph.NodeID {
	id, ok := a.g.AdjList().Mapper().Lookup(n)
	if !ok {
		return graph.NodeID(0)
	}
	return id
}

// AddNode interns n and returns its stable NodeID.
func (a *lpgMutatorAdapter) AddNode(n string) (graph.NodeID, error) {
	if err := a.g.AddNode(n); err != nil {
		return 0, err
	}
	id, _ := a.g.AdjList().Mapper().Lookup(n)
	return id, nil
}

// AddEdge inserts a directed edge and returns the endpoint NodeIDs.
func (a *lpgMutatorAdapter) AddEdge(src, dst string, w float64) (graph.NodeID, graph.NodeID, error) {
	if err := a.g.AddEdge(src, dst, w); err != nil {
		return 0, 0, err
	}
	srcID, _ := a.g.AdjList().Mapper().Lookup(src)
	dstID, _ := a.g.AdjList().Mapper().Lookup(dst)
	return srcID, dstID, nil
}

// RemoveEdge removes the directed edge (src, dst).
func (a *lpgMutatorAdapter) RemoveEdge(src, dst string) {
	a.g.AdjList().RemoveEdge(src, dst)
}

// SetNodeLabel attaches label to n.
func (a *lpgMutatorAdapter) SetNodeLabel(n, label string) error {
	if err := a.g.SetNodeLabel(n, label); err != nil {
		return err
	}
	if a.buf != nil {
		a.buf.Enqueue(index.Change{
			Op:    index.OpAddNodeLabel,
			Node:  a.resolveID(n),
			Label: uint32(a.g.Registry().Intern(label)),
		})
	}
	return nil
}

// RemoveNodeLabel detaches label from n.
func (a *lpgMutatorAdapter) RemoveNodeLabel(n, label string) {
	a.g.RemoveNodeLabel(n, label)
	if a.buf != nil {
		a.buf.Enqueue(index.Change{
			Op:    index.OpRemoveNodeLabel,
			Node:  a.resolveID(n),
			Label: uint32(a.g.Registry().Intern(label)),
		})
	}
}

// SetNodeProperty sets the named property on n.
func (a *lpgMutatorAdapter) SetNodeProperty(n, key string, value lpg.PropertyValue) error {
	if err := a.g.SetNodeProperty(n, key, value); err != nil {
		return err
	}
	if a.buf != nil {
		a.buf.Enqueue(index.Change{
			Op:       index.OpSetNodeProperty,
			Node:     a.resolveID(n),
			Property: uint32(a.g.PropertyKeys().Intern(key)),
			NewValue: value,
		})
	}
	return nil
}

// DelNodeProperty removes the named property from n.
func (a *lpgMutatorAdapter) DelNodeProperty(n, key string) {
	a.g.DelNodeProperty(n, key)
	if a.buf != nil {
		a.buf.Enqueue(index.Change{
			Op:       index.OpDelNodeProperty,
			Node:     a.resolveID(n),
			Property: uint32(a.g.PropertyKeys().Intern(key)),
		})
	}
}

// NodeProperties returns a snapshot of all properties on n.
func (a *lpgMutatorAdapter) NodeProperties(n string) map[string]lpg.PropertyValue {
	return a.g.NodeProperties(n)
}

// NodeLabels returns a snapshot of all labels on n.
func (a *lpgMutatorAdapter) NodeLabels(n string) []string {
	return a.g.NodeLabels(n)
}

// HasEdge reports whether a directed edge from src to dst is present.
func (a *lpgMutatorAdapter) HasEdge(src, dst string) bool {
	return a.g.AdjList().HasEdge(src, dst)
}

// SetEdgeLabel attaches label to the directed edge (src, dst).
func (a *lpgMutatorAdapter) SetEdgeLabel(src, dst, label string) {
	a.g.SetEdgeLabel(src, dst, label)
	if a.buf != nil {
		a.buf.Enqueue(index.Change{
			Op:    index.OpAddEdgeLabel,
			Node:  a.resolveID(src),
			Dst:   a.resolveID(dst),
			Label: uint32(a.g.Registry().Intern(label)),
		})
	}
}

// SetEdgeProperty sets the named property on the directed edge (src, dst).
func (a *lpgMutatorAdapter) SetEdgeProperty(src, dst, key string, value lpg.PropertyValue) error {
	if err := a.g.SetEdgeProperty(src, dst, key, value); err != nil {
		return err
	}
	if a.buf != nil {
		a.buf.Enqueue(index.Change{
			Op:       index.OpSetEdgeProperty,
			Node:     a.resolveID(src),
			Dst:      a.resolveID(dst),
			Property: uint32(a.g.PropertyKeys().Intern(key)),
			NewValue: value,
		})
	}
	return nil
}

// DelEdgeProperty removes the named property from the directed edge (src, dst).
func (a *lpgMutatorAdapter) DelEdgeProperty(src, dst, key string) {
	a.g.DelEdgeProperty(src, dst, key)
	if a.buf != nil {
		a.buf.Enqueue(index.Change{
			Op:       index.OpDelEdgeProperty,
			Node:     a.resolveID(src),
			Dst:      a.resolveID(dst),
			Property: uint32(a.g.PropertyKeys().Intern(key)),
		})
	}
}

// EdgeProperties returns a snapshot of every property currently set on the
// directed edge (src, dst).
func (a *lpgMutatorAdapter) EdgeProperties(src, dst string) map[string]lpg.PropertyValue {
	return a.g.EdgeProperties(src, dst)
}

// OutNeighbours returns a snapshot of the outgoing neighbour keys of n.
func (a *lpgMutatorAdapter) OutNeighbours(n string) []string {
	var out []string
	for nb := range a.g.AdjList().Neighbours(n) {
		out = append(out, nb)
	}
	return out
}

// InNeighbours returns a snapshot of the incoming neighbour keys of n by
// performing a full graph walk. This is O(V+E) and should only be called for
// DETACH DELETE operations where correctness trumps performance.
func (a *lpgMutatorAdapter) InNeighbours(n string) []string {
	nID, ok := a.g.AdjList().Mapper().Lookup(n)
	if !ok {
		return nil
	}
	var result []string
	a.g.AdjList().Mapper().Walk(func(id graph.NodeID, key string) bool {
		if id == nID {
			return true
		}
		nbs, _ := a.g.AdjList().LoadEntry(id)
		for _, nb := range nbs {
			if nb == nID {
				result = append(result, key)
				break
			}
		}
		return true
	})
	return result
}

// OutDegree returns the number of outgoing edges from n.
func (a *lpgMutatorAdapter) OutDegree(n string) int {
	id, ok := a.g.AdjList().Mapper().Lookup(n)
	if !ok {
		return 0
	}
	nbs, _ := a.g.AdjList().LoadEntry(id)
	return len(nbs)
}

// ResolveNodeID translates a node key to its NodeID.
func (a *lpgMutatorAdapter) ResolveNodeID(n string) (graph.NodeID, bool) {
	return a.g.AdjList().Mapper().Lookup(n)
}

// ResolveNodeLabel translates a NodeID back to its node key.
func (a *lpgMutatorAdapter) ResolveNodeLabel(id graph.NodeID) (string, bool) {
	return a.g.AdjList().Mapper().Resolve(id)
}

// WalkNodeIDs calls fn for every interned node.
func (a *lpgMutatorAdapter) WalkNodeIDs(fn func(graph.NodeID) bool) {
	a.g.AdjList().Mapper().Walk(func(id graph.NodeID, _ string) bool {
		return fn(id)
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// walMutatorAdapter — exec.GraphMutator backed by txn.Tx + *lpg.Graph
// ─────────────────────────────────────────────────────────────────────────────

// walMutatorAdapter applies every mutation to the in-memory graph eagerly (so
// reads within the same transaction see writes immediately) and also buffers
// the op in the txn.Tx so that [txn.Tx.CommitWALOnly] can fsync it to the WAL
// on [Result.Close].
//
// walMutatorAdapter is NOT safe for concurrent use. The store mutex is held
// from [txn.Store.Begin] (in RunInTx) until [txn.Tx.CommitWALOnly] or
// [txn.Tx.Rollback] (in Result.Close).
type walMutatorAdapter struct {
	g   *lpg.Graph[string, float64]
	tx  *txn.Tx[string, float64]
	buf *exec.IndexBuffer // nil for read-only (never reached via RunInTx)
}

func (a *walMutatorAdapter) resolveID(n string) graph.NodeID {
	id, ok := a.g.AdjList().Mapper().Lookup(n)
	if !ok {
		return graph.NodeID(0)
	}
	return id
}

// AddNode interns n and returns its stable NodeID.
func (a *walMutatorAdapter) AddNode(n string) (graph.NodeID, error) {
	if err := a.g.AddNode(n); err != nil {
		return 0, err
	}
	_ = a.tx.AddNode(n) //nolint:errcheck // tx is non-nil; only ErrTxFinished possible, which cannot occur here
	id, _ := a.g.AdjList().Mapper().Lookup(n)
	return id, nil
}

// AddEdge inserts a directed edge and returns the endpoint NodeIDs.
func (a *walMutatorAdapter) AddEdge(src, dst string, w float64) (graph.NodeID, graph.NodeID, error) {
	if err := a.g.AddEdge(src, dst, w); err != nil {
		return 0, 0, err
	}
	_ = a.tx.AddEdge(src, dst, w) //nolint:errcheck // ErrNoWeightCodec cannot occur — store has wcodec via NewEngineWithStore
	srcID, _ := a.g.AdjList().Mapper().Lookup(src)
	dstID, _ := a.g.AdjList().Mapper().Lookup(dst)
	return srcID, dstID, nil
}

// RemoveEdge removes the directed edge (src, dst).
func (a *walMutatorAdapter) RemoveEdge(src, dst string) {
	a.g.AdjList().RemoveEdge(src, dst)
	_ = a.tx.RemoveEdge(src, dst) //nolint:errcheck // ErrTxFinished impossible here
}

// SetNodeLabel attaches label to n.
func (a *walMutatorAdapter) SetNodeLabel(n, label string) error {
	if err := a.g.SetNodeLabel(n, label); err != nil {
		return err
	}
	_ = a.tx.SetNodeLabel(n, label) //nolint:errcheck // ErrTxFinished impossible here
	if a.buf != nil {
		a.buf.Enqueue(index.Change{
			Op:    index.OpAddNodeLabel,
			Node:  a.resolveID(n),
			Label: uint32(a.g.Registry().Intern(label)),
		})
	}
	return nil
}

// RemoveNodeLabel detaches label from n.
func (a *walMutatorAdapter) RemoveNodeLabel(n, label string) {
	a.g.RemoveNodeLabel(n, label)
	_ = a.tx.RemoveNodeLabel(n, label) //nolint:errcheck // ErrTxFinished impossible here
	if a.buf != nil {
		a.buf.Enqueue(index.Change{
			Op:    index.OpRemoveNodeLabel,
			Node:  a.resolveID(n),
			Label: uint32(a.g.Registry().Intern(label)),
		})
	}
}

// SetNodeProperty sets the named property on n.
func (a *walMutatorAdapter) SetNodeProperty(n, key string, value lpg.PropertyValue) error {
	if err := a.g.SetNodeProperty(n, key, value); err != nil {
		return err
	}
	_ = a.tx.SetNodeProperty(n, key, value) //nolint:errcheck // ErrTxFinished impossible here
	if a.buf != nil {
		a.buf.Enqueue(index.Change{
			Op:       index.OpSetNodeProperty,
			Node:     a.resolveID(n),
			Property: uint32(a.g.PropertyKeys().Intern(key)),
			NewValue: value,
		})
	}
	return nil
}

// DelNodeProperty removes the named property from n.
func (a *walMutatorAdapter) DelNodeProperty(n, key string) {
	a.g.DelNodeProperty(n, key)
	_ = a.tx.DelNodeProperty(n, key) //nolint:errcheck // ErrTxFinished impossible here
	if a.buf != nil {
		a.buf.Enqueue(index.Change{
			Op:       index.OpDelNodeProperty,
			Node:     a.resolveID(n),
			Property: uint32(a.g.PropertyKeys().Intern(key)),
		})
	}
}

// NodeProperties returns a snapshot of all properties on n.
func (a *walMutatorAdapter) NodeProperties(n string) map[string]lpg.PropertyValue {
	return a.g.NodeProperties(n)
}

// NodeLabels returns a snapshot of all labels on n.
func (a *walMutatorAdapter) NodeLabels(n string) []string {
	return a.g.NodeLabels(n)
}

// HasEdge reports whether a directed edge from src to dst is present.
func (a *walMutatorAdapter) HasEdge(src, dst string) bool {
	return a.g.AdjList().HasEdge(src, dst)
}

// SetEdgeLabel attaches label to the directed edge (src, dst).
func (a *walMutatorAdapter) SetEdgeLabel(src, dst, label string) {
	a.g.SetEdgeLabel(src, dst, label)
	_ = a.tx.SetEdgeLabel(src, dst, label) //nolint:errcheck // ErrTxFinished impossible here
	if a.buf != nil {
		a.buf.Enqueue(index.Change{
			Op:    index.OpAddEdgeLabel,
			Node:  a.resolveID(src),
			Dst:   a.resolveID(dst),
			Label: uint32(a.g.Registry().Intern(label)),
		})
	}
}

// SetEdgeProperty sets the named property on the directed edge (src, dst).
func (a *walMutatorAdapter) SetEdgeProperty(src, dst, key string, value lpg.PropertyValue) error {
	if err := a.g.SetEdgeProperty(src, dst, key, value); err != nil {
		return err
	}
	_ = a.tx.SetEdgeProperty(src, dst, key, value) //nolint:errcheck // ErrTxFinished impossible here
	if a.buf != nil {
		a.buf.Enqueue(index.Change{
			Op:       index.OpSetEdgeProperty,
			Node:     a.resolveID(src),
			Dst:      a.resolveID(dst),
			Property: uint32(a.g.PropertyKeys().Intern(key)),
			NewValue: value,
		})
	}
	return nil
}

// DelEdgeProperty removes the named property from the directed edge (src, dst).
func (a *walMutatorAdapter) DelEdgeProperty(src, dst, key string) {
	a.g.DelEdgeProperty(src, dst, key)
	_ = a.tx.DelEdgeProperty(src, dst, key) //nolint:errcheck // ErrTxFinished impossible here
	if a.buf != nil {
		a.buf.Enqueue(index.Change{
			Op:       index.OpDelEdgeProperty,
			Node:     a.resolveID(src),
			Dst:      a.resolveID(dst),
			Property: uint32(a.g.PropertyKeys().Intern(key)),
		})
	}
}

// EdgeProperties returns a snapshot of every property currently set on the
// directed edge (src, dst).
func (a *walMutatorAdapter) EdgeProperties(src, dst string) map[string]lpg.PropertyValue {
	return a.g.EdgeProperties(src, dst)
}

// OutNeighbours returns a snapshot of the outgoing neighbour keys of n.
func (a *walMutatorAdapter) OutNeighbours(n string) []string {
	var out []string
	for nb := range a.g.AdjList().Neighbours(n) {
		out = append(out, nb)
	}
	return out
}

// InNeighbours returns a snapshot of the incoming neighbour keys of n by
// performing a full graph walk.
func (a *walMutatorAdapter) InNeighbours(n string) []string {
	nID, ok := a.g.AdjList().Mapper().Lookup(n)
	if !ok {
		return nil
	}
	var result []string
	a.g.AdjList().Mapper().Walk(func(id graph.NodeID, key string) bool {
		if id == nID {
			return true
		}
		nbs, _ := a.g.AdjList().LoadEntry(id)
		for _, nb := range nbs {
			if nb == nID {
				result = append(result, key)
				break
			}
		}
		return true
	})
	return result
}

// OutDegree returns the number of outgoing edges from n.
func (a *walMutatorAdapter) OutDegree(n string) int {
	id, ok := a.g.AdjList().Mapper().Lookup(n)
	if !ok {
		return 0
	}
	nbs, _ := a.g.AdjList().LoadEntry(id)
	return len(nbs)
}

// ResolveNodeID translates a node key to its NodeID.
func (a *walMutatorAdapter) ResolveNodeID(n string) (graph.NodeID, bool) {
	return a.g.AdjList().Mapper().Lookup(n)
}

// ResolveNodeLabel translates a NodeID back to its node key.
func (a *walMutatorAdapter) ResolveNodeLabel(id graph.NodeID) (string, bool) {
	return a.g.AdjList().Mapper().Resolve(id)
}

// WalkNodeIDs calls fn for every interned node.
func (a *walMutatorAdapter) WalkNodeIDs(fn func(graph.NodeID) bool) {
	a.g.AdjList().Mapper().Walk(func(id graph.NodeID, _ string) bool {
		return fn(id)
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// CSR helpers for expand operators
// ─────────────────────────────────────────────────────────────────────────────

// csrPairFromGraph builds a forward and a reverse CSR snapshot from the LPG
// adjacency list.  Both snapshots are constructed in O(V+E) time and are safe
// for lock-free concurrent reads after construction.
func csrPairFromGraph(g *lpg.Graph[string, float64]) (fwd, rev *csr.CSR[float64]) {
	adj := g.AdjList()
	fwd = csr.BuildFromAdjList(adj)
	rev = fwd.BuildReverse()
	return
}

// buildEdgeTypeFilter constructs an edge-type filter map for the forward CSR
// of g.  The map key is the edge's absolute position in the CSR's EdgesSlice;
// the value is the first label attached to that edge in the LPG.
//
// When relTypes is non-empty only edges whose first label matches one of the
// listed types are included; all others are omitted from the map.  An empty
// relTypes slice means "accept all edge types" — the returned map still lists
// every typed edge so callers can perform label-keyed filtering.
//
// O(V+E) time; allocates one map entry per labelled edge.
func buildEdgeTypeFilter(g *lpg.Graph[string, float64], relTypes []string) map[uint64]string {
	adj := g.AdjList()
	fwdCSR := csr.BuildFromAdjList(adj)
	verts := fwdCSR.VerticesSlice()
	edges := fwdCSR.EdgesSlice()
	mapper := adj.Mapper()

	// Pre-build a set of accepted types for O(1) lookup.
	acceptAll := len(relTypes) == 0
	accept := make(map[string]struct{}, len(relTypes))
	for _, t := range relTypes {
		accept[t] = struct{}{}
	}

	filter := make(map[uint64]string)
	maxID := uint64(adj.MaxNodeID())
	for srcID := uint64(0); srcID < maxID; srcID++ {
		start := verts[srcID]
		end := verts[srcID+1]
		srcStr, ok := mapper.Resolve(graph.NodeID(srcID))
		if !ok {
			continue
		}
		for pos := start; pos < end; pos++ {
			dstStr, ok := mapper.Resolve(edges[pos])
			if !ok {
				continue
			}
			labels := g.EdgeLabels(srcStr, dstStr)
			if len(labels) == 0 {
				continue
			}
			typ := labels[0]
			if acceptAll {
				filter[pos] = typ
			} else if _, ok := accept[typ]; ok {
				filter[pos] = typ
			}
		}
	}
	return filter
}

// irDirToExec converts an IR Direction to the corresponding exec Direction.
func irDirToExec(d ir.Direction) exec.Direction {
	switch d {
	case ir.DirectionIncoming:
		return exec.DirIn
	case ir.DirectionBoth:
		return exec.DirBoth
	default:
		return exec.DirOut
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Compile-time assertions
// ─────────────────────────────────────────────────────────────────────────────

var _ nodeWalkerIface = (*lpgNodeWalker)(nil)
var _ labelResolverIface = (*lpgLabelResolver)(nil)
var _ exec.GraphMutator = (*walMutatorAdapter)(nil)
