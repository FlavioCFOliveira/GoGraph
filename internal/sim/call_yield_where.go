package sim

// call_yield_where.go — DST adjudication of `CALL … YIELD … WHERE <pred>`
// (rmp #2462).
//
// # Status of the known defect
//
// docs/dst-feature-coverage.md recorded that `CALL … YIELD … WHERE` silently
// ignored its WHERE filter, backlogged as #1966. That claim is STALE: the
// defect was FIXED (commit 6b6b3186, "fix(cypher): apply the WHERE predicate on
// CALL ... YIELD ... WHERE"), the fix is present at HEAD in two places — the
// visitor now captures Call.Where (cypher/parser/visitor.go) and the translator
// lifts it as a Selection over the ProcedureCall (cypher/ir/translator.go) —
// and cypher/call_yield_where_test.go guards it. This checker therefore pins
// the CORRECT behaviour with a model-computed expectation, not the defect.
//
// # What is checked
//
// The statement forms `SHOW INDEXES YIELD … WHERE …` and `SHOW CONSTRAINTS
// YIELD … WHERE …` were already covered (#2044). The procedure form is a
// different code path and was not. Both `db.constraints()` and `db.indexes()`
// are filtered here by an equality predicate on a name the harness's DDL model
// knows, and the result must equal the model-side filter exactly.
//
// # Non-vacuity
//
// A predicate that selects everything proves nothing: were the WHERE dropped
// again, an all-matching filter would still compare equal. Each probe therefore
// also asserts that the filter is STRICTLY narrowing whenever the model
// enumerates two or more rows — one row kept out of N > 1. On a model with a
// single row the filter cannot discriminate and the narrowing assertion is not
// made; [TestCallYieldWhere_ScenarioModelsAreDiscriminating] pins that the live
// scenario models do enumerate enough rows for the discriminating arm to fire.

import (
	"context"
	"fmt"
)

// callYieldWhereProbe is one procedure enumeration to filter: the procedure's
// YIELD/RETURN projection and the model-side name list it must reproduce.
type callYieldWhereProbe struct {
	label string
	// query renders the CALL … YIELD … WHERE … RETURN statement selecting name.
	query func(name string) string
	// names is the full modelled enumeration this procedure must report.
	names []string
}

// checkCallYieldWhere asserts that a WHERE predicate on a `CALL … YIELD`
// sub-clause filters the yielded rows, for both schema-introspection
// procedures, against the harness's DDL model. See the file comment for the
// #1966 history and the non-vacuity rule.
func checkCallYieldWhere(ctx context.Context, tick int64, model *SchemaModel, engine *EngineAdapter) []Violation {
	probes := [...]callYieldWhereProbe{
		{
			label: "schema introspection (CALL db.constraints YIELD WHERE)",
			query: func(name string) string {
				return "CALL db.constraints() YIELD name, type WHERE name = " +
					quoteCypherString(name) + " RETURN name"
			},
			names: model.constraintNameList(),
		},
		{
			label: "schema introspection (CALL db.indexes YIELD WHERE)",
			query: func(name string) string {
				return "CALL db.indexes() YIELD name, type WHERE name = " +
					quoteCypherString(name) + " RETURN name"
			},
			names: model.indexNameList(),
		},
	}

	var vs []Violation
	for _, p := range probes {
		if len(p.names) == 0 {
			// Nothing declared of this kind: there is no row to select, so the
			// probe has no discriminating power and is skipped rather than
			// asserting a trivially-empty equality.
			continue
		}
		// Filter on the lexicographically first name — a deterministic choice, so
		// the probe draws no randomness and the run stays bit-reproducible.
		target := p.names[0]
		query := p.query(target)

		rows, err := engine.queryRowStrings(ctx, query, 1)
		if err != nil {
			vs = append(vs, Violation{
				Kind: ViolationGraphIntegrity, Tick: tick, Op: p.label,
				Message: fmt.Sprintf("%s failed: %v", query, err),
			})
			continue
		}

		want := [][]string{{canonicalValueString(target)}}
		if diff, bad := rowSetDiff(rows, want); bad {
			vs = append(vs, Violation{
				Kind: ViolationOracleDeviation, Tick: tick, Op: p.label,
				Message: fmt.Sprintf(
					"CALL … YIELD … WHERE projection diverges from the model (the WHERE predicate on a "+
						"YIELD sub-clause must filter the yielded rows — see rmp #1966):\n%s\nquery: %s",
					diff, query),
			})
			continue
		}

		// Non-vacuity: with two or more modelled rows the predicate must have
		// excluded at least one. A filter that keeps everything would compare
		// equal even if the WHERE were dropped again, which is exactly the
		// regression this probe exists to catch.
		if len(p.names) >= 2 && len(rows) >= len(p.names) {
			vs = append(vs, Violation{
				Kind: ViolationVacuousRun, Tick: tick, Op: p.label,
				Message: fmt.Sprintf(
					"vacuous filter: the model enumerates %d rows and the WHERE-filtered projection "+
						"returned %d — the predicate excluded nothing, so it cannot detect a dropped "+
						"WHERE (rmp #1966)\nquery: %s", len(p.names), len(rows), query),
			})
		}
	}
	return vs
}
