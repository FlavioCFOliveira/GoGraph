package sim

import (
	"errors"
	"io/fs"
	"os"
	"testing"
)

// TestHandleDoesNotSurviveACrash pins rmp #2544: a handle held across a crash
// must FAIL, not silently swallow the bytes written through it.
//
// The defect was a false accusation aimed at the subject under test.
// [SimDisk.CrashHost] drops entries from the name maps, but a [SimFileHandle]
// holds its *simFile DIRECTLY, so a scenario that crashed while holding its own
// handle went on writing into an ORPHANED file. Write returned (n, nil) — a
// successful write, by its own report — and the bytes reached nothing. The
// symptom that reached the operator was data missing from the engine.
//
// Both crash kinds are driven, because a file descriptor does not survive its
// process: CrashProcess models SIGKILL, so the fd dies with the process even
// though the kernel (and any fsync already handed to it) lives on.
func TestHandleDoesNotSurviveACrash(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		crash func(d *SimDisk)
	}{
		{"CrashHost", func(d *SimDisk) { d.CrashHost() }},
		{"CrashProcess", func(d *SimDisk) { d.CrashProcess() }},
		{"Crash", func(d *SimDisk) { d.Crash() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := NewSimDisk(NewSeed(1), 0)
			h, err := d.OpenFile("wal.log", os.O_CREATE|os.O_RDWR)
			if err != nil {
				t.Fatalf("OpenFile: %v", err)
			}
			if _, err := h.Write([]byte("before")); err != nil {
				t.Fatalf("precondition: the write BEFORE the crash must succeed: %v", err)
			}
			if err := h.Sync(); err != nil {
				t.Fatalf("precondition: Sync: %v", err)
			}

			tc.crash(d)

			// THE assertion. Before rmp #2544 this returned (5, nil) and the bytes
			// went into an orphaned simFile no name reaches.
			n, err := h.Write([]byte("after"))
			if err == nil {
				t.Fatalf("Write through a handle held across %s returned (%d, nil): the bytes "+
					"were accepted and silently discarded, which presents as engine data loss "+
					"(rmp #2544)", tc.name, n)
			}
			if !errors.Is(err, ErrCrashedDisk) {
				t.Errorf("Write error = %v, want one satisfying errors.Is(err, ErrCrashedDisk) so "+
					"the author learns the handle is dead rather than inferring it", err)
			}
			// The sentinel wraps fs.ErrClosed so existing callers that treat a closed
			// handle as terminal keep working without knowing about the new error.
			if !errors.Is(err, fs.ErrClosed) {
				t.Errorf("Write error = %v, want it to WRAP fs.ErrClosed so callers that already "+
					"handle a closed handle are unaffected", err)
			}
			if n != 0 {
				t.Errorf("Write reported %d byte(s) written on a dead handle, want 0", n)
			}

			// Every other method that touches the file must agree; a handle that
			// refuses writes but still answers reads would be a half-dead fd no real
			// system produces.
			if _, err := h.Read(make([]byte, 4)); !errors.Is(err, ErrCrashedDisk) {
				t.Errorf("Read after %s = %v, want ErrCrashedDisk", tc.name, err)
			}
			if _, err := h.Stat(); !errors.Is(err, ErrCrashedDisk) {
				t.Errorf("Stat after %s = %v, want ErrCrashedDisk", tc.name, err)
			}
			if _, err := h.Seek(0, 0); !errors.Is(err, ErrCrashedDisk) {
				t.Errorf("Seek after %s = %v, want ErrCrashedDisk", tc.name, err)
			}
			if err := h.Truncate(0); !errors.Is(err, ErrCrashedDisk) {
				t.Errorf("Truncate after %s = %v, want ErrCrashedDisk", tc.name, err)
			}
			if err := h.Sync(); !errors.Is(err, ErrCrashedDisk) {
				t.Errorf("Sync after %s = %v, want ErrCrashedDisk", tc.name, err)
			}

			// Close stays a no-op: every caller closes with defer, and a cleanup
			// path must not turn one failure into two.
			if err := h.Close(); err != nil {
				t.Errorf("Close on a crashed handle = %v, want nil: closing is cleanup, "+
					"not an operation that can fail", err)
			}
		})
	}
}

// TestHandleOpenedAfterACrashIsLive is the other half of the contract, and the
// reason this is a generation stamp rather than a dead flag: the crash kills the
// handles that existed, not the disk. A scenario that reopens after recovery —
// which is what every crash-recovery scenario does — must get a working handle.
//
// Without this the test above could be satisfied by a disk that simply refuses
// everything after its first crash, which would break every recovery scenario
// while still passing.
func TestHandleOpenedAfterACrashIsLive(t *testing.T) {
	t.Parallel()
	d := NewSimDisk(NewSeed(1), 0)

	h, err := d.OpenFile("wal.log", os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := h.Write([]byte("durable")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := h.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	_ = h.Close()

	d.CrashHost()

	reopened, err := d.OpenFile("wal.log", os.O_RDWR)
	if err != nil {
		t.Fatalf("OpenFile after the crash: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	got := make([]byte, len("durable"))
	if _, err := reopened.Read(got); err != nil {
		t.Fatalf("Read through a handle opened AFTER the crash: %v", err)
	}
	if string(got) != "durable" {
		t.Errorf("read %q, want %q: the synced bytes must survive the crash", got, "durable")
	}
	if _, err := reopened.Write([]byte("more")); err != nil {
		t.Errorf("Write through a handle opened after the crash: %v: the crash kills the "+
			"handles that existed, not the disk", err)
	}
}
