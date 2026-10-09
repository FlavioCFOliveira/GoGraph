package cypher

// index_nested_loop_label_2955_test.go — guard for rmp #2955.
//
// The index nested-loop join emits its seek hits straight from the numeric
// btree companion's posting list, with no label check of its own. That is
// correct only because the planner admits the join solely over a companion
// BOUND to the inner scan's (label, property) — findBoundNumericBTree — and a
// bound companion files a node only while it carries that label. Re-validated
// at 25999b58: a node of another label sharing the indexed value, a node whose
// label was removed, a node whose label was added after CREATE INDEX, and the
// same label changes made earlier in the reading transaction all leave the
// seek's answer equal to the nested loop's, in memory and on the WAL-recovered
// engine. The guarantee is the binding, so this test pins it: it fails if a
// seek hit of another label is ever emitted.

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

const (
	q2955 = `UNWIND $rows AS r MATCH (b:P) WHERE b.age = r.a RETURN b.id AS bid, b:P AS p`
	// Every :P node and every :Q decoy carries the same ages, so a seek hit
	// that ignored the label would return a decoy.
	seed2955 = `UNWIND range(1, 200) AS i CREATE (:P {id: i, age: i}), (:Q {id: 1000 + i, age: i})`
	// Label changes after CREATE INDEX: node 3 loses :P, decoy 1004 gains it,
	// node 5 trades :P for :Q.
	relabel2955 = `MATCH (a:P {id: 3}), (b:Q {id: 1004}), (c:P {id: 5}) REMOVE a:P SET b:P SET c:Q REMOVE c:P`
)

var keys2955 = []any{int64(2), int64(3), int64(4), 5.0, int64(6)}

// want2955 is the hand-counted answer, sorted. The oracle is compared as a set:
// within one outer row the join emits ascending NodeIDs, and NodeIDs come from a
// process-wide counter, so the order of node 4 and node 1004 is not a property
// of this fixture. The nested-loop differential still compares the exact
// sequence.
var want2955 = []string{
	"1004\x1ftrue\x1f", "2\x1ftrue\x1f", "4\x1ftrue\x1f", "6\x1ftrue\x1f",
}

func sorted2955(rows []string) []string {
	out := slices.Clone(rows)
	slices.Sort(out)
	return out
}

func runAll2955(t *testing.T, eng *Engine, q string) {
	t.Helper()
	res, err := eng.RunAny(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("%q: %v", q, err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		t.Fatalf("%q: %v", q, err)
	}
	_ = res.Close()
}

// check2955 asserts the join fired, matches the hand-counted oracle and matches
// the nested loop.
func check2955(t *testing.T, eng *Engine) {
	t.Helper()
	eng.ClearPlanCache()
	before := indexNestedLoopBuildCount.Load()
	got := inljRun(t, eng, q2955, map[string]any{"rows": inljKeyRows(keys2955)})
	if indexNestedLoopBuildCount.Load() == before {
		t.Fatal("the index nested-loop join did not fire")
	}
	if !slices.Equal(sorted2955(got), want2955) {
		t.Fatalf("seek join:\n got %q\nwant %q", got, want2955)
	}
	if ref := inljRunWithoutINLJ(t, eng, q2955, map[string]any{"rows": inljKeyRows(keys2955)}); !slices.Equal(got, ref) {
		t.Fatalf("seek join disagrees with the nested loop:\n seek %q\n loop %q", got, ref)
	}
}

func TestIndexNestedLoopJoin_SeekHitsCarryTheLabel_2955(t *testing.T) {
	t.Run("memory", func(t *testing.T) {
		eng := NewEngine(lpg.New[string, float64](adjlist.Config{}))
		runAll2955(t, eng, seed2955)
		mustCreateIndex(t, eng)
		runAll2955(t, eng, relabel2955)
		check2955(t, eng)
	})
	t.Run("memory/same transaction", func(t *testing.T) {
		eng := NewEngine(lpg.New[string, float64](adjlist.Config{}))
		runAll2955(t, eng, seed2955)
		mustCreateIndex(t, eng)
		tx, err := eng.BeginTx(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		res, err := tx.ExecAny(relabel2955, nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Close()
		res, err = tx.ExecAny(q2955, map[string]any{"rows": inljKeyRows(keys2955)})
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for res.Next() {
			got = append(got, fmt.Sprintf("%v\x1f%v\x1f", res.ValueAt(0), res.ValueAt(1)))
		}
		if err := res.Err(); err != nil {
			t.Fatal(err)
		}
		_ = res.Close()
		if !slices.Equal(sorted2955(got), want2955) {
			t.Fatalf("in-transaction join:\n got %q\nwant %q", got, want2955)
		}
	})
	t.Run("wal/recovered", func(t *testing.T) {
		dir := t.TempDir()
		w, err := wal.Open(filepath.Join(dir, "wal"))
		if err != nil {
			t.Fatal(err)
		}
		st := txn.NewStoreWithOptions[string, float64](lpg.New[string, float64](adjlist.Config{}), w,
			txn.Options[string, float64]{Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec()})
		eng := NewEngineWithStore(st)
		runAll2955(t, eng, seed2955)
		mustCreateIndex(t, eng)
		runAll2955(t, eng, relabel2955)
		check2955(t, eng)
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		rec, err := recovery.Open[string, float64](dir, recovery.Options[string, float64]{
			Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec(),
		})
		if err != nil {
			t.Fatal(err)
		}
		check2955(t, NewEngineWithOptions(rec.Graph, EngineOptions{RecoveredIndexes: IndexDefsFromRecovery(rec.Indexes)}))
	})
}
