package cypher

// show.go — SHOW CONSTRAINTS / SHOW INDEXES execution (#1922).
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

import (
	"context"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/procs"
)

// showEntityTypeNode is the entityType column value for every SHOW row: GoGraph
// constraints and indexes are node-scoped only.
const showEntityTypeNode = "NODE"

// showIndexStateOnline is the state column value for every SHOW INDEXES row:
// GoGraph backfills every index synchronously before CREATE INDEX returns, so an
// index is always fully populated (never POPULATING/FAILED).
const showIndexStateOnline = "ONLINE"

// showConstraintsColumns is the ordered SHOW CONSTRAINTS output schema.
var showConstraintsColumns = []string{"name", "type", "entityType", "labelsOrTypes", "properties"}

// showIndexesColumns is the ordered SHOW INDEXES output schema.
var showIndexesColumns = []string{"name", "state", "type", "entityType", "labelsOrTypes", "properties"}

// runShowConstraints executes SHOW CONSTRAINTS. It projects the shared
// db.constraints() enumeration ([name, type, label, property], already sorted
// deterministically by ListConstraintRows) into the Neo4j-aligned column shape
// [name, type, entityType, labelsOrTypes, properties].
func (e *Engine) runShowConstraints(ctx context.Context) (*Result, error) {
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
	return e.newShowResult(ctx, showConstraintsColumns, rows), nil
}

// runShowIndexes executes SHOW INDEXES. It projects the shared db.indexes()
// enumeration ([name, type], already sorted by name via CollectIndexRows) into
// the Neo4j-aligned column shape [name, state, type, entityType, labelsOrTypes,
// properties], enriching labelsOrTypes/properties from the engine's
// index-definition registry.
func (e *Engine) runShowIndexes(ctx context.Context) (*Result, error) {
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
	return e.newShowResult(ctx, showIndexesColumns, rows), nil
}

// newShowResult wraps a materialised SHOW row set in a streaming Result with the
// given columns. The rows are snapshotted from the registries before iteration,
// so the Result needs no index buffer, index manager, or transaction (SHOW has
// no write side effects to flush). ctx governs iteration cancellation.
func (e *Engine) newShowResult(ctx context.Context, cols []string, rows []exec.Row) *Result {
	rs := exec.Run(ctx, exec.NewStaticRows(rows), cols)
	return newResult(rs, cols, nil, nil, nil)
}
