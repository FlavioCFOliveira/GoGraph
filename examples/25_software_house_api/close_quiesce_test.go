package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// openSeededDir opens a store in dir and loads the fixture. Unlike
// openSeeded it takes an explicit dir (so the test can reopen the same path)
// and registers NO cleanup Close: these tests close the store themselves as
// part of what they exercise.
func openSeededDir(t *testing.T, dir string) *dataStore {
	t.Helper()
	ds, err := openStore(context.Background(), dir)
	if err != nil {
		t.Fatalf("openStore %q: %v", dir, err)
	}
	if _, err := seedFixture(ds.txnStore); err != nil {
		t.Fatalf("seedFixture: %v", err)
	}
	return ds
}

// runWriteThroughStore mirrors what handleQuery does for a write: it takes
// the store's write hold, runs the statement through the engine, drains and
// closes the result, then releases the hold (Result.Close runs before the
// hold is released, exactly as in the handler). It returns the engine error,
// or ErrStoreClosed when the store was already closed.
func runWriteThroughStore(ds *dataStore, query string) error {
	release, err := ds.acquire(true)
	if err != nil {
		return err
	}
	defer release()

	res, err := ds.engine.RunAny(context.Background(), query, nil)
	if err != nil {
		return err
	}
	defer func() { _ = res.Close() }()
	for res.Next() {
	}
	return res.Err()
}

// developerCountFresh reopens dir from durable state only (snapshot + WAL
// replay) and returns the Developer node count. It proves what actually
// reached the WAL, independent of the in-memory graph of the closed store.
func developerCountFresh(t *testing.T, dir string) int {
	t.Helper()
	ds, err := openStore(context.Background(), dir)
	if err != nil {
		t.Fatalf("reopen %q: %v", dir, err)
	}
	defer func() { _ = ds.Close() }()
	n, _ := queryRows(t, ds, "MATCH (n:Developer) RETURN n", nil)
	return n
}

// TestCloseQuiescesInFlightWrite is the regression guard for task #1291: a
// concurrent dataStore.Close must not close the WAL underneath an in-flight
// engine write. It parks one write inside its serialisation hold, then fires
// Close on another goroutine and asserts Close BLOCKS until the write
// finishes — so the write either fully commits (its mutation reaches the
// WAL) before the WAL is released, never "applied in memory then lost".
//
// With the pre-fix Close (which closed the WAL with no hold), Close did not
// wait for the parked write: it returned while the write was still mid-flight
// and could close the WAL underneath the commit. The blocking assertion below
// fails on that Close. Under -race the unsynchronised WAL access is also
// flagged.
func TestCloseQuiescesInFlightWrite(t *testing.T) {
	dir := t.TempDir()
	ds := openSeededDir(t, dir) // 6 seeded developers, no t.Cleanup Close

	started := make(chan struct{})
	proceed := make(chan struct{})
	// Park the FIRST write that takes the hold: signal that it holds the hold,
	// then block until the test releases it. A sync.Once makes it one-shot so
	// only the targeted write parks (no other acquire runs in this test).
	var once sync.Once
	ds.beforeEngine = func() {
		once.Do(func() {
			close(started)
			<-proceed
		})
	}

	writeErr := make(chan error, 1)
	go func() {
		writeErr <- runWriteThroughStore(ds,
			"CREATE (d:Developer:People {key:'dev:zoe', name:'Zoe'})")
	}()

	// The write now holds the exclusive hold and is parked before the engine
	// call (so nothing is applied in memory yet).
	<-started

	closeDone := make(chan error, 1)
	go func() { closeDone <- ds.Close() }()

	// Close must NOT complete while the write holds the hold. If it does, it
	// closed the WAL underneath the in-flight write — the exact bug.
	select {
	case err := <-closeDone:
		close(proceed) // unblock the parked goroutine so the test can exit cleanly
		t.Fatalf("Close returned while a write held the store hold (err=%v): "+
			"Close did not quiesce the in-flight write", err)
	case <-time.After(200 * time.Millisecond):
		// Expected: Close is blocked on the exclusive hold.
	}

	// Release the parked write. It must run and commit to the WAL fully
	// BEFORE Close can take the hold and release the WAL.
	close(proceed)

	if err := <-writeErr; err != nil {
		t.Fatalf("in-flight write did not fully commit: %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("Close after quiesced write: %v", err)
	}

	// The write reached the WAL: a fresh durable reopen sees 7 developers
	// (6 seeded + dev:zoe). Anything less would mean the write applied in
	// memory but was lost from the WAL.
	if got := developerCountFresh(t, dir); got != 7 {
		t.Errorf("Developer count after durable reopen = %d, want 7 "+
			"(committed write was lost from the WAL at Close)", got)
	}
}

// TestCloseRejectsLateWrites asserts the binary shutdown outcome under a
// concurrent Close and a burst of writers: each write either FULLY commits
// (its node survives a durable reopen) or is cleanly rejected with
// ErrStoreClosed BEFORE applying — never applied-in-memory-then-lost. Run
// under -race it also asserts Close never races a concurrent write on the
// WAL.
func TestCloseRejectsLateWrites(t *testing.T) {
	dir := t.TempDir()
	ds := openSeededDir(t, dir) // 6 seeded developers

	const nWriters = 8
	var committed int64 // writes that returned success (must survive reopen)

	var wg sync.WaitGroup
	for i := 0; i < nWriters; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			key := fmt.Sprintf("dev:late%d", id)
			err := runWriteThroughStore(ds,
				fmt.Sprintf("CREATE (d:Developer:People {key:'%s'})", key))
			switch {
			case err == nil:
				atomic.AddInt64(&committed, 1)
			case errors.Is(err, ErrStoreClosed):
				// Cleanly rejected before applying: acceptable.
			default:
				t.Errorf("writer %d: unexpected error %v", id, err)
			}
		}(i)
	}

	// Race a Close against the burst.
	closeErr := ds.Close()
	if closeErr != nil {
		t.Fatalf("Close: %v", closeErr)
	}
	wg.Wait()

	// Every write that reported success must be durable; the rejected ones
	// must NOT be. So the durable Developer count is exactly 6 + committed.
	want := 6 + int(atomic.LoadInt64(&committed))
	if got := developerCountFresh(t, dir); got != want {
		t.Errorf("durable Developer count = %d, want %d (6 seeded + %d committed): "+
			"a write applied in memory but was lost from the WAL, or a rejected "+
			"write leaked into the WAL", got, want, committed)
	}
}
