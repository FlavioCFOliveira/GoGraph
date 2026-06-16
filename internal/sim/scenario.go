package sim

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/internal/clock"
)

// scenarioClock returns the clock the concurrent/liveness scenario harnesses
// drive connection deadlines through. The concurrent modes are not
// bit-reproducible, so a real clock is appropriate here (the deterministic mode
// uses the simulator's VirtualClock and never touches this).
func scenarioClock() clock.Clock { return clock.Real() }

// ExecMode selects which harness a [Scenario] drives. The deterministic modes
// are bit-reproducible from a seed and are the only modes trace recording,
// replay, and shrinking apply to; the concurrent and liveness modes use real
// goroutines whose interleaving is not seed-controlled and are
// convergence/leak-guarded rather than bit-replayable (see the package note on
// the hybrid determinism model).
type ExecMode int

// Execution modes.
const (
	// ModeDeterministic is the single-goroutine, tick-driven engine-API safety
	// loop ([Simulator.Run]). It is fully bit-reproducible from a seed and is the
	// mode trace recording, scripted replay, and shrinking operate on.
	ModeDeterministic ExecMode = iota
	// ModeConcurrent drives N real client goroutines over the Bolt wire
	// ([RunConcurrent]). Interleaving is non-deterministic; correctness is the
	// eventual-consistency oracle plus goleak/no-panic.
	ModeConcurrent
	// ModeLiveness drives the two-phase safety->liveness flow ([RunLiveness]),
	// asserting convergence within a bounded budget plus a deadlock watchdog.
	ModeLiveness
	// ModeBulkVsOnline drives a concurrent bulk store-load alongside
	// transactional online writes (see [runBulkVsOnline]).
	ModeBulkVsOnline
)

// String renders an ExecMode for reports and the catalogue listing.
func (m ExecMode) String() string {
	switch m {
	case ModeDeterministic:
		return "deterministic"
	case ModeConcurrent:
		return "concurrent"
	case ModeLiveness:
		return "liveness"
	case ModeBulkVsOnline:
		return "bulk-vs-online"
	default:
		return fmt.Sprintf("ExecMode(%d)", int(m))
	}
}

// Bit-reproducible reports whether a scenario in this mode is bit-reproducible
// from its seed and therefore eligible for trace recording, scripted replay,
// and shrinking. Only [ModeDeterministic] qualifies.
func (m ExecMode) Reproducible() bool { return m == ModeDeterministic }

// CheckSelection chooses which extra invariant checks a scenario runs beyond
// the always-on per-tick parity check ([InvariantChecker.Check]). The zero
// value runs none of the extras.
type CheckSelection struct {
	// IndexConsistency runs the full index-vs-base-data consistency check
	// ([CheckIndexConsistency]) at the end of the run (and, for the schema-chaos
	// scenario, after DDL churn). It is meaningful only for modes that exercise
	// indexes. The set of indexes to cross-check is [CheckSelection.IndexSpecs].
	IndexConsistency bool
	// IndexSpecs are the (Label, Property) indexes the consistency check walks.
	// They are declared by the scenario because the engine's index manager
	// exposes only opaque names, not (label, property) pairs.
	IndexSpecs []IndexSpec
}

// Scenario is a named, self-contained simulation configuration: a default seed,
// a workload mix, a fault/crash schedule, a tick/time budget, the execution
// mode, and which extra checks to run. A scenario is a pure config — it carries
// no mutable run state — so the same (scenario, seed) always describes the same
// run. The registry maps names to scenarios; [Scenario.Run] executes one.
//
// # Concurrency contract
//
// A Scenario value is immutable after construction and safe to read from many
// goroutines. [Scenario.Run] itself drives a single run; the concurrent modes
// it dispatches to spawn and join their own goroutines internally.
type Scenario struct {
	// Name is the stable catalogue key (kebab-case).
	Name string
	// Description is a one-line human summary of what the scenario stresses,
	// printed by the catalogue listing.
	Description string
	// Mode selects the harness (see [ExecMode]).
	Mode ExecMode
	// DefaultSeed is the seed used when a caller does not supply one.
	DefaultSeed uint64

	// MaxTicks bounds the deterministic safety loop (ModeDeterministic).
	MaxTicks int
	// Workload is the deterministic-mode actor mix factory. When nil,
	// [DefaultWorkload] is used. It is a factory (not a built Workload) so each
	// run gets a fresh, seed-parameterised mix.
	Workload func(*Seed) *Workload
	// Crash configures deterministic crash/recovery injection (ModeDeterministic).
	Crash CrashConfig
	// Checks selects the extra invariant checks.
	Checks CheckSelection

	// Connections / OpsPerConn bound the concurrent and liveness modes.
	Connections int
	OpsPerConn  int
	// Mix is the per-connection role mix for the concurrent/liveness modes. When
	// nil, the harness default is used.
	Mix *ConcurrentMix
	// ConvergeBudget bounds the liveness convergence phase (ModeLiveness).
	ConvergeBudget time.Duration

	// run, when non-nil, fully overrides the default dispatch for a custom
	// scenario (e.g. bulk-vs-online). It receives the resolved seed and returns a
	// report (nil == passed) or an error for a harness failure. The standard
	// modes leave it nil and are dispatched by [Scenario.Run].
	run func(ctx context.Context, seed uint64) (*SimReport, error)
}

// resolveSeed returns the seed to run with: the supplied override when non-zero
// intent is signalled by useOverride, else the scenario's default.
func (sc Scenario) resolveSeed(override uint64, useOverride bool) uint64 {
	if useOverride {
		return override
	}
	return sc.DefaultSeed
}

// Run executes the scenario once with the given seed and returns a report (nil
// means the scenario passed) or an error for a harness/transport failure that
// is not itself an invariant violation. It dispatches on [Scenario.Mode]; a
// scenario with a custom run override delegates to it.
//
// For the deterministic mode the run is fully reproducible from seed and the
// returned report (on failure) carries enough to replay and shrink. For the
// concurrent and liveness modes the run is convergence/leak-guarded and the
// report, when non-nil, describes the inconsistency found at quiescence.
func (sc Scenario) Run(ctx context.Context, seed uint64) (*SimReport, error) {
	if sc.run != nil {
		return sc.run(ctx, seed)
	}
	switch sc.Mode {
	case ModeDeterministic:
		return sc.runDeterministic(ctx, seed)
	case ModeConcurrent:
		return sc.runConcurrent(ctx, seed)
	case ModeLiveness:
		return sc.runLiveness(ctx, seed)
	case ModeBulkVsOnline:
		// Bulk-vs-online always supplies a custom run override, handled above.
		return nil, fmt.Errorf("sim: scenario %q (bulk-vs-online) has no run override", sc.Name)
	default:
		return nil, fmt.Errorf("sim: scenario %q has unknown mode %s", sc.Name, sc.Mode)
	}
}

// deterministicConfig builds the [Config] for a deterministic-mode run from the
// scenario plus a resolved seed. It is exported-internal so trace recording and
// shrinking can build the identical config.
func (sc Scenario) deterministicConfig(seed uint64) Config {
	wl := sc.Workload
	if wl == nil {
		wl = DefaultWorkload
	}
	ticks := sc.MaxTicks
	if ticks <= 0 {
		ticks = 1000
	}
	return Config{
		Seed:     seed,
		MaxTicks: ticks,
		Workload: wl(NewSeed(seed)),
		Crash:    sc.Crash,
	}
}

// runDeterministic runs the engine-API safety loop and, when selected, the
// full index-consistency check at the end. It always closes the simulator so no
// durable handle or goroutine leaks past the run.
func (sc Scenario) runDeterministic(ctx context.Context, seed uint64) (*SimReport, error) {
	cfg := sc.deterministicConfig(seed)
	sm, err := New(cfg)
	if err != nil {
		return nil, fmt.Errorf("sim: scenario %q new: %w", sc.Name, err)
	}
	defer func() { _ = sm.Close() }()

	report, err := sm.Run(ctx)
	if err != nil {
		return nil, fmt.Errorf("sim: scenario %q run: %w", sc.Name, err)
	}
	if report != nil {
		return report, nil
	}
	if sc.Checks.IndexConsistency {
		if v := CheckIndexConsistency(int64(cfg.MaxTicks), sm.Oracle(), sm.engine, sc.Checks.IndexSpecs...); len(v) > 0 {
			return finalReport(seed, int64(cfg.MaxTicks), sm.Oracle(), v), nil
		}
	}
	return nil, nil
}

// runConcurrent runs the concurrent multi-connection mode and maps an
// inconsistency to a report. It builds a fresh SimServer it owns and closes.
func (sc Scenario) runConcurrent(ctx context.Context, seed uint64) (*SimReport, error) {
	srv, err := newScenarioServer()
	if err != nil {
		return nil, fmt.Errorf("sim: scenario %q server: %w", sc.Name, err)
	}
	defer func() { _ = srv.Close() }()

	res, err := RunConcurrent(ctx, srv, ConcurrentConfig{
		Seed:        seed,
		Connections: sc.Connections,
		OpsPerConn:  sc.OpsPerConn,
		Mix:         sc.Mix,
	})
	if err != nil {
		return nil, fmt.Errorf("sim: scenario %q concurrent: %w", sc.Name, err)
	}
	if !res.Consistent() {
		return concurrentReport(seed, res), nil
	}
	return nil, nil
}

// runLiveness runs the two-phase safety->liveness flow and maps a
// non-converging or inconsistent outcome to a report.
func (sc Scenario) runLiveness(ctx context.Context, seed uint64) (*SimReport, error) {
	srv, err := newScenarioServer()
	if err != nil {
		return nil, fmt.Errorf("sim: scenario %q server: %w", sc.Name, err)
	}
	defer func() { _ = srv.Close() }()

	mix := sc.Mix
	if mix == nil {
		mix = &ConcurrentMix{WriterWeight: 0.5, ReaderWeight: 0.3, OverloadWeight: 0.2}
	}
	safety, err := RunConcurrent(ctx, srv, ConcurrentConfig{
		Seed:        seed,
		Connections: sc.Connections,
		OpsPerConn:  sc.OpsPerConn,
		Mix:         mix,
	})
	if err != nil {
		return nil, fmt.Errorf("sim: scenario %q liveness safety: %w", sc.Name, err)
	}
	if !safety.Consistent() {
		return concurrentReport(seed, safety), nil
	}

	budget := sc.ConvergeBudget
	if budget <= 0 {
		budget = 30 * time.Second
	}
	live, err := RunLiveness(ctx, srv, scenarioClock(), LivenessConfig{
		Seed:           seed,
		Connections:    sc.Connections,
		OpsPerConn:     sc.OpsPerConn,
		ConvergeBudget: budget,
	})
	if err != nil {
		return nil, fmt.Errorf("sim: scenario %q liveness: %w", sc.Name, err)
	}
	if !live.Converged {
		return &SimReport{
			Seed:       seed,
			FailedTick: int64(live.Ticks),
			FailedOp:   Op{Kind: OpMatch, Cypher: "<liveness convergence>"},
			Violations: []Violation{{
				Kind:    ViolationACIDConsistency,
				Tick:    int64(live.Ticks),
				Op:      "<liveness>",
				Message: "liveness phase did not converge within budget",
			}},
		}, nil
	}
	return nil, nil
}

// concurrentReport renders a concurrent-mode inconsistency as a SimReport.
func concurrentReport(seed uint64, res ConcurrentResult) *SimReport {
	return &SimReport{
		Seed:       seed,
		FailedTick: 0,
		FailedOp:   Op{Kind: OpMatch, Cypher: "<concurrent quiescence>"},
		Violations: []Violation{{
			Kind: ViolationACIDConsistency,
			Op:   "<concurrent>",
			Message: fmt.Sprintf(
				"quiescence oracle mismatch: engineNodes=%d ackedCreates=%d panics=%d transportErrors=%d",
				res.EngineNodeCount, res.AckedCreates, res.Panics, res.TransportErrors),
		}},
		OracleState: OracleSnapshot{NodeCount: int(res.AckedCreates)},
	}
}

// finalReport renders an end-of-run check violation as a SimReport.
func finalReport(seed uint64, tick int64, oracle *GraphOracle, violations []Violation) *SimReport {
	return &SimReport{
		Seed:       seed,
		FailedTick: tick,
		FailedOp:   Op{Kind: OpMatch, Cypher: "<final check>"},
		Violations: violations,
		OracleState: OracleSnapshot{
			NodeCount: oracle.NodeCount(),
			EdgeCount: oracle.EdgeCount(),
			OpCount:   len(oracle.Ops()),
		},
	}
}

// newScenarioServer builds a fresh in-memory Bolt SimServer for a concurrent or
// liveness scenario. It is a thin constructor so every scenario opens an
// isolated server it owns and closes.
func newScenarioServer() (*SimServer, error) {
	return NewSimServer(SimEngineForServer(), scenarioClock())
}

// Registry maps scenario names to scenarios and lists them in a stable order.
// It holds no global mutable state: a Registry is built explicitly with
// [NewRegistry] (or [DefaultRegistry] for the standard catalogue) and is
// read-only after construction.
//
// # Concurrency contract
//
// A Registry is immutable after [NewRegistry] returns and safe for concurrent
// reads.
type Registry struct {
	byName map[string]Scenario
	names  []string // sorted, for a stable listing
}

// NewRegistry builds a registry from the given scenarios. It returns an error
// if two scenarios share a name (a programmer error) so the catalogue cannot
// silently shadow one scenario with another.
func NewRegistry(scenarios ...Scenario) (*Registry, error) {
	r := &Registry{byName: make(map[string]Scenario, len(scenarios))}
	for _, sc := range scenarios {
		if sc.Name == "" {
			return nil, fmt.Errorf("sim: registry: scenario with empty name")
		}
		if _, dup := r.byName[sc.Name]; dup {
			return nil, fmt.Errorf("sim: registry: duplicate scenario name %q", sc.Name)
		}
		r.byName[sc.Name] = sc
		r.names = append(r.names, sc.Name)
	}
	sort.Strings(r.names)
	return r, nil
}

// Lookup returns the scenario registered under name and whether it was found.
func (r *Registry) Lookup(name string) (Scenario, bool) {
	sc, ok := r.byName[name]
	return sc, ok
}

// Names returns the registered scenario names in sorted order. The returned
// slice is freshly allocated and owned by the caller.
func (r *Registry) Names() []string {
	out := make([]string, len(r.names))
	copy(out, r.names)
	return out
}

// Scenarios returns every registered scenario in sorted-name order. The
// returned slice is freshly allocated and owned by the caller.
func (r *Registry) Scenarios() []Scenario {
	out := make([]Scenario, 0, len(r.names))
	for _, n := range r.names {
		out = append(out, r.byName[n])
	}
	return out
}

// Len returns the number of registered scenarios.
func (r *Registry) Len() int { return len(r.names) }
