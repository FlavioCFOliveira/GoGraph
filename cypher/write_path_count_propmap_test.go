package cypher_test

// write_path_count_propmap_test.go — regression battery for rmp #2781: the
// expression placements on the WRITE path that rmp #2660 did NOT reach.
//
// #2660 (72c31031) wired the per-statement subquery and pattern evaluators onto
// [buildPlanWithMutatorFull]'s buildOpts, which fixed every placement whose
// expression is evaluated through evalRow. Four property/RHS evaluator builders
// call expr.Eval DIRECTLY, so buildOpts never reached them and a COUNT { … } in
// any of their positions still failed with a typed error:
//
//	MATCH (a:P) CREATE (:Q {c: COUNT { … }})
//	  → eval: COUNT { … } subquery is not supported in this evaluation context
//	          (no SubqueryEvaluator wired)
//	MATCH (a:P) MERGE (q:Q {k: a.sid}) ON CREATE SET q.c = COUNT { … }
//	  → exec: Merge: ON CREATE: eval: COUNT { … } subquery is not supported …
//
// # Which builder owns which placement — measured, not assumed
//
// The four builders were routed through evalRow ONE AT A TIME and the whole
// placement set re-run against each arm. Every placement is fixed by exactly one
// builder, so the mapping is established rather than inferred:
//
//	buildPropsEvalFn        CREATE node property map, CREATE relationship
//	                        property map, MERGE inline property map
//	buildMapEvalFn          SET x = {…} and SET x += {…} (map LITERAL source)
//	buildMergeActionEvals   MERGE ON CREATE SET x.p = … / ON MATCH SET x.p = …
//	buildExprMapEvalFn      SET x =/+= <non-map-literal expression>, and
//	                        MERGE ON CREATE/ON MATCH SET x =/+= <expression>
//
// The last row is a finding, not a restatement of the task: NONE of the five
// placements named in #2781 reaches buildExprMapEvalFn. Its own placement class
// — the whole-entity SET whose right-hand side is not a map literal — failed
// identically and is gated here in its own right, so the change to that builder
// is not vacuous.
//
// # The oracle
//
// Every case compares the value actually WRITTEN against the value the SAME
// expression yields through a plain RETURN on an engine at the same state. An
// assertion that merely checked "no error" would pass on a wrong value; an
// assertion against a hand-written constant would still pass on a build where
// both paths had drifted together.
//
// # The fixture discriminates per row
//
// Only the FIRST TWO of the four :P nodes get a :Z self-edge, so the counted
// answer is (1, 1, 0, 0) rather than uniform. A build that evaluated the
// subquery but lost the row correlation would answer the same thing four times
// and fail here.

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// newWPCFixture builds the #2781 fixture: wpsNodes :P nodes carrying
// sid = wpsBaseSID+i, with a :Z SELF-edge on the first two only. The half-edged
// shape is what makes the counted answer differ per row.
func newWPCFixture(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	e := cypher.NewEngine(g)
	for i := range wpsNodes {
		if _, err := e.RunAny(context.Background(), fmt.Sprintf(`CREATE (:P {sid:%d})`, wpsBaseSID+i), nil); err != nil {
			t.Fatalf("fixture node %d: %v", i, err)
		}
	}
	half := fmt.Sprintf(`MATCH (a:P) WHERE a.sid < %d CREATE (a)-[:Z]->(a)`, wpsBaseSID+2)
	if _, err := e.RunAny(context.Background(), half, nil); err != nil {
		t.Fatalf("fixture edges: %v", err)
	}
	return e
}

// wpcOracle is the read-only statement every gated case is measured against:
// the identical COUNT { … } in a RETURN item, keyed by the same sid.
const wpcOracle = `MATCH (a:P) RETURN a.sid AS k, COUNT { MATCH (a)-[:Z]->(:P) } AS c`

// wpcCase is one write-path placement, the read-back that observes what it
// wrote, and the read-only statement whose rows the read-back must equal.
type wpcCase struct {
	name string
	// builder names the evaluator builder this case discriminates. Established
	// by routing each builder through evalRow alone; see the file comment.
	builder string
	// setup runs on BOTH arms before anything else.
	setup string
	// write is the statement under test.
	write string
	// probe reads back what write stored.
	probe string
	// oracle is the read-only statement whose rows probe must equal. Empty
	// means [wpcOracle].
	oracle string
	// wantRowCount pins the size of the shared answer, so two arms that agree
	// on the empty set cannot pass by agreeing with each other.
	wantRowCount int
	// control marks a case that passes with the fix neutralised. It is here to
	// mark the boundary of the defect, not to gate it.
	control bool
	why     string
}

// TestWritePathCountInPropertyMaps is the acceptance battery for rmp #2781.
func TestWritePathCountInPropertyMaps(t *testing.T) {
	const all = wpsNodes
	// preQ gives every :P node a matching :Q node, so a MERGE on the same key
	// takes its ON MATCH branch rather than ON CREATE.
	const preQ = `MATCH (a:P) CREATE (:Q {k: a.sid})`
	const probeQ = `MATCH (q:Q) RETURN q.k AS k, q.c AS c`
	const probeP = `MATCH (a:P) RETURN a.k AS k, a.c AS c`

	cases := []wpcCase{
		// ── buildPropsEvalFn ────────────────────────────────────────────────
		{
			name:         "create_node_property_map",
			builder:      "buildPropsEvalFn",
			write:        `MATCH (a:P) CREATE (:Q {k: a.sid, c: COUNT { MATCH (a)-[:Z]->(:P) }})`,
			probe:        probeQ,
			wantRowCount: all,
			why:          "placement 1 of the #2781 report: the node property map of a CREATE",
		},
		{
			name:         "create_relationship_property_map",
			builder:      "buildPropsEvalFn",
			write:        `MATCH (a:P) CREATE (a)-[:W {k: a.sid, c: COUNT { MATCH (a)-[:Z]->(:P) }}]->(a)`,
			probe:        `MATCH ()-[r:W]->() RETURN r.k AS k, r.c AS c`,
			wantRowCount: all,
			why:          "placement 2: the relationship property map of a CREATE. The created :W edge is a different type from the counted :Z, so the count is insensitive to the write's own effect",
		},
		{
			name:         "merge_inline_property_map",
			builder:      "buildPropsEvalFn",
			write:        `MATCH (a:P) MERGE (q:Q {k: a.sid, c: COUNT { MATCH (a)-[:Z]->(:P) }})`,
			probe:        probeQ,
			wantRowCount: all,
			why:          "placement 3: the inline property map of a MERGE, which is also its search predicate — the same builder in merge context (a null value would raise MergeReadOwnWrites rather than be omitted)",
		},
		{
			name:         "create_node_property_map_degree_comprehension",
			builder:      "buildPropsEvalFn",
			write:        `MATCH (a:P) CREATE (:Q {k: a.sid, c: size([(a)-[:Z]->() | 1])})`,
			probe:        probeQ,
			oracle:       `MATCH (a:P) RETURN a.sid AS k, size([(a)-[:Z]->() | 1]) AS c`,
			wantRowCount: all,
			why:          "the OTHER evaluator: #2660 established that the degree-countable size(<comprehension>) is deliberately left unhoisted (#2264) so the runtime answers it from the adjacency, which means it needs patEval and not subEval. It proves the route hands the builder BOTH fields, not only the one COUNT needs. A BARE pattern predicate would have been the sharper probe but sema refuses it as a projection value, so it has no read-only oracle to be measured against",
		},
		{
			name:         "create_node_property_map_exists",
			builder:      "buildPropsEvalFn",
			write:        `MATCH (a:P) CREATE (:Q {k: a.sid, c: EXISTS { MATCH (a)-[:Z]->(:P) }})`,
			probe:        probeQ,
			oracle:       `MATCH (a:P) RETURN a.sid AS k, EXISTS { MATCH (a)-[:Z]->(:P) } AS c`,
			wantRowCount: all,
			why:          "EXISTS { } in the same position fails identically before the fix; it is the second subEval construct and is pinned so the fix is not COUNT-shaped",
		},

		// ── buildMergeActionEvals ───────────────────────────────────────────
		{
			name:         "merge_on_create_set_property",
			builder:      "buildMergeActionEvals",
			write:        `MATCH (a:P) MERGE (q:Q {k: a.sid}) ON CREATE SET q.c = COUNT { MATCH (a)-[:Z]->(:P) }`,
			probe:        probeQ,
			wantRowCount: all,
			why:          "placement 4a, the one the #2781 report quotes by its error text: exec: Merge: ON CREATE: …",
		},
		{
			name:         "merge_on_match_set_property",
			builder:      "buildMergeActionEvals",
			setup:        preQ,
			write:        `MATCH (a:P) MERGE (q:Q {k: a.sid}) ON MATCH SET q.c = COUNT { MATCH (a)-[:Z]->(:P) }`,
			probe:        probeQ,
			wantRowCount: all,
			why:          "placement 4b: the ON MATCH branch, reached only because the setup pre-creates the :Q nodes the MERGE then finds",
		},

		// ── buildMapEvalFn ──────────────────────────────────────────────────
		{
			name:         "set_map_literal_replace",
			builder:      "buildMapEvalFn",
			write:        `MATCH (a:P) SET a = {k: a.sid, c: COUNT { MATCH (a)-[:Z]->(:P) }}`,
			probe:        probeP,
			wantRowCount: all,
			why:          "placement 5: a map-literal SET. `=` replaces the property set, so sid is gone by the read-back and k carries it; the counted pattern is label- and type-scoped, so no property write can move it",
		},
		{
			name:         "set_map_literal_merge",
			builder:      "buildMapEvalFn",
			write:        `MATCH (a:P) SET a += {k: a.sid, c: COUNT { MATCH (a)-[:Z]->(:P) }}`,
			probe:        probeP,
			wantRowCount: all,
			why:          "the `+=` spelling of placement 5, which reaches the same builder by the same route",
		},

		// ── buildExprMapEvalFn ──────────────────────────────────────────────
		//
		// A FINDING, not one of the five: no placement named in #2781 reaches
		// this builder, so without these cases the change to it would be
		// unproven. Its class is the whole-entity SET whose right-hand side is
		// NOT a map literal — a CASE, a coalesce, a map projection — which the
		// SetAllProperties build routes to ExprAST instead of MapAST.
		{
			name:         "set_all_from_case_expression",
			builder:      "buildExprMapEvalFn",
			write:        `MATCH (a:P) SET a = CASE WHEN COUNT { MATCH (a)-[:Z]->(:P) } = 1 THEN {k: a.sid, c: 1} ELSE {k: a.sid, c: 0} END`,
			probe:        probeP,
			wantRowCount: all,
			why:          "a CASE is not a map literal, so it is evaluated whole by buildExprMapEvalFn; the COUNT sits in the CASE's condition",
		},
		{
			name:         "set_all_from_coalesce_expression",
			builder:      "buildExprMapEvalFn",
			write:        `MATCH (a:P) SET a += coalesce({k: a.sid, c: COUNT { MATCH (a)-[:Z]->(:P) }}, {})`,
			probe:        probeP,
			wantRowCount: all,
			why:          "the same builder reached through a function call whose argument is the map",
		},
		{
			name:         "merge_on_create_set_all_map",
			builder:      "buildExprMapEvalFn",
			write:        `MATCH (a:P) MERGE (q:Q {k: a.sid}) ON CREATE SET q += {c: COUNT { MATCH (a)-[:Z]->(:P) }}`,
			probe:        probeQ,
			wantRowCount: all,
			why:          "the WHOLE-ENTITY form of ON CREATE SET, which goes to buildMergeSetAllActions → buildExprMapEvalFn and NOT to buildMergeActionEvals — measured, and the reason the two ON CREATE cases in this file are not duplicates",
		},
		{
			name:         "merge_on_match_set_all_map",
			builder:      "buildExprMapEvalFn",
			setup:        preQ,
			write:        `MATCH (a:P) MERGE (q:Q {k: a.sid}) ON MATCH SET q += {c: COUNT { MATCH (a)-[:Z]->(:P) }}`,
			probe:        probeQ,
			wantRowCount: all,
			why:          "the ON MATCH half of the whole-entity form",
		},

		// ── Controls: the boundary of the defect ────────────────────────────
		{
			name:         "control_ordinary_property_expression",
			builder:      "buildPropsEvalFn",
			write:        `MATCH (a:P) CREATE (:Q {k: a.sid, c: a.sid - ` + fmt.Sprint(wpsBaseSID) + `})`,
			probe:        probeQ,
			oracle:       `MATCH (a:P) RETURN a.sid AS k, a.sid - ` + fmt.Sprint(wpsBaseSID) + ` AS c`,
			wantRowCount: all,
			control:      true,
			why:          "CONTROL, and measured as one: an ordinary arithmetic property value never needed either evaluator, so it passes with the fix neutralised. It marks where the defect stops",
		},
		{
			name:         "control_ordinary_set_map_expression",
			builder:      "buildMapEvalFn",
			write:        `MATCH (a:P) SET a += {k: a.sid, c: a.sid - ` + fmt.Sprint(wpsBaseSID) + `}`,
			probe:        probeP,
			oracle:       `MATCH (a:P) RETURN a.sid AS k, a.sid - ` + fmt.Sprint(wpsBaseSID) + ` AS c`,
			wantRowCount: all,
			control:      true,
			why:          "the same control on the SET-map builder",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// kind is carried into every failure message so a red case says
			// whether it is a gate on the fix or a control that marks the
			// defect's boundary — the two mean opposite things when one fails.
			kind := "GATE"
			if c.control {
				kind = "CONTROL"
			}
			we := newWPCFixture(t)
			if c.setup != "" {
				if rows, errText := wpsDrain(t, we, c.setup); errText != "" {
					t.Fatalf("write-arm setup failed: %s (rows %v)", errText, rows)
				}
			}
			if _, errText := wpsDrain(t, we, c.write); errText != "" {
				t.Fatalf("[%s %s] write statement failed (%s): %s\n  %s", kind, c.builder, c.why, errText, c.write)
			}
			got, errText := wpsDrain(t, we, c.probe)
			if errText != "" {
				t.Fatalf("read-back failed: %s\n  %s", errText, c.probe)
			}

			oracleQ := c.oracle
			if oracleQ == "" {
				oracleQ = wpcOracle
			}
			re := newWPCFixture(t)
			if c.setup != "" {
				if rows, oErr := wpsDrain(t, re, c.setup); oErr != "" {
					t.Fatalf("oracle-arm setup failed: %s (rows %v)", oErr, rows)
				}
			}
			want, oErr := wpsDrain(t, re, oracleQ)
			if oErr != "" {
				t.Fatalf("read-only oracle failed: %s\n  %s", oErr, oracleQ)
			}

			if len(want) != c.wantRowCount {
				t.Fatalf("oracle produced %d rows, want %d (%s)\n got: %v",
					len(want), c.wantRowCount, c.why, want)
			}
			if !slices.Equal(got, want) {
				t.Errorf("[%s %s] the written value disagrees with the read-only oracle (%s)\n written: %v\n  oracle: %v\n   write: %s",
					kind, c.builder, c.why, got, want, c.write)
			}
		})
	}
}

// TestWritePathCountInPropertyMaps_SelectiveAnswer proves the fixture is
// discriminating rather than uniform. If the counted answer were the same for
// every row, a build that evaluated the subquery but lost the row correlation
// would satisfy every case in this file. It is asserted, not assumed.
func TestWritePathCountInPropertyMaps_SelectiveAnswer(t *testing.T) {
	e := newWPCFixture(t)
	got, errText := wpsDrain(t, e, wpcOracle)
	if errText != "" {
		t.Fatalf("oracle failed: %s", errText)
	}
	// wpsDrain sorts the rendered rows, so "c=0 …" sorts before "c=1 …".
	want := []string{
		"c=0 k=700002",
		"c=0 k=700003",
		"c=1 k=700000",
		"c=1 k=700001",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the fixture is not selective, so every gated case is weaker than it claims\n got: %v\nwant: %v", got, want)
	}
}

// TestWritePathCountInPropertyMaps_ExplicitTx pins the same placements on the
// OTHER write entry point. Both reach the four builders through
// buildPlanWithMutatorFull, but by different callers — RunInTx commits per
// statement, an explicit transaction's Exec does not — and only the autocommit
// one is exercised above.
func TestWritePathCountInPropertyMaps_ExplicitTx(t *testing.T) {
	ctx := context.Background()
	e := newWPCFixture(t)

	tx, err := e.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmts := []string{
		`MATCH (a:P) CREATE (:Q {k: a.sid, c: COUNT { MATCH (a)-[:Z]->(:P) }})`,
		`MATCH (a:P) MERGE (r:R {k: a.sid}) ON CREATE SET r.c = COUNT { MATCH (a)-[:Z]->(:P) }`,
		`MATCH (a:P) SET a += {c: COUNT { MATCH (a)-[:Z]->(:P) }}`,
		`MATCH (a:P) MERGE (s:S {k: a.sid}) ON CREATE SET s += {c: COUNT { MATCH (a)-[:Z]->(:P) }}`,
	}
	for _, q := range stmts {
		res, execErr := tx.ExecAny(q, nil)
		if execErr != nil {
			t.Fatalf("Exec %q: %v", q, execErr)
		}
		for res.Next() {
		}
		if iterErr := res.Err(); iterErr != nil {
			t.Fatalf("explicit-tx statement failed: %v\n  %s", iterErr, q)
		}
		_ = res.Close()
	}

	want := []string{"c=0 k=700002", "c=0 k=700003", "c=1 k=700000", "c=1 k=700001"}
	for _, probe := range []string{
		`MATCH (q:Q) RETURN q.k AS k, q.c AS c`,
		`MATCH (r:R) RETURN r.k AS k, r.c AS c`,
		`MATCH (s:S) RETURN s.k AS k, s.c AS c`,
		`MATCH (a:P) RETURN a.sid AS k, a.c AS c`,
	} {
		res, execErr := tx.ExecAny(probe, nil)
		if execErr != nil {
			t.Fatalf("Exec probe %q: %v", probe, execErr)
		}
		var rows []string
		for res.Next() {
			rows = append(rows, wpsRender(res.Record()))
		}
		if iterErr := res.Err(); iterErr != nil {
			t.Fatalf("probe failed: %v\n  %s", iterErr, probe)
		}
		_ = res.Close()
		slices.Sort(rows)
		if !slices.Equal(rows, want) {
			t.Errorf("explicit-tx write path\n probe: %s\n   got: %v\n  want: %v", probe, rows, want)
		}
	}
}

// TestWritePathCountInPropertyMaps_NullContract pins the null behaviour of the
// four routed builders, which is NOT uniform across them and which the route
// through evalRow must leave exactly as it was:
//
//   - CREATE omits a null-valued key (assigning null to a property is a no-op);
//   - MERGE raises SemanticError.MergeReadOwnWrites rather than omitting it,
//     because the same map is the search predicate;
//   - SET += REMOVES the key rather than omitting it.
//
// These are the contracts a route that "compiles but changes null propagation"
// would silently move, so they are asserted on the changed path rather than
// assumed to be covered elsewhere. Every statement below carries a NON-LITERAL
// null (a missing property access), which is what forces the per-row evaluator
// the change touches — a literal null would be resolved at build time and would
// prove nothing about this fix.
func TestWritePathCountInPropertyMaps_NullContract(t *testing.T) {
	t.Run("create_omits_null_key", func(t *testing.T) {
		e := newWPCFixture(t)
		if _, errText := wpsDrain(t, e, `MATCH (a:P) CREATE (:Q {k: a.sid, c: a.absent})`); errText != "" {
			t.Fatalf("CREATE with a null-valued property failed: %s", errText)
		}
		got, errText := wpsDrain(t, e, `MATCH (q:Q) RETURN q.k AS k, q.c AS c`)
		if errText != "" {
			t.Fatalf("read-back: %s", errText)
		}
		want := []string{"c=null k=700000", "c=null k=700001", "c=null k=700002", "c=null k=700003"}
		if !slices.Equal(got, want) {
			t.Errorf("CREATE null-key contract moved\n got: %v\nwant: %v", got, want)
		}
	})

	t.Run("merge_raises_on_null_key", func(t *testing.T) {
		e := newWPCFixture(t)
		_, errText := wpsDrain(t, e, `MATCH (a:P) MERGE (q:Q {k: a.sid, c: a.absent})`)
		const want = "SemanticError.MergeReadOwnWrites"
		if errText == "" {
			t.Fatalf("MERGE on a runtime-null property was accepted; want %s", want)
		}
		if !strings.Contains(errText, want) {
			t.Errorf("MERGE null-key error type moved\n got: %s\nwant it to name: %s", errText, want)
		}
	})

	t.Run("set_map_removes_null_key", func(t *testing.T) {
		e := newWPCFixture(t)
		if _, errText := wpsDrain(t, e, `MATCH (a:P) SET a += {c: 7}`); errText != "" {
			t.Fatalf("seeding c failed: %s", errText)
		}
		if _, errText := wpsDrain(t, e, `MATCH (a:P) SET a += {c: a.absent}`); errText != "" {
			t.Fatalf("SET += with a null-valued key failed: %s", errText)
		}
		got, errText := wpsDrain(t, e, `MATCH (a:P) RETURN a.sid AS k, a.c AS c`)
		if errText != "" {
			t.Fatalf("read-back: %s", errText)
		}
		want := []string{"c=null k=700000", "c=null k=700001", "c=null k=700002", "c=null k=700003"}
		if !slices.Equal(got, want) {
			t.Errorf("SET-map null-key removal contract moved\n got: %v\nwant: %v", got, want)
		}
	})

	t.Run("merge_action_null_rhs_is_noop", func(t *testing.T) {
		e := newWPCFixture(t)
		if _, errText := wpsDrain(t, e, `MATCH (a:P) MERGE (q:Q {k: a.sid}) ON CREATE SET q.c = a.absent`); errText != "" {
			t.Fatalf("ON CREATE SET with a null RHS failed: %s", errText)
		}
		got, errText := wpsDrain(t, e, `MATCH (q:Q) RETURN q.k AS k, q.c AS c`)
		if errText != "" {
			t.Fatalf("read-back: %s", errText)
		}
		want := []string{"c=null k=700000", "c=null k=700001", "c=null k=700002", "c=null k=700003"}
		if !slices.Equal(got, want) {
			t.Errorf("MERGE-action null-RHS contract moved\n got: %v\nwant: %v", got, want)
		}
	})
}
