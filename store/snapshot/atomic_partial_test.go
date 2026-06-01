package snapshot

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// buildFullSnapshot writes a v3 snapshot for a small string-keyed
// graph and returns the snapshot directory path. Used by the partial-
// snapshot and CRC-corruption tests below.
func buildFullSnapshot(t *testing.T) string {
	t.Helper()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	adj := g.AdjList()
	pairs := [][2]string{{"a", "b"}, {"b", "c"}, {"c", "a"}}
	for _, p := range pairs {
		if err := adj.AddEdge(p[0], p[1], 1); err != nil {
			t.Fatalf("AddEdge(%s->%s): %v", p[0], p[1], err)
		}
	}
	if err := g.SetNodeLabel("a", "Node"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	if err := g.SetNodeProperty("b", "v", lpg.Int64Value(7)); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}
	snapDir := t.TempDir()
	c := csr.BuildFromAdjList(adj)
	if err := WriteSnapshotFull(snapDir, c, g); err != nil {
		t.Fatalf("WriteSnapshotFull: %v", err)
	}
	return snapDir
}

// copySnapshotDir duplicates every regular file in src into a fresh
// TempDir (shallow copy, no sub-directories) and returns the path.
func copySnapshotDir(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", src, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(src, e.Name())) //nolint:gosec // testdata under TempDir
		if err != nil {
			t.Fatalf("ReadFile %s: %v", e.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), data, 0o600); err != nil { //nolint:gosec // dst is from t.TempDir
			t.Fatalf("WriteFile %s: %v", e.Name(), err)
		}
	}
	return dst
}

// TestSnapshot_AtomicPartial asserts that [LoadSnapshotFull] returns a
// typed error when any required segment file is absent from the
// snapshot directory — simulating a crash that interrupted the
// write phase before the atomic rename completed.
//
// The test also verifies that CRC corruption of an existing segment
// file surfaces as [ErrCorrupted], confirming that the integrity check
// is applied after successful file open.
func TestSnapshot_AtomicPartial(t *testing.T) {
	t.Parallel()

	// Verify the baseline is loadable before exercising the fault paths.
	goodDir := buildFullSnapshot(t)
	if _, err := LoadSnapshotFull(goodDir); err != nil {
		t.Fatalf("LoadSnapshotFull(good): %v", err)
	}

	// T1: remove labels.bin — LoadSnapshotFull must error (manifest
	// references the file but it is absent).
	t.Run("missing_labels_bin", func(t *testing.T) {
		t.Parallel()
		dir := copySnapshotDir(t, goodDir)
		labPath := filepath.Join(dir, LabelsFile)
		if err := os.Remove(labPath); err != nil {
			t.Fatalf("Remove(%s): %v", LabelsFile, err)
		}
		_, err := LoadSnapshotFull(dir)
		if err == nil {
			t.Fatal("LoadSnapshotFull with missing labels.bin: want error, got nil")
		}
	})

	// T2: remove properties.bin — same contract.
	t.Run("missing_properties_bin", func(t *testing.T) {
		t.Parallel()
		dir := copySnapshotDir(t, goodDir)
		propPath := filepath.Join(dir, PropertiesFile)
		if err := os.Remove(propPath); err != nil {
			t.Fatalf("Remove(%s): %v", PropertiesFile, err)
		}
		_, err := LoadSnapshotFull(dir)
		if err == nil {
			t.Fatal("LoadSnapshotFull with missing properties.bin: want error, got nil")
		}
	})

	// T3: flip one byte in labels.bin to break its CRC. The load must
	// fail with ErrCorrupted.
	t.Run("corrupted_labels_bin", func(t *testing.T) {
		t.Parallel()
		dir := copySnapshotDir(t, goodDir)
		labPath := filepath.Join(dir, LabelsFile)
		data, err := os.ReadFile(labPath) //nolint:gosec // path under t.TempDir
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", LabelsFile, err)
		}
		if len(data) == 0 {
			t.Skip("labels.bin is empty, cannot flip a byte")
		}
		data[len(data)/2] ^= 0xFF
		if err := os.WriteFile(labPath, data, 0o600); err != nil { //nolint:gosec // path under t.TempDir
			t.Fatalf("WriteFile(%s): %v", LabelsFile, err)
		}
		_, err = LoadSnapshotFull(dir)
		if !errors.Is(err, ErrCorrupted) {
			t.Fatalf("LoadSnapshotFull(corrupted labels.bin) = %v, want ErrCorrupted", err)
		}
	})

	// T4: flip one byte in properties.bin to break its CRC.
	t.Run("corrupted_properties_bin", func(t *testing.T) {
		t.Parallel()
		dir := copySnapshotDir(t, goodDir)
		propPath := filepath.Join(dir, PropertiesFile)
		data, err := os.ReadFile(propPath) //nolint:gosec // path under t.TempDir
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", PropertiesFile, err)
		}
		if len(data) == 0 {
			t.Skip("properties.bin is empty, cannot flip a byte")
		}
		data[len(data)/2] ^= 0xFF
		if err := os.WriteFile(propPath, data, 0o600); err != nil { //nolint:gosec // path under t.TempDir
			t.Fatalf("WriteFile(%s): %v", PropertiesFile, err)
		}
		_, err = LoadSnapshotFull(dir)
		if !errors.Is(err, ErrCorrupted) {
			t.Fatalf("LoadSnapshotFull(corrupted properties.bin) = %v, want ErrCorrupted", err)
		}
	})
}
