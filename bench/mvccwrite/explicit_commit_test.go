package mvccwrite

// explicit_commit_test.go — the explicit-transaction write path: BEGIN, one
// CREATE, COMMIT, per operation (rmp #2936).
//
// # Why this exists beside BenchmarkWriteContention
//
// Every arm of [BenchmarkWriteContention] is an autocommit statement. An explicit
// transaction takes a different route through the engine — it reads the
// constraint catalogue's generation at BEGIN, keeps a constraint contribution for
// its whole life, and enters the commit decision at COMMIT — so a cost confined to
// that route is invisible to the autocommit arms.
//
// The "unique" schema declares a UNIQUE constraint on the created property, so
// every commit reserves a value; "none" declares nothing and is the control. The
// values are globally distinct, so no two writers collide on the constraint and
// the arm measures the path rather than retry.

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// explicitCreate runs BEGIN; CREATE (:U {k: k}); COMMIT, rolling back on any
// failure.
func explicitCreate(ctx context.Context, eng *cypher.Engine, k int64) error {
	tx, err := eng.BeginTx(ctx)
	if err != nil {
		return err
	}
	if err := drainResult(tx.ExecAny("CREATE (:U {k: $k})", map[string]any{"k": k})); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		return err
	}
	return nil
}

// BenchmarkExplicitCommit reports ns/op, B/op and allocs/op of one explicit
// transaction creating one node, with and without a UNIQUE constraint on the
// created property, at one and eight writers.
func BenchmarkExplicitCommit(b *testing.B) {
	for _, schema := range []string{"none", "unique"} {
		for _, writers := range []int{1, 8} {
			b.Run(fmt.Sprintf("%s/writers=%d", schema, writers), func(b *testing.B) {
				ctx := context.Background()
				eng := cypher.NewEngine(lpg.New[string, float64](contentionAdjConfig()))
				if schema == "unique" {
					if err := drainResult(eng.RunAny(ctx, "CREATE CONSTRAINT u FOR (n:U) REQUIRE n.k IS UNIQUE", nil)); err != nil {
						b.Fatal(err)
					}
				}
				var next atomic.Int64
				perWriter := (b.N + writers - 1) / writers
				b.ReportAllocs()
				b.ResetTimer()
				contentionRetries.Store(0)
				got, err := runArm(writers, perWriter, func(_, _ int) error {
					k := next.Add(1)
					return withRetry(func() error { return explicitCreate(ctx, eng, k) })
				})
				b.StopTimer()
				if err != nil {
					b.Fatalf("writer failed: %v", err)
				}
				if got.commits == 0 {
					b.Fatal("no commits made")
				}
				b.ReportMetric(float64(contentionRetries.Load())/float64(got.commits), "retries/op")
			})
		}
	}
}
