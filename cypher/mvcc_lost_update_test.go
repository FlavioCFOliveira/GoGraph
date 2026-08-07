package cypher_test

// mvcc_lost_update_test.go — rmp #2324.
//
// # The defect
//
// Concurrent read-modify-write on one property LOST 46% of its committed updates.
// Four writers each issuing 100 autocommit `SET a.bal = a.bal + 1` statements, with
// every refusal retried until it succeeded, reported 400 successes and left the
// property at 216.
//
// # The mechanism
//
// The write-write conflict test sat behind a "records nothing" guard: a write whose
// value already equalled the STORED value was treated as a no-op, so no version was
// recorded and — crucially — the conflict test was skipped.
//
// That guard is sound for an idempotent write and unsound for an arithmetic one,
// because the incoming value can equal the stored one BY COINCIDENCE. A reads 1 and
// writes 2. B, whose snapshot also says 1, computes 2 as well. B's write is compared
// against the now-stored 2, judged a no-op, and accepted with no conflict test at
// all: B's statement reports SUCCESS having applied nothing, and one increment is
// gone. The stored value is exactly what a stale writer must NOT be compared against.
//
// The fix tests the chain head unconditionally, before deciding whether anything
// needs recording. It cannot cause the spurious abort the guard was added to prevent:
// a MERGE re-asserting a property it just read re-asserts over a head that IS visible
// to its own transaction, and a head that is NOT visible means somebody else changed
// the object, which must be refused.
//
// Node LABELS keep their equivalent guard (`bag.has(lid)`) and are sound with it,
// because set membership is genuinely idempotent: adding a label another transaction
// already added leaves the same state, with no arithmetic to lose.
//
// Layer: short. Both tests were verified RED against the unfixed code.

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// lostUpdateFixture seeds one :Ctr node with count 0.
func lostUpdateFixture(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	res, err := eng.RunAny(context.Background(), `CREATE (:Ctr {id:'c', n:0})`, nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := drain(res); err != nil {
		t.Fatalf("seed drain: %v", err)
	}
	return eng
}

// intColumn reads a single-row, single-column integer result. It RETURNS the stream
// error rather than failing, because a writing statement's conflict surfaces during
// the drain and is a legitimate refusal for the caller to retry.
func intColumn(res *cypher.Result, col string) (value int64, found bool, err error) {
	for res.Next() {
		if v, ok := res.Record()[col]; ok {
			switch n := v.(type) {
			case expr.IntegerValue:
				value, found = int64(n), true
			case expr.FloatValue:
				value, found = int64(n), true
			}
		}
	}
	err = res.Err()
	if cerr := res.Close(); err == nil {
		err = cerr
	}
	return value, found, err
}

const (
	lostUpdateWriters    = 4
	lostUpdatePerWriter  = 100
	lostUpdateMaxRetries = 400
)

// TestLostUpdate_ConcurrentIncrementsAreAllApplied is the acceptance shape: with
// every refusal retried until it succeeds, N*M increments must leave the counter at
// exactly N*M. Anything less is an update that a successful statement failed to
// apply.
func TestLostUpdate_ConcurrentIncrementsAreAllApplied(t *testing.T) {
	eng := lostUpdateFixture(t)
	ctx := context.Background()

	var succeeded, refused, exhausted atomic.Int64
	var wg sync.WaitGroup
	for range lostUpdateWriters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range lostUpdatePerWriter {
				applied := false
				for range lostUpdateMaxRetries {
					res, err := eng.RunAny(ctx, `MATCH (c:Ctr {id:'c'}) SET c.n = c.n + 1`, nil)
					if err == nil {
						err = drain(res)
					}
					if err == nil {
						succeeded.Add(1)
						applied = true
						break
					}
					refused.Add(1)
				}
				if !applied {
					exhausted.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	if n := exhausted.Load(); n != 0 {
		t.Fatalf("%d increments exhausted %d retries; the contention is too high for this "+
			"test to say anything about lost updates", n, lostUpdateMaxRetries)
	}

	res, err := eng.Run(ctx, `MATCH (c:Ctr {id:'c'}) RETURN c.n AS n`, nil)
	if err != nil {
		t.Fatalf("read counter: %v", err)
	}
	got, found, rerr := intColumn(res, "n")
	if rerr != nil {
		t.Fatalf("read counter: %v", rerr)
	}
	if !found {
		t.Fatal("the counter has no n property")
	}

	want := int64(lostUpdateWriters * lostUpdatePerWriter)
	if got != want {
		t.Fatalf("counter = %d, want %d — %d increments were LOST (%d statements reported "+
			"success, %d were refused and retried).\n"+
			"A statement that returns success must either have applied its update or been "+
			"refused. Losing them silently is the rmp #2324 defect: the write-write conflict "+
			"test was skipped whenever the value being written already equalled the STORED "+
			"value, which a stale writer's arithmetic can match by coincidence.",
			got, want, want-got, succeeded.Load(), refused.Load())
	}
}

// TestLostUpdate_NoTwoStatementsWriteTheSameValue is the discriminator, and it is
// the one that proves the mechanism rather than inferring it from a total.
//
// Each successful increment RETURNS the value it wrote. Two successes returning the
// same value means both computed from the same base and neither was refused — a lost
// update, mechanically. Against the unfixed code 400 successes produced only ~200
// distinct values, with one value written by five different statements.
func TestLostUpdate_NoTwoStatementsWriteTheSameValue(t *testing.T) {
	eng := lostUpdateFixture(t)
	ctx := context.Background()

	var mu sync.Mutex
	writtenBy := map[int64]int{}
	var wg sync.WaitGroup
	for range lostUpdateWriters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range lostUpdatePerWriter {
				for range lostUpdateMaxRetries {
					res, err := eng.RunAny(ctx,
						`MATCH (c:Ctr {id:'c'}) SET c.n = c.n + 1 RETURN c.n AS after`, nil)
					if err != nil {
						continue
					}
					after, found, rerr := intColumn(res, "after")
					if rerr != nil || !found {
						// A conflict surfaced during the drain: a legitimate refusal,
						// so retry rather than record anything.
						continue
					}
					mu.Lock()
					writtenBy[after]++
					mu.Unlock()
					break
				}
			}
		}()
	}
	wg.Wait()

	duplicated, worst := 0, 0
	for _, n := range writtenBy {
		if n > 1 {
			duplicated += n - 1
			if n > worst {
				worst = n
			}
		}
	}
	if duplicated != 0 {
		t.Fatalf("%d successful statements wrote a value another had already written "+
			"(worst: one value written by %d of them; %d distinct values from %d successes).\n"+
			"They computed from the same base and none was refused, which is a lost update "+
			"proven directly rather than inferred (rmp #2324).",
			duplicated, worst, len(writtenBy), lostUpdateWriters*lostUpdatePerWriter)
	}
}
