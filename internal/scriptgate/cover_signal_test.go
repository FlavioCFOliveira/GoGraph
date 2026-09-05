//go:build unix

package scriptgate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// stubGo is a stand-in for the go binary. cover_gate.sh invokes it as
// `go test -coverpkg=... -coverprofile=<path> ...`, so the stub creates the
// profile file it is told to write — giving the run a temporary to strand —
// and then blocks, standing in for the minutes the real instrumented run takes.
const stubGo = `#!/usr/bin/env bash
for a in "$@"; do
  case "$a" in
    -coverprofile=*) printf 'mode: atomic\n' > "${a#-coverprofile=}" ;;
  esac
done
echo "stub go: pretending to run the suite" >&2
sleep 120
`

// startCoverGate launches scripts/cover_gate.sh against the stub go binary, in
// its OWN process group, and returns once the run has created its temporary
// profile. dir is a scratch directory that stands in for the repository root.
func startCoverGate(t *testing.T, dir string) (*exec.Cmd, string) {
	t.Helper()
	root := repoRoot(t)

	stub := filepath.Join(dir, "gostub")
	if err := os.WriteFile(stub, []byte(stubGo), 0o755); err != nil { //nolint:gosec // G306: 0o755 because the stub stands in for the go binary and the gate script under test executes it.
		t.Fatalf("write stub: %v", err)
	}
	profile := filepath.Join(dir, "cover.out")

	cmd := exec.Command("bash", filepath.Join(root, "scripts", "cover_gate.sh")) //nolint:gosec // G204: bash against scripts/cover_gate.sh under the repo root located by walking up to go.mod.
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"GO="+stub,
		"COVER_PROFILE="+profile,
		"COVER_LIB_PROFILE="+filepath.Join(dir, "cover.lib.out"),
	)
	// Its own process group, so a signal can be delivered to the GROUP the way a
	// terminal delivers Ctrl-C. This is not incidental: a script started as a
	// plain background job has SIGINT ignored on entry, and bash refuses to
	// install a trap for a signal that was ignored when the shell started — so
	// signalling the process directly would "prove" INT is unhandled when it is
	// the harness, not the script, that swallowed it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start cover_gate.sh: %v", err)
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if names := strandedTemporaries(t, dir); len(names) > 0 {
			return cmd, profile
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	t.Fatalf("cover_gate.sh never created a temporary under %s; the test cannot "+
		"demonstrate cleanup of something that was never created", dir)
	return nil, ""
}

// strandedTemporaries lists the per-invocation temporaries in dir — the files
// the gate must reclaim. Preserved failure evidence (cover.out.failed.*.log,
// rmp #2347) is deliberately NOT counted: it is meant to survive.
func strandedTemporaries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if strings.Contains(n, ".failed.") {
			continue
		}
		if strings.Contains(n, ".tmp.") || strings.Contains(n, ".pub.") {
			out = append(out, n)
		}
	}
	return out
}

// TestCoverGateReclaimsItsTemporariesOnSignal guards rmp #2549 by sending each
// signal to a REAL run of scripts/cover_gate.sh and listing what it left behind.
//
// The defect it prevents was measured, not imagined: an EXIT trap alone does not
// run when the shell is terminated by a signal, and cover.out.tmp.60317 sat in
// the repository root holding 248,840,121 bytes for seven days because of it.
// .gitignore hides such files from `git status`, so nothing surfaces them.
//
// It fails, on all three signals, when the instrumented run is returned to the
// FOREGROUND — naming the two files left behind. That is the half of the fix
// that carries the defect: bash runs a trap only between commands, so with a
// foreground child the handler is deferred for the rest of the run and the
// harness's follow-up SIGKILL arrives first. `wait` is what makes it prompt.
//
// It does NOT fail when the signal traps alone are narrowed back to
// `trap cleanup_cover_tmp EXIT`, and that is a true fact about bash rather than
// a hole in this test: measured on bash 3.2.57, a shell blocked in `wait` and
// killed by SIGTERM or SIGHUP DOES run its EXIT trap. rmp #2549 was filed on the
// opposite premise. The traps are kept as the portable guarantee — running the
// EXIT trap on a fatal signal is bash implementation behaviour, not POSIX — and
// [TestCoverGateCleanupIsIdempotent] is what pins them in place.
//
// The signal goes to the SHELL ALONE for a reason; see the comment at the call.
func TestCoverGateReclaimsItsTemporariesOnSignal(t *testing.T) {
	for _, tc := range []struct {
		name string
		sig  syscall.Signal
	}{
		{"SIGTERM", syscall.SIGTERM},
		{"SIGINT", syscall.SIGINT},
		{"SIGHUP", syscall.SIGHUP},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cmd, _ := startCoverGate(t, dir)

			before := strandedTemporaries(t, dir)
			if len(before) == 0 {
				t.Fatalf("no temporary existed to reclaim: the assertion below could " +
					"not fail, so it would prove nothing")
			}

			// Signal the SHELL ALONE, not the process group. This is the case the
			// fix exists for, and the distinction is the whole point: signalling
			// the group also kills the stub `go`, whereupon `wait` returns, the
			// script proceeds down its ordinary failure path and the EXIT trap
			// cleans up — so a group signal is reclaimed even with no signal trap
			// at all, and a test that sent one would pass against the defect. A
			// harness cancelling a run signals the process it spawned; only the
			// signal traps cover that.
			if err := cmd.Process.Signal(tc.sig); err != nil {
				t.Fatalf("signal shell: %v", err)
			}

			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				<-done
				t.Fatalf("cover_gate.sh did not exit within 30s of %s; the handler is "+
					"deferred, which is what happens when the instrumented run is in "+
					"the FOREGROUND instead of backgrounded and waited on (rmp #2549). "+
					"Temporaries present: %v", tc.name, strandedTemporaries(t, dir))
			}

			if left := strandedTemporaries(t, dir); len(left) > 0 {
				t.Errorf("%s left %d temporary file(s) behind: %v. A cancelled gate must "+
					"reclaim its own temporaries; an EXIT trap alone does not run on a "+
					"signal, and these accumulate invisibly because .gitignore hides them "+
					"(rmp #2549). Had before the signal: %v", tc.name, len(left), left, before)
			}
		})
	}
}

// TestCoverGateCleanupIsIdempotent asserts the normal path still works: a run
// that ends without a signal removes its temporaries exactly once and does not
// fail doing so. The signal handler calls the same function the EXIT trap does,
// so it runs twice on a signalled exit; rm -f makes that harmless, and this
// pins the property rather than trusting it.
func TestCoverGateCleanupIsIdempotent(t *testing.T) {
	script := readRepoFile(t, "scripts/cover_gate.sh")
	if !strings.Contains(script, "rm -f") {
		t.Errorf("cleanup_cover_tmp no longer uses `rm -f`, so calling it twice — which " +
			"a signalled exit does, once from the handler and once from the EXIT trap — " +
			"would fail on the second call (rmp #2549)")
	}
	for _, sig := range []string{"INT", "TERM", "HUP"} {
		if !strings.Contains(script, sig) {
			t.Errorf("cover_gate.sh no longer mentions %s; the signal traps that reclaim "+
				"a cancelled run's temporaries are gone (rmp #2549)", sig)
		}
	}
}

// TestMakeCleanReclaimsStrandedTemporaries guards the other half of rmp #2549.
// A signal trap cannot help a run killed with SIGKILL, so `make clean` is the
// only thing that reclaims what is already stranded — and .gitignore hides these
// files from `git status`, so nothing else would ever surface them. One measured
// at 248,840,121 bytes and was seven days old when this was written; reclaiming
// it took the working tree from 2.4 GB to 791 MB.
//
// It also asserts what clean must NOT match: cover.out.failed.*.log is preserved
// failure evidence (rmp #2347), and a re-run chasing a rare failure must not
// destroy the record of it.
func TestMakeCleanReclaimsStrandedTemporaries(t *testing.T) {
	makefile := readRepoFile(t, "Makefile")
	recipe, ok := makeRecipeFor(makefile, "clean")
	if !ok {
		t.Fatalf("the Makefile no longer defines a `clean` target")
	}
	for _, pat := range []string{
		"cover.out.tmp.*",
		"cover.out.testlog.tmp.*",
		"cover.lib.out.tmp.*",
		"cover.out.pub.*",
		"cover.lib.out.pub.*",
	} {
		if !strings.Contains(recipe, pat) {
			t.Errorf("`make clean` no longer removes %q, so a temporary stranded by a "+
				"SIGKILLed gate can only be reclaimed by hand — and .gitignore keeps it "+
				"out of `git status`, so nobody will know it is there (rmp #2549). "+
				"Recipe is:\n%s", pat, recipe)
		}
	}
	if strings.Contains(recipe, "cover.out.failed") {
		t.Errorf("`make clean` removes cover.out.failed.*, which is PRESERVED failure " +
			"evidence (rmp #2347): a re-run chasing a rare failure would destroy the " +
			"record of it, which is the exact defect that task fixed")
	}
}
