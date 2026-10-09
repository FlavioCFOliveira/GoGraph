package cypher

// Regression battery for rmp #2931: an index entry written at commit must
// describe the state that commit produces, never the graph's present, which
// also holds other transactions' uncommitted writes.
//
// Every assertion compares an index SEEK with a SCAN of the same predicate,
// written as `n.s + '' = …` so no equality rewrite can claim it. The scan reads
// the graph through the snapshot and is the oracle; a seek that disagrees with it
// is reading a wrong index.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// commitStateEngine seeds 512 :L nodes, each with tag t<i> and s = "s<i>", and
// runs ddl. 512 because the seek paths are population-gated.
func commitStateEngine(t *testing.T, ddl string) *Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < 512; i++ {
		n := fmt.Sprintf("n%d", i)
		if err := g.AddNode(n); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeLabel(n, "L"); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeProperty(n, "tag", lpg.StringValue(fmt.Sprintf("t%d", i))); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeProperty(n, "s", lpg.StringValue(fmt.Sprintf("s%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	eng := NewEngine(g)
	if _, err := eng.Run(context.Background(), ddl, nil); err != nil {
		t.Fatalf("%s: %v", ddl, err)
	}
	return eng
}

// commitStateCount returns the single count column of a read query.
func commitStateCount(t *testing.T, eng *Engine, q string) int64 {
	t.Helper()
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer func() { _ = res.Close() }()
	var n int64 = -1
	for res.Next() {
		switch v := res.Record()["c"].(type) {
		case int64:
			n = v
		case int:
			n = int64(v)
		case expr.IntegerValue:
			n = int64(v)
		default:
			t.Fatalf("%s: count has type %T", q, v)
		}
	}
	if err := res.Err(); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// commitStateExec runs a write statement on the autocommit path.
func commitStateExec(t *testing.T, eng *Engine, q string) {
	t.Helper()
	res, err := eng.RunAny(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	_ = res.Close()
}

// commitStateSeekVsScan returns the seek and scan counts for s = value: the
// equality seek, which a hash index serves.
func commitStateSeekVsScan(t *testing.T, eng *Engine, value string) (seek, scan int64) {
	t.Helper()
	seek = commitStateCount(t, eng, fmt.Sprintf(`MATCH (n:L {s: '%s'}) RETURN count(n) AS c`, value))
	scan = commitStateCount(t, eng, fmt.Sprintf(`MATCH (n:L) WHERE n.s + '' = '%s' RETURN count(n) AS c`, value))
	return seek, scan
}

// commitStatePrefixVsScan is the same comparison through the prefix seek, which
// a string btree serves (a btree does not serve the equality rewrite). The
// prefix seek keeps its predicate as a residual filter, so it can hide an entry
// the index fabricated but never restore one the index lost.
func commitStatePrefixVsScan(t *testing.T, eng *Engine, value string) (seek, scan int64) {
	t.Helper()
	seek = commitStateCount(t, eng, fmt.Sprintf(`MATCH (n:L) WHERE n.s STARTS WITH '%s' RETURN count(n) AS c`, value))
	scan = commitStateCount(t, eng, fmt.Sprintf(`MATCH (n:L) WHERE (n.s + '') STARTS WITH '%s' RETURN count(n) AS c`, value))
	return seek, scan
}

// TestIndexCommitState_PeerRollbackDoesNotPolluteIndex is the four-step
// reproduction. A committed label add on a node that an OPEN transaction has
// re-valued must index the node under its COMMITTED value; when the open
// transaction then rolls back, the index must still agree with the graph.
//
// Before the fix the label add read the open transaction's uncommitted value,
// so the node was indexed under 'polluted' and not under 's33', permanently.
func TestIndexCommitState_PeerRollbackDoesNotPolluteIndex(t *testing.T) {
	for _, c := range []struct {
		ddl     string
		compare func(*testing.T, *Engine, string) (int64, int64)
	}{
		{"CREATE INDEX FOR (n:L) ON (n.s)", commitStateSeekVsScan},
		{"CREATE INDEX FOR (n:L) ON (n.s) OPTIONS {indexType: 'btree'}", commitStatePrefixVsScan},
	} {
		ddl := c.ddl
		t.Run(ddl, func(t *testing.T) {
			ctx := context.Background()
			eng := commitStateEngine(t, ddl)

			commitStateExec(t, eng, `MATCH (n {tag: 't33'}) REMOVE n:L`)
			tx, err := eng.BeginTx(ctx)
			if err != nil {
				t.Fatal(err)
			}
			res, err := tx.ExecAny(`MATCH (n {tag: 't33'}) SET n.s = 'polluted'`, nil)
			if err != nil {
				t.Fatal(err)
			}
			for res.Next() {
			}
			_ = res.Close()
			commitStateExec(t, eng, `MATCH (n {tag: 't33'}) SET n:L`)
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}

			for _, v := range []string{"s33", "polluted"} {
				seek, scan := c.compare(t, eng, v)
				if seek != scan {
					t.Errorf("s = %q: seek %d, scan %d", v, seek, scan)
				}
			}
			// The control: the scan must see the committed node, or the fixture
			// did not produce the state the test is about.
			if _, scan := c.compare(t, eng, "s33"); scan < 1 {
				t.Fatalf("control: scan for 's33' = %d, want at least 1", scan)
			}
		})
	}
}

// TestIndexCommitState_ConcurrentChurnNeverDiverges drives writers that add and
// remove the indexed label and change the indexed key, alongside explicit
// transactions that re-value nodes and then roll back or commit, and asserts at
// every quiescent point that every key's seek equals its scan.
//
// Quiescent points only: while writers run, a read's snapshot and the index can
// legitimately describe different instants. The claim under test is that the
// index converges to the committed state, which is what a rolled-back peer used
// to break for good.
func TestIndexCommitState_ConcurrentChurnNeverDiverges(t *testing.T) {
	const (
		nodes   = 64
		keys    = 8
		writers = 4
		rounds  = 10
	)
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < nodes; i++ {
		n := fmt.Sprintf("c%d", i)
		if err := g.AddNode(n); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeLabel(n, "L"); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeProperty(n, "tag", lpg.StringValue(n)); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeProperty(n, "s", lpg.StringValue(fmt.Sprintf("k%d", i%keys))); err != nil {
			t.Fatal(err)
		}
	}
	eng := NewEngine(g)
	for _, ddl := range []string{
		"CREATE INDEX FOR (n:L) ON (n.s)",
		"CREATE INDEX lsb FOR (n:L) ON (n.s) OPTIONS {indexType: 'btree'}",
	} {
		if _, err := eng.Run(context.Background(), ddl, nil); err != nil {
			t.Fatal(err)
		}
	}
	var committed, rolledBack atomic.Int64
	for r := 0; r < rounds; r++ {
		var stop atomic.Bool
		var wg sync.WaitGroup
		for w := 0; w < writers; w++ {
			wg.Add(1)
			go func(seed uint64) {
				defer wg.Done()
				rng := rand.New(rand.NewPCG(seed, seed^0x9e37)) //nolint:gosec // a reproducible workload, not a secret
				ctx := context.Background()
				for !stop.Load() {
					tag := fmt.Sprintf("c%d", rng.IntN(nodes))
					if rng.IntN(4) == 0 {
						// An explicit transaction re-values a node and holds the
						// write open across other writers' commits.
						tx, err := eng.BeginTx(ctx)
						if err != nil {
							continue
						}
						q := fmt.Sprintf("MATCH (n {tag: '%s'}) SET n.s = 'x%d'", tag, rng.IntN(keys))
						if res, err := tx.ExecAny(q, nil); err == nil {
							for res.Next() {
							}
							_ = res.Close()
						}
						time.Sleep(time.Duration(rng.IntN(200)) * time.Microsecond)
						if rng.IntN(2) == 0 {
							_ = tx.Rollback()
							rolledBack.Add(1)
						} else if tx.Commit() == nil {
							committed.Add(1)
						}
						continue
					}
					var q string
					switch rng.IntN(3) {
					case 0:
						q = fmt.Sprintf("MATCH (n {tag: '%s'}) SET n.s = 'k%d'", tag, rng.IntN(keys))
					case 1:
						q = fmt.Sprintf("MATCH (n {tag: '%s'}) REMOVE n:L", tag)
					default:
						q = fmt.Sprintf("MATCH (n {tag: '%s'}) SET n:L", tag)
					}
					res, err := eng.RunAny(ctx, q, nil)
					if err != nil {
						continue // a serialization conflict is an expected outcome
					}
					for res.Next() {
					}
					_ = res.Close()
					committed.Add(1)
				}
			}(uint64(r*writers+w) + 1)
		}
		time.Sleep(150 * time.Millisecond)
		stop.Store(true)
		wg.Wait()
		for k := 0; k < keys; k++ {
			for _, prefix := range []string{"k", "x"} {
				v := fmt.Sprintf("%s%d", prefix, k)
				for _, cmp := range []func(*testing.T, *Engine, string) (int64, int64){
					commitStateSeekVsScan, commitStatePrefixVsScan,
				} {
					seek, scan := cmp(t, eng, v)
					if seek != scan {
						t.Fatalf("round %d, s = %q: seek %d, scan %d (committed %d, rolled back %d)",
							r, v, seek, scan, committed.Load(), rolledBack.Load())
					}
				}
			}
		}
	}
	t.Logf("committed=%d rolledBack=%d", committed.Load(), rolledBack.Load())
	if committed.Load() == 0 || rolledBack.Load() == 0 {
		t.Fatalf("the workload did not exercise both outcomes (committed=%d rolledBack=%d)",
			committed.Load(), rolledBack.Load())
	}
}

// TestIndexCommitState_ConcurrentCreateChurnNeverDiverges is the churn test over
// nodes created while the workload runs. A commit's applier takes no shard lock
// for a node the transaction created ([lpg.CommitNodes.Private]), so this drives
// exactly those commits — batched creates, some inside explicit transactions that
// roll back — against writers that re-value, relabel and delete the freshly
// created nodes of every writer, and asserts at every quiescent point that every
// key's seek equals its scan (rmp #2931 audit, F3).
func TestIndexCommitState_ConcurrentCreateChurnNeverDiverges(t *testing.T) {
	const (
		keys    = 8
		writers = 6
		rounds  = 8
		batch   = 8
	)
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := NewEngine(g)
	for _, ddl := range []string{
		"CREATE INDEX FOR (n:L) ON (n.s)",
		"CREATE INDEX lsb FOR (n:L) ON (n.s) OPTIONS {indexType: 'btree'}",
	} {
		if _, err := eng.Run(context.Background(), ddl, nil); err != nil {
			t.Fatal(err)
		}
	}
	var created [writers]atomic.Int64
	var creates, mutations, rolledBack atomic.Int64
	drain := func(res *Result, err error) bool {
		if err != nil {
			return false
		}
		for res.Next() {
		}
		return res.Close() == nil
	}
	createRows := func(w int, base int64, rng *rand.Rand) map[string]any {
		rows := make([]any, batch)
		for i := range rows {
			rows[i] = map[string]any{
				"tag": fmt.Sprintf("f%d-%d", w, base+int64(i)),
				"s":   fmt.Sprintf("k%d", rng.IntN(keys)),
			}
		}
		return map[string]any{"rows": rows}
	}
	const createQ = "UNWIND $rows AS r CREATE (:L {tag: r.tag, s: r.s})"
	for r := 0; r < rounds; r++ {
		var stop atomic.Bool
		var wg sync.WaitGroup
		for w := 0; w < writers; w++ {
			wg.Add(1)
			go func(w int, seed uint64) {
				defer wg.Done()
				rng := rand.New(rand.NewPCG(seed, seed^0x51f1)) //nolint:gosec // a reproducible workload, not a secret
				ctx := context.Background()
				for !stop.Load() {
					switch op := rng.IntN(6); {
					case op < 2: // a batched autocommit create: the lock-free path
						base := created[w].Load()
						if drain(eng.RunAny(ctx, createQ, createRows(w, base, rng))) {
							created[w].Add(batch)
							creates.Add(1)
						}
					case op == 2: // a create inside an explicit transaction, kept or rolled back
						tx, err := eng.BeginTx(ctx)
						if err != nil {
							continue
						}
						base := created[w].Load()
						ok := drain(tx.ExecAny(createQ, createRows(w, base, rng)))
						time.Sleep(time.Duration(rng.IntN(200)) * time.Microsecond)
						if !ok || rng.IntN(2) == 0 {
							_ = tx.Rollback()
							rolledBack.Add(1)
						} else if tx.Commit() == nil {
							created[w].Add(batch)
							creates.Add(1)
						}
					default: // mutate some writer's freshly created node
						owner := rng.IntN(writers)
						n := created[owner].Load()
						if n == 0 {
							continue
						}
						tag := fmt.Sprintf("f%d-%d", owner, rng.Int64N(n))
						var q string
						switch rng.IntN(4) {
						case 0:
							q = fmt.Sprintf("MATCH (n {tag: '%s'}) SET n.s = 'x%d'", tag, rng.IntN(keys))
						case 1:
							q = fmt.Sprintf("MATCH (n {tag: '%s'}) REMOVE n:L", tag)
						case 2:
							q = fmt.Sprintf("MATCH (n {tag: '%s'}) SET n:L", tag)
						default:
							q = fmt.Sprintf("MATCH (n {tag: '%s'}) DETACH DELETE n", tag)
						}
						if drain(eng.RunAny(ctx, q, nil)) {
							mutations.Add(1)
						}
					}
				}
			}(w, uint64(r*writers+w)+1)
		}
		time.Sleep(150 * time.Millisecond)
		stop.Store(true)
		wg.Wait()
		for k := 0; k < keys; k++ {
			for _, prefix := range []string{"k", "x"} {
				v := fmt.Sprintf("%s%d", prefix, k)
				for _, cmp := range []func(*testing.T, *Engine, string) (int64, int64){
					commitStateSeekVsScan, commitStatePrefixVsScan,
				} {
					seek, scan := cmp(t, eng, v)
					if seek != scan {
						t.Fatalf("round %d, s = %q: seek %d, scan %d (creates %d, mutations %d, rolled back %d)",
							r, v, seek, scan, creates.Load(), mutations.Load(), rolledBack.Load())
					}
				}
			}
		}
	}
	t.Logf("creates=%d mutations=%d rolledBack=%d", creates.Load(), mutations.Load(), rolledBack.Load())
	if creates.Load() == 0 || mutations.Load() == 0 || rolledBack.Load() == 0 {
		t.Fatalf("the workload did not exercise every outcome (creates=%d mutations=%d rolledBack=%d)",
			creates.Load(), mutations.Load(), rolledBack.Load())
	}
}
