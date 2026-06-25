package checkpoint

// phase2_ddl_retention_test.go — hardening gate for the 2026-06-25 round-2 audit
// (#1774). A CREATE INDEX committed DURING the lock-free checkpoint phase-2
// window (after the watermark/CSR capture, so it is NOT in the snapshot) must be
// caught by the phase-3 self-sufficiency re-check (which consults
// Graph.HasIndexes), so the WAL prefix holding that OpCreateIndex frame is
// RETAINED — never truncated — preserving the index across a restart (#1755).
// This complements indexdefs_survival_test.go, which covers the index created
// BEFORE the checkpoint; this covers the racing case.

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

func TestCheckpoint_Phase2IndexDDL_RetainsWAL(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	opts := txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}
	store := txn.NewStoreWithOptions[string, int64](g, w, opts)

	// Seed so the snapshot has content and a watermark to capture.
	tx := store.Begin()
	if err := tx.AddNode("seed"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit(seed): %v", err)
	}

	var mu sync.Mutex
	cp := New(Config{Dir: dir, MaxAge: 0}, g, w, &mu,
		WithCommitSerialiser[string, int64](store.RunUnderCommitLock),
		WithMapperCodec[string, int64](store.Codec()),
	)

	// Park phase 2 long enough to commit a CREATE INDEX into the window.
	const phase2Delay = 300 * time.Millisecond
	phase2Entered := make(chan struct{})
	cp.afterCaptureHook = func() {
		close(phase2Entered)
		time.Sleep(phase2Delay)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(ctx)
	defer cp.Stop()

	cpDone := make(chan error, 1)
	go func() { cpDone <- cp.Trigger() }()

	select {
	case <-phase2Entered:
	case <-time.After(5 * time.Second):
		t.Fatal("checkpoint did not reach phase 2")
	}

	// Commit a CREATE INDEX during phase 2 — after the watermark capture, so the
	// snapshot being written does NOT contain it; only the WAL does.
	itx := store.Begin()
	if err := itx.CreateIndex(txn.IndexKindHash, "Person", "email", "ix_person_email"); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	if err := itx.Commit(); err != nil {
		t.Fatalf("Commit(CREATE INDEX): %v", err)
	}

	if err := <-cpDone; err != nil {
		t.Fatalf("checkpoint Trigger: %v", err)
	}

	// The store-direct index count makes HasIndexes true, so the phase-3 re-check
	// judged the snapshot NOT self-sufficient and SKIPPED truncation.
	if !g.HasIndexes() {
		t.Fatal("HasIndexes() = false after a committed CREATE INDEX (#1774)")
	}
	if got := cp.Stats().WALTruncBytes; got != 0 {
		t.Fatalf("WALTruncBytes = %d, want 0: a CREATE INDEX committed during phase 2 must keep the WAL retained, not truncated (#1755/#1774)", got)
	}
}
