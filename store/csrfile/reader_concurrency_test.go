package csrfile

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// TestReader_ConcurrentClose verifies that many goroutines racing to
// Close the same Reader is race-free and idempotent: the mapping is
// unmapped exactly once and every Close returns nil. Run under -race
// this also proves the writes to r.mm are serialised.
func TestReader_ConcurrentClose(t *testing.T) {
	t.Parallel()
	path, _ := writeFixture(t)
	r, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	const goroutines = 16
	var (
		wg   sync.WaitGroup
		errc = make([]error, goroutines)
	)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			errc[i] = r.Close()
		}()
	}
	wg.Wait()

	for i, e := range errc {
		if e != nil {
			t.Errorf("Close[%d] returned %v, want nil", i, e)
		}
	}
	if r.mm != nil {
		t.Fatal("mapping not released after concurrent Close")
	}
}

// TestReader_ReadBlocksClose proves the core ordering guarantee: while
// a Read callback is in flight, a concurrent Close must not unmap.
// The callback parks until released; Close is observed to complete
// only after the callback has returned.
func TestReader_ReadBlocksClose(t *testing.T) {
	t.Parallel()
	path, _ := writeFixture(t)
	r, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	inRead := make(chan struct{}) // closed once the callback is running
	release := make(chan struct{})
	var readReturned, closeReturned atomic.Int64

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		rerr := r.Read(func(verts []uint64, edges []graph.NodeID, _ []byte) error {
			close(inRead)
			<-release // hold the read lock open
			// Touch the mapping here: it MUST still be valid, because
			// a racing Close cannot have unmapped while we hold the
			// read lock.
			_ = len(verts)
			_ = len(edges)
			readReturned.Store(time.Now().UnixNano())
			return nil
		})
		if rerr != nil {
			t.Errorf("Read returned %v, want nil", rerr)
		}
	}()

	<-inRead // callback is parked, holding the read lock
	go func() {
		defer wg.Done()
		if cerr := r.Close(); cerr != nil {
			t.Errorf("Close: %v", cerr)
		}
		closeReturned.Store(time.Now().UnixNano())
	}()

	// Give Close a chance to (wrongly) proceed if the lock did not
	// hold it. It must still be blocked because the callback parks.
	time.Sleep(20 * time.Millisecond)
	if closeReturned.Load() != 0 {
		t.Fatal("Close completed while a Read callback was still in flight")
	}

	close(release) // let the callback return; Close may now proceed
	wg.Wait()

	if readReturned.Load() == 0 || closeReturned.Load() == 0 {
		t.Fatalf("missing timestamps: read=%d close=%d", readReturned.Load(), closeReturned.Load())
	}
	if closeReturned.Load() < readReturned.Load() {
		t.Errorf("Close returned (%d) before Read callback finished (%d)",
			closeReturned.Load(), readReturned.Load())
	}
}

// TestReader_ReadAfterClose verifies that Read on a closed Reader
// returns ErrReaderClosed without invoking the callback and without
// touching freed memory.
func TestReader_ReadAfterClose(t *testing.T) {
	t.Parallel()
	path, _ := writeFixture(t)
	r, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	called := false
	rerr := r.Read(func([]uint64, []graph.NodeID, []byte) error {
		called = true
		return nil
	})
	if called {
		t.Error("Read invoked the callback on a closed Reader")
	}
	if !errors.Is(rerr, ErrReaderClosed) {
		t.Errorf("Read after Close: got %v, want ErrReaderClosed", rerr)
	}
}

// TestReader_ReadReleasesLockOnPanic verifies that a panic inside the
// Read callback still releases the read lock, so a subsequent Close
// (which needs the write lock) does not deadlock.
func TestReader_ReadReleasesLockOnPanic(t *testing.T) {
	t.Parallel()
	path, _ := writeFixture(t)
	r, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected panic to propagate out of Read")
			}
		}()
		_ = r.Read(func([]uint64, []graph.NodeID, []byte) error {
			panic("boom")
		})
	}()

	// If the read lock leaked, this Close would block forever; the
	// test would hang and be killed by the go test timeout.
	done := make(chan error, 1)
	go func() { done <- r.Close() }()
	select {
	case cerr := <-done:
		if cerr != nil {
			t.Errorf("Close after panicking Read: %v", cerr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close deadlocked after a panicking Read callback")
	}
}

// TestReader_ReadObservesData is a sanity check that Read passes the
// same slices the bare accessors expose, so the migration is a pure
// safety wrapper and not an algorithm change.
func TestReader_ReadObservesData(t *testing.T) {
	t.Parallel()
	path, _ := writeFixture(t)
	r, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = r.Close() }()

	wantV, wantE, wantW := r.Vertices(), r.Edges(), r.WeightsRaw()
	rerr := r.Read(func(verts []uint64, edges []graph.NodeID, weights []byte) error {
		if len(verts) != len(wantV) || len(edges) != len(wantE) || len(weights) != len(wantW) {
			t.Errorf("Read slices differ from accessors: v=%d/%d e=%d/%d w=%d/%d",
				len(verts), len(wantV), len(edges), len(wantE), len(weights), len(wantW))
		}
		return nil
	})
	if rerr != nil {
		t.Fatalf("Read: %v", rerr)
	}
}
