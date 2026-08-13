//go:build threeway

// cpu_test.go — CPU-efficiency head-to-head: GoGraph, Neo4j and Memgraph.
//
// The sibling harnesses answer other questions. threeway_test.go asks how fast
// one query is; concurrency_test.go asks how throughput moves as clients are
// added. Neither measures what this file measures: how much PROCESSOR TIME an
// engine spends to serve one operation. Throughput conflates efficiency with
// parallelism — an engine that serves twice the operations while burning four
// times the CPU is faster and less efficient at the same time, and a wall-clock
// number cannot tell those apart.
//
// # The instrument
//
// Every engine runs as a CPU-capped container, and each container's own cgroup
// v2 accounting file reports the processor time consumed inside it:
//
//	/sys/fs/cgroup/docker/<container-id>/cpu.stat -> usage_usec
//
// That counter is exact rather than sampled (unlike `docker stats`), it is
// per-cgroup so a neighbouring container's load cannot be charged to the engine
// under test, and it counts every thread the engine runs — including the
// background ones. The client mounts the hierarchy read-only and reads the
// engine's counter before and after each measured window.
//
// The instrument was validated before use, against loads of known size: 1, 2
// and 4 busy threads for five seconds each read 5.08, 10.58 and 19.62 CPU
// seconds against 5, 10 and 20 expected. It therefore tracks parallelism and
// does not saturate at one core.
//
// # The idle baseline is subtracted
//
// An engine burns CPU when nobody is asking it anything: JIT compilation,
// garbage collection, checkpointing, telemetry. Charging that to the query
// under test would report an engine's housekeeping as its per-operation cost,
// and would do so unequally — a JVM idles far more expensively than a static
// binary. So every arm first measures an IDLE window with no load offered, and
// the reported per-operation cost is
//
//	cpu_per_op = (cpu_delta - idle_rate * wall) / ops
//
// Both the gross and the net figure are reported, because the difference
// between them is itself a finding about the engine.
//
// # Why the work per query is swept
//
// A single per-operation number cannot say WHERE the processor time goes. The
// families below run the same query shape at several result sizes K, so that
// per-operation cost can be fitted as
//
//	cpu_per_op(K) = a + b*K
//
// where a is the fixed cost of accepting a query at all — parse, plan, bind,
// transaction setup, protocol — and b is the marginal cost of one more row.
// The two are different defects with different fixes, and an engine can be
// excellent at one while losing badly at the other. Fitting them separates
// "this engine is slow to start a query" from "this engine is slow per row",
// which is the question this audit exists to answer.
//
// # The plan-cache probe is causal, not correlational
//
// The literal_rotate family is a matched pair with the seek family: identical
// query semantics, identical work, differing only in whether the changing value
// arrives as a PARAMETER or is embedded in the query TEXT. An engine that
// strips literals into parameters before consulting its plan cache serves both
// arms from one cache entry and pays the same CPU for each. An engine that keys
// its cache on the raw text must re-parse and re-plan on every operation of the
// rotating arm. The gap between the two arms is therefore a direct measurement
// of what the plan cache is worth in processor time, per engine, rather than an
// inference from a profile.
//
// # Prerequisites
//
// The three engine containers of docs/concurrency-vs-neo4j-memgraph-2026-08-11.md
// §9, plus this client running INSIDE the virtual machine with the cgroup
// hierarchy mounted:
//
//	docker run --rm --network ggbench -v /sys/fs/cgroup:/hc:ro \
//	  -e CPU_CG_gograph-bolt=/hc/docker/<gograph-id>/cpu.stat \
//	  -e CPU_CG_neo4j-bolt=/hc/docker/<neo4j-id>/cpu.stat \
//	  -e CPU_CG_memgraph-bolt=/hc/docker/<memgraph-id>/cpu.stat \
//	  ... ccbench:local -test.run TestCPUEfficiency -test.v
//
// Running the client from the host measures colima's port-forward, not the
// engines (concurrency report §2.1).
package comparison

import (
	"context"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// ── configuration ────────────────────────────────────────────────────────────

var (
	cpuDuration = time.Duration(envInt("CPU_DURATION_MS", 4_000)) * time.Millisecond
	cpuWarmup   = time.Duration(envInt("CPU_WARMUP_MS", 3_000)) * time.Millisecond
	cpuIdleMs   = time.Duration(envInt("CPU_IDLE_MS", 2_000)) * time.Millisecond
	cpuRounds   = envInt("CPU_ROUNDS", 3)
	cpuPrewarm  = time.Duration(envInt("CPU_PREWARM_MS", 10_000)) * time.Millisecond
	cpuLevels   = ccParseLevels(os.Getenv("CPU_LEVELS"), []int{1})
	cpuFamilies = os.Getenv("CPU_FAMILIES")
)

// cpuTargets is ccTargets plus an optional extra engine named by CPU_EXTRA_NAME
// / CPU_EXTRA_URI. It exists so that a PROTOTYPE build can be measured in the
// same process, against the same fixture and the same client, as the build it
// is being compared with — rather than in a second run whose conditions have to
// be argued to be equivalent.
func cpuTargets() []ccTargetSpec {
	out := ccTargets()
	if n, u := os.Getenv("CPU_EXTRA_NAME"), os.Getenv("CPU_EXTRA_URI"); n != "" && u != "" {
		out = append(out, ccTargetSpec{n, u, neo4j.NoAuth(), gographDialect})
	}
	return out
}

// cpuStatPath returns the cgroup cpu.stat file for a target, or "" when the
// caller did not supply one. A target without a counter is measured for
// throughput only and reports no CPU figure, rather than silently reporting a
// zero that would read as "free".
func cpuStatPath(target string) string {
	return os.Getenv("CPU_CG_" + target)
}

// readCPUMicros returns the cumulative processor time charged to the engine's
// cgroup. It PANICS rather than returning a sentinel when the counter cannot be
// read: a harness that answers 0 for "I could not measure" publishes an
// engine's cost as free, and a zero is indistinguishable from a real reading
// once it is in a table.
func readCPUMicros(t *testing.T, path string) int64 {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cpu.stat %q: %v", path, err)
	}
	for _, ln := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(ln, "usage_usec ") {
			var v int64
			if _, err := fmt.Sscanf(ln, "usage_usec %d", &v); err != nil {
				t.Fatalf("cpu.stat %q: parse %q: %v", path, ln, err)
			}
			return v
		}
	}
	t.Fatalf("cpu.stat %q: no usage_usec line", path)
	return 0
}

// ── workloads ────────────────────────────────────────────────────────────────

// cpuWorkload is one query family. Size is the K in the fit above: what it
// means is family-specific (rows unwound, rows returned, nodes scanned), and
// each family documents it.
type cpuWorkload struct {
	Key    string
	Desc   string
	Cypher map[string]string // per-target override; "" is the default
	// Params builds the parameter map for one operation. op rises per
	// operation so an arm cannot degenerate to one hot key.
	Params func(op, k int) map[string]any
	// Query, when non-nil, builds the query text per operation. It exists for
	// the literal-rotation probe, where the changing value must land in the
	// TEXT rather than in the parameters.
	Query func(target string, op, k int) string
	// WantRows is the exact row count one operation must return, asserted on
	// every operation. An oracle that cannot fail would report throughput for
	// a query that silently returned nothing.
	WantRows func(k int) int
	Sizes    []int
	// SizeMeaning names what K counts, for the report.
	SizeMeaning string
}

func cpuWorkloads() []cpuWorkload {
	all := cpuAllWorkloads()
	if cpuFamilies == "" {
		return all
	}
	var out []cpuWorkload
	for _, w := range all {
		for _, want := range strings.Split(cpuFamilies, ",") {
			if strings.TrimSpace(want) == w.Key {
				out = append(out, w)
			}
		}
	}
	return out
}

func cpuAllWorkloads() []cpuWorkload {
	return []cpuWorkload{
		{
			// The floor. No storage is touched and no row is produced beyond
			// one constant, so whatever CPU this costs is what the engine
			// spends to accept a query at all: protocol decode, parse, plan
			// lookup, transaction setup, result encode. Every other family
			// pays this too.
			Key:         "noop",
			Desc:        "RETURN 1 — the fixed cost of accepting a query",
			Cypher:      map[string]string{"": `RETURN 1 AS x`},
			Params:      func(op, k int) map[string]any { return nil },
			WantRows:    func(k int) int { return 1 },
			Sizes:       []int{1},
			SizeMeaning: "constant",
		},
		{
			// Pure execution pipeline: rows are manufactured by the engine
			// rather than read from storage, so the slope is the cost of
			// moving one row through the operator tree and the protocol, with
			// storage access removed from the measurement entirely.
			Key:    "unwind",
			Desc:   "UNWIND range(1,K) — marginal cost of one row, no storage",
			Cypher: map[string]string{"": `UNWIND range(1, $k) AS i RETURN i`},
			Params: func(op, k int) map[string]any {
				return map[string]any{"k": int64(k)}
			},
			WantRows:    func(k int) int { return k },
			Sizes:       []int{1, 10, 100, 1000},
			SizeMeaning: "rows produced",
		},
		{
			// The matched pair for unwind, and the causal probe for row
			// DELIVERY cost. Identical rows are produced internally — the same
			// range, the same pipeline — but the aggregation collapses them to
			// a single row before they reach the client, so the query ships one
			// record instead of K.
			//
			// unwind(K) minus unwind_agg(K) is therefore the cost of DELIVERING
			// K rows, isolated from the cost of producing them. An engine that
			// accumulates records in an output buffer and flushes once pays
			// almost nothing here; an engine that flushes its writer per record
			// pays a write(2) syscall per row, and the difference will scale
			// linearly with K.
			Key:    "unwind_agg",
			Desc:   "UNWIND range(1,K) aggregated to one row — same rows produced, one row shipped",
			Cypher: map[string]string{"": `UNWIND range(1, $k) AS i RETURN count(i) AS n`},
			Params: func(op, k int) map[string]any {
				return map[string]any{"k": int64(k)}
			},
			WantRows:    func(k int) int { return 1 },
			Sizes:       []int{1, 10, 100, 1000},
			SizeMeaning: "rows produced, 1 shipped",
		},
		{
			// Indexed point lookup: the canonical OLTP operation. K is fixed
			// at one row, so this family measures fixed cost plus exactly one
			// index probe and one property read.
			Key:    "seek",
			Desc:   "indexed point lookup by string key, parameterised",
			Cypher: map[string]string{"": `MATCH (p:Person {sid: $s}) RETURN p.name AS name`},
			Params: func(op, k int) map[string]any {
				return map[string]any{"s": fmt.Sprintf("p%08d", (op*104729)%ccNodes)}
			},
			WantRows:    func(k int) int { return 1 },
			Sizes:       []int{1},
			SizeMeaning: "rows returned",
		},
		{
			// The matched pair for seek. Same semantics, same work, same
			// result — the key travels in the query TEXT instead of in the
			// parameters. An engine that strips literals before consulting its
			// plan cache is unaffected; one that keys on raw text re-parses and
			// re-plans every operation. The delta against seek is the plan
			// cache's worth, in processor time.
			Key:  "seek_literal",
			Desc: "the seek query with the key as a ROTATING LITERAL (plan-cache probe)",
			Query: func(target string, op, k int) string {
				return fmt.Sprintf(`MATCH (p:Person {sid: 'p%08d'}) RETURN p.name AS name`,
					(op*104729)%ccNodes)
			},
			Params:      func(op, k int) map[string]any { return nil },
			WantRows:    func(k int) int { return 1 },
			Sizes:       []int{1},
			SizeMeaning: "rows returned",
		},
		{
			// One hop out of an indexed seed. K is the fixture's out-degree,
			// so this family isolates the cost of traversing and materialising
			// adjacent nodes on top of the seek it also pays.
			Key:    "expand",
			Desc:   "one-hop expansion from an indexed seed, rows materialised",
			Cypher: map[string]string{"": `MATCH (p:Person {sid: $s})-[:KNOWS]->(b) RETURN b.name AS name`},
			Params: func(op, k int) map[string]any {
				return map[string]any{"s": fmt.Sprintf("p%08d", (op*104729)%ccNodes)}
			},
			WantRows:    func(k int) int { return ccDegree },
			Sizes:       []int{1},
			SizeMeaning: "one seed, out-degree rows",
		},
		{
			// Two hops, aggregated. The aggregation keeps the result at one
			// row so the protocol cost is constant, which leaves traversal as
			// the only thing that grew relative to expand.
			Key:    "expand2",
			Desc:   "two-hop expansion from an indexed seed, aggregated to one row",
			Cypher: map[string]string{"": `MATCH (a:Person {sid: $s})-[:KNOWS]->()-[:KNOWS]->(c) RETURN count(c) AS n`},
			Params: func(op, k int) map[string]any {
				return map[string]any{"s": fmt.Sprintf("p%08d", (op*104729)%ccNodes)}
			},
			WantRows:    func(k int) int { return 1 },
			Sizes:       []int{1},
			SizeMeaning: "one seed, degree^2 edges walked",
		},
		{
			// Whole-label scan, aggregated. One row returned, ccNodes nodes
			// visited, so per-operation CPU divided by ccNodes is the cost of
			// visiting one node — the scan primitive, isolated from the
			// protocol.
			Key:         "scan_count",
			Desc:        "full label scan aggregated to a count",
			Cypher:      map[string]string{"": `MATCH (n:Person) RETURN count(n) AS n`},
			Params:      func(op, k int) map[string]any { return nil },
			WantRows:    func(k int) int { return 1 },
			Sizes:       []int{1},
			SizeMeaning: "one row, ccNodes nodes visited",
		},
		{
			// Scan plus a property predicate. Against scan_count this isolates
			// the cost of evaluating one expression per visited node, which is
			// where an interpreted expression tree diverges from a compiled or
			// vectorised one.
			Key:         "scan_filter",
			Desc:        "full label scan with a property predicate, aggregated",
			Cypher:      map[string]string{"": `MATCH (n:Person) WHERE n.age > $a RETURN count(n) AS n`},
			Params:      func(op, k int) map[string]any { return map[string]any{"a": int64(40)} },
			WantRows:    func(k int) int { return 1 },
			Sizes:       []int{1},
			SizeMeaning: "one row, ccNodes nodes visited and filtered",
		},
	}
}

func (w cpuWorkload) cypherFor(target string, op, k int) string {
	if w.Query != nil {
		return w.Query(target, op, k)
	}
	if q, ok := w.Cypher[target]; ok && q != "" {
		return q
	}
	return w.Cypher[""]
}

// ── one arm ──────────────────────────────────────────────────────────────────

type cpuResult struct {
	Target string
	Work   string
	Size   int
	Conc   int

	Ops    int64
	Errors int64
	Wall   time.Duration

	// CPUMicros is the engine's processor time over the measured window.
	CPUMicros int64
	// IdleMicrosPerSec is the engine's measured consumption with no load
	// offered, used to net housekeeping out of the per-operation figure.
	IdleMicrosPerSec float64
	HaveCPU          bool
}

// GrossCPUPerOp charges every microsecond the engine burned to the operations
// it served.
func (r cpuResult) GrossCPUPerOp() float64 {
	if r.Ops == 0 {
		return math.NaN()
	}
	return float64(r.CPUMicros) / float64(r.Ops)
}

// NetCPUPerOp removes the engine's measured idle consumption over the same
// window, so that background compilation and collection are not reported as the
// cost of the query.
func (r cpuResult) NetCPUPerOp() float64 {
	if r.Ops == 0 {
		return math.NaN()
	}
	idle := r.IdleMicrosPerSec * r.Wall.Seconds()
	net := float64(r.CPUMicros) - idle
	if net < 0 {
		net = 0
	}
	return net / float64(r.Ops)
}

func (r cpuResult) OpsPerSec() float64 {
	if r.Wall <= 0 {
		return 0
	}
	return float64(r.Ops) / r.Wall.Seconds()
}

// cpuMeasureIdle samples the engine's consumption with no load offered, and
// returns it as microseconds of CPU per second of wall time.
func cpuMeasureIdle(t *testing.T, path string, d time.Duration) float64 {
	t.Helper()
	if path == "" {
		return 0
	}
	before := readCPUMicros(t, path)
	start := time.Now()
	time.Sleep(d)
	wall := time.Since(start)
	after := readCPUMicros(t, path)
	return float64(after-before) / wall.Seconds()
}

// cpuDrive offers load at the given concurrency for d, asserting the row-count
// oracle on every operation, and returns the operations completed.
func cpuDrive(ctx context.Context, sessions []neo4j.SessionWithContext, w cpuWorkload, target string, k int, d time.Duration) (int64, int64, time.Duration) {
	var ops, errs int64
	deadline := time.Now().Add(d)
	var wg sync.WaitGroup
	start := time.Now()
	for ci := range sessions {
		wg.Add(1)
		go func(ci int) {
			defer wg.Done()
			s := sessions[ci]
			for op := 0; time.Now().Before(deadline); op++ {
				cy := w.cypherFor(target, ci*1_000_003+op, k)
				n, err := ccExec(ctx, s, cy, w.Params(ci*1_000_003+op, k))
				if err != nil {
					atomic.AddInt64(&errs, 1)
					continue
				}
				if n != w.WantRows(k) {
					atomic.AddInt64(&errs, 1)
					continue
				}
				atomic.AddInt64(&ops, 1)
			}
		}(ci)
	}
	wg.Wait()
	return ops, errs, time.Since(start)
}

// cpuRunArm warms the engine at the arm's own concurrency, measures its idle
// consumption, then opens the measured window.
func cpuRunArm(ctx context.Context, t *testing.T, d neo4j.DriverWithContext, spec ccTargetSpec, w cpuWorkload, k, conc int) cpuResult {
	t.Helper()
	sessions := make([]neo4j.SessionWithContext, conc)
	for i := range sessions {
		sessions[i] = d.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	}
	defer func() {
		for _, s := range sessions {
			_ = s.Close(ctx)
		}
	}()

	// Warm at this arm's own concurrency and query shape. A JVM that has not
	// compiled this shape reports its interpreter as the engine.
	_, _, _ = cpuDrive(ctx, sessions, w, spec.Name, k, cpuWarmup)

	path := cpuStatPath(spec.Name)
	res := cpuResult{Target: spec.Name, Work: w.Key, Size: k, Conc: conc, HaveCPU: path != ""}

	// Idle is measured AFTER the warm-up and immediately before the window, so
	// it reflects the housekeeping state the measured window actually runs in
	// rather than a cold engine's.
	res.IdleMicrosPerSec = cpuMeasureIdle(t, path, cpuIdleMs)

	var before int64
	if path != "" {
		before = readCPUMicros(t, path)
	}
	ops, errs, wall := cpuDrive(ctx, sessions, w, spec.Name, k, cpuDuration)
	if path != "" {
		res.CPUMicros = readCPUMicros(t, path) - before
	}
	res.Ops, res.Errors, res.Wall = ops, errs, wall
	return res
}

// ── the sweep ────────────────────────────────────────────────────────────────

func TestCPUEfficiency(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Minute)
	defer cancel()

	targets := cpuTargets()
	if len(targets) == 0 {
		t.Skip("no targets selected")
	}
	works := cpuWorkloads()

	var all []cpuResult
	for _, spec := range targets {
		d, err := ccNewDriver(ctx, spec, 256)
		if err != nil {
			t.Fatalf("%s: %v", spec.Name, err)
		}

		load := ccLoad(ctx, t, spec, d)
		nodes := ccCount(ctx, t, d, `MATCH (n) RETURN count(n) AS n`)
		edges := ccCount(ctx, t, d, `MATCH ()-[r]->() RETURN count(r) AS n`)
		t.Logf("%s: loaded in %v — %d nodes, %d edges, cgroup=%q",
			spec.Name, load.Round(time.Millisecond), nodes, edges, cpuStatPath(spec.Name))
		if nodes != int64(ccNodes) {
			t.Fatalf("%s: fixture has %d nodes, want %d", spec.Name, nodes, ccNodes)
		}
		if edges != int64(ccNodes*ccDegree) {
			t.Fatalf("%s: fixture has %d edges, want %d", spec.Name, edges, ccNodes*ccDegree)
		}

		// One long prewarm per target, so the first measured arm is not the
		// one paying for JIT compilation and first page-cache population.
		{
			s := []neo4j.SessionWithContext{d.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})}
			warm := cpuWorkload{
				Key:      "prewarm",
				Cypher:   map[string]string{"": `MATCH (p:Person {sid: $s}) RETURN p.name AS name`},
				Params:   func(op, k int) map[string]any { return map[string]any{"s": fmt.Sprintf("p%08d", (op*104729)%ccNodes)} },
				WantRows: func(k int) int { return 1 },
			}
			n, _, _ := cpuDrive(ctx, s, warm, spec.Name, 1, cpuPrewarm)
			t.Logf("%s: prewarm %d ops", spec.Name, n)
			_ = s[0].Close(ctx)
		}

		for _, w := range works {
			for _, k := range w.Sizes {
				for _, conc := range cpuLevels {
					// Rounds are interleaved at the arm level and the MEDIAN
					// is taken, because a single window can catch a garbage
					// collection or a compilation pause that belongs to no
					// operation in particular.
					var rounds []cpuResult
					for r := 0; r < cpuRounds; r++ {
						res := cpuRunArm(ctx, t, d, spec, w, k, conc)
						if res.Errors > 0 {
							t.Errorf("%s/%s/K=%d/c=%d: %d operations failed the row-count oracle",
								spec.Name, w.Key, k, conc, res.Errors)
						}
						rounds = append(rounds, res)
					}
					med := cpuMedian(rounds)
					all = append(all, med)
					t.Logf("%-14s %-13s K=%-5d c=%-3d  %8.0f ops/s  gross %8.1f µs/op  net %8.1f µs/op  idle %6.0f µs/s",
						med.Target, med.Work, med.Size, med.Conc, med.OpsPerSec(),
						med.GrossCPUPerOp(), med.NetCPUPerOp(), med.IdleMicrosPerSec)
				}
			}
		}
		_ = d.Close(ctx)
	}

	cpuReport(t, all)
}

// cpuMedian returns the round whose net per-operation CPU is the median, so
// that the reported arm is one that actually happened rather than an average of
// arms that did not.
func cpuMedian(rs []cpuResult) cpuResult {
	if len(rs) == 0 {
		return cpuResult{}
	}
	out := append([]cpuResult(nil), rs...)
	sort.Slice(out, func(i, j int) bool { return out[i].NetCPUPerOp() < out[j].NetCPUPerOp() })
	return out[len(out)/2]
}

// ── report ───────────────────────────────────────────────────────────────────

func cpuReport(t *testing.T, all []cpuResult) {
	t.Helper()
	var b strings.Builder

	fmt.Fprintf(&b, "\n\n=== CPU EFFICIENCY: processor time per operation ===\n")
	fmt.Fprintf(&b, "nodes=%d degree=%d window=%v rounds=%d\n\n", ccNodes, ccDegree, cpuDuration, cpuRounds)

	// Group by workload+size so engines sit side by side.
	type key struct {
		work string
		size int
		conc int
	}
	byKey := map[key][]cpuResult{}
	var order []key
	for _, r := range all {
		k := key{r.Work, r.Size, r.Conc}
		if _, seen := byKey[k]; !seen {
			order = append(order, k)
		}
		byKey[k] = append(byKey[k], r)
	}

	fmt.Fprintf(&b, "| workload | K | conc | engine | ops/s | gross µs CPU/op | net µs CPU/op | idle µs/s | vs best |\n")
	fmt.Fprintf(&b, "|---|---|---|---|---|---|---|---|---|\n")
	for _, k := range order {
		rs := byKey[k]
		best := math.Inf(1)
		for _, r := range rs {
			if r.HaveCPU && r.NetCPUPerOp() < best {
				best = r.NetCPUPerOp()
			}
		}
		for _, r := range rs {
			ratio := "—"
			if r.HaveCPU && !math.IsInf(best, 1) && best > 0 {
				ratio = fmt.Sprintf("%.2fx", r.NetCPUPerOp()/best)
			}
			gross, net, idle := "—", "—", "—"
			if r.HaveCPU {
				gross = fmt.Sprintf("%.1f", r.GrossCPUPerOp())
				net = fmt.Sprintf("%.1f", r.NetCPUPerOp())
				idle = fmt.Sprintf("%.0f", r.IdleMicrosPerSec)
			}
			fmt.Fprintf(&b, "| %s | %d | %d | %s | %.0f | %s | %s | %s | %s |\n",
				k.work, k.size, k.conc, r.Target, r.OpsPerSec(), gross, net, idle, ratio)
		}
	}

	// The fit: fixed cost versus marginal cost, for families swept over K.
	fmt.Fprintf(&b, "\n\n=== FIT: cpu_per_op(K) = a + b*K ===\n")
	fmt.Fprintf(&b, "a is the fixed cost of accepting a query; b the marginal cost of one row.\n\n")
	fmt.Fprintf(&b, "| workload | conc | engine | a (µs CPU/query) | b (µs CPU/row) | r2 | points |\n")
	fmt.Fprintf(&b, "|---|---|---|---|---|---|---|\n")

	type fitKey struct {
		work   string
		target string
		conc   int
	}
	byFit := map[fitKey][]cpuResult{}
	var fitOrder []fitKey
	for _, r := range all {
		if !r.HaveCPU {
			continue
		}
		fk := fitKey{r.Work, r.Target, r.Conc}
		if _, seen := byFit[fk]; !seen {
			fitOrder = append(fitOrder, fk)
		}
		byFit[fk] = append(byFit[fk], r)
	}
	for _, fk := range fitOrder {
		rs := byFit[fk]
		if len(rs) < 3 {
			continue // a two-point "fit" is a subtraction wearing a hat
		}
		xs := make([]float64, 0, len(rs))
		ys := make([]float64, 0, len(rs))
		for _, r := range rs {
			xs = append(xs, float64(r.Size))
			ys = append(ys, r.NetCPUPerOp())
		}
		a, bb, r2 := cpuLinFit(xs, ys)
		fmt.Fprintf(&b, "| %s | %d | %s | %.1f | %.4f | %.4f | %d |\n",
			fk.work, fk.conc, fk.target, a, bb, r2, len(rs))
	}

	t.Log(b.String())
}

// cpuLinFit is an ordinary least-squares fit of y = a + b*x, returning the
// intercept, the slope and the coefficient of determination. r2 is reported so
// that a fit which does not describe the data cannot be quoted as though it
// did.
func cpuLinFit(xs, ys []float64) (a, b, r2 float64) {
	n := float64(len(xs))
	if n < 2 {
		return math.NaN(), math.NaN(), math.NaN()
	}
	var sx, sy, sxx, sxy float64
	for i := range xs {
		sx += xs[i]
		sy += ys[i]
		sxx += xs[i] * xs[i]
		sxy += xs[i] * ys[i]
	}
	den := n*sxx - sx*sx
	if den == 0 {
		return math.NaN(), math.NaN(), math.NaN()
	}
	b = (n*sxy - sx*sy) / den
	a = (sy - b*sx) / n
	mean := sy / n
	var ssTot, ssRes float64
	for i := range xs {
		pred := a + b*xs[i]
		ssTot += (ys[i] - mean) * (ys[i] - mean)
		ssRes += (ys[i] - pred) * (ys[i] - pred)
	}
	if ssTot == 0 {
		return a, b, math.NaN()
	}
	return a, b, 1 - ssRes/ssTot
}
