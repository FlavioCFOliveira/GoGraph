package sim

import (
	"context"
	"fmt"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// defaultTickSize is the simulated duration of one tick (1ms by convention).
const defaultTickSize = time.Millisecond

// defaultCheckEvery is the default invariant-check cadence: check after every
// operation.
const defaultCheckEvery = 1

// checkerSeedMix and diskSeedMix derive independent sub-seeds for the checker
// and the disk from the master seed, so neither the check cadence nor (in
// Phase 2) the disk fault stream perturbs the workload draw stream. The
// workload draw sequence stays a pure function of cfg.Seed alone.
const (
	checkerSeedMix uint64 = 0x9e3779b97f4a7c15
	diskSeedMix    uint64 = 0xc2b2ae3d27d4eb4f
)

// Config parameterises a simulation run.
type Config struct {
	// Seed is the master seed; the entire run is a pure function of it.
	Seed uint64
	// MaxTicks is the number of ticks (operations) the safety phase runs.
	MaxTicks int
	// Workload is the actor mix. When nil, [DefaultWorkload] is used.
	Workload *Workload
	// CheckEvery is the invariant-check cadence in ticks. Values <= 0 are
	// normalised to 1 (check every tick).
	CheckEvery int
	// OnOp, when non-nil, is called synchronously with each tick and the
	// operation about to run, before it is executed. It is an observation hook
	// (e.g. for verbose tracing); it must not mutate state or draw from any
	// randomness, or it would break reproducibility. It runs on the simulation
	// goroutine.
	OnOp func(tick int64, op Op)
	// Crash configures deterministic crash/recovery injection. The zero value
	// disables it (Enabled == false), which is the safe default: a run that does
	// not opt in drives a plain in-memory engine exactly as before, byte for
	// byte. When enabled, the simulator instead drives a real SimDisk-backed
	// persistence stack (WAL append+sync + recovery replay) so a scheduled crash
	// drops the live engine and the store is reopened from the durable image.
	Crash CrashConfig
	// OnCrash, when non-nil, is called synchronously after each crash+recovery
	// cycle with the crash tick and how many WAL ops recovery replayed. Like
	// OnOp it is an observation hook and must not mutate state or draw
	// randomness.
	OnCrash func(tick int64, replayedWALOps int)
	// Disk, when its CapacityBytes > 0, bounds the SimDisk-backed durable store
	// to a finite size so the run drives the engine through a disk-full (ENOSPC)
	// condition on the real WAL append+sync path. A non-zero capacity implies the
	// durable store even when Crash is disabled; the zero value leaves the disk
	// unbounded (the prior behaviour). See [DiskConfig].
	Disk DiskConfig
}

// DiskConfig bounds the simulated disk so the harness can drive the engine
// through a disk-full (ENOSPC) condition. The zero value (CapacityBytes == 0)
// leaves the disk unbounded.
type DiskConfig struct {
	// CapacityBytes, when > 0, is the total byte budget across all files in the
	// SimDisk-backed store. A WAL append or checkpoint write that would breach it
	// returns an ENOSPC error on the real durability path.
	CapacityBytes int64
	// ENOSPCOnSync selects where the out-of-space condition surfaces: false
	// (eager, at the growing Write) or true (delayed, at Sync). See [SimDisk].
	ENOSPCOnSync bool
}

// Simulator drives the real cypher.Engine against a shadow [GraphOracle] under
// a deterministic, single-goroutine, tick-driven loop, verifying ACID and
// graph invariants after operations.
//
// # Concurrency contract
//
// Simulator is NOT safe for concurrent use and spawns no goroutines. Its
// determinism guarantee depends on a single, totally-ordered stream of draws
// from one [Seed]; [Simulator.Run] must be called from one goroutine.
type Simulator struct {
	cfg      Config
	seed     *Seed
	clock    *VirtualClock
	disk     *SimDisk
	oracle   *GraphOracle
	checker  *InvariantChecker
	workload *Workload
	engine   *EngineAdapter
	// crash is the deterministic crash scheduler. It is always non-nil but is
	// inert (never fires, never draws) when Config.Crash.Enabled is false.
	crash *CrashSchedule
	// store is the live SimDisk-backed persistence stack the simulator drives in
	// crash mode; nil when crashes are disabled (the engine is then a plain
	// in-memory engine with no durable layer). On a crash it is reopened from the
	// durable SimDisk image via real recovery.
	store *SimStore
	// crashCount and replayedOps accumulate run statistics for reports and tests.
	crashCount  int
	replayedOps int
	// rejectedWrites counts write-shaped operations the engine did NOT commit
	// (committed == false). Under the disk-full scenario this is the non-vacuity
	// guard that ENOSPC actually fired: an honest write fails only when the
	// durable WAL path could not persist it. The oracle stays frozen for each.
	rejectedWrites int
	// searchEvery, when > 0, runs the full search-algorithm battery
	// ([CheckSearch]) every searchEvery ticks in the deterministic loop, on top of
	// any terminal check the scenario runs. It lives on the Simulator (not Config)
	// because the battery is far more expensive than the per-tick parity probe and
	// only the search scenario opts in; runDeterministic sets it from the
	// scenario. Zero (the default) disables periodic search checks.
	searchEvery int
}

// New builds a Simulator with a fresh in-memory engine, oracle, checker, clock,
// and (Phase-2-bound, currently unwired) SimDisk, all driven by cfg.Seed. It
// returns an error only for an invalid configuration.
func New(cfg Config) (*Simulator, error) {
	if cfg.MaxTicks < 0 {
		return nil, fmt.Errorf("sim: MaxTicks must be non-negative, got %d", cfg.MaxTicks)
	}
	seed := NewSeed(cfg.Seed)

	wl := cfg.Workload
	if wl == nil {
		wl = DefaultWorkload(seed)
	}
	if cfg.CheckEvery <= 0 {
		cfg.CheckEvery = defaultCheckEvery
	}

	// The checker samples from its own seed, derived from the master seed, so
	// that changing the check cadence (CheckEvery) never perturbs the workload
	// draw stream: the actor/op/param sequence stays a pure function of cfg.Seed
	// alone, independent of how often invariants are checked.
	checkerSeed := NewSeed(cfg.Seed ^ checkerSeedMix)

	// The disk's fault stream draws from its own sub-seed for the same reason.
	disk := NewSimDisk(NewSeed(cfg.Seed^diskSeedMix), 0)

	s := &Simulator{
		cfg:      cfg,
		seed:     seed,
		clock:    NewVirtualClock(defaultTickSize),
		disk:     disk,
		oracle:   NewGraphOracle(),
		checker:  NewInvariantChecker(checkerSeed),
		workload: wl,
		// The crash scheduler draws from its own sub-seed, so toggling crashes
		// never perturbs the workload stream. It is inert when disabled.
		crash: NewCrashSchedule(NewSeed(cfg.Seed^crashSeedMix), cfg.Crash),
	}

	if cfg.Crash.Enabled || cfg.Disk.CapacityBytes > 0 {
		// Durable mode: drive the real SimDisk-backed persistence stack so a crash
		// can drop the engine and recovery can replay the durable WAL bytes, and so
		// a finite disk can drive the WAL append+sync path through ENOSPC. A
		// non-zero disk capacity opts in even without crashes.
		store, err := OpenSimStore(disk, simulatorStoreConfig())
		if err != nil {
			return nil, fmt.Errorf("sim: open SimDisk-backed store: %w", err)
		}
		// Apply the byte budget AFTER the initial open so the store's first WAL
		// setup is never starved; the budget then bounds the workload's growth.
		if cfg.Disk.CapacityBytes > 0 {
			disk.SetCapacity(cfg.Disk.CapacityBytes, cfg.Disk.ENOSPCOnSync)
		}
		s.store = store
		s.engine = NewEngineAdapter(store.Engine())
	} else {
		// Default (no-crash) path: a plain in-memory engine with no durable
		// layer, byte-identical to the pre-crash simulator.
		g := lpg.New[string, float64](adjlist.Config{Directed: true})
		s.engine = NewEngineAdapter(cypher.NewEngine(g))
	}

	return s, nil
}

// Run executes the safety-phase tick loop. Each tick advances the clock,
// selects an actor, asks it for an operation, runs that operation against the
// engine, applies it to the oracle, and (every CheckEvery ticks) verifies the
// invariants. On the first violation it returns a populated [SimReport]; on
// clean completion it returns (nil, nil). It honours ctx cancellation and
// deadlines, returning the ctx error if the run is interrupted.
//
// The loop runs entirely on the calling goroutine and spawns none; engine
// operations are synchronous.
func (s *Simulator) Run(ctx context.Context) (*SimReport, error) {
	var lastTick int64
	var lastOp Op
	for i := 0; i < s.cfg.MaxTicks; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		tick := s.clock.Tick()

		// Decide a crash for this tick BEFORE running the op. A scheduled crash
		// drops the live engine and reopens from the durable SimDisk image via
		// real recovery; the durability check then verifies every ACKed-committed
		// op survived (see [Simulator.maybeCrash]). The crash decision draws from
		// the crash sub-seed only (or not at all when disabled), so it never
		// perturbs the workload op stream.
		if report, err := s.maybeCrash(ctx, tick); err != nil {
			return nil, err
		} else if report != nil {
			return report, nil
		}

		actor := s.workload.SelectActor(s.seed)
		op := actor.NextOp(s.seed, s.oracle)

		if s.cfg.OnOp != nil {
			s.cfg.OnOp(tick, op)
		}

		committed := s.execute(ctx, op)
		if !committed && op.Kind.IsWrite() {
			s.rejectedWrites++
		}
		s.applyToOracle(op, committed)
		lastTick, lastOp = tick, op

		if tick%int64(s.cfg.CheckEvery) == 0 {
			if violations := s.checker.Check(tick, s.oracle, s.engine); len(violations) > 0 {
				return s.report(tick, op, violations), nil
			}
		}

		// The search battery runs on its own (coarser) cadence: it extracts the
		// whole graph and runs the search/ algorithms, so it is gated off by
		// default and opted into only by the search scenario.
		if s.searchEvery > 0 && tick%int64(s.searchEvery) == 0 {
			if violations := CheckSearch(tick, s.oracle, s.engine); len(violations) > 0 {
				return s.report(tick, op, violations), nil
			}
		}
	}
	return s.finalCheck(lastTick, lastOp), nil
}

// finalCheck runs one terminal invariant check on the final tick when the
// cadence (CheckEvery) skipped it, so a CheckEvery>1 run can never let a
// violation introduced in the last (CheckEvery-1) ticks escape unverified. It
// is a no-op for an empty run (lastTick == 0) and for any run whose last tick
// the loop already checked (lastTick % CheckEvery == 0, which always holds for
// the default CheckEvery == 1). Returns a populated report on a violation, or
// nil when the terminal state is clean or no terminal check is needed.
func (s *Simulator) finalCheck(lastTick int64, lastOp Op) *SimReport {
	if lastTick == 0 || lastTick%int64(s.cfg.CheckEvery) == 0 {
		return nil
	}
	if violations := s.checker.Check(lastTick, s.oracle, s.engine); len(violations) > 0 {
		return s.report(lastTick, lastOp, violations)
	}
	return nil
}

// engineRunDDL runs a DDL statement (CREATE/DROP INDEX/CONSTRAINT) against the
// live engine and drains it. It is used by the schema-chaos scenario to churn
// schema deterministically over the same engine the safety loop drives. DDL
// statements go through the engine's read path ([cypher.Engine.Run]), which is
// where the DDL operators live.
func (s *Simulator) engineRunDDL(ctx context.Context, query string) error {
	res, err := s.engine.Run(ctx, query, nil)
	if err != nil {
		return err
	}
	for res.Next() {
	}
	drainErr := res.Err()
	_ = res.Close()
	return drainErr
}

// schemaChurnStatements is the fixed, idempotent DDL cycle runWithDDL rotates
// through: drop then re-create the (:Person).name index. Idempotent forms (IF
// [NOT] EXISTS) make each step a clean no-op when it races nothing, so the churn
// never errors on a benign re-create/re-drop.
var schemaChurnStatements = []string{
	"DROP INDEX sim_person_name IF EXISTS",
	"CREATE INDEX sim_person_name FOR (n:Person) ON (n.name)",
}

// runWithDDL is the schema-chaos variant of [Simulator.Run]: it drives the same
// deterministic tick loop but, every ddlEvery ticks, issues the next idempotent
// DDL statement from [schemaChurnStatements] against the live engine, churning
// the index under the honest write load. Like Run it returns a populated report
// on the first invariant violation, or (nil, nil) on clean completion. The DDL
// cadence is positional (tick-driven), so the run stays a deterministic function
// of the seed.
func (s *Simulator) runWithDDL(ctx context.Context, ddlEvery int) (*SimReport, error) {
	churnIdx := 0
	var lastTick int64
	var lastOp Op
	for i := 0; i < s.cfg.MaxTicks; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		tick := s.clock.Tick()

		if ddlEvery > 0 && tick%int64(ddlEvery) == 0 {
			stmt := schemaChurnStatements[churnIdx%len(schemaChurnStatements)]
			churnIdx++
			if err := s.engineRunDDL(ctx, stmt); err != nil {
				return nil, fmt.Errorf("sim: schema churn DDL %q at tick %d: %w", stmt, tick, err)
			}
		}

		actor := s.workload.SelectActor(s.seed)
		op := actor.NextOp(s.seed, s.oracle)
		if s.cfg.OnOp != nil {
			s.cfg.OnOp(tick, op)
		}
		committed := s.execute(ctx, op)
		s.applyToOracle(op, committed)
		lastTick, lastOp = tick, op

		if tick%int64(s.cfg.CheckEvery) == 0 {
			if violations := s.checker.Check(tick, s.oracle, s.engine); len(violations) > 0 {
				return s.report(tick, op, violations), nil
			}
		}
	}
	return s.finalCheck(lastTick, lastOp), nil
}

// maybeCrash performs a crash+recovery cycle when the schedule fires at tick.
// It crashes the SimDisk-backed store (drops the in-memory engine, keeps the
// durable WAL byte image), reopens it through real recovery, rebinds the engine
// adapter to the recovered graph, and verifies the post-recovery durable state
// against the oracle's ACKed-committed set. On a durability violation it returns
// a populated report; on a recovery error it returns that error; otherwise
// (nil, nil) and the loop resumes against the recovered engine.
//
// When crashes are disabled the schedule is inert and this is a cheap no-op.
func (s *Simulator) maybeCrash(_ context.Context, tick int64) (*SimReport, error) {
	if !s.crash.ShouldCrash(tick) {
		return nil, nil
	}
	// SIGKILL-equivalent: discard the live engine and store WITHOUT a graceful
	// close, so any buffered-but-unsynced frame is lost exactly as a real crash
	// would lose it. The durable WAL byte image in the SimDisk survives.
	s.store.Crash()

	store, err := OpenSimStore(s.disk, simulatorStoreConfig())
	if err != nil {
		// A genuine recovery failure (e.g. corruption fail-stop) is a hard fault
		// the run must surface, not swallow.
		return nil, fmt.Errorf("sim: crash recovery at tick %d: %w", tick, err)
	}
	s.store = store
	s.engine = NewEngineAdapter(store.Engine())
	s.crashCount++
	s.replayedOps += store.WALOps()

	if s.cfg.OnCrash != nil {
		s.cfg.OnCrash(tick, store.WALOps())
	}

	// Durability check at the crash boundary: every op the engine ACKed as
	// committed before the crash must be present after recovery, and nothing
	// uncommitted may have leaked in (see [InvariantChecker.CheckDurability]).
	if violations := s.checker.CheckDurability(tick, s.oracle, s.engine); len(violations) > 0 {
		return s.report(tick, Op{Kind: OpMatch, Cypher: "<crash recovery>"}, violations), nil
	}

	// When the search battery is enabled, run it on the recovered graph too: this
	// is the DST-unique value for search — the traversal/path-finding/analytics
	// algorithms are validated against a graph that has actually survived a crash
	// and WAL recovery, not just a freshly-built in-memory one. Gated on
	// searchEvery so non-search scenarios pay nothing.
	if s.searchEvery > 0 {
		if violations := CheckSearch(tick, s.oracle, s.engine); len(violations) > 0 {
			return s.report(tick, Op{Kind: OpMatch, Cypher: "<post-recovery search>"}, violations), nil
		}
	}
	return nil, nil
}

// execute runs op against the engine via the read or write path per its kind
// and reports whether a write committed (the engine ACKed it without error and
// the result drained cleanly). Engine errors are not treated as violations
// here: an honest workload may legitimately hit a typed engine error, and a
// malformed actor expects one. Reporting the commit outcome lets [applyToOracle]
// advance the shadow model ONLY for writes the engine actually durably ACKed,
// which is what keeps the oracle equal to the engine's durable state across a
// crash. The result is always drained and closed so no resources leak across
// ticks.
//
// For read-shaped ops the return value is not meaningful (reads never change
// modelled state) and is reported as committed so a read still records its
// (no-effect) history entry.
func (s *Simulator) execute(ctx context.Context, op Op) bool {
	var (
		res Result
		err error
	)
	if op.Kind.IsWrite() {
		res, err = s.engine.RunWrite(ctx, op.Cypher, op.Params)
	} else {
		res, err = s.engine.Run(ctx, op.Cypher, op.Params)
	}
	if err != nil {
		return false
	}
	for res.Next() {
	}
	// A drain error after the statement was accepted means the result did not
	// fully materialise; treat it as not-committed so the oracle does not model
	// an effect the engine may not have durably applied.
	drainErr := res.Err()
	_ = res.Close()
	return drainErr == nil
}

// applyToOracle advances the shadow model for op per its kind. A write is
// applied only when the engine committed it (committed == true); a write the
// engine rejected (e.g. an injected durability fault poisoned the WAL, or a
// malformed op was refused) leaves the oracle unchanged, so the oracle always
// models exactly the engine's durable committed set. Reads and the
// expected-error malformed no-op are recorded unconditionally (they change no
// state).
func (s *Simulator) applyToOracle(op Op, committed bool) {
	switch op.Kind {
	case OpCreate:
		if committed {
			s.oracle.ApplyCreate(op.Cypher, op.Params)
		}
	case OpMerge:
		if committed {
			s.oracle.ApplyMerge(op.Cypher, op.Params)
		}
	case OpDelete:
		if committed {
			s.oracle.ApplyDelete(op.Cypher, op.Params)
		}
	case OpUpdate:
		if committed {
			s.oracle.ApplyMatch(op.Cypher, op.Params)
		}
	case OpMatch:
		// A pure read never changes state; record it regardless of outcome.
		s.oracle.ApplyMatch(op.Cypher, op.Params)
	case OpMalformed:
		// A malformed op is expected to be rejected by the engine with a typed
		// error and to leave state unchanged; the oracle records it as an
		// expected-error no-op so engine and oracle stay in lock-step.
		s.oracle.ApplyMalformed(op.Cypher, op.Params)
	}
}

// report builds a SimReport for a detected violation.
func (s *Simulator) report(tick int64, op Op, violations []Violation) *SimReport {
	return &SimReport{
		Seed:       s.cfg.Seed,
		FailedTick: tick,
		FailedOp:   op,
		Violations: violations,
		OracleState: OracleSnapshot{
			NodeCount: s.oracle.NodeCount(),
			EdgeCount: s.oracle.EdgeCount(),
			OpCount:   len(s.oracle.Ops()),
		},
	}
}

// Oracle returns the simulator's shadow model, for tests that assert on the
// modelled state after a run.
func (s *Simulator) Oracle() *GraphOracle { return s.oracle }

// CrashCount returns how many crash+recovery cycles the run performed (always 0
// when crashes are disabled).
func (s *Simulator) CrashCount() int { return s.crashCount }

// ReplayedOps returns the cumulative number of WAL ops recovery replayed across
// every crash cycle in the run.
func (s *Simulator) ReplayedOps() int { return s.replayedOps }

// RejectedWrites returns how many write-shaped operations the engine did not
// commit during the run. Under the disk-full scenario it is the non-vacuity
// guard that ENOSPC fired; it is 0 for a run that never exhausts the disk.
func (s *Simulator) RejectedWrites() int { return s.rejectedWrites }

// Close releases the simulator's durable resources. In crash mode it gracefully
// closes the live SimDisk-backed store (flushing and releasing the WAL writer)
// so no handle or goroutine leaks past the run; in the default in-memory mode it
// is a no-op. It is safe to call more than once.
func (s *Simulator) Close() error {
	if s.store == nil {
		return nil
	}
	err := s.store.Close()
	s.store = nil
	return err
}
