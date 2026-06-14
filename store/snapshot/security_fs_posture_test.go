package snapshot

// Security test battery — snapshot filesystem posture and bounded-reference
// decoders.
//
// DEFENSE LOCK-INS (these pass today; they pin behaviour that must not
// regress):
//
//   - openSnapshotComponent opens every component file O_NOFOLLOW on the unix
//     build, so a component that is a symlink pointing outside the snapshot
//     directory is rejected rather than dereferenced. The existing
//     symlink_escape_test.go covers csr.bin and manifest.json; this file
//     extends the guard to the sibling components (labels.bin, properties.bin,
//     mapper.bin) that LoadSnapshotFull also opens through the same helper, and
//     exercises the helper directly.
//   - constraints.go / tombstones.go are the correctly-bounded reference
//     decoders: a hostile count at the implausibility ceiling is rejected
//     allocating nothing proportional to the count. The existing component
//     tests assert the rejection is typed; this file additionally pins that
//     the rejection happens with a bounded allocation (the contract the
//     finding-demo decoders in security_decoder_count_bound_test.go do NOT yet
//     satisfy).

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// secStoreWriteFullSnapshot builds a small string-keyed graph carrying node +
// edge labels and properties (so labels.bin, properties.bin and mapper.bin are
// all emitted) and writes a full snapshot to a fresh directory, returning that
// directory.
func secStoreWriteFullSnapshot(t *testing.T) string {
	t.Helper()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := g.AddEdge("b", "c", 2); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := g.SetNodeLabel("a", "Person"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	if err := g.SetNodeProperty("a", "name", lpg.StringValue("alice")); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "snap")
	c := csr.BuildFromAdjList(g.AdjList())
	if err := WriteSnapshotFull(dir, c, g); err != nil {
		t.Fatalf("WriteSnapshotFull: %v", err)
	}
	return dir
}

// secStoreReplaceWithSymlink removes the file at path and recreates it as a
// symlink pointing at target. The test is skipped when the platform cannot
// create symlinks (e.g. unprivileged Windows).
func secStoreReplaceWithSymlink(t *testing.T, path, target string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove %s: %v", path, err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlink unsupported on this platform: %v", err)
	}
}

// TestSec_Store_RejectSiblingComponentSymlinks replaces each sibling component
// file (labels.bin, properties.bin, mapper.bin) in turn with a symlink that
// points at a secret OUTSIDE the snapshot directory, and asserts
// LoadSnapshotFull rejects the snapshot rather than dereferencing the link.
// This extends the existing csr.bin / manifest.json O_NOFOLLOW coverage to
// every component the loader opens through openSnapshotComponent.
func TestSec_Store_RejectSiblingComponentSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("O_NOFOLLOW is a no-op on Windows; symlink escape is governed by separate OS controls")
	}
	t.Parallel()

	components := []string{LabelsFile, PropertiesFile, MapperFile}
	for _, comp := range components {
		comp := comp
		t.Run(comp, func(t *testing.T) {
			t.Parallel()
			dir := secStoreWriteFullSnapshot(t)

			// Confirm the component actually exists in this snapshot, so the
			// test does not silently pass by replacing a missing file.
			compPath := filepath.Join(dir, comp)
			if _, err := os.Lstat(compPath); err != nil {
				t.Fatalf("precondition: component %s not present in snapshot: %v", comp, err)
			}

			secret := filepath.Join(t.TempDir(), "secret.bin")
			if err := os.WriteFile(secret, []byte("OUTSIDE-SECRET-"+comp), 0o600); err != nil {
				t.Fatalf("WriteFile secret: %v", err)
			}
			secStoreReplaceWithSymlink(t, compPath, secret)

			if _, err := LoadSnapshotFull(dir); err == nil {
				t.Fatalf("LoadSnapshotFull on a symlinked %s = nil error, want rejection", comp)
			}
		})
	}
}

// TestSec_Store_OpenSnapshotComponentRejectsSymlink exercises
// openSnapshotComponent directly: a path that is a symlink must fail to open
// (ELOOP under O_NOFOLLOW) rather than following the link, even when the link
// target is a perfectly readable regular file. This pins the unix open posture
// independently of the full loader.
func TestSec_Store_OpenSnapshotComponentRejectsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("O_NOFOLLOW is a no-op on Windows; symlink escape is governed by separate OS controls")
	}
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "real.bin")
	if err := os.WriteFile(target, []byte("readable-regular-file"), 0o600); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	link := filepath.Join(dir, "link.bin")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported on this platform: %v", err)
	}

	f, err := openSnapshotComponent(link)
	if err == nil {
		_ = f.Close()
		t.Fatal("openSnapshotComponent followed a symlink; want it rejected under O_NOFOLLOW")
	}

	// Sanity: the same helper opens the real regular file the link pointed at.
	rf, err := openSnapshotComponent(target)
	if err != nil {
		t.Fatalf("openSnapshotComponent rejected a regular file: %v", err)
	}
	if cerr := rf.Close(); cerr != nil {
		t.Errorf("Close: %v", cerr)
	}
}

// TestSec_Store_ConstraintsHostileCountBoundedReject is a defense lock-in for
// the correctly-bounded reference decoder. A constraints.bin header declaring
// a count at the implausibility ceiling (constraintsMaxCount+1) with a
// truncated body is rejected with ErrConstraintsCorrupted while allocating
// nothing proportional to the count — the count is bounded BEFORE the make().
// This is the property the finding-demo decoders
// (security_decoder_count_bound_test.go) do NOT yet satisfy; pinning it here
// documents the target shape a fix to those decoders must reach.
//
// Not t.Parallel and skipped under -race: assertBoundedAlloc reads
// process-global runtime.MemStats, which is unreliable when other goroutines
// allocate concurrently and trips the race detector against shared runtime
// state. The error-classification half is already covered, race-safe, in
// constraints_test.go.
func TestSec_Store_ConstraintsHostileCountBoundedReject(t *testing.T) {
	if secStoreRaceEnabled {
		t.Skip("assertBoundedAlloc reads process-global MemStats; unreliable and race-flagged under -race")
	}
	// A header: magic + version + count, where count is one past the ceiling.
	// constraintsMaxCount is a uint32 (count is written as uint32).
	var b []byte
	b = binary.LittleEndian.AppendUint32(b, constraintsMagic)
	b = binary.LittleEndian.AppendUint32(b, constraintsFormatVersion)
	b = binary.LittleEndian.AppendUint32(b, constraintsMaxCount+1)

	assertBoundedAlloc(t, func() {
		_, err := ReadConstraints(bytes.NewReader(b))
		if !errors.Is(err, ErrConstraintsCorrupted) {
			t.Fatalf("ReadConstraints(hostile count) = %v, want ErrConstraintsCorrupted", err)
		}
	})
}

// TestSec_Store_TombstonesHostileCountBoundedReject is the tombstones.bin
// analogue: a count at tombstonesMaxCount+1 with a truncated body is rejected
// with ErrTombstonesCorrupted while allocating nothing proportional to the
// count. tombstones.go also clamps its eager slice reservation to
// tombstonesCapHintMax, so even a count just under the ceiling allocates only
// the clamp, never the full count — the model the finding-demo decoders should
// adopt.
//
// Same -race / parallel caveat as the constraints lock-in above.
func TestSec_Store_TombstonesHostileCountBoundedReject(t *testing.T) {
	if secStoreRaceEnabled {
		t.Skip("assertBoundedAlloc reads process-global MemStats; unreliable and race-flagged under -race")
	}
	var b []byte
	b = binary.LittleEndian.AppendUint32(b, tombstonesMagic)
	b = binary.LittleEndian.AppendUint32(b, tombstonesFormatVersion)
	b = binary.LittleEndian.AppendUint64(b, tombstonesMaxCount+1)

	assertBoundedAlloc(t, func() {
		_, err := ReadTombstones(bytes.NewReader(b))
		if !errors.Is(err, ErrTombstonesCorrupted) {
			t.Fatalf("ReadTombstones(hostile count) = %v, want ErrTombstonesCorrupted", err)
		}
	})
}
