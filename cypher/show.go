package cypher

// show.go — SHOW CONSTRAINTS / SHOW INDEXES execution (#1922; YIELD/WHERE/RETURN
// projection added by #2044).
//
// SHOW CONSTRAINTS and SHOW INDEXES are modern schema-introspection statements
// (the deprecated-and-removed db.constraints() / db.indexes() procedures are the
// legacy equivalents). Unlike the CREATE/DROP DDL statements — which emit zero
// rows and are eagerly applied — SHOW returns a proper result set with named
// columns and rows.
//
// # Read-only
//
// SHOW mutates nothing. Both handlers take only concurrent-safe reads on the
// constraint registry and the index manager: no writer serialisation, no WAL,
// no visibility barrier. They therefore run on the concurrent read path and are
// permitted on a read-only transaction (see ir.IsShow and Engine.BeginReadTx).
//
// # Non-divergence from db.constraints() / db.indexes()
//
// SHOW draws its rows from the SAME enumeration the legacy procedures use, so
// the two views can never disagree on which constraints/indexes exist:
//   - SHOW CONSTRAINTS reads exec.ConstraintRegistry.ListConstraintRows — the
//     exact method db.constraints() is wired to (see procs.BuiltinSources.
//     ListConstraints in NewEngineWithOptions).
//   - SHOW INDEXES reads procs.CollectIndexRows — the exact function
//     db.indexes() delegates to. It then enriches each row with the index's
//     label/property from the engine's index-definition registry.
//
// # Column contract (deliberate Neo4j alignment and deviations)
//
// The COLUMN NAMES follow modern Neo4j (entityType, labelsOrTypes, properties)
// so a modern client recognises the shape. The COLUMN VALUES stay in GoGraph's
// native vocabulary rather than Neo4j's, which is both non-divergent from
// db.constraints()/db.indexes() and more faithful to what GoGraph actually is:
//   - constraint type is "UNIQUE" / "NOT_NULL" (GoGraph), not Neo4j's
//     "UNIQUENESS" / "NODE_PROPERTY_EXISTENCE".
//   - index type is "hash" / "btree" (GoGraph's real index kinds), not Neo4j's
//     "RANGE" — reporting a hash (equality-only) index as RANGE would be wrong.
//   - state is always "ONLINE": GoGraph builds every index synchronously (fully
//     backfilled before CREATE INDEX returns), so an index is never POPULATING
//     or FAILED.
//   - entityType is always "NODE": GoGraph indexes and constraints are
//     node-scoped only.
// Columns Neo4j reports for data GoGraph does not track (id, populationPercent,
// indexProvider, owningConstraint, ownedIndex, lastRead, readCount,
// propertyType) are omitted rather than filled with fabricated values.
// See docs/cypher.md for the full contract.
//
// # YIELD / WHERE / RETURN (#2044)
//
// A modern client (Browser :schema, the official drivers' tooling) issues the
// SHOW commands with a trailing YIELD / WHERE / RETURN projection. The parser
// (ir.parseShow) resolves it into an ir.ShowProjection; the executor materialises
// the full row set exactly as the plain form does, then applies the projection
// in Go — the yielded columns are selected/aliased into a per-row scope, the
// WHERE predicate filters that scope with expr.Eval (three-valued logic: NULL and
// false both drop the row), and the RETURN items are a final scalar projection.
// The materialised rows are already ordered deterministically by name, and
// projection/filter preserve that order.

import (
	"context"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/cypher/procs"
)

// showEntityTypeNode is the entityType column value for every SHOW row: GoGraph
// constraints and indexes are node-scoped only.
const showEntityTypeNode = "NODE"

// showIndexStateOnline is the state column value for every SHOW INDEXES row:
// GoGraph backfills every index synchronously before CREATE INDEX returns, so an
// index is always fully populated (never POPULATING/FAILED).
const showIndexStateOnline = "ONLINE"

// runShowConstraints executes SHOW CONSTRAINTS. It projects the shared
// db.constraints() enumeration ([name, type, label, property], already sorted
// deterministically by ListConstraintRows) into the Neo4j-aligned column shape
// [name, type, entityType, labelsOrTypes, properties], then applies the optional
// YIELD/WHERE/RETURN projection (#2044). params supplies any query parameters the
// WHERE/RETURN expressions reference.
func (e *Engine) runShowConstraints(ctx context.Context, proj *ir.ShowProjection, params map[string]expr.Value) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	src := e.constraintReg.ListConstraintRows() // [name, type, label, property]
	rows := make([]exec.Row, 0, len(src))
	for _, r := range src {
		// ListConstraintRows guarantees a four-element [name, type, label,
		// property] row; guard defensively so a future shape change fails a test
		// rather than panicking a live query.
		if len(r) != 4 {
			continue
		}
		rows = append(rows, exec.Row{
			r[0], // name
			r[1], // type: "UNIQUE" | "NOT_NULL"
			expr.StringValue(showEntityTypeNode),
			expr.ListValue{r[2]}, // labelsOrTypes
			expr.ListValue{r[3]}, // properties
		})
	}
	return e.newShowResult(ctx, ir.ShowConstraintColumns, rows, proj, params)
}

// runShowIndexes executes SHOW INDEXES. It projects the shared db.indexes()
// enumeration ([name, type], already sorted by name via CollectIndexRows) into
// the Neo4j-aligned column shape [name, state, type, entityType, labelsOrTypes,
// properties], enriching labelsOrTypes/properties from the engine's
// index-definition registry, then applies the optional YIELD/WHERE/RETURN
// projection (#2044). params supplies any query parameters the WHERE/RETURN
// expressions reference.
func (e *Engine) runShowIndexes(ctx context.Context, proj *ir.ShowProjection, params map[string]expr.Value) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	src := procs.CollectIndexRows(e.g.IndexManager()) // [name, type], sorted by name
	rows := make([]exec.Row, 0, len(src))
	for _, r := range src {
		if len(r) != 2 {
			continue
		}
		nameVal, typeVal := r[0], r[1]
		// labelsOrTypes/properties are known for user indexes (recorded in the
		// index-def registry at CREATE INDEX). A UNIQUE-constraint backing index
		// is listed (matching db.indexes()) but is not in that registry, so its
		// label/property are reported as empty lists rather than fabricated.
		labels, props := expr.ListValue{}, expr.ListValue{}
		if name, ok := nameVal.(expr.StringValue); ok {
			if label, prop, found := e.indexDefReg.labelProp(string(name)); found {
				labels = expr.ListValue{expr.StringValue(label)}
				props = expr.ListValue{expr.StringValue(prop)}
			}
		}
		rows = append(rows, exec.Row{
			nameVal, // name
			expr.StringValue(showIndexStateOnline),
			typeVal, // type: "hash" | "btree"
			expr.StringValue(showEntityTypeNode),
			labels,
			props,
		})
	}
	return e.newShowResult(ctx, ir.ShowIndexColumns, rows, proj, params)
}

// newShowResult wraps a materialised SHOW row set in a streaming Result. When
// proj is nil (the plain form) the rows and columns are emitted as-is. When proj
// is non-nil the YIELD/WHERE/RETURN projection is applied first (#2044). The rows
// are snapshotted from the registries before iteration, so the Result needs no
// index buffer, index manager, or transaction (SHOW has no write side effects to
// flush). ctx governs iteration cancellation.
func (e *Engine) newShowResult(ctx context.Context, cols []string, rows []exec.Row, proj *ir.ShowProjection, params map[string]expr.Value) (*Result, error) {
	if proj != nil {
		var err error
		if cols, rows, err = e.applyShowProjection(cols, rows, proj, params); err != nil {
			return nil, err
		}
	}
	rs := exec.Run(ctx, exec.NewStaticRows(rows), cols)
	return newResult(rs, cols, nil, nil, nil), nil
}

// applyShowProjection applies a parsed YIELD/WHERE/RETURN projection to the
// materialised SHOW rows and returns the projected columns and rows. The parser
// has already validated the projection (known columns, the YIELD scope barrier,
// and the rejected RETURN sub-clauses), so this evaluates the WHERE predicate and
// the RETURN items per row against the yielded scope and never has to re-check
// scoping. Output order follows the (name-sorted) input order.
func (e *Engine) applyShowProjection(cols []string, rows []exec.Row, proj *ir.ShowProjection, params map[string]expr.Value) ([]string, []exec.Row, error) {
	colIdx := make(map[string]int, len(cols))
	for i, c := range cols {
		colIdx[c] = i
	}
	// Pre-resolve each yielded item's source column index; the parser validated
	// every Source against cols, so the lookup always succeeds.
	srcIdx := make([]int, len(proj.Project))
	for i, p := range proj.Project {
		srcIdx[i] = colIdx[p.Source]
	}

	outCols := showOutputColumns(proj)
	out := make([]exec.Row, 0, len(rows))
	// A per-row scope map reused across rows: it is fully overwritten each
	// iteration and only read (never retained) by expr.Eval.
	scope := make(expr.RowContext, len(proj.Project))
	for _, row := range rows {
		for i, p := range proj.Project {
			scope[p.Output] = row[srcIdx[i]]
		}
		if proj.Where != nil {
			v, err := expr.Eval(proj.Where, scope, params, e.reg)
			if err != nil {
				return nil, nil, err
			}
			if !expr.IsTruthy(v) {
				continue
			}
		}
		outRow, err := e.showOutputRow(proj, scope, srcIdx, row, params)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, outRow)
	}
	return outCols, out, nil
}

// showOutputColumns returns the output column names for a SHOW projection: the
// RETURN item names (precomputed by the parser) when an explicit RETURN clause is
// present, otherwise the yielded (aliased) column names — which is also the
// RETURN * case, since RETURN * returns the yielded columns.
func showOutputColumns(proj *ir.ShowProjection) []string {
	if proj.Return != nil && !proj.Return.All {
		return proj.ReturnColumns
	}
	yielded := make([]string, len(proj.Project))
	for i, p := range proj.Project {
		yielded[i] = p.Output
	}
	return yielded
}

// showOutputRow builds one output row from the yielded scope: the RETURN items
// evaluated per row when a RETURN is present (yielded values in order for
// RETURN *), otherwise the yielded values themselves.
func (e *Engine) showOutputRow(proj *ir.ShowProjection, scope expr.RowContext, srcIdx []int, row exec.Row, params map[string]expr.Value) (exec.Row, error) {
	yielded := func() exec.Row {
		vals := make(exec.Row, len(proj.Project))
		for i := range proj.Project {
			vals[i] = row[srcIdx[i]]
		}
		return vals
	}
	if proj.Return == nil || proj.Return.All {
		return yielded(), nil
	}
	vals := make(exec.Row, len(proj.Return.Items))
	for i, item := range proj.Return.Items {
		v, err := expr.Eval(item.Expr, scope, params, e.reg)
		if err != nil {
			return nil, err
		}
		vals[i] = v
	}
	return vals, nil
}
