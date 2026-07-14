package scriptgate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readRepoFile reads a repo-relative file or fails the test.
func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// TestReleasePathsConverge guards #1444: neither release path may publish
// while bypassing the release gate.
//
// GitHub Actions now runs ONLY the release workflow
// (.github/workflows/release.yml) — the per-push ci.yml/tck.yml/crash.yml
// workflows were removed. Correctness, coverage, TCK, and crash gating are no
// longer enforced by GitHub; they run LOCALLY via `make release-preflight`
// (and `make ci` before every push), which a developer MUST run before
// tagging. The two release paths are therefore:
//
//   - Tag-push path (.github/workflows/release.yml): runs only the Phase-A
//     release-accuracy gate (release-doc consistency) and then goreleaser. It
//     re-runs none of the heavy correctness gates; it relies on the developer
//     having run `make release-preflight` locally before pushing the tag.
//   - Local path (`make release`): depends on release-preflight, which folds
//     in release-accuracy, the coverage gate, the bench gate, and the full
//     correctness gate (scripts/pre-release.sh: vet + build + test -race +
//     golangci-lint + TCK).
//
// The assertions are static (file content), not a live release run, so the
// gate is cheap and deterministic. Because no per-push CI stands behind the
// tag-push path any more, the local `make release-preflight` gate is the sole
// line of defence — so this test asserts its completeness. If a future change
// drops release-accuracy from the tag-push path, or removes release-preflight
// or any correctness command from the local path, this test fails, flagging
// the reintroduced bypass (#1444).
func TestReleasePathsConverge(t *testing.T) {
	// 1. Tag-push path runs the Phase-A release-accuracy gate.
	releaseYML := readRepoFile(t, ".github/workflows/release.yml")
	if !strings.Contains(releaseYML, "make release-accuracy") {
		t.Errorf("release.yml no longer runs `make release-accuracy`; the tag-push " +
			"release path would publish without the release-accuracy gate (#1444)")
	}

	// 2. Local path (`make release`) is the sole correctness gate now that no
	//    per-push CI exists. It must depend on release-preflight, which folds
	//    in the coverage gate and the full correctness gate.
	makefile := readRepoFile(t, "Makefile")
	if !strings.Contains(makefile, "release: release-preflight") {
		t.Errorf("Makefile `release` target no longer depends on `release-preflight`; " +
			"the local release path would bypass the canonical gate")
	}
	if !strings.Contains(makefile, "cover-gate") {
		t.Errorf("release-preflight no longer invokes the coverage gate; the local " +
			"release path would publish without the coverage gate")
	}
	if !strings.Contains(makefile, "scripts/pre-release.sh") {
		t.Errorf("release-preflight no longer invokes scripts/pre-release.sh; the " +
			"correctness gate (vet/build/test -race/golangci-lint/TCK) would be skipped")
	}

	// 3. The correctness gate itself must still run every mandated check. With
	//    no per-push CI behind the release, scripts/pre-release.sh is the last
	//    line of defence, so assert each gate command is present.
	preRelease := readRepoFile(t, "scripts/pre-release.sh")
	for _, want := range []string{"go vet", "go build", "go test -race", "golangci-lint", "TestTCKReport"} {
		if !strings.Contains(preRelease, want) {
			t.Errorf("scripts/pre-release.sh no longer runs %q; the correctness gate would "+
				"skip it, and there is no per-push CI to catch the gap (#1444)", want)
		}
	}
}
