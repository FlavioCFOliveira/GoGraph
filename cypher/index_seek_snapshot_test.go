package cypher_test

// index_seek_snapshot_test.go — does an INDEX SEEK respect the reader's instant?
//
// readview.go documents the secondary-index manager and the label bitmap as
// candidate sources "read at the PRESENT", and says the label bitmap is
// "re-checked against the versioned label bags". No such re-check is documented
// for the property indexes, so this asks whether one happens.
//
// The check is a SELF-CONTRADICTION, which needs no external oracle: seek a node
// by an indexed property, then assert that same property in the WHERE clause. At
// any single instant the answer is necessarily zero rows — a node the seek found
// by `id = N` must satisfy `id = N`. A row surviving means the seek and the
// property read answered at two different instants: the index is a present-time
// candidate source and nothing re-checked its candidates against the snapshot.
//
// It is driven concurrently because the engine takes its own snapshot per query,
// so straddling a commit is the only way to get a reader whose instant differs
// from the present.
//
// Layer: short.

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// scalarCount runs a `RETURN count(...) AS c` query and returns the integer.
func scalarCount(t *testing.T, eng *cypher.Engine, q string, params map[string]expr.Value) int64 {
	t.Helper()
	res, err := eng.Run(context.Background(), q, params)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer func() { _ = res.Close() }()
	var n int64
	for res.Next() {
		if v, ok := res.Record()["c"].(expr.IntegerValue); ok {
			n = int64(v)
		}
	}
	if err := res.Err(); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// TestIndexSeek_SelfContradictionUnderConcurrentWrites drives an indexed seek
// against a writer that keeps creating and deleting the nodes the seek looks
// for, and requires the seek and the predicate over the SAME property to agree.
func TestIndexSeek_SelfContradictionUnderConcurrentWrites(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	write := func(q string) error {
		res, err := eng.RunAny(ctx, q, nil)
		if err != nil {
			return err
		}
		for res.Next() {
		}
		defer func() { _ = res.Close() }()
		return res.Err()
	}
	if _, err := eng.RunInTx(ctx, `CREATE INDEX FOR (n:Item) ON (n.id)`, nil); err != nil {
		t.Fatalf("create index: %v", err)
	}

	const keys = 40
	for i := 0; i < keys; i++ {
		if err := write(fmt.Sprintf(`CREATE (:Item {id: %d, tag: 'a'})`, i)); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	// A node the seek found by `id = N` must satisfy `id = N` at the instant the
	// seek ran. Zero is the only correct answer at any single instant.
	const qContradiction = `MATCH (n:Item {id: $id}) WHERE NOT (n.id = $id) RETURN count(n) AS c`

	// SENSITIVITY CONTROLS, run before the concurrent phase. A contradiction
	// query that always returns zero because its WHERE clause is folded away, or
	// because the seek finds nothing, is VACUOUS — it would report a clean bill of
	// health for any engine at all. These pin that the clause really runs and the
	// seek really matches, so the zero below means something.
	one := map[string]expr.Value{"id": expr.IntegerValue(0)}
	if got := scalarCount(t, eng, `MATCH (n:Item {id: $id}) WHERE NOT (n.tag = 'zzz') RETURN count(n) AS c`, one); got != 1 {
		t.Fatalf("sensitivity control = %d, want 1: the WHERE clause is not being evaluated, "+
			"so the contradiction check below would pass vacuously", got)
	}
	if got := scalarCount(t, eng, `MATCH (n:Item {id: $id}) WHERE n.id = $id RETURN count(n) AS c`, one); got != 1 {
		t.Fatalf("positive control = %d, want 1: the indexed seek is not matching, "+
			"so the contradiction check below would pass vacuously", got)
	}
	if got := scalarCount(t, eng, qContradiction, one); got != 0 {
		t.Fatalf("contradiction shape = %d, want 0 at a single instant", got)
	}

	var (
		stop         atomic.Bool
		contra       atomic.Int64
		observations atomic.Int64
		readErrs     atomic.Int64
		wg           sync.WaitGroup
	)

	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for i := 0; !stop.Load(); i++ {
				id := (seed*7 + i) % keys
				res, err := eng.Run(ctx, qContradiction,
					map[string]expr.Value{"id": expr.IntegerValue(int64(id))})
				if err != nil {
					readErrs.Add(1)
					continue
				}
				for res.Next() {
					if v, ok := res.Record()["c"].(expr.IntegerValue); ok && int64(v) != 0 {
						contra.Add(int64(v))
					}
				}
				if res.Err() != nil {
					readErrs.Add(1)
				}
				_ = res.Close()
				observations.Add(1)
			}
		}(r)
	}

	// Churn the indexed property: delete and recreate, so a seek's candidate can
	// legitimately have stopped matching by the time the predicate resolves it.
	for i := 0; i < 300; i++ {
		id := i % keys
		if err := write(fmt.Sprintf(`MATCH (n:Item {id: %d}) DETACH DELETE n`, id)); err != nil {
			t.Errorf("churn delete %d: %v", id, err)
			break
		}
		if err := write(fmt.Sprintf(`CREATE (:Item {id: %d, tag: 'a'})`, id)); err != nil {
			t.Errorf("churn create %d: %v", id, err)
			break
		}
	}
	stop.Store(true)
	wg.Wait()

	if observations.Load() == 0 {
		t.Fatal("no observation completed; the check did not run")
	}
	if got := readErrs.Load(); got != 0 {
		t.Errorf("%d observation quer(ies) failed out of %d", got, observations.Load())
	}
	if got := contra.Load(); got != 0 {
		t.Errorf("INDEX SEEK / PREDICATE DISAGREEMENT: %d row(s) over %d observations were "+
			"returned by a seek on `id = $id` and then failed `n.id = $id` — the seek and the "+
			"property read answered at two different instants, so the property index is a "+
			"present-time candidate source whose candidates are not re-checked against the "+
			"reader's snapshot", got, observations.Load())
	}
	t.Logf("observations=%d contradictions=%d", observations.Load(), contra.Load())
}
