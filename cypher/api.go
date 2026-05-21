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
// Engine caches parsed and translated logical plans in a [sync.Map] keyed by
// the query string. The cached entry is the IR [ir.LogicalPlan]; the physical
// build step runs per Engine.Run call so that per-call executor state is fresh.
//
// # Concurrency
//
// Engine is safe for concurrent use. Each Run call creates an independent
// physical operator tree.
package cypher

import (
	"context"
	"fmt"
	"strings"
	"sync"

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
	"gograph/graph/index"
	"gograph/graph/lpg"
	"gograph/store/txn"
)

// ─────────────────────────────────────────────────────────────────────────────
// Engine
// ─────────────────────────────────────────────────────────────────────────────

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
	cache         sync.Map // map[string]ir.LogicalPlan
}

// NewEngine creates an Engine backed by g. The default built-in function
// registry ([funcs.DefaultRegistry]) is used.
//
// If g has no [index.Manager] attached yet, NewEngine installs a new empty one
// so that DDL statements (CREATE INDEX / DROP INDEX) work out of the box.
func NewEngine(g *lpg.Graph[string, float64]) *Engine {
	ensureIndexManager(g)
	e := &Engine{
		g:             g,
		reg:           funcs.DefaultRegistry,
		constraintReg: exec.NewConstraintRegistry(),
		procReg:       procs.NewRegistry(),
	}
	procs.RegisterBuiltins(e.procReg, g.IndexManager(), func() [][]expr.Value {
		return e.constraintReg.ListConstraintRows()
	})
	return e
}

// NewEngineWithRegistry creates an Engine backed by g using a custom function
// registry.
//
// If g has no [index.Manager] attached yet, a new empty one is installed.
func NewEngineWithRegistry(g *lpg.Graph[string, float64], reg expr.FunctionRegistry) *Engine {
	ensureIndexManager(g)
	e := &Engine{
		g:             g,
		reg:           reg,
		constraintReg: exec.NewConstraintRegistry(),
		procReg:       procs.NewRegistry(),
	}
	procs.RegisterBuiltins(e.procReg, g.IndexManager(), func() [][]expr.Value {
		return e.constraintReg.ListConstraintRows()
	})
	return e
}

// NewEngineWithStore creates an Engine backed by a WAL-enabled [txn.Store].
//
// All write queries routed through [Engine.RunInTx] use a single [txn.Tx] for
// atomicity and WAL durability: mutations are applied eagerly to the in-memory
// graph (so reads within the same transaction see the writes) and the WAL is
// fsynced on [Result.Close] when no pipeline error occurred.
//
// The underlying graph is taken from store.Graph(). If the graph has no
// [index.Manager] attached yet, a new empty one is installed.
func NewEngineWithStore(store *txn.Store[string, float64]) *Engine {
	g := store.Graph()
	ensureIndexManager(g)
	e := &Engine{
		g:             g,
		store:         store,
		reg:           funcs.DefaultRegistry,
		constraintReg: exec.NewConstraintRegistry(),
		procReg:       procs.NewRegistry(),
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

// Run executes query against the engine's graph and returns a streaming
// [Result]. The caller must call [Result.Close] when done.
//
// A wrapped [*parser.ParseError] is returned when the query has a syntax
// error; the error includes line and column information.
//
// Sprint 25 support: MATCH (full scan or label scan) + RETURN.
func (e *Engine) Run(ctx context.Context, query string, params map[string]expr.Value) (*Result, error) {
	// ── 0. DDL fast-path ─────────────────────────────────────────────────────
	if ir.IsDDL(query) {
		return e.runDDL(ctx, query)
	}

	// ── 1. Parse or retrieve from plan cache ─────────────────────────────────
	plan, err := e.planFor(query)
	if err != nil {
		return nil, err
	}

	// ── 1b. Parameter type check ─────────────────────────────────────────────
	if len(params) > 0 {
		if err := sema.CheckParams(sema.InferParamTypes(plan), params); err != nil {
			return nil, err
		}
	}

	// ── 2. Build physical operator tree ─────────────────────────────────────
	walker := &lpgNodeWalker{g: e.g}
	labelSrc := &lpgLabelResolver{g: e.g}
	op, cols, err := buildPlanEngine(plan, walker, labelSrc, e.reg, params, e.g.IndexManager(), e.procReg)
	if err != nil {
		return nil, fmt.Errorf("cypher: build plan: %w", err)
	}

	// ── 3. Wrap in streaming Result ──────────────────────────────────────────
	rs := exec.Run(ctx, op, cols)
	return &Result{rs: rs, cols: cols}, nil
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
		op = exec.NewCreateIndexOp(p.Name, kind, p.IfNotExists, idxMgr)
	case *ir.DropIndex:
		op = exec.NewDropIndexOp(p.Name, p.IfExists, idxMgr)
	case *ir.CreateConstraint:
		var kind exec.ConstraintKind
		switch p.Kind {
		case ir.ConstraintUnique:
			kind = exec.ConstraintUnique
		case ir.ConstraintNotNull:
			kind = exec.ConstraintNotNull
		}
		op = exec.NewCreateConstraintOp(p.Name, p.Label, p.Property, kind, p.IfNotExists, idxMgr, e.constraintReg)
	case *ir.DropConstraint:
		var kind exec.ConstraintKind
		switch p.Kind {
		case ir.ConstraintUnique:
			kind = exec.ConstraintUnique
		case ir.ConstraintNotNull:
			kind = exec.ConstraintNotNull
		}
		op = exec.NewDropConstraintOp(p.Name, p.Label, p.Property, kind, p.IfExists, idxMgr, e.constraintReg)
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
	return &Result{rs: exec.Run(ctx, exec.NewArgument(), nil), cols: nil}, nil
}

// RunAny executes query with params expressed as map[string]any, automatically
// converting Go native types to [expr.Value]. See [BindParams] for the
// supported conversions. RunAny is equivalent to Run when params is nil.
func (e *Engine) RunAny(ctx context.Context, query string, params map[string]any) (*Result, error) {
	converted, err := BindParams(params)
	if err != nil {
		return nil, err
	}
	return e.Run(ctx, query, converted)
}

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

// planFor returns the cached logical plan for query, or parses, translates,
// and caches it.
func (e *Engine) planFor(query string) (ir.LogicalPlan, error) {
	if v, ok := e.cache.Load(query); ok {
		return v.(ir.LogicalPlan), nil //nolint:forcetypeassert // cache invariant
	}
	astNode, err := parser.Parse(query)
	if err != nil {
		return nil, fmt.Errorf("cypher: parse: %w", err)
	}
	plan, err := ir.FromAST(astNode)
	if err != nil {
		return nil, fmt.Errorf("cypher: translate: %w", err)
	}
	actual, _ := e.cache.LoadOrStore(query, plan)
	return actual.(ir.LogicalPlan), nil //nolint:forcetypeassert // cache invariant
}

// ─────────────────────────────────────────────────────────────────────────────
// Result
// ─────────────────────────────────────────────────────────────────────────────

// Result is a forward-only streaming result set returned by [Engine.Run].
// It wraps [exec.ResultSet] and exposes the same iterator contract.
//
// Result is NOT safe for concurrent use.
type Result struct {
	rs     *exec.ResultSet
	cols   []string
	buf    *exec.IndexBuffer        // non-nil only for RunInTx results
	idxMgr *index.Manager           // non-nil only when buf != nil
	tx     *txn.Tx[string, float64] // non-nil only for WAL-backed RunInTx results
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

// Close releases all resources held by the result set.
// When the result was created by [Engine.RunInTx], Close also:
//  1. Commits or rolls back buffered index changes (always).
//  2. When the engine is WAL-backed ([NewEngineWithStore]), WAL-syncs the
//     buffered ops via [txn.Tx.CommitWALOnly] on success, or calls
//     [txn.Tx.Rollback] on error. Mutations have already been applied to the
//     in-memory graph eagerly; CommitWALOnly only persists them to the WAL.
func (r *Result) Close() error {
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

	// When the IR root is a ProduceResults, use its declared columns; otherwise
	// treat the plan as a write-only query with no output columns. A CREATE
	// without RETURN has a write operator as root.
	if pr, ok := plan.(*ir.ProduceResults); ok {
		cols = pr.Columns
		child, buildErr := buildOperatorWrite(pr.Child, walker, labelSrc, reg, params, schema, mutator, constraintReg, idxMgr)
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
	child, buildErr := buildOperatorWrite(plan, walker, labelSrc, reg, params, schema, mutator, constraintReg, idxMgr)
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
) (exec.Operator, error) {
	if plan == nil {
		// A nil plan arises when a write clause has no driving subplan (e.g.
		// CREATE without a leading MATCH). Return a single-row operator that
		// drives the write operator exactly once.
		return exec.NewSingleRowOperator(), nil
	}

	switch p := plan.(type) {

	case *ir.CreateNode:
		child, err := buildOperatorWrite(p.Child, walker, labelSrc, reg, params, schema, mutator, constraintReg, idxMgr)
		if err != nil {
			return nil, err
		}
		if p.NodeVar != "" {
			schema[p.NodeVar] = len(schema)
		}
		cn, buildErr := exec.NewCreateNode(p.NodeVar, p.Labels, p.Properties, child, mutator)
		if buildErr != nil {
			return nil, buildErr
		}
		if constraintReg != nil {
			cn.WithConstraints(constraintReg, idxMgr)
		}
		return cn, nil

	case *ir.CreateRelationship:
		child, err := buildOperatorWrite(p.Child, walker, labelSrc, reg, params, schema, mutator, constraintReg, idxMgr)
		if err != nil {
			return nil, err
		}
		if p.RelVar != "" {
			schema[p.RelVar] = len(schema)
		}
		// Pass a copy of schema so the operator captures the current state.
		schemaCopy := make(map[string]int, len(schema))
		for k, v := range schema {
			schemaCopy[k] = v
		}
		return exec.NewCreateRelationship(p.StartVar, p.EndVar, p.RelVar, p.RelType, p.Properties, schemaCopy, child, mutator)

	case *ir.SetProperty:
		child, err := buildOperatorWrite(p.Child, walker, labelSrc, reg, params, schema, mutator, constraintReg, idxMgr)
		if err != nil {
			return nil, err
		}
		schemaCopy := copySchema(schema)
		sp, buildErr := exec.NewSetProperty(p.EntityVar, p.PropertyKey, p.Value, schemaCopy, child, mutator)
		if buildErr != nil {
			return nil, buildErr
		}
		if constraintReg != nil {
			sp.WithConstraints(constraintReg, idxMgr)
		}
		return sp, nil

	case *ir.SetLabels:
		child, err := buildOperatorWrite(p.Child, walker, labelSrc, reg, params, schema, mutator, constraintReg, idxMgr)
		if err != nil {
			return nil, err
		}
		schemaCopy := copySchema(schema)
		return exec.NewSetLabels(p.NodeVar, p.Labels, schemaCopy, child, mutator), nil

	case *ir.RemoveProperty:
		child, err := buildOperatorWrite(p.Child, walker, labelSrc, reg, params, schema, mutator, constraintReg, idxMgr)
		if err != nil {
			return nil, err
		}
		schemaCopy := copySchema(schema)
		return exec.NewRemoveProperty(p.EntityVar, p.PropertyKey, schemaCopy, child, mutator), nil

	case *ir.RemoveLabels:
		child, err := buildOperatorWrite(p.Child, walker, labelSrc, reg, params, schema, mutator, constraintReg, idxMgr)
		if err != nil {
			return nil, err
		}
		schemaCopy := copySchema(schema)
		return exec.NewRemoveLabels(p.NodeVar, p.Labels, schemaCopy, child, mutator), nil

	case *ir.DeleteNode:
		child, err := buildOperatorWrite(p.Child, walker, labelSrc, reg, params, schema, mutator, constraintReg, idxMgr)
		if err != nil {
			return nil, err
		}
		schemaCopy := copySchema(schema)
		return exec.NewDeleteNode(p.NodeVar, schemaCopy, child, mutator), nil

	case *ir.DeleteRelationship:
		child, err := buildOperatorWrite(p.Child, walker, labelSrc, reg, params, schema, mutator, constraintReg, idxMgr)
		if err != nil {
			return nil, err
		}
		schemaCopy := copySchema(schema)
		return exec.NewDeleteRelationship(p.RelVar, schemaCopy, child, mutator), nil

	case *ir.DetachDelete:
		child, err := buildOperatorWrite(p.Child, walker, labelSrc, reg, params, schema, mutator, constraintReg, idxMgr)
		if err != nil {
			return nil, err
		}
		schemaCopy := copySchema(schema)
		return exec.NewDetachDelete(p.NodeVar, schemaCopy, child, mutator), nil

	case *ir.Merge:
		child, err := buildOperatorWrite(p.Child, walker, labelSrc, reg, params, schema, mutator, constraintReg, idxMgr)
		if err != nil {
			return nil, err
		}
		// Extract labels and properties from the pattern string. For the
		// current IR the pattern is an opaque string; we surface the bound vars
		// as the output schema columns.
		for _, v := range p.BoundVars {
			if v != "" {
				schema[v] = len(schema)
			}
		}
		schemaCopy := copySchema(schema)
		// The search function re-builds the child plan as a read-only scan for
		// the MERGE match check. For the current IR we return no matches
		// (pattern search requires full expression evaluation which is out of
		// scope); MERGE always takes the ON CREATE path.
		searchFn := func(_ context.Context) ([]exec.Row, error) {
			return nil, nil
		}
		labels, props := parseNodePatternStr(p.Pattern)
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
		if constraintReg != nil {
			m.WithConstraints(constraintReg, idxMgr)
		}
		return m, nil

	default:
		// Fall through to the read-operator builder.
		// procReg is nil here because buildOperatorWrite is only called from the
		// write path (buildPlanWithMutatorFull) which does not thread procReg.
		return buildOperator(plan, walker, labelSrc, reg, params, schema, idxMgr, nil)
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
func parseNodePatternStr(pattern string) (labels []string, props string) {
	// Strip outer parens.
	s := strings.TrimSpace(pattern)
	if len(s) >= 2 && s[0] == '(' && s[len(s)-1] == ')' {
		s = s[1 : len(s)-1]
	}

	// Split off any property map "{...}" suffix.
	braceIdx := strings.Index(s, "{")
	if braceIdx >= 0 {
		props = strings.TrimSpace(s[braceIdx:])
		s = s[:braceIdx]
	}

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
	child, err := buildOperator(pr.Child, walker, labelSrc, reg, params, schema, nil, nil)
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
// may both be nil.
func buildPlanEngine(
	plan ir.LogicalPlan,
	walker nodeWalkerIface,
	labelSrc labelResolverIface,
	reg expr.FunctionRegistry,
	params map[string]expr.Value,
	idxMgr *index.Manager,
	procReg *procs.Registry,
) (op exec.Operator, cols []string, err error) {
	// Standalone CALL (root is *ir.ProcedureCall): treat YieldVars as columns.
	if p, ok := plan.(*ir.ProcedureCall); ok {
		schema := make(map[string]int)
		child, buildErr := buildOperator(p, walker, labelSrc, reg, params, schema, idxMgr, procReg)
		if buildErr != nil {
			return nil, nil, buildErr
		}
		return child, p.YieldVars, nil
	}

	pr, ok := plan.(*ir.ProduceResults)
	if !ok {
		return nil, nil, fmt.Errorf("cypher: plan root must be ProduceResults, got %T", plan)
	}
	cols = pr.Columns
	schema := make(map[string]int)
	child, err := buildOperator(pr.Child, walker, labelSrc, reg, params, schema, idxMgr, procReg)
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
) (exec.Operator, error) {
	switch p := plan.(type) {

	case *ir.AllNodesScan:
		schema[p.NodeVar] = len(schema)
		return exec.NewAllNodesScan(walker), nil

	case *ir.NodeByLabelScan:
		schema[p.NodeVar] = len(schema)
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
		child, err := buildOperator(p.Child, walker, labelSrc, reg, params, schema, idxMgr, procReg)
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
				return exec.NewFilter(child, func(row exec.Row) (expr.Value, error) {
					rowCtx := buildRowCtx(row, schemaSnap, capturedG)
					return expr.Eval(predExpr, rowCtx, capturedParams, capturedReg)
				}), nil
			}
		}
		// Fallback: no AST or no graph available — pass-through filter.
		return exec.NewFilter(child, func(_ exec.Row) (expr.Value, error) {
			return expr.BoolValue(true), nil
		}), nil

	case *ir.Projection:
		child, err := buildOperator(p.Child, walker, labelSrc, reg, params, schema, idxMgr, procReg)
		if err != nil {
			return nil, err
		}
		var projG *lpg.Graph[string, float64]
		if lw, ok := walker.(*lpgNodeWalker); ok {
			projG = lw.g
		}
		return buildIRProjection(p.Items, child, schema, projG, params, reg)

	case *ir.Expand:
		child, err := buildOperator(p.Child, walker, labelSrc, reg, params, schema, idxMgr, procReg)
		if err != nil {
			return nil, err
		}
		// Sprint 25 stub: add rel/destination vars as NULL-producing columns.
		if p.RelVar != "" {
			schema[p.RelVar] = len(schema)
		}
		if p.ToVar != "" {
			schema[p.ToVar] = len(schema)
		}
		return child, nil

	case *ir.Apply:
		// Build the outer plan first so its vars enter the schema.
		outer, err := buildOperator(p.Outer, walker, labelSrc, reg, params, schema, idxMgr, procReg)
		if err != nil {
			return nil, err
		}
		// The Argument operator is the leaf of the inner plan; it re-emits the
		// outer row so correlated inner scans can consume it. For non-correlated
		// inner plans (independent scans) the arg is inert but required.
		arg := exec.NewArgument()
		inner, err := buildOperator(p.Inner, walker, labelSrc, reg, params, schema, idxMgr, procReg)
		if err != nil {
			return nil, err
		}
		return exec.NewApply(outer, inner, arg), nil

	case *ir.SemiApply:
		outer, err := buildOperator(p.Outer, walker, labelSrc, reg, params, schema, idxMgr, procReg)
		if err != nil {
			return nil, err
		}
		arg := exec.NewArgument()
		inner, err := buildOperator(p.Inner, walker, labelSrc, reg, params, schema, idxMgr, procReg)
		if err != nil {
			return nil, err
		}
		return exec.NewSemiApply(outer, inner, arg), nil

	case *ir.AntiSemiApply:
		outer, err := buildOperator(p.Outer, walker, labelSrc, reg, params, schema, idxMgr, procReg)
		if err != nil {
			return nil, err
		}
		arg := exec.NewArgument()
		inner, err := buildOperator(p.Inner, walker, labelSrc, reg, params, schema, idxMgr, procReg)
		if err != nil {
			return nil, err
		}
		return exec.NewAntiSemiApply(outer, inner, arg), nil

	case *ir.Argument:
		// Argument is the leaf of an Apply-family inner plan. At runtime the exec
		// Argument operator re-emits the outer row that was injected by the Apply
		// loop. The IR vars are already in schema from the outer build; no new
		// column registrations are needed here.
		return exec.NewArgument(), nil

	case *ir.EagerAggregation:
		child, err := buildOperator(p.Child, walker, labelSrc, reg, params, schema, idxMgr, procReg)
		if err != nil {
			return nil, err
		}
		return buildEagerAggregation(p, child, schema)

	case *ir.Sort:
		child, err := buildOperator(p.Child, walker, labelSrc, reg, params, schema, idxMgr, procReg)
		if err != nil {
			return nil, err
		}
		keys := irSortKeys(p.SortItems, schema)
		if len(keys) == 0 {
			// No resolvable sort keys — pass through without sorting.
			return child, nil
		}
		return exec.NewSort(child, keys, 0)

	case *ir.Top:
		child, err := buildOperator(p.Child, walker, labelSrc, reg, params, schema, idxMgr, procReg)
		if err != nil {
			return nil, err
		}
		keys := irSortKeys(p.SortItems, schema)
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
		child, err := buildOperator(p.Child, walker, labelSrc, reg, params, schema, idxMgr, procReg)
		if err != nil {
			return nil, err
		}
		return exec.NewLimit(child, p.Count)

	case *ir.Skip:
		child, err := buildOperator(p.Child, walker, labelSrc, reg, params, schema, idxMgr, procReg)
		if err != nil {
			return nil, err
		}
		return exec.NewSkip(child, p.Count)

	case *ir.Distinct:
		child, err := buildOperator(p.Child, walker, labelSrc, reg, params, schema, idxMgr, procReg)
		if err != nil {
			return nil, err
		}
		return exec.NewDistinct(child, 0), nil

	case *ir.OptionalExpand:
		// OptionalExpand requires CSR adjacency which is not threaded through
		// buildOperator. Like *ir.Expand, we register the output variables and
		// return the child operator with NULL-extended schema columns. A genuine
		// NULL-padded implementation requires CSR access; this stub produces the
		// correct output schema so downstream projections can reference the vars.
		child, err := buildOperator(p.Child, walker, labelSrc, reg, params, schema, idxMgr, procReg)
		if err != nil {
			return nil, err
		}
		if p.RelVar != "" {
			schema[p.RelVar] = len(schema)
		}
		if p.ToVar != "" {
			schema[p.ToVar] = len(schema)
		}
		return child, nil

	case *ir.ProcedureCall:
		return buildProcedureCallOperator(p, walker, labelSrc, reg, params, schema, idxMgr, procReg)

	default:
		return nil, fmt.Errorf("cypher: unsupported IR node %T", plan)
	}
}

// buildEagerAggregation builds the physical EagerAggregation operator from the
// IR node. It wraps the child in a pre-projection so that the exec operator
// sees rows in the expected layout: [groupByKeys..., aggArgs...].
func buildEagerAggregation(
	p *ir.EagerAggregation,
	child exec.Operator,
	schema map[string]int,
) (exec.Operator, error) {
	// Build pre-projection items:
	//   positions 0..len(GroupBy)-1  → group-by key columns
	//   positions len(GroupBy)..end  → aggregate argument columns
	items := make([]exec.ProjectionItem, 0, len(p.GroupBy)+len(p.Aggregates))

	// Group-by key projections.
	keyCols := make([]int, len(p.GroupBy))
	for i, varName := range p.GroupBy {
		keyCols[i] = i // after pre-projection, key i is at position i
		srcCol := 0
		if col, ok := schema[varName]; ok {
			srcCol = col
		}
		capturedCol := srcCol
		items = append(items, exec.ProjectionItem{
			Alias: varName,
			Eval: func(row exec.Row) (expr.Value, error) {
				if capturedCol < len(row) {
					return row[capturedCol], nil
				}
				return expr.Null, nil
			},
		})
	}

	// Aggregate argument projections.
	aggFactories := make([]funcs.AggregatorFactory, 0, len(p.Aggregates))
	for _, aggExpr := range p.Aggregates {
		factory, ferr := aggregateFactory(aggExpr.Function, aggExpr.Argument)
		if ferr != nil {
			return nil, fmt.Errorf("cypher: %w", ferr)
		}
		aggFactories = append(aggFactories, factory)

		// Resolve argument column: empty Argument means count(*) — any value works.
		if aggExpr.Argument == "" {
			items = append(items, exec.ProjectionItem{
				Alias: aggExpr.OutputName,
				Eval:  func(_ exec.Row) (expr.Value, error) { return expr.Null, nil },
			})
		} else if argCol, ok := schema[aggExpr.Argument]; ok {
			capturedArgCol := argCol
			items = append(items, exec.ProjectionItem{
				Alias: aggExpr.OutputName,
				Eval: func(row exec.Row) (expr.Value, error) {
					if capturedArgCol < len(row) {
						return row[capturedArgCol], nil
					}
					return expr.Null, nil
				},
			})
		} else {
			// Property access or unresolvable expression — emit Null.
			// Aggregates that depend on property values (e.g. sum(n.age)) require
			// the expression evaluator to have resolved the property access upstream
			// via a Projection; if it hasn't been resolved yet, Null is the safe
			// fallback (count(*) is unaffected; sum/avg/min/max of Null = Null).
			items = append(items, exec.ProjectionItem{
				Alias: aggExpr.OutputName,
				Eval:  func(_ exec.Row) (expr.Value, error) { return expr.Null, nil },
			})
		}
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
	return op, nil
}

// aggregateFactory maps an IR aggregate function name and argument to a
// [funcs.AggregatorFactory]. An empty argument string means count(*).
func aggregateFactory(fn, argument string) (funcs.AggregatorFactory, error) {
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
	default:
		return nil, fmt.Errorf("unknown aggregate function %q", fn)
	}
}

// irSortKeys converts a slice of ir.SortItem to exec.SortKey values by
// resolving each expression against the current schema. Expressions that
// cannot be resolved (property accesses, unbound variables) are skipped.
func irSortKeys(items []ir.SortItem, schema map[string]int) []exec.SortKey {
	keys := make([]exec.SortKey, 0, len(items))
	for _, si := range items {
		col, ok := schema[si.Expression]
		if !ok {
			// Skip unresolvable sort expressions — callers treat an empty result
			// as "no sort needed" and pass through the child unchanged.
			continue
		}
		keys = append(keys, exec.SortKey{
			ColIdx:    col,
			Ascending: !si.Descending,
		})
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
) (exec.Operator, error) {
	// Build child if present.
	var child exec.Operator
	if p.Child != nil {
		var err error
		child, err = buildOperator(p.Child, walker, labelSrc, reg, params, schema, idxMgr, procReg)
		if err != nil {
			return nil, err
		}
	}

	// Build argument evaluators from the schema. Argument strings are opaque
	// variable references resolved via the current schema.
	argEvals := make([]func(exec.Row) (expr.Value, error), len(p.Arguments))
	for i, argStr := range p.Arguments {
		if colIdx, ok := schema[argStr]; ok {
			idx := colIdx
			argEvals[i] = func(row exec.Row) (expr.Value, error) {
				if idx < len(row) {
					return row[idx], nil
				}
				return expr.Null, nil
			}
		} else {
			argEvals[i] = func(_ exec.Row) (expr.Value, error) { return expr.Null, nil }
		}
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
		schema[v] = len(schema)
	}

	return exec.NewProcedureCallOp(p.Namespace, p.Name, argEvals, yieldVars, child, effectiveProcReg), nil
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
			schema[p.NodeVar] = len(schema)
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
	// Only rewrite when the child is a bare scan leaf.
	var nodeVar, label string
	switch child := sel.Child.(type) {
	case *ir.AllNodesScan:
		nodeVar = child.NodeVar
	case *ir.NodeByLabelScan:
		nodeVar = child.NodeVar
		label = child.Label
	default:
		return nil, false, nil
	}

	// ── Path 1: parameterised equality  n.prop = $param ─────────────────────
	var seekVal expr.Value
	var propKey string
	paramName, pk1 := extractEqParamFromPredicate(sel.Predicate, nodeVar)
	if paramName != "" && pk1 != "" {
		sv, err := resolveSeekValue("$"+paramName, params)
		if err != nil {
			return nil, false, err
		}
		seekVal = sv
		propKey = pk1
	}

	// ── Path 2: AST-based extraction (inline literal or parameter) ──────────
	if seekVal == nil && sel.PredicateExpr != nil {
		pk2, sv, ok := extractEqFromAST(sel.PredicateExpr, nodeVar, params)
		if ok {
			seekVal = sv
			propKey = pk2
		}
	}

	if seekVal == nil {
		return nil, false, nil
	}

	// ── Prefer the auto-named index for this (label, propKey) pair ───────────
	if label != "" && propKey != "" {
		wantName := strings.ToLower(label) + "_" + strings.ToLower(propKey) + "_hash"
		if sub, err := idxMgr.GetIndex(wantName); err == nil && sub.Kind() == "hash" {
			if op, ok := tryNewHashSeek(sub, seekVal); ok {
				schema[nodeVar] = len(schema)
				return op, true, nil
			}
		}
	}

	// ── Fall back: iterate all hash indexes ──────────────────────────────────
	names := idxMgr.ListIndexes()
	for _, name := range names {
		sub, err2 := idxMgr.GetIndex(name)
		if err2 != nil || sub.Kind() != "hash" {
			continue
		}
		if op, ok := tryNewHashSeek(sub, seekVal); ok {
			schema[nodeVar] = len(schema)
			return op, true, nil
		}
	}
	return nil, false, nil
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
func lpgPropToExpr(pv lpg.PropertyValue) expr.Value {
	switch pv.Kind() {
	case lpg.PropString:
		if s, ok := pv.String(); ok {
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
	}
	return expr.Null
}

// buildRowCtx converts a row plus a schema snapshot into an expr.RowContext,
// upgrading IntegerValue(nodeID) entries to NodeValue with properties loaded
// from the graph. g may be nil when no graph is available (upgrade is skipped).
func buildRowCtx(row exec.Row, schema map[string]int, g *lpg.Graph[string, float64]) expr.RowContext {
	ctx := make(expr.RowContext, len(schema))
	for varName, colIdx := range schema {
		if colIdx >= len(row) || row[colIdx] == nil {
			continue
		}
		v := row[colIdx]
		// Upgrade IntegerValue(NodeID) → NodeValue when graph is available.
		if g != nil {
			if iv, ok := v.(expr.IntegerValue); ok {
				id := graph.NodeID(iv)
				if name, resolved := g.AdjList().Mapper().Resolve(id); resolved {
					rawProps := g.NodeProperties(name)
					props := make(expr.MapValue, len(rawProps))
					for k, pv := range rawProps {
						props[k] = lpgPropToExpr(pv)
					}
					labels := g.NodeLabels(name)
					ctx[varName] = expr.NodeValue{ID: uint64(id), Labels: labels, Properties: props}
					continue
				}
			}
		}
		ctx[varName] = v
	}
	return ctx
}

// buildIRProjection converts IR ProjectionItems to a physical Project operator.
// When an item carries a parsed AST expression (item.Expr != nil), the
// executor evaluates it via expr.Eval against a full RowContext — enabling
// property access (n.prop), function calls, and other non-trivial expressions.
// For simple variable references and string-only items the fast schema-lookup
// path is used.
func buildIRProjection(
	items []ir.ProjectionItem,
	child exec.Operator,
	schema map[string]int,
	g *lpg.Graph[string, float64],
	params map[string]expr.Value,
	reg expr.FunctionRegistry,
) (*exec.Project, error) {
	projItems := make([]exec.ProjectionItem, len(items))
	for i, item := range items {
		name := item.Name
		exprStr := item.Expression

		var evalFn func(exec.Row) (expr.Value, error)
		if item.Expr != nil {
			if v, ok := item.Expr.(*ast.Variable); ok {
				// Fast path: simple variable reference — direct column lookup.
				if colIdx, ok2 := schema[v.Name]; ok2 {
					idx := colIdx
					evalFn = func(row exec.Row) (expr.Value, error) {
						if idx < len(row) {
							return row[idx], nil
						}
						return expr.Null, nil
					}
				}
			}
			if evalFn == nil {
				// Schema-name fast path: when an upstream operator (e.g.
				// EagerAggregation) has pre-computed and named the output column,
				// prefer a direct index lookup over expression re-evaluation. This
				// avoids calling aggregate functions as scalar functions.
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
			if evalFn == nil {
				// General path: evaluate full AST expression with loaded RowContext.
				schemaSnap := copySchema(schema)
				capturedExpr := item.Expr
				capturedG := g
				capturedParams := params
				capturedReg := reg
				evalFn = func(row exec.Row) (expr.Value, error) {
					rowCtx := buildRowCtx(row, schemaSnap, capturedG)
					return expr.Eval(capturedExpr, rowCtx, capturedParams, capturedReg)
				}
			}
		} else if colIdx, ok := schema[exprStr]; ok {
			idx := colIdx
			evalFn = func(row exec.Row) (expr.Value, error) {
				if idx < len(row) {
					return row[idx], nil
				}
				return expr.Null, nil
			}
		} else if colIdx, ok := schema[name]; ok {
			// Fall back to the alias as a variable reference.
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

	plan, err := e.planFor(query)
	if err != nil {
		return nil, err
	}

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
	return &Result{rs: rs, cols: cols, buf: buf, idxMgr: e.g.IndexManager(), tx: walTx}, nil
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
func (a *lpgMutatorAdapter) AddNode(n string) graph.NodeID {
	a.g.AddNode(n)
	id, _ := a.g.AdjList().Mapper().Lookup(n)
	return id
}

// AddEdge inserts a directed edge and returns the endpoint NodeIDs.
func (a *lpgMutatorAdapter) AddEdge(src, dst string, w float64) (srcID, dstID graph.NodeID) {
	a.g.AddEdge(src, dst, w)
	srcID, _ = a.g.AdjList().Mapper().Lookup(src)
	dstID, _ = a.g.AdjList().Mapper().Lookup(dst)
	return
}

// RemoveEdge removes the directed edge (src, dst).
func (a *lpgMutatorAdapter) RemoveEdge(src, dst string) {
	a.g.AdjList().RemoveEdge(src, dst)
}

// SetNodeLabel attaches label to n.
func (a *lpgMutatorAdapter) SetNodeLabel(n, label string) {
	a.g.SetNodeLabel(n, label)
	if a.buf != nil {
		a.buf.Enqueue(index.Change{
			Op:    index.OpAddNodeLabel,
			Node:  a.resolveID(n),
			Label: uint32(a.g.Registry().Intern(label)),
		})
	}
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
func (a *lpgMutatorAdapter) SetNodeProperty(n, key string, value lpg.PropertyValue) {
	a.g.SetNodeProperty(n, key, value)
	if a.buf != nil {
		a.buf.Enqueue(index.Change{
			Op:       index.OpSetNodeProperty,
			Node:     a.resolveID(n),
			Property: uint32(a.g.PropertyKeys().Intern(key)),
			NewValue: value,
		})
	}
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
func (a *lpgMutatorAdapter) SetEdgeProperty(src, dst, key string, value lpg.PropertyValue) {
	a.g.SetEdgeProperty(src, dst, key, value)
	if a.buf != nil {
		a.buf.Enqueue(index.Change{
			Op:       index.OpSetEdgeProperty,
			Node:     a.resolveID(src),
			Dst:      a.resolveID(dst),
			Property: uint32(a.g.PropertyKeys().Intern(key)),
			NewValue: value,
		})
	}
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
func (a *walMutatorAdapter) AddNode(n string) graph.NodeID {
	a.g.AddNode(n)
	_ = a.tx.AddNode(n) //nolint:errcheck // tx is non-nil; only ErrTxFinished possible, which cannot occur here
	id, _ := a.g.AdjList().Mapper().Lookup(n)
	return id
}

// AddEdge inserts a directed edge and returns the endpoint NodeIDs.
func (a *walMutatorAdapter) AddEdge(src, dst string, w float64) (srcID, dstID graph.NodeID) {
	a.g.AddEdge(src, dst, w)
	_ = a.tx.AddEdge(src, dst, w) //nolint:errcheck // ErrNoWeightCodec cannot occur — store has wcodec via NewEngineWithStore
	srcID, _ = a.g.AdjList().Mapper().Lookup(src)
	dstID, _ = a.g.AdjList().Mapper().Lookup(dst)
	return
}

// RemoveEdge removes the directed edge (src, dst).
func (a *walMutatorAdapter) RemoveEdge(src, dst string) {
	a.g.AdjList().RemoveEdge(src, dst)
	_ = a.tx.RemoveEdge(src, dst) //nolint:errcheck // ErrTxFinished impossible here
}

// SetNodeLabel attaches label to n.
func (a *walMutatorAdapter) SetNodeLabel(n, label string) {
	a.g.SetNodeLabel(n, label)
	_ = a.tx.SetNodeLabel(n, label) //nolint:errcheck // ErrTxFinished impossible here
	if a.buf != nil {
		a.buf.Enqueue(index.Change{
			Op:    index.OpAddNodeLabel,
			Node:  a.resolveID(n),
			Label: uint32(a.g.Registry().Intern(label)),
		})
	}
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
func (a *walMutatorAdapter) SetNodeProperty(n, key string, value lpg.PropertyValue) {
	a.g.SetNodeProperty(n, key, value)
	_ = a.tx.SetNodeProperty(n, key, value) //nolint:errcheck // ErrTxFinished impossible here
	if a.buf != nil {
		a.buf.Enqueue(index.Change{
			Op:       index.OpSetNodeProperty,
			Node:     a.resolveID(n),
			Property: uint32(a.g.PropertyKeys().Intern(key)),
			NewValue: value,
		})
	}
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
func (a *walMutatorAdapter) SetEdgeProperty(src, dst, key string, value lpg.PropertyValue) {
	a.g.SetEdgeProperty(src, dst, key, value)
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
// Compile-time assertions
// ─────────────────────────────────────────────────────────────────────────────

var _ nodeWalkerIface = (*lpgNodeWalker)(nil)
var _ labelResolverIface = (*lpgLabelResolver)(nil)
var _ exec.GraphMutator = (*walMutatorAdapter)(nil)
