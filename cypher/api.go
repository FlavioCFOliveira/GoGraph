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

	"gograph/cypher/exec"
	"gograph/cypher/expr"
	"gograph/cypher/funcs"
	"gograph/cypher/ir"
	"gograph/cypher/parser"
	"gograph/graph"
	"gograph/graph/index"
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
	rs     *exec.ResultSet
	cols   []string
	buf    *exec.IndexBuffer // non-nil only for RunInTx results
	idxMgr *index.Manager    // non-nil only when buf != nil
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
// When the result was created by [Engine.RunInTx], Close also commits or
// rolls back the buffered index changes: it commits on clean close, and
// rolls back when either Close or iteration returned a non-nil error.
func (r *Result) Close() error {
	err := r.rs.Close()
	if r.buf != nil {
		if err != nil || r.rs.Err() != nil {
			r.buf.Rollback()
		} else {
			r.buf.Commit(r.idxMgr)
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
	schema := make(map[string]int)

	// When the IR root is a ProduceResults, use its declared columns; otherwise
	// treat the plan as a write-only query with no output columns. A CREATE
	// without RETURN has a write operator as root.
	if pr, ok := plan.(*ir.ProduceResults); ok {
		cols = pr.Columns
		child, buildErr := buildOperatorWrite(pr.Child, walker, labelSrc, reg, params, schema, mutator)
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
	child, buildErr := buildOperatorWrite(plan, walker, labelSrc, reg, params, schema, mutator)
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
) (exec.Operator, error) {
	if plan == nil {
		// A nil plan arises when a write clause has no driving subplan (e.g.
		// CREATE without a leading MATCH). Return a single-row operator that
		// drives the write operator exactly once.
		return exec.NewSingleRowOperator(), nil
	}

	switch p := plan.(type) {

	case *ir.CreateNode:
		child, err := buildOperatorWrite(p.Child, walker, labelSrc, reg, params, schema, mutator)
		if err != nil {
			return nil, err
		}
		if p.NodeVar != "" {
			schema[p.NodeVar] = len(schema)
		}
		return exec.NewCreateNode(p.NodeVar, p.Labels, p.Properties, child, mutator)

	case *ir.CreateRelationship:
		child, err := buildOperatorWrite(p.Child, walker, labelSrc, reg, params, schema, mutator)
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
		child, err := buildOperatorWrite(p.Child, walker, labelSrc, reg, params, schema, mutator)
		if err != nil {
			return nil, err
		}
		schemaCopy := copySchema(schema)
		return exec.NewSetProperty(p.EntityVar, p.PropertyKey, p.Value, schemaCopy, child, mutator)

	case *ir.SetLabels:
		child, err := buildOperatorWrite(p.Child, walker, labelSrc, reg, params, schema, mutator)
		if err != nil {
			return nil, err
		}
		schemaCopy := copySchema(schema)
		return exec.NewSetLabels(p.NodeVar, p.Labels, schemaCopy, child, mutator), nil

	case *ir.RemoveProperty:
		child, err := buildOperatorWrite(p.Child, walker, labelSrc, reg, params, schema, mutator)
		if err != nil {
			return nil, err
		}
		schemaCopy := copySchema(schema)
		return exec.NewRemoveProperty(p.EntityVar, p.PropertyKey, schemaCopy, child, mutator), nil

	case *ir.RemoveLabels:
		child, err := buildOperatorWrite(p.Child, walker, labelSrc, reg, params, schema, mutator)
		if err != nil {
			return nil, err
		}
		schemaCopy := copySchema(schema)
		return exec.NewRemoveLabels(p.NodeVar, p.Labels, schemaCopy, child, mutator), nil

	case *ir.DeleteNode:
		child, err := buildOperatorWrite(p.Child, walker, labelSrc, reg, params, schema, mutator)
		if err != nil {
			return nil, err
		}
		schemaCopy := copySchema(schema)
		return exec.NewDeleteNode(p.NodeVar, schemaCopy, child, mutator), nil

	case *ir.DeleteRelationship:
		child, err := buildOperatorWrite(p.Child, walker, labelSrc, reg, params, schema, mutator)
		if err != nil {
			return nil, err
		}
		schemaCopy := copySchema(schema)
		return exec.NewDeleteRelationship(p.RelVar, schemaCopy, child, mutator), nil

	case *ir.DetachDelete:
		child, err := buildOperatorWrite(p.Child, walker, labelSrc, reg, params, schema, mutator)
		if err != nil {
			return nil, err
		}
		schemaCopy := copySchema(schema)
		return exec.NewDetachDelete(p.NodeVar, schemaCopy, child, mutator), nil

	case *ir.Merge:
		child, err := buildOperatorWrite(p.Child, walker, labelSrc, reg, params, schema, mutator)
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
		return exec.NewMerge(
			firstVar(p.BoundVars),
			labels,
			props,
			p.OnCreate, p.OnMatch,
			searchFn,
			schemaCopy,
			child,
			mutator,
		)

	default:
		// Fall through to the read-operator builder.
		return buildOperator(plan, walker, labelSrc, reg, params, schema)
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

	case *ir.Apply:
		// Build the outer plan first so its vars enter the schema.
		outer, err := buildOperator(p.Outer, walker, labelSrc, reg, params, schema)
		if err != nil {
			return nil, err
		}
		// The Argument operator is the leaf of the inner plan; it re-emits the
		// outer row so correlated inner scans can consume it. For non-correlated
		// inner plans (independent scans) the arg is inert but required.
		arg := exec.NewArgument()
		inner, err := buildOperator(p.Inner, walker, labelSrc, reg, params, schema)
		if err != nil {
			return nil, err
		}
		return exec.NewApply(outer, inner, arg), nil

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
	plan, err := e.planFor(query)
	if err != nil {
		return nil, err
	}

	walker := &lpgNodeWalker{g: e.g}
	labelSrc := &lpgLabelResolver{g: e.g}
	buf := &exec.IndexBuffer{}
	mutator := &lpgMutatorAdapter{g: e.g, buf: buf}

	op, cols, err := BuildPlanWithMutator(plan, walker, labelSrc, e.reg, params, mutator)
	if err != nil {
		return nil, fmt.Errorf("cypher: build plan: %w", err)
	}

	rs := exec.Run(ctx, op, cols)
	return &Result{rs: rs, cols: cols, buf: buf, idxMgr: e.g.IndexManager()}, nil
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
// Compile-time assertions
// ─────────────────────────────────────────────────────────────────────────────

var _ nodeWalkerIface = (*lpgNodeWalker)(nil)
var _ labelResolverIface = (*lpgLabelResolver)(nil)
