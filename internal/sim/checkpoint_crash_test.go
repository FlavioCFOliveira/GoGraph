package sim

import (
	"context"
	"os"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// liveCkptStack is the live persistence stack the checkpoint-crash test drives
// over a SimDisk: the recovered graph + a WAL-backed store + a real cypher.Engine.
//
// NOTE ON THE CHECKPOINTER: a live store/checkpoint.Checkpointer cannot yet drive
// the SimDisk WAL, because its post-snapshot WAL truncation
// ([wal.Writer.TruncatePrefix]) requires a PATH-backed writer ([wal.Open]) while
// the SimDisk WAL is handle-backed ([wal.OpenWith]) — the truncation rewrites the
// WAL through the filesystem, a seam the in-memory disk's handle path does not
// expose. The snapshot is therefore published here through the proven direct
// snapshot writer (the same self-sufficient v3 writer the Checkpointer itself
// calls), and the FULL WAL is kept: recovery folds the snapshot and idempotently
// replays the whole WAL over it (handle-bearing ops dedupe via AddEdgeHIfAbsent;
// node interning is idempotent). Wiring a Checkpointer-driven, WAL-truncating
// checkpoint into the live SimDisk stack needs a WAL filesystem seam and is
// tracked as a follow-up; the truncate boundary itself is proven in
// disk_fullstack_test.go / disk_checkpoint_test.go.
type liveCkptStack struct {
	graph          *lpg.Graph[string, float64]
	store          *txn.Store[string, float64]
	eng            *EngineAdapter
	wlog           *wal.Writer
	recoveredNodes int64
}

// openLiveCkptStack recovers the full stack from the SimDisk through the REAL
// recovery.OpenFS (which promotes the last fully-published snapshot and replays
// the WAL tail on top of it), then reopens the WAL for append at the recovered
// tail and binds a real cypher.Engine. Snapshot lives under "snapshot"; the WAL
// stays at the root-level key "wal" (retaining SimDisk's dirent-durability
// exemption for a root file — otherwise a Crash would revoke the WAL itself).
func openLiveCkptStack(t *testing.T, disk *SimDisk) *liveCkptStack {
	t.Helper()
	codec := txn.NewStringCodec()
	wcodec := txn.NewFloat64WeightCodec()

	res, err := recovery.OpenFS[string, float64](
		simRecoveryFS{disk: disk}, "",
		recovery.Options[string, float64]{Codec: codec, WeightCodec: wcodec},
	)
	if err != nil {
		t.Fatalf("recovery.OpenFS: %v", err)
	}
	if !res.IsClean() {
		t.Fatalf("recovery found corruption: %v", res.TailErr)
	}
	if disk.Exists(simWALPath) {
		if err := truncateSimWALAt(disk, simWALPath, res.WALTailOffset); err != nil {
			t.Fatalf("truncate torn WAL tail: %v", err)
		}
	}
	wh, err := disk.OpenFile(simWALPath, os.O_CREATE|os.O_RDWR|os.O_APPEND)
	if err != nil {
		t.Fatalf("open WAL for append: %v", err)
	}
	wlog, err := wal.OpenWith(wh)
	if err != nil {
		t.Fatalf("wal.OpenWith: %v", err)
	}
	store := txn.NewStoreWithOptions(res.Graph, wlog, txn.Options[string, float64]{
		Codec: codec, WeightCodec: wcodec,
	})
	eng := cypher.NewEngineWithStoreAndSchema(store, res.Constraints, res.Indexes)
	return &liveCkptStack{
		graph: res.Graph, store: store, eng: NewEngineAdapter(eng), wlog: wlog,
		recoveredNodes: int64(res.Graph.LiveOrder()),
	}
}

// publishSnapshot writes a self-sufficient snapshot of the live graph to the
// SimDisk through the full fsync publish protocol (the same writer the
// Checkpointer uses), capturing the committed state as of this quiescent point.
// The WAL is intentionally left intact, so recovery folds this snapshot and
// replays the whole WAL over it.
func (s *liveCkptStack) publishSnapshot(t *testing.T, disk *SimDisk) {
	t.Helper()
	cs := csr.BuildFromAdjList(s.graph.AdjList())
	if err := snapshot.WriteSnapshotFullWithMapperCodecAndConstraintsFS(
		simSnapshotFS{disk: disk}, "snapshot", cs, s.graph, txn.NewStringCodec(), nil,
	); err != nil {
		t.Fatalf("publish snapshot: %v", err)
	}
}

// TestFullStack_LiveEngineSnapshotCrashRecovery is the T9 capstone: it drives the
// REAL cypher.Engine over a SimDisk, publishes a self-sufficient snapshot of the
// live committed state, commits MORE through the engine (WAL tail), then CRASHES
// (drops the in-memory engine/store/graph, keeping only the durable SimDisk
// image). Recovery through recovery.OpenFS must promote the snapshot and fold the
// WAL tail back, reconstructing the EXACT committed state — every acknowledged
// commit survives, none lost, none duplicated.
func TestFullStack_LiveEngineSnapshotCrashRecovery(t *testing.T) {
	defer goleak.VerifyNone(t)
	disk := NewSimDisk(NewSeed(0xCE40), 0) // no data faults: isolate snapshot+crash
	ctx := context.Background()

	s := openLiveCkptStack(t, disk)

	// Commit 20 nodes, then snapshot the live state.
	for i := 0; i < 20; i++ {
		if _, err := s.eng.RunWrite(ctx, "CREATE (:Person {id:$id})", map[string]any{"id": int64(i)}); err != nil {
			t.Fatalf("seed node %d: %v", i, err)
		}
	}
	s.publishSnapshot(t, disk)
	atSnapshot, err := s.eng.NodeCount()
	if err != nil {
		t.Fatalf("node count at snapshot: %v", err)
	}

	// Commit 10 MORE nodes AFTER the snapshot — these live only in the WAL tail.
	for i := 20; i < 30; i++ {
		if _, err := s.eng.RunWrite(ctx, "CREATE (:Person {id:$id})", map[string]any{"id": int64(i)}); err != nil {
			t.Fatalf("post-snapshot node %d: %v", i, err)
		}
	}
	total, err := s.eng.NodeCount()
	if err != nil {
		t.Fatalf("total node count: %v", err)
	}
	if atSnapshot != 20 || total != 30 {
		t.Fatalf("unexpected committed state: atSnapshot=%d total=%d (want 20, 30)", atSnapshot, total)
	}

	// CRASH: drop the in-memory stack (no graceful close); only the durable
	// SimDisk image (published snapshot + post-snapshot WAL tail) survives.
	disk.Crash()

	// RECOVER: OpenFS promotes the snapshot and replays the WAL tail.
	s2 := openLiveCkptStack(t, disk)
	recovered, err := s2.eng.NodeCount()
	if err != nil {
		t.Fatalf("recovered node count: %v", err)
	}
	if recovered != total {
		t.Fatalf("snapshot+WAL-tail crash recovery lost data: recovered=%d want=%d (snapshot=%d + tail=%d)",
			recovered, total, atSnapshot, total-atSnapshot)
	}
	if s2.recoveredNodes != total {
		t.Fatalf("recovered graph LiveOrder=%d, want %d", s2.recoveredNodes, total)
	}
}

// TestFullStack_LiveEngineSnapshotCrashBeforeTail crashes IMMEDIATELY after the
// snapshot publish (no WAL tail), isolating the snapshot-promote arm: recovery
// must reconstruct exactly the snapshotted state.
func TestFullStack_LiveEngineSnapshotCrashBeforeTail(t *testing.T) {
	defer goleak.VerifyNone(t)
	disk := NewSimDisk(NewSeed(0xCE41), 0)
	ctx := context.Background()

	s := openLiveCkptStack(t, disk)
	for i := 0; i < 12; i++ {
		if _, err := s.eng.RunWrite(ctx, "CREATE (:Person {id:$id})", map[string]any{"id": int64(i)}); err != nil {
			t.Fatalf("seed node %d: %v", i, err)
		}
	}
	s.publishSnapshot(t, disk)
	disk.Crash()

	s2 := openLiveCkptStack(t, disk)
	recovered, err := s2.eng.NodeCount()
	if err != nil {
		t.Fatalf("recovered node count: %v", err)
	}
	if recovered != 12 {
		t.Fatalf("snapshot-only crash recovery: recovered=%d, want 12", recovered)
	}
}
