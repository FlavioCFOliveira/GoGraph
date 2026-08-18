package sim

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

// priorRelease is one prior release the cross-release harness drives, together
// with what the harness REQUIRES of it. The expectations are declared here, not
// discovered at runtime, so a tag that stops meeting one fails instead of
// quietly downgrading (rmp #2531).
type priorRelease struct {
	// Tag is the git tag to build.
	Tag string
	// SnapshotFloorReason, when non-empty, declares that this tag CANNOT publish a
	// snapshot the current reader opens, and says why. It is asserted in the
	// negative: a tag declared a floor must genuinely produce no snapshot, so a
	// stale floor declaration on a tag that has since become capable fails rather
	// than silently costing coverage.
	//
	// # It is deliberately UNUSED right now — do not delete it, do not populate it
	//
	// No tag in the repository needs it. rmp #2531 swept all fourteen release tags
	// and every one publishes a snapshot the current reader opens, so nothing in the
	// tag history predates snapshot support and every entry below leaves this empty.
	//
	// It is kept for two reasons, and both matter more than the fact that it is
	// currently unexercised:
	//
	//   - A FUTURE tag could genuinely predate some future snapshot capability, and
	//     a declared, reasoned floor is the honest way to express that. Without this
	//     field the only way to express it would be to drop the tag from the list,
	//     which is the silent coverage loss this whole task existed to remove.
	//   - Asserting it in the negative makes the declaration self-policing. A floor
	//     claimed for a tag that can in fact publish FAILS, so the field cannot rot
	//     into a comfortable excuse.
	//
	// Two failure modes to refuse explicitly:
	//
	//   - Do NOT delete this as dead code. Its emptiness is the measured result that
	//     there is no floor, not evidence that the concept is unused.
	//   - Do NOT populate it to silence a failing tag. A tag that stops publishing
	//     after having published is a REGRESSION in the helper — almost certainly a
	//     too-young symbol in the staged checkpoint half — not a newly discovered
	//     limitation of that release. Fix the helper. A floor is only legitimate when
	//     the release genuinely lacks the capability, and the reason must say which
	//     capability and why.
	SnapshotFloorReason string
	// WALReplayGapExpected declares that this release's OWN WAL replay does not
	// reproduce its live counts — a PRIOR-release defect, not a current-code one.
	//
	// It is pinned rather than merely tolerated so the defect is documented in
	// executable form and cannot be rediscovered as a new finding. The expectation
	// is specific to the fixed reproducer below (seed 0xC0FFEE, 300 ops):
	// changing either means re-measuring these flags, because which ops land in
	// the stream decides whether the defect is reached at all.
	WALReplayGapExpected bool
}

// SnapshotCapable reports that this tag must publish a snapshot directory the
// current reader opens.
func (p priorRelease) SnapshotCapable() bool { return p.SnapshotFloorReason == "" }

// crossReleaseTags are the prior releases the cross-release harness drives. A tag
// absent from the environment is skipped cleanly (env precondition).
//
// # Why these four (rmp #2531)
//
// The list is a deliberate sample of the fourteen release tags, not all of them.
// A sweep of all fourteen showed v0.3.1..v0.10.0 behaving identically to v0.3.0 on
// every axis this harness measures, so including them buys repetition rather than
// coverage. Each tag kept here earns its place:
//
//   - v0.1.0 — the OLDEST artefact that exists. It is the true floor of what the
//     current code claims to read, and it carries the WAL-replay defect below.
//   - v0.2.0 — oldest release whose store the project guarantees reopens, and the
//     lower bound of the v0.2.0 -> v0.3.x adjlist recovery-panic regression class
//     (fixed in v0.3.2). Shares the WAL-replay defect with v0.1.0, so the two
//     together assert that defect as a BOUNDED RANGE rather than a single point.
//   - v0.3.0 — post-rename and pre-fix for that regression class, and the first
//     release whose own WAL replay round-trips.
//   - v0.11.0 — the CURRENT release, and so the one real users actually upgrade
//     FROM. It was absent for the harness's whole life, which meant the project
//     made a cross-release compatibility claim while never once opening a snapshot
//     written by the release it had most recently shipped.
//
// # Every tag here publishes a snapshot, and that is ASSERTED
//
// All four leave SnapshotFloorReason empty, so all four MUST publish a snapshot
// directory the current code opens. That became true when the helper's checkpoint
// half stopped naming RunCheckpoint — exported only from v0.6.0 — and switched to
// the Start/TriggerCtx/Stop trio, exported unchanged since v0.1.0. Before that,
// every genuine tag fell back to a WAL-only image and this harness proved nothing
// whatever about the snapshot format across releases. There is consequently no
// documented snapshot floor; see SnapshotFloorReason for why the field stays.
//
// # TWO ARMS, TWO DIFFERENT RECOVERY PATHS — NEITHER SUBSTITUTES FOR THE OTHER
//
// Read this before adding, removing, or "simplifying" an arm. Each tag below is
// driven through BOTH of the following, and the pair is not redundant:
//
//	TestCrossRelease_UpgradeFromPriorTags        → SNAPSHOT-load path (walOps == 0)
//	TestCrossRelease_WALOnlyImageIsSnapshotFree  → WAL-REPLAY path  (walOps  > 0)
//
// The trap is that the rmp #2531 fix LOOKS like pure gain. It is not: it is a
// trade that had to be paid for. Publishing a checkpoint truncates the WAL to the
// snapshot watermark, so recovery satisfies itself from the snapshot and replays
// nothing — which is exactly why SnapshotOnlyRecovery can demand walOps == 0. The
// same property means the fix MOVED the prior-release WAL-replay path out of
// coverage entirely. Had the WAL-only arm not been added alongside it, closing the
// snapshot gap would have SILENTLY DELETED the very prior-release defect this
// file documents (v0.1.0/v0.2.0 reading their own 32-node image back as 79 nodes):
// measured at 682 replayed WAL ops before the fix and 0 after it.
//
// So: a tag added here must be exercised by both arms, and neither arm may be
// dropped on the grounds that the other one "already covers that tag". They cover
// the same TAG through different CODE. The WAL-only arm additionally carries the
// snapshot oracle's negative control, because once every ordinary arm reports
// SnapshotOpened=true a flag true in every observed run is indistinguishable from
// one wired true.
var crossReleaseTags = []priorRelease{
	// WALReplayGapExpected values are measured at the fixed reproducer below
	// (seed 0xC0FFEE, 300 ops), on the WAL-only arm — the only arm that can see the
	// defect at all, since a published snapshot masks it.
	{Tag: "v0.1.0", WALReplayGapExpected: true},
	{Tag: "v0.2.0", WALReplayGapExpected: true},
	{Tag: "v0.3.0", WALReplayGapExpected: false},
	{Tag: "v0.11.0", WALReplayGapExpected: false},
}

// repoRoot resolves the GoGraph working-tree root via git, or skips the test
// when git or a repository is unavailable. This is the environment-precondition
// gate: cross-release tests require a git checkout with the release tags, and an
// environment without them (a tarball build, a sandbox) skips cleanly rather
// than failing — the only sanctioned skip class for this harness.
func repoRoot(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("cross-release: git not on PATH (environment precondition)")
	}
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skipf("cross-release: not a git checkout (%v) — environment precondition", err)
	}
	return strings.TrimSpace(string(out))
}

// requireTagOrSkip skips when tag is not present in the repo, or when a prior
// build attempt already proved the toolchain cannot build it. It returns the
// repo root for the caller.
func requireTagOrSkip(t *testing.T, root, tag string) {
	t.Helper()
	if err := exec.Command("git", "-C", root, "rev-parse", "--verify", "--quiet", "refs/tags/"+tag).Run(); err != nil {
		t.Skipf("cross-release: tag %s not present (environment precondition)", tag)
	}
}

// TestCrossRelease_HelperBuildsAtHead is the FAST in-environment smoke that runs
// on the default short layer: it proves the prior-release helper source compiles
// and the protocol round-trips, WITHOUT building a prior tag (which is slow).
// It builds the helper from the CURRENT tree (HEAD), drives a tiny op stream
// through it, reopens the image with the current recovery, and asserts parity.
// This exercises the worktree-build + spawn + protocol path; the genuine
// cross-version builds live in the soak-lane tests below.
//
// Gated to the soak layer: even building the helper from HEAD spawns a
// subprocess worktree build, which is slow and CI-runner-fragile. The protocol
// and image-format paths it drives are also covered by the in-process
// differential and recovery tests in the short layer.
func TestCrossRelease_HelperBuildsAtHead(t *testing.T) {
	testlayers.RequireSoak(t)
	defer goleak.VerifyNone(t)
	root := repoRoot(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Build the helper from HEAD itself (treated as a "prior release" of itself):
	// this exercises the worktree-build + spawn + protocol path end-to-end at the
	// current code, fast and tag-independent.
	res, err := RunCrossReleaseUpgrade(ctx, root, "HEAD", 12345, 60)
	if err != nil {
		// HEAD must always be buildable; a failure here is a real harness bug,
		// not an environment precondition.
		t.Fatalf("cross-release HEAD upgrade smoke: %v", err)
	}
	if !res.Parity() {
		t.Fatalf("HEAD-as-prior upgrade did not reopen to parity:\n%s", res)
	}
	// HEAD reads its own WAL faithfully: live == self-recovery == current.
	if res.PriorWALFidelityGap {
		t.Fatalf("HEAD WAL did not round-trip in its own recovery (unexpected for current code):\n%s", res)
	}
	if res.RecoveredNodes != res.PriorSelfNodes {
		t.Fatalf("HEAD-as-prior node count drift current vs self: %s", res)
	}
	if res.RecoveredNodes == 0 {
		t.Fatalf("HEAD-as-prior wrote an empty image, nothing to verify: %s", res)
	}

	// The snapshot half (rmp #2477). HEAD is the one "prior release" whose
	// checkpoint API is the current one BY CONSTRUCTION, so every step of the
	// chain is a hard assertion here — this is where a silent regression to the
	// old WAL-only image would be caught.
	//
	// Since rmp #2531 the genuine prior tags assert the same chain rather than
	// merely logging it: the staged checkpoint half now names only symbols exported
	// since v0.1.0, so publishing is no longer a property of the tag that the
	// harness must accept whatever it turns out to be. The one fact that still
	// differs by design is the integrity trailer — HEAD's manifest MUST verify,
	// a prior tag's MUST NOT — which is what proves that flag discriminates by
	// artefact age.
	if !res.HelperCheckpointBuilt {
		t.Fatalf("HEAD helper did not build with its checkpoint half (fallback: %v):\n%s",
			res.HelperBuildFallbackErr, res)
	}
	if !res.CheckpointPublished {
		t.Fatalf("HEAD-as-prior did not publish a checkpoint (err %q):\n%s", res.CheckpointErr, res)
	}
	if !res.PriorSnapshot.Present {
		t.Fatalf("HEAD-as-prior published a checkpoint but left no snapshot manifest on disk:\n%s", res)
	}
	if !res.SnapshotOpened {
		t.Fatalf("current recovery did not open the HEAD-written snapshot directory:\n%s", res)
	}
	// The provenance gate: a full graph recovered with ZERO replayed WAL ops can
	// only have come through the snapshot bytes, so the snapshot path is proven
	// load-bearing rather than merely present. Without this, an image whose WAL
	// still held every op would pass every assertion above while the snapshot
	// contributed nothing.
	if !res.SnapshotOnlyRecovery {
		t.Fatalf("HEAD-as-prior recovery was not snapshot-only (walOps=%d nodes=%d): the WAL, "+
			"not the snapshot, could account for the recovered graph:\n%s",
			res.ReplayedWALOps, res.RecoveredNodes, res)
	}
	if res.PriorSnapshot.ManifestVersion == 0 {
		t.Fatalf("HEAD snapshot manifest read back version 0:\n%s", res)
	}
	// HEAD writes the rmp #2520 trailer, so its own manifest must verify. This is
	// the two-sided half of the golden-fixture assertion in
	// crossrelease_compat_test.go, which requires the OPPOSITE of a pre-#2520
	// artefact: together they prove IntegrityVerified discriminates.
	if !res.PriorSnapshot.IntegrityVerified {
		t.Fatalf("HEAD-written manifest did not verify its own integrity trailer (integrity=%q):\n%s",
			res.PriorSnapshot.Integrity, res)
	}
	t.Logf("HEAD-as-prior snapshot provenance: %s", res)
}

// TestCrossRelease_DifferentialHeadSmoke is the fast differential smoke: prior
// (HEAD) vs current in-process must agree on every op (they are the same code),
// proving the differential plumbing + classification works without a slow tag
// build.
//
// Gated to the soak layer: it spawns a subprocess worktree build of HEAD, which
// is slow and CI-runner-fragile. The differential plumbing and classification
// are covered in-process by TestDifferential_* in the short layer.
func TestCrossRelease_DifferentialHeadSmoke(t *testing.T) {
	testlayers.RequireSoak(t)
	defer goleak.VerifyNone(t)
	root := repoRoot(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	res, err := RunCrossReleaseDifferential(ctx, root, "HEAD", 777, 60)
	if err != nil {
		t.Fatalf("cross-release HEAD differential smoke: %v", err)
	}
	if !res.Agreed {
		t.Fatalf("HEAD-vs-current differential diverged unexpectedly:\n%s", res)
	}
	if !res.FinalCountsMatch {
		t.Fatalf("HEAD-vs-current end-state counts differ:\n%s", res)
	}
}

// TestCrossRelease_UpgradeFromPriorTags is the genuine cross-version upgrade
// test: a PRIOR release writes a store image, the CURRENT code reopens it, full
// oracle parity is asserted (or a clear data-compat fail-stop). It is slow (it
// builds a prior tag) so it runs only in the soak lane; each tag absent from the
// environment is skipped cleanly.
func TestCrossRelease_UpgradeFromPriorTags(t *testing.T) {
	testlayers.RequireSoak(t)
	defer goleak.VerifyNone(t)
	root := repoRoot(t)

	for _, rel := range crossReleaseTags {
		rel := rel
		tag := rel.Tag
		t.Run(tag, func(t *testing.T) {
			requireTagOrSkip(t, root, tag)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

			res, err := RunCrossReleaseUpgrade(ctx, root, tag, 0xC0FFEE, 300)
			if err != nil {
				// A build/worktree failure is an environment precondition (e.g. the
				// tag's tree does not build with this toolchain): skip, do not fail.
				t.Skipf("cross-release: cannot build prior tag %s in this environment: %v", tag, err)
			}

			if res.DataCompatError != nil {
				// The current code FAILED-STOP opening the prior image. That is the
				// SANCTIONED outcome (clear, non-silent) — but for v0.2.0/v0.3.0,
				// whose stores the project guarantees data-compat for (v0.3.2 fixed
				// the recovery panic), a fail-stop is itself a regression to surface.
				t.Fatalf("DATA-COMPAT FAIL-STOP reopening %s image with current code "+
					"(reproducer: seed=0xC0FFEE ops=300): %v\n%s", tag, res.DataCompatError, res)
			}
			// The cross-version contract: the current code recovers the prior image
			// IDENTICALLY to the prior release's own recovery. A prior-release WAL
			// fidelity gap (its WAL does not round-trip in its own release) is a
			// PRIOR defect — logged, not failed, because the current code's duty is
			// faithful re-read, not retroactive repair of a prior persistence bug.
			if !res.Parity() {
				t.Fatalf("CURRENT-CODE DATA-COMPAT REGRESSION reopening %s image "+
					"(current recovery diverges from the prior release's own recovery; "+
					"reproducer: seed=0xC0FFEE ops=300):\n%s", tag, res)
			}
			if res.PriorWALFidelityGap {
				t.Logf("cross-release upgrade %s -> current: current code FAITHFUL to prior self-recovery; "+
					"PRIOR-release WAL fidelity gap noted (not a current-code defect): %s", tag, res)
			} else {
				t.Logf("cross-release upgrade %s -> current: full PARITY (live==self==current): %s", tag, res)
			}

			// ── The snapshot half is now REQUIRED, not reported (rmp #2531) ────────
			//
			// It used to be logged, because whether a tag's checkpoint API matched the
			// staged helper half was treated as a property of the tag. It is not: it
			// is a property of which symbols the staged half names, and once that half
			// uses only the trio exported since v0.1.0, every tag here can publish.
			// Logging the WAL-only fallback therefore stopped describing a limitation
			// and started hiding a regression — a future edit that reintroduced a
			// too-young symbol would silently return this harness to proving nothing
			// about the snapshot format.
			//
			// The degradation path itself is untouched and still loud: a tag that
			// genuinely cannot build the checkpoint half still yields a WAL-only image
			// and still reports BuildFallbackErr. What changed is that a tag DECLARED
			// snapshot-capable may no longer take it silently.
			if !rel.SnapshotCapable() {
				// A declared floor is asserted in the negative, so the declaration
				// cannot outlive the limitation that justified it.
				if res.PriorSnapshot.Present {
					t.Fatalf("tag %s is declared a snapshot FLOOR (%q) but published a snapshot "+
						"directory anyway — the floor declaration is stale and is costing coverage:\n%s",
						tag, rel.SnapshotFloorReason, res)
				}
				t.Logf("cross-release upgrade %s: DOCUMENTED SNAPSHOT FLOOR — %s (image is WAL-only by declaration)",
					tag, rel.SnapshotFloorReason)
				return
			}
			if !res.HelperCheckpointBuilt {
				t.Fatalf("tag %s is declared snapshot-capable but its helper built WITHOUT the "+
					"checkpoint half, so the image is WAL-only and this tag contributes NO "+
					"snapshot-format coverage (build fallback: %v):\n%s",
					tag, res.HelperBuildFallbackErr, res)
			}
			if !res.CheckpointPublished {
				t.Fatalf("tag %s built its checkpoint half but did not publish (err %q):\n%s",
					tag, res.CheckpointErr, res)
			}
			if !res.PriorSnapshot.Present {
				t.Fatalf("tag %s published a checkpoint but left no snapshot manifest on disk:\n%s", tag, res)
			}
			// The load-bearing assertion of the whole task: a snapshot written by a
			// genuinely OLDER binary, opened by current code.
			if !res.SnapshotOpened {
				t.Fatalf("current recovery did not open the snapshot directory written by %s:\n%s", tag, res)
			}
			// Provenance: a non-empty graph recovered with ZERO replayed WAL ops can
			// only have come through the prior release's snapshot bytes, so the
			// snapshot is proven load-bearing rather than merely present alongside a
			// WAL that could account for everything.
			if !res.SnapshotOnlyRecovery {
				t.Fatalf("recovery of the %s image was not snapshot-only (walOps=%d nodes=%d): the WAL, "+
					"not the prior release's snapshot, could account for the recovered graph:\n%s",
					tag, res.ReplayedWALOps, res.RecoveredNodes, res)
			}
			if res.PriorSnapshot.ManifestVersion == 0 {
				t.Fatalf("snapshot written by %s read back manifest version 0:\n%s", tag, res)
			}
			// A prior release predates the rmp #2520 integrity trailer, so its
			// manifest must NOT verify. Asserted rather than tolerated: together with
			// the HEAD-as-prior arm, which requires the opposite, this proves
			// IntegrityVerified discriminates by artefact age instead of being
			// constant.
			if res.PriorSnapshot.IntegrityVerified {
				t.Fatalf("manifest written by %s verified an integrity trailer, but that tag "+
					"predates rmp #2520 which introduced it (integrity=%q):\n%s",
					tag, res.PriorSnapshot.Integrity, res)
			}
			// Non-vacuity: the snapshot must have carried real component data back
			// into the graph. Without this, an empty-but-well-formed snapshot would
			// satisfy every assertion above.
			if res.SnapshotLabels == 0 || res.SnapshotProperties == 0 {
				t.Fatalf("snapshot written by %s contributed no label/property records "+
					"(labels=%d props=%d), so its component files proved nothing:\n%s",
					tag, res.SnapshotLabels, res.SnapshotProperties, res)
			}
			t.Logf("cross-release SNAPSHOT-FORMAT coverage %s -> current: manifest v%d written by %s, "+
				"opened by current code, snapshot-only recovery (walOps=0), %d labels + %d properties from the image",
				tag, res.PriorSnapshot.ManifestVersion, tag, res.SnapshotLabels, res.SnapshotProperties)
		})
	}
}

// TestCrossRelease_DifferentialFromPriorTags is the genuine cross-version
// differential: the same op stream against a prior release and against current,
// with divergences classified. Benign (plan/version-dependent) divergences are
// recorded, not failed; any unexpected difference fails with a reproducer.
func TestCrossRelease_DifferentialFromPriorTags(t *testing.T) {
	testlayers.RequireSoak(t)
	defer goleak.VerifyNone(t)
	root := repoRoot(t)

	for _, rel := range crossReleaseTags {
		tag := rel.Tag
		t.Run(tag, func(t *testing.T) {
			requireTagOrSkip(t, root, tag)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

			res, err := RunCrossReleaseDifferential(ctx, root, tag, 0xBADF00D, 300)
			if err != nil {
				t.Skipf("cross-release: cannot build prior tag %s in this environment: %v", tag, err)
			}
			if !res.Agreed {
				t.Fatalf("UNEXPECTED cross-release divergence %s vs current "+
					"(reproducer: seed=0xBADF00D ops=300):\n%s", tag, res)
			}
			t.Logf("cross-release differential %s vs current: AGREED (%d benign divergences)\n%s",
				tag, len(res.Divergences), res)
		})
	}
}

// TestCrossRelease_WALOnlyImageIsSnapshotFree is the snapshot oracle's NEGATIVE
// control, and the last remaining exercise of a prior release's WAL-REPLAY path
// (rmp #2531).
//
// # Why a negative control is necessary
//
// Every tag in [crossReleaseTags] now publishes a snapshot, so every arm of
// TestCrossRelease_UpgradeFromPriorTags asserts SnapshotOpened is TRUE. A flag
// observed true in every run the suite performs is indistinguishable from a flag
// wired to true: the assertion would survive a reader that reported a snapshot
// hit unconditionally. This test drives the same pipeline over an image built
// with [XReleaseBuildOptions.ForceWALOnly] — provably without a snapshot — and
// requires the flag to read FALSE. Only the pair carries information.
//
// # Why it is also required coverage, not just a control
//
// Publishing a checkpoint truncates the WAL to the snapshot's watermark, so
// recovery satisfies itself from the snapshot and replays almost nothing:
// SnapshotOnlyRecovery holds precisely because walOps is 0. That means fixing the
// checkpoint half MOVED the prior-release WAL replay path out of coverage. It is
// not a hypothetical loss — v0.2.0 has a genuine WAL-replay defect whose
// signature (live 32/20 against self-recovery 79/23 at the fixed reproducer) is
// visible ONLY on a WAL-only image, and vanishes the moment a snapshot exists to
// answer recovery instead. Keeping this arm means the snapshot work added a path
// rather than swapping one blindness for another.
//
// The current-code contract asserted here is the same one the snapshot arm
// asserts: current recovery must reproduce the prior release's OWN reading of its
// own image, faithfully — including when that reading is the defective one. The
// current code's duty is faithful re-read, never retroactive repair.
func TestCrossRelease_WALOnlyImageIsSnapshotFree(t *testing.T) {
	testlayers.RequireSoak(t)
	defer goleak.VerifyNone(t)
	root := repoRoot(t)

	for _, rel := range crossReleaseTags {
		rel := rel
		tag := rel.Tag
		t.Run(tag, func(t *testing.T) {
			requireTagOrSkip(t, root, tag)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

			res, err := RunCrossReleaseUpgradeWithOptions(ctx, root, tag, 0xC0FFEE, 300,
				XReleaseBuildOptions{ForceWALOnly: true})
			if err != nil {
				t.Skipf("cross-release: cannot build prior tag %s in this environment: %v", tag, err)
			}

			// ForceWALOnly skipped a stage that would have succeeded, so nothing
			// failed and there must be no build error masquerading as an API drift.
			if res.HelperBuildFallbackErr != nil {
				t.Fatalf("forced WAL-only build at %s reported a build fallback error, "+
					"which would be indistinguishable from a genuine checkpoint-API drift: %v",
					tag, res.HelperBuildFallbackErr)
			}
			if res.HelperCheckpointBuilt || res.CheckpointPublished {
				t.Fatalf("forced WAL-only helper at %s still published a checkpoint "+
					"(built=%v published=%v): the control does not control anything:\n%s",
					tag, res.HelperCheckpointBuilt, res.CheckpointPublished, res)
			}
			// The two-sided oracle. Both halves must read false: the filesystem shows
			// no manifest, and recovery reports no snapshot hit.
			if res.PriorSnapshot.Present {
				t.Fatalf("forced WAL-only image at %s has a snapshot manifest on disk:\n%s", tag, res)
			}
			if res.SnapshotOpened {
				t.Fatalf("current recovery reported a snapshot hit on a WAL-ONLY %s image: "+
					"SnapshotOpened cannot discriminate, so its TRUE readings elsewhere prove nothing:\n%s",
					tag, res)
			}
			if res.SnapshotOnlyRecovery {
				t.Fatalf("forced WAL-only image at %s reported snapshot-only recovery:\n%s", tag, res)
			}
			// Non-vacuity: the WAL replay path must genuinely have run. A WAL-only
			// image that replayed nothing would satisfy every negative above while
			// exercising no more than the snapshot arms already do.
			if res.ReplayedWALOps == 0 {
				t.Fatalf("forced WAL-only image at %s replayed ZERO WAL ops, so this arm "+
					"exercised no WAL-replay path at all:\n%s", tag, res)
			}
			if res.RecoveredNodes == 0 {
				t.Fatalf("forced WAL-only image at %s recovered an empty graph:\n%s", tag, res)
			}
			// The current-code contract, on the path the snapshot arms no longer reach.
			if !res.Parity() {
				t.Fatalf("CURRENT-CODE DATA-COMPAT REGRESSION on the WAL-ONLY %s image "+
					"(reproducer: seed=0xC0FFEE ops=300 ForceWALOnly):\n%s", tag, res)
			}

			// The prior-release WAL-replay defect, pinned so it is documented in
			// executable form rather than rediscovered as a new finding on some later
			// audit. The expectation is tied to this exact reproducer; see
			// priorRelease.WALReplayGapExpected.
			switch {
			case rel.WALReplayGapExpected && !res.PriorWALFidelityGap:
				t.Fatalf("tag %s is documented as having a PRIOR-release WAL-replay fidelity gap, "+
					"but its own recovery now round-trips (live n=%d e=%d, self n=%d e=%d). Either the "+
					"reproducer no longer reaches the defect or the documentation is stale — re-measure "+
					"before relaxing this:\n%s",
					tag, res.PriorLiveNodes, res.PriorLiveEdges, res.PriorSelfNodes, res.PriorSelfEdges, res)
			case !rel.WALReplayGapExpected && res.PriorWALFidelityGap:
				t.Fatalf("tag %s developed an UNDOCUMENTED prior-release WAL-replay fidelity gap "+
					"(live n=%d e=%d, self n=%d e=%d):\n%s",
					tag, res.PriorLiveNodes, res.PriorLiveEdges, res.PriorSelfNodes, res.PriorSelfEdges, res)
			case rel.WALReplayGapExpected:
				t.Logf("cross-release WAL-only %s: oracle NEGATIVE reading confirmed "+
					"(snapshotOnDisk=%v snapshotOpened=%v snapshotOnly=%v walOps=%d). "+
					"DOCUMENTED PRIOR-RELEASE DEFECT reproduced — "+
					"that release's own WAL replay reads back n=%d e=%d from an image whose live state was "+
					"n=%d e=%d. Current code reproduces the prior reading FAITHFULLY (n=%d e=%d), which is "+
					"the cross-version contract; it is NOT a current-code defect and must not be filed as one. "+
					"walOps=%d",
					tag, res.PriorSnapshot.Present, res.SnapshotOpened, res.SnapshotOnlyRecovery, res.ReplayedWALOps,
					res.PriorSelfNodes, res.PriorSelfEdges, res.PriorLiveNodes, res.PriorLiveEdges,
					res.RecoveredNodes, res.RecoveredEdges, res.ReplayedWALOps)
			default:
				t.Logf("cross-release WAL-only %s: oracle NEGATIVE reading confirmed "+
					"(snapshotOnDisk=%v snapshotOpened=%v snapshotOnly=%v); prior release's own WAL replay "+
					"round-trips (live==self==current n=%d e=%d), walOps=%d",
					tag, res.PriorSnapshot.Present, res.SnapshotOpened, res.SnapshotOnlyRecovery,
					res.RecoveredNodes, res.RecoveredEdges, res.ReplayedWALOps)
			}
		})
	}
}
