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
// while bypassing a release gate.
//
// The two paths do NOT run one identical command. Since the per-push workflow
// de-duplication (bb3ad3a) the release gate is deliberately SPLIT, and each
// path owns a well-defined slice of it:
//
//   - Tag-push CI path (.github/workflows/release.yml): the tagged commit is
//     always on main, so ci.yml/tck.yml/crash.yml have already gated that SHA
//     (build/vet, `go test -race` on the short layer plus its timing budget,
//     the coverage gate, the full openCypher TCK execution, and the
//     crash-injection durability battery). The release job re-runs none of
//     them; it runs only the Phase-A release-accuracy gate (release-doc
//     consistency) and then goreleaser. That is safe ONLY because those
//     per-push gates exist and run on every push to main — so this test
//     asserts they do.
//   - Local path (`make release`): a developer cutting a release off-CI has no
//     per-push workflow behind them, so `make release` depends on
//     release-preflight, which folds in release-accuracy, the coverage gate,
//     the bench gate, and the full correctness gate (scripts/pre-release.sh:
//     vet + build + test -race + golangci-lint + TCK).
//
// The assertions are static (file content), not a live release run, so the
// gate is cheap and deterministic on every PR. If a future change drops a
// per-push gate the tag-push path relies on — or removes release-preflight
// from the local path — this test fails, flagging the reintroduced bypass
// (#1444).
func TestReleasePathsConverge(t *testing.T) {
	// 1. Tag-push CI path runs the Phase-A release-accuracy gate.
	releaseYML := readRepoFile(t, ".github/workflows/release.yml")
	if !strings.Contains(releaseYML, "make release-accuracy") {
		t.Errorf("release.yml no longer runs `make release-accuracy`; the tag-push " +
			"release path would publish without the release-accuracy gate (#1444)")
	}

	// 2. The tag-push path re-runs no correctness/coverage/TCK/crash gate; it
	//    trusts the per-push workflows to have gated the tagged SHA. Assert
	//    each such gate still exists AND triggers on push to main, so removing
	//    one cannot silently un-gate the release path.
	perPush := []struct {
		workflow string // repo-relative workflow file
		gate     string // substring proving the gate command runs
		desc     string // what the gate enforces
	}{
		{".github/workflows/ci.yml", "make test-short-timings", "build/vet + `go test -race` short layer + timing budget"},
		{".github/workflows/ci.yml", "make cover-gate", "coverage gate"},
		{".github/workflows/tck.yml", "TestTCKExecution", "full openCypher TCK execution"},
		{".github/workflows/crash.yml", "make test-crashinject", "crash-injection durability battery"},
	}
	for _, g := range perPush {
		yml := readRepoFile(t, g.workflow)
		if !strings.Contains(yml, "push:") || !strings.Contains(yml, "branches: [main]") {
			t.Errorf("%s no longer triggers on push to main; the tag-push release path "+
				"relies on it having gated the tagged SHA (%s) (#1444)", g.workflow, g.desc)
		}
		if !strings.Contains(yml, g.gate) {
			t.Errorf("%s no longer runs `%s` (%s); the tag-push release path would "+
				"publish a SHA that skipped this gate (#1444)", g.workflow, g.gate, g.desc)
		}
	}

	// 3. Local path (`make release`) runs the full canonical gate.
	makefile := readRepoFile(t, "Makefile")
	if !strings.Contains(makefile, "release: release-preflight") {
		t.Errorf("Makefile `release` target no longer depends on `release-preflight`; " +
			"the local release path would bypass the canonical gate")
	}
	if !strings.Contains(makefile, "scripts/pre-release.sh") {
		t.Errorf("release-preflight no longer invokes scripts/pre-release.sh; the " +
			"correctness gate (vet/build/test -race/golangci-lint/TCK) would be skipped")
	}
}
