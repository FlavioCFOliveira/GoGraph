package cypher_test

// readtx_isolation_test.go — hardening gate for the 2026-06-25 round-2 audit
// (#1774): a BeginReadTx reader must never observe a PARTIAL multi-write. A
// writer creates :P nodes two-at-a-time in a single atomic transaction; a
// concurrent read-only transaction repeatedly counts them. Read-committed
// isolation guarantees the count is ALWAYS even — the reader sees both nodes of
// a pair or neither, never one. Run under -race.

import (
	"context"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

func TestReadTx_NeverObservesPartialPair(t *testing.T) {
	t.Parallel()
	eng, _ := storelessEngineWithGraph(t)
	ctx := context.Background()

	const pairs = 200
	var wg sync.WaitGroup
	done := make(chan struct{})

	// Writer: commit :P nodes in atomic pairs.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		for i := 0; i < pairs; i++ {
			res, err := eng.RunInTx(ctx, "CREATE (:P),(:P)", nil)
			if err != nil {
				t.Errorf("writer RunInTx: %v", err)
				return
			}
			for res.Next() { //nolint:revive // drain
			}
			if err := res.Err(); err != nil {
				t.Errorf("writer drain: %v", err)
				return
			}
			_ = res.Close()
		}
	}()

	// Reader: repeatedly count :P via a read-only tx; the count must stay even.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			tx, err := eng.BeginReadTx(ctx)
			if err != nil {
				t.Errorf("BeginReadTx: %v", err)
				return
			}
			res, err := tx.Exec("MATCH (n:P) RETURN count(n) AS c", nil)
			if err != nil {
				t.Errorf("read Exec: %v", err)
				_ = tx.Rollback()
				return
			}
			if res.Next() {
				if c, ok := res.Record()["c"].(expr.IntegerValue); ok && int64(c)%2 != 0 {
					t.Errorf("read observed an ODD :P count %d — a partial atomic pair was visible (isolation breach)", int64(c))
				}
			}
			_ = res.Close()
			_ = tx.Rollback()
		}
	}()

	wg.Wait()
}
