package sim

// ddl_checkpoint_crash.go — DDL across the checkpoint/snapshot boundary
// (rmp #2464).
//
// Every scenario that issued DDL before this one ran WAL-ONLY, so recovery
// always REPLAYED the OpCreateIndex/OpCreateConstraint frames and the snapshot
// schema components (store/snapshot/constraints.go, indexdefs.go, indexes.go)
// were never the source of a recovered index or constraint. That left the
// single highest-value durability gap in the DST: the loss mode the
// checkpointer's phase-3 self-sufficiency re-verification exists to prevent
// (#1464/#1755 — truncating the WAL prefix that first DECLARED a constraint or
// an index) was never exercised.
//
// This scenario occupies exactly that intersection. In each phase it
//
//	issues DDL → writes data → publishes a real checkpoint that truncates the
//	WAL prefix holding the DDL frames → crashes → reopens through real recovery
//
// and then adjudicates the recovered schema on four independent surfaces: the
// index-vs-base-data seek/scan oracle ([CheckIndexConsistency]), the
// schema-introspection oracle ([CheckSchemaIntrospection], rmp #2455 — SHOW
// INDEXES / SHOW CONSTRAINTS and db.indexes() / db.constraints() against the
// harness's own DDL model), the UNIQUE accept/reject adjudicator (modelled on
// the per-op comparison [Simulator.runConstraintLoop] performs), and the
// durable-image evidence itself.
//
// The last one is what separates testing the INTENDED path from testing that
// it works somehow: the run measures the WAL byte image on the [SimDisk] before
// and after every checkpoint and requires the reclaimed prefix to COVER the
// offset at which the DDL frames ended, so the DDL is demonstrably gone from
// the WAL before the crash. The pure-snapshot phase additionally requires the
// reopen to replay ZERO WAL ops: the constraint and the indexes it then
// enforces and serves can only have come from the published snapshot.

import (
	"context"
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// ScenarioDDLCheckpointCrash is the catalogue key of the DDL-across-the-
// checkpoint-boundary scenario.
const ScenarioDDLCheckpointCrash = "ddl-checkpoint-crash"

// ddlCheckpointDiskSeedMix derives the scenario's disk sub-seed from the run
// seed, so the disk stream is decorrelated from the data stream exactly as the
// other durable scenarios do.
const ddlCheckpointDiskSeedMix uint64 = 0xDD1C_9E67_0117_5A0F

// The DDL this scenario declares. The constraint uses the modern
// FOR … REQUIRE grammar and the two indexes cover both kinds — a default
// (hash) index on a string property and an explicit btree index on a numeric
// one — so the snapshot's indexdefs.bin round-trip is proven for each.
//
// ddlCheckpointCityIndexDDL and the constraint are declared in phase 1, BEFORE
// the first snapshot; ddlCheckpointAgeIndexDDL is declared in phase 2, AFTER
// it, so the second snapshot must fold a schema that the first one did not
// carry.
const (
	ddlCheckpointConstraintDDL = "CREATE CONSTRAINT ddl_person_key_unique FOR (n:Person) REQUIRE n.key IS UNIQUE"
	ddlCheckpointCityIndexDDL  = "CREATE INDEX ddl_person_city FOR (n:Person) ON (n.city)"
	ddlCheckpointAgeIndexDDL   = "CREATE INDEX ddl_person_age FOR (n:Person) ON (n.age) OPTIONS {indexType:'btree'}"
)

// Names of the declared schema objects, kept beside the DDL so the harness
// model and the DDL cannot drift apart.
const (
	ddlCheckpointConstraintName = "ddl_person_key_unique"
	ddlCheckpointCityIndexName  = "ddl_person_city"
	ddlCheckpointAgeIndexName   = "ddl_person_age"
)

// ddlCheckpointCreate is the data template. Every Person carries the UNIQUE
// key, the hash-indexed city, and the btree-indexed age, so one write
// maintains the constraint and both indexes.
const ddlCheckpointCreate = "CREATE (:Person {key:$key, city:$city, age:$age})"

// ddlCheckpointBatch is how many Persons each write burst commits. It is small
// deliberately: the scenario's cost is dominated by the per-value index-seek
// battery, and the property under test (schema surviving the snapshot
// boundary) does not grow more true with more rows.
const ddlCheckpointBatch = 40

// ddlCheckpointCities bounds the city vocabulary so the hash index carries
// several node ids per value — a seek that returned only the first match would
// then diverge from the full scan.
const ddlCheckpointCities = 5

// ddlCheckpointStoreConfig is the FULL-STACK durable layout the scenario needs:
// the WAL at db/wal and a published snapshot at db/snapshot, so a checkpoint
// can truncate the WAL prefix and recovery goes through the full
// [recovery.OpenFS] snapshot+WAL path. The graph is a SIMPLE directed graph,
// matching the simulator's own durable shape.
func ddlCheckpointStoreConfig() simStoreConfig {
	return simStoreConfig{
		graphConfig: adjlist.Config{Directed: true, Multigraph: false},
		dir:         defaultCheckpointDir,
	}
}

// ddlCheckpointCrashScenario verifies that a schema declared BEFORE a
// checkpoint survives the checkpoint's WAL-prefix truncation and a subsequent
// crash — recovered from the snapshot's constraints.bin / indexdefs.bin /
// indexes components rather than replayed from the WAL. It is deterministic
// and bit-reproducible: single goroutine, no wall clock, every draw from the
// run seed.
func ddlCheckpointCrashScenario() Scenario {
	return Scenario{
		Name: ScenarioDDLCheckpointCrash,
		Description: "DDL across the checkpoint/snapshot boundary: a UNIQUE constraint and two indexes declared before a " +
			"checkpoint that truncates the WAL prefix declaring them, then crash + recovery from the snapshot alone",
		Mode:        ModeDeterministic,
		DefaultSeed: 0xDD1C4B00,
		run:         runDDLCheckpointCrash,
	}
}

// ddlCheckpointOptions parameterises one run. The zero value is NOT the
// scenario's configuration; use [defaultDDLCheckpointOptions].
type ddlCheckpointOptions struct {
	// damageSnapshot, when non-nil, is invoked on the durable image
	// immediately after each checkpoint published its snapshot and before the
	// crash. It is the second SENSITIVITY SEAM (rmp #2464): a test uses it to
	// remove a published snapshot COMPONENT, so recovery is fed a snapshot
	// that cannot carry the schema and the post-recovery oracles must fire.
	// Nil — the scenario's own configuration — leaves the image untouched.
	//
	// An injector that removes something MUST declare the removal durable with
	// [SimDisk.ArmRemoveWritebackForPath]. Since rmp #2536 an ordinary removal is
	// reversible until its parent directory is fsync'd, and this seam runs BEFORE
	// the crash, so an unarmed removal is liable to be undone by that very crash —
	// which restores the component and leaves the sensitivity proof asserting
	// nothing.
	damageSnapshot func(disk *SimDisk, dir string)
	// phases overrides the fixed phase plan. Nil selects [ddlCheckpointPhases],
	// the scenario's own plan; a test supplies a DEGENERATE plan to prove the
	// terminal non-vacuity gate is really wired into the run.
	phases []ddlCheckpointPhase
	// wireSchemaSpecs selects whether each checkpoint is published with the
	// engine's constraint/index spec providers wired ([SimStore.Checkpoint]) or
	// DELIBERATELY unwired ([SimStore.checkpointWithoutSchemaSpecs]). False is
	// the first sensitivity seam: an unwired checkpoint publishes a snapshot
	// with no constraints.bin / indexdefs.bin, the checkpointer's phase-3
	// self-sufficiency re-verification then correctly REFUSES to truncate, and
	// the WAL-prefix oracle below must fire.
	wireSchemaSpecs bool
}

// defaultDDLCheckpointOptions is the scenario's own configuration: schema specs
// wired, exactly as the production engine wiring does; the fixed phase plan;
// and an undamaged durable image.
func defaultDDLCheckpointOptions() ddlCheckpointOptions {
	return ddlCheckpointOptions{wireSchemaSpecs: true}
}

// plan returns the phase plan this run drives.
func (o ddlCheckpointOptions) plan() []ddlCheckpointPhase {
	if o.phases != nil {
		return o.phases
	}
	return ddlCheckpointPhases()
}

// ddlCheckpointPhase is one DDL → data → checkpoint → crash → recover cycle.
type ddlCheckpointPhase struct {
	// applyModel advances the harness's schema model in lock-step with ddl.
	applyModel func(*SchemaModel)
	// name identifies the phase in violation messages and in the evidence.
	name string
	// ddl is the schema this phase declares before writing any data.
	ddl []string
	// specs are the (label, property) indexes the consistency oracle must
	// cross-check from this phase onwards, cumulative.
	specs []IndexSpec
	// tailWrites is how many Persons are committed AFTER the checkpoint and
	// before the crash. Zero makes the reopen a PURE snapshot recovery (an
	// empty WAL, zero ops replayed); a positive value leaves a genuine WAL
	// suffix so the reopen exercises snapshot + WAL-tail recovery instead.
	tailWrites int
}

// ddlCheckpointPhases is the fixed, deterministic plan. Phase 1 declares the
// constraint and the hash index before the first snapshot and crashes with an
// EMPTY WAL, so the recovered schema can only have come from the snapshot.
// Phase 2 declares a second (btree, numeric) index AFTER that snapshot and
// crashes with a non-empty WAL suffix, so the mixed snapshot+WAL recovery path
// is exercised too — and the second snapshot must fold a schema the first did
// not carry.
func ddlCheckpointPhases() []ddlCheckpointPhase {
	cityIdx := IndexSpec{Label: "Person", Property: "city"}
	keyIdx := IndexSpec{Label: "Person", Property: "key"}
	ageIdx := IndexSpec{Label: "Person", Property: "age", Numeric: true}
	return []ddlCheckpointPhase{
		{
			name: "pure-snapshot",
			ddl:  []string{ddlCheckpointConstraintDDL, ddlCheckpointCityIndexDDL},
			applyModel: func(m *SchemaModel) {
				m.AddUniqueConstraint(ddlCheckpointConstraintName, "Person", "key")
				m.AddIndex(ddlCheckpointCityIndexName, SchemaIndexHash, "Person", "city")
			},
			specs:      []IndexSpec{cityIdx, keyIdx},
			tailWrites: 0,
		},
		{
			name: "snapshot+wal-tail",
			ddl:  []string{ddlCheckpointAgeIndexDDL},
			applyModel: func(m *SchemaModel) {
				m.AddIndex(ddlCheckpointAgeIndexName, SchemaIndexBTree, "Person", "age")
			},
			specs:      []IndexSpec{cityIdx, keyIdx, ageIdx},
			tailWrites: ddlCheckpointBatch / 2,
		},
	}
}

// ddlCheckpointCycleEvidence records what one phase actually did to the durable
// image, so the oracles adjudicate MEASURED bytes rather than an assumption
// that the machinery ran.
type ddlCheckpointCycleEvidence struct {
	// name is the phase name.
	name string
	// walBeforeDDL / walAfterDDL bracket the DDL statements: their difference
	// is the durable byte range the OpCreateIndex/OpCreateConstraint frames
	// occupy, and walAfterDDL is therefore the offset the checkpoint's
	// reclaimed prefix must COVER for the DDL to be gone from the WAL.
	walBeforeDDL int64
	walAfterDDL  int64
	// walBeforeCheckpoint / walAfterCheckpoint bracket the checkpoint.
	walBeforeCheckpoint int64
	walAfterCheckpoint  int64
	// walOpsReplayed is how many WAL ops the post-crash reopen replayed. Zero
	// on the pure-snapshot phase (everything came from the snapshot), positive
	// on the phase that leaves a tail.
	walOpsReplayed int
	// dupRejected / freshAccepted count the two arms of the post-recovery
	// UNIQUE adjudication.
	dupRejected   int
	freshAccepted int
	// snapshotPublished records that the snapshot manifest existed on the
	// SimDisk after the checkpoint.
	snapshotPublished bool
}

// reclaimed is the number of WAL bytes the phase's checkpoint truncated away.
func (c *ddlCheckpointCycleEvidence) reclaimed() int64 {
	return c.walBeforeCheckpoint - c.walAfterCheckpoint
}

// ddlCheckpointEvidence is the whole run's evidence, handed back to tests so
// the non-vacuity and sensitivity assertions read MEASURED numbers.
type ddlCheckpointEvidence struct {
	cycles      []ddlCheckpointCycleEvidence
	checkpoints int
}

// runDDLCheckpointCrash is the catalogue entry point.
func runDDLCheckpointCrash(ctx context.Context, seed uint64) (*SimReport, error) {
	_, report, err := runDDLCheckpointCrashWith(ctx, seed, defaultDDLCheckpointOptions())
	return report, err
}

// runDDLCheckpointCrashWith performs one run and returns the collected evidence
// alongside the report (nil == passed). Tests use it to assert on what the run
// actually exercised, and to drive the sensitivity seam.
func runDDLCheckpointCrashWith(
	ctx context.Context, seed uint64, opts ddlCheckpointOptions,
) (*ddlCheckpointEvidence, *SimReport, error) {
	disk := NewSimDisk(NewSeed(seed^ddlCheckpointDiskSeedMix), 0)
	cfg := ddlCheckpointStoreConfig()
	rnd := NewSeed(seed)
	model := NewSchemaModel()
	ev := &ddlCheckpointEvidence{}

	st, err := OpenSimStore(disk, cfg)
	if err != nil {
		return ev, nil, fmt.Errorf("sim: ddl-checkpoint-crash open: %w", err)
	}
	// The live store is closed on every exit path; a crashed store is replaced
	// by its reopened successor before this ever fires.
	defer func() {
		if st != nil {
			_ = st.Close()
		}
	}()

	plan := opts.plan()
	written := 0
	for i, phase := range plan {
		if err := ctx.Err(); err != nil {
			return ev, nil, err
		}
		env := ddlCheckpointPhaseEnv{disk: disk, store: st, cfg: cfg, model: model, rnd: rnd, seed: seed, opts: opts, index: i}
		next, cyc, report, err := runDDLCheckpointPhase(ctx, &env, &phase, &written)
		st = next
		ev.cycles = append(ev.cycles, cyc)
		if cyc.reclaimed() > 0 {
			ev.checkpoints++
		}
		if err != nil {
			return ev, nil, fmt.Errorf("sim: ddl-checkpoint-crash phase %d (%s): %w", i, phase.name, err)
		}
		if report != nil {
			return ev, report, nil
		}
	}

	// Assert-something-was-seen: the run must have published real checkpoints
	// that really reclaimed WAL bytes, exercised BOTH recovery paths, and
	// driven both arms of the constraint adjudicator.
	if v := checkDDLCheckpointNonVacuity(int64(len(ev.cycles)), len(plan), ev); len(v) > 0 {
		return ev, ddlCheckpointReport(seed, int64(len(ev.cycles)), v), nil
	}
	return ev, nil, nil
}

// ddlCheckpointPhaseEnv is the invariant environment one phase runs in: the
// durable image, the live store, the harness model, and the run's seeds. It is
// a parameter object so the phase signature stays readable.
type ddlCheckpointPhaseEnv struct {
	disk  *SimDisk
	store *SimStore
	model *SchemaModel
	rnd   *Seed
	cfg   simStoreConfig
	opts  ddlCheckpointOptions
	seed  uint64
	// index is the phase's position in the plan. The scenario has no tick loop,
	// so it is what [Violation.Tick] and the report carry: the coordinate an
	// operator needs to locate a failure in the plan.
	index int
}

// runDDLCheckpointPhase drives one phase end to end and returns the store to
// continue with (the reopened one), the phase's evidence, and a report on the
// first violation. The returned store is always non-nil unless an error is
// returned, so the caller can keep driving (and closing) it.
func runDDLCheckpointPhase(
	ctx context.Context, env *ddlCheckpointPhaseEnv, phase *ddlCheckpointPhase, written *int,
) (*SimStore, ddlCheckpointCycleEvidence, *SimReport, error) {
	disk, st, cfg, model, rnd := env.disk, env.store, env.cfg, env.model, env.rnd
	cyc := ddlCheckpointCycleEvidence{name: phase.name}
	engine := NewEngineAdapter(st.Engine())
	tick := int64(env.index)
	report := func(v []Violation) *SimReport { return ddlCheckpointReport(env.seed, tick, v) }

	// --- declare the schema, bracketed by the durable WAL image ---
	before, err := simWALSize(disk, cfg.dir)
	if err != nil {
		return st, cyc, nil, fmt.Errorf("WAL size before DDL: %w", err)
	}
	cyc.walBeforeDDL = before
	for _, ddl := range phase.ddl {
		if err := engineRunDDLOn(ctx, engine, ddl); err != nil {
			return st, cyc, nil, fmt.Errorf("DDL %q: %w", ddl, err)
		}
	}
	phase.applyModel(model)
	if cyc.walAfterDDL, err = simWALSize(disk, cfg.dir); err != nil {
		return st, cyc, nil, fmt.Errorf("WAL size after DDL: %w", err)
	}

	// --- write data the schema must maintain ---
	if err := ddlCheckpointWriteBatch(ctx, engine, rnd, written, ddlCheckpointBatch); err != nil {
		return st, cyc, nil, err
	}
	// Baseline BEFORE any snapshot: the schema must already be correct here, so
	// a post-recovery failure is unambiguously a checkpoint/recovery defect and
	// not a DDL that never worked.
	if v := ddlCheckpointAdjudicateSchema(tick, model, engine, phase.specs); len(v) > 0 {
		return st, cyc, report(v), nil
	}

	// --- publish a real checkpoint and measure the WAL prefix it reclaims ---
	if cyc.walBeforeCheckpoint, err = simWALSize(disk, cfg.dir); err != nil {
		return st, cyc, nil, fmt.Errorf("WAL size before checkpoint: %w", err)
	}
	if err := ddlCheckpointPublish(st, env.opts); err != nil {
		return st, cyc, nil, fmt.Errorf("checkpoint: %w", err)
	}
	if cyc.walAfterCheckpoint, err = simWALSize(disk, cfg.dir); err != nil {
		return st, cyc, nil, fmt.Errorf("WAL size after checkpoint: %w", err)
	}
	cyc.snapshotPublished = disk.Exists(cfg.dir + "/" + simSnapshotName + "/manifest.json")
	if v := checkDDLFramesReclaimed(tick, &cyc); len(v) > 0 {
		return st, cyc, report(v), nil
	}
	// Sensitivity seam only: remove a published snapshot component so recovery
	// is fed an image that cannot carry the schema (see
	// [ddlCheckpointOptions.damageSnapshot]).
	if env.opts.damageSnapshot != nil {
		env.opts.damageSnapshot(disk, cfg.dir)
	}

	// --- optional WAL tail, then the crash ---
	if phase.tailWrites > 0 {
		if err := ddlCheckpointWriteBatch(ctx, engine, rnd, written, phase.tailWrites); err != nil {
			return st, cyc, nil, err
		}
	}
	// HOST crash ([SimStore.Crash] is [SimDisk.CrashHost]): drop the engine,
	// discard every byte no successful fsync covered, and revoke every dirent
	// whose parent directory was never fsynced.
	st.Crash()
	next, err := OpenSimStore(disk, cfg)
	if err != nil {
		return nil, cyc, nil, fmt.Errorf("reopen after crash: %w", err)
	}
	cyc.walOpsReplayed = next.WALOps()
	engine = NewEngineAdapter(next.Engine())

	// --- adjudicate the RECOVERED schema ---
	if v := checkDDLRecoverySource(tick, phase.tailWrites == 0, &cyc); len(v) > 0 {
		return next, cyc, report(v), nil
	}
	if v := ddlCheckpointAdjudicateSchema(tick, model, engine, phase.specs); len(v) > 0 {
		return next, cyc, report(v), nil
	}
	accepted, rejected, v := checkUniqueStillEnforced(ctx, tick, engine,
		ddlCheckpointKey(0), ddlCheckpointKey(*written))
	cyc.dupRejected += rejected
	cyc.freshAccepted += accepted
	if len(v) > 0 {
		return next, cyc, report(v), nil
	}
	*written++ // the accepted fresh key consumed one id
	return next, cyc, nil, nil
}

// ddlCheckpointPublish runs one checkpoint, wired or (for the sensitivity seam)
// deliberately unwired.
func ddlCheckpointPublish(st *SimStore, opts ddlCheckpointOptions) error {
	if opts.wireSchemaSpecs {
		return st.Checkpoint()
	}
	return st.checkpointWithoutSchemaSpecs()
}

// ddlCheckpointAdjudicateSchema runs the two independent read-back oracles over
// the current engine: the harness's DDL model against BOTH introspection
// surfaces (rmp #2455), and every declared index against its own base data.
func ddlCheckpointAdjudicateSchema(
	tick int64, model *SchemaModel, engine *EngineAdapter, specs []IndexSpec,
) []Violation {
	vs := CheckSchemaIntrospection(tick, model, engine)
	return append(vs, CheckIndexConsistency(tick, nil, engine, specs...)...)
}

// ddlCheckpointKey renders the UNIQUE key of the i-th Person.
func ddlCheckpointKey(i int) string { return fmt.Sprintf("k%d", i) }

// ddlCheckpointWriteBatch commits n Persons, each with a fresh UNIQUE key, a
// city drawn from a small vocabulary (so a hash-index value covers several
// nodes) and a seed-drawn age. Every write must commit: a refusal here is a
// harness fault, not an invariant violation, because no key is ever reused.
func ddlCheckpointWriteBatch(ctx context.Context, engine *EngineAdapter, rnd *Seed, written *int, n int) error {
	for i := 0; i < n; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		params := map[string]any{
			"key":  ddlCheckpointKey(*written),
			"city": fmt.Sprintf("c%d", *written%ddlCheckpointCities),
			"age":  int64(rnd.IntN(20)),
		}
		if !runWriteCommitted(ctx, engine, ddlCheckpointCreate, params) {
			return fmt.Errorf("write of %q was refused (no key is ever reused, so this is a harness fault)", params["key"])
		}
		*written++
	}
	return nil
}

// engineRunDDLOn runs a DDL statement through an engine adapter and drains it.
// It is [Simulator.engineRunDDL] for a scenario that owns its store directly
// rather than through a [Simulator].
func engineRunDDLOn(ctx context.Context, engine *EngineAdapter, query string) error {
	res, err := engine.Run(ctx, query, nil)
	if err != nil {
		return err
	}
	for res.Next() { // draining is the point
	}
	drainErr := res.Err()
	_ = res.Close()
	return drainErr
}

// runWriteCommitted runs a write statement through the engine's write path and
// reports whether the engine COMMITTED it — the same accept/reject signal
// [Simulator.executeCounted] derives (accepted without error AND drained
// cleanly), so an adjudicator built on it sees exactly what the safety loop
// sees.
func runWriteCommitted(ctx context.Context, engine *EngineAdapter, query string, params map[string]any) bool {
	res, err := engine.RunWrite(ctx, query, params)
	if err != nil {
		return false
	}
	for res.Next() { // draining is the point
	}
	drainErr := res.Err()
	_ = res.Close()
	return drainErr == nil
}

// ─────────────────────────────────────────────────────────────────────────────
// oracles
// ─────────────────────────────────────────────────────────────────────────────

// checkDDLFramesReclaimed asserts the checkpoint truncated a WAL prefix that
// COVERS the DDL frames — the difference between proving recovery used the
// snapshot and merely observing that it worked somehow.
//
// The DDL frames occupy [walBeforeDDL, walAfterDDL) of the WAL, so a reclaimed
// prefix of at least walAfterDDL bytes leaves none of them on disk. When the
// checkpointer refuses to truncate — which is exactly what its phase-3
// self-sufficiency re-verification does when the snapshot carries no
// constraints.bin/indexdefs.bin for a graph that has constraints or indexes
// (#1464/#1755) — the reclaimed prefix is 0 and this fires.
func checkDDLFramesReclaimed(tick int64, cyc *ddlCheckpointCycleEvidence) []Violation {
	const op = "DDL WAL-prefix reclamation"
	fail := func(format string, args ...any) []Violation {
		return []Violation{{
			Kind: ViolationACIDDurability, Tick: tick, Op: op,
			Message: fmt.Sprintf("%s: ", cyc.name) + fmt.Sprintf(format, args...),
		}}
	}
	if !cyc.snapshotPublished {
		return fail("no snapshot manifest was published: the checkpoint never produced a durable image")
	}
	if cyc.walAfterDDL <= cyc.walBeforeDDL {
		return fail("the DDL appended no durable WAL bytes (WAL %d -> %d): there is no DDL prefix to reclaim,"+
			" so this phase would prove nothing", cyc.walBeforeDDL, cyc.walAfterDDL)
	}
	if cyc.walBeforeCheckpoint <= 0 {
		return fail("the WAL was empty before the checkpoint: nothing could be truncated")
	}
	if cyc.reclaimed() <= 0 {
		return fail("the checkpoint reclaimed 0 WAL bytes (WAL %d -> %d): the prefix that DECLARED the schema is"+
			" still on disk, so a recovery would REPLAY the DDL instead of loading it from the snapshot",
			cyc.walBeforeCheckpoint, cyc.walAfterCheckpoint)
	}
	if cyc.reclaimed() < cyc.walAfterDDL {
		return fail("the checkpoint reclaimed %d WAL bytes but the DDL frames end at offset %d:"+
			" the frames that declared the constraint/index survive in the WAL and would be replayed",
			cyc.reclaimed(), cyc.walAfterDDL)
	}
	return nil
}

// checkDDLRecoverySource asserts the reopen took the recovery path the phase
// was built to exercise.
//
// pureSnapshot phases crash with an EMPTY WAL, so recovery must replay ZERO
// ops: every label, property, index definition and constraint the recovered
// engine then serves can only have come from the published snapshot. The
// other phase deliberately leaves a WAL tail, so a zero replay there would mean
// the tail was silently lost.
func checkDDLRecoverySource(tick int64, pureSnapshot bool, cyc *ddlCheckpointCycleEvidence) []Violation {
	const op = "recovery source"
	fail := func(format string, args ...any) []Violation {
		return []Violation{{
			Kind: ViolationACIDDurability, Tick: tick, Op: op,
			Message: fmt.Sprintf("%s: ", cyc.name) + fmt.Sprintf(format, args...),
		}}
	}
	if pureSnapshot {
		if cyc.walAfterCheckpoint != 0 {
			return fail("the WAL still holds %d bytes after the checkpoint, so this phase is not a PURE snapshot"+
				" recovery and the zero-replay claim below would be meaningless", cyc.walAfterCheckpoint)
		}
		if cyc.walOpsReplayed != 0 {
			return fail("recovery replayed %d WAL ops after a checkpoint that emptied the WAL:"+
				" the graph did not come from the snapshot alone", cyc.walOpsReplayed)
		}
		return nil
	}
	if cyc.walOpsReplayed <= 0 {
		return fail("recovery replayed %d WAL ops although the phase committed a post-checkpoint tail:"+
			" the snapshot+WAL-tail path was NOT exercised (or the tail was lost)", cyc.walOpsReplayed)
	}
	return nil
}

// checkUniqueStillEnforced drives BOTH arms of the recovered UNIQUE constraint
// and adjudicates the engine's accept/reject outcome against the harness's
// prediction — the same comparison [Simulator.runConstraintLoop] makes per op,
// narrowed to the one question this scenario asks: does the constraint that
// crossed the snapshot boundary still bite?
//
//   - heldKey is a key the recovered graph already carries, so the CREATE MUST
//     be rejected and must leave exactly one node carrying it (an atomic
//     rejection, not a partially-applied one);
//   - freshKey is a key nothing carries, so the CREATE MUST commit — the
//     control that proves the rejection above discriminates rather than the
//     engine having become unable to write Persons at all.
//
// It returns how many arms committed and how many were rejected so the caller
// can assert the adjudication was not vacuous.
func checkUniqueStillEnforced(
	ctx context.Context, tick int64, engine *EngineAdapter, heldKey, freshKey string,
) (accepted, rejected int, vs []Violation) {
	fail := func(format string, args ...any) {
		vs = append(vs, Violation{
			Kind: ViolationACIDConsistency, Tick: tick, Op: "recovered UNIQUE enforcement",
			Message: fmt.Sprintf(format, args...),
		})
	}
	dup := map[string]any{"key": heldKey, "city": "dup", "age": int64(0)}
	if runWriteCommitted(ctx, engine, ddlCheckpointCreate, dup) {
		fail("UNIQUE enforcement gap after recovery: a duplicate CREATE for the held key %q COMMITTED;"+
			" the constraint did not survive the checkpoint/snapshot boundary", heldKey)
	} else {
		rejected++
	}
	if n, err := ddlCheckpointCountByKey(ctx, engine, heldKey); err != nil {
		fail("count of the held key %q failed: %v", heldKey, err)
	} else if n != 1 {
		fail("after the rejected duplicate, %d nodes carry key %q, want exactly 1:"+
			" the refused write was not applied atomically", n, heldKey)
	}

	fresh := map[string]any{"key": freshKey, "city": "dup", "age": int64(1)}
	if !runWriteCommitted(ctx, engine, ddlCheckpointCreate, fresh) {
		fail("a CREATE for the unheld key %q was REJECTED: the recovered constraint refuses valid writes,"+
			" so the rejection above proves nothing", freshKey)
	} else {
		accepted++
	}
	if n, err := ddlCheckpointCountByKey(ctx, engine, freshKey); err != nil {
		fail("count of the fresh key %q failed: %v", freshKey, err)
	} else if n != 1 {
		fail("after the accepted CREATE, %d nodes carry key %q, want exactly 1", n, freshKey)
	}
	return accepted, rejected, vs
}

// ddlCheckpointCountByKey counts the Persons carrying key through the engine's
// own read path — which resolves through the constraint's backing index.
func ddlCheckpointCountByKey(ctx context.Context, engine *EngineAdapter, key string) (int64, error) {
	res, err := engine.Run(ctx, "MATCH (n:Person {key:$key}) RETURN count(n)", map[string]any{"key": key})
	if err != nil {
		return 0, err
	}
	defer func() { _ = res.Close() }()
	var n int64
	if res.Next() {
		if v, ok := res.IntAt(0); ok {
			n = v
		}
	}
	return n, res.Err()
}

// checkDDLCheckpointNonVacuity is the terminal assert-something-was-seen gate:
// it requires the run to have published real checkpoints that really reclaimed
// WAL bytes, to have exercised BOTH the pure-snapshot and the snapshot+WAL-tail
// recovery paths, and to have driven both arms of the UNIQUE adjudicator. A run
// that silently exercised none of that would otherwise pass by doing nothing.
func checkDDLCheckpointNonVacuity(tick int64, wantPhases int, ev *ddlCheckpointEvidence) []Violation {
	const op = "ddl-checkpoint non-vacuity"
	var vs []Violation
	fail := func(format string, args ...any) {
		vs = append(vs, Violation{
			Kind: ViolationOracleDeviation, Tick: tick, Op: op,
			Message: fmt.Sprintf(format, args...),
		})
	}
	if len(ev.cycles) != wantPhases {
		fail("the run completed %d of %d phases", len(ev.cycles), wantPhases)
		return vs
	}
	if ev.checkpoints == 0 {
		fail("no checkpoint reclaimed any WAL bytes: the snapshot path was never exercised")
	}
	var pure, tailed, dup, fresh int
	for _, c := range ev.cycles {
		if c.walAfterCheckpoint == 0 && c.walOpsReplayed == 0 {
			pure++
		}
		if c.walOpsReplayed > 0 {
			tailed++
		}
		dup += c.dupRejected
		fresh += c.freshAccepted
	}
	if pure == 0 {
		fail("no phase recovered from the snapshot ALONE (an empty WAL and zero replayed ops)")
	}
	if tailed == 0 {
		fail("no phase recovered through the snapshot + WAL-tail path")
	}
	if dup == 0 {
		fail("no duplicate was ever rejected after recovery: the UNIQUE constraint never bit")
	}
	if fresh == 0 {
		fail("no fresh key was ever accepted after recovery: the rejection arm has no control")
	}
	return vs
}

// ddlCheckpointReport renders a violation as a scenario report.
func ddlCheckpointReport(seed uint64, tick int64, v []Violation) *SimReport {
	return &SimReport{
		Scenario:   ScenarioDDLCheckpointCrash,
		Mode:       ModeDeterministic,
		Seed:       seed,
		FailedTick: tick,
		FailedOp:   Op{Kind: OpMatch, Cypher: "<ddl across the checkpoint boundary>"},
		Violations: v,
	}
}
