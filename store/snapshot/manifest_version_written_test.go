package snapshot

import (
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// lastPreMultigraphManifestVersion is the highest manifest version a build
// before rmp #3072 accepts. Such a build reads an absent "directed" or
// "multigraph" key as an undirected simple graph, so it must refuse every
// manifest this build writes; it does so only for a version above this one.
const lastPreMultigraphManifestVersion = 4

// TestWrittenManifest_AlwaysCurrentVersion pins that every manifest this
// build writes is stamped [ManifestVersion], including the two shapes that
// builds before rmp #3072 stamped with a lower version: a full snapshot
// without mapper.bin (an int64-keyed graph and no codec, formerly v2) and a
// CSR-only snapshot (formerly v1). Each one must still load, and the
// mapper-less one must carry neither mapper.bin nor a WAL position.
func TestWrittenManifest_AlwaysCurrentVersion(t *testing.T) {
	t.Parallel()
	if ManifestVersion <= lastPreMultigraphManifestVersion {
		t.Fatalf("ManifestVersion = %d, want > %d so pre-#3072 builds refuse it",
			ManifestVersion, lastPreMultigraphManifestVersion)
	}

	t.Run("full_without_mapper", func(t *testing.T) {
		t.Parallel()
		g := lpg.New[int64, int64](adjlist.Config{})
		if err := g.AddEdge(1, 2, 7); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
		dir := filepath.Join(t.TempDir(), "snap")
		if err := WriteSnapshotFull(dir, csr.BuildFromAdjList(g.AdjList()), g); err != nil {
			t.Fatalf("WriteSnapshotFull: %v", err)
		}
		loaded, err := LoadSnapshotFull(dir)
		if err != nil {
			t.Fatalf("LoadSnapshotFull: %v", err)
		}
		m := loaded.Manifest
		if m.Version != ManifestVersion {
			t.Fatalf("Manifest.Version = %d, want %d", m.Version, ManifestVersion)
		}
		for _, f := range m.Files {
			if f.Name == MapperFile {
				t.Fatalf("manifest lists %s for an int64-keyed graph written without a codec", MapperFile)
			}
		}
		if len(loaded.Mapper.Pairs) != 0 || len(loaded.Mapper.RawPairs) != 0 {
			t.Fatalf("mapper readback = %d pairs, %d raw pairs, want none",
				len(loaded.Mapper.Pairs), len(loaded.Mapper.RawPairs))
		}
		if m.StoreID != "" || m.WALRedoPos != 0 || m.WALFormat != 0 {
			t.Fatalf("mapper-less manifest carries a WAL position: store_id=%q redo=%d format=%d",
				m.StoreID, m.WALRedoPos, m.WALFormat)
		}
		if got := len(loaded.CSR.Edges); got != 1 {
			t.Fatalf("loaded CSR edges = %d, want 1", got)
		}
	})

	t.Run("csr_only", func(t *testing.T) {
		t.Parallel()
		a := adjlist.New[string, int64](adjlist.Config{})
		if err := a.AddEdge("a", "b", 1); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
		dir := filepath.Join(t.TempDir(), "snap")
		if err := WriteSnapshotCSR(dir, csr.BuildFromAdjList(a)); err != nil {
			t.Fatalf("WriteSnapshotCSR: %v", err)
		}
		loaded, err := LoadSnapshotFull(dir)
		if err != nil {
			t.Fatalf("LoadSnapshotFull: %v", err)
		}
		if loaded.Manifest.Version != ManifestVersion {
			t.Fatalf("Manifest.Version = %d, want %d", loaded.Manifest.Version, ManifestVersion)
		}
		if got := len(loaded.CSR.Edges); got != 1 {
			t.Fatalf("loaded CSR edges = %d, want 1", got)
		}
	})
}
