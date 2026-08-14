package sim

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// entityFixture is the standard fixture for the entity probes: five Persons
// over three cities plus a KNOWS shape chosen so every probe family is
// non-degenerate — a two-hop chain (s1→s2→s3) so paths of length 2 and 3 exist,
// a 2-CYCLE (s3⇄s4) so a path may legitimately return to a node it already
// visited, and a SELF-LOOP (s5→s5) so the trail reference's
// relationship-uniqueness rule is observable rather than vacuous.
func entityFixture(t *testing.T) (*EngineAdapter, *GraphOracle) {
	t.Helper()
	return seedCityFixture(t,
		[]cityPerson{
			{"s1", 40, "c0"},
			{"s2", 30, "c1"},
			{"s3", 20, "c2"},
			{"s4", 10, "c0"},
			{"s5", 50, "c1"},
		},
		[][2]string{{"s1", "s2"}, {"s2", "s3"}, {"s3", "s4"}, {"s4", "s3"}, {"s5", "s5"}},
	)
}

// TestSurfaceEntity_PassAndCatch is the sensitivity proof of rmp #2458: the
// entity battery is clean on a consistent fixture, and EACH probe family fires
// when its own oracle reference is perturbed — a wrong label set, a wrong
// property map, a transposed edge endpoint, and a path count the model no
// longer supports. Each family is driven individually (not through
// [CheckCypherSurfaceEntity]) so a perturbation that happens to trip a
// neighbouring probe cannot be mistaken for the family under test firing.
func TestSurfaceEntity_PassAndCatch(t *testing.T) {
	ctx := context.Background()

	t.Run("baseline clean", func(t *testing.T) {
		a, o := entityFixture(t)
		if v := CheckCypherSurfaceEntity(0, o, a); len(v) > 0 {
			t.Fatalf("consistent fixture should be clean, got: %v", v)
		}
	})

	t.Run("wrong label set fires labels(n)", func(t *testing.T) {
		a, o := entityFixture(t)
		if v := checkEntityLabels(ctx, 0, o, a); len(v) > 0 {
			t.Fatalf("baseline labels probe should be clean, got: %v", v)
		}
		o.nodes[o.byName["s3"]].Labels = []string{"Person", "Ghost"}
		if v := checkEntityLabels(ctx, 0, o, a); len(v) == 0 {
			t.Fatal("labels(n) FAILED to detect a label the engine never stored")
		}
	})

	t.Run("dropped label fires labels(n)", func(t *testing.T) {
		a, o := entityFixture(t)
		// The engine holds :Person; a model that forgot it must not pass.
		o.nodes[o.byName["s2"]].Labels = nil
		if v := checkEntityLabels(ctx, 0, o, a); len(v) == 0 {
			t.Fatal("labels(n) FAILED to detect a modelled node with no labels")
		}
	})

	t.Run("wrong property value fires properties(n)", func(t *testing.T) {
		a, o := entityFixture(t)
		if v := checkEntityProperties(ctx, 0, o, a); len(v) > 0 {
			t.Fatalf("baseline properties probe should be clean, got: %v", v)
		}
		o.nodes[o.byName["s1"]].Properties["age"] = int64(41)
		if v := checkEntityProperties(ctx, 0, o, a); len(v) == 0 {
			t.Fatal("properties(n) FAILED to detect a perturbed property VALUE")
		}
	})

	t.Run("extra property key fires properties(n)", func(t *testing.T) {
		a, o := entityFixture(t)
		o.nodes[o.byName["s1"]].Properties["nickname"] = "ghost"
		if v := checkEntityProperties(ctx, 0, o, a); len(v) == 0 {
			t.Fatal("properties(n) FAILED to detect a modelled key the engine lacks")
		}
	})

	t.Run("transposed endpoint fires startNode/endNode", func(t *testing.T) {
		a, o := entityFixture(t)
		if v := checkEntityEdges(ctx, 0, o, a); len(v) > 0 {
			t.Fatalf("baseline edge probe should be clean, got: %v", v)
		}
		// Rewrite s1->s2 as s2->s1 in the model only: the row count is
		// unchanged, so only an endpoint-aware probe can see the difference.
		src, dst := o.byName["s1"], o.byName["s2"]
		e := o.edges[edgeKey{src: src, dst: dst, label: "KNOWS"}]
		if e == nil {
			t.Fatal("fixture is missing the s1->s2 edge")
		}
		delete(o.edges, edgeKey{src: src, dst: dst, label: "KNOWS"})
		o.edges[edgeKey{src: dst, dst: src, label: "KNOWS"}] = &EdgeState{
			SrcID: dst, DstID: src, Label: "KNOWS", Properties: map[string]any{},
		}
		if v := checkEntityEdges(ctx, 0, o, a); len(v) == 0 {
			t.Fatal("startNode/endNode FAILED to detect a transposed edge endpoint")
		}
	})

	t.Run("comprehension route probe is driven and sensitive", func(t *testing.T) {
		a, o := entityFixture(t)
		if v := checkEntityCompRoutes(ctx, 0, o, a); len(v) > 0 {
			t.Fatalf("baseline comprehension-route probe should be clean, got: %v", v)
		}
		// Prove the probe actually RAN rather than short-circuiting on an
		// incomplete model: every arm must produce rows, so a reference the
		// engine cannot satisfy has to fire. Transposing s1->s2 in the model
		// leaves the row COUNT untouched, so only a probe that compares the
		// bindings — through both consumers — can see it.
		src, dst := o.byName["s1"], o.byName["s2"]
		if o.edges[edgeKey{src: src, dst: dst, label: "KNOWS"}] == nil {
			t.Fatal("fixture is missing the s1->s2 edge")
		}
		delete(o.edges, edgeKey{src: src, dst: dst, label: "KNOWS"})
		o.edges[edgeKey{src: dst, dst: src, label: "KNOWS"}] = &EdgeState{
			SrcID: dst, DstID: src, Label: "KNOWS", Properties: map[string]any{},
		}
		if v := checkEntityCompRoutes(ctx, 0, o, a); len(v) == 0 {
			t.Fatal("comprehension route probe FAILED to detect a transposed edge endpoint")
		}
	})

	t.Run("each comprehension route arm produces rows", func(t *testing.T) {
		// An arm whose query returned nothing would compare empty against empty
		// and pass for ever. Assert each of the six queries is non-empty and that
		// the two routes of one direction return the SAME number of rows, so a
		// route that silently enumerates nothing cannot hide behind the oracle
		// comparison above.
		a, o := entityFixture(t)
		pairs, complete := knowsEndpointNames(o)
		if !complete || len(pairs) == 0 {
			t.Fatal("fixture produced no reference edges")
		}
		for _, arm := range []struct{ anchor, pattern string }{
			{"a", "(a)-[r:KNOWS]->(b:Person)"},
			{"b", "(b)<-[r:KNOWS]-(a:Person)"},
			{"a", "(a)-[r:KNOWS]-(b:Person)"},
		} {
			counts := map[string]int{}
			for _, rt := range entityCompRoutes(arm.anchor, arm.pattern) {
				n := 0
				if err := forEachRow(ctx, a, rt.query, func(func(int) expr.Value) error {
					n++
					return nil
				}); err != nil {
					t.Fatalf("%s %s: %v", arm.pattern, rt.route, err)
				}
				if n == 0 {
					t.Fatalf("%s %s route returned NO rows — the probe would be vacuous", arm.pattern, rt.route)
				}
				counts[rt.route] = n
			}
			if counts["hoisted"] != counts["fallback"] {
				t.Fatalf("%s: hoisted=%d rows, fallback=%d rows", arm.pattern, counts["hoisted"], counts["fallback"])
			}
		}
	})

	t.Run("transposed reference fires the forward arm", func(t *testing.T) {
		a, o := entityFixture(t)
		want, complete := knowsEndpointNames(o)
		if !complete || len(want) == 0 {
			t.Fatal("fixture produced no reference edges")
		}
		eids := knowsEIDByEndpointNames(o)
		rows := entityEdgeRows(want, false)
		if v := compareEntityEdges(ctx, 0, "forward", entityEdgeForwardQuery, rows, eids, a); len(v) > 0 {
			t.Fatalf("forward arm should be clean, got: %v", v)
		}
		// Transposing every reference pair must fire: the fixture contains the
		// 2-cycle s3⇄s4, so a probe that compared endpoints only as an unordered
		// pair would still pass on those two rows.
		transposed := make([][2]string, len(want))
		for i, w := range want {
			transposed[i] = [2]string{w[1], w[0]}
		}
		if v := compareEntityEdges(ctx, 0, "forward", entityEdgeForwardQuery,
			entityEdgeRows(transposed, false), eids, a); len(v) == 0 {
			t.Fatal("forward arm did NOT fire against a fully transposed reference")
		}
	})

	t.Run("transposed reference fires the reverse and undirected arms", func(t *testing.T) {
		// The reverse and undirected arms exist because of rmp #2504, where the
		// forward read of a reciprocal pair was correct and the other two were
		// not. The fixture's 2-cycle s3⇄s4 IS such a pair, so a transposed
		// reference must fire on each arm — otherwise the arm is decoration.
		a, o := entityFixture(t)
		want, complete := knowsEndpointNames(o)
		if !complete || len(want) == 0 {
			t.Fatal("fixture produced no reference edges")
		}
		eids := knowsEIDByEndpointNames(o)
		transposed := make([][2]string, len(want))
		for i, w := range want {
			transposed[i] = [2]string{w[1], w[0]}
		}
		if v := compareEntityEdges(ctx, 0, "reverse", entityEdgeReverseQuery,
			entityEdgeRows(want, false), eids, a); len(v) > 0 {
			t.Fatalf("reverse arm should be clean, got: %v", v)
		}
		if v := compareEntityEdges(ctx, 0, "reverse", entityEdgeReverseQuery,
			entityEdgeRows(transposed, false), eids, a); len(v) == 0 {
			t.Fatal("reverse arm did NOT fire against a fully transposed reference")
		}
		if v := compareEntityEdges(ctx, 0, "undirected", entityEdgeUndirectedQuery,
			entityEdgeRows(want, true), eids, a); len(v) > 0 {
			t.Fatalf("undirected arm should be clean, got: %v", v)
		}
		if v := compareEntityEdges(ctx, 0, "undirected", entityEdgeUndirectedQuery,
			entityEdgeRows(transposed, true), eids, a); len(v) == 0 {
			t.Fatal("undirected arm did NOT fire against a fully transposed reference")
		}
	})

	t.Run("extra oracle edge fires the path histogram", func(t *testing.T) {
		a, o := entityFixture(t)
		if v := checkEntityPaths(ctx, 0, o, a); len(v) > 0 {
			t.Fatalf("baseline path probe should be clean, got: %v", v)
		}
		// A KNOWS edge the engine never stored changes the number of paths of
		// length 1, 2 and 3 that the reference expects.
		o.edges[edgeKey{src: o.byName["s1"], dst: o.byName["s5"], label: "KNOWS"}] = &EdgeState{
			SrcID: o.byName["s1"], DstID: o.byName["s5"], Label: "KNOWS", Properties: map[string]any{},
		}
		if v := checkEntityPaths(ctx, 0, o, a); len(v) == 0 {
			t.Fatal("path probe FAILED to detect a path the model expects and the engine cannot produce")
		}
	})

	t.Run("removed oracle edge fires the path histogram", func(t *testing.T) {
		a, o := entityFixture(t)
		delete(o.edges, edgeKey{src: o.byName["s2"], dst: o.byName["s3"], label: "KNOWS"})
		if v := checkEntityPaths(ctx, 0, o, a); len(v) == 0 {
			t.Fatal("path probe FAILED to detect paths the engine returns and the model does not expect")
		}
	})

	t.Run("perturbed entity count fires elementId", func(t *testing.T) {
		a, o := entityFixture(t)
		if v := checkEntityElementID(ctx, 0, o, a); len(v) > 0 {
			t.Fatalf("baseline elementId probe should be clean, got: %v", v)
		}
		o.ApplyCreate(tmplCreatePersonCity, map[string]any{"name": "ghost", "age": int64(1), "city": "c9"})
		if v := checkEntityElementID(ctx, 0, o, a); len(v) == 0 {
			t.Fatal("elementId probe FAILED to detect a Person the engine never stored")
		}
	})
}

// TestEntityPaths_HappyPathHandChecked pins the path probe's own arithmetic on
// the hand-enumerated fixture, so the probe is known to have SEEN the paths it
// claims to check rather than passing on an empty result. Over the fixture's
// KNOWS shape (s1→s2→s3, s3⇄s4, s5→s5) the trails of length 1..3 are, by hand:
//
//	len 1: s1→s2, s2→s3, s3→s4, s4→s3, s5→s5              (5)
//	len 2: s1→s2→s3, s2→s3→s4, s3→s4→s3, s4→s3→s4          (4)
//	len 3: s1→s2→s3→s4, s2→s3→s4→s3, s3→s4→s3 ends (edge   (2)
//	       s3→s4 already used), s4→s3→s4 likewise ends;
//	       so only s1→…  and s2→… continue
//
// Relationship uniqueness is what stops the 2-cycle and the self-loop from
// generating unbounded paths, and it is why s5 contributes exactly ONE trail.
func TestEntityPaths_HappyPathHandChecked(t *testing.T) {
	_, o := entityFixture(t)
	counts, ok := knowsTrailCounts(o, knowsVLEMaxLen, entityTrailLimit)
	if !ok {
		t.Fatal("trail enumeration reported the fixture as over the limit")
	}
	byLen := map[int]int{}
	for k, c := range counts {
		byLen[k.Len] += c
	}
	for _, tc := range []struct{ length, want int }{{1, 5}, {2, 4}, {3, 2}} {
		if byLen[tc.length] != tc.want {
			t.Errorf("trails of length %d = %d, hand-computed %d (all: %v)", tc.length, byLen[tc.length], tc.want, byLen)
		}
	}
	// The self-loop must contribute exactly one trail: a walk enumeration would
	// give it three (s5→s5, s5→s5→s5, s5→s5→s5→s5).
	if got := counts[trailKey{Anchor: "s5", Len: 1}]; got != 1 {
		t.Errorf("self-loop anchor s5 has %d trails of length 1, want 1", got)
	}
	for _, l := range []int{2, 3} {
		if got := counts[trailKey{Anchor: "s5", Len: l}]; got != 0 {
			t.Errorf("self-loop anchor s5 has %d trails of length %d, want 0 (an edge may not repeat)", got, l)
		}
	}
	// The 2-cycle must yield a length-2 trail returning to its start: node
	// repetition is allowed, only relationship repetition is not.
	if got := counts[trailKey{Anchor: "s3", Len: 2}]; got != 1 {
		t.Errorf("2-cycle anchor s3 has %d trails of length 2, want 1 (s3→s4→s3)", got)
	}
}

// TestEntityTrailCounts_LimitRespected proves the enumeration bound is a real
// gate rather than decoration: with a limit of one, the fixture's five
// single-hop trails alone must exceed it and the reference must decline.
func TestEntityTrailCounts_LimitRespected(t *testing.T) {
	_, o := entityFixture(t)
	if _, ok := knowsTrailCounts(o, knowsVLEMaxLen, 1); ok {
		t.Fatal("trail enumeration accepted a fixture that exceeds the limit")
	}
	if _, ok := knowsTrailCounts(o, knowsVLEMaxLen, entityTrailLimit); !ok {
		t.Fatal("trail enumeration declined a fixture well inside the limit")
	}
}

// TestEntityIDViolations_EachContractFires drives the PURE elementId comparison
// with hand-built observations, one per contract, so every failure mode is
// proven to fire without needing an engine that misbehaves. The engine-side
// contract these encode was read from cypher/funcs/completeness.go (fnElementID
// returns strconv.FormatInt of the same integer id() returns, for nodes AND
// relationships — no database or generation prefix) and confirmed empirically.
func TestEntityIDViolations_EachContractFires(t *testing.T) {
	good := []entityID{
		{Key: "s1", ElementID: "140", ID: 140},
		{Key: "s2", ElementID: "165", ID: 165},
	}
	clone := func(in []entityID) []entityID { return append([]entityID(nil), in...) }

	if v := entityIDViolations(0, "elementId(n)", clone(good), clone(good), len(good)); len(v) > 0 {
		t.Fatalf("a consistent id observation should be clean, got: %v", v)
	}

	t.Run("unstable across two reads", func(t *testing.T) {
		second := clone(good)
		second[1].ElementID = "999"
		second[1].ID = 999
		if v := entityIDViolations(0, "elementId(n)", clone(good), second, len(good)); len(v) == 0 {
			t.Fatal("an elementId that changed between two reads did NOT fire")
		}
	})

	t.Run("not distinct across entities", func(t *testing.T) {
		dup := clone(good)
		dup[1].ElementID = "140"
		dup[1].ID = 140
		if v := entityIDViolations(0, "elementId(n)", dup, clone(dup), len(dup)); len(v) == 0 {
			t.Fatal("two entities sharing one elementId did NOT fire")
		}
	})

	t.Run("elementId is not the decimal id", func(t *testing.T) {
		odd := clone(good)
		odd[0].ElementID = "4:abcd:140" // the Neo4j-style form this engine does NOT use
		if v := entityIDViolations(0, "elementId(n)", odd, clone(odd), len(odd)); len(v) == 0 {
			t.Fatal("an elementId that is not the decimal rendering of id() did NOT fire")
		}
	})

	t.Run("row count disagrees with the oracle", func(t *testing.T) {
		if v := entityIDViolations(0, "elementId(n)", clone(good), clone(good), len(good)+1); len(v) == 0 {
			t.Fatal("a row count below the modelled entity count did NOT fire")
		}
	})

	t.Run("two reads of different length", func(t *testing.T) {
		if v := entityIDViolations(0, "elementId(n)", clone(good), clone(good)[:1], len(good)); len(v) == 0 {
			t.Fatal("two reads returning different row counts did NOT fire")
		}
	})
}

// TestNonDeterministicFuncs_Invariants runs the rand/randomUUID/timestamp
// invariants against the real engine and proves the UUID shape assertion is not
// vacuous by checking the pattern rejects the near-misses a broken generator
// would produce.
func TestNonDeterministicFuncs_Invariants(t *testing.T) {
	eng := newTestEngine(t)
	a := NewEngineAdapter(eng)
	if v := CheckNonDeterministicFuncs(0, a); len(v) > 0 {
		t.Fatalf("non-deterministic invariants reported violations: %v", v)
	}

	// The v4 pattern must accept a genuine value and reject each near-miss.
	if !reUUIDv4.MatchString("0ae396a8-0e40-464e-a1f2-26c80d2225e9") {
		t.Error("the UUID pattern rejects a genuine version-4 UUID")
	}
	for _, bad := range []string{
		"",
		"0ae396a8-0e40-464e-a1f2-26c80d2225e",   // one digit short
		"0ae396a8-0e40-164e-a1f2-26c80d2225e9",  // version nibble 1, not 4
		"0ae396a8-0e40-464e-c1f2-26c80d2225e9",  // variant nibble c
		"0AE396A8-0E40-464E-A1F2-26C80D2225E9",  // upper case
		"0ae396a80e40464ea1f226c80d2225e9",      // unhyphenated
		"0ae396a8-0e40-464e-a1f2-26c80d2225e9x", // trailing junk
	} {
		if reUUIDv4.MatchString(bad) {
			t.Errorf("the UUID pattern accepted %q", bad)
		}
	}
}

// TestNonDeterministicFuncs_TimestampIsStatementFrozen pins the statement-clock
// contract the timestamp() invariant relies on: two calls in ONE statement
// return the same integer (cypher/stmt_now_reg.go overrides timestamp()
// alongside the five temporal `now` constructors), and the value is epoch
// MILLISECONDS — three orders of magnitude above a seconds-valued clock.
func TestNonDeterministicFuncs_TimestampIsStatementFrozen(t *testing.T) {
	eng := newTestEngine(t)
	a := NewEngineAdapter(eng)
	first, second, err := readTimestampPair(context.Background(), a)
	if err != nil {
		t.Fatalf("timestamp probe: %v", err)
	}
	if first != second {
		t.Fatalf("timestamp() is not statement-frozen: %d then %d in one statement", first, second)
	}
	if first < 1_000_000_000_000 || first >= 10_000_000_000_000 {
		t.Fatalf("timestamp() = %d is outside the epoch-millisecond window [1e12, 1e13)", first)
	}
	later, _, err := readTimestampPair(context.Background(), a)
	if err != nil {
		t.Fatalf("timestamp probe (second statement): %v", err)
	}
	if later < first {
		t.Fatalf("timestamp() went backwards across two statements: %d then %d", first, later)
	}
}
