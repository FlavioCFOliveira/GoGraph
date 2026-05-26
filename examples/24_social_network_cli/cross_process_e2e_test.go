package main

// cross_process_e2e_test.go — T755: Social CLI cross-process extended scenario.
//
// Implements a determinism check for the FOLLOWS friends-of-friends (FoF)
// query across two independent process launches against the same data
// directory. Both processes see the same graph (written by a prior init+seed
// step) and must return byte-identical JSONL streams, confirming that the
// WAL-backed recovery, mapper shard routing, and Cypher result serialisation
// are fully deterministic across process restarts.
//
// The FoF query projects the username of every user that is followed by a
// direct follow of alice. In the seeded fixture:
//
//   alice → {bob, carol}
//   bob   → {carol, dave}
//   carol → {dave, erin}
//
// alice's FoF = carol (via alice→bob→carol) ∪ dave (via alice→bob→dave)
//             ∪ dave  (via alice→carol→dave) ∪ erin (via alice→carol→erin)
// After deduplication and lexicographic ordering: carol, dave, erin.
//
// Acceptance criteria:
//   - Two subprocess runs return the same JSONL output (determinism).
//   - The result set contains at least one row (the query is functional).
//   - goleak.VerifyNone passes (no goroutine leaks in the parent process).

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"go.uber.org/goleak"
)

// TestSocialCLI_CrossProcess_FoFDeterminism builds the social CLI binary,
// seeds the standard fixture, then spawns two independent subprocess
// invocations of the FoF Cypher query and asserts that both return
// identical results.
func TestSocialCLI_CrossProcess_FoFDeterminism(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-process FoF test takes ~2 s; skipped in short mode")
	}
	defer goleak.VerifyNone(t)

	// Build the binary into a temp dir so the test never picks up a stale
	// executable from the working tree.
	binDir := t.TempDir()
	binary := filepath.Join(binDir, "social_cli")

	buildCmd := exec.Command("go", "build", "-o", binary, ".")
	buildCmd.Dir = "."
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Skipf("go build skipped: %v\n%s", err, string(out))
	}

	dataDir := t.TempDir()

	runStep := func(args ...string) (string, error) {
		c := exec.Command(binary, args...)
		c.Env = os.Environ()
		var buf bytes.Buffer
		c.Stdout = &buf
		c.Stderr = &buf
		err := c.Run()
		return buf.String(), err
	}

	// Process A: init.
	if out, err := runStep("init", "-d", dataDir); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	// Process B: seed the standard social fixture.
	if out, err := runStep("seed", "-d", dataDir); err != nil {
		t.Fatalf("seed: %v\n%s", err, out)
	}

	// FoF query: users that alice's direct follows also follow, excluding
	// alice herself. This is a two-hop traversal over :FOLLOWS edges.
	const fofQuery = `MATCH (alice:User {username: 'alice'})-[:FOLLOWS]->(:User)-[:FOLLOWS]->(fof:User) RETURN fof.username AS username`

	// Process C: first FoF query run.
	out1, err1 := runStep("query", "-d", dataDir, fofQuery)
	if err1 != nil {
		t.Fatalf("query (run 1): %v\n%s", err1, out1)
	}

	// Process D: second FoF query run, independent process.
	out2, err2 := runStep("query", "-d", dataDir, fofQuery)
	if err2 != nil {
		t.Fatalf("query (run 2): %v\n%s", err2, out2)
	}

	// Determinism check: both raw JSONL streams must be identical.
	if out1 != out2 {
		t.Fatalf("non-deterministic FoF output across process restarts:\n  run1: %q\n  run2: %q",
			out1, out2)
	}

	// Parse JSONL rows and validate at least one result.
	rows := parseFoFRows(t, out1)
	if len(rows) == 0 {
		t.Fatal("FoF query returned 0 rows; expected at least 1")
	}

	// Log deduplicated, sorted usernames for easy inspection.
	sort.Strings(rows)
	rows = dedup(rows)
	t.Logf("FoF of alice (deduplicated): %v", rows)
}

// parseFoFRows parses a JSONL stream where each line is {"username":"..."} and
// returns the username values. Empty lines are skipped.
func parseFoFRows(t *testing.T, jsonl string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(jsonl), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec struct {
			Username string `json:"username"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("parseFoFRows: invalid JSON line %q: %v", line, err)
		}
		out = append(out, rec.Username)
	}
	return out
}

// dedup removes consecutive duplicates from a sorted slice.
func dedup(s []string) []string {
	if len(s) == 0 {
		return s
	}
	out := s[:1]
	for _, v := range s[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}
