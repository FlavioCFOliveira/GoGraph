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
	"sync"

	"github.com/RoaringBitmap/roaring/v2/roaring64"

	"gograph/cypher/exec"
	"gograph/cypher/expr"
	"gograph/cypher/funcs"
	"gograph/cypher/ir"
	"gograph/cypher/parser"
	"gograph/graph"
	"gograph/graph/lpg"
)

// ─────────────────────────────────────────────────────────────────────────────
// Engine
// ─────────────────────────────────────────────────────────────────────────────

// Engine is the public query engine. It binds a graph, a function registry,
// and a plan cache, and exposes a single Run method for query execution.
//
// Engine is safe for concurrent use.
type Engine struct {
	g     *lpg.Graph[string, float64]
	reg   expr.FunctionRegistry
	cache sync.Map // map[string]ir.LogicalPlan
}

// NewEngine creates an Engine backed by g. The default built-in function
// registry ([funcs.DefaultRegistry]) is used.
func NewEngine(g *lpg.Graph[string, float64]) *Engine {
	return &Engine{
		g:   g,
		reg: funcs.DefaultRegistry,
	}
}

// NewEngineWithRegistry creates an Engine backed by g using a custom function
// registry.
func NewEngineWithRegistry(g *lpg.Graph[string, float64], reg expr.FunctionRegistry) *Engine {
	return &Engine{g: g, reg: reg}
}

// Run executes query against the engine's graph and returns a streaming
// [Result]. The caller must call [Result.Close] when done.
//
// A wrapped [*parser.ParseError] is returned when the query has a syntax
// error; the error includes line and column information.
//
// Sprint 25 support: MATCH (full scan or label scan) + RETURN.
func (e *Engine) Run(ctx context.Context, query string, params map[string]expr.Value) (*Result, error) {
	// ── 1. Parse or retrieve from plan cache ─────────────────────────────────
	plan, err := e.planFor(query)
	if err != nil {
		return nil, err
	}

	// ── 2. Build physical operator tree ─────────────────────────────────────
	walker := &lpgNodeWalker{g: e.g}
	labelSrc := &lpgLabelResolver{g: e.g}
	op, cols, err := BuildPlan(plan, walker, labelSrc, e.reg, params)
	if err != nil {
		return nil, fmt.Errorf("cypher: build plan: %w", err)
	}

	// ── 3. Wrap in streaming Result ──────────────────────────────────────────
	rs := exec.Run(ctx, op, cols)
	return &Result{rs: rs, cols: cols}, nil
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
	rs   *exec.ResultSet
	cols []string
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
func (r *Result) Close() error { return r.rs.Close() }

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
	child, err := buildOperator(pr.Child, walker, labelSrc, reg, params, schema)
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

// buildOperator recursively converts an IR plan node to a physical operator.
// schema accumulates variable→column-index bindings as operators are visited
// top-down (left-to-right for children).
func buildOperator(
	plan ir.LogicalPlan,
	walker nodeWalkerIface,
	labelSrc labelResolverIface,
	reg expr.FunctionRegistry,
	params map[string]expr.Value,
	schema map[string]int,
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

	case *ir.Selection:
		child, err := buildOperator(p.Child, walker, labelSrc, reg, params, schema)
		if err != nil {
			return nil, err
		}
		// Sprint 25: predicate string is the opaque AST string from the IR.
		// Full expression re-evaluation is not in scope; emit a pass-through filter.
		return exec.NewFilter(child, func(_ exec.Row) (expr.Value, error) {
			return expr.BoolValue(true), nil
		}), nil

	case *ir.Projection:
		child, err := buildOperator(p.Child, walker, labelSrc, reg, params, schema)
		if err != nil {
			return nil, err
		}
		return buildIRProjection(p.Items, child, schema)

	case *ir.Expand:
		child, err := buildOperator(p.Child, walker, labelSrc, reg, params, schema)
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

	default:
		return nil, fmt.Errorf("cypher: unsupported IR node %T", plan)
	}
}

// buildIRProjection converts IR ProjectionItems to a physical Project operator.
// For Sprint 25 each item expression is treated as a variable reference looked
// up in schema, falling back to NULL for unresolvable expressions.
func buildIRProjection(
	items []ir.ProjectionItem,
	child exec.Operator,
	schema map[string]int,
) (*exec.Project, error) {
	projItems := make([]exec.ProjectionItem, len(items))
	for i, item := range items {
		name := item.Name
		exprStr := item.Expression

		var evalFn func(exec.Row) (expr.Value, error)
		if colIdx, ok := schema[exprStr]; ok {
			idx := colIdx
			evalFn = func(row exec.Row) (expr.Value, error) {
				if idx < len(row) {
					return row[idx], nil
				}
				return expr.Null, nil
			}
		} else if colIdx, ok := schema[name]; ok {
			// Try using the alias as a variable reference.
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
// Compile-time assertions
// ─────────────────────────────────────────────────────────────────────────────

var _ nodeWalkerIface = (*lpgNodeWalker)(nil)
var _ labelResolverIface = (*lpgLabelResolver)(nil)
