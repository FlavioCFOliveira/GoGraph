package cypher_test

// constraint_index_seek_test.go — regression gate for task #1407:
// MATCH (n:L) WHERE n.p = $val on a (label, prop) pair with a UNIQUE
// constraint must return the correct row count, not silently zero.
//
// Root cause (pre-fix): the constraint backing hash index was created as
// an unbound indexhash.New[string]() that never received change events,
// so the planner's NodeByIndexSeek rewrite served an empty index and
// returned zero rows. The fix (commit 80255d7) makes the constraint
// backing index a bound, backfilled subscriber identical to the one
// created by CREATE INDEX.
//
// Layer: short.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// constraintIndexSeekEngine returns a fresh engine backed by a directed LPG.
func constraintIndexSeekEngine() *cypher.Engine {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	return cypher.NewEngine(g)
}

// TestConstraintIndex_EqualitySeek_ReturnsCorrectCount is the primary
// gate: after CREATE CONSTRAINT + INSERT, an equality MATCH must return
// the real row count, not 0 (task #1407).
func TestConstraintIndex_EqualitySeek_ReturnsCorrectCount(t *testing.T) {
	t.Parallel()
	eng := constraintIndexSeekEngine()
	ctx := context.Background()

	if _, err := eng.RunAny(ctx, `CREATE CONSTRAINT u ON (n:T) ASSERT n.p IS UNIQUE`, nil); err != nil {
		t.Fatalf("CREATE CONSTRAINT: %v", err)
	}
	if _, err := eng.RunAny(ctx, `CREATE (:T {p:'a'})`, nil); err != nil {
		t.Fatalf("CREATE node: %v", err)
	}

	res, err := eng.RunAny(ctx, `MATCH (n:T) WHERE n.p = 'a' RETURN count(n) AS c`, nil)
	if err != nil {
		t.Fatalf("MATCH: %v", err)
	}
	defer func() {
		if cerr := res.Close(); cerr != nil {
			t.Errorf("Close: %v", cerr)
		}
	}()
	if !res.Next() {
		t.Fatal("no result rows")
	}
	c, ok := res.Record()["c"].(expr.IntegerValue)
	if !ok {
		t.Fatalf("unexpected count type: %T", res.Record()["c"])
	}
	if int(c) != 1 {
		t.Errorf("count(n) = %d; want 1 (constraint backing index not serving correct rows)", int(c))
	}
	if err := res.Err(); err != nil {
		t.Fatalf("result error: %v", err)
	}
}

// TestConstraintIndex_EqualitySeek_MultipleNodes verifies multiple distinct
// values under the constraint are each seekable.
func TestConstraintIndex_EqualitySeek_MultipleNodes(t *testing.T) {
	t.Parallel()
	eng := constraintIndexSeekEngine()
	ctx := context.Background()

	if _, err := eng.RunAny(ctx, `CREATE CONSTRAINT u ON (n:User) ASSERT n.email IS UNIQUE`, nil); err != nil {
		t.Fatalf("CREATE CONSTRAINT: %v", err)
	}
	values := []string{"alice@example.com", "bob@example.com", "carol@example.com"}
	for _, v := range values {
		q := `CREATE (:User {email:'` + v + `'})`
		if _, err := eng.RunAny(ctx, q, nil); err != nil {
			t.Fatalf("CREATE %s: %v", v, err)
		}
	}

	for _, v := range values {
		q := `MATCH (n:User) WHERE n.email = '` + v + `' RETURN count(n) AS c`
		res, err := eng.RunAny(ctx, q, nil)
		if err != nil {
			t.Fatalf("MATCH %s: %v", v, err)
		}
		if res.Next() {
			c, ok := res.Record()["c"].(expr.IntegerValue)
			if !ok || int(c) != 1 {
				t.Errorf("email=%s: count=%v; want 1", v, res.Record()["c"])
			}
		} else {
			t.Errorf("email=%s: no result rows", v)
		}
		_ = res.Close()
	}
}

// TestConstraintIndex_EqualitySeek_MissingValue confirms that seeking a
// value absent from the constrained (label, prop) returns 0, not an error.
func TestConstraintIndex_EqualitySeek_MissingValue(t *testing.T) {
	t.Parallel()
	eng := constraintIndexSeekEngine()
	ctx := context.Background()

	if _, err := eng.RunAny(ctx, `CREATE CONSTRAINT u ON (n:T) ASSERT n.p IS UNIQUE`, nil); err != nil {
		t.Fatalf("CREATE CONSTRAINT: %v", err)
	}
	if _, err := eng.RunAny(ctx, `CREATE (:T {p:'present'})`, nil); err != nil {
		t.Fatalf("CREATE: %v", err)
	}

	res, err := eng.RunAny(ctx, `MATCH (n:T) WHERE n.p = 'absent' RETURN count(n) AS c`, nil)
	if err != nil {
		t.Fatalf("MATCH absent: %v", err)
	}
	defer func() { _ = res.Close() }()
	if res.Next() {
		c, _ := res.Record()["c"].(expr.IntegerValue)
		if int(c) != 0 {
			t.Errorf("count for absent value = %d; want 0", int(c))
		}
	}
	if err := res.Err(); err != nil {
		t.Fatalf("result error: %v", err)
	}
}
