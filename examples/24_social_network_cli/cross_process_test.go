package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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
