package sim

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/store/bulk"
)

// Standard scenario names. They are the kebab-case keys the CLI and the
// integration tests use to select a scenario from [DefaultRegistry].
const (
	ScenarioCrashStorm        = "crash-storm"
	ScenarioWriteHeavy        = "write-heavy"
	ScenarioReadHeavy         = "read-heavy"
	ScenarioSchemaChaos       = "schema-chaos"
	ScenarioSearch            = "search"
	ScenarioSearchCrash       = "search-crash"
	ScenarioBadActors         = "bad-actors"
	ScenarioOverload          = "overload"
	ScenarioBulkVsOnline      = "bulk-vs-online"
	ScenarioLongRunning       = "long-running"
	ScenarioDiskFull          = "disk-full"
	ScenarioMemPressure       = "mem-pressure"
	ScenarioCPUStarvation     = "cpu-starvation"
	ScenarioConstraintEnforce = "constraint-enforce"
	ScenarioTypeCoverage      = "type-coverage"
	ScenarioCypherPaths       = "cypher-paths"
	ScenarioEdgeProperties    = "edge-properties"
	ScenarioIndexDiversity    = "index-diversity"
	ScenarioCypherSurface     = "cypher-surface"
)

// cpuStarvationGOMAXPROCS is the processor clamp the cpu-starvation scenario
// imposes for the duration of its run: a single OS thread runs the whole goroutine
// fleet, so a compute-heavy actor competes directly with honest short queries for
// the one core. It exercises the CLAUDE.md fair-scheduling mandate — long
// operations must yield so latency tails stay bounded and the system still makes
// forward progress (the liveness watchdog asserts convergence, not a deadlock).
const cpuStarvationGOMAXPROCS = 1

// cpuStarvationConns / cpuStarvationOps bound the contention workload. They are
// kept modest so the short layer stays well under budget while still oversubscribing
// the single clamped core with many goroutines.
const (
	cpuStarvationConns = 12
	cpuStarvationOps   = 12
)

// memPressureMaxResultRows and memPressureMaxCollectItems are the clamped
// logical-resource budgets for the mem-pressure scenario. They are small enough
// that the over-budget OverloadReader statements (a 5000-row UNWIND, a 160k-row
// Cartesian, a whole-graph collect) breach them every tick, yet large enough
// that the honest workload and the harness's own count/LIMIT queries stay well
// within budget.
const (
	memPressureMaxResultRows   = 200
	memPressureMaxCollectItems = 50
)

// diskFullCapacityBytes is the byte budget for the disk-full scenario. It is
// sized (empirically) to a fraction of the WAL bytes a full write-heavy run
// would produce, so the disk fills partway through and the remaining commits
// hit ENOSPC on the real WAL append+sync path — exercising the engine's
// poison/fail-stop durability contract while a non-trivial committed graph
// already exists to keep the parity and durability checks meaningful.
const diskFullCapacityBytes = 24 * 1024

// DefaultRegistry builds the standard Phase-4 scenario catalogue. It holds no
// global mutable state — every call returns a freshly-built [Registry] — so two
// callers never share scenario state. The returned registry lists every
// scenario named by the Scenario* constants.
//
// The tick/connection budgets here are the SHORT-layer defaults: small enough
// that the catalogue runs well under the per-package 60s short-test ceiling. The
// long-running scenario carries a larger budget and is exercised only under the
// soak layer (see the integration tests).
func DefaultRegistry() (*Registry, error) {
	return NewRegistry(
		crashStormScenario(),
		writeHeavyScenario(),
		readHeavyScenario(),
		schemaChaosScenario(),
		searchScenario(),
		searchCrashScenario(),
		badActorsScenario(),
		overloadScenario(),
		bulkVsOnlineScenario(),
		longRunningScenario(),
		diskFullScenario(),
		memPressureScenario(),
		cpuStarvationScenario(),
		constraintEnforceScenario(),
		constraintExistenceScenario(),
		typeCoverageScenario(),
		cypherPathsScenario(),
		edgePropertiesScenario(),
		indexDiversityScenario(),
		cypherSurfaceScenario(),
	)
}

// cpuStarvationScenario stresses fair scheduling under CPU starvation: it clamps
// GOMAXPROCS to a single core (see [runCPUStarvation]) and runs a hog-heavy
// concurrent safety phase (60% overload / giant UNWIND + Cartesian + deep VLE
// competing with honest writers and readers), then asserts the liveness phase
// CONVERGES within budget. The system must keep making forward progress despite
// the compute hog monopolising the one core — no deadlock, no livelock
// (RESONANCE), no panic, no goroutine leak. It is concurrent (leak/no-panic +
// convergence guarded), not bit-reproducible.
func cpuStarvationScenario() Scenario {
	return Scenario{
		Name:        ScenarioCPUStarvation,
		Description: "compute hog vs honest queries on a single clamped core (fair scheduling: forward progress, no resonance)",
		Mode:        ModeLiveness,
		DefaultSeed: 0xC9057A40,
		Connections: cpuStarvationConns,
		OpsPerConn:  cpuStarvationOps,
		Mix:         &ConcurrentMix{WriterWeight: 0.2, ReaderWeight: 0.2, OverloadWeight: 0.6},
		run:         runCPUStarvation,
	}
}

// runCPUStarvation clamps GOMAXPROCS to a single core for the duration of the
// run, so the hog-heavy safety phase and the liveness convergence phase both
// contend for one OS thread, then delegates to the standard liveness flow. The
// clamp is process-global, so the integration test must not run in parallel; it
// is always restored on return. The liveness watchdog classifies a stuck run as
// RESONANCE (deadlock/livelock), which is the real fair-scheduling failure this
// scenario hunts — latency percentiles are deliberately NOT asserted (they are
// statistical and would flake).
func runCPUStarvation(ctx context.Context, seed uint64) (*SimReport, error) {
	prev := runtime.GOMAXPROCS(cpuStarvationGOMAXPROCS)
	defer runtime.GOMAXPROCS(prev)

	// Delegate to the standard liveness dispatch via a scenario value WITHOUT a
	// run override (so Run routes to runLiveness rather than recursing here).
	inner := Scenario{
		Name:        ScenarioCPUStarvation,
		Mode:        ModeLiveness,
		Connections: cpuStarvationConns,
		OpsPerConn:  cpuStarvationOps,
		Mix:         &ConcurrentMix{WriterWeight: 0.2, ReaderWeight: 0.2, OverloadWeight: 0.6},
	}
	return inner.Run(ctx, seed)
}

// memPressureScenario drives an honest writer alongside an OverloadReader that
// issues over-budget reads, with the engine's logical-resource budgets
// (MaxResultRows / MaxCollectItems) clamped low. It verifies the CLAUDE.md
// bounded-resources / backpressure-never-panic / graceful-degradation contract
// deterministically: every over-budget read is refused with a typed error and
// changes no state, so engine and oracle stay in lock-step and the honest writes
// still commit. The caps fire on integer counters (not heap), so it is fully
// bit-reproducible.
func memPressureScenario() Scenario {
	return Scenario{
		Name:        ScenarioMemPressure,
		Description: "over-budget reads against clamped logical budgets (bounded-resource graceful degradation; no panic, oracle in lock-step)",
		Mode:        ModeDeterministic,
		DefaultSeed: 0x3E308E55,
		MaxTicks:    500,
		Workload:    MemPressureWorkload,
		EngineOpts: cypher.EngineOptions{
			MaxResultRows:   memPressureMaxResultRows,
			MaxCollectItems: memPressureMaxCollectItems,
		},
	}
}

// diskFullScenario drives the honest write-heavy workload against a finite
// SimDisk so the WAL append+sync path is forced through a disk-full (ENOSPC)
// condition, with deterministic crash+recovery cycles on top. It verifies the
// engine's ACID contract under exhaustion: a commit that cannot durably write
// fails atomically (the oracle never advances for it, so engine/oracle parity
// holds), the WAL writer fail-stops, and after recovery no acknowledged commit
// is lost (ACID_DURABILITY) and no uncommitted state leaks in (ACID_ATOMICITY).
// It uses delayed-allocation ENOSPC (the harder fsync-commit path) and is
// bit-reproducible.
func diskFullScenario() Scenario {
	return Scenario{
		Name:        ScenarioDiskFull,
		Description: "honest writes against a finite disk: ENOSPC on the WAL path + crash/recovery (atomic fail-stop durability)",
		Mode:        ModeDeterministic,
		DefaultSeed: 0xD15CF011,
		MaxTicks:    500,
		Workload:    WriteHeavyWorkload,
		Disk:        DiskConfig{CapacityBytes: diskFullCapacityBytes, ENOSPCOnSync: true},
		Crash:       CrashConfig{Enabled: true, CrashProb: 1.0 / 80.0, StabilityWindow: 25},
	}
}

// crashStormScenario crashes and recovers frequently via the FULL snapshot+WAL
// path on the SimDisk, stressing the durability/recovery loop. In-loop
// checkpointing is enabled (#1740): the loop periodically publishes a real
// self-sufficient snapshot and truncates the WAL prefix, so a crash that follows
// a checkpoint recovers by reconstructing the graph from the snapshot and
// replaying only the WAL suffix — exercising [recovery.OpenFS], not just
// WAL-only replay. The ACID-D durability oracle is asserted after every recovery
// regardless of which path ran. It is deterministic and replayable.
func crashStormScenario() Scenario {
	return Scenario{
		Name:        ScenarioCrashStorm,
		Description: "frequent crash+recovery via the full snapshot+WAL path with in-loop checkpoints (durability stress)",
		Mode:        ModeDeterministic,
		DefaultSeed: 0xC4A5,
		MaxTicks:    600,
		Workload:    WriteHeavyWorkload,
		// A high crash probability with a short stability window packs many
		// crash/recovery cycles into the budget.
		Crash: CrashConfig{Enabled: true, CrashProb: 1.0 / 60.0, StabilityWindow: 25},
		// Checkpoint frequently (well inside the crash stability window) so most
		// crashes are preceded by at least one snapshot+WAL-truncate, driving the
		// full snapshot+WAL recovery path on the in-memory disk.
		Checkpoint: CheckpointConfig{Enabled: true, Every: 30},
	}
}

// writeHeavyScenario drives an 80/20 write/read deterministic mix, stressing the
// write path and oracle parity.
func writeHeavyScenario() Scenario {
	return Scenario{
		Name:        ScenarioWriteHeavy,
		Description: "80/20 write/read deterministic mix (write-path + oracle parity stress)",
		Mode:        ModeDeterministic,
		DefaultSeed: 0x5217E,
		MaxTicks:    800,
		Workload:    WriteHeavyWorkload,
	}
}

// readHeavyScenario drives a 20/80 write/read deterministic mix, stressing the
// read path and isolation.
func readHeavyScenario() Scenario {
	return Scenario{
		Name:        ScenarioReadHeavy,
		Description: "20/80 write/read deterministic mix (read-path + isolation stress)",
		Mode:        ModeDeterministic,
		DefaultSeed: 0x5EAD,
		MaxTicks:    800,
		Workload:    ReadHeavyWorkload,
	}
}

// schemaChaosScenario churns DDL (create/drop/re-create an index) under a
// deterministic write load and runs the full index-consistency check after the
// churn. It uses a custom run override so it can interleave engine-API DDL with
// honest writes deterministically (the wire-based SchemaChanger is concurrent
// and not replayable). It is bit-reproducible.
func schemaChaosScenario() Scenario {
	return Scenario{
		Name:        ScenarioSchemaChaos,
		Description: "frequent DDL (index create/drop/re-create) under write load + full index-consistency check",
		Mode:        ModeDeterministic,
		DefaultSeed: 0x5C4A05,
		MaxTicks:    500,
		Workload:    WriteHeavyWorkload,
		Checks:      CheckSelection{IndexConsistency: true, IndexSpecs: []IndexSpec{{Label: "Person", Property: "name"}}},
		run:         runSchemaChaos,
	}
}

// searchScenario drives a deterministic write-heavy workload to build a
// Person/KNOWS graph, then runs the search-algorithm battery ([CheckSearch])
// periodically (every SearchEvery ticks) and once more at the end. Each
// exercised search/ algorithm is cross-checked against an independent naive
// reference, and the engine graph is held to full structural parity with the
// oracle model. It is bit-reproducible, so a failure replays and shrinks like
// any other deterministic scenario.
func searchScenario() Scenario {
	return Scenario{
		Name:        ScenarioSearch,
		Description: "search/ battery (BFS/DFS/WCC) over the live graph + full engine-vs-oracle structural parity",
		Mode:        ModeDeterministic,
		DefaultSeed: 0x5EA4C8,
		MaxTicks:    800,
		Workload:    WriteHeavyWorkload,
		SearchEvery: 200,
		Checks:      CheckSelection{Search: true},
	}
}

// searchCrashScenario is the crash-enabled variant of the search scenario: it
// drives the same write-heavy workload and search battery but injects
// deterministic crash+recovery cycles, so the search/ algorithms are validated
// against the graph that survives WAL recovery (the simulator runs the full
// search battery immediately after every recovery, on top of the periodic and
// terminal checks). It is bit-reproducible.
func searchCrashScenario() Scenario {
	return Scenario{
		Name:        ScenarioSearchCrash,
		Description: "search/ battery validated on the crash+recovery-survived graph (durability x algorithms)",
		Mode:        ModeDeterministic,
		DefaultSeed: 0x5EA4C2A5,
		MaxTicks:    500,
		Workload:    WriteHeavyWorkload,
		SearchEvery: 150,
		Checks:      CheckSelection{Search: true},
		Crash:       CrashConfig{Enabled: true, CrashProb: 1.0 / 80.0, StabilityWindow: 25},
	}
}

// badActorsScenario runs a 100% malformed/abuse deterministic workload: every
// op is ill-formed and must be rejected with a typed error, leaving state
// unchanged. The engine errors are EXPECTED and not violations (the oracle
// models each malformed op as a no-op, so engine and oracle stay in lock-step).
func badActorsScenario() Scenario {
	return Scenario{
		Name:        ScenarioBadActors,
		Description: "100% malformed/abuse workload (every op rejected with a typed error; no state change)",
		Mode:        ModeDeterministic,
		DefaultSeed: 0xBAD,
		MaxTicks:    600,
		Workload:    malformedOnlyWorkload,
	}
}

// overloadScenario runs the concurrent overload mix (giant tx / huge UNWIND /
// large result sets / deep VLE) over the real Bolt wire. The engine's typed
// bound errors are EXPECTED (graceful degradation) and not violations. It is
// concurrent (leak/no-panic guarded), not bit-replayable.
func overloadScenario() Scenario {
	return Scenario{
		Name:        ScenarioOverload,
		Description: "concurrent giant tx/queries/UNWIND/VLE (bounded resource caps; graceful degradation)",
		Mode:        ModeConcurrent,
		DefaultSeed: 0x07E410AD,
		Connections: 12,
		OpsPerConn:  8,
		// A high overload weight so most connections push the engine's bounds.
		Mix: &ConcurrentMix{WriterWeight: 0.3, ReaderWeight: 0.1, OverloadWeight: 0.6},
	}
}

// bulkVsOnlineScenario runs a concurrent store/bulk CSR load alongside
// transactional online writes against the live engine, watching for goroutine/
// heap stability and clean completion of both. It uses a custom run override
// because the bulk loader is an offline CSR builder writing its own output
// file, distinct from the transactional engine. It is concurrent, not
// bit-replayable.
func bulkVsOnlineScenario() Scenario {
	return Scenario{
		Name:        ScenarioBulkVsOnline,
		Description: "concurrent store/bulk CSR load + transactional online writes (resource stability)",
		Mode:        ModeBulkVsOnline,
		DefaultSeed: 0xB014,
		Connections: 6,
		OpsPerConn:  40,
		run:         runBulkVsOnline,
	}
}

// longRunningScenario drives a large deterministic write/read mix (many small
// ops) and watches oracle parity throughout — the goroutine/heap-stability watch
// is the test's goleak + bounded-growth assertion around it. Its budget is large
// (soak-layer); the catalogue lists it but the integration test gates the full
// budget behind the soak layer.
func longRunningScenario() Scenario {
	return Scenario{
		Name:        ScenarioLongRunning,
		Description: "millions of small ops; oracle parity + goroutine/heap stability watch (soak)",
		Mode:        ModeDeterministic,
		DefaultSeed: 0x10067,
		MaxTicks:    2_000_000,
		// A bounded-churn workload keeps the working set near churnHighWater so the
		// run is a true heap/goroutine-stability watch rather than an O(n²) blow-up.
		Workload: SteadyStateWorkload,
		// Check periodically (not every tick) so the full-graph parity probe does
		// not dominate a millions-of-ops run; stability is the property here.
		CheckEvery: 500,
	}
}

// malformedOnlyWorkload is a 100% MalformedSender mix for the bad-actors
// scenario: every op is ill-formed.
func malformedOnlyWorkload(_ *Seed) *Workload {
	return &Workload{
		Actors:  []Actor{MalformedSender{}},
		Weights: []float64{1.0},
	}
}

// schemaChurnEvery is the tick cadence at which runSchemaChaos issues a DDL
// statement, interleaving index create/drop/re-create with the honest write
// load.
const schemaChurnEvery = 50

// runSchemaChaos is the schema-chaos custom run: it drives the deterministic
// engine-API safety loop while periodically issuing DDL over the same engine,
// then runs the full index-consistency check at the end. Determinism holds
// because the DDL cadence is positional (tick-driven) and the workload draw
// stream is the master seed's alone.
func runSchemaChaos(ctx context.Context, seed uint64) (*SimReport, error) {
	sc := schemaChaosScenario()
	cfg := sc.DeterministicConfig(seed)

	sm, err := New(cfg)
	if err != nil {
		return nil, fmt.Errorf("sim: schema-chaos new: %w", err)
	}
	defer func() { _ = sm.Close() }()

	// Create the index up front so the whole run maintains it under DDL churn.
	if err := sm.engineRunDDL(ctx, "CREATE INDEX sim_person_name FOR (n:Person) ON (n.name)"); err != nil {
		return nil, fmt.Errorf("sim: schema-chaos initial index: %w", err)
	}

	report, err := sm.runWithDDL(ctx, schemaChurnEvery)
	if err != nil {
		return nil, fmt.Errorf("sim: schema-chaos run: %w", err)
	}
	if report != nil {
		return report, nil
	}
	if v := CheckIndexConsistency(int64(cfg.MaxTicks), sm.Oracle(), sm.engine, sc.Checks.IndexSpecs...); len(v) > 0 {
		return finalReport(seed, int64(cfg.MaxTicks), sm.Oracle(), v), nil
	}
	return nil, nil
}

// bulkLoadEdges is the number of edges the bulk-vs-online scenario streams
// through the offline bulk loader. It is bounded so the scenario stays within
// the short-test budget while still being a genuinely heavy concurrent load.
const bulkLoadEdges = 20_000

// runBulkVsOnline runs an offline bulk CSR load concurrently with transactional
// online writes over the Bolt wire, then asserts both completed cleanly and the
// online writes are all present (eventual-consistency oracle). The bulk loader
// writes its own csrfile; the two share no graph state, so correctness here is
// resource stability (both finish, no panic, no leak — the caller's goleak
// check) plus the online oracle.
func runBulkVsOnline(ctx context.Context, seed uint64) (*SimReport, error) {
	sc := bulkVsOnlineScenario()
	srv, err := newScenarioServer()
	if err != nil {
		return nil, fmt.Errorf("sim: bulk-vs-online server: %w", err)
	}
	defer func() { _ = srv.Close() }()

	dir, err := os.MkdirTemp("", "sim-bulk-*")
	if err != nil {
		return nil, fmt.Errorf("sim: bulk-vs-online tempdir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	var (
		wg       sync.WaitGroup
		bulkRows atomic.Int64
		bulkErr  atomic.Pointer[error]
	)

	// Bulk loader goroutine: offline CSR build to a temp csrfile.
	wg.Add(1)
	go func() {
		defer wg.Done()
		rows, err := bulkLoad(ctx, NewSeed(seed), filepath.Join(dir, "out.csr"))
		if err != nil {
			bulkErr.Store(&err)
			return
		}
		bulkRows.Store(int64(rows))
	}()

	// Online transactional writes over the Bolt wire, concurrently.
	online, runErr := RunConcurrent(ctx, srv, ConcurrentConfig{
		Seed:        seed,
		Connections: sc.Connections,
		OpsPerConn:  sc.OpsPerConn,
		Mix:         &ConcurrentMix{WriterWeight: 0.9, ReaderWeight: 0.1},
	})
	wg.Wait()

	if runErr != nil {
		return nil, fmt.Errorf("sim: bulk-vs-online online writes: %w", runErr)
	}
	if ep := bulkErr.Load(); ep != nil {
		return nil, fmt.Errorf("sim: bulk-vs-online bulk load: %w", *ep)
	}
	if !online.Consistent() {
		return concurrentReport(seed, online), nil
	}
	if bulkRows.Load() != int64(bulkLoadEdges) {
		return &SimReport{
			Seed:     seed,
			FailedOp: Op{Kind: OpMatch, Cypher: "<bulk load>"},
			Violations: []Violation{{
				Kind:    ViolationOracleDeviation,
				Op:      "<bulk>",
				Message: fmt.Sprintf("bulk load row mismatch: got %d want %d", bulkRows.Load(), bulkLoadEdges),
			}},
		}, nil
	}
	return nil, nil
}

// bulkLoad streams bulkLoadEdges seed-derived edges through the offline bulk
// loader and finalises the csrfile. The edge endpoints are seed-derived so the
// load is a deterministic function of seed even though it runs concurrently with
// the online writes.
func bulkLoad(ctx context.Context, seed *Seed, outPath string) (int, error) {
	loader := bulk.New(bulk.Options{
		OutputPath: outPath,
		Directed:   true,
		Multigraph: true,
		MaxRows:    bulkLoadEdges,
	})
	for i := 0; i < bulkLoadEdges; i++ {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		src := fmt.Sprintf("b%d", seed.Uint64N(4096))
		dst := fmt.Sprintf("b%d", seed.Uint64N(4096))
		if err := loader.Add(bulk.Edge{Src: src, Dst: dst, Weight: 1}); err != nil {
			return 0, err
		}
	}
	rows, _, err := loader.Finalise()
	if err != nil {
		return 0, err
	}
	return rows, nil
}
