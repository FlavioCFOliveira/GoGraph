package sim

import (
	"context"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// swarmSeedMix derives each worker's per-run seed from the swarm's master seed
// and the run index, so a swarm run is itself a pure function of its master
// seed: the i-th run always uses the same derived seed regardless of which
// worker picks it up or in what order. It is a different mixing constant from
// the per-simulator sub-seed mixes so the two streams never alias.
const swarmSeedMix uint64 = 0xd1b54a32d192ed03

// SwarmConfig parameterises a [Swarm] run: the master seed, the scenario to
// run, the worker cap, and the budget (by run count, wall-clock duration, or
// both — whichever bound is hit first ends the swarm).
type SwarmConfig struct {
	// MasterSeed seeds the deterministic derivation of every per-run seed, so
	// the whole swarm reproduces from this one value. Two swarm runs with the
	// same MasterSeed, Scenario, and Runs execute the identical set of seeds.
	MasterSeed uint64
	// Scenario is the name of the catalogue scenario each run executes. It must
	// resolve in the registry the swarm was built with.
	Scenario string
	// Workers is the worker-pool cap. Values <= 0 are normalised to
	// min(GOMAXPROCS, max(1, Runs)). The pool never spawns more than this many
	// run goroutines at once.
	Workers int
	// Runs is the seed-count budget: the swarm executes exactly this many runs
	// (each a distinct derived seed) unless Duration elapses first. When Runs <=
	// 0 the swarm is duration-bounded only and MUST carry a positive Duration.
	Runs int
	// Duration is the wall-clock budget. When > 0 the swarm stops scheduling new
	// runs once it elapses (in-flight runs finish). Zero means no time bound, in
	// which case Runs MUST be positive.
	Duration time.Duration
	// Clock is the time source the duration budget reads. When nil, the real
	// clock is used. The per-run simulations remain seed-driven and never read
	// this clock — it bounds only the across-run scheduling, which is concurrent
	// and not bit-reproducible by construction.
	Clock clock.Clock
	// Selector, when non-nil, is consulted before each run to choose the
	// scenario for that run (coverage-biased selection, Phase 5 #1565). When
	// nil, every run uses Scenario. The selector must be safe for concurrent use
	// because workers call it from many goroutines.
	Selector ScenarioSelector
	// Observe, when non-nil, is called once per completed run with its outcome.
	// It runs under the aggregator's lock (so it is serialised across workers)
	// and must not block; it is an observation hook for coverage feeding and
	// live reporting.
	Observe func(SwarmRun)
}

// ScenarioSelector chooses the scenario name a given swarm run should execute.
// The coverage tracker implements it to steer runs toward under-covered paths;
// a nil selector means "always run the configured scenario". Implementations
// must be safe for concurrent use.
type ScenarioSelector interface {
	// Select returns the scenario name for the run identified by runIndex. The
	// default scenario is supplied so a selector can fall back to it.
	Select(runIndex int, defaultScenario string) string
}

// SwarmRun is the outcome of one seed in a swarm: the seed it ran, the scenario
// name, whether it failed, the failure report (nil on pass), and any harness
// error (a transport/setup failure that is not itself an invariant violation).
type SwarmRun struct {
	// Index is the run's position in the deterministic schedule (0-based).
	Index int
	// Seed is the derived per-run seed.
	Seed uint64
	// Scenario is the scenario name this run executed.
	Scenario string
	// Report is the failure report when the run found an invariant violation,
	// else nil.
	Report *SimReport
	// Err is a harness error (setup/transport failure), else nil. A run with a
	// non-nil Err counts as a failure distinct from an invariant violation.
	Err error
}

// Failed reports whether the run failed for any reason (an invariant violation
// or a harness error).
func (r SwarmRun) Failed() bool { return r.Report != nil || r.Err != nil }

// ReproduceLine returns a copy-pasteable command that re-runs exactly this
// (scenario, seed) under the CLI, so a swarm failure is reproducible verbatim.
func (r SwarmRun) ReproduceLine() string {
	return fmt.Sprintf("go run ./cmd/sim -scenario=%s %d", r.Scenario, r.Seed)
}

// SwarmResult aggregates a completed swarm: the totals, the wall-clock elapsed,
// and every failing run with its reproduce line. It is the value the CLI and
// the integration tests assert on.
type SwarmResult struct {
	// MasterSeed is the seed the schedule was derived from (for reproducing the
	// whole swarm).
	MasterSeed uint64
	// Runs is how many runs actually executed (<= the Runs budget; fewer when
	// the duration budget cut it short).
	Runs int
	// Passes is the number of runs that found no violation and no harness error.
	Passes int
	// Failures lists every failing run in ascending run-index order, so a report
	// is deterministic regardless of worker completion order.
	Failures []SwarmRun
	// Elapsed is the wall-clock time the swarm took.
	Elapsed time.Duration
	// Workers is the worker cap the swarm actually ran with.
	Workers int
}

// FailureCount returns the number of failing runs.
func (r SwarmResult) FailureCount() int { return len(r.Failures) }

// Throughput returns runs per second over the elapsed wall-clock, or 0 when no
// time elapsed (avoids a divide-by-zero in a degenerate empty run).
func (r SwarmResult) Throughput() float64 {
	if r.Elapsed <= 0 {
		return 0
	}
	return float64(r.Runs) / r.Elapsed.Seconds()
}

// Summary renders a one-block human-readable summary: totals, throughput, and
// every failing seed's reproduce line. It always ends without a trailing
// newline so the caller controls spacing.
func (r SwarmResult) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Swarm summary: master-seed=%d runs=%d passes=%d failures=%d workers=%d elapsed=%s throughput=%.0f runs/s",
		r.MasterSeed, r.Runs, r.Passes, len(r.Failures), r.Workers, r.Elapsed.Round(time.Millisecond), r.Throughput())
	for _, f := range r.Failures {
		fmt.Fprintf(&b, "\n  FAIL run=%d scenario=%s seed=%d -> %s", f.Index, f.Scenario, f.Seed, f.ReproduceLine())
		if f.Err != nil {
			fmt.Fprintf(&b, " (harness error: %v)", f.Err)
		}
	}
	return b.String()
}

// Swarm runs many independent seeds of a scenario across a bounded worker pool,
// time-boxed by run count and/or wall-clock duration, and aggregates the
// outcomes. Each worker runs ONE seed at a time against its own freshly-built
// scenario harness; the only shared mutable state is the aggregator, guarded by
// a mutex. Per-run determinism is whatever the scenario's mode guarantees (a
// deterministic scenario reproduces bit-for-bit from its derived seed); only the
// across-run scheduling is concurrent.
//
// # Concurrency contract
//
// A Swarm is built with [NewSwarm] and run once with [Swarm.Run], which spawns
// exactly Workers goroutines and joins them all before returning — it leaks no
// goroutine. Swarm is not intended for reuse across [Swarm.Run] calls.
type Swarm struct {
	reg *Registry
	cfg SwarmConfig
}

// NewSwarm builds a swarm over reg with cfg. It validates that the budget is
// well-formed (at least one of Runs or Duration is positive) and that the
// configured scenario resolves, returning an error otherwise so a
// misconfiguration fails fast rather than running an empty or unbounded swarm.
func NewSwarm(reg *Registry, cfg *SwarmConfig) (*Swarm, error) {
	if reg == nil {
		return nil, fmt.Errorf("sim: NewSwarm: nil registry")
	}
	if cfg.Runs <= 0 && cfg.Duration <= 0 {
		return nil, fmt.Errorf("sim: NewSwarm: budget must set a positive Runs or Duration")
	}
	if _, ok := reg.Lookup(cfg.Scenario); !ok {
		return nil, fmt.Errorf("sim: NewSwarm: unknown scenario %q", cfg.Scenario)
	}
	return &Swarm{reg: reg, cfg: *cfg}, nil
}

// resolveWorkers returns the worker cap, normalising a non-positive value to
// min(GOMAXPROCS, runHint) but never below 1.
func (s *Swarm) resolveWorkers(runHint int) int {
	w := s.cfg.Workers
	if w > 0 {
		return w
	}
	w = runtime.GOMAXPROCS(0)
	if runHint > 0 && runHint < w {
		w = runHint
	}
	if w < 1 {
		w = 1
	}
	return w
}

// resolveClock returns the configured clock or the real clock when none is set.
func (s *Swarm) resolveClock() clock.Clock {
	if s.cfg.Clock != nil {
		return s.cfg.Clock
	}
	return clock.Real()
}

// derivedSeed returns the per-run seed for run index i: a deterministic mix of
// the master seed and i, so the whole schedule is a pure function of the master
// seed. The mix uses a NewSeed-backed PCG draw keyed by (master, i) so adjacent
// run indices produce well-separated seeds rather than a trivial +1 sequence.
// i is a run index and is always non-negative (the dispatcher counts up from 0).
func (s *Swarm) derivedSeed(i int) uint64 {
	//nolint:gosec // G115: i is a run index, always >= 0; the uint64 conversion
	// cannot wrap. The conversion only mixes the index into the seed.
	idx := uint64(i)
	return NewSeed(s.cfg.MasterSeed^(idx*swarmSeedMix)).Uint64N(1<<62) ^ ((idx + 1) * swarmSeedMix)
}

// Run executes the swarm and returns its aggregated result. It spawns the
// resolved number of worker goroutines, each pulling run indices from a bounded
// dispatch channel, running the scenario for the derived seed, and pushing the
// outcome through the mutex-guarded aggregator. Scheduling stops when the run
// budget is exhausted, the duration budget elapses, or ctx is cancelled;
// in-flight runs always finish so no goroutine is abandoned. The returned error
// is non-nil only for a ctx cancellation that interrupted scheduling.
func (s *Swarm) Run(ctx context.Context) (SwarmResult, error) {
	start := time.Now()
	clk := s.resolveClock()

	// The deadline derives from the (possibly virtual) clock so a test can drive
	// a duration budget without real sleeping; a zero Duration means no bound.
	var deadline time.Time
	if s.cfg.Duration > 0 {
		deadline = clk.Now().Add(s.cfg.Duration)
	}

	runHint := s.cfg.Runs
	workers := s.resolveWorkers(runHint)

	// A bounded dispatch channel of run indices. With a count budget the
	// dispatcher feeds exactly Runs indices; with a pure duration budget it feeds
	// indices until the deadline. The channel capacity equals the worker count so
	// the queue is bounded and back-pressured by the workers.
	indexCh := make(chan int, workers)

	var (
		mu       sync.Mutex
		result   = SwarmResult{MasterSeed: s.cfg.MasterSeed, Workers: workers}
		wg       sync.WaitGroup
		canceled bool
	)

	record := func(run SwarmRun) {
		mu.Lock()
		defer mu.Unlock()
		result.Runs++
		if run.Failed() {
			result.Failures = append(result.Failures, run)
		} else {
			result.Passes++
		}
		if s.cfg.Observe != nil {
			s.cfg.Observe(run)
		}
	}

	// Worker pool: each worker runs one derived seed at a time until the channel
	// is drained and closed.
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range indexCh {
				record(s.runOne(ctx, idx))
			}
		}()
	}

	// Dispatcher: feed run indices until a budget or cancellation stops it, then
	// close the channel so workers drain and exit.
	dispatch := func() {
		defer close(indexCh)
		for i := 0; ; i++ {
			if runHint > 0 && i >= runHint {
				return
			}
			if err := ctx.Err(); err != nil {
				mu.Lock()
				canceled = true
				mu.Unlock()
				return
			}
			if s.cfg.Duration > 0 && !clk.Now().Before(deadline) {
				return
			}
			select {
			case indexCh <- i:
			case <-ctx.Done():
				mu.Lock()
				canceled = true
				mu.Unlock()
				return
			}
		}
	}
	dispatch()
	wg.Wait()

	result.Elapsed = time.Since(start)
	sort.Slice(result.Failures, func(i, j int) bool {
		return result.Failures[i].Index < result.Failures[j].Index
	})

	if canceled {
		return result, ctx.Err()
	}
	return result, nil
}

// runOne executes a single swarm run for run index idx: it resolves the
// scenario (consulting the selector when present), derives the seed, and runs
// the scenario, mapping the outcome to a [SwarmRun]. It never panics out: a
// scenario harness error is captured into the run's Err field.
func (s *Swarm) runOne(ctx context.Context, idx int) SwarmRun {
	scenarioName := s.cfg.Scenario
	if s.cfg.Selector != nil {
		scenarioName = s.cfg.Selector.Select(idx, s.cfg.Scenario)
	}
	seed := s.derivedSeed(idx)

	sc, ok := s.reg.Lookup(scenarioName)
	if !ok {
		return SwarmRun{
			Index:    idx,
			Seed:     seed,
			Scenario: scenarioName,
			Err:      fmt.Errorf("sim: swarm: selector chose unknown scenario %q", scenarioName),
		}
	}
	report, err := sc.Run(ctx, seed)
	return SwarmRun{
		Index:    idx,
		Seed:     seed,
		Scenario: scenarioName,
		Report:   report,
		Err:      err,
	}
}
