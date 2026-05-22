package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCLI_CrossProcessSnapshotConsistency builds the example binary,
// runs the full init → seed → snapshot → stats lifecycle as four
// separate processes, and asserts that `stats` produces byte-identical
// counts both before and after the snapshot+reopen. This is the
// regression closed by Sprint 56 T3: the previous maphash-based shard
// router used a per-process seed, so NodeIDs assigned in one process
// did not match the snapshot's labels.bin NodeIDs written by another,
// causing labels to drift onto the wrong nodes after recovery.
//
// The test is skipped when `go build` fails (e.g., on an offline
// sandbox without the module cache); it is otherwise deterministic.
func TestCLI_CrossProcessSnapshotConsistency(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-process test takes ~1s; skipped in short mode")
	}

	binDir := t.TempDir()
	binary := filepath.Join(binDir, "social_cli")

	// Build the example binary in an isolated temp dir so the test
	// cannot pick up a stale executable from the working tree.
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

	// Step 1 — init in its own process.
	if _, err := runStep("init", "-d", dataDir); err != nil {
		t.Fatalf("init: %v", err)
	}

	// Step 2 — seed in another process.
	if _, err := runStep("seed", "-d", dataDir); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Step 3 — stats post-seed in a third process. Baseline counts.
	preSnap, err := runStep("stats", "-d", dataDir)
	if err != nil {
		t.Fatalf("stats (pre-snapshot): %v", err)
	}

	// Step 4 — snapshot in a fourth process.
	if _, err := runStep("snapshot", "-d", dataDir); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	// Step 5 — stats post-snapshot in a fifth process. Must equal the
	// pre-snapshot counts; the historical drift was off-by-one on one
	// label category per snapshot cycle.
	postSnap, err := runStep("stats", "-d", dataDir)
	if err != nil {
		t.Fatalf("stats (post-snapshot): %v", err)
	}

	if preSnap != postSnap {
		t.Fatalf("snapshot/reopen drift:\n  pre:  %s\n  post: %s", preSnap, postSnap)
	}

	// Confirm the counts also match the documented fixture totals so a
	// future regression that silently zeros out the graph is caught.
	var counts map[string]any
	if err := json.Unmarshal([]byte(preSnap), &counts); err != nil {
		t.Fatalf("invalid JSON stats: %v\n%s", err, preSnap)
	}
	want := map[string]float64{
		"authored": 8, "comments": 5, "follows": 8, "likes": 7,
		"on": 5, "posts": 3, "replies": 2, "users": 5,
	}
	for k, wantV := range want {
		got, ok := counts[k].(float64)
		if !ok || got != wantV {
			t.Fatalf("counts[%q] = %v (%T), want %v", k, counts[k], counts[k], wantV)
		}
	}
}

// TestCLI_CrossProcessCypherCreateSurvives is the regression closed by the
// per-process __cx_<hex> counter-reset fix in cypher/exec/create_node.go. It
// drives N consecutive Cypher CREATE statements through N separate process
// launches of the example CLI against the same data directory, then opens a
// final process to MATCH every :User node and asserts that:
//
//  1. The post-MATCH set contains exactly the N usernames that were CREATEd
//     (no row was overwritten across process restarts).
//  2. All N resulting nodes carry distinct internal NodeIDs (no two CREATEs
//     re-interned the same synthetic __cx_<hex> key).
//
// Before the fix, the synthetic-key counter reset to zero in every new
// process, so every CREATE in cycles 2..N regenerated the key __cx_1, which
// resolved (via the persisted mapper) to the existing NodeID assigned in
// cycle 1, and the follow-up SetNodeProperty calls overwrote the previous
// row's username and display_name. The empirical signature was: after N
// cycles only ONE user survived, with NodeID equal to the cycle-1 NodeID.
func TestCLI_CrossProcessCypherCreateSurvives(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-process CREATE test takes ~1s; skipped in short mode")
	}

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

	// Step 1 — init the data directory (one process).
	if out, err := runStep("init", "-d", dataDir); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	// Step 2 — drive N successive CREATEs, each in its own process. The
	// usernames are chosen to be distinct from the seed fixture so the
	// regression cannot be masked by the seed's natural-key inserts.
	usernames := []string{"frank", "gina", "heidi", "ivan", "judy", "kim"}
	for _, u := range usernames {
		display := strings.ToUpper(u[:1]) + u[1:]
		query := "CREATE (u:User {username:'" + u + "', display_name:'" + display + "'})"
		if out, err := runStep("query", "-d", dataDir, query); err != nil {
			t.Fatalf("create %s: %v\n%s", u, err, out)
		}
	}

	// Step 3 — final process: MATCH all users, project username and
	// internal NodeID. RETURN of a bare variable currently emits the
	// NodeID as a scalar (not a full NodeValue), so we project the
	// fields we want directly.
	out, err := runStep("query", "-d", dataDir, "MATCH (u:User) RETURN u.username AS username, ID(u) AS id")
	if err != nil {
		t.Fatalf("match: %v\n%s", err, out)
	}

	// Parse the JSONL output. Each non-empty line is one record.
	type rec struct {
		Username string `json:"username"`
		ID       uint64 `json:"id"`
	}
	var records []rec
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var r rec
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		records = append(records, r)
	}

	// Build the set of surviving usernames and the set of NodeIDs they
	// occupy. Both must contain exactly len(usernames) distinct entries.
	gotUsernames := make(map[string]struct{}, len(records))
	gotNodeIDs := make(map[uint64]string, len(records))
	for _, r := range records {
		if _, dup := gotUsernames[r.Username]; dup {
			t.Fatalf("duplicate username %q across records", r.Username)
		}
		gotUsernames[r.Username] = struct{}{}
		if prev, dup := gotNodeIDs[r.ID]; dup {
			t.Fatalf("NodeID %d shared by users %q and %q (counter-reset regression)", r.ID, prev, r.Username)
		}
		gotNodeIDs[r.ID] = r.Username
	}

	for _, u := range usernames {
		if _, ok := gotUsernames[u]; !ok {
			t.Errorf("user %q missing from MATCH result (counter-reset regression overwrote it)", u)
		}
	}
	if len(gotNodeIDs) != len(usernames) {
		t.Errorf("got %d distinct NodeIDs for %d CREATEs (want %d)", len(gotNodeIDs), len(usernames), len(usernames))
	}
}
