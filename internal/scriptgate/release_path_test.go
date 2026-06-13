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

// TestReleasePathsConverge guards #1444: the tag-push release path
// (.github/workflows/release.yml) and the local path (`make release`) must
// run the SAME canonical gate — `make release-preflight` — so neither can
// publish while bypassing a release gate. Before #1444 release.yml ran only
// scripts/pre-release.sh, silently skipping every release-accuracy and soak
// gate that lives in release-preflight.
//
// The assertions are static (file content), not a live release run, so the
// gate is cheap and deterministic on every PR.
func TestReleasePathsConverge(t *testing.T) {
	releaseYML := readRepoFile(t, ".github/workflows/release.yml")
	makefile := readRepoFile(t, "Makefile")

	// 1. The CI release workflow must invoke the canonical preflight target.
	if !strings.Contains(releaseYML, "make release-preflight") {
		t.Errorf("release.yml does not run `make release-preflight`; the tag-push " +
			"release path would bypass the release-accuracy and soak gates (#1444)")
	}

	// 2. The local `make release` target must depend on release-preflight.
	if !strings.Contains(makefile, "release: release-preflight") {
		t.Errorf("Makefile `release` target no longer depends on `release-preflight`; " +
			"the local release path would bypass the canonical gate")
	}

	// 3. release-preflight must fold in the correctness gate (pre-release.sh:
	//    vet + build + test -race + lint + TCK) so the single canonical gate is
	//    complete on BOTH paths.
	if !strings.Contains(makefile, "scripts/pre-release.sh") {
		t.Errorf("release-preflight no longer invokes scripts/pre-release.sh; the " +
			"correctness gate (vet/build/test -race/lint/TCK) would be skipped")
	}

	// 4. release-preflight must keep enforcing the mandatory soak gate (#1399).
	if !strings.Contains(makefile, "release_soak_gate.sh") {
		t.Errorf("release-preflight no longer invokes scripts/release_soak_gate.sh; " +
			"the mandatory pre-release soak gate would be skipped")
	}
}
