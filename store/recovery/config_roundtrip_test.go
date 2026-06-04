package recovery

// config_roundtrip_test.go — regression coverage for rmp task #1290:
// "Persist adjlist Config (directed/multigraph) in the snapshot manifest
// and reconstruct from it on recovery."
//
// Before the fix, recovery.Open hardcoded adjlist.Config{Directed: true,
// Multigraph: true}. A graph created SIMPLE (Multigraph: false) — where
// repeated AddEdge(a,b) collapses to one edge — silently became a
// MULTIGRAPH after a snapshot+reopen: the same AddEdge(a,b) call then
// appended a parallel edge. The recovered graph had divergent edge-
// insertion semantics from the in-process graph for the same public API.
//
// The fix persists the originating graph's directed/multigraph shape in
// the snapshot manifest (snapshot.Manifest.GraphConfig) and rebuilds the
// graph with it. A snapshot written without the field (older snapshots,
// or the CSR-only legacy writer) defaults to the historical
// {Directed: true, Multigraph: true} so the additive-CREATE openCypher
// engine snapshots — and every other pre-fix snapshot — replay exactly
// as before.
//
// Layer: short. White-box (package recovery) so the tests can assert the
// recovered graph's Config() directly and exercise recoveryGraphConfig.
// goleak-clean (graphs/WALs are local and closed).

import (
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

// TestRecovery_SimpleGraphConfigSurvivesSnapshot is the task-#1290
// acceptance criterion. A SIMPLE (Multigraph: false) graph collapses two
// AddEdge(a,b) calls to one edge; after snapshot + recovery.Open it must
// STILL be simple, so a third AddEdge(a,b) on the recovered graph is also
// a no-op and the edge count stays one.
//
// Pre-fix this FAILS: recovery rebuilt the graph as a multigraph, so the
// post-recovery AddEdge(a,b) appended a parallel edge and Size became 2.
func TestRecovery_SimpleGraphConfigSurvivesSnapshot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Build a SIMPLE graph and add the same edge twice — it collapses to
	// one edge under simple-graph semantics.
	g := lpg.New[string, int64](adjlist.Config{Directed: true, Multigraph: false})
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge #1: %v", err)
	}
	if err := g.AddEdge("a", "b", 2); err != nil {
		t.Fatalf("AddEdge #2: %v", err)
	}
	if got := g.AdjList().Size(); got != 1 {
		t.Fatalf("pre-snapshot Size = %d, want 1 (simple-graph collapse)", got)
	}

	writeSelfSufficientSnapshot(t, dir, g)

	res, err := Open[string, int64](dir, cfgRoundTripOpts())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !res.SnapshotHit {
		t.Fatal("SnapshotHit = false")
	}

	// The recovered graph must have been reconstructed SIMPLE.
	if cfg := res.Graph.Config(); cfg.Multigraph {
		t.Fatalf("recovered Config.Multigraph = true, want false (simple-graph shape must survive the snapshot)")
	}
	if got := res.Graph.AdjList().Size(); got != 1 {
		t.Fatalf("post-recovery Size = %d, want 1 (the single collapsed edge)", got)
	}

	// The crux of the AC: the same AddEdge(a,b) on the recovered graph must
	// be a no-op (simple-graph idempotence), not append a parallel edge.
	if err := res.Graph.AddEdge("a", "b", 3); err != nil {
		t.Fatalf("post-recovery AddEdge: %v", err)
	}
	if got := res.Graph.AdjList().Size(); got != 1 {
		t.Fatalf("post-recovery Size after re-AddEdge = %d, want 1 — recovered graph diverged to multigraph semantics", got)
	}
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

	g := lpg.New[string, int64](adjlist.Config{Directed: true, Multigraph: true})
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
	if cfg := res.Graph.Config(); !cfg.Multigraph {
		t.Fatalf("recovered Config.Multigraph = false, want true (multigraph shape must survive the snapshot)")
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
// {Directed: true, Multigraph: true}, so pre-fix snapshots (especially the
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
	g := lpg.New[string, int64](adjlist.Config{Directed: true, Multigraph: false})
	if err := g.AddEdge("a", "b", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	snapDir := writeSelfSufficientSnapshot(t, dir, g)

	stripGraphConfigFromManifest(t, snapDir)

	res, err := Open[string, int64](dir, cfgRoundTripOpts())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// A manifest without graph_config must default to multigraph.
	if cfg := res.Graph.Config(); !cfg.Multigraph || !cfg.Directed {
		t.Fatalf("recovered Config = %+v, want {Directed:true Multigraph:true} for a manifest without graph_config", cfg)
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
	if got := recoveryGraphConfig(nil); got != (adjlist.Config{Directed: true, Multigraph: true}) {
		t.Fatalf("recoveryGraphConfig(nil) = %+v, want {Directed:true Multigraph:true}", got)
	}

	// Present field -> honoured verbatim, including a simple graph.
	for _, tc := range []struct {
		name string
		in   snapshot.GraphConfig
		want adjlist.Config
	}{
		{"simple-directed", snapshot.GraphConfig{Directed: true, Multigraph: false}, adjlist.Config{Directed: true, Multigraph: false}},
		{"multi-directed", snapshot.GraphConfig{Directed: true, Multigraph: true}, adjlist.Config{Directed: true, Multigraph: true}},
		{"simple-undirected", snapshot.GraphConfig{Directed: false, Multigraph: false}, adjlist.Config{Directed: false, Multigraph: false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gc := tc.in
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
	var m snapshot.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("json.Unmarshal(manifest): %v", err)
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
