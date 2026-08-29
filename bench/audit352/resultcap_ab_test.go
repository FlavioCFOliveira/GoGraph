package audit352_test

import (
	"fmt"
	"runtime"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/internal/planseam"
)

// ---------------------------------------------------------------------------
// SAME-PROCESS A/B for the ParallelScanProject result-budget counters.
//
// BEFORE #2649, exec.(*ParallelScanProject).overResultBudget incremented TWO
// process-shared atomics for EVERY produced row, from EVERY parallel worker:
//
//	over := op.maxRows > 0 && op.sharedRows.Add(1) > op.maxRows
//	if op.maxBytes > 0 && op.estimateRow != nil {
//	    if op.sharedBytes.Add(op.estimateRow(row)) > op.maxBytes { over = true }
//	}
//
// That function no longer exists. It was replaced by a worker-confined tally
// (chargeBudget) flushed to the shared totals in cap-relative batches
// (flushBudget), so the atomics are touched once per ~1024 rows per worker
// instead of twice per row. This harness is retained to measure what that
// recovered, and to keep measuring it: the `batched` arm is the production
// path, and `both_off` remains the ceiling it is judged against.
//
// Both caps are ON by default: resolveMaxResultRows/Bytes map the zero value
// to a finite DefaultMaxResultRows/DefaultMaxResultBytes (api.go:1463, :1503).
// The public sentinels MaxResultRowsUnlimited / MaxResultBytesUnlimited turn
// each dimension off independently, which makes the atomics toggleable
// WITHOUT touching module source — a genuine single-variable experiment in
// one process, one binary, one fixture.
//
// Arms are sub-benchmarks of a single parent, so `-count=N` interleaves them
// A,B,C,D,A,B,C,D,… rather than running all of A then all of B. Back-to-back
// blocks on this machine have turned a byte-identical control into "22
// significant rows"; interleaving is the countermeasure.
//
// ctrl_a and ctrl_b are BYTE-IDENTICAL default-configured arms. Any
// benchstat-significant difference between them is this run's noise floor,
// and no delta smaller than it may be reported as a finding.
//
// NOTE ON WHAT THIS BOUNDS: the uncapped arm removes a real safety guarantee,
// so it is NOT a proposed fix. It measures the CEILING of what a fix that
// keeps the guarantee (per-worker local counters reconciled in batches) could
// recover.
// ---------------------------------------------------------------------------

type capArm struct {
	name string
	opts cypher.EngineOptions
}

// capSandwichArms is the three-arm control sandwich the per-shape and
// per-GOMAXPROCS sweeps run: a default arm on either side of the arm under test,
// so drift over the sweep shows up as the two controls disagreeing.
//
// It is a function rather than a package-level slice so each caller gets its own
// copy and cannot mutate another sweep's arms, and so the callers can index it
// instead of ranging it by value (gocritic rangeValCopy: capArm is 192 bytes).
func capSandwichArms() []capArm {
	return []capArm{
		{"ctrl_a__default", cypher.EngineOptions{}},
		{"batched", cypher.EngineOptions{
			MaxResultRows:  cypher.DefaultMaxResultRows,
			MaxResultBytes: cypher.DefaultMaxResultBytes,
		}},
		{"both_off", cypher.EngineOptions{
			MaxResultRows:  cypher.MaxResultRowsUnlimited,
			MaxResultBytes: cypher.MaxResultBytesUnlimited,
		}},
		{"ctrl_b__default", cypher.EngineOptions{}},
	}
}

// capArms is the headline arm set. `batched` is the arm under test: the
// production configuration after #2649, in which both caps are enforced through
// per-worker tallies flushed in batches.
//
// It is deliberately spelled with EXPLICIT finite caps rather than relying on
// the zero value's mapping, so the arm keeps measuring what it claims to measure
// if a future change alters what the zero value resolves to. That makes it
// runtime-identical to the two default control arms today — which is stated here
// rather than hidden: `batched` names the path under test, and the ctrl arms
// bracket it, so the three together give both the measurement and its noise
// floor at no extra configuration.
func capArms() []capArm {
	return []capArm{
		{"ctrl_a__default", cypher.EngineOptions{}},
		{"batched", cypher.EngineOptions{
			MaxResultRows:  cypher.DefaultMaxResultRows,
			MaxResultBytes: cypher.DefaultMaxResultBytes,
		}},
		{"rows_off", cypher.EngineOptions{MaxResultRows: cypher.MaxResultRowsUnlimited}},
		{"bytes_off", cypher.EngineOptions{MaxResultBytes: cypher.MaxResultBytesUnlimited}},
		{"both_off", cypher.EngineOptions{
			MaxResultRows:  cypher.MaxResultRowsUnlimited,
			MaxResultBytes: cypher.MaxResultBytesUnlimited,
		}},
		{"ctrl_b__default", cypher.EngineOptions{}},
	}
}

// capABQuery is the shape the CPU profile attributed 17.35% of flat CPU to
// sync/atomic.(*Int64).Add under: a parallel scan that projects one property
// per node and ships every row.
const capABQuery = `MATCH (p:Person) RETURN p.salary`

// BenchmarkResultCapAB is the headline A/B. Run interleaved:
//
//	go test -run='^$' -bench='^BenchmarkResultCapAB$' -benchmem -count=10 ./bench/audit352/
func BenchmarkResultCapAB(b *testing.B) {
	// Indexed rather than ranged by value: capArm carries an EngineOptions and
	// copying one per iteration is 192 bytes the loop has no use for (gocritic
	// rangeValCopy).
	arms := capArms()
	for i := range arms {
		engine := cypher.NewEngineWithOptions(benchGraph, arms[i].opts)
		b.Run(arms[i].name, func(b *testing.B) { runQuery(b, engine, capABQuery) })
	}
}

// BenchmarkResultCapAB_Procs repeats the A/B across GOMAXPROCS levels. If the
// cost is genuinely shared-atomic contention between parallel workers, the
// gap between default and both_off must GROW with the number of workers. If
// it does not, the attribution is wrong and the finding must be withdrawn.
//
//	go test -run='^$' -bench='^BenchmarkResultCapAB_Procs$' -benchmem -count=6 ./bench/audit352/
func BenchmarkResultCapAB_Procs(b *testing.B) {
	orig := runtime.GOMAXPROCS(0)
	defer runtime.GOMAXPROCS(orig)
	for _, procs := range []int{1, 2, 4, 8, 10} {
		procs := procs
		b.Run(fmt.Sprintf("procs=%02d", procs), func(b *testing.B) {
			sandwich := capSandwichArms()
			for i := range sandwich {
				engine := cypher.NewEngineWithOptions(benchGraph, sandwich[i].opts)
				b.Run(sandwich[i].name, func(b *testing.B) {
					runtime.GOMAXPROCS(procs)
					defer runtime.GOMAXPROCS(orig)
					runQuery(b, engine, capABQuery)
				})
			}
		})
	}
}

// TestResultCapAB_Preconditions proves the A/B is single-variable: every arm
// must compile to the SAME physical plan and ship the SAME rows. Only the
// budget configuration may differ.
//
// It also proves the A/B is measuring the operator the finding is ATTRIBUTED to.
// capABQuery is a pure-property projection, and a pure-property projection over
// a simple predicate can route to the columnar filter/project chain (#2065)
// instead of exec.ParallelScanProject — in which case every number this file
// produces would be about a different operator than the one #2649 changed. The
// Explain assertion above is not sufficient on its own: this package's own
// fixture documents a shape that planned one way on a fresh engine and another
// on a warmed one, so what Explain renders is not proof of what Run builds. The
// planner's build counter is, because Run is what bumps it.
func TestResultCapAB_Preconditions(t *testing.T) {
	var firstPlan string
	for i, arm := range capArms() {
		engine := cypher.NewEngineWithOptions(benchGraph, arm.opts)
		p, err := engine.Explain(capABQuery, nil)
		if err != nil {
			t.Fatalf("%s: Explain: %v", arm.name, err)
		}
		if i == 0 {
			firstPlan = p
			t.Logf("plan (all arms):\n%s", p)
		} else if p != firstPlan {
			t.Fatalf("%s plans differently:\n%s\nwant:\n%s", arm.name, p, firstPlan)
		}

		before := planseam.ParallelScanProjectBuilds.Load()
		if n := countRows(t, engine, capABQuery); n != nodeCount {
			t.Fatalf("%s shipped %d rows, want %d", arm.name, n, nodeCount)
		}
		built := planseam.ParallelScanProjectBuilds.Load() - before
		if built == 0 {
			t.Fatalf("%s: running %q built ZERO exec.ParallelScanProject leaves, so this "+
				"A/B does not measure the operator #2649 changed (the pure-property "+
				"projection routed elsewhere, most likely the columnar chain)",
				arm.name, capABQuery)
		}
		t.Logf("%s: ParallelScanProject builds during one run: %d", arm.name, built)
	}
}

// BenchmarkResultCapAB_Shapes checks whether the effect is specific to the
// parallel scan or general. The aggregating twin produces the same rows but
// does not run under ParallelScanProject, so it should NOT move; a shape that
// moves when it has no reason to would mean the toggle is doing something
// other than what this experiment assumes.
func BenchmarkResultCapAB_Shapes(b *testing.B) {
	shapes := []struct{ name, q string }{
		{"ship_parallel", `MATCH (p:Person) RETURN p.salary`},
		{"ship_whole_node", `MATCH (p:Person) RETURN p`},
		{"agg_columnar", `MATCH (p:Person) RETURN count(p.salary) AS c`},
		{"filter_columnar", `MATCH (p:Person) WHERE p.bucket < 50 RETURN p.salary`},
		{"expand_ship", `MATCH (a:Person)-[:KNOWS]->(b:Person) RETURN b.salary`},
	}
	for _, s := range shapes {
		s := s
		b.Run(s.name, func(b *testing.B) {
			sandwich := capSandwichArms()
			for i := range sandwich {
				engine := cypher.NewEngineWithOptions(benchGraph, sandwich[i].opts)
				b.Run(sandwich[i].name, func(b *testing.B) { runQuery(b, engine, s.q) })
			}
		})
	}
}
