package checkpoint_test

// constraints_unwired_test.go — gate for #1464: when the embedder declares a
// schema constraint but forgets checkpoint.WithConstraintSpecs, a
// WAL-truncating checkpoint must NOT silently drop the constraint.
//
// Before the fix, runCheckpoint computed needConstraints from
// len(c.constraintsFn()) — which is empty when constraintsFn is nil — so the
// snapshot was judged self-sufficient, the WAL prefix that declared the
// constraint was truncated, and the UNIQUE constraint silently vanished on the
// next reopen. After the fix, needConstraints is sourced from
// Graph.HasConstraints (the engine-maintained truth), so a missing
// constraints.bin forces the existing fail-safe branch: the WAL is retained
// (no truncation) and recovery replays the constraint op on top of the
// snapshot. Durability and Consistency hold; the operator sees the degraded
// mode via the store.checkpoint.truncate_skipped_not_self_sufficient metric.
//
// Layer: short. NOT parallel: it installs a process-global metrics backend.
// goleak-clean (checkpointer, engines, and WAL are local and closed).

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/metrics"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// countingMetrics is a minimal metrics.Backend that records counter totals so
// the test can assert the fail-safe metric fired. Defined locally (rather than
// reusing the package-internal countingBackend) because this test lives in the
// external checkpoint_test package so it can import cypher without a cycle.
type countingMetrics struct {
	mu      sync.Mutex
	counter map[string]uint64
}

func (c *countingMetrics) IncCounter(name string, delta uint64) {
	c.mu.Lock()
	c.counter[name] += delta
	c.mu.Unlock()
}
func (c *countingMetrics) ObserveLatency(string, time.Duration) {}
func (c *countingMetrics) count(name string) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counter[name]
}

// TestCheckpointer_ConstraintsPresent_SpecsUnwired_SkipsTruncation is the
// #1464 gate: constraints declared, WithConstraintSpecs NOT wired, mapper side
// self-sufficient (WithMapperCodec) so the ONLY missing snapshot component is
// constraints.bin. The checkpoint must retain the WAL and the constraint must
// survive the reopen.
func TestCheckpointer_ConstraintsPresent_SpecsUnwired_SkipsTruncation(t *testing.T) {
	// Not parallel: process-global metrics backend.
	mb := &countingMetrics{counter: map[string]uint64{}}
	metrics.SetBackend(mb)
	defer metrics.SetBackend(nil)

	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	// --- Process 1: declare the constraint, insert one node, checkpoint with
	// the constraint source DELIBERATELY UNWIRED.
	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	store := txn.NewStoreWithOptions[string, float64](g, w, csStoreOpts())
	eng := cypher.NewEngineWithStore(store)

	if err := csRunOne(t, eng, `CREATE CONSTRAINT u_name ON (n:Person) ASSERT n.name IS UNIQUE`); err != nil {
		t.Fatalf("CREATE CONSTRAINT: %v", err)
	}
	if err := csRunOne(t, eng, `CREATE (n:Person {name: 'alice'})`); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	var unusedMu sync.Mutex
	cp := checkpoint.New[string, float64](checkpoint.Config{Dir: dir}, g, w, &unusedMu,
		checkpoint.WithCommitSerialiser[string, float64](store.RunUnderCommitLock),
		checkpoint.WithMapperCodec[string, float64](store.Codec()),
		// NO WithConstraintSpecs — this is the misconfiguration #1464 guards.
	)
	cctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(cctx)
	if terr := cp.Trigger(); terr != nil {
		t.Fatalf("checkpoint Trigger: %v", terr)
	}
	cp.Stop()

	// Fail-safe: the WAL must NOT have been truncated (the constraint op is the
	// reason the snapshot is not self-sufficient), and the degrade must be
	// surfaced via the metric.
	if got := cp.Stats().WALTruncBytes; got != 0 {
		t.Fatalf("WAL was truncated (WALTruncBytes = %d) despite an un-persisted constraint — #1464 fail-safe did not engage; constraints would be lost on reopen", got)
	}
	if got := mb.count("store.checkpoint.truncate_skipped_not_self_sufficient"); got != 1 {
		t.Fatalf("truncate-skipped metric = %d, want 1 — the degraded mode was not surfaced", got)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	// --- Process 2: restart from disk. Because the WAL was retained, recovery
	// replays the CREATE CONSTRAINT op and the constraint is still enforced.
	res, err := recovery.Open[string, float64](dir, csRecOpts())
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	if len(res.Constraints) == 0 {
		t.Fatalf("recovery.Result.Constraints is empty after checkpoint+restart; the UNIQUE constraint was lost (#1464 regression)")
	}
	w2, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("reopen wal.Open: %v", err)
	}
	defer func() {
		if cerr := w2.Close(); cerr != nil {
			t.Errorf("wal.Close (reopen): %v", cerr)
		}
	}()
	store2 := txn.NewStoreWithOptions[string, float64](res.Graph, w2, csStoreOpts())
	eng2 := cypher.NewEngineWithStoreAndConstraints(store2, res.Constraints)

	dupErr := csRunOne(t, eng2, `CREATE (n:Person {name: 'alice'})`)
	if dupErr == nil {
		t.Fatalf("duplicate insert after checkpoint+restart was accepted; the UNIQUE constraint did not survive (#1464)")
	}
	if !errors.Is(dupErr, exec.ErrConstraintViolation) {
		t.Fatalf("duplicate insert after checkpoint+restart: got %v, want one wrapping exec.ErrConstraintViolation", dupErr)
	}
	if err := csRunOne(t, eng2, `CREATE (n:Person {name: 'bob'})`); err != nil {
		t.Fatalf("distinct insert after checkpoint+restart rejected: %v", err)
	}
}
