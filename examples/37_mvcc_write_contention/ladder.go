package main

// ladder.go — phase 6: the concurrency ladder (rmp #2934).
//
// The deterministic catalogue (phase 5) runs each scenario over every
// interleaving of a few steps. It cannot show what MVCC does under load: many
// goroutines, retries, out-of-order publication, version retention, and the
// derived structures (indexes, constraints, the count store) under random churn.
// This phase runs the §2 rows of docs/mvcc-scenario-catalogue.md (L01-L20) and
// MG11 as randomised concurrent arms at a ladder of goroutine counts, and holds
// every arm to the invariants the catalogue states for it.
//
// # Output
//
// The same contract as the rest of the example: bare lines carry deterministic
// verdicts (`ladder.<row> level=<n> <check>=true`), "# " lines carry volatile
// telemetry (`# ladder.<row> level=<n> <counter>=<value> ...`). Every counter the
// arm reads is printed, so a run is its own evidence.
//
// # What is a gate and what is telemetry
//
// Throughputs, latencies, conflict counts and retry streaks are telemetry. The
// gates are invariants that hold on any machine at any load: zero forbidden
// anomalies, seek = scan, no dangling edge, conservation, version memory back
// within its bound after quiescence, and two STRUCTURAL bounds on the frontier —
// at most one commit in flight and at most one waiting session per goroutine, so
// the peaks are bounded by the goroutine count, not by a fraction of a run.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/trace"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// ladderConfig is the shape of one ladder run.
type ladderConfig struct {
	// levels are the goroutine counts of the ladder.
	levels []int
	// totalOps is the number of logical operations one arm performs at one level,
	// shared among its goroutines (each runs at least minOpsPerWorker).
	totalOps int
	// soak enables the arms the catalogue reserves for the soak layer: the
	// horizon capacity cliff (L09) and the full-size parallel count (L17), and the
	// structural self-conflict streak gate (see checkDisjoint).
	soak bool
	// seed fixes the random choices of every arm.
	seed uint64
	// rows, when non-empty, restricts the run to the arms with these ids (as in
	// ladderRows: "L01", "L06", "L10", ...). Empty runs every arm.
	rows []string
}

// minOpsPerWorker is the floor of operations each goroutine performs, so the top
// of the ladder still makes every goroutine commit more than once.
const minOpsPerWorker = 4

// lagBudget bounds one visibility-lag measurement: the frontier must reach every
// commit allocated at the sampled instant within it. It equals the writers' retry
// budget: a frontier that takes longer than a writer is prepared to retry is a
// frontier the writers experience as stalled.
const lagBudget = retryBudget

func defaultLadderConfig() ladderConfig {
	return ladderConfig{levels: []int{1, 8, 64}, totalOps: 256, seed: 1}
}

// opsPerWorker is how many operations each of n goroutines performs.
func (c *ladderConfig) opsPerWorker(n int) int {
	return max(minOpsPerWorker, c.totalOps/n)
}

// ---------------------------------------------------------------------------
// Output.

// ladderOut prints the facts of one ladder run and remembers every key it printed
// and every check that failed.
type ladderOut struct {
	w io.Writer
	// prefix names the phase on every line: "ladder" (phase 6) or "durability"
	// (phase 7).
	prefix string
	mu     sync.Mutex
	fails  []string
	keys   map[string]struct{}
}

func newLadderOut(w io.Writer) *ladderOut { return newPhaseOut(w, "ladder") }

// newPhaseOut returns an output record whose lines are prefixed with prefix.
func newPhaseOut(w io.Writer, prefix string) *ladderOut {
	return &ladderOut{w: w, prefix: prefix, keys: make(map[string]struct{}, 512)}
}

// tele prints one volatile telemetry line: key/value pairs after the row and level.
func (o *ladderOut) tele(row string, level int, kv ...any) {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s.%s level=%d", o.prefix, row, level)
	o.mu.Lock()
	defer o.mu.Unlock()
	for i := 0; i+1 < len(kv); i += 2 {
		k, _ := kv[i].(string)
		fmt.Fprintf(&b, " %s=%v", k, kv[i+1])
		o.keys[row+"."+k] = struct{}{}
	}
	fmt.Fprintln(o.w, b.String())
}

// check prints one deterministic verdict and records a failure with its detail.
func (o *ladderOut) check(row string, level int, name string, ok bool, format string, args ...any) {
	o.mu.Lock()
	defer o.mu.Unlock()
	fmt.Fprintf(o.w, "%s.%s level=%d %s=%v\n", o.prefix, row, level, name, ok)
	o.keys[row+"."+name] = struct{}{}
	if !ok {
		o.fails = append(o.fails, fmt.Sprintf("%s.%s level=%d %s: %s", o.prefix, row, level, name, fmt.Sprintf(format, args...)))
	}
}

// failed returns every failed check.
func (o *ladderOut) failed() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.fails...)
}

// has reports whether key (row.counter) was printed.
func (o *ladderOut) has(key string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, ok := o.keys[key]
	return ok
}

// ---------------------------------------------------------------------------
// The driver.

// ladderRow is one arm of the ladder: it builds its own engine, runs at the given
// goroutine count, and reports through out.
type ladderRow struct {
	id  string
	run func(ctx context.Context, lc *ladderConfig, out *ladderOut, level int) error
	// once marks an arm that runs once per ladder run, at the top level, rather
	// than at every level (L09 and L17 are about a fixed shape, not a ladder).
	once bool
	// soakOnly marks an arm that the catalogue reserves for the soak layer.
	soakOnly bool
}

// ladderRows returns every arm, in catalogue order.
func ladderRows() []ladderRow {
	return []ladderRow{
		{id: "L01", run: rowHistory},    // L01, L02, L03
		{id: "L04", run: rowHotCounter}, // L04
		{id: "L05", run: rowLargeTxn},   // L05
		{id: "L06", run: rowIndexChurn}, // L06, L07
		{id: "L08", run: rowLongReader}, // L08
		{id: "L09", run: rowHorizonCliff, once: true, soakOnly: true},
		{id: "L10", run: rowWALFrontier}, // L10, L20
		{id: "L11", run: rowDisjoint},    // L11, L12
		{id: "L13", run: rowMergeStorm},  // L13, L14, MG11
		{id: "L15", run: rowHubChurn},    // L15, L16
		{id: "L17", run: rowParallelCount, once: true},
		{id: "L18", run: rowAbortHeavy}, // L18
		{id: "L19", run: rowDDLCycles},  // L19 (DD08 overlap, DD09 drain)
	}
}

// phaseLadder runs every arm at every level and returns the output record. The
// returned error is a harness failure (an arm could not be built); a failed
// invariant is recorded in out and reported by the caller.
func phaseLadder(ctx context.Context, w io.Writer, lc *ladderConfig) (*ladderOut, error) {
	out := newLadderOut(w)
	fmt.Fprintf(w, "## phase 6 — concurrency ladder (levels=%v soak=%v)\n", lc.levels, lc.soak)
	fmt.Fprintf(w, "# ladder.snapshot_capacity=%d\n", mvcc.HorizonCapacity)
	if len(lc.levels) == 0 {
		return out, nil
	}
	top := lc.levels[len(lc.levels)-1]
	for _, level := range lc.levels {
		var m0 runtime.MemStats
		runtime.ReadMemStats(&m0)
		start := time.Now()
		for _, r := range ladderRows() {
			if r.soakOnly && !lc.soak {
				continue
			}
			if r.once && level != top {
				continue
			}
			if len(lc.rows) > 0 && !slices.Contains(lc.rows, r.id) {
				continue
			}
			var err error
			trace.WithRegion(ctx, fmt.Sprintf("ladder/%s/%d", r.id, level), func() {
				err = r.run(ctx, lc, out, level)
			})
			if err != nil {
				return out, fmt.Errorf("ladder %s level %d: %w", r.id, level, err)
			}
		}
		var m1 runtime.MemStats
		runtime.ReadMemStats(&m1)
		out.tele("mem", level,
			"elapsed", time.Since(start).Round(time.Millisecond),
			"heap_alloc_bytes", m1.HeapAlloc,
			"heap_inuse_bytes", m1.HeapInuse,
			"total_alloc_bytes", m1.TotalAlloc-m0.TotalAlloc,
			"num_gc", m1.NumGC-m0.NumGC,
			"goroutines", runtime.NumGoroutine())
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Engines.

// memEngine is an in-memory engine and its graph.
type memEngine struct {
	g   *lpg.Graph[string, float64]
	eng *cypher.Engine
}

func newMemEngine() *memEngine {
	g := newGraph()
	return &memEngine{g: g, eng: cypher.NewEngine(g)}
}

func (m *memEngine) close() { _ = m.eng.Close() }

// cyRunner is the surface shared by cypher.Engine (sessionless) and
// cypher.Session (the session arm).
type cyRunner interface {
	BeginTx(ctx context.Context) (*cypher.ExplicitTx, error)
	BeginReadTx(ctx context.Context) (*cypher.ExplicitTx, error)
	Run(ctx context.Context, query string, params map[string]expr.Value) (*cypher.Result, error)
	RunInTx(ctx context.Context, query string, params map[string]expr.Value) (*cypher.Result, error)
}

// runnerFor returns the engine itself for the sessionless arm, or a fresh session.
func runnerFor(eng *cypher.Engine, session bool) cyRunner {
	if session {
		return eng.NewSession()
	}
	return eng
}

func armName(session bool) string {
	if session {
		return "session"
	}
	return "sessionless"
}

// ---------------------------------------------------------------------------
// Cypher helpers.

// P builds a parameter map from alternating names and values (int, int64,
// string, bool).
func P(kv ...any) map[string]expr.Value {
	m := make(map[string]expr.Value, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		k, _ := kv[i].(string)
		switch v := kv[i+1].(type) {
		case int:
			m[k] = expr.IntegerValue(int64(v))
		case int64:
			m[k] = expr.IntegerValue(v)
		case uint64:
			m[k] = expr.IntegerValue(int64(v)) // #nosec G115 -- workload counters, far below 2^63
		case string:
			m[k] = expr.StringValue(v)
		case bool:
			m[k] = expr.BoolValue(v)
		default:
			panic(fmt.Sprintf("P: unsupported parameter type %T", v))
		}
	}
	return m
}

// drain reads a whole result and closes it, returning every error it carried.
func drain(res *cypher.Result, err error) ([][]expr.Value, error) {
	if err != nil {
		return nil, err
	}
	var rows [][]expr.Value
	n := len(res.Columns())
	for res.Next() {
		row := make([]expr.Value, n)
		for i := range row {
			row[i] = res.ValueAt(i)
		}
		rows = append(rows, row)
	}
	rerr := res.Err()
	cerr := res.Close()
	if rerr != nil {
		return rows, rerr
	}
	return rows, cerr
}

// intAt returns rows[r][c] as an int64, or -1 when it is absent or not an integer.
func intAt(rows [][]expr.Value, r, c int) int64 {
	if r >= len(rows) || c >= len(rows[r]) {
		return -1
	}
	if v, ok := rows[r][c].(expr.IntegerValue); ok {
		return int64(v)
	}
	return -1
}

// mustRun runs one autocommit statement and fails on any error.
func mustRun(ctx context.Context, eng *cypher.Engine, q string, p map[string]expr.Value) error {
	if _, err := drain(eng.RunInTx(ctx, q, p)); err != nil {
		return fmt.Errorf("%s: %w", q, err)
	}
	return nil
}

// seekScanPair runs a statement returning the columns seek and scan.
func seekScanPair(ctx context.Context, r cyRunner, q string, p map[string]expr.Value) (seek, scan int64, err error) {
	rows, err := drain(r.Run(ctx, q, p))
	if err != nil {
		return 0, 0, err
	}
	return intAt(rows, 0, 0), intAt(rows, 0, 1), nil
}

// inTx runs body in one explicit transaction and commits it, or rolls it back
// when commit is false (the deliberate abort of the abort-heavy arms). A body
// error rolls the transaction back and is returned.
func inTx(ctx context.Context, r cyRunner, commit bool, body func(tx *cypher.ExplicitTx) error) error {
	tx, err := r.BeginTx(ctx)
	if err != nil {
		return err
	}
	if berr := body(tx); berr != nil {
		_ = tx.Rollback()
		return berr
	}
	if !commit {
		if rerr := tx.Rollback(); rerr != nil {
			return rerr
		}
		return errRolledBack
	}
	return tx.Commit()
}

// errRolledBack marks a transaction the workload rolled back on purpose.
var errRolledBack = errors.New("rolled back by the workload")

// isConflict reports whether err is a write-write refusal a client retries.
func isConflict(err error) bool {
	return errors.Is(err, cypher.ErrSerializationConflict) || errors.Is(err, cypher.ErrTxPoisoned)
}

// isTypedRefusal reports whether err is one of the refusals the engine documents
// for concurrent writers: a serialization conflict or a constraint violation.
func isTypedRefusal(err error) bool {
	return isConflict(err) || errors.Is(err, exec.ErrConstraintViolation)
}

// ---------------------------------------------------------------------------
// Retry accounting.

// txStats counts the outcomes of one arm's logical operations.
type txStats struct {
	ops         atomic.Int64 // logical operations attempted
	commits     atomic.Int64 // logical operations acknowledged
	conflicts   atomic.Int64 // refused attempts (retried or not)
	rolledBack  atomic.Int64 // deliberate rollbacks
	unrecovered atomic.Int64 // operations that exhausted the retry budget
	violations  atomic.Int64 // constraint violations (typed, not retried)
	otherErrs   atomic.Int64 // any other error
	maxStreak   atomic.Int64 // longest run of consecutive refusals of one operation
	// maxStreakNS is the longest WALL-CLOCK span of such a run: from the first
	// refusal of an operation to its last attempt. The retry budget is wall clock
	// (retryBudget), so this, not the attempt count, is what the streak gate holds
	// against it: an attempt count measures how fast a refused attempt returns.
	maxStreakNS atomic.Int64
	firstErr    atomic.Pointer[error]
	latMu       sync.Mutex
	lat         []time.Duration
}

// retry runs op until it commits, is rolled back on purpose, fails with a
// non-conflict error, or the retry budget expires. It returns the operation's
// final error (nil or errRolledBack on success).
func (s *txStats) retry(ctx context.Context, op func() error) error {
	s.ops.Add(1)
	deadline := time.Now().Add(retryBudget)
	streak := 0
	var firstRefusal time.Time
	end := func() {
		atomicMax(&s.maxStreak, int64(streak))
		if streak > 0 {
			atomicMax(&s.maxStreakNS, int64(time.Since(firstRefusal)))
		}
	}
	for {
		t0 := time.Now()
		err := op()
		switch {
		case err == nil:
			s.commits.Add(1)
			s.record(time.Since(t0))
			end()
			return nil
		case errors.Is(err, errRolledBack):
			s.rolledBack.Add(1)
			end()
			return err
		case isConflict(err):
			s.conflicts.Add(1)
			if streak == 0 {
				firstRefusal = t0
			}
			streak++
			if time.Now().After(deadline) || ctx.Err() != nil {
				s.unrecovered.Add(1)
				end()
				return err
			}
		case errors.Is(err, exec.ErrConstraintViolation):
			s.violations.Add(1)
			end()
			return err
		default:
			s.otherErrs.Add(1)
			s.firstErr.CompareAndSwap(nil, &err)
			end()
			return err
		}
	}
}

func (s *txStats) record(d time.Duration) {
	s.latMu.Lock()
	s.lat = append(s.lat, d)
	s.latMu.Unlock()
}

// latency returns p50, p99 and max of the acknowledged attempts.
func (s *txStats) latency() (p50, p99, mx time.Duration) {
	s.latMu.Lock()
	ds := append([]time.Duration(nil), s.lat...)
	s.latMu.Unlock()
	if len(ds) == 0 {
		return 0, 0, 0
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[len(ds)/2], ds[(len(ds)-1)*99/100], ds[len(ds)-1]
}

func (s *txStats) errText() string {
	if p := s.firstErr.Load(); p != nil {
		return (*p).Error()
	}
	return "none"
}

// ---------------------------------------------------------------------------
// Fan-out.

// fanOut runs fn on n goroutines and waits for all of them. It returns the first
// non-nil error any of them returned.
func fanOut(ctx context.Context, n int, fn func(ctx context.Context, id int) error) error {
	var wg sync.WaitGroup
	var first atomic.Pointer[error]
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			if err := fn(ctx, id); err != nil {
				first.CompareAndSwap(nil, &err)
			}
		}(i)
	}
	wg.Wait()
	if p := first.Load(); p != nil {
		return *p
	}
	return nil
}

// ---------------------------------------------------------------------------
// The MVCC sampler.

// sampler reads lpg.MVCCStats on a tick while an arm runs and keeps the peaks and
// the bounded-growth verdicts. It also measures VISIBILITY LAG: at a sampled
// instant it notes the newest allocated commit (Now + InFlightCommits) and times
// how long the frontier takes to reach it.
type sampler struct {
	g    *lpg.Graph[string, float64]
	stop chan struct{}
	done chan struct{}
	// ceiling enables the WithinCeiling check (off while the arm holds a long reader).
	ceiling bool

	samples         int
	peakInFlight    uint64
	peakWaiting     int64
	peakTotal       int64
	peakActive      int
	peakUnreg       int64
	maxWM           int64
	maxStale        int64
	ceilingBreaches int
	maxLag          time.Duration
	lagSamples      int
	lagTimeouts     int
	last            lpg.MVCCStats
}

// samplerTick is the sampling period. Short enough to see a burst, long enough
// that the sampler is not a competing workload.
const samplerTick = 500 * time.Microsecond

func startSampler(g *lpg.Graph[string, float64], ceiling bool) *sampler {
	s := &sampler{g: g, stop: make(chan struct{}), done: make(chan struct{}), ceiling: ceiling}
	st0 := g.MVCCStats()
	s.observe(&st0)
	go s.loop()
	return s
}

func (s *sampler) loop() {
	defer close(s.done)
	t := time.NewTicker(samplerTick)
	defer t.Stop()
	for n := 0; ; n++ {
		select {
		case <-s.stop:
			return
		case <-t.C:
		}
		st := s.g.MVCCStats()
		s.observe(&st)
		if n%4 == 0 {
			s.measureLag(&st)
		}
	}
}

func (s *sampler) observe(st *lpg.MVCCStats) {
	s.samples++
	inflight, _, _, waiting := frontierOf(st)
	s.peakInFlight = max(s.peakInFlight, inflight)
	s.peakWaiting = max(s.peakWaiting, waiting)
	s.peakTotal = max(s.peakTotal, st.Total)
	s.peakActive = max(s.peakActive, st.ActiveSnapshots)
	s.peakUnreg = max(s.peakUnreg, st.UnregisteredSnapshots)
	s.maxWM = max(s.maxWM, st.WatermarkRegressions)
	s.maxStale = max(s.maxStale, st.HorizonStaleLeaves)
	if s.ceiling && st.ActiveReaders() == 0 && !st.WithinCeiling() {
		s.ceilingBreaches++
	}
	s.last = *st
}

// measureLag times how long the frontier takes to pass every commit allocated
// at the instant st was read.
func (s *sampler) measureLag(st *lpg.MVCCStats) {
	inflight, _, _, _ := frontierOf(st)
	if inflight == 0 {
		return
	}
	target := st.Now + inflight
	t0 := time.Now()
	for s.g.MVCCStats().Now < target {
		if time.Since(t0) > lagBudget {
			s.lagTimeouts++
			break
		}
		runtime.Gosched()
	}
	s.lagSamples++
	s.maxLag = max(s.maxLag, time.Since(t0))
}

// finish stops the sampler and takes one last reading.
func (s *sampler) finish() {
	close(s.stop)
	<-s.done
	st := s.g.MVCCStats()
	s.observe(&st)
}

// report prints the frontier and growth telemetry and the structural verdicts.
// The trailing int, the arm's goroutine count, is accepted for call-site
// uniformity and is not used.
func (s *sampler) report(out *ladderOut, row string, level, _ int) {
	st := s.last
	inflight, ooo, helped, waiting := frontierOf(&st)
	out.tele(row, level,
		"samples", s.samples,
		"peak_in_flight_commits", s.peakInFlight,
		"peak_sessions_waiting", s.peakWaiting,
		"out_of_order_publications", ooo,
		"helped_publications", helped,
		"max_visibility_lag", s.maxLag,
		"lag_samples", s.lagSamples,
		"lag_timeouts", s.lagTimeouts,
		"peak_versions_total", s.peakTotal,
		"peak_active_snapshots", s.peakActive,
		"peak_unregistered_snapshots", s.peakUnreg,
		"watermark_regressions", s.maxWM,
		"horizon_stale_leaves", s.maxStale,
		"ceiling_breaches", s.ceilingBreaches,
		"versions_bound", st.Bound,
		"versions_ceiling", st.Ceiling,
		"snapshot_capacity", st.SnapshotCapacity,
		"commits", st.Write.Commits,
		"aborts", st.Write.Aborts,
		"conflicts", st.Write.Conflicts,
		"conflicts_by_store", conflictsByStore(&st.Write),
		"in_flight_commits_end", inflight,
		"sessions_waiting_end", waiting)
	out.check(row, level, "sampled", s.samples > 1, "the sampler took %d readings", s.samples)
	out.check(row, level, "watermark_regressions_zero", s.maxWM == 0, "WatermarkRegressions reached %d", s.maxWM)
	out.check(row, level, "horizon_stale_leaves_zero", s.maxStale == 0, "HorizonStaleLeaves reached %d", s.maxStale)
	if s.ceiling {
		out.check(row, level, "within_ceiling", s.ceilingBreaches == 0,
			"%d readings above the ceiling %d with no reader open", s.ceilingBreaches, st.Ceiling)
	}
	// InFlightCommits is a WINDOW — newest allocated minus the frontier — not a
	// count of commits held by goroutines: finished commits above an unfinished
	// one stay in it. Its peak therefore has no bound in the goroutine count (729
	// was measured at 256 goroutines, README.md, "Phase 6"). What is gated is that
	// the window always drains: every lag measurement below reaches its target
	// within lagBudget, and the window is zero after quiescence (reportQuiesce).
	// SessionsWaiting is a count of parked callers, at most one per goroutine.
	out.check(row, level, "visibility_lag_bounded", s.lagTimeouts == 0,
		"%d lag measurements exceeded %s (max %s)", s.lagTimeouts, lagBudget, s.maxLag)
}

// conflictsByStore renders the per-store conflict counts as name:count pairs.
func conflictsByStore(w *mvcc.WriteCounts) string {
	var parts []string
	for i, n := range w.ByStore {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%s:%d", strings.ReplaceAll(mvcc.ConflictStoreName(i), " ", "_"), n))
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ",")
}

// quiesce waits until no commit is in flight, then sweeps the vacuum to
// completion, and returns the settled reading.
func quiesce(ctx context.Context, g *lpg.Graph[string, float64]) (lpg.MVCCStats, error) {
	deadline := time.Now().Add(hangBudget)
	for {
		st := g.MVCCStats()
		inflight, _, _, waiting := frontierOf(&st)
		if inflight == 0 && waiting == 0 {
			break
		}
		if time.Now().After(deadline) {
			return st, fmt.Errorf("quiesce: %d commits still in flight after %s", inflight, hangBudget)
		}
		if err := ctx.Err(); err != nil {
			return st, err
		}
		time.Sleep(time.Millisecond)
	}
	// Two sweeps: the first can free records whose removal makes a second batch
	// (index-removal backlog behind a reclaimed version) eligible.
	g.ReclaimNow()
	g.ReclaimNow()
	return g.MVCCStats(), nil
}

// reportQuiesce prints the settled state and checks InFlightCommits = 0 and
// Total <= Bound.
func reportQuiesce(out *ladderOut, row string, level int, st *lpg.MVCCStats) {
	inflight, _, _, waiting := frontierOf(st)
	out.tele(row, level, "quiesced_versions_total", st.Total, "quiesced_active_snapshots", st.ActiveSnapshots,
		"quiesced_unregistered", st.UnregisteredSnapshots)
	out.check(row, level, "quiesced_in_flight_zero", inflight == 0 && waiting == 0,
		"after quiescence InFlightCommits=%d SessionsWaiting=%d", inflight, waiting)
	out.check(row, level, "quiesced_within_bound", st.WithinBound(),
		"after quiescence Total=%d above Bound=%d (label=%d prop=%d adj=%d edge=%d life=%d idx=%d)",
		st.Total, st.Bound, st.LabelDeltas, st.PropDeltas, st.AdjVersions, st.EdgeSideVersions,
		st.NodeLifeRecords, st.IndexRemovalBacklog)
}

// reportTx prints one arm's operation outcomes.
func reportTx(out *ladderOut, row string, level int, arm string, s *txStats, elapsed time.Duration) {
	p50, p99, mx := s.latency()
	rate := 0.0
	if elapsed > 0 {
		rate = float64(s.commits.Load()) / elapsed.Seconds()
	}
	out.tele(row, level, "arm", arm,
		"ops", s.ops.Load(), "commits", s.commits.Load(),
		"commits_per_sec", fmt.Sprintf("%.0f", rate),
		"refused_attempts", s.conflicts.Load(), "rolled_back", s.rolledBack.Load(),
		"unrecovered", s.unrecovered.Load(), "constraint_violations", s.violations.Load(),
		"other_errors", s.otherErrs.Load(),
		"longest_retry_streak", s.maxStreak.Load(), "longest_retry_streak_time", time.Duration(s.maxStreakNS.Load()),
		"commit_p50", p50, "commit_p99", p99, "commit_max", mx)
}

// storeDirFor returns a fresh directory for a WAL-backed arm under TMPDIR.
// The row, level and arm identify the call site only: the directory takes the
// literal "ex37-store-" prefix that internal/tmphygiene owns, so the temp-area
// guard sees every store this example creates.
func storeDirFor(_ string, _ int, _ string) (string, error) {
	return os.MkdirTemp("", "ex37-store-*")
}
