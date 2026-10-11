package cypher_test

// engines_b2_wal_close_3095_test.go — regression gate for rmp #3095.
//
// The WAL-backed engine of the enginesB2 harness opened a wal.Writer and never
// closed it unless the test called recover. wal.Open starts a background
// segment preparer that creates the next segment file in dir/wal.d; only
// wal.Writer.Close waits for it. Under CPU load the preparer of a finished
// test was still inside createSegment when t.TempDir's RemoveAll emptied
// wal.d, so RemoveAll failed with "wal.d: directory not empty".
//
// The gate is deterministic: once a subtest that built the WAL-backed engine
// has returned (its cleanups have run), its writer must already be closed, so
// a further Close reports wal.ErrWriterClosed. On the old harness that Close
// succeeded, because nothing had closed the writer.
//
// Layer: short. goleak-clean (the writer is closed by the harness cleanup).

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

func TestEnginesB2WALClosedWhenTestEnds_3095(t *testing.T) {
	t.Parallel()
	var w *wal.Writer
	t.Run("wal", func(t *testing.T) {
		e := enginesB2()["wal"](t)
		if e.wlog == nil {
			t.Fatal("WAL-backed engine exposes no writer")
		}
		w = e.wlog
		if _, err := e.eng.RunAny(context.Background(), "CREATE (:N {v: 1})", nil); err != nil {
			t.Fatalf("CREATE: %v", err)
		}
	})
	if w == nil {
		t.Fatal("subtest did not build the WAL-backed engine")
	}
	err := w.Close()
	if !errors.Is(err, wal.ErrWriterClosed) {
		t.Fatalf("writer still open after its test ended: Close = %v, want %v", err, wal.ErrWriterClosed)
	}
}
