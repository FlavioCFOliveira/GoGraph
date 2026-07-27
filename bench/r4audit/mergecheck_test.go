//go:build r4audit

package r4audit

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestMergeBindsAllMatches decides whether the round-4 report's "early exit on
// first match" recommendation for MERGE is sound. If MERGE binds EVERY matching
// node (behaving like MATCH), an early exit would silently drop rows and the
// recommendation is wrong.
func TestMergeBindsAllMatches(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	for _, q := range []string{`CREATE (:X {v: 1})`, `CREATE (:X {v: 1})`, `CREATE (:X {v: 1})`} {
		res, err := eng.RunAny(context.Background(), q, nil)
		if err != nil {
			t.Fatalf("setup: %v", err)
		}
		for res.Next() {
		}
		_ = res.Close()
	}
	res, err := eng.RunAny(context.Background(), `MERGE (n:X {v: 1}) RETURN n.v AS v`, nil)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	rows := 0
	for res.Next() {
		rows++
	}
	if e := res.Err(); e != nil {
		t.Fatalf("merge drain: %v", e)
	}
	_ = res.Close()
	fmt.Printf("three matching :X nodes exist; MERGE (n:X {v:1}) RETURN n.v yielded %d row(s)\n", rows)
	if rows == 1 {
		fmt.Println("=> MERGE binds ONE match: an early exit would be SOUND")
	} else {
		fmt.Printf("=> MERGE binds ALL %d matches: an early exit would DROP ROWS and is UNSOUND\n", rows)
	}
}
