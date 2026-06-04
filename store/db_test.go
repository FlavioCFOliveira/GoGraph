package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// stack bundles the pieces a WAL-backed store is built from so each test can
// stand the full thing up with one call.
type stack struct {
	dir   string
	wal   *wal.Writer
	store *txn.Store[string, int64]
	cp    *checkpoint.Checkpointer[string, int64]
	g     *lpg.Graph[string, int64]
}

// newStack opens a WAL, a typed store, and a background checkpointer wired the
// way an embedder would (commit-serialiser + mapper codec so the snapshot is
// self-sufficient and the WAL can be truncated), and starts the checkpoint
// goroutine. cancel stops the Start context; tests that close via the composed
// DB do not need it, but it is returned so a test can drive the loop's
// context-cancellation exit too.
func newStack(t *testing.T, cfg checkpoint.Config) (*stack, context.CancelFunc) {
	t.Helper()
	dir := t.TempDir()
	wlog, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	st := txn.NewStoreWithCodec(g, wlog, txn.NewStringCodec())
	if cfg.Dir == "" {
		cfg.Dir = dir
	}
	var unusedMu sync.Mutex
	cp := checkpoint.New(cfg, g, wlog, &unusedMu,
		checkpoint.WithCommitSerialiser[string, int64](st.RunUnderCommitLock),
		checkpoint.WithMapperCodec[string, int64](txn.NewStringCodec()))
	ctx, cancel := context.WithCancel(context.Background())
	cp.Start(ctx)
	return &stack{dir: dir, wal: wlog, store: st, cp: cp, g: g}, cancel
}

// commitEdges commits one transaction per (src,dst,label) triple through the
// store, exercising the WAL append + fsync + in-memory apply path.
func commitEdges(t *testing.T, st *txn.Store[string, int64], edges [][3]string) {
	t.Helper()
	for _, e := range edges {
		tx := st.Begin()
		if err := tx.AddEdge(e[0], e[1], 0); err != nil {
			_ = tx.Rollback()
			t.Fatalf("AddEdge %s->%s: %v", e[0], e[1], err)
		}
		if err := tx.SetEdgeLabel(e[0], e[1], e[2]); err != nil {
			_ = tx.Rollback()
			t.Fatalf("SetEdgeLabel %s->%s: %v", e[0], e[1], err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit %s->%s: %v", e[0], e[1], err)
		}
	}
}

var sampleEdges = [][3]string{
	{"alice", "bob", "KNOWS"},
	{"bob", "carol", "KNOWS"},
	{"carol", "dave", "FOLLOWS"},
}

// TestDBClose_NoLeak_RejectsSubsequentAppend is the primary acceptance test:
// the full stack + a running checkpointer, real writes, then the composed
// store.DB.Close. After Close the checkpoint goroutine must be stopped (the
// package goleak TestMain turns a leak into a failure) AND a subsequent
// wal.Append must be rejected with wal.ErrWriterClosed (the WAL is closed).
//
// Before this change there was no composed Close at all; the assertion is that
// the new API exists and tears the stack down correctly.
func TestDBClose_NoLeak_RejectsSubsequentAppend(t *testing.T) {
	t.Parallel()
	// A fast cadence so the checkpoint goroutine is genuinely doing periodic
	// work (Sync/Truncate against the WAL) concurrently with the writes and the
	// close — this is the window a wrong teardown order would corrupt.
	s, cancel := newStack(t, checkpoint.Config{
		MaxAge:   10 * time.Millisecond,
		Interval: 2 * time.Millisecond,
	})
	defer cancel()

	commitEdges(t, s.store, sampleEdges)

	db := store.New(s.wal, store.WithCheckpointer(s.cp))
	if err := db.CloseCtx(context.Background()); err != nil {
		t.Fatalf("db.Close: %v", err)
	}

	// The WAL is closed: a subsequent append is rejected. This proves Close
	// reached step 3 (and, with the goleak gate, that it reached step 2 first).
	if err := s.wal.Append([]byte("after-close")); err == nil {
		t.Fatal("wal.Append after composed Close succeeded; want ErrWriterClosed")
	} else if !isWriterClosed(err) {
		t.Fatalf("wal.Append after Close = %v; want wal.ErrWriterClosed", err)
	}

	// The checkpointer is stopped: a Trigger returns the stopped sentinel
	// rather than running a checkpoint against the closed WAL.
	if err := s.cp.Trigger(); err == nil {
		t.Fatal("checkpoint.Trigger after composed Close succeeded; want ErrCheckpointerStopped")
	}
	// The stop must NOT have swallowed a WAL error into LastError: a clean
	// composed shutdown stops the loop before the WAL is ever closed, so the
	// loop never touched a closed WAL.
	if le := s.cp.Stats().LastError; le != "" {
		t.Fatalf("checkpointer LastError = %q after composed Close; want empty (loop stopped before WAL close)", le)
	}
}

// TestDBClose_WrongOrder_SurfacesSwallowedError is the ordering negative test.
// It reproduces the bug the composed Close exists to prevent: closing the WAL
// BEFORE stopping the checkpointer leaves the still-running checkpoint loop to
// Sync/Truncate a closed WAL, whose ErrWriterClosed is swallowed into
// Stats.LastError instead of surfacing. The composed store.DB.Close avoids this
// by stopping the loop first (asserted by the sibling positive test, which sees
// an empty LastError); here we drive the wrong order by hand and prove the
// error really is produced and swallowed, so the ordering is load-bearing.
func TestDBClose_WrongOrder_SurfacesSwallowedError(t *testing.T) {
	t.Parallel()
	s, cancel := newStack(t, checkpoint.Config{MaxAge: 0}) // trigger-only, no ticker
	defer cancel()
	// Stop the loop ourselves at the end so this test leaves no goroutine for
	// goleak (we are deliberately NOT using the composed Close here).
	defer s.cp.Stop()

	commitEdges(t, s.store, sampleEdges)

	// WRONG ORDER: close the WAL while the checkpoint loop is still alive.
	if err := s.wal.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	// Now drive a checkpoint through the live loop. It runs Sync on the closed
	// WAL, gets ErrWriterClosed, and records it in Stats.LastError — the
	// swallowed-error behaviour. Trigger itself returns the same error.
	trigErr := s.cp.Trigger()
	if trigErr == nil {
		t.Fatal("checkpoint.Trigger on a closed WAL returned nil; want a WAL-closed error")
	}
	if !isWriterClosed(trigErr) {
		t.Fatalf("checkpoint.Trigger error = %v; want wal.ErrWriterClosed", trigErr)
	}
	if le := s.cp.Stats().LastError; !strings.Contains(le, wal.ErrWriterClosed.Error()) {
		t.Fatalf("checkpointer LastError = %q; want it to contain %q (the swallowed WAL-closed error)",
			le, wal.ErrWriterClosed.Error())
	}
}

// TestDBClose_Idempotent_Concurrent verifies that Close runs the teardown once
// and is safe under concurrent and repeated calls: every caller observes the
// same (nil) error and no double WAL close leaks an ErrWriterClosed to the
// caller.
func TestDBClose_Idempotent_Concurrent(t *testing.T) {
	t.Parallel()
	s, cancel := newStack(t, checkpoint.Config{MaxAge: 0})
	defer cancel()
	commitEdges(t, s.store, sampleEdges)

	db := store.New(s.wal, store.WithCheckpointer(s.cp))

	const callers = 8
	var wg sync.WaitGroup
	errs := make([]error, callers)
	wg.Add(callers)
	for i := range callers {
		go func(i int) {
			defer wg.Done()
			// Mix the io.Closer Close() and the ctx-aware CloseCtx so both
			// methods are proven to funnel through the same sync.Once: only one
			// teardown runs no matter which entry point or how many racers.
			if i%2 == 0 {
				errs[i] = db.Close()
			} else {
				errs[i] = db.CloseCtx(context.Background())
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent Close caller %d = %v; want nil (idempotent, no double-close error)", i, err)
		}
	}
	// A further serial call still returns the same cached result.
	if err := db.CloseCtx(context.Background()); err != nil {
		t.Fatalf("repeat Close = %v; want nil", err)
	}
}

// TestDBClose_FinalCheckpoint takes a final checkpoint on Close and verifies it
// ran (one more checkpoint than without the option) while the teardown still
// rejects a later append.
func TestDBClose_FinalCheckpoint(t *testing.T) {
	t.Parallel()
	s, cancel := newStack(t, checkpoint.Config{MaxAge: 0}) // no ticker: only the final checkpoint fires
	defer cancel()
	commitEdges(t, s.store, sampleEdges)

	before := s.cp.Stats().Checkpoints

	db := store.New(s.wal, store.WithCheckpointer(s.cp), store.WithFinalCheckpoint())
	if err := db.CloseCtx(context.Background()); err != nil {
		t.Fatalf("db.Close: %v", err)
	}

	after := s.cp.Stats().Checkpoints
	if after != before+1 {
		t.Fatalf("checkpoints after final-checkpoint Close = %d; want %d (exactly one final checkpoint)", after, before+1)
	}
	if err := s.wal.Append([]byte("x")); !isWriterClosed(err) {
		t.Fatalf("append after final-checkpoint Close = %v; want wal.ErrWriterClosed", err)
	}
}

// TestDBClose_NoCheckpointer covers the WAL-only DB: Close just closes the WAL.
func TestDBClose_NoCheckpointer(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	wlog, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	db := store.New(wlog) // no checkpointer
	if err := db.CloseCtx(context.Background()); err != nil {
		t.Fatalf("db.Close (WAL-only): %v", err)
	}
	if err := wlog.Append([]byte("x")); !isWriterClosed(err) {
		t.Fatalf("append after WAL-only Close = %v; want wal.ErrWriterClosed", err)
	}
}

// isWriterClosed reports whether err is wal.ErrWriterClosed.
func isWriterClosed(err error) bool {
	return errors.Is(err, wal.ErrWriterClosed)
}
