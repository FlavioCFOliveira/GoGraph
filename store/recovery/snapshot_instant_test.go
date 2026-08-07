package recovery_test

// snapshot_instant_test.go — rmp #2309 (MVCC C3d), acceptance criterion 4: a
// snapshot records the instant it captured, and that instant is consistent with the
// data in it.
//
// Layer: short.
//
// # Why the snapshot half of the derivation is the half that matters
//
// Recovery derives the MVCC clock by folding a maximum over every commit instant it
// can see. C3c measured that the WAL half is unobservable on its own: replay mints
// an instant per OP while the durable maximum counts TRANSACTIONS, so the replayed
// clock always overshoots and the WAL-derived floor never bites.
//
// A snapshot changes that, and it is the normal production shape. A checkpoint
// TRUNCATES the WAL prefix, so the instants of everything the image folded are no
// longer in the log at all. Deriving from the WAL alone would then restore a clock
// far below data the image already holds, and the next commit would re-mint instants
// that are durably in it.
//
// So the image has to name its own instant. It is the same quantity Memgraph reads
// back from a snapshot as info.start_timestamp and restores as
// timestamp_ = max(timestamp_, next_timestamp).

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// TestSnapshotInstant_IsRecordedAndConsistentWithTheData is AC 4.
//
// "Consistent with the data" is asserted in the direction that matters: the recorded
// instant must be at or AFTER every commit whose writes the image contains, and at
// or BEFORE any commit it does not. An instant recorded too EARLY is the dangerous
// one — recovery would restore a clock below data the image already holds.
func TestSnapshotInstant_IsRecordedAndConsistentWithTheData(t *testing.T) {
	dir := t.TempDir()
	wr, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	defer func() { _ = wr.Close() }()

	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	st := txn.NewStoreWithOptions[string, float64](g, wr, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	eng := cypher.NewEngineWithStore(st)
	ctx := context.Background()

	const inSnapshot = 5
	for i := 0; i < inSnapshot; i++ {
		if _, cerr := eng.RunInTx(ctx, "CREATE (n:Acct {id: $id})",
			map[string]expr.Value{"id": expr.IntegerValue(int64(i))}); cerr != nil {
			t.Fatalf("commit %d: %v", i, cerr)
		}
	}
	instantAtCapture := g.MVCCStats().Now
	if instantAtCapture == 0 {
		t.Fatal("the engine published no commit instant, so there is no instant for the " +
			"capture to record and this test cannot discriminate")
	}

	capt, err := snapshot.CaptureGraph[string, float64](g, csr.BuildFromAdjList[string, float64](g.AdjList()), nil, nil)
	if err != nil {
		t.Fatalf("CaptureGraph: %v", err)
	}

	// The capture must name the instant the graph was actually at.
	if got := capt.CommitTS(); got != instantAtCapture {
		t.Fatalf("the capture records instant %d but the graph was at %d. An instant "+
			"recorded too EARLY is the dangerous direction: recovery would restore a "+
			"clock below data the image already holds, then re-mint instants that are "+
			"durably in it", got, instantAtCapture)
	}

	// Commits AFTER the capture must not be inside it, so the recorded instant is
	// genuinely a boundary rather than "whatever the clock said later".
	for i := inSnapshot; i < inSnapshot+3; i++ {
		if _, cerr := eng.RunInTx(ctx, "CREATE (n:Acct {id: $id})",
			map[string]expr.Value{"id": expr.IntegerValue(int64(i))}); cerr != nil {
			t.Fatalf("post-capture commit %d: %v", i, cerr)
		}
	}
	if after := g.MVCCStats().Now; after <= capt.CommitTS() {
		t.Fatalf("the clock is %d after three more commits but the capture recorded "+
			"%d: the recorded instant is not a boundary at all", after, capt.CommitTS())
	}
	if got := capt.CommitTS(); got != instantAtCapture {
		t.Fatalf("the capture's instant moved to %d after later commits, want %d: a "+
			"capture is an immutable image and its instant must be frozen with it",
			got, instantAtCapture)
	}

	// And it must reach the manifest on disk.
	snapDir := filepath.Join(dir, "snapshot")
	if werr := snapshot.WriteCapture[float64](snapDir, capt, nil, nil); werr != nil {
		t.Fatalf("WriteCapture: %v", werr)
	}
	man := loadManifest(t, snapDir)
	if man.CommitTS != instantAtCapture {
		t.Fatalf("the manifest records instant %d, want %d: the capture knows its "+
			"instant but the published image does not, so recovery cannot read it",
			man.CommitTS, instantAtCapture)
	}
}

// TestSnapshotInstant_RecoveryDerivesTheFloorFromTheImage is the consequence: a
// directory whose WAL carries fewer instants than the image must still restore a
// clock above the image.
func TestSnapshotInstant_RecoveryDerivesTheFloorFromTheImage(t *testing.T) {
	dir := t.TempDir()
	wr, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	st := txn.NewStoreWithOptions[string, float64](g, wr, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	eng := cypher.NewEngineWithStore(st)
	for i := 0; i < 4; i++ {
		if _, cerr := eng.RunInTx(context.Background(), "CREATE (n:Acct {id: $id})",
			map[string]expr.Value{"id": expr.IntegerValue(int64(i))}); cerr != nil {
			t.Fatalf("commit %d: %v", i, cerr)
		}
	}
	capt, err := snapshot.CaptureGraph[string, float64](g, csr.BuildFromAdjList[string, float64](g.AdjList()), txn.NewStringCodec(), nil)
	if err != nil {
		t.Fatalf("CaptureGraph: %v", err)
	}
	if werr := snapshot.WriteCapture[float64](filepath.Join(dir, "snapshot"), capt, nil, nil); werr != nil {
		t.Fatalf("WriteCapture: %v", werr)
	}
	if cerr := wr.Close(); cerr != nil {
		t.Fatalf("close wal: %v", cerr)
	}

	// Remove the WAL entirely: the extreme of a truncated prefix, and the state a
	// checkpoint approaches. Every instant must now come from the image.
	if rerr := os.Remove(filepath.Join(dir, "wal")); rerr != nil {
		t.Fatalf("remove wal: %v", rerr)
	}

	res, err := recovery.Open[string, float64](dir, recovery.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	if !res.SnapshotHit {
		t.Fatal("recovery did not load the snapshot, so this test exercised nothing")
	}
	if res.MaxCommitTS != capt.CommitTS() {
		t.Fatalf("recovery derived a maximum of %d from a directory whose only source "+
			"of instants is an image recorded at %d: with the WAL prefix truncated "+
			"there is nowhere else for the floor to come from, and a clock below the "+
			"image re-mints instants that are durably in it",
			res.MaxCommitTS, capt.CommitTS())
	}
	if now := res.Graph.MVCCStats().Now; now <= capt.CommitTS() {
		t.Fatalf("the recovered clock reads %d, at or below the image's %d",
			now, capt.CommitTS())
	}
}

// TestSnapshotInstant_AnEmptyWALDoesNotEraseTheImagesInstant is the regression for
// combining the two sources.
//
// The derived floor is a MAXIMUM over everything durable, and recovery reads the
// snapshot's instant first and the WAL's second. Assigning the WAL's value instead
// of taking the maximum discards the image's instant whenever the surviving suffix
// carries a smaller one — and after a checkpoint with no subsequent writes the
// suffix carries NOTHING, so the floor collapses to zero and the next commit starts
// re-minting instants the image already holds.
//
// That is the ordinary state of a freshly checkpointed directory, not an exotic one,
// and the sibling test above cannot catch it: it removes the WAL entirely, so the
// WAL branch never runs.
func TestSnapshotInstant_AnEmptyWALDoesNotEraseTheImagesInstant(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")
	wr, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	st := txn.NewStoreWithOptions[string, float64](g, wr, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	eng := cypher.NewEngineWithStore(st)
	for i := 0; i < 4; i++ {
		if _, cerr := eng.RunInTx(context.Background(), "CREATE (n:Acct {id: $id})",
			map[string]expr.Value{"id": expr.IntegerValue(int64(i))}); cerr != nil {
			t.Fatalf("commit %d: %v", i, cerr)
		}
	}
	capt, err := snapshot.CaptureGraph[string, float64](
		g, csr.BuildFromAdjList[string, float64](g.AdjList()), txn.NewStringCodec(), nil)
	if err != nil {
		t.Fatalf("CaptureGraph: %v", err)
	}
	if werr := snapshot.WriteCapture[float64](filepath.Join(dir, "snapshot"), capt, nil, nil); werr != nil {
		t.Fatalf("WriteCapture: %v", werr)
	}
	if cerr := wr.Close(); cerr != nil {
		t.Fatalf("close wal: %v", cerr)
	}

	// Truncate the WAL to empty — what a prefix-truncating checkpoint leaves behind
	// when nothing was committed after the image. The file EXISTS, so recovery walks
	// its (empty) replay path and reaches the fold.
	if terr := os.Truncate(walPath, 0); terr != nil {
		t.Fatalf("truncate wal: %v", terr)
	}

	res, err := recovery.Open[string, float64](dir, recovery.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	if !res.SnapshotHit {
		t.Fatal("recovery did not load the snapshot, so this test exercised nothing")
	}
	if res.MaxCommitTS != capt.CommitTS() {
		t.Fatalf("recovery derived a maximum of %d with a snapshot recorded at %d and "+
			"an EMPTY WAL: the empty log's zero has overwritten the image's instant "+
			"instead of losing the maximum to it, so a freshly checkpointed directory "+
			"restores a clock of zero and re-mints every instant the image holds",
			res.MaxCommitTS, capt.CommitTS())
	}
}

// loadManifest reads a published snapshot's manifest from disk.
func loadManifest(t *testing.T, snapDir string) snapshot.Manifest {
	t.Helper()
	f, err := os.Open(filepath.Join(snapDir, "manifest.json")) //nolint:gosec // test-owned temp path
	if err != nil {
		t.Fatalf("open manifest: %v", err)
	}
	defer func() { _ = f.Close() }()
	m, err := snapshot.LoadManifest(f)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	return m
}
