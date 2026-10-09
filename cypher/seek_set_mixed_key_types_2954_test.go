package cypher_test

// seek_set_mixed_key_types_2954_test.go — regression gate for rmp #2954.
//
// The key-set seek (seek_set_plan.go) replaces the Selection that carries a
// disjunction of equalities on one property, so its answer is the answer. It
// probed the string hash index for every string key and SKIPPED every other
// key, on the premise that a key the index cannot hold matches nothing. That
// premise is false: a string hash index holds only the string-valued nodes, so
// a node whose property is the integer 5 is simply absent from it while
// `n.s = 5` is TRUE for it. Measured at 25999b58, `n.s = 5 OR n.s = 'v1'` lost
// the nodes holding 5 and 5.0 (openCypher 9: 5 = 5.0 is true across the numeric
// types), and `n.s = true OR n.s = 'v1'` lost the node holding true; the
// UNWIND-correlated form, whose pushed hint the seek also serves, lost them too.
//
// Each shape runs with a hash index and with a btree index, in memory and on
// the WAL engine and the engine recovered from its log (with its indexes), and
// is checked against a hand-counted oracle and against the same query on an
// identically seeded engine with no index (the scan arm).

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

const seed2954 = `UNWIND range(1, 100) AS i CREATE (:L {id: i, s: 'v' + toString(i)})`

// mixed2954 holds the non-string values the string index cannot hold, and the
// string '5', which must NOT equal the integer 5.
const mixed2954 = `CREATE (:L {id: 201, s: 5}), (:L {id: 202, s: 5.0}), (:L {id: 203, s: true}), (:L {id: 204, s: '5'}), (:M {id: 301, s: 5})`

type case2954 struct {
	name, where string
	want        []string
	// seek marks the all-string control, where the key-set seek must still fire.
	seek bool
}

var cases2954 = []case2954{
	{name: "all strings (control)", where: `n.s = 'v1' OR n.s = 'v2'`, want: []string{"1", "2"}, seek: true},
	{name: "integer and string", where: `n.s = 5 OR n.s = 'v1'`, want: []string{"1", "201", "202"}},
	{name: "string and integer", where: `n.s = 'v1' OR n.s = 5`, want: []string{"1", "201", "202"}},
	{name: "float and string", where: `n.s = 5.0 OR n.s = 'v1'`, want: []string{"1", "201", "202"}},
	{name: "boolean and string", where: `n.s = true OR n.s = 'v1'`, want: []string{"1", "203"}},
	{name: "string 5 and string", where: `n.s = '5' OR n.s = 'v1'`, want: []string{"1", "204"}},
	{name: "null, integer and string", where: `n.s = null OR n.s = 5 OR n.s = 'v3'`, want: []string{"3", "201", "202"}},
}

func run2954(t *testing.T, eng *cypher.Engine, q string) []string {
	t.Helper()
	res, err := eng.RunAny(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("%q: %v", q, err)
	}
	var out []string
	for res.Next() {
		out = append(out, canon2941(res.Record()["id"]))
	}
	if err := res.Err(); err != nil {
		t.Fatalf("%q: %v", q, err)
	}
	_ = res.Close()
	return out
}

func check2954(t *testing.T, eng, ref *cypher.Engine, kind string) {
	t.Helper()
	for _, c := range cases2954 {
		q := `MATCH (n:L) WHERE ` + c.where + ` RETURN n.id AS id ORDER BY id`
		got := run2954(t, eng, q)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: indexed engine %q\n got %v\nwant %v", c.name, q, got, c.want)
		}
		// The scan arm: the same graph with no index at all.
		if scan := run2954(t, ref, q); !slices.Equal(scan, c.want) {
			t.Errorf("%s: scan engine %q\n got %v\nwant %v", c.name, q, scan, c.want)
		}
		if c.seek && kind == "hash" {
			plan, err := eng.ExplainLogical(q, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(plan, "NodeByIndexSeekSet") {
				t.Errorf("%s: the key-set seek must still serve an all-string set:\n%s", c.name, plan)
			}
		}
	}
	// The UNWIND-correlated form: the key set reaches the seek as a pushed hint.
	const unwindQ = `UNWIND [5, 'v1', true] AS k MATCH (n:L) WHERE n.s = k RETURN n.id AS id ORDER BY id`
	want := []string{"1", "201", "202", "203"}
	if got := run2954(t, eng, unwindQ); !slices.Equal(got, want) {
		t.Errorf("UNWIND form, indexed engine:\n got %v\nwant %v", got, want)
	}
	if got := run2954(t, ref, unwindQ); !slices.Equal(got, want) {
		t.Errorf("UNWIND form, scan engine:\n got %v\nwant %v", got, want)
	}
}

// scanRef2954 seeds the fixture on an engine with no index, the scan arm.
func scanRef2954(t *testing.T) *cypher.Engine {
	t.Helper()
	ref := cypher.NewEngine(lpg.New[string, float64](adjlist.Config{}))
	run2954(t, ref, seed2954)
	run2954(t, ref, mixed2954)
	return ref
}

func TestSeekSetMixedKeyTypes_2954(t *testing.T) {
	for _, kind := range []string{"hash", "btree"} {
		ddl := `CREATE INDEX l_s FOR (n:L) ON (n.s) OPTIONS {indexType: '` + kind + `'}`
		t.Run(kind+"/memory", func(t *testing.T) {
			eng := cypher.NewEngine(lpg.New[string, float64](adjlist.Config{}))
			for _, q := range []string{seed2954, mixed2954, ddl} {
				run2954(t, eng, q)
			}
			check2954(t, eng, scanRef2954(t), kind)
		})
		t.Run(kind+"/wal/recovered", func(t *testing.T) {
			dir := t.TempDir()
			w, err := wal.Open(filepath.Join(dir, "wal"))
			if err != nil {
				t.Fatal(err)
			}
			st := txn.NewStoreWithOptions[string, float64](lpg.New[string, float64](adjlist.Config{}), w,
				txn.Options[string, float64]{Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec()})
			eng := cypher.NewEngineWithStore(st)
			// The index exists before the mixed values are written, so the
			// write-path index maintenance is exercised as well as the backfill.
			for _, q := range []string{seed2954, ddl, mixed2954} {
				run2954(t, eng, q)
			}
			check2954(t, eng, scanRef2954(t), kind)
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			rec, err := recovery.Open[string, float64](dir, recovery.Options[string, float64]{
				Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec(),
			})
			if err != nil {
				t.Fatal(err)
			}
			check2954(t, cypher.NewEngineWithOptions(rec.Graph, cypher.EngineOptions{
				RecoveredIndexes: cypher.IndexDefsFromRecovery(rec.Indexes),
			}), scanRef2954(t), kind)
		})
	}
}
