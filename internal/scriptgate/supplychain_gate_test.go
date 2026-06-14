package scriptgate

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestSupplyChainPinDiscipline is a static, PR-cheap regression gate for the
// build/release supply chain, born of the [SEC-2026-06-14c] audit. It does not
// run a release; it asserts file content only, so it is deterministic and
// fast on every PR.
//
// It guards three properties:
//
//  1. Every third-party GitHub Action referenced in .github/workflows/ is
//     pinned to an immutable 40-hex commit SHA (CWE-829: download of code
//     without integrity check). A floating tag (@v6, @main) would let a moved
//     or poisoned upstream tag execute arbitrary code in CI.
//
//  2. The cyclonedx-gomod SBOM generator in the release path is pinned to an
//     exact version (CWE-494). A floating @latest would let a poisoned
//     upstream release forge the shipped SBOM.
//
//  3. The .goreleaser.yaml pin-maintenance comment does not point maintainers
//     at a .github/dependabot.yml that no longer exists. That file was removed
//     deliberately (commit 28d3c20); the pins are maintained MANUALLY. A stale
//     instruction is a supply-chain maintenance hazard ([SEC-2026-06-14c] #1495).
func TestSupplyChainPinDiscipline(t *testing.T) {
	// --- 1 + 2: action and tool pins across every workflow -------------
	workflowsDir := filepath.Join(repoRoot(t), ".github", "workflows")
	entries, err := os.ReadDir(workflowsDir)
	if err != nil {
		t.Fatalf("read workflows dir: %v", err)
	}

	// `uses: owner/repo@<ref>` — capture the ref. A local action
	// (`uses: ./...`) has no @ and is exempt.
	usesRe := regexp.MustCompile(`uses:\s+([^\s@./][^\s@]*)@([^\s#]+)`)
	// A pinned ref is a full 40-char lowercase hex SHA.
	shaRe := regexp.MustCompile(`^[0-9a-f]{40}$`)

	sawWorkflow := false
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml") {
			continue
		}
		sawWorkflow = true
		body := readRepoFile(t, filepath.Join(".github", "workflows", e.Name()))
		for _, m := range usesRe.FindAllStringSubmatch(body, -1) {
			action, ref := m[1], m[2]
			if !shaRe.MatchString(ref) {
				t.Errorf("%s: action %q is pinned to %q, not a 40-hex commit SHA "+
					"(CWE-829: a moved/poisoned tag could run arbitrary code in CI). "+
					"Pin to the commit SHA with a # version comment.",
					e.Name(), action, ref)
			}
		}
	}
	if !sawWorkflow {
		t.Fatal("no .github/workflows/*.yml files found; the pin gate scanned nothing")
	}

	// cyclonedx-gomod must be installed at an exact version, never @latest.
	releaseYML := readRepoFile(t, ".github/workflows/release.yml")
	if strings.Contains(releaseYML, "cyclonedx-gomod@latest") {
		t.Error("release.yml installs cyclonedx-gomod@latest; pin it to an exact " +
			"version (CWE-494: a poisoned upstream release could forge the SBOM)")
	}
	if !regexp.MustCompile(`cyclonedx-gomod@v\d+\.\d+\.\d+`).MatchString(releaseYML) {
		t.Error("release.yml does not pin cyclonedx-gomod to an exact vMAJOR.MINOR.PATCH version")
	}

	// --- 3: no stale dependabot.yml reference --------------------------
	goreleaser := readRepoFile(t, ".goreleaser.yaml")
	if strings.Contains(goreleaser, "dependabot.yml") {
		t.Error(".goreleaser.yaml references .github/dependabot.yml, but that config " +
			"was removed (commit 28d3c20); pins are maintained MANUALLY. Update the " +
			"comment so maintainers are not pointed at a non-existent automation " +
			"mechanism ([SEC-2026-06-14c] #1495).")
	}
}

// TestBoltSessionUsesCryptoRand guards that the Bolt server session layer —
// which mints connection identifiers — draws randomness from crypto/rand, not
// math/rand (CWE-338: predictable PRNG in a security context). Even though the
// current session id is only a log identifier, pinning the import keeps a
// future security-sensitive id (token/nonce) from silently regressing to a
// predictable source. Static import check; PR-cheap.
func TestBoltSessionUsesCryptoRand(t *testing.T) {
	session := readRepoFile(t, "bolt/server/session.go")
	if !strings.Contains(session, `"crypto/rand"`) {
		t.Error("bolt/server/session.go no longer imports crypto/rand; randomID and " +
			"any session identifier must use a CSPRNG (CWE-338)")
	}
	// Reject a regression to math/rand (with or without /v2) in this file.
	if regexp.MustCompile(`"math/rand(/v2)?"`).MatchString(session) {
		t.Error("bolt/server/session.go imports math/rand; the session layer is a " +
			"security context and must use crypto/rand (CWE-338)")
	}
}
