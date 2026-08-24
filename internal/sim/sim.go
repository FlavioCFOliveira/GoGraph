package sim

import (
	"context"
	"fmt"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// defaultTickSize is the simulated duration of one tick (1ms by convention).
const defaultTickSize = time.Millisecond

// defaultCheckEvery is the default invariant-check cadence: check after every
// operation.
const defaultCheckEvery = 1

// defaultCheckpointEvery is the default tick cadence between in-loop checkpoints
// when [CheckpointConfig.Every] is not set. It is short enough that a default
// crash-storm budget sees several checkpoint+crash cycles exercising the full
// snapshot+WAL recovery path.
const defaultCheckpointEvery = 40

// defaultCheckpointDir is the SimDisk directory the snapshot and WAL live under
// when [CheckpointConfig.Dir] is not set. The WAL is placed at <dir>/wal and the
// snapshot at <dir>/snapshot so recovery uses the full snapshot+WAL path.
const defaultCheckpointDir = "db"

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
	// Workload is the actor mix. When nil, [DefaultWorkload] is used.
	Workload *Workload
	// OnOp, when non-nil, is called synchronously with each tick and the
	// operation about to run, before it is executed. It is an observation hook
	// (e.g. for verbose tracing); it must not mutate state or draw from any
	// randomness, or it would break reproducibility. It runs on the simulation
	// goroutine.
	OnOp func(tick int64, op Op)
	// OnCrash, when non-nil, is called synchronously after each crash+recovery
	// cycle with the crash tick and how many WAL ops recovery replayed. Like
	// OnOp it is an observation hook and must not mutate state or draw
	// randomness.
	OnCrash func(tick int64, replayedWALOps int)
	// Checkpoint configures deterministic, in-loop checkpointing of the
	// SimDisk-backed store. The zero value disables it (Enabled == false), so a
	// run that does not opt in is byte-identical to before. When enabled, the
	// durable store is opened in FULL-STACK mode (WAL + snapshot under a checkpoint
	// directory) and the loop publishes a real snapshot + truncates the WAL prefix
	// every [CheckpointConfig.Every] ticks, so a subsequent crash recovers through
	// the full snapshot+WAL path. It requires the durable store, so it implies the
	// SimDisk-backed stack even when [Config.Crash] is disabled. See
	// [CheckpointConfig].
	Checkpoint CheckpointConfig
	// EngineOpts configures the in-memory engine the deterministic loop drives
	// (the non-crash, non-disk path). The zero value is byte-identical to
	// [cypher.NewEngine], so a scenario that does not set it is unaffected. The
	// mem-pressure scenario clamps the logical-resource budgets here
	// (MaxResultRows / MaxResultBytes / MaxCollectItems) so over-budget ops are
	// refused with a typed error, exercising graceful degradation deterministically.
	// It is applied only on the in-memory path; the durable (crash/disk) path uses
	// the recovery-config engine.
	EngineOpts cypher.EngineOptions
	// Crash configures deterministic crash/recovery injection. The zero value
	// disables it (Enabled == false), which is the safe default: a run that does
	// not opt in drives a plain in-memory engine exactly as before, byte for
	// byte. When enabled, the simulator instead drives a real SimDisk-backed
	// persistence stack (WAL append+sync + recovery replay) so a scheduled crash
	// drops the live engine and the store is reopened from the durable image.
	Crash CrashConfig
	// Disk, when its CapacityBytes > 0, bounds the SimDisk-backed durable store
	// to a finite size so the run drives the engine through a disk-full (ENOSPC)
	// condition on the real WAL append+sync path. A non-zero capacity implies the
	// durable store even when Crash is disabled; the zero value leaves the disk
	// unbounded (the prior behaviour). See [DiskConfig].
	Disk DiskConfig
	// Multigraph, when true, opens the engine's graph (durable and in-memory
	// alike) as a directed MULTIGRAPH, so a repeated CREATE between the same
	// endpoints adds a parallel edge instance instead of being rejected. Only a
	// scenario whose oracle models edges per instance may set it (the
	// edge-properties scenario, rmp #2449, discriminates instances by a unique
	// eid property); the default (false) keeps the simple-graph shape every
	// other scenario's (src,dst,label)-keyed oracle model requires, byte for
	// byte.
	Multigraph bool
	// Seed is the master seed; the entire run is a pure function of it.
	Seed uint64
	// MaxTicks is the number of ticks (operations) the safety phase runs.
	MaxTicks int
	// CheckEvery is the invariant-check cadence in ticks. Values <= 0 are
	// normalised to 1 (check every tick).
	CheckEvery int
}

// CheckpointConfig parameterises deterministic in-loop checkpointing. The zero
// value disables it (Enabled == false), the safe default: a run that does not
// opt in never checkpoints and keeps the legacy WAL-only durable layout.
type CheckpointConfig struct {
	// Dir is the SimDisk directory the snapshot and WAL live under. A non-empty
	// dir places the WAL at dir/wal and the snapshot at dir/snapshot. When empty
	// it falls back to [defaultCheckpointDir].
	Dir string
	// Every is the tick cadence between checkpoints. A non-positive value falls
	// back to [defaultCheckpointEvery]. The first checkpoint fires at the first
	// tick that is a positive multiple of Every.
	Every int
	// Enabled turns in-loop checkpointing on. When true the durable store is
	// opened in full-stack mode and the loop checkpoints on the cadence below.
	Enabled bool
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
	// FaultRate is the probability (clamped to [0,1]) that any individual Sync on
	// the SimDisk-backed durable store fails with [ErrSimFault] and that a freshly
	// written sector is marked faulted (a torn write). It is threaded into the
	// disk the durable-mode [New] path drives; the zero value (the default every
	// scenario carries) disables it, keeping the disk fault-free and every
	// existing scenario byte-identical. It takes effect only on the durable path
	// — the one a non-zero [DiskConfig.CapacityBytes], [Config.Crash] or
	// [Config.Checkpoint] selects — because the plain in-memory engine path never
	// touches the disk.
	FaultRate float64
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
	// crash is the deterministic crash scheduler. It is always non-nil but is
	// inert (never fires, never draws) when Config.Crash.Enabled is false.
	crash *CrashSchedule
	// store is the live SimDisk-backed persistence stack the simulator drives in
	// crash mode; nil when crashes are disabled (the engine is then a plain
	// in-memory engine with no durable layer). On a crash it is reopened from the
	// durable SimDisk image via real recovery.
	store *SimStore
	// memGraph is the plain in-memory graph the non-durable path builds, retained
	// so [Simulator.graph] can reach the live graph in BOTH modes. It is nil
	// whenever store is non-nil (the durable path owns its graph).
	memGraph *lpg.Graph[string, float64]
	clock    *VirtualClock
	disk     *SimDisk
	oracle   *GraphOracle
	checker  *InvariantChecker
	workload *Workload
	engine   *EngineAdapter
	seed     *Seed
	cfg      Config
	// crashCount and replayedOps accumulate run statistics for reports and tests.
	crashCount  int
	replayedOps int
	// rejectedWrites counts write-shaped operations the engine did NOT commit
	// (committed == false). Under the disk-full scenario this is the non-vacuity
	// guard that ENOSPC actually fired: an honest write fails only when the
	// durable WAL path could not persist it. The oracle stays frozen for each.
	rejectedWrites int
	// rejectedReads counts read-shaped operations whose result drain returned an
	// error (committed == false). Under the mem-pressure scenario this is the
	// non-vacuity guard that a logical-resource budget actually fired: an
	// over-budget read is refused with a typed error and changes no state, so the
	// oracle stays in lock-step.
	rejectedReads int
	// searchEvery, when > 0, runs the full search-algorithm battery
	// ([CheckSearch]) every searchEvery ticks in the deterministic loop, on top of
	// any terminal check the scenario runs. It lives on the Simulator (not Config)
	// because the battery is far more expensive than the per-tick parity probe and
	// only the search scenario opts in; runDeterministic sets it from the
	// scenario. Zero (the default) disables periodic search checks.
	searchEvery int
	// checkpointEvery, when > 0, drives a real snapshot+WAL-truncate checkpoint of
	// the SimDisk-backed store every checkpointEvery ticks (see
	// [Simulator.maybeCheckpoint]). It is set from [Config.Checkpoint] in [New] and
	// is 0 (disabled) for any run that does not opt in. When enabled the store is
	// opened in full-stack mode so a subsequent crash recovers via the full
	// snapshot+WAL path.
	checkpointEvery int
	// checkpointCount accumulates the number of successful in-loop checkpoints for
	// reports and tests.
	checkpointCount int
	// boundary holds the measurements of the most recent FORCED crossing of the
	// snapshot boundary ([Simulator.crossSnapshotBoundary], rmp #2468) — the WAL
	// image before and after the checkpoint and the ops the following recovery
	// replayed. Its zero value (crossed == false) means the run never crossed,
	// which [checkSnapshotSourcedRecovery] reports as a violation rather than
	// treating as "nothing to check".
	boundary snapshotBoundary
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
	// FaultRate is threaded from the config (0 — the default — leaves the disk
	// fault-free, byte-identical to the pre-FaultRate behaviour). It bites only on
	// the durable path selected below; the plain in-memory path never opens the disk.
	disk := NewSimDisk(NewSeed(cfg.Seed^diskSeedMix), cfg.Disk.FaultRate)

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

	if cfg.Crash.Enabled || cfg.Disk.CapacityBytes > 0 || cfg.Checkpoint.Enabled {
		// Durable mode: drive the real SimDisk-backed persistence stack so a crash
		// can drop the engine and recovery can replay the durable WAL bytes, and so
		// a finite disk can drive the WAL append+sync path through ENOSPC. A
		// non-zero disk capacity, or in-loop checkpointing, opts in even without
		// crashes.
		storeCfg := simulatorStoreConfig()
		// A per-instance-modelling scenario opts into parallel edges; every other
		// scenario keeps the simple-graph base shape (see [Config.Multigraph]).
		storeCfg.graphConfig.Multigraph = cfg.Multigraph
		if cfg.Checkpoint.Enabled {
			// Full-stack layout: WAL + snapshot under a checkpoint directory so a
			// real Checkpointer can publish a snapshot and truncate the WAL prefix,
			// and a crash recovers through the full snapshot+WAL path.
			dir := cfg.Checkpoint.Dir
			if dir == "" {
				dir = defaultCheckpointDir
			}
			storeCfg.dir = dir
			s.checkpointEvery = cfg.Checkpoint.Every
			if s.checkpointEvery <= 0 {
				s.checkpointEvery = defaultCheckpointEvery
			}
		}
		store, err := OpenSimStore(disk, storeCfg)
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
		// layer, byte-identical to the pre-crash simulator. EngineOpts is the zero
		// value for every scenario except mem-pressure (which clamps the logical
		// budgets); a zero EngineOptions is byte-identical to cypher.NewEngine.
		g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: cfg.Multigraph})
		s.memGraph = g
		s.engine = NewEngineAdapter(cypher.NewEngineWithOptions(g, cfg.EngineOpts))
	}

	return s, nil
}

// graph returns the live graph the engine writes through, in either mode: the
// recovered store's graph on the durable path (which [Simulator.maybeCrash]
// replaces wholesale on every recovery, so this must never be cached across a
// crash) and the retained in-memory graph otherwise.
//
// It exists for the few checks that must read the engine's own durable IDENTITY
// rather than a Cypher projection — the stable edge handles and node ids the
// handle-collision fixture (rmp #2515) is built on, which no query surfaces.
func (s *Simulator) graph() *lpg.Graph[string, float64] {
	if s.store != nil {
		return s.store.Graph()
	}
	return s.memGraph
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

		// Checkpoint for this tick BEFORE the crash decision, so a checkpoint that
		// lands just before a crash is the realistic ordering the full snapshot+WAL
		// recovery path must survive: the snapshot folds (and the WAL prefix loses)
		// the committed prefix, then the crash drops the engine and recovery must
		// rebuild from snapshot + the WAL suffix. A checkpoint failure is a hard
		// durability fault the run must surface.
		if err := s.maybeCheckpoint(tick); err != nil {
			return nil, err
		}

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

		committed, counters := s.executeCounted(ctx, op)
		if !committed {
			if op.Kind.IsWrite() {
				s.rejectedWrites++
			} else {
				s.rejectedReads++
			}
		}
		// Per-op counters oracle (#2448): the engine's effect report for this op
		// must match the effect the oracle predicts, adjudicated on the pre-apply
		// model — so it runs between execute and applyToOracle.
		if violations := CheckOpCounters(tick, op, committed, counters, s.oracle); len(violations) > 0 {
			return s.report(tick, op, violations), nil
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

// schemaChurnStep pairs one idempotent churn DDL statement with the mutation
// it implies on the harness's schema model, so the introspection oracle
// (rmp #2455) can hold SHOW INDEXES / SHOW CONSTRAINTS and the db.* procedures
// to the model after every flip.
type schemaChurnStep struct {
	ddl   string
	apply func(m *SchemaModel)
}

// schemaChurnSteps is the fixed, idempotent DDL cycle runWithDDL rotates
// through: drop and re-create the (:Person).name index, and drop and
// re-create a UNIQUE constraint on a label the workload never writes
// (:Contact), so SHOW CONSTRAINTS churns too without perturbing the honest
// write stream. Idempotent forms (IF [NOT] EXISTS) make each step a clean
// no-op when it races nothing, so the churn never errors on a benign
// re-create/re-drop. The constraint create deliberately uses the modern
// FOR ... REQUIRE grammar (the legacy ON ... ASSERT spelling runs in the
// constraint scenarios and the wire SchemaChanger), and the index create is
// OPTIONS-free, so it exercises the default (hash) kind.
var schemaChurnSteps = []schemaChurnStep{
	{
		ddl:   "CREATE CONSTRAINT sim_contact_email_uq IF NOT EXISTS FOR (n:Contact) REQUIRE n.email IS UNIQUE",
		apply: func(m *SchemaModel) { m.AddUniqueConstraint("sim_contact_email_uq", "Contact", "email") },
	},
	{
		ddl:   "DROP CONSTRAINT sim_contact_email_uq IF EXISTS",
		apply: func(m *SchemaModel) { m.DropConstraint("sim_contact_email_uq") },
	},
	{
		ddl:   "DROP INDEX sim_person_name IF EXISTS",
		apply: func(m *SchemaModel) { m.DropIndex("sim_person_name") },
	},
	{
		ddl:   "CREATE INDEX sim_person_name FOR (n:Person) ON (n.name)",
		apply: func(m *SchemaModel) { m.AddIndex("sim_person_name", SchemaIndexHash, "Person", "name") },
	},
}

// runWithDDL is the schema-chaos variant of [Simulator.Run]: it drives the same
// deterministic tick loop but, every ddlEvery ticks, issues the next idempotent
// DDL statement from [schemaChurnSteps] against the live engine, churning the
// index and a constraint under the honest write load. After each DDL it
// advances model in lock-step and runs the schema-introspection oracle
// ([CheckSchemaIntrospection], rmp #2455), so a churn flip whose effect the
// introspection surfaces do not reflect fails immediately. Like Run it returns
// a populated report on the first invariant violation, or (nil, nil) on clean
// completion. The DDL cadence is positional (tick-driven), so the run stays a
// deterministic function of the seed.
func (s *Simulator) runWithDDL(ctx context.Context, ddlEvery int, model *SchemaModel) (*SimReport, error) {
	churnIdx := 0
	var lastTick int64
	var lastOp Op
	for i := 0; i < s.cfg.MaxTicks; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		tick := s.clock.Tick()

		if ddlEvery > 0 && tick%int64(ddlEvery) == 0 {
			step := schemaChurnSteps[churnIdx%len(schemaChurnSteps)]
			churnIdx++
			if err := s.engineRunDDL(ctx, step.ddl); err != nil {
				return nil, fmt.Errorf("sim: schema churn DDL %q at tick %d: %w", step.ddl, tick, err)
			}
			step.apply(model)
			if violations := CheckSchemaIntrospection(tick, model, s.engine); len(violations) > 0 {
				return s.report(tick, Op{Kind: OpMatch, Cypher: "<post-DDL schema introspection>"}, violations), nil
			}
		}

		actor := s.workload.SelectActor(s.seed)
		op := actor.NextOp(s.seed, s.oracle)
		if s.cfg.OnOp != nil {
			s.cfg.OnOp(tick, op)
		}
		committed, counters := s.executeCounted(ctx, op)
		// Per-op counters oracle (#2448), on the pre-apply model as in [Simulator.Run].
		if violations := CheckOpCounters(tick, op, committed, counters, s.oracle); len(violations) > 0 {
			return s.report(tick, op, violations), nil
		}
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
	// HOST crash ([SimDisk.Crash] is [SimDisk.CrashHost]): discard the live
	// engine and store WITHOUT a graceful close, and discard from the SimDisk
	// every byte no successful fsync covered — both the frames still sitting in
	// the WAL's bufio buffer and those written through to the disk but never
	// fsync'd. Only the fsync'd WAL prefix survives.
	//
	// Revoke not-yet-durable directory entries too (#1811): a real power-loss
	// crash loses any create/rename whose parent directory was never fsync'd.
	// SimDisk.Crash() models exactly that. The dedicated crash tests already
	// call it; invoking it here exercises the dirent-revocation model in the
	// INTEGRATED crash-storm / full-stack loop, so a future async-checkpoint or
	// mid-publish window cannot silently promote a snapshot a real crash would
	// have lost. Harmless under the current synchronous-checkpoint ordering.
	s.disk.Crash()
	// Reopen with the SAME store configuration the crashed store used — crucially
	// the same durable layout. In full-stack mode (cfg.dir set) this reopens the
	// WAL at dir/wal and recovers via the full snapshot+WAL path; reopening with
	// the default WAL-only config here would point recovery at the wrong (empty)
	// root-level WAL key and drop every committed op.
	store, err := OpenSimStore(s.disk, s.store.Config())
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

// maybeCheckpoint runs a real snapshot+WAL-truncate checkpoint of the
// SimDisk-backed store when in-loop checkpointing is enabled and the tick is on
// the configured cadence. It is a cheap no-op when checkpointing is disabled
// (checkpointEvery == 0) or there is no durable store. A checkpoint publishes a
// self-sufficient snapshot to <dir>/snapshot and prefix-truncates the WAL, so
// the NEXT crash recovery exercises the full [recovery.OpenFS] snapshot+WAL path
// rather than WAL-only replay.
//
// A checkpoint error is returned to the caller as a hard fault: a checkpoint
// that cannot publish its snapshot or truncate the WAL is a durability failure
// the run must surface, never swallow. This is intentionally STRICTER than the
// production background checkpointer ([checkpoint.Checkpointer.Start]/Trigger),
// whose periodic fires record the error in Stats and retry on the next cadence
// (a transient ENOSPC merely defers WAL reclamation without compromising
// durability); the simulator must not mask a durability-machinery break, so it
// fails the run.
func (s *Simulator) maybeCheckpoint(tick int64) error {
	if s.checkpointEvery <= 0 || s.store == nil {
		return nil
	}
	if tick <= 0 || tick%int64(s.checkpointEvery) != 0 {
		return nil
	}
	if err := s.store.Checkpoint(); err != nil {
		return fmt.Errorf("sim: checkpoint at tick %d: %w", tick, err)
	}
	s.checkpointCount++
	return nil
}

// forceCrash performs ONE crash+recovery cycle unconditionally and runs the
// harness's own durability check on the recovered engine. opLabel is the report
// op the violation (if any) is attributed to, so a failure names the scenario
// that forced the crash.
//
// It mirrors [Simulator.maybeCrash]'s body — a HOST crash on the SimDisk, a
// reopen with the SAME store configuration the crashed store used (crucially the
// same durable layout, or recovery would point at an empty root-level WAL and
// drop every committed op), a rebind of the engine adapter, and
// [InvariantChecker.CheckDurability] — because that is the sequence whose
// semantics a post-recovery clause assumes.
//
// It exists so a scenario's post-recovery coverage claim can rest on a
// CONSTRUCTED crash rather than on one the seeded schedule may or may not have
// drawn inside the budget. On a run that already crashed the caller skips it, so
// the forced arm never changes a run that already had coverage.
//
// A run with no durable layer has no recovery to construct and gets (nil, nil);
// the caller's non-vacuity gate is what reports the missing coverage.
func (s *Simulator) forceCrash(tick int64, opLabel string) (*SimReport, error) {
	if s.store == nil {
		return nil, nil
	}
	storeCfg := s.store.Config()
	s.store.Crash()
	store, err := OpenSimStore(s.disk, storeCfg)
	if err != nil {
		return nil, fmt.Errorf("sim: forced crash recovery (%s) at tick %d: %w", opLabel, tick, err)
	}
	s.store = store
	s.engine = NewEngineAdapter(store.Engine())
	s.crashCount++
	s.replayedOps += store.WALOps()
	if v := s.checker.CheckDurability(tick, s.oracle, s.engine); len(v) > 0 {
		return s.report(tick, Op{Kind: OpMatch, Cypher: opLabel}, v), nil
	}
	return nil, nil
}

// checkCheckpointsFired is the assert-something-was-seen gate for any scenario
// that opts into in-loop checkpointing: it reports a violation when the run
// published no checkpoint at all.
//
// It exists because a [CheckpointConfig] is INERT unless the run loop actually
// calls [Simulator.maybeCheckpoint] — which only [Simulator.Run] does
// automatically, so every custom run loop must wire the call itself (rmp
// #2457/#2464). Without this gate a scenario could carry a checkpoint
// configuration, never publish a snapshot, and still pass: the snapshot
// recovery path it claims to exercise would simply never run. The gate is
// deliberately UNCONDITIONAL on the configuration — it fires just as loudly
// when the configuration was removed as when the call was — so it cannot be
// silenced by the very change it is there to catch.
func (s *Simulator) checkCheckpointsFired(tick int64) []Violation {
	if s.checkpointCount > 0 {
		return nil
	}
	return []Violation{{
		Kind: ViolationACIDDurability, Tick: tick, Op: "checkpoint non-vacuity",
		Message: "the run published NO checkpoint: the snapshot recovery path was never exercised" +
			" (either Config.Checkpoint is disabled or the run loop never calls maybeCheckpoint)",
	}}
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
	committed, _ := s.executeCounted(ctx, op)
	return committed
}

// executeCounted runs op exactly like [Simulator.execute] and additionally
// returns the engine's per-statement write-effect counters, read from the SAME
// drained result the tick executed — never from an extra query. The counters
// are nil when the statement failed before producing a result, or when the op
// ran on the read path (a read-only statement reports nil counters by the
// engine contract). Reading them draws no randomness, so a run that feeds them
// to [CheckOpCounters] stays byte-identical whenever the check finds nothing.
func (s *Simulator) executeCounted(ctx context.Context, op Op) (bool, *exec.QueryCounters) {
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
		return false, nil
	}
	for res.Next() {
	}
	// A drain error after the statement was accepted means the result did not
	// fully materialise; treat it as not-committed so the oracle does not model
	// an effect the engine may not have durably applied.
	drainErr := res.Err()
	var counters *exec.QueryCounters
	if cr, ok := res.(counterReporter); ok {
		counters = cr.Counters()
	}
	_ = res.Close()
	return drainErr == nil, counters
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

// CheckpointCount returns how many in-loop checkpoints the run published (always
// 0 when checkpointing is disabled). Each checkpoint published a self-sufficient
// snapshot and truncated the WAL prefix it folded.
func (s *Simulator) CheckpointCount() int { return s.checkpointCount }

// Disk returns the SimDisk backing the durable store, for tests that inspect the
// durable image (e.g. asserting a snapshot directory exists after a checkpoint).
// It is non-nil whenever a durable store is in use.
func (s *Simulator) Disk() *SimDisk { return s.disk }

// RejectedWrites returns how many write-shaped operations the engine did not
// commit during the run. Under the disk-full scenario it is the non-vacuity
// guard that ENOSPC fired; it is 0 for a run that never exhausts the disk.
func (s *Simulator) RejectedWrites() int { return s.rejectedWrites }

// RejectedReads returns how many read-shaped operations the engine refused
// (drain error) during the run. Under the mem-pressure scenario it is the
// non-vacuity guard that a logical-resource budget fired.
func (s *Simulator) RejectedReads() int { return s.rejectedReads }

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
