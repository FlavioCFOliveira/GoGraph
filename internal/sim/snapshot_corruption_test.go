package sim

// snapshot_corruption_test.go — tests for the rmp #2467 snapshot component
// corruption battery. They cover three obligations beyond "the scenario
// passes": that the sweep really reaches all nine typed sentinels, that each
// oracle FIRES when the property it guards is broken (sensitivity), and that the
// two non-fail-stop behaviours the battery pins — a tolerated index payload and
// the manifest's un-checksummed key region — are what the store actually does
// rather than what it was assumed to do.

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
)

// snapshotCorruptionSeeds are a small fixed spread of seeds. The scenario is
// bit-reproducible and each run covers every component, so a fixed set varies
// only the fixture and the interior offsets.
var snapshotCorruptionSeeds = []uint64{0x2467_C0DE, 0x1, 0x9E3779B9, 0xD125F00D}

// TestSnapshotCorruption_Scenario runs the registered scenario across seeds:
// every published component fail-stops recovery with its typed sentinel, no
// half-graph is observable, the WAL and the rest of the image are untouched, and
// the restored image still recovers the exact committed model.
func TestSnapshotCorruption_Scenario(t *testing.T) {
	defer goleak.VerifyNone(t)
	sc := snapshotCorruptionFailStopScenario()
	for _, seed := range snapshotCorruptionSeeds {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		report, err := sc.Run(ctx, seed)
		cancel()
		if err != nil {
			t.Fatalf("seed %#x run error: %v", seed, err)
		}
		if report != nil {
			t.Fatalf("seed %#x reported a violation:\n%s", seed, report)
		}
	}
}

// TestSnapshotCorruption_CoversEveryTypedSentinel is the coverage assertion the
// task exists for: ONE run must observe all nine typed corruption sentinels the
// store declares, each raised by corrupting the component that owns it.
func TestSnapshotCorruption_CoversEveryTypedSentinel(t *testing.T) {
	defer goleak.VerifyNone(t)
	ev, report, err := runSnapshotCorruptionWith(context.Background(), 0x2467_C0DE, defaultSnapshotCorruptionOptions())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if report != nil {
		t.Fatalf("run reported a violation:\n%s", report)
	}
	want := snapshotCorruptionComponents()
	if len(want) != 9 {
		t.Fatalf("the sweep names %d components, want the 9 typed sentinels", len(want))
	}
	for _, comp := range want {
		found := false
		for _, seen := range ev.sentinelsSeen {
			if seen == comp.sentinelName {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("sentinel %s (from %s) was never observed", comp.sentinelName, comp.file)
		}
	}
	// Every component got a magic arm, and every CRC-COVERED one also got the
	// seed-chosen interior arm — since rmp #2520 that is all nine, manifest.json
	// included, because it now carries a CRC32C trailer over its own bytes.
	wantArms := len(want)
	for _, comp := range want {
		if comp.crcCovered {
			wantArms++
		}
	}
	if got := len(ev.arms); got != wantArms {
		t.Fatalf("drove %d arms, want %d", got, wantArms)
	}
	for i := range ev.arms {
		a := &ev.arms[i]
		if !a.bytesChanged || !a.refused || !a.sentinelMatched || !a.imageIntact || !a.walIntact || !a.restoredClean {
			t.Errorf("arm %s/%s incomplete: %+v", a.component, a.kind, *a)
		}
	}
	if !ev.indexRebuildVerified || ev.indexPayloadsCorrupted == 0 {
		t.Errorf("the index-payload tolerance arm did not run to completion: corrupted=%d verified=%v",
			ev.indexPayloadsCorrupted, ev.indexRebuildVerified)
	}
	if len(ev.guards) != 2 {
		t.Errorf("drove %d manifest guards, want 2", len(ev.guards))
	}
}

// TestSnapshotCorruption_DegenerateSweepFailsTheGate is the sensitivity seam for
// the terminal non-vacuity gate: a plan that corrupts only one component must be
// REJECTED, because the run then leaves eight typed sentinels unobserved.
func TestSnapshotCorruption_DegenerateSweepFailsTheGate(t *testing.T) {
	defer goleak.VerifyNone(t)
	full := snapshotCorruptionComponents()
	opts := defaultSnapshotCorruptionOptions()
	opts.components = full[:1] // csr.bin's sentinel alone would otherwise "pass"

	// The run itself passes its per-arm oracles; the gate is what must fire, so
	// drive the gate directly against the evidence a degenerate run produced.
	ev, report, err := runSnapshotCorruptionWith(context.Background(), 0x2467_C0DE, opts)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if report != nil {
		t.Fatalf("the degenerate run failed its per-arm oracles, not the gate:\n%s", report)
	}
	vs := checkSnapshotCorruptionNonVacuity(defaultSnapshotCorruptionOptions(), ev)
	if len(vs) == 0 {
		t.Fatal("the non-vacuity gate accepted a run that corrupted one of nine components")
	}
	joined := violationsText(vs)
	for _, comp := range full[1:] {
		if !strings.Contains(joined, comp.sentinelName) {
			t.Errorf("the gate did not name the unobserved sentinel %s:\n%s", comp.sentinelName, joined)
		}
	}
}

// TestSnapshotCorruption_OracleFiresWhenRecoveryWronglySucceeds is the
// reachability proof for the battery's primary oracle. Every arm asserts that
// recovery REFUSED; an assertion that can only ever hold proves nothing, so this
// test aims one arm at a component whose corruption the store is KNOWN to
// tolerate, and requires the run to REPORT the acceptance rather than pass.
//
// The substrate is an indexes/<name>.bin payload. It is the only durable file in
// a published snapshot a reopen accepts damaged, because a corrupt index payload
// is a rebuild trigger rather than a fatal error (pinned by
// TestSnapshotCorruption_IndexPayloadIsToleratedNotIgnored). Every other
// component — manifest.json now included, since rmp #2520 gave it a CRC32C
// trailer over its own bytes — is checksummed end to end, so there is no byte in
// one whose corruption recovery would wrongly accept.
//
// It used to be built on a byte of the manifest's `commit_ts` KEY NAME, which
// was outside every checksum. Closing that gap removed the control's substrate,
// so the control moved rather than being dropped.
func TestSnapshotCorruption_OracleFiresWhenRecoveryWronglySucceeds(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fx, err := buildSnapshotCorruptionFixture(ctx, 0x2467_C0DE)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	paths := snapshotIndexPayloadPaths(fx)
	if len(paths) == 0 {
		t.Fatal("the fixture published no index payloads: the reachability control has no substrate")
	}
	rel := strings.TrimPrefix(paths[0], fx.snapDir+"/")
	if rel == paths[0] {
		t.Fatalf("index payload %q is not under the snapshot directory %q", paths[0], fx.snapDir)
	}

	// Drive the arm as if a damaged index payload had to fail-stop. It does not,
	// so the arm must report that recovery succeeded.
	tolerated := snapshotCorruptionComponent{
		file: rel, sentinel: snapshot.ErrCorrupted, sentinelName: "ErrCorrupted",
	}
	opts := defaultSnapshotCorruptionOptions()
	opts.components = []snapshotCorruptionComponent{tolerated}
	opts.skipManifestGuards = true
	opts.skipIndexTolerance = true
	opts.skipManifestGap = true

	_, report, err := runSnapshotCorruptionWith(ctx, 0x2467_C0DE, opts)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if report == nil {
		t.Fatal("the battery PASSED a corruption recovery wrongly accepted: the refusal oracle is unreachable")
	}
	if !strings.Contains(report.String(), "SUCCEEDED") {
		t.Fatalf("the report does not name the wrongly-accepted reopen:\n%s", report)
	}
}

// TestSnapshotCorruption_NoOpCorruptionIsRejected proves the battery cannot pass
// on a corruption that changed nothing. It is the guard against the quietest
// false pass available here: a flip in a region the file does not really hold.
func TestSnapshotCorruption_NoOpCorruptionIsRejected(t *testing.T) {
	defer goleak.VerifyNone(t)
	arm := snapshotCorruptionArm{component: snapshot.CSRFile, kind: "magic"}
	ev := &snapshotCorruptionEvidence{arms: []snapshotCorruptionArm{arm}}
	vs := checkSnapshotCorruptionNonVacuity(snapshotCorruptionOptions{
		components:         snapshotCorruptionComponents()[1:2],
		skipInteriorArm:    true,
		skipManifestGuards: true,
		skipIndexTolerance: true,
		skipManifestGap:    true,
	}, ev)
	if !strings.Contains(violationsText(vs), "never changed a byte on disk") {
		t.Fatalf("the gate accepted an arm that changed no byte:\n%s", violationsText(vs))
	}
}

// TestSnapshotCorruption_EveryComponentByteIsCRCCovered establishes the fact the
// interior arm rests on: there is no byte of any published component whose
// corruption goes unnoticed. It sweeps every byte of the smallest components and
// a stride through the larger ones, requiring each flip to be refused.
//
// manifest.json is included since rmp #2520 gave it a CRC32C trailer over its
// own bytes; it was the one exception before that, and the exception is what
// TestSnapshotCorruption_ManifestUncheckedByteCensus measured.
func TestSnapshotCorruption_EveryComponentByteIsCRCCovered(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fx, err := buildSnapshotCorruptionFixture(ctx, 0x2467_C0DE)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	for _, comp := range snapshotCorruptionComponents() {
		path := fx.snapDir + "/" + comp.file
		img, err := fx.disk.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", comp.file, err)
		}
		stride := max(1, len(img)/16)
		for off := 0; off < len(img); off += stride {
			if err := fx.disk.CorruptRange(path, int64(off), 1); err != nil {
				t.Fatalf("corrupt %s@%d: %v", comp.file, off, err)
			}
			st, rerr := OpenSimStore(fx.disk, snapshotCorruptionStoreConfig())
			if st != nil {
				_ = st.Close()
			}
			if rerr == nil {
				t.Errorf("%s byte %d of %d is NOT covered: recovery accepted the flip", comp.file, off, len(img))
			} else if !errors.Is(rerr, snapshot.ErrCorrupted) && !errors.Is(rerr, comp.sentinel) {
				t.Errorf("%s byte %d flipped -> %v, want ErrCorrupted or %s", comp.file, off, rerr, comp.sentinelName)
			}
			if err := fx.disk.CorruptRange(path, int64(off), 1); err != nil {
				t.Fatalf("restore %s@%d: %v", comp.file, off, err)
			}
		}
	}
}

// TestSnapshotCorruption_ManifestKeyRegionIsChecksummed pins the closure of the
// coverage gap rmp #2467 measured and rmp #2520 fixed.
//
// manifest.json used to carry the CRC32C of every other component and none of
// its own, so a single byte flipped inside a JSON KEY NAME left valid JSON whose
// key encoding/json then ignored, and the field decoded to its zero value with
// no error anywhere. It now carries a CRC32C trailer over its own bytes, so the
// same flip fail-stops recovery with snapshot.ErrManifestCorrupted.
//
// This is the inverse of the assertion that stood here before, which required
// the flip to go UNDETECTED. That test was written as a ratchet — "if the store
// ever gains a manifest integrity check this test fails" — and this is that
// check landing.
func TestSnapshotCorruption_ManifestKeyRegionIsChecksummed(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fx, err := buildSnapshotCorruptionFixture(ctx, 0x2467_C0DE)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	gap, vs, err := runSnapshotManifestGapArm(ctx, fx)
	if err != nil {
		t.Fatalf("gap arm: %v", err)
	}
	if len(vs) > 0 {
		t.Fatalf("the manifest key-region arm reported a violation:\n%s", violationsText(vs))
	}
	// Non-vacuity first: the fixture must really restore a clock floor, or
	// "the floor was not zeroed" would hold for the wrong reason.
	if gap.cleanTS == 0 {
		t.Fatal("the intact manifest yielded a zero clock floor: the assertion below would be vacuous")
	}
	if !gap.detected {
		t.Fatalf("a byte flipped in the manifest's commit_ts KEY NAME was ACCEPTED by recovery"+
			" (clock floor %d -> %d): the CRC32C trailer is not covering the manifest's key region",
			gap.cleanTS, gap.corruptTS)
	}
	t.Logf("manifest key-name flip REFUSED; the intact image still restores an MVCC clock floor of %d", gap.cleanTS)
}

// TestSnapshotCorruption_MVCCClockFloorIsNotSilentlyZeroable is the specific
// durability assertion behind the test above, stated in the terms that matter to
// the engine rather than to the file format.
//
// recovery.Result.MaxCommitTS feeds RestoreMVCCClock (rmp #2309): a graph
// reopened with a floor of 0 re-mints instants the durable image already
// contains, which is silent MVCC corruption rather than a lost node — no test
// that counts recovered nodes can see it. The manifest is the only place that
// floor is stored once a checkpoint has truncated the WAL prefix that carried
// it, so it must not be possible for a byte flip to drop it.
//
// The test sweeps EVERY byte of the published manifest and requires each flip to
// either be refused outright or to leave the floor exactly where it was. A run
// that never observed the intact floor, or never drove a flip, fails.
func TestSnapshotCorruption_MVCCClockFloorIsNotSilentlyZeroable(t *testing.T) {
	defer goleak.VerifyNone(t)
	fx, err := buildSnapshotCorruptionFixture(context.Background(), 0x2467_C0DE)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	path := fx.snapDir + "/" + snapshotManifestFile
	img, err := fx.disk.ReadFile(path)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	clean, err := snapshotCorruptionClockFloor(fx)
	if err != nil {
		t.Fatalf("clock floor from the intact manifest: %v", err)
	}
	if clean == 0 {
		t.Fatal("the intact manifest restores a zero MVCC clock floor: the sweep below would be vacuous")
	}

	refused, accepted := 0, 0
	for off := range int64(len(img)) {
		if err := fx.disk.CorruptRange(path, off, 1); err != nil {
			t.Fatalf("corrupt manifest@%d: %v", off, err)
		}
		got, rerr := snapshotCorruptionClockFloor(fx)
		switch {
		case rerr != nil:
			refused++
		default:
			accepted++
			if got != clean {
				t.Errorf("flipping manifest byte %d of %d moved the MVCC clock floor %d -> %d with no error",
					off, len(img), clean, got)
			}
		}
		if err := fx.disk.CorruptRange(path, off, 1); err != nil {
			t.Fatalf("restore manifest@%d: %v", off, err)
		}
	}
	if refused == 0 {
		t.Fatalf("no manifest flip was refused across %d bytes: the sweep proves nothing", len(img))
	}
	t.Logf("MVCC clock floor %d held across %d manifest byte flips: %d refused, %d accepted without moving it",
		clean, len(img), refused, accepted)
}

// TestSnapshotCorruption_ManifestUncheckedByteCensus is the census the fix is
// measured against: it flips each byte of a PUBLISHED manifest in turn, reopens
// the full stack, and counts the flips recovery accepts.
//
// The figure it produces is the one quoted in docs/dst-feature-coverage.md, and
// it is reproducible rather than asserted. Before rmp #2520 it measured 360 of
// 1399 bytes (25.7%) accepted silently, because manifest.json carried the CRC32C
// of every other component and none of its own. With the CRC32C trailer the only
// acceptable figure is ZERO: every byte of the document and of the trailer is
// covered, so there is no flip a reopen can miss.
func TestSnapshotCorruption_ManifestUncheckedByteCensus(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fx, err := buildSnapshotCorruptionFixture(ctx, 0x2467_C0DE)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	path := fx.snapDir + "/" + snapshotManifestFile
	img, err := fx.disk.ReadFile(path)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var undetected []int64
	for off := range int64(len(img)) {
		if err := fx.disk.CorruptRange(path, off, 1); err != nil {
			t.Fatalf("corrupt manifest@%d: %v", off, err)
		}
		st, rerr := OpenSimStore(fx.disk, snapshotCorruptionStoreConfig())
		if rerr == nil {
			undetected = append(undetected, off)
		}
		if st != nil {
			_ = st.Close()
		}
		if err := fx.disk.CorruptRange(path, off, 1); err != nil {
			t.Fatalf("restore manifest@%d: %v", off, err)
		}
	}
	t.Logf("manifest census: %d of %d bytes (%.1f%%) can be flipped with NO error from recovery",
		len(undetected), len(img), 100*float64(len(undetected))/float64(len(img)))
	if len(undetected) != 0 {
		t.Fatalf("%d of %d manifest bytes were accepted silently (offsets %v):"+
			" the CRC32C trailer does not cover the whole file — update docs/dst-feature-coverage.md if this is intended",
			len(undetected), len(img), undetected)
	}
}

// TestSnapshotCorruption_IndexPayloadIsToleratedNotIgnored pins the second
// non-fail-stop behaviour: a corrupt indexes/<name>.bin is a rebuild trigger,
// not a fatal error — and the rebuild is real, so a seek still agrees with a
// full scan.
func TestSnapshotCorruption_IndexPayloadIsToleratedNotIgnored(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fx, err := buildSnapshotCorruptionFixture(ctx, 0x1)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	paths := snapshotIndexPayloadPaths(fx)
	if len(paths) == 0 {
		t.Fatal("the fixture published no index payloads")
	}
	n, verified, vs, err := runSnapshotIndexToleranceArm(ctx, fx, NewSeed(0x1))
	if err != nil {
		t.Fatalf("tolerance arm: %v", err)
	}
	if len(vs) > 0 {
		t.Fatalf("a corrupt index payload broke recovery:\n%s", violationsText(vs))
	}
	if n != len(paths) || !verified {
		t.Fatalf("tolerance arm corrupted %d of %d payloads, verified=%v", n, len(paths), verified)
	}
	// And the payloads really were restored, so the fixture is reusable.
	for _, path := range paths {
		if !fx.disk.Exists(path) {
			t.Fatalf("index payload %s vanished", path)
		}
	}
}

// TestSnapshotCorruption_FixturePublishesEveryComponent proves the battery's
// precondition: the fixture's DDL, data, parallel typed relationships and node
// deletion really do make all nine components present in the published image.
// Without this, an arm for an absent component would be silently unreachable.
func TestSnapshotCorruption_FixturePublishesEveryComponent(t *testing.T) {
	defer goleak.VerifyNone(t)
	fx, err := buildSnapshotCorruptionFixture(context.Background(), 0x9E3779B9)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	for _, comp := range snapshotCorruptionComponents() {
		path := fx.snapDir + "/" + comp.file
		img, rerr := fx.disk.ReadFile(path)
		if rerr != nil {
			t.Errorf("component %s absent: %v", comp.file, rerr)
			continue
		}
		if len(img) == 0 {
			t.Errorf("component %s is empty", comp.file)
		}
	}
	// The checkpoint must have folded the whole WAL, so the snapshot is the ONLY
	// durable source of the committed graph and a refusal is a genuine fail-stop
	// rather than a fallback onto a stale WAL.
	if len(fx.walBytes) != 0 {
		t.Errorf("db/wal holds %d bytes after the checkpoint, want 0: a refusal could fall back onto it", len(fx.walBytes))
	}
	if len(fx.committed) != snapshotCorruptionNodes-1 {
		t.Errorf("committed model holds %d keys, want %d", len(fx.committed), snapshotCorruptionNodes-1)
	}
}

// TestSnapshotCorruption_ManifestGuardsRejectShapeNotBytes covers the two
// manifest-level guards that are not byte flips: a version this build does not
// understand and a manifest past the size ceiling.
func TestSnapshotCorruption_ManifestGuardsRejectShapeNotBytes(t *testing.T) {
	defer goleak.VerifyNone(t)
	ctx := context.Background()
	fx, err := buildSnapshotCorruptionFixture(ctx, 0x2467_C0DE)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	before, err := fx.disk.ReadFile(fx.snapDir + "/" + snapshotManifestFile)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	arms, vs, err := runSnapshotManifestGuards(ctx, fx)
	if err != nil {
		t.Fatalf("guards: %v", err)
	}
	if len(vs) > 0 {
		t.Fatalf("a manifest guard did not hold:\n%s", violationsText(vs))
	}
	if len(arms) != 2 {
		t.Fatalf("drove %d guards, want 2", len(arms))
	}
	for i := range arms {
		if a := &arms[i]; !a.refused || !a.matched {
			t.Errorf("guard %s: refused=%v matched(%s)=%v", a.name, a.refused, a.wantErrName, a.matched)
		}
	}
	after, err := fx.disk.ReadFile(fx.snapDir + "/" + snapshotManifestFile)
	if err != nil {
		t.Fatalf("read manifest after guards: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("the guard probes did not restore the published manifest")
	}
}

// TestSnapshotCorruption_Deterministic pins bit-reproducibility: the same seed
// run twice must produce the same outcome, so any failure always replays.
func TestSnapshotCorruption_Deterministic(t *testing.T) {
	defer goleak.VerifyNone(t)
	sc := snapshotCorruptionFailStopScenario()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	const seed = 0x5EED5EED
	ev1, r1, e1 := runSnapshotCorruptionWith(ctx, seed, defaultSnapshotCorruptionOptions())
	ev2, r2, e2 := runSnapshotCorruptionWith(ctx, seed, defaultSnapshotCorruptionOptions())
	if e1 != nil || e2 != nil {
		t.Fatalf("%s run errors: %v / %v", sc.Name, e1, e2)
	}
	if (r1 == nil) != (r2 == nil) {
		t.Fatalf("%s non-deterministic: run1 report=%v run2 report=%v", sc.Name, r1, r2)
	}
	if r1 != nil {
		t.Fatalf("%s reported a violation:\n%s", sc.Name, r1)
	}
	// The arm sequence is fixed by the plan, and each interior offset is drawn
	// from the seed and scaled to its component's byte length — so two runs agree
	// on the offset whenever they agree on that length.
	//
	// Two components legitimately vary in length between fixtures built in the
	// same process. manifest.json embeds `created_at`, whose RFC3339 fraction
	// trims trailing zeros; and mapper.bin holds the engine-minted natural keys,
	// which are `__cx_<hex>` drawn from a PROCESS-GLOBAL counter
	// (cypher/exec/create_node.go), so their decimal width — and with it the
	// component's size — grows as a process creates more nodes. The comparison
	// therefore requires identical offsets exactly where the sizes match, and
	// reports a size difference rather than hiding it.
	if len(ev1.arms) != len(ev2.arms) {
		t.Fatalf("arm counts differ: %d vs %d", len(ev1.arms), len(ev2.arms))
	}
	for i := range ev1.arms {
		a, b := &ev1.arms[i], &ev2.arms[i]
		if a.component != b.component || a.kind != b.kind {
			t.Fatalf("arm %d differs: %s/%s vs %s/%s", i, a.component, a.kind, b.component, b.kind)
		}
		if a.size != b.size {
			t.Logf("arm %d (%s/%s): component size %d vs %d across identical runs", i, a.component, a.kind, a.size, b.size)
			continue
		}
		if a.offset != b.offset {
			t.Fatalf("arm %d (%s/%s) drew offset %d then %d over an identically sized component",
				i, a.component, a.kind, a.offset, b.offset)
		}
	}
	if ev1.manifestGapCleanTS != ev2.manifestGapCleanTS || ev1.manifestGapCorruptTS != ev2.manifestGapCorruptTS {
		t.Fatalf("the measured clock floors differ across identical runs: (%d,%d) vs (%d,%d)",
			ev1.manifestGapCleanTS, ev1.manifestGapCorruptTS, ev2.manifestGapCleanTS, ev2.manifestGapCorruptTS)
	}
}

// violationsText joins violation messages for a test assertion.
func violationsText(vs []Violation) string {
	var b strings.Builder
	for i := range vs {
		b.WriteString(vs[i].Message)
		b.WriteByte('\n')
	}
	return b.String()
}
