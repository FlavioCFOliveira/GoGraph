package sim

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// checkpointedImageFacts is what [writeCheckpointedImage] observed while
// building a real on-disk store image: the counts it wrote and the WAL sizes
// that bracket the checkpoint.
type checkpointedImageFacts struct {
	nodes           int
	edges           int
	walBytesBefore  int64
	walBytesAfter   int64
	snapshotEntries int
}

// writeCheckpointedImage builds, on the REAL filesystem under dir, exactly the
// image shape the cross-release helper now produces at a prior tag: a WAL-backed
// store driven through the Cypher engine, then a published checkpoint (snapshot
// directory + WAL prefix truncate), then a clean close.
//
// It exists so the full-stack reopen introduced by rmp #2477 is exercised on
// every change. The genuine cross-version run needs git, a worktree and a
// buildable tag and is therefore soak-gated; the code path it drives on the
// CURRENT side — recovery.OpenCtx over a directory that holds a snapshot AND a
// truncated WAL — is the same one either way, and that half needs no subprocess.
func writeCheckpointedImage(ctx context.Context, dir string, nNodes int) (checkpointedImageFacts, error) {
	var facts checkpointedImageFacts
	walPath := filepath.Join(dir, "wal")
	wlog, err := wal.Open(walPath)
	if err != nil {
		return facts, err
	}
	defer func() { _ = wlog.Close() }()

	// The same shape the helper uses: a directed SIMPLE graph over string keys
	// and float64 weights.
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: false})
	st := txn.NewStoreWithOptions(g, wlog, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	eng := cypher.NewEngineWithStore(st)

	for i := range nNodes {
		q := "CREATE (n:Person {id: " + strconv.Itoa(i) + ", name: 'n" + strconv.Itoa(i) + "'})"
		res, rerr := eng.RunInTx(ctx, q, nil)
		if rerr != nil {
			return facts, rerr
		}
		_ = res.Close()
		facts.nodes++
	}
	for i := 1; i < nNodes; i++ {
		q := "MATCH (a:Person {id: " + strconv.Itoa(i-1) + "}), (b:Person {id: " + strconv.Itoa(i) + "}) CREATE (a)-[:KNOWS]->(b)"
		res, rerr := eng.RunInTx(ctx, q, nil)
		if rerr != nil {
			return facts, rerr
		}
		_ = res.Close()
		facts.edges++
	}

	if fi, serr := os.Stat(walPath); serr == nil {
		facts.walBytesBefore = fi.Size()
	}

	// Publish the checkpoint exactly as cmd/sim-xrelease-helper/checkpoint.go
	// does — same constructor, same (empty) option set, same fresh mutex. The
	// fidelity is deliberate: this is where the helper's minimal option set is
	// proven sufficient, on the current build, before a prior tag runs it.
	var storeMu sync.Mutex
	cp := checkpoint.New[string, float64](checkpoint.Config{Dir: dir}, g, wlog, &storeMu)
	if cerr := cp.RunCheckpoint(); cerr != nil {
		return facts, cerr
	}

	if fi, serr := os.Stat(walPath); serr == nil {
		facts.walBytesAfter = fi.Size()
	}
	if ents, derr := os.ReadDir(filepath.Join(dir, "snapshot")); derr == nil {
		facts.snapshotEntries = len(ents)
	}
	return facts, nil
}

// TestCrossRelease_FullStackReopenOpensSnapshot is the short-layer proof that
// the cross-release reopen genuinely opens a SNAPSHOT DIRECTORY (rmp #2477).
//
// Before this task the harness reopened a prior release's image with
// recovery.ReplayWAL — the WAL-only core — so no prior release's manifest.json
// or csr.bin was ever parsed by current code in any cross-release test. The
// reopen now routes through recovery.OpenCtx. This test drives that path over an
// image built here, with the checkpoint published exactly as the helper
// publishes it, so the current-side half of the change is covered without the
// soak-gated subprocess build.
func TestCrossRelease_FullStackReopenOpensSnapshot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()

	const nNodes = 24
	facts, err := writeCheckpointedImage(ctx, dir, nNodes)
	if err != nil {
		t.Fatalf("writeCheckpointedImage: %v", err)
	}

	// ── Verdict ───────────────────────────────────────────────────────────────
	// The image is on disk. The current full-stack reopen must find the snapshot,
	// recover the whole graph, and agree with what the filesystem shows.
	inspect := InspectPriorSnapshotDir(dir)
	if !inspect.Present {
		t.Fatalf("checkpoint published no snapshot manifest under %s", dir)
	}
	if inspect.ManifestErr != nil {
		t.Fatalf("current reader rejected the published manifest: %v", inspect.ManifestErr)
	}
	rec, err := recoverImageGraph(ctx, dir)
	if err != nil {
		t.Fatalf("full-stack reopen of the checkpointed image: %v", err)
	}
	if !rec.snapshotHit {
		t.Fatalf("full-stack reopen did not load the snapshot directory it was given "+
			"(manifest v%d, files %v)", inspect.ManifestVersion, inspect.Files)
	}
	gotNodes, _ := rec.engine.NodeCount()
	if gotNodes != int64(nNodes) {
		t.Fatalf("recovered node count = %d, want %d", gotNodes, nNodes)
	}
	gotEdges, _ := rec.engine.EdgeCount()
	if gotEdges != int64(facts.edges) {
		t.Fatalf("recovered edge count = %d, want %d", gotEdges, facts.edges)
	}
	// The snapshot must have carried the labels and properties too, not just the
	// topology: a CSR-only recovery would satisfy both counts above.
	if rec.snapshotLabels == 0 {
		t.Errorf("snapshot contributed 0 label records for %d labelled nodes", nNodes)
	}
	if rec.snapshotProperties == 0 {
		t.Errorf("snapshot contributed 0 property records for %d propertied nodes", nNodes)
	}

	// ── Non-vacuity gate (shape only) ─────────────────────────────────────────
	// Everything above would also pass on an image whose WAL still held every op,
	// with the snapshot contributing nothing. Two independent facts rule that
	// out: the WAL shrank across the checkpoint, and the reopen replayed ZERO WAL
	// ops while recovering a non-empty graph. The graph can then only have come
	// through the snapshot bytes.
	if facts.walBytesAfter >= facts.walBytesBefore {
		t.Fatalf("NON-VACUITY: the checkpoint did not truncate the WAL (%d -> %d bytes); "+
			"the WAL could still account for the recovered graph",
			facts.walBytesBefore, facts.walBytesAfter)
	}
	if rec.walOps != 0 {
		t.Fatalf("NON-VACUITY: the reopen replayed %d WAL ops, so the snapshot is not "+
			"proven load-bearing for the recovered graph", rec.walOps)
	}
	if facts.snapshotEntries < 2 {
		t.Fatalf("NON-VACUITY: the published snapshot holds %d files; a manifest alone "+
			"proves no component was read", facts.snapshotEntries)
	}

	// ── The provenance classifier is two-sided ────────────────────────────────
	// classifySnapshotProvenance is what makes "the reader skipped the snapshot"
	// a detectable state. A classifier that returned "" unconditionally would
	// leave Parity() permanently true on that axis, so both directions are
	// asserted here rather than only the passing one.
	clean := &CrossReleaseUpgradeResult{Tag: "local", PriorSnapshot: inspect, SnapshotOpened: true}
	if gap := classifySnapshotProvenance(clean); gap != "" {
		t.Errorf("classifySnapshotProvenance on a healthy reopen = %q, want empty", gap)
	}
	skipped := &CrossReleaseUpgradeResult{Tag: "local", PriorSnapshot: inspect, SnapshotOpened: false}
	if gap := classifySnapshotProvenance(skipped); gap == "" {
		t.Error("classifySnapshotProvenance did not flag a snapshot present on disk but not loaded")
	}
	phantom := &CrossReleaseUpgradeResult{Tag: "local", SnapshotOpened: true}
	if gap := classifySnapshotProvenance(phantom); gap == "" {
		t.Error("classifySnapshotProvenance did not flag a snapshot hit with no manifest on disk")
	}
	unparsable := &CrossReleaseUpgradeResult{
		Tag:            "local",
		PriorSnapshot:  PriorSnapshotFacts{Present: true, ManifestErr: os.ErrInvalid},
		SnapshotOpened: true,
	}
	if gap := classifySnapshotProvenance(unparsable); gap == "" {
		t.Error("classifySnapshotProvenance did not flag a manifest the current reader rejects")
	}

	// ── Witness ───────────────────────────────────────────────────────────────
	t.Logf("full-stack reopen: manifest v%d integrity=%q verified=%v files=%v; "+
		"WAL %d -> %d bytes across the checkpoint; walOps=%d snapshotLabels=%d snapshotProperties=%d; "+
		"recovered n=%d e=%d",
		inspect.ManifestVersion, inspect.Integrity, inspect.IntegrityVerified, inspect.Files,
		facts.walBytesBefore, facts.walBytesAfter, rec.walOps, rec.snapshotLabels, rec.snapshotProperties,
		gotNodes, gotEdges)
}
