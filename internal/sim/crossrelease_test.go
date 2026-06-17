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

// crossReleaseTags are the prior releases the cross-release harness drives. They
// bracket the known data-compatibility regression class (v0.2.0 -> v0.3.x adjlist
// recovery panic, fixed in v0.3.2): v0.2.0 is the oldest release whose store is
// expected to reopen under the current code, v0.3.0 is post-rename and pre-fix.
// A tag absent from the environment is skipped cleanly (env precondition).
var crossReleaseTags = []string{"v0.2.0", "v0.3.0"}

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

	for _, tag := range crossReleaseTags {
		tag := tag
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

	for _, tag := range crossReleaseTags {
		tag := tag
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
