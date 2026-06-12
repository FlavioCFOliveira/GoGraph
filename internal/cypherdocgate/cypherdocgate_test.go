// Package cypherdocgate holds a documentation-freshness gate that guards
// docs/cypher.md — the canonical Cypher engine reference the README points to.
//
// The engine's explicit-transaction API (BeginTx / ExplicitTx) and its finite
// default result budgets (MaxResultRows / MaxResultBytes / MaxCollectItems)
// CHANGE observed behaviour: a previously-streaming query now stops with a typed
// bounded-resource error once it crosses a default cap. A user hitting that
// error and consulting the reference must find it documented. This test asserts
// that the reference mentions each behaviour-changing token, so the doc can
// never silently fall behind those features again.
//
// It is a test-only package; it ships no production code.
package cypherdocgate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// requiredTokens are the identifiers that docs/cypher.md must mention because
// each one names a behaviour-changing feature a user may consult the reference
// to understand.
var requiredTokens = []string{
	"BeginTx",
	"ExplicitTx",
	"MaxResultRows",
	"MaxResultBytes",
	"MaxCollectItems",
}

// repoRoot walks up from the test's working directory until it finds the
// directory containing go.mod, which is the repository root. It fails the test
// if no such directory is found before reaching the filesystem root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("repo root (dir containing go.mod) not found above working directory")
		}
		dir = parent
	}
}

// TestCypherDocMentionsTransactionAPIAndBudgets asserts that docs/cypher.md
// contains every required token. It fails before the documentation was updated
// (none of the tokens were present) and passes after.
func TestCypherDocMentionsTransactionAPIAndBudgets(t *testing.T) {
	t.Parallel()

	docPath := filepath.Join(repoRoot(t), "docs", "cypher.md")
	raw, err := os.ReadFile(docPath) //nolint:gosec // fixed in-repo doc path derived from go.mod location
	if err != nil {
		t.Fatalf("read %s: %v", docPath, err)
	}
	doc := string(raw)

	for _, token := range requiredTokens {
		if !strings.Contains(doc, token) {
			t.Errorf("docs/cypher.md does not mention %q; the engine reference must document it", token)
		}
	}
}
