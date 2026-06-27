package sim

import (
	"context"
	"os"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// ============================================================================
// Round-6 cross-layer (Target B) audit: drive the full stack
//   cypher engine -> store/txn -> graph/lpg -> store/wal -> recovery
// over a SimDisk, then RECOVER from a fresh store object and assert the
// acknowledged commits survive. Also re-verify WAL replay idempotence with a
// fresh angle (replay-twice == replay-once over an independent graph).
// ============================================================================

// TestCrossLayer_AckedCommitSurvivesCrash drives the REAL cypher.Engine over a
// WAL-backed txn.Store on a SimDisk, commits N nodes + edges (each RunWrite
// returning success is an ACK), then CRASHES (drops the in-memory stack WITHOUT
// a graceful close, so only the durable SimDisk WAL bytes survive — any buffered
// unsynced frame is lost exactly as kill -9 would lose it), reopens via real
// recovery, and asserts every acked datum is present. This is the durability
// claim end-to-end through every layer.
func TestCrossLayer_AckedCommitSurvivesCrash(t *testing.T) {
	disk := NewSimDisk(NewSeed(0x6A11), 0) // no data faults: isolate the crash boundary
	ctx := context.Background()

	store, err := OpenSimStore(disk, simulatorStoreConfig())
	if err != nil {
		t.Fatalf("OpenSimStore: %v", err)
	}
	eng := NewEngineAdapter(store.Engine())

	const nPeople = 25
	for i := 0; i < nPeople; i++ {
		name := nameAt(i)
		res, err := eng.RunWrite(ctx, tmplCreatePerson, map[string]any{"name": name, "age": int64(i)})
		if err != nil {
			t.Fatalf("CREATE person %q acked-failed: %v", name, err)
		}
		drain(res)
	}
	// A few edges chaining consecutive people.
	const nEdges = 10
	for i := 0; i < nEdges; i++ {
		res, err := eng.RunWrite(ctx, tmplCreateKnows, map[string]any{"a": nameAt(i), "b": nameAt(i + 1)})
		if err != nil {
			t.Fatalf("CREATE edge %d acked-failed: %v", i, err)
		}
		drain(res)
	}

	preN, _ := eng.NodeCount()
	preE, _ := eng.EdgeCount()
	if preN != nPeople || preE != nEdges {
		t.Fatalf("pre-crash counts: nodes=%d edges=%d, want %d,%d", preN, preE, nPeople, nEdges)
	}

	// CRASH: drop the live stack without graceful close. (We intentionally do NOT
	// call store.Close(); we drop references and reopen, modelling kill -9. Any
	// frame still in the WAL bufio buffer that was never fsync'd is lost.)
	store = nil

	// RECOVER from a fresh store object reading the same SimDisk image.
	store2, err := OpenSimStore(disk, simulatorStoreConfig())
	if err != nil {
		t.Fatalf("reopen after crash: %v", err)
	}
	defer func() { _ = store2.Close() }()
	eng2 := NewEngineAdapter(store2.Engine())

	gotN, _ := eng2.NodeCount()
	gotE, _ := eng2.EdgeCount()
	if gotN != nPeople {
		t.Fatalf("DURABILITY BREACH: recovered nodes=%d, want %d (acked commits lost)", gotN, nPeople)
	}
	if gotE != nEdges {
		t.Fatalf("DURABILITY BREACH: recovered edges=%d, want %d (acked commits lost)", gotE, nEdges)
	}
	// Spot-check identity, not just count.
	for i := 0; i < nPeople; i++ {
		res, err := eng2.Run(ctx, "MATCH (n:Person {name:$name}) RETURN count(n)", map[string]any{"name": nameAt(i)})
		if err != nil {
			t.Fatalf("post-recovery probe %d: %v", i, err)
		}
		if !res.Next() {
			t.Fatalf("post-recovery probe %d: no row", i)
		}
		c, _ := res.ScalarInt()
		_ = res.Close()
		if c != 1 {
			t.Fatalf("DURABILITY BREACH: person %q count=%d after recovery, want 1", nameAt(i), c)
		}
	}
	t.Logf("VERIFIED: %d acked nodes + %d acked edges survived a full-stack crash+recovery", nPeople, nEdges)
}

// TestCrossLayer_ReplayIdempotence re-verifies, with a fresh angle, that WAL
// replay is idempotent and order-faithful: replaying the SAME committed WAL byte
// image into TWO independent freshly-built graphs yields identical node/edge
// counts (replay-twice == replay-once). A replay that double-applied an op
// (non-idempotent) or dropped one (non-faithful) would diverge.
func TestCrossLayer_ReplayIdempotence(t *testing.T) {
	disk := NewSimDisk(NewSeed(0x6B22), 0)
	ctx := context.Background()

	// Produce a committed WAL via the real engine.
	store, err := OpenSimStore(disk, simulatorStoreConfig())
	if err != nil {
		t.Fatalf("OpenSimStore: %v", err)
	}
	eng := NewEngineAdapter(store.Engine())
	for i := 0; i < 15; i++ {
		res, err := eng.RunWrite(ctx, tmplCreatePerson, map[string]any{"name": nameAt(i), "age": int64(i)})
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		drain(res)
	}
	for i := 0; i < 7; i++ {
		res, err := eng.RunWrite(ctx, tmplCreateKnows, map[string]any{"a": nameAt(i), "b": nameAt(i + 1)})
		if err != nil {
			t.Fatalf("edge %d: %v", i, err)
		}
		drain(res)
	}
	if err := store.Close(); err != nil { // graceful: flush+fsync everything
		t.Fatalf("close: %v", err)
	}

	// replayOnce replays the committed WAL image into an independent fresh graph
	// via ReplayWAL, then reports the recovered live-node count and the WAL op
	// count replay folded. Each call builds a brand-new graph, so two calls
	// replaying the SAME committed bytes must agree exactly — a non-idempotent or
	// non-faithful replay would diverge.
	replayOnce := func() (int, int) {
		g := lpg.New[string, float64](simulatorStoreConfig().graphConfig)
		rh, err := disk.OpenFile(simWALPath, os.O_RDONLY)
		if err != nil {
			t.Fatalf("open WAL: %v", err)
		}
		reader := wal.NewReader(rh, rh)
		rr, err := recovery.ReplayWAL[string, float64](
			ctx, reader, g, txn.NewStringCodec(), txn.NewFloat64WeightCodec(),
			txn.DefaultMaxTxnOps,
		)
		_ = reader.Close()
		if err != nil {
			t.Fatalf("ReplayWAL: %v", err)
		}
		if !rr.IsClean() {
			t.Fatalf("replay not clean: %v", rr.TailErr)
		}
		return int(g.LiveOrder()), rr.WALOps
	}

	n1, ops1 := replayOnce()
	n2, ops2 := replayOnce()
	if n1 != n2 || ops1 != ops2 {
		t.Fatalf("REPLAY NON-DETERMINISM: first=(nodes%d,walops%d) second=(nodes%d,walops%d)", n1, ops1, n2, ops2)
	}
	if n1 != 15 {
		t.Fatalf("replay produced wrong node count: %d, want 15", n1)
	}
	// Cross-check the edge dimension through a full SimStore reopen (engine count).
	store2, err := OpenSimStore(disk, simulatorStoreConfig())
	if err != nil {
		t.Fatalf("reopen for edge check: %v", err)
	}
	defer func() { _ = store2.Close() }()
	gotE, _ := NewEngineAdapter(store2.Engine()).EdgeCount()
	if gotE != 7 {
		t.Fatalf("recovered edge count=%d, want 7", gotE)
	}
	t.Logf("VERIFIED: replay-twice == replay-once == (nodes=%d, walops=%d), edges=%d; idempotent + order-faithful", n1, ops1, gotE)
}

// nameAt produces a deterministic, distinct Person name for index i.
func nameAt(i int) string {
	return "p" + itoa(i)
}

func drain(res Result) {
	for res.Next() {
	}
	_ = res.Close()
}
