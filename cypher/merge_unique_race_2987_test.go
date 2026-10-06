package cypher_test

// merge_unique_race_2987_test.go — rmp #2987.
//
// Concurrent AUTOCOMMIT MERGE of one key under a UNIQUE constraint must converge
// on one node with EVERY caller succeeding: the losers of the creation race match
// the winner's node instead of failing with a constraint violation. That is the
// guarantee cypher/merge_race_test.go states (F10); its own 8x1 shape rarely
// overlaps the callers, so it passed while the guarantee was broken.
//
// The shape here makes the race certain rather than likely: every round names a
// FRESH key, and all goroutines are released onto it together, so each round is
// one creation race with goroutines-1 losers. Only the first MERGE of a key can
// race — a key that already exists is simply matched — which is why the key must
// change every round.
//
// Layer: short. Race-clean.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// mergeUniqueRaceMemEngine is the in-memory wiring of the #2987 reproduction.
func mergeUniqueRaceMemEngine(t *testing.T) *cypher.Engine {
	t.Helper()
	eng := cypher.NewEngine(lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true}))
	t.Cleanup(func() { _ = eng.Close() })
	return eng
}

// mergeUniqueRaceExec runs one autocommit statement to completion.
func mergeUniqueRaceExec(ctx context.Context, eng *cypher.Engine, q string, p map[string]expr.Value) error {
	res, err := eng.RunInTx(ctx, q, p)
	if err != nil {
		return err
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		_ = res.Close()
		return err
	}
	return res.Close()
}

// TestMerge_ConcurrentUniqueKey_EveryCallerSucceeds is the #2987 regression gate,
// on the in-memory engine and on the WAL-backed engine.
func TestMerge_ConcurrentUniqueKey_EveryCallerSucceeds(t *testing.T) {
	t.Parallel()
	for _, arm := range []struct {
		name string
		eng  func(*testing.T) *cypher.Engine
	}{
		{"memory", mergeUniqueRaceMemEngine},
		{"wal", mergeRaceEngine},
	} {
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			const (
				goroutines = 8
				rounds     = 16
			)
			ctx := context.Background()
			eng := arm.eng(t)
			if err := mergeUniqueRaceExec(ctx, eng, "CREATE CONSTRAINT k_u FOR (n:K) REQUIRE n.k IS UNIQUE", nil); err != nil {
				t.Fatalf("CREATE CONSTRAINT: %v", err)
			}
			// ON CREATE SET is the side effect a retry must not double-apply: the
			// surviving node must carry it exactly once, from the one caller that
			// created it.
			const q = "MERGE (n:K {k:$k}) ON CREATE SET n.c = 1 RETURN n.k"

			var (
				mu       sync.Mutex
				failures []error
			)
			for r := 0; r < rounds; r++ {
				start := make(chan struct{})
				var wg sync.WaitGroup
				wg.Add(goroutines)
				for g := 0; g < goroutines; g++ {
					go func() {
						defer wg.Done()
						<-start
						if err := mergeUniqueRaceExec(ctx, eng, q, map[string]expr.Value{"k": expr.IntegerValue(r)}); err != nil {
							mu.Lock()
							failures = append(failures, fmt.Errorf("round %d: %w", r, err))
							mu.Unlock()
						}
					}()
				}
				close(start)
				wg.Wait()
			}

			if len(failures) > 0 {
				violations := 0
				for _, f := range failures {
					if errors.Is(f, exec.ErrConstraintViolation) {
						violations++
					}
				}
				t.Errorf("%d of %d concurrent autocommit MERGE calls failed (%d constraint violations); "+
					"under a UNIQUE constraint every loser must match the winner. first: %v",
					len(failures), goroutines*rounds, violations, failures[0])
			}

			res, err := eng.Run(ctx, "MATCH (n:K) RETURN n.k AS k, count(*) AS c, sum(n.c) AS s ORDER BY k", nil)
			if err != nil {
				t.Fatalf("MATCH: %v", err)
			}
			rows := drainRecords(t, res)
			if len(rows) != rounds {
				t.Fatalf("got %d distinct keys, want %d: %v", len(rows), rounds, rows)
			}
			for _, row := range rows {
				if fmtAny(row["c"]) != "1" || fmtAny(row["s"]) != "1" {
					t.Errorf("key %v: %v nodes carrying ON CREATE sum %v, want exactly one node set once",
						row["k"], row["c"], row["s"])
				}
			}
		})
	}
}
