package cypher_test

// security_audit2026c_concurrency_test.go — FOURTH security audit
// (SEC-2026-06-14c), concurrency / ACID / resource-bounding surface.
//
// Unlike security_audit2026c_test.go (single-threaded Cypher-expression
// findings), this file holds SECURE-BEHAVIOUR LOCK-INS for the engine's
// cross-cutting concurrency invariants. The audit traced the lpg visMu
// visibility barrier, the per-shard atomic side-effect counters, and the
// store/txn single-writer model and found them solid; these tests pin that
// conclusion so a future change that regresses the barrier (cf. commit
// 3b22734) or makes a side-effect counter non-atomic is caught under -race.
//
// Every test is BOUNDED: a small fixed goroutine count and iteration count,
// no soak/nightly tags, and a context deadline so a regression that
// deadlocks the engine fails fast instead of hanging the runner.

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// recordInt reads an integer column from a result record, returning 0 when
// the column is absent or not an integer.
func recordInt(rec map[string]interface{}, col string) int64 {
	if v, ok := rec[col]; ok {
		if iv, ok := v.(expr.IntegerValue); ok {
			return int64(iv)
		}
	}
	return 0
}

// countLabel runs a one-shot count of nodes carrying label on eng.
func countLabel(ctx context.Context, t *testing.T, eng *cypher.Engine, label string) int64 {
	t.Helper()
	res, err := eng.Run(ctx, "MATCH (n:"+label+") RETURN count(n) AS c", nil)
	if err != nil {
		t.Fatalf("count Run: %v", err)
	}
	var got int64
	for res.Next() {
		got = recordInt(res.Record(), "c")
	}
	if cerr := res.Close(); cerr != nil {
		t.Fatalf("count Close: %v", cerr)
	}
	return got
}

// TestSEC14c_ConcurrentWriteRead_NoRace_NoPartialReads exercises the
// documented-safe concurrent path: many RunInTx writers committing
// independent CREATE statements while many Run readers count nodes on the
// SAME engine. It asserts three concurrency invariants of the audited
// surface:
//
//   - CWE-362 (race): under -race the run must report ZERO data races on
//     the shared lpg graph state (per-shard maps, tombstones, side-effect
//     counters) and the visMu barrier.
//   - ACID atomicity: a reader must never observe a half-applied CREATE.
//     Each writer creates a node carrying a label AND a property in one
//     statement; a reader that sees the node must also see the property,
//     so the labelled-count and propertied-count it observes always agree.
//   - ACID isolation / no lost writes: after every writer has committed,
//     the final committed node count equals the number of successful
//     commits — no commit is silently dropped or double-counted.
//
// The whole test is bounded by a 30s context so a barrier regression that
// deadlocks fails fast rather than hanging CI.
func TestSEC14c_ConcurrentWriteRead_NoRace_NoPartialReads(t *testing.T) {
	t.Parallel()

	const (
		writers          = 8
		commitsPerWriter = 16
		readers          = 8
		readsPerReader   = 64
	)

	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var (
		wg          sync.WaitGroup
		commitOK    [writers]int
		partialSeen sync.Map // reader id -> first partial-read description
	)

	// Writers: each commits commitsPerWriter independent nodes, every node
	// carrying both an :Audit label and a tag property created atomically in
	// one RunInTx statement.
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < commitsPerWriter; i++ {
				if ctx.Err() != nil {
					return
				}
				q := fmt.Sprintf("CREATE (n:Audit {w: %d, i: %d, tag: %d})", w, i, w*1000+i)
				res, err := eng.RunInTx(ctx, q, nil)
				if err != nil {
					continue
				}
				for res.Next() {
				}
				if cerr := res.Close(); cerr == nil {
					commitOK[w]++
				}
			}
		}(w)
	}

	// Readers: repeatedly count :Audit nodes two ways in the SAME snapshot
	// query and assert the two counts agree. If a reader ever saw a node
	// whose label was visible but whose tag property was not, the counts
	// would diverge — proof of a half-applied (non-atomic) CREATE.
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			for j := 0; j < readsPerReader; j++ {
				if ctx.Err() != nil {
					return
				}
				res, err := eng.Run(ctx,
					"MATCH (n:Audit) RETURN count(n) AS labelled, count(n.tag) AS tagged", nil)
				if err != nil {
					continue
				}
				var labelled, tagged int64
				for res.Next() {
					rec := res.Record()
					labelled = recordInt(rec, "labelled")
					tagged = recordInt(rec, "tagged")
				}
				_ = res.Close()
				if labelled != tagged {
					partialSeen.LoadOrStore(r, fmt.Sprintf(
						"reader %d observed labelled=%d but tagged=%d (partial CREATE)",
						r, labelled, tagged))
				}
			}
		}(r)
	}

	wg.Wait()
	if ctx.Err() != nil {
		t.Fatalf("test exceeded deadline (possible barrier deadlock): %v", ctx.Err())
	}

	// Invariant 1+2: no partial read was ever observed.
	partialSeen.Range(func(_, v any) bool {
		t.Errorf("ACID atomicity violation: %s", v)
		return true
	})

	// Invariant 3: final committed count == sum(successful commits).
	wantCommits := 0
	for _, c := range commitOK {
		wantCommits += c
	}
	if got := countLabel(ctx, t, eng, "Audit"); int(got) != wantCommits {
		t.Errorf("lost/double write: committed %d :Audit nodes but graph holds %d",
			wantCommits, got)
	}
}

// TestSEC14c_ConcurrentBeginWriters_SingleWriterSerialised hammers the
// single-writer model indirectly through the engine: N goroutines each open
// a write transaction that mutates the graph under contention. The
// single-writer serialisation (visMu in write mode) must (a) admit exactly
// one writer's mutations at a time — proven by every successful commit being
// fully and exactly visible with no torn count — and (b) never deadlock
// under contention. Bounded by a context deadline so a lock-ordering
// regression fails fast.
func TestSEC14c_ConcurrentBeginWriters_SingleWriterSerialised(t *testing.T) {
	t.Parallel()

	const (
		writers          = 16
		commitsPerWriter = 8
	)

	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var (
		wg      sync.WaitGroup
		okCount atomic.Int64
	)

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < commitsPerWriter; i++ {
				if ctx.Err() != nil {
					return
				}
				res, err := eng.RunInTx(ctx, fmt.Sprintf("CREATE (:W {w: %d, i: %d})", w, i), nil)
				if err != nil {
					continue
				}
				for res.Next() {
				}
				if res.Close() == nil {
					okCount.Add(1)
				}
			}
		}(w)
	}
	wg.Wait()
	if ctx.Err() != nil {
		t.Fatalf("writers exceeded deadline (possible single-writer deadlock): %v", ctx.Err())
	}

	// Every successful commit must be fully and exactly visible.
	if got := countLabel(ctx, t, eng, "W"); got != okCount.Load() {
		t.Errorf("single-writer serialisation lost a commit: committed %d but graph holds %d",
			okCount.Load(), got)
	}
}
