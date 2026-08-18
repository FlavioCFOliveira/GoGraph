package sim

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
)

// disk_rename_crash_test.go — harness-fidelity gates for the crash semantics of
// [SimDisk.Rename] (rmp #2514).
//
// rename(2) is atomic. A crash immediately after one leaves either the NEW name
// or the OLD name — never neither, because "neither" would mean the file was
// unlinked, which is not a partial outcome of a rename. The simulator modelled
// exactly that impossible third outcome until #2514: [SimDisk.Crash] revoked
// every un-fsync'd directory entry, and nothing put the source name back.
//
// The cost was not theoretical. The snapshot publish protocol issues its two
// renames — archive(live -> live.bak) then publish(staging -> live) — back to
// back with no fsync between them, so a crash landing between them dropped BOTH
// copies of the graph. That made recovery's interrupted-publish promote repair
// unreachable under simulation, and would have made any unarmed mid-rename crash
// report a durability violation the engine never caused.
//
// The gates below are written to the project's three-gate shape: an
// UNCONDITIONAL verdict gate that fails on an illegal outcome; a SEPARATE
// shape-only gate proving the situation actually arose; and witness detail via
// t.Logf only, because an unmet precondition is a fact to report, not a defect.

// publishPairOutcome is the durable image a crash inside the archive/publish
// rename pair left, reduced to the three names that identify it.
type publishPairOutcome struct {
	live    bool
	bak     bool
	staging bool
}

func (o publishPairOutcome) String() string {
	return fmt.Sprintf("live=%t bak=%t staging=%t", o.live, o.bak, o.staging)
}

// The three outcomes a real filesystem can leave after a crash inside the
// publish pair, named for the durable prefix of the two renames that survived.
//
//   - neitherRename — no rename reached stable storage: the live snapshot is
//     still the OLD one and the staging tree is untouched.
//   - archiveOnly — only the archive rename survived: the live name is absent
//     and the previous snapshot is stranded at .bak. This is the state
//     recovery's promote repair exists for.
//   - bothRenames — both survived: the NEW snapshot is live and the old one is
//     still archived.
//
// Nothing else is producible. In particular there is no outcome in which the
// publish rename survives while the archive rename that made room for it does
// not — a journalling filesystem commits its metadata in order, so the durable
// renames are always a prefix of the issued ones — and no outcome in which the
// live name is absent with no backup, which is the impossible one.
var (
	outcomeNeitherRename = publishPairOutcome{live: true, bak: false, staging: true}
	outcomeArchiveOnly   = publishPairOutcome{live: false, bak: true, staging: true}
	outcomeBothRenames   = publishPairOutcome{live: true, bak: true, staging: false}
)

// legalPublishPairOutcomes is the EXACT set a crash inside the pair may produce.
// It is an equality claim, not a subset claim: an outcome outside it is a
// harness defect, and an outcome inside it that never occurs across a spread of
// seeds means the set is over-stated and the model does not really sample it.
func legalPublishPairOutcomes() []publishPairOutcome {
	return []publishPairOutcome{outcomeNeitherRename, outcomeArchiveOnly, outcomeBothRenames}
}

// stagePublishPair reproduces the publish protocol's rename pair on a bare
// SimDisk, up to but excluding the parent-directory fsync that would make the
// publish durable, and crashes there.
//
// Every name it creates is made durable BEFORE the pair runs — the live
// snapshot through its own parent fsync, the staging tree through a staging-dir
// fsync — so the only thing the crash has to adjudicate is the two renames. If
// the durability of any other dirent leaked into the outcome the assertions
// below would be measuring the wrong thing.
func stagePublishPair(t *testing.T, seed uint64) (*SimDisk, publishPairOutcome, int, int) {
	t.Helper()
	d := NewSimDisk(NewSeed(seed), 0) // no data faults: isolate the dirent model

	// A live snapshot, fully published and durable.
	writeFile(t, d, "db/snapshot/manifest.json", []byte("old"))
	if err := d.DirSync("db/snapshot"); err != nil {
		t.Fatalf("DirSync live: %v", err)
	}
	if err := d.ParentDirSync("db/snapshot"); err != nil {
		t.Fatalf("ParentDirSync live: %v", err)
	}
	// A staging tree, fully written and fsync'd, ready to publish.
	writeFile(t, d, "db/snapshot.tmp/manifest.json", []byte("new"))
	if err := d.DirSync("db/snapshot.tmp"); err != nil {
		t.Fatalf("DirSync staging: %v", err)
	}

	// The pair, with NO fsync between them and none after — the exact window.
	if err := d.Rename("db/snapshot", "db/snapshot.bak"); err != nil {
		t.Fatalf("archive rename: %v", err)
	}
	if err := d.Rename("db/snapshot.tmp", "db/snapshot"); err != nil {
		t.Fatalf("publish rename: %v", err)
	}
	pending := d.PendingRenameCount()
	d.Crash()
	_, rolledBack := d.LastCrashRenameOutcome()

	return d, publishPairOutcome{
		live:    d.Exists("db/snapshot/manifest.json"),
		bak:     d.Exists("db/snapshot.bak/manifest.json"),
		staging: d.Exists("db/snapshot.tmp/manifest.json"),
	}, pending, rolledBack
}

// renameCrashSeeds is the seed spread the outcome-set gates sample. It is wide
// enough that each of the three legal outcomes is expected many times over: the
// boundary is drawn uniformly from three values, so the chance any one of them
// is missed across this many seeds is negligible.
func renameCrashSeeds() []uint64 {
	seeds := make([]uint64, 0, 96)
	for i := uint64(0); i < 96; i++ {
		seeds = append(seeds, 0x2514_0000+i*0x9E37_79B1)
	}
	return seeds
}

// TestSimDiskRenameCrash_PublishPairOutcomeSetIsLegal is the VERDICT gate: a
// crash inside the publish pair must never leave an outcome outside the legal
// set — above all never the impossible one in which both copies of the graph are
// gone. It is unconditional: every seed is adjudicated, and a single illegal
// outcome fails.
func TestSimDiskRenameCrash_PublishPairOutcomeSetIsLegal(t *testing.T) {
	legal := make(map[publishPairOutcome]bool, len(legalPublishPairOutcomes()))
	for _, o := range legalPublishPairOutcomes() {
		legal[o] = true
	}
	for _, seed := range renameCrashSeeds() {
		_, got, pending, rolledBack := stagePublishPair(t, seed)
		if !legal[got] {
			t.Fatalf("seed %#x: crash inside the publish pair left %s (pending=%d rolledBack=%d), "+
				"which no filesystem can produce; legal outcomes are %v",
				seed, got, pending, rolledBack, legalPublishPairOutcomes())
		}
		// The load-bearing half of the verdict, stated separately so a
		// regression names itself: at every instant at least one complete
		// snapshot must be on disk.
		if !got.live && !got.bak {
			t.Fatalf("seed %#x: crash left NO snapshot at all (%s) — both names of a rename were lost",
				seed, got)
		}
	}
}

// TestSimDiskRenameCrash_PublishPairReachesEveryLegalOutcome is the SHAPE gate:
// it proves the situation the verdict gate adjudicates actually arises, and that
// the model really samples the whole set rather than collapsing onto one branch.
// An outcome set that is never fully observed is over-stated, and a verdict gate
// over a set with an unreachable member pins less than it claims.
func TestSimDiskRenameCrash_PublishPairReachesEveryLegalOutcome(t *testing.T) {
	seen := make(map[publishPairOutcome]int)
	windowEntered := 0
	for _, seed := range renameCrashSeeds() {
		_, got, pending, _ := stagePublishPair(t, seed)
		seen[got]++
		if pending == 2 {
			windowEntered++
		}
	}

	// Shape 1: the crash really landed between the two renames every time —
	// both were pending, neither had been made durable behind the test's back.
	if windowEntered != len(renameCrashSeeds()) {
		t.Fatalf("the crash landed inside the rename pair on %d of %d seeds; the staging is not producing the window it claims",
			windowEntered, len(renameCrashSeeds()))
	}
	// Shape 2: every legal outcome is actually produced.
	for _, want := range legalPublishPairOutcomes() {
		if seen[want] == 0 {
			t.Errorf("legal outcome %s never occurred across %d seeds: the crash model does not sample it, so the verdict gate's outcome set is over-stated",
				want, len(renameCrashSeeds()))
		}
	}
	// Shape 3: nothing outside the set was produced. The verdict gate already
	// covers this; repeating it here keeps this test self-describing if it is
	// ever read on its own.
	for got := range seen {
		legal := false
		for _, want := range legalPublishPairOutcomes() {
			if got == want {
				legal = true
			}
		}
		if !legal {
			t.Errorf("illegal outcome %s observed", got)
		}
	}

	keys := make([]string, 0, len(seen))
	for o, n := range seen {
		keys = append(keys, fmt.Sprintf("%s x%d", o, n))
	}
	sort.Strings(keys)
	t.Logf("outcome distribution over %d seeds: %s", len(renameCrashSeeds()), strings.Join(keys, " | "))
}

// TestSimDiskRenameCrash_StrandedBackupNeedsNoArming records the win the fix was
// for: the state recovery's promote repair exists to handle is now reachable
// with NO scenario-level arming at all. Before #2514 the default model could not
// produce it — the crash revoked both names — so the repair was dead code under
// simulation and had to be reached with an opt-in primitive.
//
// The gate is unconditional on the spread as a whole (the branch must occur),
// not on any single seed, because which branch a given seed takes is exactly the
// non-determinism being modelled.
func TestSimDiskRenameCrash_StrandedBackupNeedsNoArming(t *testing.T) {
	stranded := 0
	for _, seed := range renameCrashSeeds() {
		_, got, _, _ := stagePublishPair(t, seed)
		if got == outcomeArchiveOnly {
			stranded++
		}
	}
	if stranded == 0 {
		t.Fatalf("the stranded-backup state never arose across %d unarmed seeds: recovery's promote repair is unreachable by default",
			len(renameCrashSeeds()))
	}
	t.Logf("stranded-backup reached without arming on %d of %d seeds", stranded, len(renameCrashSeeds()))
}

// TestSimDiskRenameCrash_OutcomeIsSeedReproducible pins the determinism the whole
// harness rests on: the same seed must produce the same outcome every time, and
// the choice must come from the seed rather than from map ordering or the clock.
func TestSimDiskRenameCrash_OutcomeIsSeedReproducible(t *testing.T) {
	for _, seed := range renameCrashSeeds()[:16] {
		_, first, _, firstRolled := stagePublishPair(t, seed)
		for rep := 0; rep < 3; rep++ {
			_, again, _, againRolled := stagePublishPair(t, seed)
			if again != first || againRolled != firstRolled {
				t.Fatalf("seed %#x replay %d: got %s (rolledBack=%d), first run gave %s (rolledBack=%d) — the crash outcome is not a function of the seed",
					seed, rep, again, againRolled, first, firstRolled)
			}
		}
	}
}

// TestSimDiskRenameCrash_DoesNotPerturbTheFaultStream proves the new sub-stream
// is genuinely independent: turning the rename model on must not shift the
// torn-write / Sync fault sequence every other arm on this disk is careful to
// leave alone. Two disks on the same seed, one of which performs renames and one
// of which does not, must draw the same fault decisions.
func TestSimDiskRenameCrash_DoesNotPerturbTheFaultStream(t *testing.T) {
	// A high fault rate so the stream is observable at all: at rate 0 every draw
	// returns false regardless of position, and the test could not fail.
	const rate = 0.5
	syncOutcomes := func(withRenames bool) []bool {
		d := NewSimDisk(NewSeed(0x2514_F00D), rate)
		h, err := d.OpenFile("probe", os.O_CREATE|os.O_WRONLY)
		if err != nil {
			t.Fatalf("OpenFile: %v", err)
		}
		defer func() { _ = h.Close() }()
		out := make([]bool, 0, 32)
		for i := 0; i < 32; i++ {
			// BOTH arms create the file, because [SimFileHandle.Write] draws a
			// per-sector fault decision from the disk's Seed; only the renames
			// and the crash differ, which is what this test is about.
			name := fmt.Sprintf("dir/f%d", i)
			writeFileNoSync(t, d, name)
			if withRenames {
				// A rename between every Sync: under a model that drew the
				// crash choice from the disk's own Seed this alone would shift
				// every decision below it.
				if err := d.Rename(name, name+".moved"); err != nil {
					t.Fatalf("Rename: %v", err)
				}
			}
			out = append(out, h.Sync() != nil)
		}
		if withRenames {
			d.Crash() // also exercise the crash draw itself
		}
		return out
	}
	plain, renamed := syncOutcomes(false), syncOutcomes(true)
	if len(plain) != len(renamed) {
		t.Fatalf("length mismatch: %d vs %d", len(plain), len(renamed))
	}
	for i := range plain {
		if plain[i] != renamed[i] {
			t.Fatalf("Sync fault stream diverged at draw %d (%t vs %t): the rename crash model is drawing from the disk's fault Seed",
				i, plain[i], renamed[i])
		}
	}
	// Non-vacuity: the stream must actually contain both outcomes, or the
	// comparison above would hold on any implementation.
	faults := 0
	for _, f := range plain {
		if f {
			faults++
		}
	}
	if faults == 0 || faults == len(plain) {
		t.Fatalf("the sampled Sync fault stream is constant (%d faults of %d): it cannot witness a perturbation",
			faults, len(plain))
	}
	t.Logf("fault stream identical across %d draws (%d faults)", len(plain), faults)
}

// writeFileNoSync creates a file with a single write and no Sync, so it consumes
// only the per-sector write draws. The helper exists because [writeFile] Syncs,
// which would make the two arms of the perturbation test consume different
// numbers of Sync draws for reasons unrelated to renames.
func writeFileNoSync(t *testing.T, d *SimDisk, path string) {
	t.Helper()
	h, err := d.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
	if err != nil {
		t.Fatalf("OpenFile %s: %v", path, err)
	}
	if _, err := h.Write([]byte("x")); err != nil {
		t.Fatalf("Write %s: %v", path, err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close %s: %v", path, err)
	}
}

// TestSimDiskRenameCrash_RevokeBothArmReproducesTheDefect is the gate on the
// gate: it proves the verdict gate above can actually fail, by arming the
// explicitly-illegal outcome and showing the harness produces exactly the state
// #2514 removed. Without this, a verdict gate that passed because the model no
// longer reaches ANY interesting state would be indistinguishable from one that
// passed because the model is correct.
func TestSimDiskRenameCrash_RevokeBothArmReproducesTheDefect(t *testing.T) {
	d := NewSimDisk(NewSeed(0x2514_DEAD), 0)
	writeFile(t, d, "db/snapshot/manifest.json", []byte("old"))
	if err := d.DirSync("db/snapshot"); err != nil {
		t.Fatalf("DirSync live: %v", err)
	}
	if err := d.ParentDirSync("db/snapshot"); err != nil {
		t.Fatalf("ParentDirSync live: %v", err)
	}
	writeFile(t, d, "db/snapshot.tmp/manifest.json", []byte("new"))
	if err := d.DirSync("db/snapshot.tmp"); err != nil {
		t.Fatalf("DirSync staging: %v", err)
	}

	// Reproduce the pre-#2514 default on BOTH renames of the pair.
	d.ArmRenameRevokeBothForPath("db/snapshot.bak")
	if err := d.Rename("db/snapshot", "db/snapshot.bak"); err != nil {
		t.Fatalf("archive rename: %v", err)
	}
	d.ArmRenameRevokeBothForPath("db/snapshot")
	if err := d.Rename("db/snapshot.tmp", "db/snapshot"); err != nil {
		t.Fatalf("publish rename: %v", err)
	}
	if got := d.RenameRevokeBothCount(); got != 2 {
		t.Fatalf("RenameRevokeBothCount = %d, want 2 — the arms never matched", got)
	}
	d.Crash()

	got := publishPairOutcome{
		live:    d.Exists("db/snapshot/manifest.json"),
		bak:     d.Exists("db/snapshot.bak/manifest.json"),
		staging: d.Exists("db/snapshot.tmp/manifest.json"),
	}
	// The armed fault must produce the impossible outcome — no snapshot at all.
	if got.live || got.bak {
		t.Fatalf("the revoke-both arm left %s; it must reproduce the total-loss outcome so the verdict gate has something to reject", got)
	}
	// And that outcome must be outside the legal set the verdict gate accepts,
	// which is what makes that gate falsifiable.
	for _, legal := range legalPublishPairOutcomes() {
		if got == legal {
			t.Fatalf("the reproduced defect state %s is inside the legal set: the verdict gate cannot fail", got)
		}
	}
	t.Logf("armed revoke-both reproduced the pre-fix state: %s", got)
}
