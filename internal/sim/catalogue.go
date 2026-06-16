package sim

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/FlavioCFOliveira/GoGraph/store/bulk"
)

// Standard scenario names. They are the kebab-case keys the CLI and the
// integration tests use to select a scenario from [DefaultRegistry].
const (
	ScenarioCrashStorm   = "crash-storm"
	ScenarioWriteHeavy   = "write-heavy"
	ScenarioReadHeavy    = "read-heavy"
	ScenarioSchemaChaos  = "schema-chaos"
	ScenarioBadActors    = "bad-actors"
	ScenarioOverload     = "overload"
	ScenarioBulkVsOnline = "bulk-vs-online"
	ScenarioLongRunning  = "long-running"
)

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
		badActorsScenario(),
		overloadScenario(),
		bulkVsOnlineScenario(),
		longRunningScenario(),
	)
}

// crashStormScenario crashes and recovers frequently via the SimDisk WAL path,
// stressing the durability/recovery loop. It is deterministic and replayable.
func crashStormScenario() Scenario {
	return Scenario{
		Name:        ScenarioCrashStorm,
		Description: "frequent crash+recovery via the SimDisk WAL path (durability stress)",
		Mode:        ModeDeterministic,
		DefaultSeed: 0xC4A5,
		MaxTicks:    600,
		Workload:    WriteHeavyWorkload,
		// A high crash probability with a short stability window packs many
		// crash/recovery cycles into the budget.
		Crash: CrashConfig{Enabled: true, CrashProb: 1.0 / 60.0, StabilityWindow: 25},
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
