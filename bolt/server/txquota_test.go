package server

// txquota_test.go — rmp #2175, unit level.
//
// txQuota caps concurrently-open explicit transactions per principal. These
// tests cover the arithmetic and, importantly, the BOUNDEDNESS of its map: the
// contract is that its size tracks open transactions, not principals ever seen,
// so a client cannot grow it by authenticating under many names.

import (
	"errors"
	"strconv"
	"sync"
	"testing"
)

func TestTxQuota_AcquireUpToLimitThenRejects(t *testing.T) {
	t.Parallel()
	q := newTxQuota(3)
	for i := 0; i < 3; i++ {
		if err := q.acquire("alice"); err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
	}
	err := q.acquire("alice")
	if err == nil {
		t.Fatal("the fourth acquire succeeded against a limit of 3")
	}
	var qe *errTxQuotaExceeded
	if !errors.As(err, &qe) {
		t.Fatalf("error is %T, want *errTxQuotaExceeded", err)
	}
	if qe.limit != 3 || qe.principal != "alice" {
		t.Fatalf("error carries principal %q limit %d, want alice/3", qe.principal, qe.limit)
	}
	if got := q.openFor("alice"); got != 3 {
		t.Fatalf("openFor = %d, want 3 (a rejected acquire must not count)", got)
	}
}

func TestTxQuota_PrincipalsAreIndependent(t *testing.T) {
	t.Parallel()
	q := newTxQuota(1)
	if err := q.acquire("alice"); err != nil {
		t.Fatalf("alice: %v", err)
	}
	if err := q.acquire("bob"); err != nil {
		t.Fatalf("bob must not be affected by alice's slot: %v", err)
	}
	if err := q.acquire("alice"); err == nil {
		t.Fatal("alice exceeded her limit of 1")
	}
}

func TestTxQuota_ReleaseFreesASlot(t *testing.T) {
	t.Parallel()
	q := newTxQuota(1)
	if err := q.acquire("alice"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	q.release("alice")
	if err := q.acquire("alice"); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
}

// TestTxQuota_ReleaseIsIdempotent matters because one transaction may be torn
// down by several paths — a handler closing it, then the connection-teardown
// rollback — and an over-release would hand out slots the limit does not allow.
func TestTxQuota_ReleaseIsIdempotent(t *testing.T) {
	t.Parallel()
	q := newTxQuota(2)
	if err := q.acquire("alice"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	q.release("alice")
	q.release("alice")
	q.release("alice")
	if got := q.openFor("alice"); got != 0 {
		t.Fatalf("openFor = %d after over-release, want 0", got)
	}
	// The limit must still be exactly 2, not inflated by the extra releases.
	for i := 0; i < 2; i++ {
		if err := q.acquire("alice"); err != nil {
			t.Fatalf("acquire %d after over-release: %v", i, err)
		}
	}
	if err := q.acquire("alice"); err == nil {
		t.Fatal("over-release inflated the limit")
	}
}

// TestTxQuota_MapIsBoundedByOpenTransactions pins the bounded-resources
// contract: the map must not retain an entry for a principal with no open
// transaction, or a client could grow it without limit by authenticating under
// many names.
func TestTxQuota_MapIsBoundedByOpenTransactions(t *testing.T) {
	t.Parallel()
	q := newTxQuota(4)
	const principals = 5000
	for i := 0; i < principals; i++ {
		p := "p" + strconv.Itoa(i)
		if err := q.acquire(p); err != nil {
			t.Fatalf("acquire %s: %v", p, err)
		}
		q.release(p)
	}
	if got := q.tracked(); got != 0 {
		t.Fatalf("tracked = %d after %d acquire/release pairs, want 0 — the map retains "+
			"principals with no open transaction", got, principals)
	}

	// With transactions genuinely open, the map is proportional to those.
	for i := 0; i < 10; i++ {
		if err := q.acquire("p" + strconv.Itoa(i)); err != nil {
			t.Fatalf("acquire: %v", err)
		}
	}
	if got := q.tracked(); got != 10 {
		t.Fatalf("tracked = %d, want 10", got)
	}
}

func TestTxQuota_DisabledByNonPositiveLimit(t *testing.T) {
	t.Parallel()
	for _, limit := range []int{0, -1} {
		q := newTxQuota(limit)
		for i := 0; i < 1000; i++ {
			if err := q.acquire("alice"); err != nil {
				t.Fatalf("limit %d: acquire %d rejected: %v", limit, i, err)
			}
		}
		if got := q.tracked(); got != 0 {
			t.Fatalf("limit %d: a disabled quota tracked %d principals, want 0", limit, got)
		}
	}
}

// TestTxQuota_ConcurrentAcquireNeverExceedsLimit is the race-detector check: the
// quota is shared by every connection, so its arithmetic must hold under
// concurrent acquire/release.
func TestTxQuota_ConcurrentAcquireNeverExceedsLimit(t *testing.T) {
	t.Parallel()
	const limit = 8
	q := newTxQuota(limit)

	var peak int
	var live int
	var guard sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := 0; r < 200; r++ {
				if err := q.acquire("alice"); err != nil {
					continue // at the cap; that is the expected outcome
				}
				guard.Lock()
				live++
				if live > peak {
					peak = live
				}
				guard.Unlock()

				guard.Lock()
				live--
				guard.Unlock()
				q.release("alice")
			}
		}()
	}
	wg.Wait()

	if peak > limit {
		t.Fatalf("observed %d concurrently held slots, want at most %d", peak, limit)
	}
	if got := q.openFor("alice"); got != 0 {
		t.Fatalf("openFor = %d after all releases, want 0", got)
	}
}
