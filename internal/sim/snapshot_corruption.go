package sim

// snapshot_corruption.go — snapshot component corruption fail-stop battery
// (rmp #2467).
//
// The store defines nine typed corruption sentinels — one per durable snapshot
// component — plus manifest size and version guards, and it records a CRC32C
// for every component in the manifest. Until this scenario the ONLY corruption
// the simulator ever injected was a byte flip inside a WAL frame
// (`wal-corruption-failstop`, ST5): `SimDisk.CorruptRange` appeared nowhere
// else outside the disk's own unit tests, so not one of the nine sentinels, and
// not one of the per-component CRCs, had ever been reached under simulation.
//
// This scenario corrupts each published component in turn and adjudicates the
// recovery that follows. Every arm is deterministic and bit-reproducible: the
// fixture, the interior offsets and the arm order all derive from the run seed,
// and `CorruptRange` draws nothing from it.
//
// Three outcomes are distinguished, because the store really has three:
//
//   - FAIL-STOP (nine components). manifest.json, csr.bin, labels.bin,
//     properties.bin, mapper.bin, tombstones.bin, edgehandles.bin,
//     constraints.bin and indexdefs.bin each refuse the reopen. The MAGIC arm
//     requires the component's own typed sentinel; the seed-chosen INTERIOR arm
//     requires detection at all — every byte of a binary component is inside the
//     manifest CRC, so a flip anywhere must be caught.
//   - TOLERATED BY DESIGN (indexes/<name>.bin). A corrupt index payload is NOT
//     fatal: `snapshot.LoadIndexes` reports nil bytes for it and the engine
//     REBUILDS that index from the recovered graph — per index, never a
//     fail-stop, because an index is derived data over an already-recovered,
//     independently integrity-checked graph, and every reference engine reaches
//     the same conclusion (PostgreSQL discards and rebuilds pg_internal.init,
//     Memgraph rebuilds its label indexes unconditionally, Neo4j coerces an
//     unreadable index header to POPULATING). This is THE documented exception
//     to the battery's fail-stop rule, and it is what keeps the nine-component
//     oracle falsifiable: indexes/<name>.bin is the only durable file a reopen
//     accepts damaged, so it is the only substrate on which
//     TestSnapshotCorruption_OracleFiresWhenRecoveryWronglySucceeds can prove
//     the fail-stop oracle fires at all.
//
//     The arm is PAIRED (rmp #2490). Requiring only that the reopen succeeds is
//     vacuous on the hydration axis: before #2490 the engine rebuilt every index
//     whatever the payload said, so "rebuilt" was indistinguishable from
//     "rebuilt because corrupt". The arm therefore drives a CONTROL reopen over
//     the intact payloads and requires every index HYDRATED, then the corrupt
//     reopen and requires every index REBUILT and every payload reported
//     unreadable — and both must return the exact committed node set with every
//     declared index still agreeing with a full label scan, so neither
//     "tolerated" nor "hydrated" can degrade into "silently wrong".
//   - UNDETECTED (the manifest's JSON key-name region). manifest.json carries no
//     checksum of its own, so a flip inside a KEY NAME leaves valid JSON whose
//     key `encoding/json` then ignores — the field decodes to its zero value.
//     The arm measures the consequence for `commit_ts`, the MVCC clock floor
//     recovery restores (`store/recovery`, rmp #2309), and requires the damage
//     to be CONTAINED: whatever the flip does to the clock, no committed node
//     may be lost. See docs/dst-feature-coverage.md.
//
// Nothing is assumed. Every corruption is read back and compared against the
// pre-corruption bytes before any sentinel is asserted — a flip that changed
// nothing would otherwise produce a false pass — the durable image is compared
// before and after each refused reopen so a failed recovery is proven to have
// mutated nothing, and each arm restores the flip (XOR 0xFF is an involution)
// and reopens CLEAN, so the refusal is attributable to the corruption and not
// to the harness.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// ScenarioSnapshotCorruptionFailStop is the catalogue key of the snapshot
// component corruption battery.
const ScenarioSnapshotCorruptionFailStop = "snapshot-corruption-failstop"

// snapshotCorruptionDiskSeedMix derives the scenario's disk sub-seed from the
// run seed, so the disk stream is decorrelated from the data stream exactly as
// the other durable scenarios do.
const snapshotCorruptionDiskSeedMix uint64 = 0x2467_C0FF_EE00_5A0F

// snapshotManifestFile is the manifest's file name inside a published snapshot
// directory. store/snapshot spells it as a literal at every use site rather
// than exporting a constant, so the harness pins it here.
const snapshotManifestFile = "manifest.json"

// The fixture's DDL. The UNIQUE constraint produces constraints.bin, the two
// indexes produce indexdefs.bin, and both index kinds (a hash index on a string
// property and a btree index on a numeric one) produce indexes/<name>.bin
// payloads for the tolerated-corruption arm.
const (
	snapshotCorruptionConstraintDDL = "CREATE CONSTRAINT sc_person_key_unique FOR (n:Person) REQUIRE n.key IS UNIQUE"
	snapshotCorruptionCityIndexDDL  = "CREATE INDEX sc_person_city FOR (n:Person) ON (n.city)"
	snapshotCorruptionAgeIndexDDL   = "CREATE INDEX sc_person_age FOR (n:Person) ON (n.age) OPTIONS {indexType:'btree'}"
)

// snapshotCorruptionCreate is the data template; every Person carries the
// UNIQUE key, the hash-indexed city and the btree-indexed age.
const snapshotCorruptionCreate = "CREATE (:Person {key:$key, city:$city, age:$age})"

// Fixture shape. snapshotCorruptionNodes Persons are created and the LAST one
// is deleted, so tombstones.bin is emitted and the committed model is the first
// snapshotCorruptionNodes-1 keys. snapshotCorruptionCities bounds the city
// vocabulary so the hash index carries several ids per value.
const (
	snapshotCorruptionNodes  = 16
	snapshotCorruptionCities = 3
)

// snapshotCorruptionComponent names one durable snapshot component, the typed
// sentinel its structural reader raises when its header is damaged, and that
// sentinel's Go identifier for violation messages.
type snapshotCorruptionComponent struct {
	// sentinel is the typed error the component's reader wraps its structural
	// failures in. errors.Is against it is the arm's primary assertion.
	sentinel error
	// file is the component's name inside the published snapshot directory.
	file string
	// sentinelName is the sentinel's Go identifier, so a violation message names
	// what was expected rather than printing an error string.
	sentinelName string
	// crcCovered records whether a CRC32C covers this component's whole byte
	// range, so a flip ANYWHERE in it must be detected. Only a CRC-covered
	// component gets the seed-chosen interior arm.
	//
	// It is now true for EVERY component including manifest.json, which since
	// rmp #2520 carries a CRC32C trailer over its own bytes in addition to the
	// per-file CRCs it holds for everything else. It was false for the manifest
	// before that: the manifest checksummed the whole directory and none of
	// itself, so a byte flipped in a JSON key name left valid JSON whose renamed
	// key encoding/json dropped, and 360 of 1399 bytes (25.7%) of a published
	// manifest were accepted silently. See [runSnapshotManifestGapArm].
	crcCovered bool
}

// snapshotCorruptionComponents returns the nine fail-stop components in a fixed
// order, each paired with the typed sentinel its reader raises. The list is the
// scenario's intended sweep: the terminal non-vacuity gate fails the run unless
// every entry was corrupted and every sentinel observed.
//
// The order matters only for reproducibility of the report; each arm restores
// the image it damaged, so the arms are independent.
func snapshotCorruptionComponents() []snapshotCorruptionComponent {
	return []snapshotCorruptionComponent{
		{file: snapshotManifestFile, sentinel: snapshot.ErrManifestCorrupted, sentinelName: "ErrManifestCorrupted", crcCovered: true},
		{file: snapshot.CSRFile, sentinel: snapshot.ErrCSRCorrupted, sentinelName: "ErrCSRCorrupted", crcCovered: true},
		{file: snapshot.LabelsFile, sentinel: snapshot.ErrLabelsCorrupted, sentinelName: "ErrLabelsCorrupted", crcCovered: true},
		{file: snapshot.PropertiesFile, sentinel: snapshot.ErrPropertiesCorrupted, sentinelName: "ErrPropertiesCorrupted", crcCovered: true},
		{file: snapshot.MapperFile, sentinel: snapshot.ErrMapperCorrupted, sentinelName: "ErrMapperCorrupted", crcCovered: true},
		{file: snapshot.TombstonesFile, sentinel: snapshot.ErrTombstonesCorrupted, sentinelName: "ErrTombstonesCorrupted", crcCovered: true},
		{file: snapshot.EdgeHandlesFile, sentinel: snapshot.ErrEdgeHandlesCorrupted, sentinelName: "ErrEdgeHandlesCorrupted", crcCovered: true},
		{file: snapshot.ConstraintsFile, sentinel: snapshot.ErrConstraintsCorrupted, sentinelName: "ErrConstraintsCorrupted", crcCovered: true},
		{file: snapshot.IndexDefsFile, sentinel: snapshot.ErrIndexDefsCorrupted, sentinelName: "ErrIndexDefsCorrupted", crcCovered: true},
	}
}

// snapshotCorruptionStoreConfig is the FULL-STACK durable layout the scenario
// needs: the WAL at db/wal and a published snapshot at db/snapshot, so a
// checkpoint truncates the WAL prefix and every reopen goes through the full
// [recovery.OpenFS] snapshot+WAL path. The graph is a directed MULTIGRAPH so
// parallel typed relationships produce per-handle edge metadata, which is what
// makes edgehandles.bin present in the published image.
func snapshotCorruptionStoreConfig() simStoreConfig {
	return simStoreConfig{
		graphConfig: adjlist.Config{Directed: true, Multigraph: true},
		dir:         defaultCheckpointDir,
	}
}

// snapshotCorruptionFailStopScenario verifies that corrupting ANY published
// snapshot component fail-stops recovery with that component's typed sentinel,
// that recovery refuses rather than loading a half-graph, and that the WAL and
// the rest of the durable image are left untouched — while pinning the two
// places where the store deliberately does NOT fail-stop (a corrupt index
// payload, which it rebuilds; and the manifest's un-checksummed key-name
// region). It is deterministic and bit-reproducible: single goroutine, no wall
// clock, every draw from the run seed.
func snapshotCorruptionFailStopScenario() Scenario {
	return Scenario{
		Name: ScenarioSnapshotCorruptionFailStop,
		Description: "corrupt each published snapshot component in turn: recovery fail-stops with the component's typed sentinel, " +
			"refuses to load a half-graph, and leaves the WAL and the rest of the image untouched",
		Mode:        ModeDeterministic,
		DefaultSeed: 0x2467_C0DE,
		run:         runSnapshotCorruptionFailStop,
	}
}

// snapshotCorruptionOptions parameterises one run. The zero value is NOT the
// scenario's configuration; use [defaultSnapshotCorruptionOptions].
type snapshotCorruptionOptions struct {
	// components overrides the intended sweep. Nil selects
	// [snapshotCorruptionComponents], the scenario's own nine-component plan; a
	// test supplies a DEGENERATE plan to prove the terminal non-vacuity gate is
	// really wired into the run.
	components []snapshotCorruptionComponent
	// interiorOffsets pins the interior arm's byte offset for a named component,
	// overriding the seed draw. It is a TEST SEAM: it is what lets a test aim the
	// interior arm at a byte KNOWN to be outside any checksum (a manifest key
	// name) and so prove that the "recovery wrongly succeeded" oracle fires. The
	// scenario itself never sets it.
	interiorOffsets map[string]int64
	// skipInteriorArm drops the seed-chosen interior byte flip, leaving only the
	// magic arm. It exists so a test can isolate which arm produced a sentinel;
	// the scenario itself always runs both.
	skipInteriorArm bool
	// skipManifestGuards drops the oversize/unsupported-version manifest probes.
	skipManifestGuards bool
	// skipIndexTolerance drops the indexes/<name>.bin tolerated-corruption arm.
	skipIndexTolerance bool
	// skipManifestGap drops the un-checksummed manifest key-region arm.
	skipManifestGap bool
}

// defaultSnapshotCorruptionOptions is the scenario's own configuration: the
// full nine-component sweep, both corruption arms per component, both manifest
// guards, the index-tolerance arm and the manifest-gap arm.
func defaultSnapshotCorruptionOptions() snapshotCorruptionOptions {
	return snapshotCorruptionOptions{}
}

// plan returns the component sweep this run drives.
func (o snapshotCorruptionOptions) plan() []snapshotCorruptionComponent {
	if o.components != nil {
		return o.components
	}
	return snapshotCorruptionComponents()
}

// snapshotCorruptionArm records what ONE corruption arm actually did, so the
// oracles adjudicate measured bytes rather than an assumption that the
// machinery ran.
type snapshotCorruptionArm struct {
	// component is the corrupted file's name and kind is "magic" or "interior".
	component string
	kind      string
	// offset is the byte the arm flipped; size is the component's byte length.
	offset int64
	size   int64
	// bytesChanged records that the flipped byte really differs from what was
	// there before. A no-op corruption would make every downstream assertion
	// vacuous, so this is checked BEFORE the sentinel is asserted.
	bytesChanged bool
	// refused records that the reopen returned an error AND no store, so no
	// partially-populated graph is observable.
	refused bool
	// sentinelMatched records that errors.Is found the component's own typed
	// sentinel (magic arm), or any corruption sentinel (interior arm).
	sentinelMatched bool
	// imageIntact records that the durable image is byte-identical before and
	// after the refused reopen — a failed recovery mutated nothing.
	imageIntact bool
	// walIntact records that db/wal is byte-identical to the post-checkpoint
	// baseline, so the corruption and not the harness is what recovery saw.
	walIntact bool
	// restoredClean records that undoing the flip restored the bytes exactly and
	// the snapshot then recovered the exact committed model again.
	restoredClean bool
}

// snapshotCorruptionGuardArm records one manifest-level guard probe.
type snapshotCorruptionGuardArm struct {
	// name identifies the guard ("unsupported-version" / "oversize").
	name string
	// wantErr is the typed error the guard must raise; matched records whether
	// errors.Is found it.
	wantErrName string
	matched     bool
	// refused records that the reopen returned an error and no store.
	refused bool
}

// snapshotCorruptionEvidence is the whole run's evidence, handed back to tests
// so the non-vacuity and sensitivity assertions read MEASURED numbers.
type snapshotCorruptionEvidence struct {
	// arms holds one entry per (component, arm-kind) pair actually driven.
	arms []snapshotCorruptionArm
	// guards holds the manifest-level guard probes actually driven.
	guards []snapshotCorruptionGuardArm
	// sentinelsSeen names every typed sentinel a magic arm actually matched.
	sentinelsSeen []string
	// indexPayloadsCorrupted counts the indexes/<name>.bin payloads flipped in
	// the tolerated-corruption arm; indexRebuildVerified records that the
	// reopened engine's indexes still agreed with a full scan afterwards.
	indexPayloadsCorrupted int
	indexRebuildVerified   bool
	// The PAIRED hydration measurements (rmp #2490). indexControl* are what the
	// reopen over the INTACT payloads measured and indexCorrupt* what the reopen
	// over the flipped ones measured; indexRegistered is how many indexes the
	// reopened engine registered, which is the independent anchor both sides are
	// compared against. Without the control arm the corrupt arm's "everything
	// rebuilt" is satisfied by an engine that never hydrates at all.
	indexRegistered                           int
	indexControlHydrated, indexControlRebuilt int
	indexCorruptHydrated, indexCorruptRebuilt int
	indexCorruptUnreadable                    int
	// manifestGapRan records that the un-checksummed key-region arm was driven;
	// manifestGapDetected records whether recovery caught it after all.
	manifestGapRan      bool
	manifestGapDetected bool
	// manifestGapCleanTS / manifestGapCorruptTS are the MVCC clock floors
	// (recovery.Result.MaxCommitTS) recovery derived from the intact and the
	// key-flipped manifest. A drop between them is the measured consequence of
	// the manifest carrying no checksum of its own.
	manifestGapCleanTS   uint64
	manifestGapCorruptTS uint64
	// committed is the model the fixture acknowledged; cleanRecovered is what a
	// clean reopen of the published snapshot returned.
	committed      int
	cleanRecovered int
}

// runSnapshotCorruptionFailStop is the catalogue entry point.
func runSnapshotCorruptionFailStop(ctx context.Context, seed uint64) (*SimReport, error) {
	_, report, err := runSnapshotCorruptionWith(ctx, seed, defaultSnapshotCorruptionOptions())
	return report, err
}

// snapshotCorruptionFixture is the published durable image the arms damage,
// plus everything needed to adjudicate a reopen over it.
type snapshotCorruptionFixture struct {
	disk *SimDisk
	// committed is the set of Person keys the fixture acknowledged and did not
	// delete — exactly what every clean reopen must return.
	committed map[string]struct{}
	// walBytes is the post-checkpoint WAL image; every arm requires db/wal to
	// still equal it.
	walBytes []byte
	// dir is the store directory ("db"); snapDir is the published snapshot
	// directory ("db/snapshot").
	dir     string
	snapDir string
}

// runSnapshotCorruptionWith performs one run and returns the collected evidence
// alongside the report (nil == passed). Tests use it to assert on what the run
// actually exercised and to drive the sensitivity seams.
//
// one arm per component plus the guard, tolerance and gap arms; splitting the dispatch would hide the sweep.
func runSnapshotCorruptionWith(
	ctx context.Context, seed uint64, opts snapshotCorruptionOptions,
) (*snapshotCorruptionEvidence, *SimReport, error) {
	ev := &snapshotCorruptionEvidence{}
	fx, err := buildSnapshotCorruptionFixture(ctx, seed)
	if err != nil {
		return ev, nil, err
	}
	ev.committed = len(fx.committed)

	// Baseline: the published snapshot recovers the exact committed model. Every
	// later refusal is measured against this, so a refusal can never be an image
	// that never worked in the first place.
	recovered, v, err := snapshotCorruptionCleanReopen(ctx, fx)
	if err != nil {
		return ev, nil, err
	}
	ev.cleanRecovered = recovered
	if len(v) > 0 {
		return ev, snapshotCorruptionReport(seed, v), nil
	}

	rnd := NewSeed(seed)
	for _, comp := range opts.plan() {
		if err := ctx.Err(); err != nil {
			return ev, nil, err
		}
		// The interior arm asserts that a CRC catches a flip the structural parser
		// would accept, so it applies only where a CRC covers the component. Since
		// rmp #2520 that is every component: the binary ones through the manifest's
		// per-file CRC32C, and manifest.json itself through its own CRC32C trailer.
		kinds := []string{"magic"}
		if !opts.skipInteriorArm && comp.crcCovered {
			kinds = append(kinds, "interior")
		}
		for _, kind := range kinds {
			pinned := int64(-1)
			if off, ok := opts.interiorOffsets[comp.file]; ok && kind == "interior" {
				pinned = off
			}
			arm, vs, aerr := runSnapshotCorruptionArm(ctx, fx, &comp, kind, rnd, pinned)
			if aerr != nil {
				return ev, nil, fmt.Errorf("sim: %s %s/%s arm: %w",
					ScenarioSnapshotCorruptionFailStop, comp.file, kind, aerr)
			}
			ev.arms = append(ev.arms, arm)
			if kind == "magic" && arm.sentinelMatched {
				ev.sentinelsSeen = append(ev.sentinelsSeen, comp.sentinelName)
			}
			if len(vs) > 0 {
				return ev, snapshotCorruptionReport(seed, vs), nil
			}
		}
	}

	if !opts.skipManifestGuards {
		guards, vs, gerr := runSnapshotManifestGuards(ctx, fx)
		if gerr != nil {
			return ev, nil, gerr
		}
		ev.guards = guards
		if len(vs) > 0 {
			return ev, snapshotCorruptionReport(seed, vs), nil
		}
	}

	if !opts.skipIndexTolerance {
		arm, vs, ierr := runSnapshotIndexToleranceArm(ctx, fx, rnd)
		if ierr != nil {
			return ev, nil, ierr
		}
		ev.indexPayloadsCorrupted, ev.indexRebuildVerified = arm.flipped, arm.verified
		ev.indexRegistered = arm.registered
		ev.indexControlHydrated, ev.indexControlRebuilt = arm.controlHydrated, arm.controlRebuilt
		ev.indexCorruptHydrated, ev.indexCorruptRebuilt = arm.corruptHydrated, arm.corruptRebuilt
		ev.indexCorruptUnreadable = arm.corruptUnreadable
		if len(vs) > 0 {
			return ev, snapshotCorruptionReport(seed, vs), nil
		}
	}

	if !opts.skipManifestGap {
		gap, vs, merr := runSnapshotManifestGapArm(ctx, fx)
		if merr != nil {
			return ev, nil, merr
		}
		ev.manifestGapRan = true
		ev.manifestGapDetected = gap.detected
		ev.manifestGapCleanTS, ev.manifestGapCorruptTS = gap.cleanTS, gap.corruptTS
		if len(vs) > 0 {
			return ev, snapshotCorruptionReport(seed, vs), nil
		}
	}

	if vs := checkSnapshotCorruptionNonVacuity(opts, ev); len(vs) > 0 {
		return ev, snapshotCorruptionReport(seed, vs), nil
	}
	return ev, nil, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// fixture
// ─────────────────────────────────────────────────────────────────────────────

// buildSnapshotCorruptionFixture drives a full-stack store through the DDL,
// data, parallel typed relationships and node deletion that make ALL NINE
// components present in the published image, then publishes one real checkpoint
// (which truncates the WAL prefix it folded) and closes cleanly.
//
// The checkpoint is what makes the battery meaningful: it leaves the snapshot as
// the ONLY durable source of the committed graph, so a recovery that silently
// ignored a corrupt component would return an empty graph rather than a stale
// one, and the refusal is a genuine fail-stop rather than a fallback.
func buildSnapshotCorruptionFixture(ctx context.Context, seed uint64) (*snapshotCorruptionFixture, error) {
	disk := NewSimDisk(NewSeed(seed^snapshotCorruptionDiskSeedMix), 0)
	cfg := snapshotCorruptionStoreConfig()
	st, err := OpenSimStore(disk, cfg)
	if err != nil {
		return nil, fmt.Errorf("sim: snapshot-corruption open: %w", err)
	}
	engine := NewEngineAdapter(st.Engine())

	for _, ddl := range []string{
		snapshotCorruptionConstraintDDL,
		snapshotCorruptionCityIndexDDL,
		snapshotCorruptionAgeIndexDDL,
	} {
		if err := engineRunDDLOn(ctx, engine, ddl); err != nil {
			_ = st.Close()
			return nil, fmt.Errorf("sim: snapshot-corruption DDL %q: %w", ddl, err)
		}
	}

	committed := make(map[string]struct{}, snapshotCorruptionNodes)
	for i := range snapshotCorruptionNodes {
		key := snapshotCorruptionKey(i)
		params := map[string]any{
			"key":  key,
			"city": fmt.Sprintf("sc-city%d", i%snapshotCorruptionCities),
			"age":  int64(i),
		}
		if !runWriteCommitted(ctx, engine, snapshotCorruptionCreate, params) {
			_ = st.Close()
			return nil, fmt.Errorf("sim: snapshot-corruption CREATE %q was rejected", key)
		}
		committed[key] = struct{}{}
	}

	// Parallel typed relationships with per-CREATE properties: this is what makes
	// the graph carry per-handle edge metadata, and so what makes edgehandles.bin
	// present in the published snapshot.
	for _, rel := range snapshotCorruptionRels() {
		if !runWriteCommitted(ctx, engine, rel, nil) {
			_ = st.Close()
			return nil, fmt.Errorf("sim: snapshot-corruption relationship %q was rejected", rel)
		}
	}

	// One deletion, so the capture emits tombstones.bin.
	dead := snapshotCorruptionKey(snapshotCorruptionNodes - 1)
	if !runWriteCommitted(ctx, engine,
		"MATCH (n:Person {key:$key}) DETACH DELETE n", map[string]any{"key": dead}) {
		_ = st.Close()
		return nil, fmt.Errorf("sim: snapshot-corruption DETACH DELETE %q was rejected", dead)
	}
	delete(committed, dead)

	if err := st.Checkpoint(); err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("sim: snapshot-corruption checkpoint: %w", err)
	}
	if err := st.Close(); err != nil {
		return nil, fmt.Errorf("sim: snapshot-corruption close: %w", err)
	}

	fx := &snapshotCorruptionFixture{
		disk:      disk,
		dir:       cfg.dir,
		snapDir:   cfg.dir + "/" + simSnapshotName,
		committed: committed,
	}
	// Every component the sweep intends to damage must really be on disk;
	// a missing one would make its arm silently unreachable.
	for _, comp := range snapshotCorruptionComponents() {
		if !disk.Exists(fx.snapDir + "/" + comp.file) {
			return nil, fmt.Errorf("sim: snapshot-corruption fixture published no %s: the component's arm would be vacuous", comp.file)
		}
	}
	if fx.walBytes, err = disk.ReadFile(walPathFor(cfg.dir)); err != nil {
		return nil, fmt.Errorf("sim: snapshot-corruption read WAL image: %w", err)
	}
	return fx, nil
}

// snapshotCorruptionRels are the parallel, typed, propertied relationships the
// fixture creates. Two of them join the SAME ordered pair with different types,
// so the per-pair union that labels.bin stores is not enough to reconstruct
// them and edgehandles.bin is emitted.
func snapshotCorruptionRels() []string {
	return []string{
		"MATCH (a:Person {key:'sc0'}),(b:Person {key:'sc1'}) CREATE (a)-[:KNOWS {since:1}]->(b)",
		"MATCH (a:Person {key:'sc0'}),(b:Person {key:'sc1'}) CREATE (a)-[:LIKES {since:2}]->(b)",
		"MATCH (a:Person {key:'sc1'}),(b:Person {key:'sc2'}) CREATE (a)-[:KNOWS {since:3}]->(b)",
	}
}

// snapshotCorruptionKey names the i-th fixture Person.
func snapshotCorruptionKey(i int) string { return fmt.Sprintf("sc%d", i) }

// snapshotCorruptionSpecs are the (label, property) indexes the tolerated-
// corruption arm cross-checks against the base data after a rebuild.
func snapshotCorruptionSpecs() []IndexSpec {
	return []IndexSpec{
		{Label: "Person", Property: "city"},
		{Label: "Person", Property: "key"},
		{Label: "Person", Property: "age", Numeric: true},
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// one corruption arm
// ─────────────────────────────────────────────────────────────────────────────

// runSnapshotCorruptionArm drives ONE (component, arm-kind) pair end to end:
// flip a byte, prove the bytes changed, reopen through the normal recovery path,
// adjudicate the refusal, then restore the flip and prove the snapshot recovers
// the exact committed model again.
//
// kind selects the offset: "magic" flips byte 0 — the component header the
// structural reader validates first, which is what raises the component's OWN
// typed sentinel — and "interior" flips a seed-chosen byte anywhere past it,
// which the manifest CRC must catch even when the structure still parses. A
// non-negative pinned overrides the interior draw (the test seam described on
// [snapshotCorruptionOptions.interiorOffsets]).
func runSnapshotCorruptionArm(
	ctx context.Context,
	fx *snapshotCorruptionFixture,
	comp *snapshotCorruptionComponent,
	kind string,
	rnd *Seed,
	pinned int64,
) (snapshotCorruptionArm, []Violation, error) {
	path := fx.snapDir + "/" + comp.file
	before, err := fx.disk.ReadFile(path)
	if err != nil {
		return snapshotCorruptionArm{}, nil, fmt.Errorf("read %s: %w", comp.file, err)
	}
	arm := snapshotCorruptionArm{component: comp.file, kind: kind, size: int64(len(before))}
	if len(before) < 2 {
		return arm, nil, fmt.Errorf("%s is %d bytes: too small to corrupt meaningfully", comp.file, len(before))
	}
	arm.offset = 0
	if kind == "interior" {
		arm.offset = 1 + int64(rnd.Uint64N(uint64(len(before)-1)))
		if pinned >= 0 && pinned < int64(len(before)) {
			arm.offset = pinned
		}
	}

	// --- flip, and PROVE the flip changed the file ---
	if err := fx.disk.CorruptRange(path, arm.offset, 1); err != nil {
		return arm, nil, fmt.Errorf("corrupt %s at %d: %w", comp.file, arm.offset, err)
	}
	after, err := fx.disk.ReadFile(path)
	if err != nil {
		return arm, nil, fmt.Errorf("read back %s: %w", comp.file, err)
	}
	arm.bytesChanged = !bytes.Equal(before, after)
	if !arm.bytesChanged {
		// Undo before surfacing, so the fixture stays usable for the report.
		_ = fx.disk.CorruptRange(path, arm.offset, 1)
		return arm, []Violation{{
			Kind: ViolationVacuousRun, Op: "<snapshot-corruption-nonvacuity>",
			Message: fmt.Sprintf("flipping byte %d of %s changed nothing on disk — every assertion below it would be vacuous",
				arm.offset, comp.file),
		}}, nil
	}

	// --- the durable image as recovery is about to see it ---
	imageBefore := fx.disk.Snapshot()

	st, rerr := OpenSimStore(fx.disk, snapshotCorruptionStoreConfig())
	if st != nil {
		_ = st.Close()
	}
	var vs []Violation
	fail := func(format string, args ...any) {
		vs = append(vs, Violation{
			Kind: ViolationACIDDurability, Op: "<snapshot-corruption:" + comp.file + "/" + kind + ">",
			Message: fmt.Sprintf(format, args...),
		})
	}

	// (a) recovery must REFUSE — and refuse without handing back a graph.
	switch {
	case rerr == nil:
		fail("reopen over a corrupted %s (byte %d flipped) SUCCEEDED: the checkpoint already truncated the WAL,"+
			" so recovery either loaded a damaged image or silently rebuilt an empty graph", comp.file, arm.offset)
	case st != nil:
		fail("reopen over a corrupted %s failed with %v yet still returned a store: a partially-populated graph is observable",
			comp.file, rerr)
	default:
		arm.refused = true
	}

	// (b) the failure must carry the component's typed sentinel. The magic arm
	// requires the component's OWN sentinel; the interior arm requires the
	// corruption to be classified at all, since a byte in a payload region parses
	// fine and is caught by the manifest CRC as a directory-level corruption.
	arm.sentinelMatched = snapshotCorruptionClassify(rerr, comp, kind)
	if arm.refused && !arm.sentinelMatched {
		want := comp.sentinelName
		if kind == "interior" {
			want = comp.sentinelName + " or ErrCorrupted"
		}
		fail("reopen over a corrupted %s failed with %v, want an error matching %s", comp.file, rerr, want)
	}

	// (c) a failed recovery must have mutated NOTHING, and the WAL in particular
	// must still be the post-checkpoint image.
	imageAfter := fx.disk.Snapshot()
	arm.imageIntact = snapshotImagesEqual(imageBefore, imageAfter)
	if !arm.imageIntact {
		fail("the durable image changed across a REFUSED reopen of a corrupted %s: recovery half-applied something before failing",
			comp.file)
	}
	walNow, err := fx.disk.ReadFile(walPathFor(fx.dir))
	if err != nil {
		return arm, vs, fmt.Errorf("read WAL after %s arm: %w", comp.file, err)
	}
	arm.walIntact = bytes.Equal(walNow, fx.walBytes)
	if !arm.walIntact {
		fail("db/wal changed while only %s was corrupted (%d bytes, want %d): the failure is not attributable to the snapshot corruption",
			comp.file, len(walNow), len(fx.walBytes))
	}

	// --- restore (XOR 0xFF is an involution) and prove the image was intact ---
	if err := fx.disk.CorruptRange(path, arm.offset, 1); err != nil {
		return arm, vs, fmt.Errorf("restore %s at %d: %w", comp.file, arm.offset, err)
	}
	restored, err := fx.disk.ReadFile(path)
	if err != nil {
		return arm, vs, fmt.Errorf("read restored %s: %w", comp.file, err)
	}
	if !bytes.Equal(restored, before) {
		return arm, vs, fmt.Errorf("restoring %s at %d did not reproduce the original bytes", comp.file, arm.offset)
	}
	if len(vs) > 0 {
		return arm, vs, nil
	}

	// (d) the CONTROL: with the flip undone the very same image recovers the exact
	// committed model, so the refusal above was caused by the corruption and not
	// by the harness.
	recovered, cv, err := snapshotCorruptionCleanReopen(ctx, fx)
	if err != nil {
		return arm, vs, err
	}
	if len(cv) > 0 {
		return arm, cv, nil
	}
	arm.restoredClean = recovered == len(fx.committed)
	if !arm.restoredClean {
		fail("after restoring %s the clean reopen recovered %d of %d committed nodes: the control does not hold",
			comp.file, recovered, len(fx.committed))
	}
	return arm, vs, nil
}

// snapshotCorruptionClassify reports whether err carries the corruption
// classification the arm requires: the component's OWN typed sentinel for the
// magic arm, or any snapshot corruption sentinel for the interior arm.
//
// The distinction is measured, not assumed. A flip in a component's HEADER
// fails the structural reader, which wraps its own sentinel; a flip in a payload
// region parses cleanly and is caught only when the CRC32C is compared against
// the manifest entry, which surfaces as [snapshot.ErrCorrupted] — the
// directory-level sentinel — without the component's own.
func snapshotCorruptionClassify(err error, comp *snapshotCorruptionComponent, kind string) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, comp.sentinel) {
		return true
	}
	if kind == "interior" {
		return errors.Is(err, snapshot.ErrCorrupted)
	}
	return false
}

// snapshotImagesEqual reports whether two [SimDisk.Snapshot] images hold exactly
// the same paths with exactly the same bytes.
func snapshotImagesEqual(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for path, want := range a {
		got, ok := b[path]
		if !ok || !bytes.Equal(got, want) {
			return false
		}
	}
	return true
}

// snapshotCorruptionCleanReopen reopens the store over the (undamaged) image and
// checks it returns EXACTLY the committed model — no acknowledged node lost, no
// phantom, and the deleted node still absent. It returns the recovered count.
func snapshotCorruptionCleanReopen(ctx context.Context, fx *snapshotCorruptionFixture) (int, []Violation, error) {
	st, err := OpenSimStore(fx.disk, snapshotCorruptionStoreConfig())
	if err != nil {
		return 0, nil, fmt.Errorf("sim: snapshot-corruption clean reopen: %w", err)
	}
	defer func() { _ = st.Close() }()

	got, err := snapshotCorruptionKeys(ctx, NewEngineAdapter(st.Engine()))
	if err != nil {
		return 0, nil, fmt.Errorf("sim: snapshot-corruption read recovered keys: %w", err)
	}
	var vs []Violation
	for _, missing := range setMinus(fx.committed, got) {
		vs = append(vs, Violation{
			Kind: ViolationACIDDurability, Op: "<snapshot-corruption-control>",
			Message: fmt.Sprintf("committed node %q absent from a CLEAN reopen of the published snapshot", missing),
		})
	}
	for _, phantom := range setMinus(got, fx.committed) {
		vs = append(vs, Violation{
			Kind: ViolationACIDConsistency, Op: "<snapshot-corruption-control>",
			Message: fmt.Sprintf("node %q recovered from the published snapshot was never committed (or was deleted)", phantom),
		})
	}
	return len(got), vs, nil
}

// snapshotCorruptionKeys reads the recovered Person keys through the engine.
func snapshotCorruptionKeys(ctx context.Context, engine *EngineAdapter) (map[string]struct{}, error) {
	res, err := engine.Run(ctx, "MATCH (n:Person) RETURN n.key", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Close() }()
	out := make(map[string]struct{})
	for res.Next() {
		if k, ok := res.StringAt(0); ok {
			out[k] = struct{}{}
		}
	}
	return out, res.Err()
}

// ─────────────────────────────────────────────────────────────────────────────
// manifest-level guards
// ─────────────────────────────────────────────────────────────────────────────

// runSnapshotManifestGuards probes the two manifest-level guards the package
// declares beyond corruption: an unsupported schema version
// ([snapshot.ErrManifestUnsupported]) and a manifest larger than
// [snapshot.DefaultMaxManifestBytes] ([snapshot.ErrManifestTooLarge]). Neither is
// a byte flip — both are shapes a hostile or badly-migrated store directory can
// hold — so each is driven by REPLACING manifest.json and then restoring it.
func runSnapshotManifestGuards(
	ctx context.Context, fx *snapshotCorruptionFixture,
) ([]snapshotCorruptionGuardArm, []Violation, error) {
	path := fx.snapDir + "/" + snapshotManifestFile
	original, err := fx.disk.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read manifest: %w", err)
	}

	// Version guard: bump the recorded version past what this build understands.
	//
	// The bump goes through snapshot.LoadManifest -> WriteManifest rather than a
	// byte substitution, so the probe is RE-FRAMED with a valid CRC32C trailer
	// (rmp #2520). A patched-in-place manifest would fail its own checksum and
	// this guard would then observe ErrManifestCorrupted — passing for the wrong
	// reason and never reaching the version gate it exists to test.
	loaded, err := snapshot.LoadManifest(bytes.NewReader(original))
	if err != nil {
		return nil, nil, fmt.Errorf("decode the published manifest: %w", err)
	}
	if loaded.Version != snapshot.ManifestVersion {
		return nil, nil, fmt.Errorf("published manifest is version %d, want %d", loaded.Version, snapshot.ManifestVersion)
	}
	loaded.Version = snapshot.ManifestVersion + 1
	var bumpedBuf bytes.Buffer
	if err := snapshot.WriteManifest(&bumpedBuf, loaded); err != nil {
		return nil, nil, fmt.Errorf("re-frame the version-bumped manifest: %w", err)
	}
	bumped := bumpedBuf.Bytes()
	if bytes.Equal(bumped, original) {
		return nil, nil, fmt.Errorf("the version-bumped manifest is byte-identical to the published one")
	}

	// Size guard: pad past DefaultMaxManifestBytes with JSON whitespace INSIDE the
	// object, so the document stays syntactically valid.
	//
	// Padding inside the object was once the ONLY way to reach this guard: the
	// ceiling bounded what the DECODER consumed, and json.Decoder stops the moment
	// it has read one complete value, so whitespace after the closing brace was
	// never read and a manifest.json of any size on disk was accepted (measured).
	// Since rmp #2520 the ceiling bounds the bytes READ — verifying the CRC32C
	// trailer requires reading to the end of the file — so padding either side now
	// trips it. The padding is kept inside because that is the stricter probe: it
	// reaches the guard under both readings.
	//
	// The probe carries no trailer, which is deliberate. It must fail on its SIZE,
	// and the ceiling is checked before any framing, so an unframed image proves
	// the ordering as well as the guard.
	if original[0] != '{' {
		return nil, nil, fmt.Errorf("manifest does not start with '{': cannot pad inside the object")
	}
	oversize := make([]byte, 0, snapshot.DefaultMaxManifestBytes+len(original)+1)
	oversize = append(oversize, '{')
	oversize = append(oversize, bytes.Repeat([]byte(" "), snapshot.DefaultMaxManifestBytes+1)...)
	oversize = append(oversize, original[1:]...)

	probes := []struct {
		image       []byte
		wantErr     error
		name        string
		wantErrName string
	}{
		{name: "unsupported-version", image: bumped, wantErr: snapshot.ErrManifestUnsupported, wantErrName: "ErrManifestUnsupported"},
		{name: "oversize", image: oversize, wantErr: snapshot.ErrManifestTooLarge, wantErrName: "ErrManifestTooLarge"},
	}

	arms := make([]snapshotCorruptionGuardArm, 0, len(probes))
	var vs []Violation
	for _, p := range probes {
		if err := ctx.Err(); err != nil {
			return arms, vs, err
		}
		if err := snapshotWriteFile(fx.disk, path, p.image); err != nil {
			return arms, vs, fmt.Errorf("write %s manifest: %w", p.name, err)
		}
		st, rerr := OpenSimStore(fx.disk, snapshotCorruptionStoreConfig())
		if st != nil {
			_ = st.Close()
		}
		arm := snapshotCorruptionGuardArm{
			name:        p.name,
			wantErrName: p.wantErrName,
			refused:     rerr != nil && st == nil,
			matched:     errors.Is(rerr, p.wantErr),
		}
		arms = append(arms, arm)
		if !arm.refused {
			vs = append(vs, Violation{
				Kind: ViolationACIDDurability, Op: "<snapshot-manifest-guard:" + p.name + ">",
				Message: fmt.Sprintf("a %s manifest was ACCEPTED by recovery (err=%v)", p.name, rerr),
			})
		} else if !arm.matched {
			vs = append(vs, Violation{
				Kind: ViolationACIDDurability, Op: "<snapshot-manifest-guard:" + p.name + ">",
				Message: fmt.Sprintf("a %s manifest failed with %v, want an error matching %s", p.name, rerr, p.wantErrName),
			})
		}
		if err := snapshotWriteFile(fx.disk, path, original); err != nil {
			return arms, vs, fmt.Errorf("restore manifest after %s: %w", p.name, err)
		}
		if len(vs) > 0 {
			return arms, vs, nil
		}
	}
	return arms, vs, nil
}

// snapshotWriteFile replaces the whole contents of path on disk and makes the
// replacement DURABLE. It is the harness's way of substituting a crafted
// component for the published one; the caller restores the original afterwards.
//
// The durability step is load-bearing, not hygiene. The substituted component
// stands in for one the publish protocol had already fsync'd, so a scenario that
// then crashes must find the crafted bytes, not the ones they replaced. Until
// rmp #2535 the helper could omit it because [SimDisk.CrashHost] retained every
// written byte; once a crash discards what no fsync covered, an unsynced
// substitution is reverted — and, because the O_TRUNC above lowers the durable
// image to zero first, reverted to an EMPTY file, which recovery reports as
// "manifest corrupted: EOF". [SimDisk.MarkDataDurable] rather than a Sync
// because a fixture step must draw nothing from the seed and must not be
// failable by the fault injector.
func snapshotWriteFile(disk *SimDisk, path string, data []byte) error {
	h, err := disk.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
	if err != nil {
		return err
	}
	if _, err := h.Write(data); err != nil {
		_ = h.Close()
		return err
	}
	if err := h.Close(); err != nil {
		return err
	}
	return disk.MarkDataDurable(path)
}

// ─────────────────────────────────────────────────────────────────────────────
// tolerated corruption: indexes/<name>.bin
// ─────────────────────────────────────────────────────────────────────────────

// snapshotIndexArmResult is what the paired index-payload arm MEASURED.
type snapshotIndexArmResult struct {
	// flipped is how many indexes/<name>.bin payloads were corrupted.
	flipped int
	// registered is how many indexes the reopened engine registered. Both sides
	// are compared against it rather than against a constant, so the arm cannot
	// drift out of step with the engine's internal numeric companions or with the
	// UNIQUE constraint's backing index.
	registered int
	// controlHydrated / controlRebuilt are the population counters of the reopen
	// over the INTACT payloads; corruptHydrated / corruptRebuilt those of the
	// reopen over the flipped ones, and corruptUnreadable how many payloads that
	// reopen was told it could not use.
	controlHydrated, controlRebuilt int
	corruptHydrated, corruptRebuilt int
	corruptUnreadable               int
	// verified records that BOTH reopens returned the exact committed node set
	// with every declared index still agreeing with a full label scan.
	verified bool
}

// runSnapshotIndexToleranceArm drives the PAIRED index-payload arm: one reopen
// over the intact payloads, which must HYDRATE every index it registers, and one
// over payloads with a byte flipped in each, which must REBUILD every index it
// registers — and both must answer identically.
//
// It requires the opposite of a fail-stop from the corrupt half:
// `snapshot.LoadIndexes` reports a CRC-failing payload as nil bytes, recovery
// classifies it as [recovery.ErrIndexPayloadUnreadable], and the engine rebuilds
// that index from the LPG, so the reopen must SUCCEED.
//
// The control half is what makes the corrupt half mean anything. "Every index
// rebuilt" is the normal outcome of a reopen that CANNOT hydrate — which is
// exactly what this harness did before rmp #2490 — so on its own it does not
// distinguish "rebuilt because the payload was corrupt" from "rebuilt whatever
// the payload said". Proving the same fixture hydrates when the payloads are
// intact is what turns the corrupt arm into a real observation.
//
// Tolerance is only acceptable if it is invisible in the results, so neither
// half stops at "no error": each cross-checks every declared index against a
// full label scan on its reopened engine ([CheckIndexConsistency]) — which is
// what separates "rebuilt" from "silently missing entries" — and each re-reads
// the committed key set.
func runSnapshotIndexToleranceArm(
	ctx context.Context, fx *snapshotCorruptionFixture, rnd *Seed,
) (snapshotIndexArmResult, []Violation, error) {
	var res snapshotIndexArmResult
	paths := snapshotIndexPayloadPaths(fx)
	if len(paths) == 0 {
		return res, nil, fmt.Errorf("the fixture published no indexes/ payloads: the arm would be vacuous")
	}

	// ── CONTROL: the payloads are intact, so every index must hydrate. ────────
	control, cvs, cerr := snapshotIndexReopen(ctx, fx, "control")
	if cerr != nil {
		return res, nil, cerr
	}
	res.registered = control.registered
	res.controlHydrated, res.controlRebuilt = control.hydrated, control.rebuilt
	if len(cvs) > 0 {
		return res, cvs, nil
	}
	if vs := checkSnapshotIndexPopulation("control", control,
		"every payload is intact and the checkpoint left no WAL suffix, so every index must be HYDRATED",
		func(p cypher.RecoveredIndexPopulation, registered int) bool {
			return p.Hydrated == registered && p.Rebuilt == 0
		}); len(vs) > 0 {
		return res, vs, nil
	}

	// ── CORRUPT: flip one interior byte of every payload. ─────────────────────
	flipped := make(map[string]int64, len(paths))
	for _, path := range paths {
		img, err := fx.disk.ReadFile(path)
		if err != nil {
			return res, nil, fmt.Errorf("read index payload %s: %w", path, err)
		}
		if len(img) < 2 {
			continue
		}
		off := int64(rnd.Uint64N(uint64(len(img))))
		if err := fx.disk.CorruptRange(path, off, 1); err != nil {
			return res, nil, fmt.Errorf("corrupt index payload %s: %w", path, err)
		}
		now, err := fx.disk.ReadFile(path)
		if err != nil {
			return res, nil, fmt.Errorf("read back index payload %s: %w", path, err)
		}
		if bytes.Equal(img, now) {
			return res, nil, fmt.Errorf("flipping byte %d of %s changed nothing on disk", off, path)
		}
		flipped[path] = off
	}
	res.flipped = len(flipped)
	// Restore on every exit path, so the fixture stays usable for later arms.
	defer func() {
		for path, off := range flipped {
			_ = fx.disk.CorruptRange(path, off, 1)
		}
	}()

	corrupt, kvs, kerr := snapshotIndexReopen(ctx, fx, "corrupt")
	if kerr != nil {
		return res, nil, kerr
	}
	res.corruptHydrated, res.corruptRebuilt = corrupt.hydrated, corrupt.rebuilt
	res.corruptUnreadable = corrupt.unreadable
	if len(kvs) > 0 {
		return res, kvs, nil
	}
	if vs := checkSnapshotIndexPopulation("corrupt", corrupt,
		"every payload has a flipped byte, so every index must be REBUILT from the recovered graph"+
			" — never a fail-stop, because an index is derived data",
		func(p cypher.RecoveredIndexPopulation, registered int) bool {
			return p.Rebuilt == registered && p.Hydrated == 0
		}); len(vs) > 0 {
		return res, vs, nil
	}
	// The corruption must be the REASON, not a coincidence: every payload the
	// reopen was offered must have been reported unreadable.
	if corrupt.unreadable != res.flipped {
		return res, []Violation{{
			Kind: ViolationVacuousRun, Op: "<snapshot-index-tolerance:corrupt>",
			Message: fmt.Sprintf("%d payloads were flipped but recovery reported only %d unreadable:"+
				" the rebuilds cannot all be attributed to the corruption", res.flipped, corrupt.unreadable),
		}}, nil
	}
	// Both halves must have populated the SAME number of indexes, or the two are
	// not comparable and the pairing proves nothing.
	if corrupt.registered != control.registered {
		return res, []Violation{{
			Kind: ViolationOracleDeviation, Op: "<snapshot-index-tolerance>",
			Message: fmt.Sprintf("the control reopen registered %d indexes and the corrupt reopen %d:"+
				" the two halves are not comparable", control.registered, corrupt.registered),
		}}, nil
	}
	res.verified = true
	return res, nil, nil
}

// snapshotIndexReopenResult is one half of the paired arm: the population
// counters of that reopen plus how many indexes it registered.
type snapshotIndexReopenResult struct {
	registered            int
	hydrated, rebuilt     int
	unreadable, corrupted int
}

// snapshotIndexReopen reopens the fixture through the normal recovery path and
// verifies the reopened engine answers correctly: every declared index must
// agree with a full label scan, and the committed key set must come back exactly.
//
// It is shared by both halves of the paired arm deliberately. The whole claim
// being tested is that a hydrated index and a rebuilt one are indistinguishable
// in their answers, so both halves must be held to ONE verification body — two
// near-identical bodies could drift and let one half be checked more weakly than
// the other.
func snapshotIndexReopen(
	ctx context.Context, fx *snapshotCorruptionFixture, half string,
) (snapshotIndexReopenResult, []Violation, error) {
	var out snapshotIndexReopenResult
	op := "<snapshot-index-tolerance:" + half + ">"
	st, rerr := OpenSimStore(fx.disk, snapshotCorruptionStoreConfig())
	if rerr != nil {
		return out, []Violation{{
			Kind: ViolationOracleDeviation, Op: op,
			Message: fmt.Sprintf("the %s reopen FAILED (%v); a damaged indexes/<name>.bin is a per-index"+
				" REBUILD and never a fail-stop, so either that contract or this arm is wrong", half, rerr),
		}}, nil
	}
	defer func() { _ = st.Close() }()

	pop := st.RecoveredIndexPopulation()
	out.registered = len(st.Engine().ListIndexes())
	out.hydrated, out.rebuilt = pop.Hydrated, pop.Rebuilt
	out.unreadable, out.corrupted = pop.PayloadUnreadable, pop.PayloadCorrupted

	engine := NewEngineAdapter(st.Engine())
	if vs := CheckIndexConsistency(0, nil, engine, snapshotCorruptionSpecs()...); len(vs) > 0 {
		return out, vs, nil
	}
	// A consistent index over a graph that lost rows is still wrong, so the
	// committed model is re-read too.
	got, err := snapshotCorruptionKeys(ctx, engine)
	if err != nil {
		return out, nil, fmt.Errorf("read keys after the %s index reopen: %w", half, err)
	}
	if len(setMinus(fx.committed, got)) > 0 || len(setMinus(got, fx.committed)) > 0 {
		return out, []Violation{{
			Kind: ViolationACIDDurability, Op: op,
			Message: fmt.Sprintf("the %s reopen returned %d nodes, want the %d committed",
				half, len(got), len(fx.committed)),
		}}, nil
	}
	return out, nil, nil
}

// checkSnapshotIndexPopulation adjudicates one half's population counters
// against the side that half requires, anchored to the number of indexes the
// reopen actually registered.
//
// The anchor matters. "Some index hydrated" and "no index hydrated" are both
// satisfiable by an engine that populated nothing, so the count is compared with
// an INDEPENDENT measure of how many indexes exist — the manager's own
// registered-name list — and the arm additionally requires that populating
// accounts for every one of them.
func checkSnapshotIndexPopulation(
	half string, r snapshotIndexReopenResult, why string,
	ok func(cypher.RecoveredIndexPopulation, int) bool,
) []Violation {
	op := "<snapshot-index-tolerance:" + half + ">"
	pop := cypher.RecoveredIndexPopulation{
		Hydrated: r.hydrated, Rebuilt: r.rebuilt,
		PayloadUnreadable: r.unreadable, PayloadCorrupted: r.corrupted,
	}
	if r.registered == 0 {
		return []Violation{{
			Kind: ViolationVacuousRun, Op: op,
			Message: "the reopened engine registered NO index, so a population assertion of either side" +
				" is vacuous: there was nothing to populate",
		}}
	}
	if pop.Hydrated+pop.Rebuilt != r.registered {
		return []Violation{{
			Kind: ViolationOracleDeviation, Op: op,
			Message: fmt.Sprintf("the %s reopen populated %d indexes (hydrated %d + rebuilt %d) but registered %d:"+
				" an index registered without being populated is seekable while empty",
				half, pop.Hydrated+pop.Rebuilt, pop.Hydrated, pop.Rebuilt, r.registered),
		}}
	}
	if !ok(pop, r.registered) {
		return []Violation{{
			Kind: ViolationOracleDeviation, Op: op,
			Message: fmt.Sprintf("%s: got hydrated=%d rebuilt=%d over %d registered indexes"+
				" (payload unreadable=%d, corrupted=%d)",
				why, pop.Hydrated, pop.Rebuilt, r.registered, pop.PayloadUnreadable, pop.PayloadCorrupted),
		}}
	}
	return nil
}

// snapshotIndexPayloadPaths lists the published indexes/<name>.bin files, in a
// deterministic order.
func snapshotIndexPayloadPaths(fx *snapshotCorruptionFixture) []string {
	prefix := fx.snapDir + "/" + snapshot.IndexesDir + "/"
	var out []string
	for path := range fx.disk.Snapshot() {
		if strings.HasPrefix(path, prefix) {
			out = append(out, path)
		}
	}
	slices.Sort(out)
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// the manifest's JSON key names, and the MVCC clock floor behind them
// ─────────────────────────────────────────────────────────────────────────────

// snapshotManifestGapResult is what the manifest key-region arm measured.
type snapshotManifestGapResult struct {
	// detected records whether recovery rejected the key-name flip.
	detected bool
	// cleanTS is the MVCC clock floor recovery derives from the intact manifest.
	// corruptTS is the floor it derives from the key-flipped one, and is only
	// meaningful when detected is false — a refused reopen restores no clock at
	// all, which is the point.
	cleanTS, corruptTS uint64
}

// runSnapshotManifestGapArm drives the arm aimed at what used to be the store's
// only un-checksummed durable region: a JSON KEY NAME inside manifest.json.
//
// A byte flipped there leaves a syntactically valid document whose key
// `encoding/json` no longer recognises, so the field is silently dropped and
// decodes to its zero value. Until rmp #2520 nothing caught that, because the
// manifest carried the CRC32C of every other component and none of its own; 360
// of 1399 bytes (25.7%) of a published manifest could be flipped with no error
// from recovery.
//
// The arm flips a byte of the `commit_ts` key specifically, because that field
// is the MVCC clock floor recovery restores (`recovery.Result.MaxCommitTS` ->
// `RestoreMVCCClock`, rmp #2309) and zeroing it makes a reopened graph re-mint
// instants the image already contains — the worst measured consequence of the
// gap, and the one rmp #2309 exists to prevent.
//
// It now asserts the closed behaviour: the flip MUST be detected, so the clock
// floor is not silently zeroable. The arm keeps measuring the intact floor first,
// which is what makes the assertion non-vacuous — a fixture that restored no
// clock at all would satisfy "the floor was not zeroed" for the wrong reason.
// It also keeps the containment check for the case where a future change makes
// the flip survivable again: whatever happens to the clock, the reopen must
// still return exactly the committed node set.
func runSnapshotManifestGapArm(
	ctx context.Context, fx *snapshotCorruptionFixture,
) (snapshotManifestGapResult, []Violation, error) {
	var res snapshotManifestGapResult
	path := fx.snapDir + "/" + snapshotManifestFile
	original, err := fx.disk.ReadFile(path)
	if err != nil {
		return res, nil, fmt.Errorf("read manifest: %w", err)
	}
	// The key is `"commit_ts"`; flip a byte of the NAME, not of the value.
	idx := bytes.Index(original, []byte(`"commit_ts"`))
	if idx < 0 {
		return res, nil, fmt.Errorf("the published manifest carries no commit_ts field: the gap arm would be vacuous")
	}
	off := int64(idx + 3) // inside the key name, past the opening quote

	if res.cleanTS, err = snapshotCorruptionClockFloor(fx); err != nil {
		return res, nil, fmt.Errorf("clock floor from the intact manifest: %w", err)
	}
	if res.cleanTS == 0 {
		return res, nil, fmt.Errorf("the intact manifest yielded a zero MVCC clock floor: the gap arm has nothing to measure")
	}

	if err := fx.disk.CorruptRange(path, off, 1); err != nil {
		return res, nil, fmt.Errorf("corrupt the commit_ts key: %w", err)
	}
	defer func() { _ = fx.disk.CorruptRange(path, off, 1) }()
	damaged, err := fx.disk.ReadFile(path)
	if err != nil {
		return res, nil, fmt.Errorf("read back the manifest: %w", err)
	}
	if bytes.Equal(original, damaged) {
		return res, nil, fmt.Errorf("flipping byte %d of the manifest changed nothing on disk", off)
	}

	st, rerr := OpenSimStore(fx.disk, snapshotCorruptionStoreConfig())
	var vs []Violation
	if rerr != nil {
		// The manifest's own CRC32C trailer caught the flip (rmp #2520). Recovery
		// fail-stops, so no clock floor is restored at all and the MVCC clock
		// cannot be zeroed by this damage.
		res.detected = true
		if st != nil {
			_ = st.Close()
			vs = append(vs, Violation{
				Kind: ViolationACIDDurability, Op: "<snapshot-manifest-key-region>",
				Message: fmt.Sprintf("a refused reopen over a key-flipped manifest (%v) still returned a store:"+
					" a partially-populated graph is observable", rerr),
			})
		}
		if !errors.Is(rerr, snapshot.ErrManifestCorrupted) {
			vs = append(vs, Violation{
				Kind: ViolationACIDDurability, Op: "<snapshot-manifest-key-region>",
				Message: fmt.Sprintf("a manifest key-name flip was refused with %v, want ErrManifestCorrupted", rerr),
			})
		}
		return res, vs, nil
	}
	defer func() { _ = st.Close() }()

	// Recovery ACCEPTED a manifest whose bytes were damaged. That is the defect
	// rmp #2520 closed, so it is a violation and not a measurement any more.
	if res.corruptTS, err = snapshotCorruptionClockFloor(fx); err != nil {
		return res, nil, fmt.Errorf("clock floor from the key-flipped manifest: %w", err)
	}
	vs = append(vs, Violation{
		Kind: ViolationACIDDurability, Op: "<snapshot-manifest-key-region>",
		Message: fmt.Sprintf("a flipped byte in the manifest's commit_ts KEY NAME was ACCEPTED by recovery"+
			" (MVCC clock floor %d -> %d): the manifest's CRC32C trailer is not covering its own key region",
			res.cleanTS, res.corruptTS),
	})
	// Containment: whatever the clock did, the undetected flip must not have cost
	// a committed node.
	got, err := snapshotCorruptionKeys(ctx, NewEngineAdapter(st.Engine()))
	if err != nil {
		return res, vs, fmt.Errorf("read keys after the manifest key flip: %w", err)
	}
	for _, missing := range setMinus(fx.committed, got) {
		vs = append(vs, Violation{
			Kind: ViolationACIDDurability, Op: "<snapshot-manifest-key-region>",
			Message: fmt.Sprintf("an UNDETECTED manifest key-name flip lost committed node %q:"+
				" the damage is not even contained", missing),
		})
	}
	return res, vs, nil
}

// snapshotCorruptionClockFloor reports the MVCC clock floor
// ([recovery.Result.MaxCommitTS]) recovery derives from the durable image as it
// currently stands. It runs the real full-stack recovery core over the SimDisk,
// so the number is what a production reopen would restore the clock to.
func snapshotCorruptionClockFloor(fx *snapshotCorruptionFixture) (uint64, error) {
	res, err := recovery.OpenFS[string, float64](
		simRecoveryFS{disk: fx.disk}, fx.dir,
		recovery.Options[string, float64]{
			Codec:       txn.NewStringCodec(),
			WeightCodec: txn.NewFloat64WeightCodec(),
		},
	)
	if err != nil {
		return 0, err
	}
	return res.MaxCommitTS, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// terminal non-vacuity gate
// ─────────────────────────────────────────────────────────────────────────────

// checkSnapshotCorruptionNonVacuity is the assert-something-was-seen gate. A
// battery whose arms silently did nothing would pass by construction, so the run
// must show that every component of the intended sweep was really corrupted with
// bytes that really changed, that every typed sentinel was really observed, that
// the refusals really left the image intact, and that the tolerated and
// un-checksummed arms really ran.
func checkSnapshotCorruptionNonVacuity(
	opts snapshotCorruptionOptions, ev *snapshotCorruptionEvidence,
) []Violation {
	const op = "snapshot-corruption non-vacuity"
	var vs []Violation
	fail := func(format string, args ...any) {
		vs = append(vs, Violation{
			Kind: ViolationOracleDeviation, Op: op,
			Message: fmt.Sprintf(format, args...),
		})
	}

	plan := opts.plan()
	wantArms := len(plan)
	if !opts.skipInteriorArm {
		for _, comp := range plan {
			if comp.crcCovered {
				wantArms++
			}
		}
	}
	if len(ev.arms) != wantArms {
		fail("the run drove %d corruption arms, want %d (one per component per arm kind)", len(ev.arms), wantArms)
	}
	// Every intended component must appear, with its bytes proven changed, its
	// refusal recorded, and the image proven intact across it.
	seen := make(map[string]int, len(plan))
	for i := range ev.arms {
		a := &ev.arms[i]
		seen[a.component]++
		if !a.bytesChanged {
			fail("the %s/%s arm never changed a byte on disk", a.component, a.kind)
		}
		if !a.refused {
			fail("the %s/%s arm did not record a refusal", a.component, a.kind)
		}
		if !a.sentinelMatched {
			fail("the %s/%s arm matched no corruption sentinel", a.component, a.kind)
		}
		if !a.imageIntact || !a.walIntact {
			fail("the %s/%s arm did not establish that the refused reopen left the image and the WAL intact", a.component, a.kind)
		}
		if !a.restoredClean {
			fail("the %s/%s arm never re-established the clean control after restoring the flip", a.component, a.kind)
		}
	}
	for _, comp := range plan {
		if seen[comp.file] == 0 {
			fail("component %s was never corrupted: the sweep did not cover its typed sentinel %s", comp.file, comp.sentinelName)
		}
	}
	// Every typed sentinel in the plan must have been OBSERVED, not merely
	// expected: a sentinel the store stopped raising would otherwise pass.
	for _, comp := range plan {
		if !slices.Contains(ev.sentinelsSeen, comp.sentinelName) {
			fail("sentinel %s was never observed during the run", comp.sentinelName)
		}
	}

	if !opts.skipManifestGuards {
		if len(ev.guards) != 2 {
			fail("the run drove %d manifest guards, want 2 (unsupported-version and oversize)", len(ev.guards))
		}
		for i := range ev.guards {
			if g := &ev.guards[i]; !g.matched {
				fail("the %s manifest guard never matched %s", g.name, g.wantErrName)
			}
		}
	}
	if !opts.skipIndexTolerance {
		if ev.indexPayloadsCorrupted == 0 {
			fail("no indexes/<name>.bin payload was corrupted: the tolerated-corruption arm was vacuous")
		}
		if !ev.indexRebuildVerified {
			fail("the index-payload arm never verified BOTH reopens against a full scan and the committed model")
		}
		// The PAIRING itself must be non-vacuous (rmp #2490). Each clause below is
		// a way the arm could complete while proving nothing about the hydration
		// axis, which is exactly what it did before the control half existed.
		if ev.indexRegistered == 0 {
			fail("the index-payload arm registered no index in either reopen: both population assertions were vacuous")
		}
		if ev.indexControlHydrated == 0 {
			fail("the CONTROL reopen (intact payloads) hydrated nothing, so the corrupt reopen's" +
				" all-rebuilt result is indistinguishable from an engine that never hydrates")
		}
		if ev.indexCorruptRebuilt == 0 {
			fail("the CORRUPT reopen rebuilt nothing, so the flipped payloads changed no decision")
		}
		if ev.indexCorruptUnreadable == 0 {
			fail("the CORRUPT reopen reported no unreadable payload, so its rebuilds cannot be" +
				" attributed to the corruption this arm injected")
		}
	}
	if !opts.skipManifestGap && !ev.manifestGapRan {
		fail("the un-checksummed manifest key-region arm never ran: the battery has no reachability control")
	}
	if ev.cleanRecovered != ev.committed {
		fail("the baseline clean reopen recovered %d of %d committed nodes", ev.cleanRecovered, ev.committed)
	}
	return vs
}

// snapshotCorruptionReport renders violations as a scenario report.
func snapshotCorruptionReport(seed uint64, v []Violation) *SimReport {
	return &SimReport{
		Scenario:   ScenarioSnapshotCorruptionFailStop,
		Mode:       ModeDeterministic,
		Seed:       seed,
		FailedOp:   Op{Kind: OpMatch, Cypher: "<snapshot component corruption>"},
		Violations: v,
	}
}
