package sim

import (
	"context"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// Metric names the oracle reads. These are ALREADY-EXPORTED metrics emitted by
// the production engine via internal/metrics; the oracle adds no production
// hook. Each cypher.Run / cypher.RunInTx call emits one latency observation
// (a per-invocation count) and increments a paired ".errors" counter on the
// error path.
const (
	// metricRunInTx is the per-invocation latency observation the engine emits
	// for every write-path statement (cypher.Engine.RunInTx). Its observation
	// COUNT equals the number of write statements executed.
	metricRunInTx = "cypher.RunInTx"
	// metricRunInTxErrors counts write statements the engine rejected with an
	// error (cypher.Engine.RunInTx error path).
	metricRunInTxErrors = "cypher.RunInTx.errors"
	// metricRun is the per-invocation latency observation for read-path
	// statements (cypher.Engine.Run).
	metricRun = "cypher.Run"
	// metricRunErrors counts read statements the engine rejected with an error.
	metricRunErrors = "cypher.Run.errors"
)

// recordingBackend is a [metrics.Backend] that tallies counter increments and
// latency-observation COUNTS in memory, so a test can read the engine's
// already-exported metrics before/after a run. It is a TEST-SIDE sink installed
// via metrics.SetBackend; it adds no production hook — it only reads what the
// production code already emits.
//
// # Concurrency contract
//
// recordingBackend is safe for concurrent use (the metrics surface is global
// and may be hit from any goroutine); every method takes the mutex.
type recordingBackend struct {
	mu        sync.Mutex
	counters  map[string]uint64
	latencyN  map[string]uint64 // per-name observation count
	latencSum map[string]time.Duration
}

// newRecordingBackend builds an empty recording backend.
func newRecordingBackend() *recordingBackend {
	return &recordingBackend{
		counters:  make(map[string]uint64),
		latencyN:  make(map[string]uint64),
		latencSum: make(map[string]time.Duration),
	}
}

// IncCounter implements [metrics.Backend].
func (b *recordingBackend) IncCounter(name string, delta uint64) {
	b.mu.Lock()
	b.counters[name] += delta
	b.mu.Unlock()
}

// ObserveLatency implements [metrics.Backend]: it counts the observation (and
// sums the duration, retained for completeness) so the oracle can read an
// invocation count for a wired symbol.
func (b *recordingBackend) ObserveLatency(name string, d time.Duration) {
	b.mu.Lock()
	b.latencyN[name]++
	b.latencSum[name] += d
	b.mu.Unlock()
}

// counter returns the current value of a counter (0 if never incremented).
func (b *recordingBackend) counter(name string) uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.counters[name]
}

// latencyCount returns how many latency observations a name received.
func (b *recordingBackend) latencyCount(name string) uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.latencyN[name]
}

// MetricsSnapshot is an immutable read of the engine's exported metrics plus the
// goroutine count at one instant. The oracle takes one before and one after a
// run and asserts the deltas match the oracle's accounting and the reliability
// bounds.
type MetricsSnapshot struct {
	// RunInTxCount / RunCount are the per-invocation latency-observation counts
	// for the write and read paths.
	RunInTxCount uint64
	RunCount     uint64
	// RunInTxErrors / RunErrors are the engine error counters.
	RunInTxErrors uint64
	RunErrors     uint64
	// Goroutines is the live goroutine count at the snapshot instant.
	Goroutines int
}

// MetricsOracleResult is the verdict of a metrics-oracle check: the before/after
// snapshots, the accounting the oracle expected, and any discrepancy.
type MetricsOracleResult struct {
	// Before / After are the snapshots bracketing the run.
	Before MetricsSnapshot
	After  MetricsSnapshot
	// ExpectedWrites / ExpectedWriteErrors are the oracle's accounting of how
	// many write statements ran and how many the engine should have rejected.
	ExpectedWrites      uint64
	ExpectedWriteErrors uint64
	// Discrepancies lists every mismatch found (empty when the metrics are
	// consistent with the oracle and the reliability bounds hold).
	Discrepancies []string
}

// Consistent reports whether the metrics matched the oracle and the reliability
// bounds (no discrepancy).
func (r MetricsOracleResult) Consistent() bool { return len(r.Discrepancies) == 0 }

// String renders the result: the deltas and every discrepancy.
func (r MetricsOracleResult) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "metrics oracle: writes=%d (expected %d) writeErrors=%d (expected %d) goroutines %d->%d",
		r.After.RunInTxCount-r.Before.RunInTxCount, r.ExpectedWrites,
		r.After.RunInTxErrors-r.Before.RunInTxErrors, r.ExpectedWriteErrors,
		r.Before.Goroutines, r.After.Goroutines)
	for _, d := range r.Discrepancies {
		fmt.Fprintf(&b, "\n  DISCREPANCY: %s", d)
	}
	return b.String()
}

// MetricsOracle reads the engine's exported metrics and the goroutine count
// around a run and certifies that the observed deltas match an oracle's
// accounting (committed writes, observed errors) and the reliability bounds
// (goroutine baseline restored). It installs a test-side recording
// [metrics.Backend]; because that backend is the global metrics sink, an oracle
// must be used SERIALLY (the caller must not run two concurrent oracles or any
// other metrics-emitting work in parallel) — [NewMetricsOracle] documents this.
//
// # Concurrency contract
//
// A MetricsOracle is NOT safe for concurrent use and must be the only metrics
// consumer active for the duration of its [MetricsOracle.Snapshot] bracket.
type MetricsOracle struct {
	backend       *recordingBackend
	before, after MetricsSnapshot
}

// NewMetricsOracle installs a recording backend as the global metrics sink and
// returns an oracle over it. The caller MUST call [MetricsOracle.Restore] when
// done (typically deferred) to put the previous backend back. Because the
// metrics sink is global, the oracle and the work it brackets must run serially
// — install it, run the workload, snapshot, restore, all on one goroutine with
// no concurrent metrics-emitting work.
func NewMetricsOracle() *MetricsOracle {
	rb := newRecordingBackend()
	// metrics has no exported "current backend" getter, so we cannot read the
	// previous backend to restore it exactly; SetBackend(nil) restores the
	// documented no-op default, which is the production baseline. Restore uses
	// nil for that reason.
	metrics.SetBackend(rb)
	return &MetricsOracle{backend: rb}
}

// Snapshot reads the current exported-metric values and the live goroutine
// count into a [MetricsSnapshot].
func (o *MetricsOracle) Snapshot() MetricsSnapshot {
	return MetricsSnapshot{
		RunInTxCount:  o.backend.latencyCount(metricRunInTx),
		RunCount:      o.backend.latencyCount(metricRun),
		RunInTxErrors: o.backend.counter(metricRunInTxErrors),
		RunErrors:     o.backend.counter(metricRunErrors),
		Goroutines:    runtime.NumGoroutine(),
	}
}

// Restore reinstalls the no-op default metrics backend. It is safe to call more
// than once.
func (o *MetricsOracle) Restore() {
	metrics.SetBackend(nil)
}

// Check certifies a before/after pair against the oracle's accounting and the
// reliability bounds, returning the verdict. expectedWrites is how many
// write-path statements the workload executed; expectedWriteErrors is how many
// of those the engine should have rejected (the oracle's count of expected
// failures). goroutineSlack is the tolerance on the goroutine delta — a healthy
// run returns to its baseline, but a small positive slack absorbs the runtime's
// own bookkeeping goroutines that a single -count run may leave parked.
func (o *MetricsOracle) Check(before, after MetricsSnapshot, expectedWrites, expectedWriteErrors uint64, goroutineSlack int) MetricsOracleResult {
	res := MetricsOracleResult{
		Before:              before,
		After:               after,
		ExpectedWrites:      expectedWrites,
		ExpectedWriteErrors: expectedWriteErrors,
	}

	gotWrites := after.RunInTxCount - before.RunInTxCount
	if gotWrites != expectedWrites {
		res.Discrepancies = append(res.Discrepancies,
			fmt.Sprintf("write-statement count: metric delta %d != oracle accounting %d", gotWrites, expectedWrites))
	}
	gotErrors := after.RunInTxErrors - before.RunInTxErrors
	if gotErrors != expectedWriteErrors {
		res.Discrepancies = append(res.Discrepancies,
			fmt.Sprintf("write-error count: metric delta %d != oracle accounting %d", gotErrors, expectedWriteErrors))
	}
	if d := after.Goroutines - before.Goroutines; d > goroutineSlack {
		res.Discrepancies = append(res.Discrepancies,
			fmt.Sprintf("goroutine leak: count grew by %d (slack %d): %d -> %d", d, goroutineSlack, before.Goroutines, after.Goroutines))
	}
	sort.Strings(res.Discrepancies)
	return res
}

// MetricsRunStats is the oracle-side accounting of a metrics-bracketed run: how
// many write statements were issued and how many the engine rejected, derived
// from the run's own outcomes (the per-op committed flag). It is the ground
// truth the metric deltas are checked against.
type MetricsRunStats struct {
	// Writes is the number of write-path statements issued.
	Writes uint64
	// WriteErrors is the number of write statements the engine rejected (the
	// per-op execute reported not-committed because RunWrite returned an error).
	WriteErrors uint64
}

// RunWithMetricsOracle drives a deterministic write workload for the seed
// against a fresh in-memory engine while a [MetricsOracle] brackets it, and
// returns the verdict. It is the wired metrics-oracle check used by the swarm
// and the integration tests: after the run, the engine's exported RunInTx
// observation count and error counter must match the oracle's own count of
// write statements and rejections, and the goroutine count must return to its
// baseline.
//
// It must run SERIALLY (it installs the global metrics backend); the caller must
// not run concurrent metrics-emitting work. It restores the no-op backend before
// returning.
func RunWithMetricsOracle(ctx context.Context, seed uint64, ops int, wlFactory func(*Seed) *Workload) (MetricsOracleResult, error) {
	if ops <= 0 {
		ops = 400
	}
	if wlFactory == nil {
		wlFactory = WriteHeavyWorkload
	}

	oracle := NewMetricsOracle()
	defer oracle.Restore()

	stats, err := driveMetricsWorkload(ctx, seed, ops, wlFactory, oracle)
	if err != nil {
		return MetricsOracleResult{}, err
	}
	// A goroutine slack of 0 is the strict baseline; the in-memory engine spawns
	// no goroutines, so a clean run returns exactly to baseline.
	return oracle.lastCheck(stats, 0), nil
}

// driveMetricsWorkload runs the workload, taking the before snapshot just before
// the first op and the after snapshot just after the last, and returns the
// oracle-side write/error accounting. The before/after are stored on the oracle
// for lastCheck.
func driveMetricsWorkload(ctx context.Context, seed uint64, ops int, wlFactory func(*Seed) *Workload, oracle *MetricsOracle) (MetricsRunStats, error) {
	s := NewSeed(seed)
	g := newMetricsEngine()
	eng := NewEngineAdapter(g)
	model := NewGraphOracle()
	workload := wlFactory(s)

	oracle.before = oracle.Snapshot()
	var stats MetricsRunStats
	for i := 0; i < ops; i++ {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		actor := workload.SelectActor(s)
		op := actor.NextOp(s, model)
		if op.Kind.IsWrite() {
			stats.Writes++
			committed, runErr := writeTrackingErr(ctx, eng, op)
			if runErr {
				// A RunInTx-level error is what the engine's cypher.RunInTx.errors
				// counter records; a clean-but-not-committed drain error does not
				// touch that counter, so only RunInTx errors are counted here.
				stats.WriteErrors++
			}
			applyOpToOracle(model, op, committed)
		} else {
			_ = executeOnEngine(ctx, eng, op)
			applyOpToOracle(model, op, true)
		}
	}
	oracle.after = oracle.Snapshot()
	return stats, nil
}

// writeTrackingErr runs a write op and reports (committed, runInTxErrored).
// runInTxErrored is true exactly when RunWrite (cypher.RunInTx) returned an
// error — the condition the engine's cypher.RunInTx.errors counter records —
// distinct from a clean statement that simply drained to a not-committed result.
func writeTrackingErr(ctx context.Context, eng *EngineAdapter, op Op) (committed, runInTxErrored bool) {
	res, err := eng.RunWrite(ctx, op.Cypher, op.Params)
	if err != nil {
		return false, true
	}
	for res.Next() {
	}
	drainErr := res.Err()
	_ = res.Close()
	return drainErr == nil, false
}

// newMetricsEngine builds the fresh in-memory engine the metrics oracle drives:
// a directed simple graph, matching the simulator's non-crash engine shape.
func newMetricsEngine() *cypher.Engine {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	return cypher.NewEngine(g)
}

// lastCheck certifies the oracle's bracketed before/after against the run stats.
func (o *MetricsOracle) lastCheck(stats MetricsRunStats, goroutineSlack int) MetricsOracleResult {
	return o.Check(o.before, o.after, stats.Writes, stats.WriteErrors, goroutineSlack)
}

// CheckGoroutineBaseline certifies ONLY the reliability bound — that the live
// goroutine count returned to its baseline (within slack) — between the before
// and after snapshots. It is the metrics-oracle check that applies to the
// CONCURRENT swarm: per-run write/error counts cannot be attributed to one run
// when many workers share the global metrics sink, but a goroutine leak across
// the whole swarm IS observable and is the bound that matters there. A clean
// swarm spawns its workers and joins them all, so the count must return to
// baseline.
func (o *MetricsOracle) CheckGoroutineBaseline(before, after MetricsSnapshot, goroutineSlack int) MetricsOracleResult {
	return o.Check(before, after, after.RunInTxCount-before.RunInTxCount, after.RunInTxErrors-before.RunInTxErrors, goroutineSlack)
}

// RunSwarmWithMetricsOracle runs a swarm bracketed by a metrics oracle and
// asserts the reliability bound: after the (concurrent) swarm spawns and joins
// every worker, the live goroutine count must return to its baseline (within
// slack). It is the swarm-level wiring of the metrics oracle. Because the
// metrics sink is global it must run serially with respect to other
// metrics-emitting work; it restores the no-op backend before returning.
//
// The returned SwarmResult is the swarm's own aggregate; the MetricsOracleResult
// carries the goroutine-baseline verdict.
func RunSwarmWithMetricsOracle(ctx context.Context, sw *Swarm, goroutineSlack int) (SwarmResult, MetricsOracleResult, error) {
	oracle := NewMetricsOracle()
	defer oracle.Restore()

	before := oracle.Snapshot()
	res, err := sw.Run(ctx)
	if err != nil {
		return res, MetricsOracleResult{}, err
	}
	after := oracle.Snapshot()
	return res, oracle.CheckGoroutineBaseline(before, after, goroutineSlack), nil
}
