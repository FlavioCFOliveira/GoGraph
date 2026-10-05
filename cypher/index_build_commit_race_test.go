package cypher

// index_build_commit_race_test.go — a CREATE INDEX build racing an explicit
// transaction's commit must not register the index without the commit's node
// (rmp #2936).
//
// Layer: short.
//
// The two shapes of graph/index/commit_gate.go are reproduced deterministically
// with the engine's test seams, which pause an explicit transaction between its
// index decision and its publication (shape 1) or between its index delivery and
// its publication (shape 2) while a CREATE INDEX runs. Before the fix the build
// completed during the pause and the index lost the node; a MERGE trusting it then
// created a duplicate. The stress test drives CREATE/DROP INDEX against committing
// writers of both kinds.

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// buildRacePopulation is enough :L nodes for the planner to take the seek.
const buildRacePopulation = 512

// buildRaceEngine returns an engine over a graph seeded with filler :L nodes, on
// the in-memory or the WAL wiring.
func buildRaceEngine(t *testing.T, walBacked bool) *Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < buildRacePopulation; i++ {
		n := fmt.Sprintf("filler%d", i)
		if err := g.AddNode(n); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeLabel(n, "L"); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeProperty(n, "s", lpg.StringValue(fmt.Sprintf("f%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	if !walBacked {
		return NewEngine(g)
	}
	wr, err := wal.Open(filepath.Join(t.TempDir(), "wal"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wr.Close() })
	st := txn.NewStoreWithOptions[string, float64](g, wr, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	return NewEngineWithStore(st)
}

// buildRaceAssertOne asserts that exactly one :L node carries s = v, by seek and
// by scan, and that a MERGE on it matches rather than creates.
func buildRaceAssertOne(t *testing.T, eng *Engine, v string) {
	t.Helper()
	if seek, scan := commitStateSeekVsScan(t, eng, v); seek != 1 || scan != 1 {
		t.Errorf("s = %q: seek %d, scan %d, want 1 and 1: the index was registered without the node", v, seek, scan)
	}
	commitStateExec(t, eng, fmt.Sprintf(`MERGE (:L {s: '%s'})`, v))
	if c := commitStateCount(t, eng, fmt.Sprintf(`MATCH (n:L) WHERE n.s + '' = '%s' RETURN count(n) AS c`, v)); c != 1 {
		t.Errorf("s = %q: %d nodes after MERGE, want 1: MERGE trusted the index and created a duplicate", v, c)
	}
}

// buildRaceDDLs are the two DDL statements that build a node property index:
// CREATE INDEX, and CREATE CONSTRAINT … IS UNIQUE, whose backing index and
// value-set are built from the same scan (rmp #2936 covers both).
var buildRaceDDLs = []struct{ name, create, drop string }{
	{"index", "CREATE INDEX ls FOR (n:L) ON (n.s)", "DROP INDEX ls"},
	{"unique", "CREATE CONSTRAINT us FOR (n:L) REQUIRE n.s IS UNIQUE", "DROP CONSTRAINT us"},
}

// buildRaceAssertUnique asserts that the UNIQUE constraint on (:L).s refuses a
// second node carrying v.
func buildRaceAssertUnique(t *testing.T, eng *Engine, v string) {
	t.Helper()
	res, err := eng.RunAny(context.Background(), fmt.Sprintf(`CREATE (:L {s: '%s'})`, v), nil)
	if err == nil {
		for res.Next() {
		}
		err = res.Err()
		_ = res.Close()
	}
	if !errors.Is(err, exec.ErrConstraintViolation) {
		t.Errorf("a second node with s = %q committed under a live UNIQUE constraint (err %v): the "+
			"constraint's value-set was seeded without the racing commit", v, err)
	}
}

func TestIndexBuild_ExplicitCommitRacingCreateIndex(t *testing.T) {
	for _, ddl := range buildRaceDDLs {
		for _, shape := range []string{"decided-before-build", "delivered-before-build"} {
			for _, walBacked := range []bool{false, true} {
				name := ddl.name + "/" + shape + "/mem"
				if walBacked {
					name = ddl.name + "/" + shape + "/wal"
				}
				t.Run(name, func(t *testing.T) {
					ctx := context.Background()
					eng := buildRaceEngine(t, walBacked)
					if shape == "delivered-before-build" {
						// An index the transaction's change DOES concern, so the change
						// is delivered — to this index only — before the build starts.
						commitStateExec(t, eng, "CREATE INDEX lt FOR (n:L) ON (n.t)")
					}
					tx, err := eng.BeginTx(ctx)
					if err != nil {
						t.Fatal(err)
					}
					res, err := tx.ExecAny(`CREATE (:L {s: 'v', t: 'w'})`, nil)
					if err != nil {
						t.Fatal(err)
					}
					for res.Next() {
					}
					_ = res.Close()

					paused, resume := make(chan struct{}), make(chan struct{})
					var once sync.Once
					hook := func() { once.Do(func() { close(paused); <-resume }) }
					if shape == "decided-before-build" {
						eng.commitDecidedHookForTest = hook
					} else {
						eng.indexDeliveredHookForTest = hook
					}
					committed := make(chan error, 1)
					go func() { committed <- tx.Commit() }()
					<-paused

					built := make(chan error, 1)
					go func() {
						res, err := eng.RunAny(ctx, ddl.create, nil)
						if err == nil {
							for res.Next() {
							}
							err = res.Close()
						}
						built <- err
					}()
					var builtDuringPause bool
					select {
					case err := <-built:
						builtDuringPause = true
						if err != nil {
							t.Fatalf("%s: %v", ddl.create, err)
						}
					case <-time.After(300 * time.Millisecond):
					}
					close(resume)
					if err := <-committed; err != nil {
						t.Fatalf("Commit: %v", err)
					}
					if !builtDuringPause {
						if err := <-built; err != nil {
							t.Fatalf("%s: %v", ddl.create, err)
						}
					}
					eng.commitDecidedHookForTest, eng.indexDeliveredHookForTest = nil, nil
					if builtDuringPause {
						t.Error("the build completed while a commit that had already decided was unpublished: " +
							"the build did not wait out the commit-decision bracket")
					}
					// MERGE first, then a plain CREATE: under UNIQUE both must leave one node.
					buildRaceAssertOne(t, eng, "v")
					if ddl.name == "unique" {
						buildRaceAssertUnique(t, eng, "v")
					}
				})
			}
		}
	}
}

// TestIndexBuild_CreateDropUnderCommittingWriters runs CREATE INDEX and DROP
// INDEX repeatedly against writers that commit through explicit transactions
// (some rolled back) and autocommit, creating nodes and MERGE-ing on keys only
// they write. After every build it stops the writers and asserts that every key
// written is found by the seek exactly as often as by the scan, and that no MERGE
// key has more than one node.
//
// Each writer runs through its own [Session] (rmp #2977). "No MERGE key has more
// than one node" holds only if a writer's next statement observes its previous
// commit, and that is the session contract, not the engine's: a sessionless
// commit above an in-flight one returns before it is visible, so the writer's
// next MERGE can start below it and create a second node, which snapshot
// isolation permits (docs/isolation-design.md, "Commit visibility and the
// session contract"; TestMergeVisibility_SessionContract pins both halves).
// An index build holds commits in flight long enough to open that window.
func TestIndexBuild_CreateDropUnderCommittingWriters(t *testing.T) {
	for _, arm := range []struct {
		writers   int
		walBacked bool
	}{{8, true}, {64, false}} {
		t.Run(fmt.Sprintf("writers=%d/wal=%v", arm.writers, arm.walBacked), func(t *testing.T) {
			const rounds, mergeKeys = 40, 8
			ctx := context.Background()
			eng := buildRaceEngine(t, arm.walBacked)
			var mu sync.Mutex
			// created counts every committed write per key; touched is the keys
			// written this round, which are the ones a build racing this round's
			// commits can have lost. A MERGE key keeps its count across rounds.
			created := map[string]int{}
			var touched map[string]struct{}
			uniqueRounds := 0
			var explicit, autocommit, rolledBack atomic.Int64
			run := func(q string) bool {
				res, err := eng.RunAny(ctx, q, nil)
				if err != nil {
					return false
				}
				for res.Next() {
				}
				return res.Close() == nil
			}
			for r := 0; r < rounds; r++ {
				touched = map[string]struct{}{}
				var stop atomic.Bool
				var wg sync.WaitGroup
				for w := 0; w < arm.writers; w++ {
					wg.Add(1)
					go func(w int) {
						defer wg.Done()
						rng := rand.New(rand.NewPCG(uint64(r*arm.writers+w)+1, 3)) //nolint:gosec // a workload, not a secret
						// The writer's own session: its next statement observes its
						// previous commit, which the MERGE-key oracle relies on.
						sess := eng.NewSession()
						for i := 0; !stop.Load(); i++ {
							var q, key string
							if rng.IntN(2) == 0 {
								key = fmt.Sprintf("m%d-%d", w, rng.IntN(mergeKeys))
								q = fmt.Sprintf(`MERGE (:L {s: '%s'})`, key)
							} else {
								key = fmt.Sprintf("c%d-%d-%d", r, w, i)
								q = fmt.Sprintf(`CREATE (:L {s: '%s'})`, key)
							}
							if rng.IntN(2) == 0 {
								res, err := sess.RunAny(ctx, q, nil)
								if err != nil {
									continue
								}
								for res.Next() {
								}
								if res.Close() != nil {
									continue
								}
								autocommit.Add(1)
							} else {
								tx, err := sess.BeginTx(ctx)
								if err != nil {
									continue
								}
								res, err := tx.ExecAny(q, nil)
								if err != nil {
									_ = tx.Rollback()
									continue
								}
								for res.Next() {
								}
								_ = res.Close()
								if rng.IntN(8) == 0 {
									_ = tx.Rollback()
									rolledBack.Add(1)
									continue
								}
								if tx.Commit() != nil {
									continue
								}
								explicit.Add(1)
							}
							mu.Lock()
							created[key]++
							touched[key] = struct{}{}
							mu.Unlock()
						}
					}(w)
				}
				time.Sleep(3 * time.Millisecond)
				ddl := buildRaceDDLs[r%len(buildRaceDDLs)]
				if !run(ddl.create) {
					stop.Store(true)
					wg.Wait()
					t.Fatalf("round %d: %s failed", r, ddl.create)
				}
				stop.Store(true)
				wg.Wait()
				mu.Lock()
				for key := range touched {
					want := int64(created[key])
					if key[0] == 'm' {
						want = 1
					}
					seek, scan := commitStateSeekVsScan(t, eng, key)
					if scan != want || seek != scan {
						mu.Unlock()
						t.Fatalf("round %d, s = %q: seek %d, scan %d, want %d (explicit %d, autocommit %d, rolled back %d)",
							r, key, seek, scan, want, explicit.Load(), autocommit.Load(), rolledBack.Load())
					}
				}
				if ddl.name == "unique" {
					// Under the live constraint every value written this round must
					// be reserved: a second node with it must be refused. A commit
					// the build missed is missing from the value-set, and this
					// CREATE is what would then succeed.
					for key := range touched {
						res, err := eng.RunAny(ctx, fmt.Sprintf(`CREATE (:L {s: '%s'})`, key), nil)
						if err == nil {
							for res.Next() {
							}
							err = res.Err()
							_ = res.Close()
						}
						if !errors.Is(err, exec.ErrConstraintViolation) {
							mu.Unlock()
							t.Fatalf("round %d: a second node with s = %q committed under a live UNIQUE "+
								"constraint (err %v)", r, key, err)
						}
					}
					if dups := commitStateCount(t, eng, `MATCH (n:L) WITH n.s AS s, count(*) AS k WHERE k > 1 RETURN count(*) AS c`); dups != 0 {
						mu.Unlock()
						t.Fatalf("round %d: %d values carried by more than one node under a live UNIQUE constraint", r, dups)
					}
					uniqueRounds++
				}
				mu.Unlock()
				if !run(ddl.drop) {
					t.Fatalf("round %d: %s failed", r, ddl.drop)
				}
			}
			t.Logf("explicit=%d autocommit=%d rolledBack=%d keys=%d",
				explicit.Load(), autocommit.Load(), rolledBack.Load(), len(created))
			if uniqueRounds == 0 {
				t.Fatal("no round built a UNIQUE constraint")
			}
			if explicit.Load() == 0 || autocommit.Load() == 0 || rolledBack.Load() == 0 {
				t.Fatal("the workload did not exercise every kind of commit")
			}
		})
	}
}
