package query_test

// index_seek_bench_test.go — empirical evidence that an index-backed property
// predicate is orders of magnitude faster than the per-node scan it replaces
// (task #1651). Compare with:
//
//	go test ./graph/query/ -run x -bench 'BenchmarkSeek' -benchmem -count=10 > new.txt
//	benchstat new.txt
//
// The Scan variant runs the query against a graph with no index manager (the
// historical path); the Index variant runs the identical query against an
// identical graph carrying a covering bound index. Both report the same match
// count (asserted once outside the timed loop), so the only difference timed is
// scan vs seek.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/graph/query"
)

const benchN = 200_000

// benchEqualityScan times the dept equality predicate with NO index: every
// candidate node is resolved and its property compared (the path #1651
// replaces).
func BenchmarkSeek_EqualityScan(b *testing.B) {
	g, c := buildEmployeeGraph(b, benchN, 1)
	e := query.New(g, c)
	pred := query.WithProperty[string, int64](fxPropDept, lpg.StringValue("Engineering"))

	want := e.Match().Vertex(query.WithLabel[string, int64](fxLabelPerson), pred).Cardinality()
	if want == 0 {
		b.Fatalf("empty match set; benchmark would be meaningless")
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		got := e.Match().Vertex(query.WithLabel[string, int64](fxLabelPerson), pred).Cardinality()
		if got != want {
			b.Fatalf("got %d, want %d", got, want)
		}
	}
}

// benchEqualityIndex times the same predicate with a covering bound hash index:
// the predicate is served by a seek + bitmap intersection.
func BenchmarkSeek_EqualityIndex(b *testing.B) {
	g, c := buildEmployeeGraph(b, benchN, 1)
	attachHashIndex(b, g, fxLabelPerson, fxPropDept, "person_dept_hash", projDeptString)
	e := query.New(g, c)
	pred := query.WithProperty[string, int64](fxPropDept, lpg.StringValue("Engineering"))

	want := e.Match().Vertex(query.WithLabel[string, int64](fxLabelPerson), pred).Cardinality()
	if want == 0 {
		b.Fatalf("empty match set; benchmark would be meaningless")
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		got := e.Match().Vertex(query.WithLabel[string, int64](fxLabelPerson), pred).Cardinality()
		if got != want {
			b.Fatalf("got %d, want %d", got, want)
		}
	}
}

// BenchmarkSeek_EqualitySingletonIndex times a unique-property equality (the
// dominant singleton shape): the seek's clone-free small-posting-list path
// returns one id with no full-bitmap clone. The key "p0" carries a unique id
// property, indexed here, so the match set is a singleton.
func BenchmarkSeek_EqualitySingletonIndex(b *testing.B) {
	g, c := buildEmployeeGraph(b, benchN, 1)
	// Give every node a unique string id property and index it.
	g.AdjList().Mapper().Walk(func(_ graph.NodeID, key string) bool {
		_ = g.SetNodeProperty(key, "uid", lpg.StringValue(key))
		return true
	})
	// Rebuild CSR is unnecessary (no edges changed); reuse c.
	attachHashIndex(b, g, fxLabelPerson, "uid", "person_uid_hash", projUIDString)
	e := query.New(g, c)
	pred := query.WithProperty[string, int64]("uid", lpg.StringValue("p0"))

	want := e.Match().Vertex(query.WithLabel[string, int64](fxLabelPerson), pred).Cardinality()
	if want != 1 {
		b.Fatalf("singleton expected, got %d", want)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		got := e.Match().Vertex(query.WithLabel[string, int64](fxLabelPerson), pred).Cardinality()
		if got != want {
			b.Fatalf("got %d, want %d", got, want)
		}
	}
}

func projUIDString(pv lpg.PropertyValue) (string, bool) {
	if pv.Kind() != lpg.PropString {
		return "", false
	}
	return pv.String()
}
