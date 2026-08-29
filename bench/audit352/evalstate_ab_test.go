package audit352_test

// evalstate_ab_test.go — the wall-clock and allocation A/B for rmp #2653
// (replacing the RowContext sentinel that [expr.EvalWith] smuggled its
// per-evaluation state through with an explicit `*evalCallState` parameter).
//
// # What the change removes, per evaluated row
//
// Measured at HEAD with an exact counter compiled into EvalWith and into all
// three sentinel readers, on this package's 120 000-node benchGraph:
//
//	shape                                        rows   EvalWith  sentinelReads
//	MATCH (p:Person) RETURN p.bucket           120000     120000              0
//	MATCH (p:Person) RETURN p.bucket + 1 AS b  120000     120000              0
//	MATCH (p:Person) RETURN DISTINCT p.bucket      100     120000              0
//	MATCH (p:Person) RETURN p.firstName
//	                 ORDER BY p.firstName LIMIT 10  10     120000              0
//	MATCH (p:Person) RETURN p                  120000          0              0   ← control
//
// The reader count is the finding: on every one of these shapes the sentinel is
// written and erased without ever being read. The removed work per evaluated row
// is therefore exactly three string-keyed map operations on the same 18-byte
// NUL-bracketed constant — one mapaccess2_faststr (the save), one
// mapassign_faststr (the install), one mapdelete_faststr (the erase) — plus the
// hash of that constant each one pays.
//
// # Why this A/B is CROSS-BINARY and not same-process
//
// Same reason as schemawalk_ab_test.go: the sentinel and its three extract
// helpers are DELETED, so a process built from the converted source has no
// second arm to select. The arms are two binaries run ALTERNATELY (a/b/a/b…),
// and the confound a single binary would have removed — code layout, ASLR,
// build nondeterminism — is instead MEASURED, by taking the noise floor between
// two independent builds of the SAME source.
//
//	# noise floor: same source, two builds, interleaved
//	go test -c -o /tmp/ef1.test ./bench/audit352/
//	go test -c -o /tmp/ef2.test ./bench/audit352/
//	for i in $(seq 1 6); do
//	  for x in ef1 ef2; do
//	    /tmp/$x.test -test.run='^$' -test.bench=BenchmarkEvalCallState \
//	      -test.benchtime=1s -test.count=1 -test.benchmem >> /tmp/floor.$x.txt
//	  done
//	done
//	benchstat /tmp/floor.ef1.txt /tmp/floor.ef2.txt
//
// # The null control
//
// `whole_node` (`MATCH (p:Person) RETURN p`) is the NULL CONTROL and it is
// load-bearing, not decorative: it must NOT move. It holds everything else
// fixed — the same fixture, the same 120 000 rows, the same
// `Project -> ParallelScanProject` plan as `scan_prop` — and differs only in
// that a whole-node projection is served without ever entering the expression
// evaluator. The counter above measures that directly: 0 EvalWith calls against
// scan_prop's 120 000. If whole_node moves, the reading is machine drift and
// every other number here is void.
//
// The columnar shapes (`WHERE p.bucket < 50 RETURN p.firstName`) were rejected
// as controls despite also measuring 0 EvalWith calls: they plan
// ColumnarProject/ColumnarFilter, a different program, so a move there could not
// be told apart from a columnar-path change.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
)

// evalCallStateShapes all run against the shared 120 000-node benchGraph, so no
// arm can differ from another by its fixture.
var evalCallStateShapes = []struct {
	name  string
	query string
	// evalWith is the exact number of [expr.EvalWith] entries one run of the
	// query makes, measured at HEAD with a compiled-in counter. It is recorded
	// here as the attribution: the per-row work this change removes is paid
	// exactly this many times.
	evalWith int
	// rows is the number of rows the query ships, asserted before the timed
	// loop so an arm that silently ran a different workload fails loudly.
	rows int
}{
	// The scan shape: one property read projected per row, schema width 1, so
	// the sentinel was half of the row map's keys and half of its string-keyed
	// map traffic.
	{"scan_prop", `MATCH (p:Person) RETURN p.bucket`, 120_000, 120_000},
	// The same scan with an arithmetic projection, so the evaluator recurses
	// once more per row under the same single EvalWith entry.
	{"scan_expr", `MATCH (p:Person) RETURN p.bucket + 1 AS b`, 120_000, 120_000},
	// The sort shape: 120 000 evaluations feeding a Top that ships 10 rows, so
	// per-row evaluation cost is measured with result marshalling held near
	// zero.
	{"sort_top", `MATCH (p:Person) RETURN p.firstName ORDER BY p.firstName LIMIT 10`, 120_000, 10},
	// 120 000 evaluations feeding a Distinct that ships 100 rows.
	{"distinct", `MATCH (p:Person) RETURN DISTINCT p.bucket`, 120_000, 100},
	// NULL CONTROL — same plan, same row count, zero EvalWith entries.
	{"whole_node", `MATCH (p:Person) RETURN p`, 0, 120_000},
}

// BenchmarkEvalCallState drives each shape on a shared warm engine. runQuery
// brackets every arm with an Explain equality check, so a plan that drifted
// mid-measurement fails the benchmark rather than silently averaging two
// programs.
func BenchmarkEvalCallState(b *testing.B) {
	engine := cypher.NewEngine(benchGraph)
	for _, s := range evalCallStateShapes {
		s := s
		b.Run(s.name, func(b *testing.B) {
			if got := countRows(b, engine, s.query); got != s.rows {
				b.Fatalf("%s: shipped %d rows, harness assumes %d", s.name, got, s.rows)
			}
			runQuery(b, engine, s.query)
		})
	}
}
