package recovery

// config_roundtrip_test.go — regression coverage for rmp task #1290:
// "Persist adjlist Config in the snapshot manifest and reconstruct from it
// on recovery."
//
// Every graph is a directed multigraph (rmp #3072), so the persisted shape
// is the weightless flag alone. A snapshot written without the field (older
// snapshots, or the CSR-only legacy writer) defaults to the zero Config, and
// a legacy manifest's "directed"/"multigraph" keys select nothing: parallel
// edges survive a snapshot round trip, and AddEdge after recovery appends.
//
// Layer: short. White-box (package recovery) so the tests can assert the
// recovered graph's Config() directly and exercise recoveryGraphConfig.
// goleak-clean (graphs/WALs are local and closed).

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// cfgRoundTripOpts is the recovery codec pair for the [string, int64]
// shape these tests use.
func cfgRoundTripOpts() Options[string, int64] {
	return Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}
}

// writeSelfSufficientSnapshot snapshots g into <dir>/snapshot and truncates
// the WAL at <dir>/wal to zero bytes, so a subsequent recovery.Open is fed
// by the snapshot alone (the self-sufficient v3 path for string keys). It
// returns the snapshot directory. The WAL is created and immediately
// truncated so recovery's WAL-open path is exercised with an empty file.
func writeSelfSufficientSnapshot(t *testing.T, dir string, g *lpg.Graph[string, int64]) string {
	t.Helper()
	walPath := filepath.Join(dir, "wal")
	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	cs := csr.BuildFromAdjList(g.AdjList())
	snapDir := filepath.Join(dir, "snapshot")
	if err := snapshot.WriteSnapshotFull(snapDir, cs, g); err != nil {
		w.Close()
		t.Fatalf("WriteSnapshotFull: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}
	if err := os.Truncate(walPath, 0); err != nil {
		t.Fatalf("truncate WAL: %v", err)
	}
	return snapDir
}

// TestRecovery_MultigraphConfigSurvivesSnapshot is the symmetric guard: a
// MULTIGRAPH graph's parallel edges must survive snapshot + recovery.Open
// as parallel edges, and the recovered graph must remain a multigraph so a
// further AddEdge(a,b) appends rather than collapses. This pins that the
// fix did not regress the (unchanged) multigraph behaviour the openCypher
// additive-CREATE model depends on.
func TestRecovery_MultigraphConfigSurvivesSnapshot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	g := lpg.New[string, int64](adjlist.Config{})
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge #1: %v", err)
	}
	if err := g.AddEdge("a", "b", 2); err != nil {
		t.Fatalf("AddEdge #2: %v", err)
	}
	if got := g.AdjList().Size(); got != 2 {
		t.Fatalf("pre-snapshot Size = %d, want 2 (multigraph keeps parallel edges)", got)
	}

	writeSelfSufficientSnapshot(t, dir, g)

	res, err := Open[string, int64](dir, cfgRoundTripOpts())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Both parallel edges must have survived the CSR round-trip.
	if got := res.Graph.AdjList().Size(); got != 2 {
		t.Fatalf("post-recovery Size = %d, want 2 — parallel edges collapsed across recovery", got)
	}
	// A further AddEdge(a,b) must append (multigraph), confirming the
	// recovered graph kept multigraph insertion semantics.
	if err := res.Graph.AddEdge("a", "b", 3); err != nil {
		t.Fatalf("post-recovery AddEdge: %v", err)
	}
	if got := res.Graph.AdjList().Size(); got != 3 {
		t.Fatalf("post-recovery Size after re-AddEdge = %d, want 3 — recovered graph lost multigraph semantics", got)
	}
}

// TestRecovery_AbsentGraphConfigDefaultsToMultigraph is the backward-
// compatibility guard. A snapshot whose manifest carries no graph_config
// field — every snapshot written before this field existed, and any
// CSR-only legacy snapshot — must reconstruct as the historical default
// {}, so pre-fix snapshots (especially the
// openCypher engine's additive-CREATE snapshots) replay exactly as before.
//
// The test writes a normal v3 snapshot (which now DOES carry graph_config),
// then rewrites its manifest with the field stripped to faithfully emulate
// an older on-disk manifest. Component files and their CRCs are untouched.
func TestRecovery_AbsentGraphConfigDefaultsToMultigraph(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Originating graph is SIMPLE; if recovery honoured a (now-absent)
	// persisted config it would rebuild simple. The point of the test is
	// that with the field stripped, recovery instead falls back to the
	// multigraph default regardless of the originating shape.
	g := lpg.New[string, int64](adjlist.Config{})
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	snapDir := writeSelfSufficientSnapshot(t, dir, g)

	stripGraphConfigFromManifest(t, snapDir)

	res, err := Open[string, int64](dir, cfgRoundTripOpts())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// A manifest without graph_config must default to the zero Config.
	if cfg := res.Graph.Config(); cfg != (adjlist.Config{}) {
		t.Fatalf("recovered Config = %+v, want {} for a manifest without graph_config", cfg)
	}
	// And the default behaviour must be observable: AddEdge(a,b) appends.
	if err := res.Graph.AddEdge("a", "b", 2); err != nil {
		t.Fatalf("post-recovery AddEdge: %v", err)
	}
	if got := res.Graph.AdjList().Size(); got != 2 {
		t.Fatalf("post-recovery Size = %d, want 2 — config-less snapshot must recover as multigraph", got)
	}
}

// TestRecoveryGraphConfig_DefaultAndPersisted is a focused unit test of the
// recoveryGraphConfig resolver: a nil GraphConfig yields the multigraph
// default; a present one is honoured verbatim.
func TestRecoveryGraphConfig_DefaultAndPersisted(t *testing.T) {
	t.Parallel()

	// Absent field (nil) -> historical default.
	if got := recoveryGraphConfig(nil); got != (adjlist.Config{}) {
		t.Fatalf("recoveryGraphConfig(nil) = %+v, want {}", got)
	}

	// Present field -> honoured verbatim. The legacy "directed" and
	// "multigraph" keys select nothing: a simple graph is a valid multigraph,
	// and an undirected snapshot is migrated by the loader (see
	// legacy_undirected_test.go).
	for _, tc := range []struct {
		name string
		in   string
		want adjlist.Config
	}{
		{"weighted", `{}`, adjlist.Config{}},
		{"weightless", `{"weightless": true}`, adjlist.Config{Weightless: true}},
		{"legacy-simple-directed", `{"directed": true, "multigraph": false}`, adjlist.Config{}},
		{"legacy-multi-directed", `{"directed": true, "multigraph": true}`, adjlist.Config{}},
		{"legacy-simple-undirected", `{"directed": false, "multigraph": false}`, adjlist.Config{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var gc snapshot.GraphConfig
			if err := json.Unmarshal([]byte(tc.in), &gc); err != nil {
				t.Fatalf("decode %s: %v", tc.in, err)
			}
			got := recoveryGraphConfig(&gc)
			if got != tc.want {
				t.Fatalf("recoveryGraphConfig(%+v) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

// stripGraphConfigFromManifest rewrites <snapDir>/manifest.json with the
// graph_config field removed, emulating a snapshot written before the
// field existed. The component-file entries (and their CRCs) are preserved,
// so LoadSnapshotFull's per-file CRC validation still passes.
func stripGraphConfigFromManifest(t *testing.T, snapDir string) {
	t.Helper()
	manifestPath := filepath.Join(snapDir, "manifest.json")
	raw, err := os.ReadFile(manifestPath) //nolint:gosec // path under t.TempDir
	if err != nil {
		t.Fatalf("ReadFile(manifest.json): %v", err)
	}
	// Decode through the real reader, not json.Unmarshal: manifest.json is a JSON
	// document followed by a CRC32C trailer, so json.Unmarshal rejects the tail
	// ("invalid character '\x00' after top-level value"). snapshot.WriteManifest
	// below re-frames the stripped manifest, which is what keeps this helper
	// producing a manifest recovery will actually open.
	m, err := snapshot.LoadManifest(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("LoadManifest(manifest): %v", err)
	}
	if m.GraphConfig == nil {
		t.Fatalf("precondition failed: a freshly written snapshot manifest has no graph_config; the writer is not persisting it")
	}
	m.GraphConfig = nil
	f, err := os.Create(manifestPath) //nolint:gosec // path under t.TempDir
	if err != nil {
		t.Fatalf("Create(manifest.json): %v", err)
	}
	if err := snapshot.WriteManifest(f, m); err != nil {
		_ = f.Close()
		t.Fatalf("WriteManifest: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close(manifest.json): %v", err)
	}
}
