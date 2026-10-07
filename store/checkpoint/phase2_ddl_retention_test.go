package checkpoint

// phase2_ddl_retention_test.go — hardening gate for the 2026-06-25 round-2 audit
// (#1774). A CREATE INDEX committed DURING the lock-free checkpoint phase-2
// window (after the watermark/CSR capture, so it is NOT in the snapshot) must
// survive a restart (#1755). With the segmented WAL (docs/design-wal-v2.md §2.4)
// its OpCreateIndex frame lies at or above the checkpoint's redo position, which
// no checkpoint discards and recovery always replays, so the snapshot may be
// recorded as the start of recovery without a phase-3 re-check under the commit
// lock.
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
	"github.com/FlavioCFOliveira/GoGraph/internal/waltest"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
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

	if !g.HasIndexes() {
		t.Fatal("HasIndexes() = false after a committed CREATE INDEX (#1774)")
	}
	// The snapshot (captured before the DDL) stands alone for every frame below
	// its redo position, so the checkpoint records it as the start of recovery.
	redo, ok, err := waltest.CheckpointRecorded(dir)
	if err != nil || !ok {
		t.Fatalf("the checkpoint did not record its snapshot (err %v)", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}
	// The DDL frame lies at or above the redo position, so recovery replays it
	// and the index survives the restart.
	res, err := recovery.Open[string, int64](dir, recovery.OptionsFromTxn(opts))
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	found := false
	for _, ix := range res.Indexes {
		found = found || ix.Name == "ix_person_email"
	}
	if !found {
		t.Fatalf("index ix_person_email lost across the restart (redo position %d, recovered %+v) — #1755/#1774", redo, res.Indexes)
	}
}
