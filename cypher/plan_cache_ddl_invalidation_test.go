package cypher_test

// plan_cache_ddl_invalidation_test.go — T933
//
// Verifies that successful DDL operations (CREATE INDEX, DROP INDEX,
// CREATE CONSTRAINT, DROP CONSTRAINT) flush the Engine's plan cache so
// subsequent queries re-plan against the new schema, and that IF [NOT]
// EXISTS silent no-ops do NOT invalidate.
//
// The invalidation metric is observed via a custom metrics.Backend
// installed with metrics.SetBackend; tests that install a backend are
// not parallel because the backend is process-global.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"gograph/cypher"
	"gograph/cypher/exec"
	"gograph/graph/adjlist"
	"gograph/graph/index"
	"gograph/graph/lpg"
	cmetrics "gograph/internal/metrics"
)

// invalidationProbe records plan-cache metrics so DDL-invalidation tests
// can assert exactly-once invalidation semantics.
//
// The global metrics backend is swapped atomically; tests that install
// this probe MUST NOT run in parallel.
type invalidationProbe struct {
	hits          atomic.Uint64
	misses        atomic.Uint64
	evictions     atomic.Uint64
	invalidations atomic.Uint64
}

func (p *invalidationProbe) IncCounter(name string, delta uint64) {
	switch name {
	case "cypher.plan_cache.hits":
		p.hits.Add(delta)
	case "cypher.plan_cache.misses":
		p.misses.Add(delta)
	case "cypher.plan_cache.evictions":
		p.evictions.Add(delta)
	case "cypher.plan_cache.invalidations":
		p.invalidations.Add(delta)
	}
}

func (p *invalidationProbe) ObserveLatency(string, time.Duration) {}

// installInvalidationProbe swaps in a fresh probe and registers cleanup
// to restore the no-op backend.
func installInvalidationProbe(t *testing.T) *invalidationProbe {
	t.Helper()
	p := &invalidationProbe{}
	cmetrics.SetBackend(p)
	t.Cleanup(func() { cmetrics.SetBackend(nil) })
	return p
}

// newPersonEngine builds an empty Engine over a fresh graph. The Person
// label and name property are introduced by the test queries; the graph
// itself starts empty so each sub-test is fully self-contained.
func newPersonEngine(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	return cypher.NewEngine(g)
}

// runDDLQuery executes q against eng and drains the result, failing on
// error. Used to populate the plan cache without examining rows.
func runDDLQuery(t *testing.T, eng *cypher.Engine, q string) {
	t.Helper()
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run %q: %v", q, err)
	}
	for res.Next() {
		_ = res.Record()
	}
	if err := res.Err(); err != nil {
		t.Fatalf("iteration error on %q: %v", q, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("Close on %q: %v", q, err)
	}
}

// TestPlanCacheDDLInvalidation verifies that successful DDL operations
// invalidate the Engine plan cache while IF [NOT] EXISTS silent no-ops
// do not. Sub-tests cover the four DDL operators and the silent-branch
// negative case.
func TestPlanCacheDDLInvalidation(t *testing.T) {
	t.Run("CreateIndex_InvalidatesCache", testCreateIndexInvalidatesCache)
	t.Run("DropIndex_InvalidatesCache", testDropIndexInvalidatesCache)
	t.Run("CreateConstraintUnique_InvalidatesCache", testCreateConstraintUniqueInvalidatesCache)
	t.Run("CreateConstraintNotNull_InvalidatesCache", testCreateConstraintNotNullInvalidatesCache)
	t.Run("DropConstraintUnique_InvalidatesCache", testDropConstraintUniqueInvalidatesCache)
	t.Run("DropConstraintNotNull_InvalidatesCache", testDropConstraintNotNullInvalidatesCache)
	t.Run("CreateIndexIfNotExistsNoOp_DoesNotInvalidate", testCreateIndexIfNotExistsNoOpDoesNotInvalidate)
	t.Run("DropIndexIfExistsNoOp_DoesNotInvalidate", testDropIndexIfExistsNoOpDoesNotInvalidate)
	t.Run("ClearPlanCache_DirectCall_EmitsCounter", testClearPlanCacheDirectCallEmitsCounter)
}

// testCreateIndexInvalidatesCache asserts that CREATE INDEX clears the
// cache: query before CREATE → miss; same query → hit; CREATE INDEX;
// same query → miss (re-planned).
func testCreateIndexInvalidatesCache(t *testing.T) {
	probe := installInvalidationProbe(t)
	eng := newPersonEngine(t)
	const q = `MATCH (n:Person {name: 'alice'}) RETURN n`

	// 1. First run — miss; plan inserted.
	runDDLQuery(t, eng, q)
	if got := probe.misses.Load(); got != 1 {
		t.Fatalf("after first run: misses = %d, want 1", got)
	}
	if got := probe.hits.Load(); got != 0 {
		t.Fatalf("after first run: hits = %d, want 0", got)
	}

	// 2. Second run — hit; cache reused.
	runDDLQuery(t, eng, q)
	if got := probe.hits.Load(); got != 1 {
		t.Fatalf("after second run: hits = %d, want 1", got)
	}

	// 3. CREATE INDEX — must invalidate.
	runDDLQuery(t, eng, `CREATE INDEX person_name FOR (n:Person) ON (n.name)`)
	if got := probe.invalidations.Load(); got != 1 {
		t.Fatalf("after CREATE INDEX: invalidations = %d, want 1", got)
	}

	// 4. Third run — must miss again (cache was flushed).
	prevMisses := probe.misses.Load()
	runDDLQuery(t, eng, q)
	if got := probe.misses.Load(); got != prevMisses+1 {
		t.Fatalf("after third run: misses = %d, want %d (cache should have been invalidated)",
			got, prevMisses+1)
	}
}

// testDropIndexInvalidatesCache asserts that DROP INDEX clears the
// cache. The setup CREATE INDEX itself also invalidates (counter = 1);
// the test then warms the cache and expects DROP to push it to 2.
func testDropIndexInvalidatesCache(t *testing.T) {
	probe := installInvalidationProbe(t)
	eng := newPersonEngine(t)
	const q = `MATCH (n:Person {name: 'bob'}) RETURN n`

	// Setup: create an index so we have something to drop.
	runDDLQuery(t, eng, `CREATE INDEX person_name FOR (n:Person) ON (n.name)`)
	if got := probe.invalidations.Load(); got != 1 {
		t.Fatalf("setup CREATE INDEX: invalidations = %d, want 1", got)
	}

	// Warm the cache.
	runDDLQuery(t, eng, q) // miss
	runDDLQuery(t, eng, q) // hit
	prevHits := probe.hits.Load()
	if prevHits < 1 {
		t.Fatalf("warm-up: hits = %d, want >= 1", prevHits)
	}

	// DROP INDEX — must invalidate.
	runDDLQuery(t, eng, `DROP INDEX person_name`)
	if got := probe.invalidations.Load(); got != 2 {
		t.Fatalf("after DROP INDEX: invalidations = %d, want 2", got)
	}

	// Re-running the original query must miss again.
	prevMisses := probe.misses.Load()
	runDDLQuery(t, eng, q)
	if got := probe.misses.Load(); got != prevMisses+1 {
		t.Fatalf("after DROP INDEX, third run: misses = %d, want %d", got, prevMisses+1)
	}
}

// testCreateConstraintUniqueInvalidatesCache asserts that CREATE
// CONSTRAINT (UNIQUE) clears the cache.
func testCreateConstraintUniqueInvalidatesCache(t *testing.T) {
	probe := installInvalidationProbe(t)
	eng := newPersonEngine(t)
	const q = `MATCH (n:Person {name: 'carol'}) RETURN n`

	runDDLQuery(t, eng, q) // miss
	runDDLQuery(t, eng, q) // hit
	if got := probe.invalidations.Load(); got != 0 {
		t.Fatalf("before DDL: invalidations = %d, want 0", got)
	}

	runDDLQuery(t, eng, `CREATE CONSTRAINT person_email_uniq ON (n:Person) ASSERT n.email IS UNIQUE`)
	if got := probe.invalidations.Load(); got != 1 {
		t.Fatalf("after CREATE CONSTRAINT UNIQUE: invalidations = %d, want 1", got)
	}

	prevMisses := probe.misses.Load()
	runDDLQuery(t, eng, q)
	if got := probe.misses.Load(); got != prevMisses+1 {
		t.Fatalf("after CREATE CONSTRAINT UNIQUE re-run: misses = %d, want %d",
			got, prevMisses+1)
	}
}

// testCreateConstraintNotNullInvalidatesCache asserts that CREATE
// CONSTRAINT (NOT NULL) clears the cache.
func testCreateConstraintNotNullInvalidatesCache(t *testing.T) {
	probe := installInvalidationProbe(t)
	eng := newPersonEngine(t)
	const q = `MATCH (n:Person {name: 'dave'}) RETURN n`

	runDDLQuery(t, eng, q) // miss
	runDDLQuery(t, eng, q) // hit
	if got := probe.invalidations.Load(); got != 0 {
		t.Fatalf("before DDL: invalidations = %d, want 0", got)
	}

	runDDLQuery(t, eng, `CREATE CONSTRAINT person_name_nn ON (n:Person) ASSERT n.name IS NOT NULL`)
	if got := probe.invalidations.Load(); got != 1 {
		t.Fatalf("after CREATE CONSTRAINT NOT NULL: invalidations = %d, want 1", got)
	}

	prevMisses := probe.misses.Load()
	runDDLQuery(t, eng, q)
	if got := probe.misses.Load(); got != prevMisses+1 {
		t.Fatalf("after CREATE CONSTRAINT NOT NULL re-run: misses = %d, want %d",
			got, prevMisses+1)
	}
}

// testDropConstraintUniqueInvalidatesCache exercises the DropConstraint
// operator directly with explicit label+property so the backing index
// lookup succeeds. The Cypher-level "DROP CONSTRAINT name" cannot reach
// the real-drop path because the IR carries an empty (label, prop)
// pair (documented limitation in cypher/drop_constraint_test.go).
func testDropConstraintUniqueInvalidatesCache(t *testing.T) {
	probe := installInvalidationProbe(t)
	eng := newPersonEngine(t)

	// Set up a UNIQUE constraint via Cypher so the backing index, the
	// registry entry, and the Engine cache are all engaged.
	runDDLQuery(t, eng, `CREATE CONSTRAINT person_email_uniq ON (n:Person) ASSERT n.email IS UNIQUE`)
	if got := probe.invalidations.Load(); got != 1 {
		t.Fatalf("setup CREATE CONSTRAINT: invalidations = %d, want 1", got)
	}

	// Warm the cache so the DROP visibly drops a populated cache.
	const q = `MATCH (n:Person {name: 'erin'}) RETURN n`
	runDDLQuery(t, eng, q)
	runDDLQuery(t, eng, q)

	// Drive the operator directly so label+property are populated.
	mgr := freshManager(t)
	// We need the constraint registry; the engine exposes ClearPlanCache as
	// the operator-facing hook, so wire that as the callback.
	dropOp := exec.NewDropConstraintOp(
		"person_email_uniq", "Person", "email",
		exec.ConstraintUnique, false,
		mgr,
		// The Engine owns the canonical registry but does not expose it; we
		// avoid that by building a fresh registry and pre-populating it with
		// the same (label, prop) so the unregister succeeds AND we can pass
		// e.ClearPlanCache as the callback under test.
		newPrePopulatedRegistry(t, mgr, "Person", "email"),
		eng.ClearPlanCache,
	)
	if err := dropOp.Init(context.Background()); err != nil {
		t.Fatalf("DropConstraintOp.Init: %v", err)
	}
	var row exec.Row
	if _, err := dropOp.Next(&row); err != nil {
		t.Fatalf("DropConstraintOp.Next: %v", err)
	}
	if err := dropOp.Close(); err != nil {
		t.Fatalf("DropConstraintOp.Close: %v", err)
	}

	if got := probe.invalidations.Load(); got != 2 {
		t.Fatalf("after DROP CONSTRAINT UNIQUE: invalidations = %d, want 2", got)
	}

	// Re-running must miss because the cache was flushed.
	prevMisses := probe.misses.Load()
	runDDLQuery(t, eng, q)
	if got := probe.misses.Load(); got != prevMisses+1 {
		t.Fatalf("after DROP CONSTRAINT re-run: misses = %d, want %d", got, prevMisses+1)
	}
}

// testDropConstraintNotNullInvalidatesCache exercises the DropConstraint
// operator for the NOT NULL kind. Same reason for direct-operator drive
// as testDropConstraintUniqueInvalidatesCache.
func testDropConstraintNotNullInvalidatesCache(t *testing.T) {
	probe := installInvalidationProbe(t)
	eng := newPersonEngine(t)

	runDDLQuery(t, eng, `CREATE CONSTRAINT person_name_nn ON (n:Person) ASSERT n.name IS NOT NULL`)
	if got := probe.invalidations.Load(); got != 1 {
		t.Fatalf("setup CREATE CONSTRAINT NOT NULL: invalidations = %d, want 1", got)
	}

	const q = `MATCH (n:Person) RETURN n`
	runDDLQuery(t, eng, q)
	runDDLQuery(t, eng, q)

	mgr := freshManager(t)
	reg := exec.NewConstraintRegistry()
	reg.RegisterNotNull("Person", "name")
	dropOp := exec.NewDropConstraintOp(
		"person_name_nn", "Person", "name",
		exec.ConstraintNotNull, false,
		mgr, reg,
		eng.ClearPlanCache,
	)
	if err := dropOp.Init(context.Background()); err != nil {
		t.Fatalf("DropConstraintOp.Init: %v", err)
	}
	var row exec.Row
	if _, err := dropOp.Next(&row); err != nil {
		t.Fatalf("DropConstraintOp.Next: %v", err)
	}
	if err := dropOp.Close(); err != nil {
		t.Fatalf("DropConstraintOp.Close: %v", err)
	}

	if got := probe.invalidations.Load(); got != 2 {
		t.Fatalf("after DROP CONSTRAINT NOT NULL: invalidations = %d, want 2", got)
	}

	prevMisses := probe.misses.Load()
	runDDLQuery(t, eng, q)
	if got := probe.misses.Load(); got != prevMisses+1 {
		t.Fatalf("after DROP CONSTRAINT NOT NULL re-run: misses = %d, want %d",
			got, prevMisses+1)
	}
}

// testCreateIndexIfNotExistsNoOpDoesNotInvalidate verifies that
// CREATE INDEX IF NOT EXISTS on an already-existing index is a silent
// no-op and does NOT increment the invalidations counter.
func testCreateIndexIfNotExistsNoOpDoesNotInvalidate(t *testing.T) {
	probe := installInvalidationProbe(t)
	eng := newPersonEngine(t)

	// First create — real mutation; should invalidate once.
	runDDLQuery(t, eng, `CREATE INDEX person_name FOR (n:Person) ON (n.name)`)
	if got := probe.invalidations.Load(); got != 1 {
		t.Fatalf("after first CREATE INDEX: invalidations = %d, want 1", got)
	}

	// Warm cache.
	const q = `MATCH (n:Person {name: 'frank'}) RETURN n`
	runDDLQuery(t, eng, q)
	runDDLQuery(t, eng, q)

	// Second create with IF NOT EXISTS — silent; should NOT bump counter.
	runDDLQuery(t, eng, `CREATE INDEX IF NOT EXISTS person_name FOR (n:Person) ON (n.name)`)
	if got := probe.invalidations.Load(); got != 1 {
		t.Fatalf("after IF NOT EXISTS no-op: invalidations = %d, want 1 (no change)", got)
	}

	// And the cache must still be warm — the next run is a hit.
	prevHits := probe.hits.Load()
	runDDLQuery(t, eng, q)
	if got := probe.hits.Load(); got <= prevHits {
		t.Fatalf("after IF NOT EXISTS no-op, cache should remain warm: hits = %d, want > %d",
			got, prevHits)
	}
}

// testDropIndexIfExistsNoOpDoesNotInvalidate verifies that DROP INDEX
// IF EXISTS on a missing index is a silent no-op and does NOT increment
// the invalidations counter.
func testDropIndexIfExistsNoOpDoesNotInvalidate(t *testing.T) {
	probe := installInvalidationProbe(t)
	eng := newPersonEngine(t)

	// Warm cache without any DDL.
	const q = `MATCH (n:Person) RETURN n`
	runDDLQuery(t, eng, q)
	runDDLQuery(t, eng, q)

	// DROP INDEX IF EXISTS on a non-existent name — silent no-op.
	runDDLQuery(t, eng, `DROP INDEX never_existed IF EXISTS`)
	if got := probe.invalidations.Load(); got != 0 {
		t.Fatalf("after IF EXISTS no-op DROP: invalidations = %d, want 0", got)
	}

	// Cache must still be warm.
	prevHits := probe.hits.Load()
	runDDLQuery(t, eng, q)
	if got := probe.hits.Load(); got <= prevHits {
		t.Fatalf("after IF EXISTS no-op DROP, cache should remain warm: hits = %d, want > %d",
			got, prevHits)
	}
}

// testClearPlanCacheDirectCallEmitsCounter verifies that
// Engine.ClearPlanCache, when called directly, emits the
// invalidations counter exactly once even on an empty cache.
func testClearPlanCacheDirectCallEmitsCounter(t *testing.T) {
	probe := installInvalidationProbe(t)
	eng := newPersonEngine(t)

	// Empty-cache invocation must still emit the counter.
	eng.ClearPlanCache()
	if got := probe.invalidations.Load(); got != 1 {
		t.Fatalf("ClearPlanCache on empty cache: invalidations = %d, want 1", got)
	}

	// A second call also emits — idempotent in effect, but the counter
	// records every invalidation so operators can observe each call.
	eng.ClearPlanCache()
	if got := probe.invalidations.Load(); got != 2 {
		t.Fatalf("ClearPlanCache second call: invalidations = %d, want 2", got)
	}
}

// freshManager returns a new index.Manager scoped to the
// drop-constraint operator-level sub-tests. The Engine type does not
// expose its internal manager, but the unit under test in these
// sub-tests is the onSchemaChange callback (eng.ClearPlanCache), not
// the manager itself — using a separate manager pre-populated with the
// backing index is sufficient to drive the operator's success path.
func freshManager(t *testing.T) *index.Manager {
	t.Helper()
	return index.NewManager()
}

// newPrePopulatedRegistry returns a fresh ConstraintRegistry with a
// UNIQUE constraint already registered for (label, prop), and creates
// the matching backing index in mgr. The synthetic name follows the
// same convention as the production CreateConstraintOp so the drop
// path resolves it correctly.
func newPrePopulatedRegistry(t *testing.T, mgr *index.Manager, label, prop string) *exec.ConstraintRegistry {
	t.Helper()
	reg := exec.NewConstraintRegistry()
	// Create the backing index that DropConstraintOp will look up.
	createOp := exec.NewCreateConstraintOp(
		"probe", label, prop,
		exec.ConstraintUnique, false,
		mgr, reg,
		nil, // no callback — we are setting up state, not exercising invalidation
	)
	if err := createOp.Init(context.Background()); err != nil {
		t.Fatalf("setup CreateConstraintOp.Init: %v", err)
	}
	var row exec.Row
	if _, err := createOp.Next(&row); err != nil {
		t.Fatalf("setup CreateConstraintOp.Next: %v", err)
	}
	if err := createOp.Close(); err != nil {
		t.Fatalf("setup CreateConstraintOp.Close: %v", err)
	}
	return reg
}
