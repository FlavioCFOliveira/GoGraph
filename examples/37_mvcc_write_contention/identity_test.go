package main

// identity_test.go — the short-layer gate of phase 8, GG07: node identity under
// concurrent CREATE, rollback and store reopen (rmp #3015).
//
// Every workload runs in a child: this test binary re-executed with
// -test.run=^TestIdentityChild$ and the child's spec in the environment, so
// each child's process-wide node-key counter starts at zero.

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"testing"
)

const envIdentityChild = "EX37_IDENTITY_CHILD"

// identityKeys lists every check a complete run must have printed.
func identityKeys() []string {
	rows := []string{"GG07.reopen.R", "GG07.two_stores.A", "GG07.two_stores.B", "GG07.memory.M1", "GG07.memory.M2"}
	keys := make([]string, 0, len(rows)*len(idCheckNames))
	for _, row := range rows {
		for _, n := range idCheckNames {
			keys = append(keys, row+"."+n)
		}
	}
	return keys
}

func TestIdentity(t *testing.T) {
	ic := identityConfig{childCmd: func(ctx context.Context, spec string) *exec.Cmd {
		// os.Args[0] is this test binary; the arguments are fixed.
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestIdentityChild$", "-test.count=1") //nolint:gosec // G204: the test's own binary
		cmd.Env = append(os.Environ(), envIdentityChild+"="+spec)
		return cmd
	}}
	var buf bytes.Buffer
	out, err := phaseIdentity(context.Background(), &buf, &ic)
	if err != nil {
		t.Fatalf("identity: %v\n%s", err, buf.String())
	}
	for _, f := range out.failed() {
		t.Error(f)
	}
	for _, k := range identityKeys() {
		if !out.has(k) {
			t.Errorf("phase 8 never emitted %s", k)
		}
	}
	if t.Failed() || testing.Verbose() {
		t.Log("\n" + buf.String())
	}
}

// TestIdentityChild is the GG07 child. It does nothing unless the parent set
// the environment.
func TestIdentityChild(t *testing.T) {
	spec := os.Getenv(envIdentityChild)
	if spec == "" {
		return
	}
	if err := runIdentityChild(context.Background(), spec, os.Stdout); err != nil {
		t.Fatalf("identity child: %v", err)
	}
}
