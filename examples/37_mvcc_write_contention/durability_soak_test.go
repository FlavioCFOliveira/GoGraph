//go:build soak

package main

// durability_soak_test.go — the soak layer of phase 7 (rmp #2935): D02, kill -9 of
// a child process committing with 32 concurrent writers and a checkpointer
// triggered back to back, over five runs.
//
// The child is this test binary re-executed with -test.run=^TestDurabilityChild$
// and the store directory in the environment.
//
// Run: go test -tags soak -run TestDurabilityKill9 ./examples/37_mvcc_write_contention/

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"testing"
)

const (
	envChildDir   = "EX37_DURABILITY_CHILD_DIR"
	envChildLevel = "EX37_DURABILITY_CHILD_LEVEL"
)

func TestDurabilityKill9(t *testing.T) {
	dc := durabilityConfig{killRuns: 5, killLevel: 32, seed: 1,
		childCmd: func(ctx context.Context, dir string, level int) *exec.Cmd {
			// os.Args[0] is this test binary; the arguments are fixed.
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDurabilityChild$", "-test.count=1") //nolint:gosec // G204: the test's own binary
			cmd.Env = append(os.Environ(), envChildDir+"="+dir, envChildLevel+"="+strconv.Itoa(level))
			return cmd
		}}
	keys := make([]string, 0, 50)
	for _, r := range []string{"D02.run0", "D02.run1", "D02.run2", "D02.run3", "D02.run4"} {
		for _, k := range []string{"acked_present", "refused_absent", "whole_or_absent", "counters_conserved",
			"open_absent", "clock_not_rewound", "no_hole", "new_session_sees_acked", "seek_equals_scan",
			"post_recovery_commit_is_new"} {
			keys = append(keys, r+"."+k)
		}
	}
	runDurability(t, &dc, keys)
}

// TestDurabilityChild is the kill child. It does nothing unless the parent set
// the environment; when it did, it commits until it is killed.
func TestDurabilityChild(t *testing.T) {
	dir := os.Getenv(envChildDir)
	if dir == "" {
		return
	}
	level, err := strconv.Atoi(os.Getenv(envChildLevel))
	if err != nil {
		t.Fatalf("child level: %v", err)
	}
	if err := runDurabilityChild(context.Background(), dir, level, os.Stdout); err != nil {
		t.Fatalf("durability child: %v", err)
	}
}
