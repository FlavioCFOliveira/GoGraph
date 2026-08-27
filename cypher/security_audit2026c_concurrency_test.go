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
	// Deliberately NOT t.Parallel(): this test spawns many concurrent
	// txn writers/readers; running it in the parallel batch under -race
	// starves CPU from deadline-sensitive peers (e.g. the Cartesian
	// intermediate-work bound test) on shared CI runners. Run serially.

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

	// wg.Wait() RETURNING is itself the proof that no barrier deadlock occurred: a
	// deadlocked worker never reaches its wg.Done(), so this line would not be
	// reached at all and the package -timeout would fail the run. The
	// `if ctx.Err() != nil { t.Fatalf("possible barrier deadlock") }` that used to
	// sit here therefore COULD NOT detect the deadlock it named, and could only
	// ever fire on a machine under load — reporting a slow host as an ACID
	// violation, which is the most alarming false signal this file can emit
	// (rmp #2569).
	//
	// What the deadline genuinely can do is TRUNCATE the workload, because the
	// same ctx is also the workers' escape hatch. The invariants below are valid
	// on any prefix — each compares what was committed against what the graph
	// holds — so a truncated run is still sound. It simply certifies less, and a
	// run that certified almost nothing must not read as a clean pass. That is
	// what the non-vacuity floor below is for, and it is the honest replacement
	// for a deadline check that never worked.
	committed := 0
	for _, c := range commitOK {
		committed += c
	}
	if ctx.Err() != nil {
		t.Logf("the 30s deadline expired, so the workers took their escape hatch and this run "+
			"exercised LESS than the full workload: %d of %d commits succeeded. The invariants "+
			"below still hold on that prefix. This is NOT a deadlock — wg.Wait() returned, which "+
			"a deadlocked worker would have prevented (rmp #2569).",
			committed, writers*commitsPerWriter)
	}
	// Non-vacuity floor, deliberately STRUCTURAL rather than proportional: at
	// least one commit must have landed, so the invariants below are not compared
	// on an empty graph where 0 == 0 holds for any engine.
	//
	// A PROPORTIONAL floor ("a quarter of the intended commits") was written here
	// first and was wrong for the same reason this whole task exists: commitOK
	// counts SUCCESSFUL commits, so under contention legitimate write conflicts
	// lower it, and a fraction of a fixed workload inside a fixed 30s window is a
	// RATE over a non-dilating window — the exact defect shape rmp #2588 and
	// rmp #2517 were filed for. It would have turned a loaded machine red all over
	// again, one layer down. "Something was observed" is structural; "enough was
	// observed" is a rate.
	if committed < 1 {
		t.Fatalf("NOT ONE of the %d intended commits succeeded, so the invariants below compare "+
			"0 against 0 and would hold on any engine: this run certifies nothing about "+
			"concurrent write/read atomicity", writers*commitsPerWriter)
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

// TestSEC14c_ConcurrentBeginWriters_SingleWriterSerialised hammers the write path
// through the engine: N goroutines each open a write transaction that mutates the
// graph under contention. Every successful commit must be fully and exactly visible
// with no torn count, and the engine must never deadlock. Bounded by a context
// deadline so a lock-ordering regression fails fast.
//
// The mechanism it exercises CHANGED at rmp #2320 and the name is now historical.
// visMu is no longer what serialises an ordinary write — it is held SHARED by
// [lpg.Graph.ApplyVersioned], and atomic visibility comes from the transaction's
// shared commit record instead. What still serialises writes on this store-LESS
// wiring is cypher.Engine.writeMu, which BeginTx holds for the transaction's whole
// lifetime; retiring it is rmp #2306. The invariant asserted below — no torn count,
// no deadlock — is unchanged and is what the test is really for, so it is left
// exactly as it was.
func TestSEC14c_ConcurrentBeginWriters_SingleWriterSerialised(t *testing.T) {
	// Deliberately NOT t.Parallel(): see the sibling test above — heavy
	// concurrent writers must not contend with deadline-sensitive parallel
	// peers under -race on shared CI runners.

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

	// Same reasoning as the sibling test above: wg.Wait() returning proves no
	// deadlock, so a post-wait ctx.Err() check could only ever report a slow host
	// as one (rmp #2569). The invariant below holds on any prefix; the floor is
	// what stops a truncated run passing vacuously.
	if ctx.Err() != nil {
		t.Logf("the 30s deadline expired, so the writers took their escape hatch and this run "+
			"exercised LESS than the full workload: %d of %d commits succeeded. The invariant "+
			"below still holds on that prefix. This is NOT a deadlock or a livelock — wg.Wait() "+
			"returned (rmp #2569).", okCount.Load(), writers*commitsPerWriter)
	}
	// Structural, not proportional — see the sibling test above for why a fraction
	// of a fixed workload in a fixed window is a rate and would reintroduce the
	// very defect this task removes.
	if okCount.Load() < 1 {
		t.Fatalf("NOT ONE of the %d intended commits succeeded, so the visibility invariant "+
			"below compares 0 against 0 and would hold on any engine: this run certifies "+
			"nothing about commit visibility under contention", writers*commitsPerWriter)
	}

	// Every successful commit must be fully and exactly visible.
	if got := countLabel(ctx, t, eng, "W"); got != okCount.Load() {
		t.Errorf("a commit was LOST: committed %d but graph holds %d",
			okCount.Load(), got)
	}
}
