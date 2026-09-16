package parser

// strip_attribution_bench_test.go — WHAT [StripLiterals] allocates, and in
// proportion to WHAT (rmp #2844, sprint 362 parsing campaign, stage 2).
//
// # Why this exists
//
// Stage 1 (rmp #2843) established that StripLiterals is the whole warm path:
// it runs on every execution, cache hit included, and for the corpus statement
// 25_software_house_api/15 its cost (1085.0 ns, 912 B, 35 allocs) is the whole
// of PrepareWarm's (1047.0 ns, 912 B, 35 allocs), against 14.24 ns / 0 B /
// 0 allocs for a quote-free statement.
//
// "35 allocations" is not an attribution. Three different costs would produce
// that number and imply three different fixes:
//
//	per CALL      — a fixed set-up cost, which a pool or a reuse would remove;
//	per LITERAL   — proportional to what is hoisted, which is irreducible work;
//	per TOKEN     — proportional to the statement's length in identifiers,
//	                which is a scanner defect and reducible to zero.
//
// A profile says where the allocations are; only a SINGLE-VARIABLE experiment
// says what they are proportional to. This file is that experiment. Each
// generator below varies exactly ONE dimension of the input and holds the other
// two fixed, so the slope of allocs/op against that dimension is the answer.
//
// # The three dimensions
//
//	stripShapeIdents(k)   +4 lowercase identifiers per step, literals fixed at 1
//	stripShapeLiterals(k) +1 hoistable string literal per step, identifiers fixed
//	stripShapeBytes(k)    +k bytes of trailing comment, identifiers and literals fixed
//
// and one control, stripShapeQuoteFree(k), which is stripShapeIdents(k) with
// the single literal written as a parameter. It carries no quote byte at all,
// so it exercises the fast reject and nothing else: it is the floor every other
// shape is measured against.
//
// The generators emit syntactically valid openCypher, and
// TestStripShapesParse asserts it, so no shape measures a scan over text the
// module would reject.
//
// # Running it
//
//	go test -run=^$ -bench='^BenchmarkStripShape$' -benchmem \
//	    -benchtime=100ms -count=10 ./cypher/parser/
//
// and, for the per-corpus-statement allocation table that the model is fitted
// against:
//
//	go test -run='^TestStripAllocationTable$' -v ./cypher/parser/

import (
	"fmt"
	"strings"
	"testing"
)

// Sinks for this file. Named apart from stage_bench_test.go's so neither file
// can silently consume the other's result.
var (
	stripAttrString string
	stripAttrMap    map[string]string
	stripAttrBool   bool
)

// stripShapeIdents returns a statement carrying exactly ONE hoistable string
// literal and 4*k+4 lowercase identifier tokens: the base `MATCH (n:L) WHERE
// n.p0 = 'v'` contributes n, p0, n and v is inside the literal — the counted
// four are n (pattern), n and p0 (predicate) and the label L is uppercase, so
// the base is deliberately not claimed as an exact count. What IS exact is the
// STEP: every k adds the conjunct ` AND n.p<i> = n.p<i>`, which is four
// lowercase identifier tokens (n, p<i>, n, p<i>) and no literal.
//
// Only the slope is load-bearing, so the base's exact composition does not
// need to be known.
func stripShapeIdents(k int) string {
	var b strings.Builder
	b.WriteString("MATCH (n:L) WHERE n.p0 = 'v'")
	for i := 1; i <= k; i++ {
		fmt.Fprintf(&b, " AND n.p%d = n.p%d", i, i)
	}
	b.WriteString(" RETURN n")
	return b.String()
}

// stripShapeQuoteFree is stripShapeIdents with the literal written as a
// parameter. It has the same identifier count and no quote byte anywhere, so
// StripLiterals must reject it in the two IndexByte passes of the fast path
// without allocating. It is the control: whatever this costs is what a fully
// parameterised statement pays, and the difference against stripShapeIdents at
// the same k is what ONE literal costs a statement of that size.
func stripShapeQuoteFree(k int) string {
	var b strings.Builder
	b.WriteString("MATCH (n:L) WHERE n.p0 = $v")
	for i := 1; i <= k; i++ {
		fmt.Fprintf(&b, " AND n.p%d = n.p%d", i, i)
	}
	b.WriteString(" RETURN n")
	return b.String()
}

// stripShapeLiterals returns a statement whose WHERE is an IN over k+1 string
// literals. Every step adds exactly ONE hoistable literal and NO identifier:
// the list elements are literals, and the identifiers n and p are written once.
func stripShapeLiterals(k int) string {
	var b strings.Builder
	b.WriteString("MATCH (n:L) WHERE n.p IN ['v0'")
	for i := 1; i <= k; i++ {
		fmt.Fprintf(&b, ",'v%d'", i)
	}
	b.WriteString("] RETURN n")
	return b.String()
}

// stripShapeBytes returns the one-literal base with k bytes of trailing
// line comment. The scanner walks a comment byte by byte and can allocate
// nothing while doing it, so this isolates the cost of SCANNING LENGTH from
// the cost of the tokens found in it.
func stripShapeBytes(k int) string {
	return "MATCH (n:L) WHERE n.p0 = 'v' RETURN n\n//" + strings.Repeat("x", k)
}

// stripShapes is the measured grid. The k values are chosen so a slope is
// visible over more than a doubling on every dimension.
var stripShapes = []struct {
	name string
	gen  func(int) string
	ks   []int
}{
	{"identsWithOneLiteral", stripShapeIdents, []int{0, 2, 4, 8, 16, 32}},
	{"identsQuoteFree", stripShapeQuoteFree, []int{0, 2, 4, 8, 16, 32}},
	{"literals", stripShapeLiterals, []int{0, 2, 4, 8, 16, 32}},
	{"commentBytes", stripShapeBytes, []int{0, 64, 256, 1024, 4096}},
}

// BenchmarkStripShape measures StripLiterals over the single-variable grid.
// Read the allocs/op column across the k values of ONE shape: its slope is what
// the allocation is proportional to.
func BenchmarkStripShape(b *testing.B) {
	for _, sh := range stripShapes {
		b.Run(sh.name, func(b *testing.B) {
			for _, k := range sh.ks {
				q := sh.gen(k)
				b.Run(fmt.Sprintf("k%04d_bytes%04d", k, len(q)), func(b *testing.B) {
					b.ReportAllocs()
					b.SetBytes(int64(len(q)))
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						stripAttrString, stripAttrMap, stripAttrBool = StripLiterals(q)
					}
				})
			}
		})
	}
}

// BenchmarkStripCorpus measures StripLiterals over every corpus statement, so
// the shape grid's model can be checked against real statements rather than
// only against generated ones. It duplicates the "Strip" arm of
// BenchmarkFrontEndStage deliberately: that arm is part of the stage ladder and
// must keep its name for comparability with the stage-1 baseline, and this one
// is free to be run on its own with a memory profile attached.
func BenchmarkStripCorpus(b *testing.B) {
	for _, s := range loadCorpus(b) {
		q := s.Text
		b.Run(benchName(s.ID), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(q)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				stripAttrString, stripAttrMap, stripAttrBool = StripLiterals(q)
			}
		})
	}
}

// TestStripShapesParse asserts every generated shape is a statement this module
// accepts. A generator that drifted into invalid Cypher would still produce a
// number — StripLiterals is a byte scanner and never parses — and that number
// would describe text no user could send.
func TestStripShapesParse(t *testing.T) {
	for _, sh := range stripShapes {
		for _, k := range sh.ks {
			q := sh.gen(k)
			if _, err := Parse(q); err != nil {
				t.Errorf("%s k=%d does not parse: %v\n%s", sh.name, k, err, q)
			}
		}
	}
}

// TestStripShapeControlIsQuoteFree asserts the control really is the fast-reject
// path: it must carry no quote byte, and StripLiterals must report that nothing
// was hoisted. Without this the control could silently become a second literal
// arm and the "one literal costs X" difference would be meaningless.
func TestStripShapeControlIsQuoteFree(t *testing.T) {
	for _, k := range []int{0, 2, 4, 8, 16, 32} {
		q := stripShapeQuoteFree(k)
		if strings.ContainsAny(q, "'\"") {
			t.Fatalf("k=%d: the control carries a quote byte, so it does not exercise the fast reject:\n%s", k, q)
		}
		got, params, ok := StripLiterals(q)
		if ok || params != nil || got != q {
			t.Fatalf("k=%d: the control hoisted something (ok=%v, params=%v)", k, ok, params)
		}
	}
	// And the literal arm must actually hoist, or the two arms differ in
	// nothing measurable.
	for _, k := range []int{0, 2, 4, 8, 16, 32} {
		q := stripShapeIdents(k)
		if _, params, ok := StripLiterals(q); !ok || len(params) != 1 {
			t.Fatalf("k=%d: stripShapeIdents hoisted %d literals (ok=%v), want exactly 1", k, len(params), ok)
		}
	}
	for _, k := range []int{0, 2, 4, 8, 16, 32} {
		q := stripShapeLiterals(k)
		if _, params, ok := StripLiterals(q); !ok || len(params) != k+1 {
			t.Fatalf("k=%d: stripShapeLiterals hoisted %d literals (ok=%v), want %d", k, len(params), ok, k+1)
		}
	}
}

// TestStripAllocationTable prints, for every corpus statement, the allocation
// count [testing.AllocsPerRun] observes for StripLiterals together with the
// statement's size and how many literals it hoists. It asserts nothing about
// the numbers — it is an EVIDENCE EMITTER, whose output is the input to the
// allocation model fitted in the task's report — but it does assert the two
// facts that would make the table meaningless: that the corpus is non-empty and
// that every measurement is a whole number of allocations.
//
// Run it with -v; it is silent otherwise.
func TestStripAllocationTable(t *testing.T) {
	corpus := loadCorpus(t)
	t.Logf("%-32s %6s %8s %9s", "id", "bytes", "hoisted", "allocs")
	for _, s := range corpus {
		q := s.Text
		_, params, _ := StripLiterals(q)
		n := testing.AllocsPerRun(200, func() {
			stripAttrString, stripAttrMap, stripAttrBool = StripLiterals(q)
		})
		if n != float64(int64(n)) {
			t.Errorf("%s: AllocsPerRun returned a fractional count %v, which means the "+
				"measurement did not settle; the table cannot be fitted", s.ID, n)
		}
		t.Logf("%-32s %6d %8d %9.0f", s.ID, len(q), len(params), n)
	}
}
