package cypher

// index_nested_loop_exact_key_test.go — the regression gate for rmp #2263: the
// index nested-loop join must not emit a row whose join keys are unequal under
// openCypher value equality.
//
// # The defect
//
// The numeric btree companion the operator seeks is keyed on float64, and a node
// whose property is an int64 of magnitude above 2^53 is filed under the float64
// that int64 ROUNDS to. Two distinct integers therefore share one index key:
// 2^53 and 2^53+1 both file under 9007199254740992.0. A seek for 2^53 yields both
// nodes, and the operator emitted both without ever comparing the node's ACTUAL
// value against the outer key — a phantom row that no downstream predicate
// removes.
//
// [exec.exactFloat64Key] guards the OUTER key against exactly this rounding, and
// that guard was mistaken for the whole story. It is not: it makes the seek key
// faithful to the outer value, and says nothing about whether the index ENTRIES
// it lands on are faithful to theirs. The two are independent, and only the
// second produces this phantom.
//
// # Why the oracle is literal
//
// The fixture assigns every node a DISTINCT age, so the answer to every key below
// is one hand-countable set of node ids, written out verbatim. A differential
// against the hash join or the nested loop would not have caught this: the
// project has twice lost a real defect to a differential whose arms shared the
// broken code, and here the arms share [expr.Value.Equal] itself. The literal
// oracle is the only arm that is independent of the engine.

import (
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// twoPow53 is the largest magnitude an int64 carries that float64 represents
// exactly. It is the boundary the whole defect lives on.
const twoPow53 = int64(1) << 53

// inljExactFixtureSize is the node count. It must clear
// [indexNestedLoopMinPopulation] (64) or the planner declines the seek and the
// test would silently assert against the hash join instead.
const inljExactFixtureSize = 100

// inljExactAge returns the age node i carries in the exact-key fixture. It is the
// ORACLE's view of the graph — derived from the fixture rule, never read back
// through the engine — and every value it returns is DISTINCT, so a key matches
// at most one node and the expected id set can be written down by eye.
//
// The four highest ids straddle both float64 boundaries:
//
//	96  −(2^53+1)  not float64-representable; rounds onto −2^53
//	97  −2^53      exactly representable
//	98   2^53      exactly representable
//	99   2^53+1    not float64-representable; rounds onto 2^53
//
// so ids 97/99 and 96/98... — precisely, 97 and 96 share one float64 key and 98
// and 99 share another. Everything below carries its own id as its age, with
// every 7th ≡ 3 stored as an integral FLOAT so the cross-type case openCypher
// requires (3 = 3.0) is exercised on the same fixture.
func inljExactAge(i int) any {
	switch i {
	case inljExactFixtureSize - 4:
		return -twoPow53 - 1
	case inljExactFixtureSize - 3:
		return -twoPow53
	case inljExactFixtureSize - 2:
		return twoPow53
	case inljExactFixtureSize - 1:
		return twoPow53 + 1
	}
	if i%7 == 3 {
		return float64(i)
	}
	return int64(i)
}

// inljExactFixtureGraph builds the fixture described on [inljExactAge]. Every
// node carries a numeric age, which is the planner's admission condition
// (numericIndexCoversScan): a gap or a string would make the planner DECLINE the
// seek and the test would prove nothing.
func inljExactFixtureGraph(t *testing.T) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	g.SetIndexManager(index.NewManager())
	for i := 0; i < inljExactFixtureSize; i++ {
		key := fmt.Sprintf("n%d", i)
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(key, "P"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeProperty(key, "id", lpg.Int64Value(int64(i))); err != nil {
			t.Fatalf("SetNodeProperty(id): %v", err)
		}
		var err error
		switch age := inljExactAge(i).(type) {
		case int64:
			err = g.SetNodeProperty(key, "age", lpg.Int64Value(age))
		case float64:
			err = g.SetNodeProperty(key, "age", lpg.Float64Value(age))
		default:
			t.Fatalf("fixture age for node %d has unexpected type %T", i, age)
		}
		if err != nil {
			t.Fatalf("SetNodeProperty(age): %v", err)
		}
	}
	return g
}

// TestIndexNestedLoopJoin_ExactKeyReverification is rmp #2263's gate.
//
// Each case names ONE outer key and the LITERAL set of node ids that key must
// match. The seek must have fired (asserted from indexNestedLoopBuildCount, per
// #2233's acceptance criterion 4) or the case is measuring the wrong operator.
func TestIndexNestedLoopJoin_ExactKeyReverification(t *testing.T) {
	const q = `UNWIND $rows AS r MATCH (b:P) WHERE b.age = r.a RETURN b.id AS bid`

	cases := []struct {
		name string
		key  any
		want []int64 // the node ids the key matches, in ascending order
	}{
		// ── The defect (acceptance criterion 1) ──
		//
		// 2^53 and 2^53+1 are distinct integers that share one float64 index key.
		// Node 99 holds 2^53+1, which is NOT equal to 2^53 under openCypher, so it
		// must not be emitted. Before the fix both ids came back.
		{"integer key at +2^53 admits only its own node", twoPow53, []int64{98}},
		{"integer key at -2^53 admits only its own node", -twoPow53, []int64{97}},
		// A FLOAT key at the boundary reaches the seek too — a float is always an
		// exact float64 key — so it collides identically and is a second, distinct
		// route into the same defect.
		{"float key at +2^53 admits only the equal node", float64(twoPow53), []int64{98}},
		{"float key at -2^53 admits only the equal node", float64(-twoPow53), []int64{97}},

		// ── The oversized keys themselves (control) ──
		//
		// These take the operator's FALLBACK path, which has always compared exactly.
		// They are here so a fix that merely widened the fallback — rather than
		// re-verifying the seek's own candidates — is still measured.
		{"oversized positive integer key matches its own node", twoPow53 + 1, []int64{99}},
		{"oversized negative integer key matches its own node", -twoPow53 - 1, []int64{96}},
		{"oversized integer key held by no node matches nothing", twoPow53 + 3, nil},

		// ── Not an over-correction (acceptance criterion 2) ──
		//
		// Every ordinary key below 2^53 must still match, which is what proves the
		// re-verification did not simply reject the candidates it was given.
		{"small integer key still matches", int64(5), []int64{5}},
		{"integer key just below the boundary matches nothing here", twoPow53 - 1, nil},
		{"absent integer key matches nothing", int64(9999), nil},

		// ── Integer/float equivalence is preserved (acceptance criterion 3) ──
		//
		// openCypher requires 1 = 1.0 across the numeric types, which is the whole
		// reason the index buckets on float64. Node 3 holds the FLOAT 3.0 and node 5
		// the INTEGER 5, so these two cases cross the type boundary in both
		// directions.
		{"integer key matches a float-valued node", int64(3), []int64{3}},
		{"float key matches an integer-valued node", float64(5), []int64{5}},
		{"float key matches a float-valued node", float64(3), []int64{3}},
		{"float key with a fractional part matches nothing", 3.5, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := NewEngine(inljExactFixtureGraph(t))
			mustCreateIndex(t, eng)

			before := indexNestedLoopBuildCount.Load()
			got := inljRun(t, eng, q, map[string]any{"rows": inljKeyRows([]any{tc.key})})
			if indexNestedLoopBuildCount.Load() == before {
				t.Fatal("the index nested-loop join did not fire, so this case is not " +
					"exercising the operator under test")
			}

			want := make([]string, 0, len(tc.want))
			for _, id := range tc.want {
				want = append(want, fmt.Sprintf("%v\x1f", id))
			}
			if len(got) != len(want) {
				t.Fatalf("key %v (%T) matched %d rows, the hand-counted oracle says %d\n"+
					"  got  %q\n  want %q\n"+
					"A row here whose keys are not equal under openCypher is the phantom "+
					"rmp #2263 describes: the float64 index key is lossy above 2^53, so the "+
					"seek's candidates must be re-verified against their ACTUAL values.",
					tc.key, tc.key, len(got), len(want), got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("key %v (%T) row %d: got %q, oracle says %q",
						tc.key, tc.key, i, got[i], want[i])
				}
			}
		})
	}
}

// TestIndexNestedLoopJoin_ExactKeyBatchAgreesWithPerKeyRuns runs every key of the
// matrix above in ONE batch and checks the result against the concatenation of
// the per-key answers.
//
// It is not a restatement of the per-key test. The operator carries per-row state
// — the seek buffer, the fallback's inner arm, the reused output row — across
// outer rows, so a re-verification that leaked state between rows (a stale outer
// key, a scratch row aliased into the emitted one) would pass every single-key
// case and fail here.
func TestIndexNestedLoopJoin_ExactKeyBatchAgreesWithPerKeyRuns(t *testing.T) {
	const q = `UNWIND $rows AS r MATCH (b:P) WHERE b.age = r.a RETURN b.id AS bid`

	// Interleaved deliberately: a seek row, then a fallback row, then a seek row
	// again, so the fallback's inner-arm drive runs BETWEEN two seeks.
	keys := []any{
		twoPow53,      // seek, collides with node 99
		twoPow53 + 1,  // fallback
		int64(3),      // seek, cross-type against a float-valued node
		-twoPow53 - 1, // fallback, negative side
		float64(-twoPow53),
		int64(5),
		int64(9999),
	}
	// Hand-counted, in emission order: the join is outer-major, so the ids appear
	// in the keys' own order, and each key matches at most one node.
	want := []int64{98, 99, 3, 96, 97, 5}

	eng := NewEngine(inljExactFixtureGraph(t))
	mustCreateIndex(t, eng)

	before := indexNestedLoopBuildCount.Load()
	got := inljRun(t, eng, q, map[string]any{"rows": inljKeyRows(keys)})
	if indexNestedLoopBuildCount.Load() == before {
		t.Fatal("the index nested-loop join did not fire")
	}

	if len(got) != len(want) {
		t.Fatalf("batch matched %d rows, the hand-counted oracle says %d\n  got %q",
			len(got), len(want), got)
	}
	for i, id := range want {
		if w := fmt.Sprintf("%v\x1f", id); got[i] != w {
			t.Fatalf("batch row %d: got %q, oracle says %q (full: %q)", i, got[i], w, got)
		}
	}
}

// TestIndexNestedLoopJoin_ExactKeyMatchesReferencePlans is the differential that
// sits UNDER the literal oracle above, not in place of it.
//
// Its job is narrower than the oracle's: it proves the re-verification did not
// change the emitted SEQUENCE relative to the two plans this operator may be
// substituted for. The oracle proves the rows are right; this proves the order
// and the multiplicity still match the plan the query would otherwise have run.
func TestIndexNestedLoopJoin_ExactKeyMatchesReferencePlans(t *testing.T) {
	const q = `UNWIND $rows AS r MATCH (b:P) WHERE b.age = r.a RETURN r.a AS k, b.id AS bid`

	keys := []any{
		twoPow53, twoPow53 + 1, -twoPow53, -twoPow53 - 1,
		float64(twoPow53), float64(-twoPow53),
		int64(3), float64(5), int64(9999), 3.5,
	}
	params := map[string]any{"rows": inljKeyRows(keys)}

	eng := NewEngine(inljExactFixtureGraph(t))
	mustCreateIndex(t, eng)
	before := indexNestedLoopBuildCount.Load()
	gotSeek := inljRun(t, eng, q, params)
	if indexNestedLoopBuildCount.Load() == before {
		t.Fatal("the index nested-loop join did not fire")
	}

	hjEng := NewEngine(inljExactFixtureGraph(t))
	mustCreateIndex(t, hjEng)
	gotHash := inljRunWithoutINLJ(t, hjEng, q, params)

	nlEng := NewEngineWithOptions(inljExactFixtureGraph(t), EngineOptions{DisableHashJoin: true})
	mustCreateIndex(t, nlEng)
	gotNested := inljRun(t, nlEng, q, params)

	assertSameSequence(t, "index nested-loop join", gotSeek, "hash join", gotHash)
	assertSameSequence(t, "index nested-loop join", gotSeek, "nested loop", gotNested)
}
