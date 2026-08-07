package wal_test

import (
	"bytes"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/internal/testfs"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// TestSyncGroup_FailAll is the gate for the group-commit fail-all durability
// branch (#1507): when the group leader's fsync fails, EVERY member of the
// group must fail its SyncGroup with the sync error — none may believe it
// committed, because the writer's poison discards the entire un-synced suffix
// (every member's frames and markers). A subsequent recovery must observe zero
// durable frames.
//
// ReturnEIOOnSync fails every fsync, so whichever group member becomes leader
// fails, and the rest observe the sticky poison.
func TestSyncGroup_FailAll(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	walPath := filepath.Join(dir, "group_fail.wal")
	ff, err := testfs.New(walPath, testfs.Faults{ReturnEIOOnSync: true})
	if err != nil {
		t.Fatalf("testfs.New: %v", err)
	}
	w, err := wal.OpenWith(ff)
	if err != nil {
		_ = ff.Close()
		t.Fatalf("wal.OpenWith: %v", err)
	}

	const members = 8
	// Append every member's frame up front so all frames are buffered before any
	// SyncGroup runs: they form one group covered by a single leader fsync.
	//
	// Each member keeps its OWN watermark, which is what SyncGroup takes: a
	// committer cannot ask the writer what its frames' end offset was after the
	// fact, because the accepted offset is shared and a poison rewinds it (rmp
	// #2322). AppendRun of a single frame is exactly one Append plus that offset.
	marks := make([]int64, members)
	for m := 0; m < members; m++ {
		frame := bytes.Repeat([]byte{byte(0xA0 + m)}, 16)
		mark, aerr := w.AppendRun(func(emit func([]byte) error) error { return emit(frame) })
		if aerr != nil {
			t.Fatalf("Append(%d): %v", m, aerr)
		}
		marks[m] = mark
	}

	var failures atomic.Int64
	var nilAcks atomic.Int64
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(members)
	for m := 0; m < members; m++ {
		go func(m int) {
			defer wg.Done()
			<-start
			if serr := w.SyncGroup(marks[m]); serr != nil {
				failures.Add(1)
				if !errors.Is(serr, testfs.ErrSyncFailed) {
					t.Errorf("SyncGroup error = %v; want ErrSyncFailed", serr)
				}
				// EVERY member — not just the leader — must receive the durability
				// CLASS, because fail-all is precisely the policy that makes an
				// innocent member fail (rmp #2306). A member that got only a bare I/O
				// error could not tell this from a conflict of its own, and the Bolt
				// boundary could not keep it out of the driver's retriable family.
				if !errors.Is(serr, wal.ErrDurabilityFailed) {
					t.Errorf("SyncGroup error = %v; not errors.Is(wal.ErrDurabilityFailed). "+
						"A group member cannot identify a durability fail-stop, so a managed "+
						"transaction would retry it against a poisoned writer.", serr)
				}
			} else {
				nilAcks.Add(1)
			}
		}(m)
	}
	close(start)
	wg.Wait()

	// Not a single member may have been acknowledged: the shared fsync failed.
	if got := nilAcks.Load(); got != 0 {
		t.Fatalf("%d SyncGroup calls returned nil; want 0 (the group fsync failed, no member may commit)", got)
	}
	if got := failures.Load(); got != members {
		t.Fatalf("%d of %d members failed; want all %d to fail", got, members, members)
	}

	// The writer is poisoned: a later append must keep failing.
	if aerr := w.Append([]byte{0xFF}); aerr == nil {
		t.Fatal("Append after poison = nil; want sticky error")
	}

	// Close releases the file (best-effort on its error path).
	_ = w.Close()

	// Recovery: the un-synced suffix was discarded; no frame is durable.
	r, err := wal.OpenReader(walPath)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer func() { _ = r.Close() }()
	n := 0
	for range r.Frames() {
		n++
	}
	if n != 0 {
		t.Fatalf("recovered %d frame(s); want 0 (failed group fsync acknowledges nothing)", n)
	}
}

// TestSyncGroup_Coalesces verifies the follower fast path: when several frames
// are buffered and several goroutines call SyncGroup, the number of real fsyncs
// (Stats().Syncs) is strictly fewer than the number of SyncGroup calls — one
// leader's fsync covers the followers whose watermark it spans. It also asserts
// every member is acknowledged (nil) and the frames are durable.
func TestSyncGroup_Coalesces(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	walPath := filepath.Join(dir, "group_coalesce.wal")
	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	defer func() { _ = w.Close() }()

	const members = 64
	marks := make([]int64, members)
	for m := 0; m < members; m++ {
		frame := bytes.Repeat([]byte{byte(m)}, 8)
		mark, aerr := w.AppendRun(func(emit func([]byte) error) error { return emit(frame) })
		if aerr != nil {
			t.Fatalf("Append(%d): %v", m, aerr)
		}
		marks[m] = mark
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(members)
	var acks atomic.Int64
	for m := 0; m < members; m++ {
		go func(m int) {
			defer wg.Done()
			<-start
			if serr := w.SyncGroup(marks[m]); serr != nil {
				t.Errorf("SyncGroup: %v", serr)
				return
			}
			acks.Add(1)
		}(m)
	}
	close(start)
	wg.Wait()

	if got := acks.Load(); got != members {
		t.Fatalf("acknowledged = %d; want %d", got, members)
	}
	// All members' frames were buffered before any SyncGroup ran, so a single
	// leader fsync can cover every watermark: fewer real fsyncs than calls. The
	// exact count is timing-dependent (1..members) but must be < members for
	// coalescing to have happened at all.
	if syncs := w.Stats().Syncs; syncs >= uint64(members) {
		t.Fatalf("Stats().Syncs = %d; want < %d (group commit must coalesce fsyncs)", syncs, members)
	}
}
