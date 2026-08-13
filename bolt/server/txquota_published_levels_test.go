package server

import (
	"errors"
	"testing"
)

// publishedConcurrencyLevels are the levels CLAUDE.md commits this module to
// measuring and reporting at, under "Reliability and Concurrency Mandates".
// They are written out rather than derived so that raising the published
// contract without revisiting the server defaults fails here.
var publishedConcurrencyLevels = []int{1, 8, 64, 256, 1024}

// TestDefaultQuotaAdmitsEveryPublishedConcurrencyLevel is the rmp #2419 gate.
//
// It asserts the default configuration's BEHAVIOUR — that one principal can
// hold open as many explicit transactions as the highest level the module
// publishes — rather than the value of the constant. Asserting the constant
// would pass on any number at all and would not notice the contract it exists
// to satisfy moving.
//
// Before #2419 this failed at the seventeenth acquisition: the default was 16,
// so a single principal could not reach 64, 256 or 1024 without the embedder
// overriding it first, which every harness in this repository had already had
// to do.
func TestDefaultQuotaAdmitsEveryPublishedConcurrencyLevel(t *testing.T) {
	t.Parallel()
	for _, level := range publishedConcurrencyLevels {
		q := newTxQuota(DefaultMaxOpenTxPerPrincipal)
		for i := 0; i < level; i++ {
			if err := q.acquire("neo4j"); err != nil {
				t.Fatalf("published level %d: the default configuration refused open transaction %d of %d: %v",
					level, i+1, level, err)
			}
		}
		if got := q.openFor("neo4j"); got != level {
			t.Fatalf("published level %d: quota records %d open transactions", level, got)
		}
		for i := 0; i < level; i++ {
			q.release("neo4j")
		}
		if got := q.openFor("neo4j"); got != 0 {
			t.Fatalf("published level %d: after releasing every slot the quota still records %d", level, got)
		}
	}
}

// TestDefaultQuotaStillBoundsAndStillIsolates pins what raising the default did
// NOT give up: the bound is still finite, exceeding it is still a typed refusal
// rather than a crash or a silent admission, and one principal's consumption
// still cannot deny another's. A ceiling that stopped doing any of these would
// have traded the mandate away rather than reconciled it.
func TestDefaultQuotaStillBoundsAndStillIsolates(t *testing.T) {
	t.Parallel()
	if DefaultMaxOpenTxPerPrincipal <= 0 {
		t.Fatalf("the default quota must stay finite and enforcing, got %d", DefaultMaxOpenTxPerPrincipal)
	}
	q := newTxQuota(DefaultMaxOpenTxPerPrincipal)
	for i := 0; i < DefaultMaxOpenTxPerPrincipal; i++ {
		if err := q.acquire("alice"); err != nil {
			t.Fatalf("acquire %d of %d: %v", i+1, DefaultMaxOpenTxPerPrincipal, err)
		}
	}
	err := q.acquire("alice")
	if err == nil {
		t.Fatal("the quota admitted one more than its limit: the bound is not enforced")
	}
	var exceeded *errTxQuotaExceeded
	if !errors.As(err, &exceeded) {
		t.Fatalf("exceeding the quota gave %T, want *errTxQuotaExceeded", err)
	}
	if exceeded.limit != DefaultMaxOpenTxPerPrincipal {
		t.Fatalf("the refusal reports a limit of %d, want %d", exceeded.limit, DefaultMaxOpenTxPerPrincipal)
	}
	// A second principal is unaffected by the first having exhausted its own.
	if err := q.acquire("bob"); err != nil {
		t.Fatalf("a principal at its limit denied a DIFFERENT principal: %v", err)
	}
}
