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

	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	// The checker samples from its own seed, derived from the master seed, so
	// that changing the check cadence (CheckEvery) never perturbs the workload
	// draw stream: the actor/op/param sequence stays a pure function of cfg.Seed
	// alone, independent of how often invariants are checked.
	checkerSeed := NewSeed(cfg.Seed ^ checkerSeedMix)

	return &Simulator{
		cfg:      cfg,
		seed:     seed,
		clock:    NewVirtualClock(defaultTickSize),
		disk:     NewSimDisk(NewSeed(cfg.Seed^diskSeedMix), 0), // built now; engine wiring is Phase 2.
		oracle:   NewGraphOracle(),
		checker:  NewInvariantChecker(checkerSeed),
		workload: wl,
		engine:   NewEngineAdapter(eng),
	}, nil
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
	for i := 0; i < s.cfg.MaxTicks; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		tick := s.clock.Tick()

		actor := s.workload.SelectActor(s.seed)
		op := actor.NextOp(s.seed, s.oracle)

		if s.cfg.OnOp != nil {
			s.cfg.OnOp(tick, op)
		}

		s.execute(ctx, op)
		s.applyToOracle(op)

		if tick%int64(s.cfg.CheckEvery) == 0 {
			if violations := s.checker.Check(tick, s.oracle, s.engine); len(violations) > 0 {
				return s.report(tick, op, violations), nil
			}
		}
	}
	return nil, nil
}

// execute runs op against the engine via the read or write path per its kind.
// Engine errors are not treated as violations here: an honest workload may
// legitimately hit a typed engine error (and the checker catches any resulting
// state divergence). The result is always drained and closed so no resources
// leak across ticks.
func (s *Simulator) execute(ctx context.Context, op Op) {
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
		return
	}
	for res.Next() {
	}
	_ = res.Err()
	_ = res.Close()
}

// applyToOracle advances the shadow model for op per its kind.
func (s *Simulator) applyToOracle(op Op) {
	switch op.Kind {
	case OpCreate:
		s.oracle.ApplyCreate(op.Cypher, op.Params)
	case OpMerge:
		s.oracle.ApplyMerge(op.Cypher, op.Params)
	case OpDelete:
		s.oracle.ApplyDelete(op.Cypher, op.Params)
	case OpMatch, OpUpdate:
		// OpUpdate (SET) and OpMatch (reads, incl. the SET template) are both
		// modelled by ApplyMatch, which routes the SET template to its handler
		// and treats every other MATCH as a pure read.
		s.oracle.ApplyMatch(op.Cypher, op.Params)
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
