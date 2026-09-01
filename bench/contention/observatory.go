// Package contention is the committed contention observatory for the GoGraph
// Optimization Laboratory (rmp sprint 353, task #2678).
//
// # Why it exists
//
// Before this package, nothing in the repository enabled Go's contention
// profilers. No source file called [runtime.SetMutexProfileFraction] or
// [runtime.SetBlockProfileRate]; the only traces were a comment in
// bench/audit352/gctax_test.go and prose in docs/ describing ad-hoc
// `go test -mutexprofile` invocations that were never committed. Contention
// was therefore inferred from the SHAPE of a throughput curve and never
// attributed to a lock. Compliance Mandate 3 requires the opposite: readiness
// for extreme concurrency is "proven, never presumed".
//
// This package turns that into a repeatable instrument.
//
// # The two-window design, and why it is not one window
//
// Mutex and block profiling perturb the very thing they measure. At full rate
// the runtime timestamps every lock handoff and every blocking event, which
// inflates wall-clock and can REORDER contention: a lock that is hot without
// the profiler may look cool with it, because the profiler's own bookkeeping
// changes the arrival pattern. A single profiled run therefore cannot supply
// both the effect and the cause.
//
// So every measurement runs two windows over the identical workload:
//
//   - the UNPROFILED window ([WindowEffect]) supplies the effect — throughput,
//     p50, p99. These are the numbers that may be quoted for scaling.
//   - the PROFILED window ([WindowProbe]) supplies the cause — mutex and block
//     attribution per lock call site. Its throughput is recorded but must NEVER
//     be quoted as the module's throughput; it is the probe's throughput.
//
// Reading a scaling claim off the profiled window is the classic error this
// design exists to prevent.
//
// # One fresh process per WINDOW
//
// Each window runs in its own child process. There are two independent reasons,
// and both were established by measurement rather than by argument.
//
// The first is profile cumulativity. The mutex and block profiles accumulate
// for the lifetime of a process and the runtime exposes no way to reset them.
// Two workloads measured in one process contaminate each other, and the second
// inherits the first one's samples. An early revision ran the instrument's own
// controls in the shared test process, and the negative control caught it by
// "detecting" contention in a fixture that has no shared state at all.
//
// The second is heap warmth, and it is why a process per MEASUREMENT was not
// enough. When both windows ran in one child, the unprofiled window always ran
// first and paid every first-touch cost — plan compilation, heap growth, page
// faults, pool population — while the profiled window inherited all of it warm,
// along with a raised GC goal. Measured on cypher-write-mem at level 16, over
// six fresh processes, the PROFILED window was FASTER in five of the six, by
// about 8% at the median. That is not a credible physical result; it is the
// ordering confound swamping the effect the design set out to expose, and it
// would have been published as a probe_slowdown below 1.0 — the instrument
// claiming that turning full-rate profiling on speeds the module up.
//
// A warm-up pass narrows that gap but cannot close it, because the second
// window still inherits the whole of the first window's run on top of any
// warm-up. Alternating the order merely averages two different confounds. So
// the same principle the package already applies to profile state is applied to
// heap state: one window, one process, no inheritance.
//
// # Concurrency
//
// [Observe] is NOT safe for concurrent use and must not be called from two
// goroutines: it mutates the process-global profiling rates. It is called once
// per child process, which is exactly how the sweep drives it.
package contention

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// levels is the goroutine ladder every workload walks. These are the exact
// concurrency levels Compliance Mandate 3 obliges the module to publish numbers
// for.
//
// On a 10-core host only the 1..8 region is a scaling signal; 64 and above are
// deliberate oversubscription, where the question is not "does it go faster"
// (it cannot) but "does it degrade gracefully and where does it block".
var levels = []int{1, 8, 64, 256, 1024}

// Levels returns the goroutine ladder every workload walks.
//
// It returns a fresh slice on every call, so the ladder cannot be mutated
// through the returned value by one caller and observed changed by another.
// Levels is safe for concurrent use.
func Levels() []int { return slices.Clone(levels) }

// Window selects which half of a measurement to run. The two windows are
// deliberately run in separate processes; see the package documentation.
type Window string

const (
	// WindowEffect is the unprofiled window. It writes no profiles, and its
	// throughput and latency are the quotable numbers.
	WindowEffect Window = "effect"
	// WindowProbe is the profiled window. It writes cpu, mutex, block and
	// goroutine profiles, and its throughput is the probe's, never the
	// module's.
	WindowProbe Window = "probe"
)

// ParseWindow converts a command-line argument into a [Window].
func ParseWindow(s string) (Window, bool) {
	switch Window(s) {
	case WindowEffect:
		return WindowEffect, true
	case WindowProbe:
		return WindowProbe, true
	default:
		return "", false
	}
}

// Op is one unit of work, executed concurrently by every worker goroutine. It
// must be safe for concurrent use — that is the property under test. worker is
// the goroutine's index in [0,level) and iter counts that worker's calls, so an
// Op can carve a disjoint key space and measure the MECHANISM rather than
// data conflicts.
type Op func(ctx context.Context, worker, iter int) error

// Workload is one exercise of a module surface.
//
// A Workload is an immutable description and is safe for concurrent use. All
// mutable state lives in whatever Setup builds, and Setup is called once per
// measurement window.
type Workload struct {
	// Name is the stable identifier used in filenames and reports.
	Name string
	// Surface names the module packages the workload is meant to drive, so a
	// sweep can state which surfaces it reached and which it did not.
	Surface string
	// Ops is the total number of operations across all workers. It is fixed
	// per workload rather than per level so that every level does the SAME
	// total work: a fraction of a fixed workload in a fixed window is a rate,
	// and comparing rates across levels is the whole point.
	Ops int
	// Setup builds the fixture and returns the operation plus a teardown. dir
	// is a writable directory private to this measurement.
	Setup func(dir string) (op Op, teardown func() error, err error)
}

// Metrics is the effect half of a measurement. It is a plain value: safe to
// copy and to read concurrently, carrying no internal synchronisation.
type Metrics struct {
	Workload  string  `json:"workload"`
	Surface   string  `json:"surface"`
	Level     int     `json:"level"`
	Ops       int     `json:"ops"`
	Profiled  bool    `json:"profiled"`
	WallNanos int64   `json:"wall_nanos"`
	OpsPerSec float64 `json:"ops_per_sec"`
	P50Nanos  int64   `json:"p50_nanos"`
	P99Nanos  int64   `json:"p99_nanos"`
	MaxNanos  int64   `json:"max_nanos"`
	// LatencySampleEvery is the systematic-sampling stride used for the
	// percentiles: 1 means every operation was timed. Recorded so a reader can
	// see how the percentiles were obtained rather than assuming.
	LatencySampleEvery int   `json:"latency_sample_every"`
	LatencySamples     int   `json:"latency_samples"`
	Errors             int64 `json:"errors"`
	// GoMaxProcs and NumCPU are recorded because a scaling number is
	// meaningless without them.
	GoMaxProcs int `json:"gomaxprocs"`
	NumCPU     int `json:"numcpu"`
}

// Result pairs the two windows of one measurement. Each half is produced by its
// own child process. Like [Metrics] it is a plain value carrying no internal
// synchronisation.
type Result struct {
	Effect Metrics `json:"effect"` // unprofiled: quotable throughput
	Probe  Metrics `json:"probe"`  // profiled: attribution only
	// ProfileDir is the PROBE child's directory, holding mutex.pb.gz,
	// block.pb.gz, cpu.pb.gz and goroutine.pb.gz. The effect child writes no
	// profiles at all.
	ProfileDir string `json:"profile_dir"`
}

// workSplit shares ops operations among level workers.
//
// It exists as a named type because plain integer division does not do the job
// and the failure is silent: perWorker*level discards up to level-1 operations,
// which at level 1024 dropped 48.8% of a 2000-operation workload. That made the
// scaling column compare runs that had done different amounts of work, while
// the package documentation promised the opposite. The remainder is therefore
// handed out, one extra operation each, to the lowest-numbered workers.
type workSplit struct {
	perWorker int
	remainder int
}

func splitWork(ops, level int) workSplit {
	if ops < 0 {
		ops = 0
	}
	return workSplit{perWorker: ops / level, remainder: ops % level}
}

// count returns the number of operations worker performs.
func (s workSplit) count(worker int) int {
	if worker < s.remainder {
		return s.perWorker + 1
	}
	return s.perWorker
}

// max returns the largest per-worker operation count, for pre-sizing.
func (s workSplit) max() int {
	if s.remainder > 0 {
		return s.perWorker + 1
	}
	return s.perWorker
}

// sampleStride is the systematic-sampling stride: drive times one operation in
// every stride, aiming for roughly targetSamples observations however many
// operations the workload issues.
func sampleStride(total int) int {
	const targetSamples = 4096
	if s := total / targetSamples; s >= 1 {
		return s
	}
	return 1
}

// samplePhase is the offset within the stride at which one worker takes its
// samples.
//
// It is per-worker, and that is the point. With a shared phase of 0 every
// worker times its own FIRST operation - the instant just after the release
// barrier, carrying the stampede and whatever the workload initialises lazily.
// At level 1024 that drew 20% of all samples from the start-up instant, and the
// p99, being the top 1%, came entirely from inside it.
func samplePhase(worker, stride int) int { return worker % stride }

// drive runs op with level goroutines until ops operations have been issued in
// total, and returns the effect metrics. Every worker records its own latency
// samples into its own slice, so the measurement adds no shared state of its
// own — an observatory that contends is an observatory that lies.
func drive(w Workload, opFn Op, level, ops int, profiled bool) Metrics {
	ctx := context.Background()

	split := splitWork(ops, level)
	total := ops

	// Latency is SAMPLED, not measured per operation. A time.Now pair costs
	// tens of nanoseconds; on the cheapest workloads here an operation costs
	// about eighty. Timing every one would make the harness a co-author of the
	// result and, worse, would DILUTE the contention being hunted — a constant
	// additive per-op cost flattens the scaling curve and makes a contended
	// path look healthier than it is.
	//
	// The stride self-balances: cheap workloads run many ops and get sparse
	// sampling, expensive ones run few and get dense sampling.
	sampleEvery := sampleStride(total)

	samples := make([][]int64, level)
	for i := range samples {
		samples[i] = make([]int64, 0, split.max()/sampleEvery+1)
	}

	var errs atomic.Int64
	var wg, ready sync.WaitGroup
	start := make(chan struct{})

	wg.Add(level)
	ready.Add(level)
	for g := range level {
		go func(worker int) {
			defer wg.Done()
			count := split.count(worker)

			phase := samplePhase(worker, sampleEvery)

			// The slice header is written back once, after the run, rather
			// than on every append: the headers of adjacent workers share a
			// cache line, and the observatory must not manufacture the false
			// sharing it exists to detect.
			buf := samples[worker]

			ready.Done()
			<-start // release every worker at once: a staggered start hides contention

			for i := range count {
				if i%sampleEvery == phase {
					t0 := time.Now()
					err := opFn(ctx, worker, i)
					buf = append(buf, time.Since(t0).Nanoseconds())
					if err != nil {
						errs.Add(1)
					}
					continue
				}
				if err := opFn(ctx, worker, i); err != nil {
					errs.Add(1)
				}
			}
			samples[worker] = buf
		}(g)
	}

	// Wait for every worker to reach the barrier before starting the clock.
	// Without this, close(start) can fire before the scheduler has run some of
	// the goroutines at all, so the window timed their creation ramp rather
	// than the workload — most visibly at level 1024, where it landed in the
	// very samples the p99 is drawn from.
	ready.Wait()
	t0 := time.Now()
	close(start)
	wg.Wait()
	wall := time.Since(t0)

	all := make([]int64, 0, total/sampleEvery+level)
	for _, s := range samples {
		all = append(all, s...)
	}
	slices.Sort(all)

	m := Metrics{
		Workload:   w.Name,
		Surface:    w.Surface,
		Level:      level,
		Ops:        total,
		Profiled:   profiled,
		WallNanos:  wall.Nanoseconds(),
		Errors:     errs.Load(),
		GoMaxProcs: runtime.GOMAXPROCS(0),
		NumCPU:     runtime.NumCPU(),

		LatencySampleEvery: sampleEvery,
		LatencySamples:     len(all),
	}
	if wall > 0 {
		m.OpsPerSec = float64(total) / wall.Seconds()
	}
	if n := len(all); n > 0 {
		m.P50Nanos = all[n*50/100]
		m.P99Nanos = all[min(n*99/100, n-1)]
		m.MaxNanos = all[n-1]
	}
	return m
}

// Observe runs ONE window of a measurement in the current process and, for
// [WindowProbe], writes that window's profiles into outDir.
//
// One window per process is deliberate: see the package documentation. A caller
// that wants a [Result] runs Observe twice, in two fresh children, and pairs
// the halves.
//
// Observe is NOT safe for concurrent use: for [WindowProbe] it sets the
// process-global profiling rates. On every path — success, error, and panic —
// it restores the mutex profile fraction to the value it found and RESETS the
// block profile rate to 0. The reset is not a restore: the runtime exposes no
// getter for the block rate, so a rate the caller had set before calling
// Observe is lost and must be set again afterwards.
func Observe(w Workload, level int, win Window, outDir string) (Metrics, error) {
	if level < 1 {
		return Metrics{}, fmt.Errorf("level must be >= 1, got %d", level)
	}
	if win != WindowEffect && win != WindowProbe {
		return Metrics{}, fmt.Errorf("unknown window %q", win)
	}
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		return Metrics{}, fmt.Errorf("mkdir %s: %w", outDir, err)
	}

	ops := w.Ops
	if ops <= 0 {
		ops = 20000
	}

	fixtureDir := filepath.Join(outDir, "fixture")
	if err := os.MkdirAll(fixtureDir, 0o750); err != nil {
		return Metrics{}, fmt.Errorf("mkdir %s: %w", fixtureDir, err)
	}
	opFn, teardown, err := w.Setup(fixtureDir)
	if err != nil {
		return Metrics{}, fmt.Errorf("setup %s (%s): %w", w.Name, win, err)
	}

	var m Metrics
	var runErr error
	if win == WindowProbe {
		m, runErr = withProfiling(outDir, func() Metrics {
			return drive(w, opFn, level, ops, true)
		})
	} else {
		m = drive(w, opFn, level, ops, false)
	}

	// Both errors are reported. Returning the teardown error alone would
	// discard a profiling failure, which is the more interesting of the two.
	var tearErr error
	if teardown != nil {
		if err := teardown(); err != nil {
			tearErr = fmt.Errorf("teardown %s (%s): %w", w.Name, win, err)
		}
	}
	if err := errors.Join(runErr, tearErr); err != nil {
		return Metrics{}, err
	}
	return m, nil
}

// withProfiling enables full-rate mutex and block profiling plus a CPU profile
// for the duration of fn, writes the profiles into outDir, and restores the
// process-global rates before returning.
//
// The teardown is deferred so that a panic inside fn cannot leave the process
// wedged: a leaked profiling rate would silently tax and distort every later
// measurement in the same process, and a CPU profile left running would make
// every subsequent StartCPUProfile fail while leaking the file handle.
func withProfiling(outDir string, fn func() Metrics) (m Metrics, err error) {
	const (
		mutexFraction = 1 // sample every mutex contention event
		blockRateNs   = 1 // sample every blocking event
	)

	prevMutex := runtime.SetMutexProfileFraction(mutexFraction)
	runtime.SetBlockProfileRate(blockRateNs)
	defer func() {
		runtime.SetMutexProfileFraction(prevMutex)
		// The runtime exposes no getter for the block rate, so this is a reset
		// to the documented default of 0 (disabled), not a restore. Observe's
		// godoc states that plainly rather than claiming otherwise.
		runtime.SetBlockProfileRate(0)
	}()

	cpuFile, err := os.Create(filepath.Join(outDir, "cpu.pb.gz")) //nolint:gosec // G304: path is composed from the harness's own run directory
	if err != nil {
		return m, fmt.Errorf("create cpu profile: %w", err)
	}
	if err := pprof.StartCPUProfile(cpuFile); err != nil {
		_ = cpuFile.Close()
		return m, fmt.Errorf("start cpu profile: %w", err)
	}
	// Registered after the rate restore, so on a panic it runs FIRST: stop the
	// profile, then drop the rates. cpuRunning is still set only when fn
	// panicked, because the normal path below clears it.
	cpuRunning := true
	defer func() {
		if cpuRunning {
			pprof.StopCPUProfile()
			_ = cpuFile.Close()
		}
	}()

	m = fn()

	pprof.StopCPUProfile()
	cpuRunning = false
	if err := cpuFile.Close(); err != nil {
		return m, fmt.Errorf("close cpu profile: %w", err)
	}

	for _, p := range []string{"mutex", "block", "goroutine"} {
		if err := writeProfile(outDir, p); err != nil {
			return m, err
		}
	}
	return m, nil
}

func writeProfile(outDir, name string) error {
	prof := pprof.Lookup(name)
	if prof == nil {
		return fmt.Errorf("pprof.Lookup(%q) returned nil", name)
	}
	f, err := os.Create(filepath.Join(outDir, name+".pb.gz")) //nolint:gosec // G304: path is composed from the harness's own run directory
	if err != nil {
		return fmt.Errorf("create %s profile: %w", name, err)
	}
	if err := prof.WriteTo(f, 0); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s profile: %w", name, err)
	}
	return f.Close()
}

// metricsFile is the per-window record a child leaves for its parent.
const metricsFile = "metrics.json"

// WriteMetrics serialises one window's [Metrics] into dir, next to whatever
// profiles that window produced, so a sweep can be re-read without re-running
// it.
//
// It is safe for concurrent use only across distinct dir values: two calls
// naming the same directory race on the same file.
func WriteMetrics(dir string, m *Metrics) error {
	return writeJSON(filepath.Join(dir, metricsFile), m)
}

// ReadMetrics reads back what [WriteMetrics] wrote.
//
// ReadMetrics is safe for concurrent use.
func ReadMetrics(dir string) (Metrics, error) {
	var m Metrics
	b, err := os.ReadFile(filepath.Join(dir, metricsFile)) //nolint:gosec // G304: path is composed from the harness's own run directory
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("decode %s: %w", filepath.Join(dir, metricsFile), err)
	}
	return m, nil
}

// WriteResult serialises a paired [Result] into dir, so the two windows of one
// measurement can be read back together.
//
// It is safe for concurrent use only across distinct dir values.
func WriteResult(dir string, r *Result) error {
	return writeJSON(filepath.Join(dir, "result.json"), r)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o600)
}
