package cypher_test

// index_hash_numeric_companion_test.go — a hash index carries a numeric
// companion, so the DEFAULT CREATE INDEX serves a numeric point lookup
// (task #2226).
//
// WHAT WAS WRONG. The hash index stores only string keys:
// projectStringPropValue rejects every non-PropString kind. Hash is the DEFAULT
// index type, so `CREATE INDEX FOR (n:L) ON (n.age)` on an integer property
// built an index that could never hold a single entry. SHOW INDEXES still
// reported it state "ONLINE", indistinguishable from a working one, and
// `MATCH (n:L {age: 30})` silently fell back to a full label scan — 3.90 ms
// against 6.25 µs with a btree at 20 000 nodes.
//
// Results were always correct; the user simply had an index that did nothing.
// That is the defect: not a wrong answer, but a silent absence of the thing the
// user asked for, reported as present.
//
// THE FIX builds the same unified numeric companion btree (#1652) alongside a
// hash index that a btree index already got, so the equality rewrite (#2169)
// finds it via its deterministic internal name. Measurements are recorded in
// docs/benchmarks/index-key-type-2026-07-27.md.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// hashCompanionEngine seeds n :N nodes carrying a numeric `v` and a string `s`,
// then creates the named index with the given type.
func hashCompanionEngine(t *testing.T, n int, ddl string) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("n%d", i)
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(key, "N"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeProperty(key, "v", lpg.Int64Value(int64(i))); err != nil {
			t.Fatalf("SetNodeProperty v: %v", err)
		}
		if err := g.SetNodeProperty(key, "s", lpg.StringValue(fmt.Sprintf("s%d", i))); err != nil {
			t.Fatalf("SetNodeProperty s: %v", err)
		}
	}
	eng := cypher.NewEngine(g)
	if ddl != "" {
		runSetup(t, eng, ddl)
	}
	return eng
}

// companionNames returns the internal numeric companions currently registered.
func companionNames(eng *cypher.Engine) []string {
	var out []string
	for _, n := range eng.ListIndexes() {
		if strings.HasSuffix(n, "_btree_num") {
			out = append(out, n)
		}
	}
	return out
}

// TestHashIndexBuildsNumericCompanion pins that the DEFAULT index type now
// produces a companion, which is what makes a numeric point lookup indexed.
func TestHashIndexBuildsNumericCompanion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ddl  string
	}{
		{"default (hash)", `CREATE INDEX n_v FOR (n:N) ON (n.v)`},
		{"explicit hash", `CREATE INDEX n_v FOR (n:N) ON (n.v) OPTIONS {indexType: 'hash'}`},
		{"explicit btree", `CREATE INDEX n_v FOR (n:N) ON (n.v) OPTIONS {indexType: 'btree'}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eng := hashCompanionEngine(t, 32, tc.ddl)
			got := companionNames(eng)
			if len(got) != 1 || got[0] != "n_v_btree_num" {
				t.Errorf("companions = %v, want [n_v_btree_num]; without it a numeric point lookup has no index", got)
			}
		})
	}
}

// TestHashIndexNumericLookupIsIndexed asserts the observable behaviour rather
// than the presence of a structure: with the default index in place, a numeric
// point lookup returns the right row, and it does so through the companion.
//
// Correctness alone cannot distinguish an index seek from a full scan — both
// return the same row — so this pairs the result assertion with the companion
// assertion above. The timing evidence lives in the benchmark.
func TestHashIndexNumericLookupIsIndexed(t *testing.T) {
	t.Parallel()
	eng := hashCompanionEngine(t, 64, `CREATE INDEX n_v FOR (n:N) ON (n.v)`)

	for _, q := range []string{
		`MATCH (n:N {v: 7}) RETURN n.s AS name`,
		`MATCH (n:N) WHERE n.v = 7 RETURN n.s AS name`,
		`MATCH (n:N) WHERE n.v >= 7 AND n.v <= 7 RETURN n.s AS name`,
	} {
		got := collectColumn(t, eng, q, "name")
		if len(got) != 1 || got[0] != "s7" {
			t.Errorf("%s => %v, want [s7]", q, got)
		}
	}
}

// TestHashIndexNumericCrossTypeEquality pins openCypher value equality across
// the int/float divide through the companion: the unified float64 key means
// 7 and 7.0 are the same index entry, so a lookup by either finds both nodes.
func TestHashIndexNumericCrossTypeEquality(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	runSetup(t, eng, `CREATE (:N {v: 7, tag: 'int'})`)
	runSetup(t, eng, `CREATE (:N {v: 7.0, tag: 'float'})`)
	runSetup(t, eng, `CREATE (:N {v: 8, tag: 'other'})`)
	runSetup(t, eng, `CREATE INDEX n_v FOR (n:N) ON (n.v)`)

	for _, q := range []string{
		`MATCH (n:N {v: 7}) RETURN n.tag AS name`,
		`MATCH (n:N {v: 7.0}) RETURN n.tag AS name`,
	} {
		got := collectColumn(t, eng, q, "name")
		if len(got) != 2 || got[0] != "float" || got[1] != "int" {
			t.Errorf("%s => %v, want [float int]; 7 and 7.0 are the same value", q, got)
		}
	}
}

// TestHashIndexCompanionDroppedWithIndex pins the lifecycle: DROP INDEX removes
// the companion it created, so the pair does not leak.
func TestHashIndexCompanionDroppedWithIndex(t *testing.T) {
	t.Parallel()
	eng := hashCompanionEngine(t, 16, `CREATE INDEX n_v FOR (n:N) ON (n.v)`)

	if got := companionNames(eng); len(got) != 1 {
		t.Fatalf("companions before DROP = %v, want one", got)
	}
	runSetup(t, eng, `DROP INDEX n_v`)
	if got := companionNames(eng); len(got) != 0 {
		t.Errorf("companions after DROP = %v, want none; the companion leaked", got)
	}
	// The query must still answer correctly, now by scan.
	got := collectColumn(t, eng, `MATCH (n:N {v: 3}) RETURN n.s AS name`, "name")
	if len(got) != 1 || got[0] != "s3" {
		t.Errorf("post-DROP lookup => %v, want [s3]", got)
	}
}

// TestHashIndexSharedCompanionSurvivesPartialDrop is the case that makes
// indexCoverage count BOTH index kinds. Two indexes over the same
// (label, property) share one companion, so dropping one must not strip it from
// the other — which a btree-only view of coverage would have done, silently
// turning the survivor's numeric lookups back into full scans.
func TestHashIndexSharedCompanionSurvivesPartialDrop(t *testing.T) {
	t.Parallel()

	for _, second := range []string{
		`CREATE INDEX n_v2 FOR (n:N) ON (n.v)`,
		`CREATE INDEX n_v2 FOR (n:N) ON (n.v) OPTIONS {indexType: 'btree'}`,
	} {
		eng := hashCompanionEngine(t, 16, `CREATE INDEX n_v FOR (n:N) ON (n.v)`)
		runSetup(t, eng, second)
		if got := companionNames(eng); len(got) != 1 {
			t.Fatalf("two indexes on one pair share exactly one companion, got %v", got)
		}
		runSetup(t, eng, `DROP INDEX n_v`)
		if got := companionNames(eng); len(got) != 1 {
			t.Errorf("second index %q: companion was dropped with the first index, got %v; the survivor still needs it", second, got)
		}
		runSetup(t, eng, `DROP INDEX n_v2`)
		if got := companionNames(eng); len(got) != 0 {
			t.Errorf("second index %q: companion leaked after both were dropped, got %v", second, got)
		}
	}
}

// TestHashIndexStringPathUnaffected is acceptance criterion 4: no regression on
// the string path. A hash index on a string property still answers a point
// lookup, and its companion (built for the numeric case) holds no string keys,
// so it cannot shadow the hash index.
func TestHashIndexStringPathUnaffected(t *testing.T) {
	t.Parallel()
	eng := hashCompanionEngine(t, 64, `CREATE INDEX n_s FOR (n:N) ON (n.s)`)

	got := collectColumn(t, eng, `MATCH (n:N {s: 's9'}) RETURN n.s AS name`, "name")
	if len(got) != 1 || got[0] != "s9" {
		t.Errorf("string point lookup => %v, want [s9]", got)
	}
	// A range over the string property still works.
	res, err := eng.Run(context.Background(), `MATCH (n:N) WHERE n.s >= 's1' AND n.s <= 's11' RETURN count(n) AS c`, nil)
	if err != nil {
		t.Fatalf("string range: %v", err)
	}
	rows := collectRecords(t, res)
	if c, ok := rows[0]["c"].(expr.IntegerValue); !ok || c == 0 {
		t.Errorf("string range returned %v rows, want a non-zero count", rows[0]["c"])
	}
}
