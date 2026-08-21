package query_test

// index_seek_bench_test.go — empirical evidence that an index-backed property
// predicate is orders of magnitude faster than the per-node scan it replaces
// (task #1651). Compare with:
//
//	go test ./graph/query/ -run '^$' -bench 'BenchmarkSeek' -benchmem -count=10 > new.txt
//	benchstat new.txt
//
// The filter is '^$' and not the more common `-run x`: `-run` takes a REGEX, so
// `x` matches every test whose name merely CONTAINS an x, which in this package
// includes TestSeek_NumericRangeResidualFilterIsExactAtTheBoundary. MEASURED —
// with `-run x` that test runs, and a failure in it aborts the run before a
// single benchmark line is produced.
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

// ----- range benchmarks (task #2600) ---------------------------------------
//
// #2600 changed how a NUMERIC range is priced, in two opposite directions, so
// both are measured rather than asserted:
//
//   - An INT64-bounded range used to be served by no index at all (seekRangeInto
//     asserted a btreeRanger[int64] that no engine-created index satisfies), so
//     it read every candidate's property. It is now narrowed by the float64
//     companion first. BenchmarkSeek_RangeNumericScan vs
//     BenchmarkSeek_RangeNumericIndex prices that.
//   - The seek is only a SUPERSET, so valueInRange now runs over what survives
//     it — work the previous float64-bounded seek skipped. The cost is a
//     function of SELECTIVITY, which is why a selective and a broad window are
//     both measured: the broad window is the worst case, where the seek removes
//     almost nothing and the residual reads almost every candidate.
//
// The string range is included as a control: it is served EXACTLY, so its
// predicate is still discharged and no residual runs. It must not move.

// benchAgeLo/benchAgeHi bound the SELECTIVE window: buildEmployeeGraph draws
// ages uniformly from [21, 65], so [30, 31] keeps roughly 2/45 of the nodes.
const (
	benchAgeLo int64 = 30
	benchAgeHi int64 = 31
	// The BROAD window covers the whole drawn range, so the seek removes nothing
	// and the residual filter pays its worst case.
	benchAgeBroadLo int64 = 21
	benchAgeBroadHi int64 = 65
)

// benchRange times one range predicate, with the index shape attach installs
// (nil for none). The match count is asserted outside the timed loop so a
// variant that silently answers differently cannot look faster.
func benchRange(
	b *testing.B,
	attach func(testing.TB, *lpg.Graph[string, int64]),
	pred query.Predicate[string, int64],
	wantNonEmpty bool,
) {
	b.Helper()
	g, c := buildEmployeeGraph(b, benchN, 1)
	if attach != nil {
		attach(b, g)
	}
	e := query.New(g, c)
	label := query.WithLabel[string, int64](fxLabelPerson)

	got := e.Match().Vertex(label, pred).Cardinality()
	if wantNonEmpty && got == 0 {
		b.Fatal("empty match set; the benchmark would be meaningless")
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if n := e.Match().Vertex(label, pred).Cardinality(); n != got {
			b.Fatalf("match count moved mid-run: %d != %d", n, got)
		}
	}
}

func BenchmarkSeek_RangeNumericScan(b *testing.B) {
	benchRange(b, nil,
		query.WithRange[string, int64](fxPropAge,
			lpg.Int64Value(benchAgeLo), lpg.Int64Value(benchAgeHi)), true)
}

func BenchmarkSeek_RangeNumericIndex(b *testing.B) {
	benchRange(b, func(tb testing.TB, g *lpg.Graph[string, int64]) {
		attachBTreeIndex(tb, g, fxLabelPerson, fxPropAge, "person_age_btree_num", projAgeNumeric)
	}, query.WithRange[string, int64](fxPropAge,
		lpg.Int64Value(benchAgeLo), lpg.Int64Value(benchAgeHi)), true)
}

func BenchmarkSeek_RangeNumericScanBroad(b *testing.B) {
	benchRange(b, nil,
		query.WithRange[string, int64](fxPropAge,
			lpg.Int64Value(benchAgeBroadLo), lpg.Int64Value(benchAgeBroadHi)), true)
}

func BenchmarkSeek_RangeNumericIndexBroad(b *testing.B) {
	benchRange(b, func(tb testing.TB, g *lpg.Graph[string, int64]) {
		attachBTreeIndex(tb, g, fxLabelPerson, fxPropAge, "person_age_btree_num", projAgeNumeric)
	}, query.WithRange[string, int64](fxPropAge,
		lpg.Int64Value(benchAgeBroadLo), lpg.Int64Value(benchAgeBroadHi)), true)
}

func BenchmarkSeek_RangeStringIndex(b *testing.B) {
	benchRange(b, func(tb testing.TB, g *lpg.Graph[string, int64]) {
		attachBTreeIndex(tb, g, fxLabelPerson, fxPropDept, "person_dept_btree", projDeptString)
	}, query.WithRange[string, int64](fxPropDept,
		lpg.StringValue("Engineering"), lpg.StringValue("Finance")), true)
}
