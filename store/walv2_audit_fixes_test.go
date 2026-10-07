package store_test

// walv2_audit_fixes_test.go — the storage-engine audit findings on WAL v2 step
// 2 that recovery decides: a torn legacy tail followed by segment frames, and
// dedicated tests for ErrLegacyNotSealed, ErrRedoPointNotFrameBoundary and
// ErrRedoPointMidTransaction, each over the minimal on-disk state.
//
// Layer: short.

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/internal/waltest"
	"github.com/FlavioCFOliveira/GoGraph/store"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// tornFramePrefix returns the first bytes of an encoded frame: a torn tail.
func tornFramePrefix(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if _, err := wal.Encode(&buf, wal.Frame{Version: wal.CurrentVersion, Payload: []byte("torn"), Pos: 1, StoreID: 1}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()[:20]
}

// TestWALv2_TornLegacyTailBeforeSegmentFramesIsRefused (audit D2): a legacy
// file that ends in a torn frame, followed by segment frames, does not end in
// its seal. Recovery refuses it with ErrLegacyNotSealed instead of stopping at
// the torn legacy tail and reporting a clean recovery without a single segment
// frame.
func TestWALv2_TornLegacyTailBeforeSegmentFramesIsRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		legacy func(t *testing.T, stub []byte) []byte
	}{
		{"seal_stub_then_torn_bytes", func(t *testing.T, stub []byte) []byte {
			return append(bytes.Clone(stub), tornFramePrefix(t)...)
		}},
		{"torn_bytes_only", func(t *testing.T, _ []byte) []byte { return tornFramePrefix(t) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := committedDir(t, 3)
			walPath := filepath.Join(dir, "wal")
			stub, err := os.ReadFile(walPath) //nolint:gosec // path under t.TempDir
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(walPath, tc.legacy(t, stub), 0o600); err != nil {
				t.Fatal(err)
			}
			requireRefused(t, dir, wal.ErrLegacyNotSealed)
		})
	}
}

// TestWALv2_LegacyNotSealedIsRefused pins ErrLegacyNotSealed: a valid control
// file with OR 0, a legacy file whose last frame is not a seal, segment 1 with
// valid frames, and no snapshot redo position.
func TestWALv2_LegacyNotSealedIsRefused(t *testing.T) {
	t.Parallel()
	dir := committedDir(t, 2)
	walPath := filepath.Join(dir, "wal")
	locs, err := waltest.LocateFrames(walPath)
	if err != nil || len(locs) == 0 {
		t.Fatalf("locate frames: %v (%d)", err, len(locs))
	}
	log, err := wal.OpenLog(walPath)
	if err != nil {
		t.Fatal(err)
	}
	if ctl, ok := log.Control(); !ok || ctl.OldestRetainedPos != 0 {
		t.Fatalf("precondition: control %+v (present %t) must record OR 0", ctl, ok)
	}
	if _, err := os.Stat(filepath.Join(dir, "snapshot", "manifest.json")); !os.IsNotExist(err) {
		t.Fatalf("precondition: no snapshot manifest may exist (%v)", err)
	}
	// One legacy (v1) frame carrying an op record, and no seal after it.
	var legacy bytes.Buffer
	if _, err := wal.Encode(&legacy, wal.Frame{Payload: locs[0].Frame.Payload}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(walPath, legacy.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	requireRefused(t, dir, wal.ErrLegacyNotSealed)
}

// redoDir builds a store with a checkpoint whose snapshot records a redo
// position, then one more committed transaction after it, and returns the
// directory, the snapshot's redo position, and the frames of that last
// transaction (its ops, then its commit marker).
func redoDir(t *testing.T) (dir string, redo uint64, last []waltest.FrameLoc) {
	t.Helper()
	dir = t.TempDir()
	o, err := store.Open[string, float64](dir, openOptions())
	if err != nil {
		t.Fatal(err)
	}
	commitNodes(t, o.Store(), "a", "b")
	var mu sync.Mutex
	cp := checkpoint.New(checkpoint.Config{Dir: dir}, o.Graph(), o.WAL(), &mu,
		checkpoint.WithCommitSerialiser[string, float64](o.Store().RunUnderCommitLock),
		checkpoint.WithMapperCodec[string, float64](txn.NewStringCodec()),
		checkpoint.WithWeightCodec[string, float64](txn.NewFloat64WeightCodec()))
	if err := cp.RunCheckpoint(); err != nil {
		t.Fatal(err)
	}
	commitNodes(t, o.Store(), "z")
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	m, err := snapshot.ReadManifestFile(filepath.Join(dir, "snapshot", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if m.WALFormat != 2 {
		t.Fatalf("precondition: manifest wal_format %d, want 2", m.WALFormat)
	}
	locs, err := waltest.LocateFrames(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range locs {
		if l.Frame.Pos >= m.WALRedoPos {
			last = append(last, l)
		}
	}
	if len(last) < 2 {
		t.Fatalf("precondition: %d frames at or above the redo position %d, want ops and a marker", len(last), m.WALRedoPos)
	}
	if res, err := recoverDir(dir); err != nil || !res.IsClean() {
		t.Fatalf("control: the untampered directory must recover clean: %v", err)
	}
	return dir, m.WALRedoPos, last
}

// setRedo rewrites the snapshot manifest of dir with redo position r.
func setRedo(t *testing.T, dir string, r uint64) {
	t.Helper()
	p := filepath.Join(dir, "snapshot", "manifest.json")
	m, err := snapshot.ReadManifestFile(p)
	if err != nil {
		t.Fatal(err)
	}
	m.WALRedoPos = r
	var buf bytes.Buffer
	if err := snapshot.WriteManifest(&buf, m); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestWALv2_RedoPointNotFrameBoundaryIsRefused pins
// ErrRedoPointNotFrameBoundary: a v4 manifest whose redo position R lies
// inside a frame, with OR <= R < E.
func TestWALv2_RedoPointNotFrameBoundaryIsRefused(t *testing.T) {
	t.Parallel()
	dir, _, last := redoDir(t)
	setRedo(t, dir, last[0].Frame.Pos+1)
	requireRefused(t, dir, recovery.ErrRedoPointNotFrameBoundary)
}

// TestWALv2_RedoPointMidTransactionIsRefused pins ErrRedoPointMidTransaction:
// frames [op seq=n][OpCommit seq=n] with the manifest's redo position R at the
// commit marker, so the transaction has ops below R and its marker at R.
func TestWALv2_RedoPointMidTransactionIsRefused(t *testing.T) {
	t.Parallel()
	dir, _, last := redoDir(t)
	setRedo(t, dir, last[len(last)-1].Frame.Pos)
	requireRefused(t, dir, recovery.ErrRedoPointMidTransaction)
}
