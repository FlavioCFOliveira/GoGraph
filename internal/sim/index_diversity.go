package sim

import (
	"context"
	"fmt"
)

// indexDiversitySpecs are the indexes the index-diversity scenario creates and
// cross-checks: a HASH index on a string property, a BTREE index on a numeric
// property, and a BTREE index on a second string property. Together they cover
// both index kinds and both string and integer value types; the seek-vs-scan
// consistency invariant is identical for each, so a divergence on any one is a
// real index bug for that (kind, value-type).
var indexDiversitySpecs = []IndexSpec{
	{Label: "Person", Property: "name"},               // hash, string
	{Label: "Person", Property: "age", Numeric: true}, // btree, numeric
	{Label: "Person", Property: "city"},               // btree, string
}

// indexDiversityDDL creates the three indexes. They are declared AFTER the bulk
// load so each backfill runs over an above-threshold graph, engaging the
// morsel-parallel phase-2 of the backfill (the > backfillParallelMinNodes path).
var indexDiversityDDL = []string{
	"CREATE INDEX idx_person_name FOR (n:Person) ON (n.name) OPTIONS {indexType:'hash'}",
	"CREATE INDEX idx_person_age FOR (n:Person) ON (n.age) OPTIONS {indexType:'btree'}",
	"CREATE INDEX idx_person_city FOR (n:Person) ON (n.city) OPTIONS {indexType:'btree'}",
}

// indexDiversityBulk is the number of Person nodes the scenario bulk-loads before
// creating the indexes — comfortably above backfillParallelMinNodes (8192) so the
// parallel backfill phase is exercised.
const indexDiversityBulk = 9000

// indexDiversityScenario verifies index-type diversity and the parallel backfill
// under the DST: it bulk-loads an above-threshold Person graph, creates a HASH
// (string), a BTREE (numeric), and a BTREE (string) index — each backfilled
// through the morsel-parallel phase — then churns writes with crash/recovery,
// running the thorough seek-vs-scan index-consistency check throughout and after
// every recovery. It also carries the access-path parity and plan-stability
// oracles (rmp #2447): literal and parameterised predicates must agree on
// results and on the physical access path, and a fixed probe set must re-plan
// byte-identically after every plan-cache rebuild. On top of those it carries
// the seek-result diversity oracle (rmp #2450): bounded and half-open ranges,
// STARTS WITH, and IN-shaped predicates — in literal and parameterised
// spellings — must reproduce, as id-multisets and counts, an independent
// full-scan reference filtered client-side — and the statistics-regime oracle
// (rmp #2456): CALL db.stats.refresh() is driven before the crash window,
// after every recovery, and at seed-chosen ticks, pinning the procedure's
// row/throttle contract, result identity across every refresh, and the
// post-crash empty-collector regime. It is bit-reproducible (the parallel
// backfill produces identical index contents regardless of worker
// scheduling; the refresh outcome at the deterministic points is a pure
// function of engine lifetime).
//
// Checkpointing is enabled (rmp #2464) so the three index DEFINITIONS cross the
// snapshot boundary: a checkpoint truncates the WAL prefix that declared them,
// after which they can only be recovered from the snapshot's indexdefs.bin and
// indexes/ components. Before that this scenario ran WAL-only, so every
// post-recovery index-consistency and introspection check was validating a
// REPLAYED CREATE INDEX rather than a snapshot-loaded definition. A terminal
// gate ([Simulator.checkCheckpointsFired]) requires the checkpoints to have
// actually fired, because a [CheckpointConfig] is inert unless the custom run
// loop calls [Simulator.maybeCheckpoint].
//
// On top of the definitions it now covers the index CONTENTS across that
// boundary (rmp #2490). A recovered index is populated either by DESERIALIZING
// the snapshot's indexes/<name>.bin payload or by rebuilding from the recovered
// graph, decided per index against three preconditions; the two constructed arms
// of [indexHydrationArms] drive one reopen down each branch and assert the
// engine-scoped population counters in BOTH directions, because a hydrated index
// and a rebuilt one are — by contract — indistinguishable in their answers.
// It also carries the intersect-planner probes ([IndexIntersectProbes]): a
// two-predicate conjunction over the two BTREE-indexed properties, which is the
// only shape that reaches the planner's bitmap composition and its budgeted
// RangeCount / RangeCountFrom gate.
func indexDiversityScenario() Scenario {
	return Scenario{
		Name:        ScenarioIndexDiversity,
		Description: "hash + btree + numeric indexes, parallel backfill, seek-vs-scan consistency through checkpoint + crash/recovery",
		Mode:        ModeDeterministic,
		DefaultSeed: 0x10DE5,
		MaxTicks:    260,
		Crash:       CrashConfig{Enabled: true, CrashProb: 1.0 / 60.0, StabilityWindow: 20},
		// Well inside the crash stability window, so most crashes follow at least
		// one snapshot+WAL-truncate and recover through [recovery.OpenFS].
		Checkpoint: CheckpointConfig{Enabled: true, Every: 40},
		run:        runIndexDiversity,
	}
}

// runIndexDiversity bulk-loads the graph, creates the diverse indexes (parallel
// backfill), then drives a churn loop that maintains the indexed properties and
// crashes periodically. It runs the seek-vs-scan consistency check after the
// initial backfill, periodically during churn, and immediately after every
// crash/recovery — and, at the same cadence, the schema-introspection oracle
// ([CheckSchemaIntrospection], rmp #2455), which holds SHOW INDEXES and
// db.indexes() to the harness's model of the three declared indexes (name,
// kind, label, property), so a recovery that re-registers an index with the
// wrong shape fails immediately — plus the access-path parity check and
// the seek-result diversity check ([IndexSeekResults], whose terminal Finish
// asserts the run was not vacuous), plus the plan-stability check after every
// recovery and at the end (the probe set and baseline are fixed at scenario
// start; see [indexDiversityParityProbes] and [CapturePlanBaseline]), plus
// the statistics-regime oracle ([StatsRegime], rmp #2456): db.stats.refresh()
// runs once before the crash window (with a back-to-back throttle probe),
// once after every recovery (the recovered engine must start with an empty
// collector and immediately allow a rebuild), and at seed-chosen ticks, with
// the probe battery asserted result-identical across every refresh and a
// terminal non-vacuity Finish. It crashes WITHOUT the oracle durability check (the bulk nodes
// are not modelled in the minimal oracle; this scenario's invariant is engine
// self-consistency — the index agreeing with its own base data — not oracle
// parity). It is deterministic.
func runIndexDiversity(ctx context.Context, seed uint64) (*SimReport, error) {
	sc := indexDiversityScenario()
	sm, report, err := runIndexDiversitySim(ctx, seed, sc.DeterministicConfig(seed), indexDiversityBulk,
		&indexDiversityEvidence{})
	if sm != nil {
		defer func() { _ = sm.Close() }()
	}
	return report, err
}

// indexDiversityEvidence is what the run MEASURED about the two surfaces rmp
// #2490 added, handed back so a test asserts on numbers rather than on the mere
// absence of a violation.
//
// It follows the shape the snapshot-corruption battery already uses: the
// checkers themselves ARE the record, so a test reads the very counters the
// terminal non-vacuity gates read and cannot drift from them.
type indexDiversityEvidence struct {
	// arms is the deserialize-not-rebuild arm pair with the population counters
	// each of its reopens measured. Nil until the loop has constructed it.
	arms *indexHydrationArms
	// intersect is the intersect-planner probe set with its composed / rows /
	// solo-control tallies. Nil until the loop has constructed it.
	intersect *IndexIntersectProbes
}

// runIndexDiversitySim is [runIndexDiversity] over an explicit [Config] and bulk
// size, with the simulator handed back instead of closed. It is split out so a
// short-layer test can drive the SAME loop — and therefore the same
// checkpoint/crash wiring and the same terminal gates — at a budget the short
// layer can afford, and can then assert on what the run actually exercised. The
// full-scale scenario (an above-threshold graph engaging the morsel-parallel
// backfill) stays soak-gated; see index_diversity_test.go. The caller owns
// closing the returned simulator, which is non-nil whenever construction
// succeeded.
func runIndexDiversitySim(
	ctx context.Context, seed uint64, cfg Config, bulk int, ev *indexDiversityEvidence,
) (*Simulator, *SimReport, error) {
	sm, err := New(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("sim: index-diversity new: %w", err)
	}
	report, rerr := indexDiversityLoop(ctx, sm, seed, cfg, bulk, ev)
	return sm, report, rerr
}

// indexDiversityLoop is the scenario body, over a simulator the caller owns and
// closes. Splitting it from [runIndexDiversitySim] keeps every violation exit a
// plain two-value return.
func indexDiversityLoop(
	ctx context.Context, sm *Simulator, seed uint64, cfg Config, bulk int, ev *indexDiversityEvidence,
) (*SimReport, error) {
	// Bulk-load an above-threshold Person graph with string + numeric properties.
	for i := 0; i < bulk; i++ {
		q := fmt.Sprintf("CREATE (:Person {name:'p%d', age:%d, city:'c%d'})", i, i%500, i%100)
		if !sm.execute(ctx, Op{Kind: OpCreate, Cypher: q}) {
			return nil, fmt.Errorf("sim: index-diversity bulk load failed at %d", i)
		}
	}
	// Create the diverse indexes; each backfills the 9000-node graph in parallel.
	for _, ddl := range indexDiversityDDL {
		if err := sm.engineRunDDL(ctx, ddl); err != nil {
			return nil, fmt.Errorf("sim: index-diversity DDL %q: %w", ddl, err)
		}
	}
	// Schema-introspection model (rmp #2455): the three declared indexes, held
	// against SHOW INDEXES / db.indexes() after the backfill, after every
	// recovery (a recovered-DDL divergence check), periodically, and at the end.
	model := NewSchemaModel()
	model.AddIndex("idx_person_name", SchemaIndexHash, "Person", "name")
	model.AddIndex("idx_person_age", SchemaIndexBTree, "Person", "age")
	model.AddIndex("idx_person_city", SchemaIndexBTree, "Person", "city")
	// Consistency right after the parallel backfill.
	if v := CheckIndexConsistency(0, nil, sm.engine, indexDiversitySpecs...); len(v) > 0 {
		return sm.report(0, Op{Kind: OpMatch, Cypher: "<post-backfill index check>"}, v), nil
	}
	if v := CheckSchemaIntrospection(0, model, sm.engine); len(v) > 0 {
		return sm.report(0, Op{Kind: OpMatch, Cypher: "<post-backfill schema introspection>"}, v), nil
	}

	// Intersect-planner probes (rmp #2490): a two-predicate conjunction over the
	// two BTREE-indexed properties, which is the shape that reaches the planner's
	// bitmap composition and its budgeted RangeCount / RangeCountFrom gate. Drawn
	// from the checker's OWN sub-seed so the workload, crash, parity, seek-result
	// and statistics streams stay byte-identical. Constructed before the hydration
	// arms so the arms' post-recovery battery can include it.
	intersect := NewIndexIntersectProbes(NewSeed(seed^intersectSeedMix), bulk)
	ev.intersect = intersect
	if v := intersect.Check(0, sm.engine); len(v) > 0 {
		return sm.report(0, Op{Kind: OpMatch, Cypher: "<post-backfill intersect probe>"}, v), nil
	}

	// The two CONSTRUCTED recovery arms (rmp #2490). They run here — before the
	// parity probe set, the plan baseline and the statistics regime are primed —
	// so everything downstream is captured on the engine and graph the churn loop
	// will actually drive, and so the arms cannot perturb a baseline that was
	// taken before them. Each arm replaces sm.store and sm.engine with a reopened
	// pair, and each re-runs the scenario's own post-recovery battery: a hydrated
	// index must answer exactly as a rebuilt one does.
	arms := &indexHydrationArms{}
	ev.arms = arms
	armVerify := func(tick int64) []Violation {
		if v := CheckIndexConsistency(tick, nil, sm.engine, indexDiversitySpecs...); len(v) > 0 {
			return v
		}
		if v := CheckSchemaIntrospection(tick, model, sm.engine); len(v) > 0 {
			return v
		}
		return intersect.Check(tick, sm.engine)
	}
	if v, err := arms.runHydrateArm(sm, 0, armVerify); err != nil {
		return nil, err
	} else if len(v) > 0 {
		return sm.report(0, Op{Kind: OpMatch, Cypher: "<hydrate arm>"}, v), nil
	}
	if v, err := arms.runStaleArm(ctx, sm, 0, armVerify); err != nil {
		return nil, err
	} else if len(v) > 0 {
		return sm.report(0, Op{Kind: OpMatch, Cypher: "<stale arm>"}, v), nil
	}

	// Access-path parity probes (rmp #2447): drawn from their own sub-seed so
	// the probe set is a pure function of the run seed and consumes nothing
	// from the workload stream — the scenario's op/param sequence stays
	// byte-identical to the pre-parity behaviour. The same fixed set backs the
	// plan-stability baseline below.
	probes := indexDiversityParityProbes(NewSeed(seed^paritySeedMix), bulk)
	if v := CheckAccessPathParity(0, nil, sm.engine, probes...); len(v) > 0 {
		return sm.report(0, Op{Kind: OpMatch, Cypher: "<post-backfill access-path parity check>"}, v), nil
	}
	// Seek-result diversity probes (rmp #2450): range, prefix, and IN-shaped
	// predicates result-verified against an independent full-scan reference, in
	// literal and parameterised spellings. Windows come from the checker's own
	// sub-seed, so the workload stream stays byte-identical; the checker is
	// stateful so the terminal Finish can assert non-vacuity over the run.
	seekResults := NewIndexSeekResults(NewSeed(seed^seekResultsSeedMix), bulk)
	if v := seekResults.Check(0, sm.engine); len(v) > 0 {
		return sm.report(0, Op{Kind: OpMatch, Cypher: "<post-backfill seek-result check>"}, v), nil
	}
	// Plan-stability baseline: every later Explain of the same probes — above
	// all after a crash rebuilt the plan cache — must reproduce these renderings
	// byte-identically.
	planBase, err := CapturePlanBaseline(sm.engine, probes...)
	if err != nil {
		return nil, fmt.Errorf("sim: index-diversity plan baseline: %w", err)
	}

	// Statistics-driven planning regime (rmp #2456): the first refresh runs
	// HERE — before the crash window opens (StabilityWindow ticks) — so the
	// engine leaves its no-statistics regime at least once per run, and a
	// back-to-back second call pins the rate-limit refusal. The post-backfill
	// battery just above is the immediately-before capture; the battery below
	// re-verifies every seek answer and the parity probes on the refreshed
	// engine. Mid-run refresh ticks are drawn from the checker's own sub-seed
	// so the workload stream stays byte-identical.
	statsRegime := NewStatsRegime(probes...)
	statsSeed := NewSeed(seed ^ statsSeedMix)
	// The oracle set, bundled so the post-recovery battery is spelled once.
	checks := &indexDiversityChecks{
		sm: sm, model: model, probes: probes, planBase: planBase,
		seek: seekResults, stats: statsRegime, intersect: intersect, arms: arms,
	}
	if v := statsRegime.CheckRefresh(0, sm.engine, ExpectRebuild); len(v) > 0 {
		return sm.report(0, Op{Kind: OpMatch, Cypher: "<post-backfill stats refresh>"}, v), nil
	}
	if v := statsRegime.CheckRefresh(0, sm.engine, ExpectRefusal); len(v) > 0 {
		return sm.report(0, Op{Kind: OpMatch, Cypher: "<post-backfill stats refresh throttle>"}, v), nil
	}
	if v := seekResults.Check(0, sm.engine); len(v) > 0 {
		return sm.report(0, Op{Kind: OpMatch, Cypher: "<post-refresh seek-result check>"}, v), nil
	}
	if v := CheckAccessPathParity(0, nil, sm.engine, probes...); len(v) > 0 {
		return sm.report(0, Op{Kind: OpMatch, Cypher: "<post-refresh access-path parity check>"}, v), nil
	}

	churn := bulk
	for i := 0; i < cfg.MaxTicks; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		tick := sm.clock.Tick()

		// Checkpoint BEFORE the crash decision, matching [Simulator.Run]: a
		// checkpoint that lands just before a crash is the realistic ordering the
		// full snapshot+WAL recovery path must survive, and it is what makes the
		// index DEFINITIONS cross the snapshot boundary (rmp #2464) — the
		// truncated WAL prefix no longer holds the CREATE INDEX frames, so the
		// post-recovery checks below validate a snapshot-loaded schema.
		if err := sm.maybeCheckpoint(tick); err != nil {
			return nil, err
		}

		// Manual crash (no oracle durability check — see the doc comment). On
		// recovery the indexes are re-registered and then populated — hydrated
		// from the snapshot payload when all three preconditions hold, otherwise
		// rebuilt from the recovered graph (rmp #2490). Under this loop's churn
		// the WAL suffix almost always carries a :Person write, so almost every
		// crash here legitimately takes the REBUILD branch; which branch each
		// cycle took is recorded below rather than assumed, and the two branches
		// are driven deliberately by [indexHydrationArms] before the loop starts.
		// Either way the consistency check must still hold against the recovered
		// graph.
		if sm.crash.ShouldCrash(tick) {
			// Reopen with the SAME durable layout the crashed store used: under
			// checkpointing that is the full-stack layout (WAL at <dir>/wal beside
			// the snapshot), and reopening with the default WAL-only config would
			// point recovery at an empty root-level WAL and drop every committed op.
			storeCfg := sm.store.Config()
			sm.store.Crash()
			store, oerr := OpenSimStore(sm.disk, storeCfg)
			if oerr != nil {
				return nil, fmt.Errorf("sim: index-diversity crash recovery at tick %d: %w", tick, oerr)
			}
			sm.store = store
			sm.engine = NewEngineAdapter(store.Engine())
			// Record what recovery actually replayed, as [Simulator.maybeCrash]
			// does for the loops that use it: with checkpointing on, a crash that
			// follows a snapshot replays only the WAL suffix, so this is the
			// measured evidence of which recovery path each cycle took.
			sm.replayedOps += store.WALOps()
			sm.crashCount++
			if r := checks.postRecovery(tick, store); r != nil {
				return r, nil
			}
		}

		// Churn: create a fresh indexed Person, and periodically delete one, so the
		// bound indexes self-maintain on insert and delete.
		churn++
		create := fmt.Sprintf("CREATE (:Person {name:'q%d', age:%d, city:'c%d'})", churn, churn%500, churn%100)
		sm.execute(ctx, Op{Kind: OpCreate, Cypher: create})
		if sm.seed.Float64() < 0.3 {
			del := fmt.Sprintf("MATCH (n:Person {name:'q%d'}) DETACH DELETE n", churn-1)
			sm.execute(ctx, Op{Kind: OpDelete, Cypher: del})
		}

		if tick%40 == 0 {
			if v := CheckIndexConsistency(tick, nil, sm.engine, indexDiversitySpecs...); len(v) > 0 {
				return sm.report(tick, Op{Kind: OpMatch, Cypher: "<periodic index check>"}, v), nil
			}
			if v := CheckSchemaIntrospection(tick, model, sm.engine); len(v) > 0 {
				return sm.report(tick, Op{Kind: OpMatch, Cypher: "<periodic schema introspection>"}, v), nil
			}
			if v := CheckAccessPathParity(tick, nil, sm.engine, probes...); len(v) > 0 {
				return sm.report(tick, Op{Kind: OpMatch, Cypher: "<periodic access-path parity check>"}, v), nil
			}
			if v := seekResults.Check(tick, sm.engine); len(v) > 0 {
				return sm.report(tick, Op{Kind: OpMatch, Cypher: "<periodic seek-result check>"}, v), nil
			}
			if v := intersect.Check(tick, sm.engine); len(v) > 0 {
				return sm.report(tick, Op{Kind: OpMatch, Cypher: "<periodic intersect probe>"}, v), nil
			}
		}

		// Seed-chosen mid-run refresh probes (rmp #2456): the outcome is
		// wall-clock dependent (the rate limiter measures real time, and only
		// the first call of an engine lifetime is deterministically allowed),
		// so the probe accepts either verdict while still pinning the row
		// shape, the tracked-pairs observable, and result identity across the
		// call. The draw comes from the checker's own stream, consumed every
		// tick, so the probe ticks are a pure function of the run seed.
		if statsSeed.Float64() < 0.02 {
			if v := statsRegime.CheckRefresh(tick, sm.engine, ExpectEither); len(v) > 0 {
				return sm.report(tick, Op{Kind: OpMatch, Cypher: "<mid-run stats refresh probe>"}, v), nil
			}
		}
	}
	// Terminal consistency, introspection, parity, and plan-stability checks.
	if v := CheckIndexConsistency(int64(cfg.MaxTicks), nil, sm.engine, indexDiversitySpecs...); len(v) > 0 {
		return sm.report(int64(cfg.MaxTicks), Op{Kind: OpMatch, Cypher: "<terminal index check>"}, v), nil
	}
	if v := CheckSchemaIntrospection(int64(cfg.MaxTicks), model, sm.engine); len(v) > 0 {
		return sm.report(int64(cfg.MaxTicks), Op{Kind: OpMatch, Cypher: "<terminal schema introspection>"}, v), nil
	}
	if v := CheckAccessPathParity(int64(cfg.MaxTicks), nil, sm.engine, probes...); len(v) > 0 {
		return sm.report(int64(cfg.MaxTicks), Op{Kind: OpMatch, Cypher: "<terminal access-path parity check>"}, v), nil
	}
	if v := CheckPlanStability(int64(cfg.MaxTicks), planBase, sm.engine); len(v) > 0 {
		return sm.report(int64(cfg.MaxTicks), Op{Kind: OpMatch, Cypher: "<terminal plan stability check>"}, v), nil
	}
	if v := seekResults.Check(int64(cfg.MaxTicks), sm.engine); len(v) > 0 {
		return sm.report(int64(cfg.MaxTicks), Op{Kind: OpMatch, Cypher: "<terminal seek-result check>"}, v), nil
	}
	if v := intersect.Check(int64(cfg.MaxTicks), sm.engine); len(v) > 0 {
		return sm.report(int64(cfg.MaxTicks), Op{Kind: OpMatch, Cypher: "<terminal intersect probe>"}, v), nil
	}
	// Intersect non-vacuity (rmp #2490): the planner must really have composed,
	// a composed arm must really have returned rows, and the single-property
	// control must really have seeked WITHOUT composing — without the last one
	// the composed assertions cannot be told from "some index was used".
	if v := intersect.Finish(int64(cfg.MaxTicks)); len(v) > 0 {
		return sm.report(int64(cfg.MaxTicks), Op{Kind: OpMatch, Cypher: "<intersect vacuity check>"}, v), nil
	}
	// Non-vacuity over the whole run: at least one seek-result arm must have
	// returned rows at least once, or the checker compared only empty sets.
	if v := seekResults.Finish(int64(cfg.MaxTicks)); len(v) > 0 {
		return sm.report(int64(cfg.MaxTicks), Op{Kind: OpMatch, Cypher: "<seek-result vacuity check>"}, v), nil
	}
	// Statistics-regime non-vacuity (rmp #2456): the run must have completed
	// at least one rebuild that published statistics (non-zero tracked pairs)
	// and observed at least one rate-limit refusal, or the statistics path
	// never engaged. Refresh-correlated plan changes are legal and are
	// reported through [StatsRegime.PlanChanges], never failed.
	if v := statsRegime.Finish(int64(cfg.MaxTicks)); len(v) > 0 {
		return sm.report(int64(cfg.MaxTicks), Op{Kind: OpMatch, Cypher: "<stats regime vacuity check>"}, v), nil
	}
	// Hydration-arm non-vacuity (rmp #2490): both sides of the per-index
	// deserialize-or-rebuild decision must have been driven. It is silent on a
	// WAL-only configuration, which has no snapshot to hydrate from — a coverage
	// clause may only fail a run whose precondition was constructed.
	if v := arms.finish(int64(cfg.MaxTicks)); len(v) > 0 {
		return sm.report(int64(cfg.MaxTicks), Op{Kind: OpMatch, Cypher: "<hydration arm vacuity check>"}, v), nil
	}
	// Checkpoint non-vacuity (rmp #2464): without at least one published
	// snapshot the index definitions never crossed the snapshot boundary and
	// every post-recovery check above merely re-validated a WAL replay.
	if v := sm.checkCheckpointsFired(int64(cfg.MaxTicks)); len(v) > 0 {
		return sm.report(int64(cfg.MaxTicks), Op{Kind: OpMatch, Cypher: "<checkpoint vacuity check>"}, v), nil
	}
	return nil, nil
}

// indexDiversityChecks bundles the scenario's oracle set so the post-recovery
// battery is spelled ONCE instead of being inlined in the run loop. It carries no
// state of its own: every field is one of the stateful checkers the loop built.
type indexDiversityChecks struct {
	sm        *Simulator
	model     *SchemaModel
	probes    []ParityProbe
	planBase  *PlanBaseline
	seek      *IndexSeekResults
	stats     *StatsRegime
	intersect *IndexIntersectProbes
	arms      *indexHydrationArms
}

// postRecovery runs the whole battery a crash recovery must survive, in order,
// and returns the report of the first violation it finds (nil when all hold).
//
// store is the store the reopen produced, needed for the population counters the
// branch recorder observes. The order and the op labels are exactly what the run
// loop spelled inline before rmp #2490 split them out, so a report from this
// scenario reads identically.
func (c *indexDiversityChecks) postRecovery(tick int64, store *SimStore) *SimReport {
	if v := CheckIndexConsistency(tick, nil, c.sm.engine, indexDiversitySpecs...); len(v) > 0 {
		return c.sm.report(tick, Op{Kind: OpMatch, Cypher: "<post-recovery index check>"}, v)
	}
	// Recovered-DDL introspection: the recovered engine must re-register
	// every index with the same name, kind, and (label, property) shape,
	// on both introspection surfaces (rmp #2455).
	if v := CheckSchemaIntrospection(tick, c.model, c.sm.engine); len(v) > 0 {
		return c.sm.report(tick, Op{Kind: OpMatch, Cypher: "<post-recovery schema introspection>"}, v)
	}
	// The recovery rebuilt the plan cache from scratch: the fixed probes
	// must re-plan byte-identically, and literal/param parity must hold
	// on the recovered engine.
	if v := CheckPlanStability(tick, c.planBase, c.sm.engine); len(v) > 0 {
		return c.sm.report(tick, Op{Kind: OpMatch, Cypher: "<post-recovery plan stability check>"}, v)
	}
	if v := CheckAccessPathParity(tick, nil, c.sm.engine, c.probes...); len(v) > 0 {
		return c.sm.report(tick, Op{Kind: OpMatch, Cypher: "<post-recovery access-path parity check>"}, v)
	}
	if v := c.seek.Check(tick, c.sm.engine); len(v) > 0 {
		return c.sm.report(tick, Op{Kind: OpMatch, Cypher: "<post-recovery seek-result check>"}, v)
	}
	// The composed access path has to survive a recovery too: the
	// intersection is built from two indexes the reopen populated, so a
	// hydration or rebuild that lost entries shows up here as either a
	// result divergence or a plan that no longer composes.
	if v := c.intersect.Check(tick, c.sm.engine); len(v) > 0 {
		return c.sm.report(tick, Op{Kind: OpMatch, Cypher: "<post-recovery intersect probe>"}, v)
	}
	// Which branch this cycle's reopen took, MEASURED. Under churn the
	// WAL suffix normally carries a :Person write and the rebuild branch
	// is the correct one; the counters are recorded so the report states
	// what happened instead of implying it.
	c.arms.record(store.RecoveredIndexPopulation())
	// Post-crash statistics regime (rmp #2456): the collector is
	// in-memory and never rebuilt by recovery, so the recovered engine
	// must report zero tracked pairs; its fresh rate limiter must then
	// allow an immediate rebuild, across which every seek answer must
	// hold (the seek-result check above is the immediately-before
	// battery, the one below the immediately-after battery).
	if v := c.stats.CheckRecovered(tick, c.sm.engine); len(v) > 0 {
		return c.sm.report(tick, Op{Kind: OpMatch, Cypher: "<post-recovery stats regime>"}, v)
	}
	if v := c.stats.CheckRefresh(tick, c.sm.engine, ExpectRebuild); len(v) > 0 {
		return c.sm.report(tick, Op{Kind: OpMatch, Cypher: "<post-recovery stats refresh>"}, v)
	}
	if v := c.seek.Check(tick, c.sm.engine); len(v) > 0 {
		return c.sm.report(tick, Op{Kind: OpMatch, Cypher: "<post-recovery post-refresh seek-result check>"}, v)
	}
	return nil
}
