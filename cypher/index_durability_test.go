package cypher_test

// index_durability_test.go — regression gate for task #1343.
//
// CREATE INDEX / DROP INDEX used to write nothing to the WAL: the index was
// registered in-memory only, so recovery.Open surfaced no index definitions
// and the engine rebuilt none on reopen.  A user-created index silently
// vanished after a restart (a scan-fallback divergence — correct results, but
// the planner's NodeByIndexSeek rewrite was no longer available), and
// res.Graph.IndexManager().ListIndexes() nil-dereferenced before an Engine was
// attached.
//
// The fix:
//   - txn.Tx gains CreateIndex/DropIndex, emitting OpCreateIndex/OpDropIndex.
//   - recovery.Open accumulates those ops into Result.Indexes.
//   - NewEngineWithStoreAndSchema re-registers AND backfills each recovered
//     index so a post-restart seek serves the correct nodes.
//   - index.Manager's GetIndex/ListIndexes/Count are nil-safe.
//
// GATE: the durability assertions below (Result.Indexes populated,
// ListIndexes shows the index, the recovered hash index is backfilled) fail on
// the unfixed code and pass after the fix.
//
// Layer: short.  Goroutine-leak coverage is the package-level
// goleak.VerifyTestMain in testmain_test.go (every store/WAL here is local and
// closed); per-test goleak.VerifyNone is intentionally omitted so the tests can
// run with t.Parallel(), matching constraint_durability_test.go.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	indexhash "github.com/FlavioCFOliveira/GoGraph/graph/index/hash"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// ─── helpers ────────────────────────────────────────────────────────────────

func idxRecOpts() recovery.Options[string, float64] {
	return recovery.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	}
}

func idxStoreOpts() txn.Options[string, float64] {
	return txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	}
}

// idxCycle opens dir, runs every query through a WAL-backed engine, then
// closes the WAL cleanly.  It uses NewEngineWithStoreAndSchema so recovered
// index definitions are re-registered.  The last query's error is returned.
func idxCycle(t *testing.T, dir string, queries ...string) error {
	t.Helper()
	res, err := recovery.Open[string, float64](dir, idxRecOpts())
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	defer func() {
		if cerr := w.Close(); cerr != nil {
			t.Errorf("wal.Close: %v", cerr)
		}
	}()
	store := txn.NewStoreWithOptions[string, float64](res.Graph, w, idxStoreOpts())
	eng := cypher.NewEngineWithStoreAndSchema(store, res.Constraints, res.Indexes)

	var lastErr error
	for _, q := range queries {
		lastErr = idxRunOne(t, eng, q)
	}
	if serr := w.Sync(); serr != nil {
		t.Fatalf("wal.Sync: %v", serr)
	}
	return lastErr
}

// idxRunOne runs a write-capable query and returns its terminal error.
func idxRunOne(t *testing.T, eng *cypher.Engine, q string) error {
	t.Helper()
	ctx := context.Background()
	r, err := eng.RunInTxAny(ctx, q, nil)
	if err != nil {
		return err
	}
	for r.Next() { //nolint:revive // drain to commit write
	}
	rerr := r.Err()
	if cerr := r.Close(); cerr != nil && rerr == nil {
		rerr = cerr
	}
	return rerr
}

// idxQuery runs a read-only query and returns the result rows as
// []map[string]any.  The test fails if the query itself errors.
func idxQuery(t *testing.T, eng *cypher.Engine, q string) []map[string]any {
	t.Helper()
	ctx := context.Background()
	r, err := eng.Run(ctx, q, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", q, err)
	}
	defer func() {
		if cerr := r.Close(); cerr != nil {
			t.Errorf("Result.Close: %v", cerr)
		}
	}()
	var rows []map[string]any
	for r.Next() {
		rec := r.Record()
		row := make(map[string]any, len(rec))
		for k, v := range rec {
			row[k] = v
		}
		rows = append(rows, row)
	}
	if err := r.Err(); err != nil {
		t.Fatalf("Result.Err after drain: %v", err)
	}
	return rows
}

// ─── 1. nil-manager ListIndexes must not panic ───────────────────────────────

// TestIndexDurability_ListIndexesOnFreshRecovery guards the nil-deref: calling
// ListIndexes() on the graph returned by recovery.Open before an Engine is
// attached must not panic and must return nil (no indexes yet).
func TestIndexDurability_ListIndexesOnFreshRecovery(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// First open: create an empty WAL so the dir is well-formed.
	res, err := recovery.Open[string, float64](dir, idxRecOpts())
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	if serr := w.Sync(); serr != nil {
		t.Fatalf("wal.Sync: %v", serr)
	}
	if cerr := w.Close(); cerr != nil {
		t.Fatalf("wal.Close: %v", cerr)
	}

	// Before any engine is attached, IndexManager() may return nil on a brand-
	// new LPG.  ListIndexes() must not panic in that case.
	idxMgr := res.Graph.IndexManager()
	names := idxMgr.ListIndexes() // must not panic on a nil manager
	if len(names) != 0 {
		t.Fatalf("expected empty index list on fresh graph, got %v", names)
	}
}

// ─── 2. Hash index survives a plain WAL reopen ───────────────────────────────

// TestIndexDurability_HashSurvivesWALReopen is the primary acceptance criterion
// for task #1343 (hash path): a CREATE INDEX op persisted to the WAL is
// replayed by recovery.Open, and the engine re-registers and backfills the
// index so a subsequent lookup returns the correct nodes.
func TestIndexDurability_HashSurvivesWALReopen(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Cycle 1: create the index and insert a node.
	if err := idxCycle(t, dir,
		`CREATE INDEX person_name FOR (n:Person) ON (n.name)`,
		`CREATE (:Person {name: 'Alice'})`,
	); err != nil {
		t.Fatalf("cycle 1: %v", err)
	}

	// Cycle 2: reopen. The durable definition must reappear in Result.Indexes
	// (this is what the unfixed code never produced).
	res, err := recovery.Open[string, float64](dir, idxRecOpts())
	if err != nil {
		t.Fatalf("recovery.Open cycle 2: %v", err)
	}
	if len(res.Indexes) != 1 {
		t.Fatalf("expected 1 recovered index, got %d: %v", len(res.Indexes), res.Indexes)
	}
	if res.Indexes[0].Name != "person_name" {
		t.Fatalf("expected index name %q, got %q", "person_name", res.Indexes[0].Name)
	}
	if res.Indexes[0].Kind != txn.IndexKindHash {
		t.Fatalf("expected hash index kind, got %v", res.Indexes[0].Kind)
	}
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open cycle 2: %v", err)
	}
	defer func() {
		if cerr := w.Close(); cerr != nil {
			t.Errorf("wal.Close: %v", cerr)
		}
	}()
	store := txn.NewStoreWithOptions[string, float64](res.Graph, w, idxStoreOpts())
	eng := cypher.NewEngineWithStoreAndSchema(store, res.Constraints, res.Indexes)

	// The index must be present in the manager.
	names := eng.ListIndexes()
	found := false
	for _, n := range names {
		if n == "person_name" {
			found = true
		}
	}
	if !found {
		t.Fatalf("index %q not found in engine after reopen; got %v", "person_name", names)
	}

	// The index must be BACKFILLED, not merely registered: the recovered hash
	// index must already contain Alice's node. The engine installs its index
	// manager on res.Graph, so we inspect it directly to prove the backfill ran
	// (a Cypher equality query alone cannot distinguish a backfilled index from
	// a scan fallback, since both return the correct row).
	sub, gerr := res.Graph.IndexManager().GetIndex("person_name")
	if gerr != nil {
		t.Fatalf("GetIndex(person_name) after reopen: %v", gerr)
	}
	hidx, ok := sub.(*indexhash.Index[string])
	if !ok {
		t.Fatalf("recovered index is %T, want *hash.Index[string]", sub)
	}
	if c := hidx.Cardinality("Alice"); c != 1 {
		t.Fatalf("recovered hash index not backfilled: Cardinality(%q) = %d, want 1", "Alice", c)
	}

	// End-to-end: the auto-planned equality query must return Alice after reopen.
	rows := idxQuery(t, eng, `MATCH (n:Person) WHERE n.name = 'Alice' RETURN n.name AS name`)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row after reopen, got %d", len(rows))
	}
	got, ok := rows[0]["name"].(expr.StringValue)
	if !ok {
		t.Fatalf("expected name to be a StringValue, got %T", rows[0]["name"])
	}
	if string(got) != "Alice" {
		t.Fatalf("expected name=Alice, got %q", string(got))
	}
	if serr := w.Sync(); serr != nil {
		t.Errorf("wal.Sync: %v", serr)
	}
}

// ─── 3. BTree index survives a plain WAL reopen ──────────────────────────────

// TestIndexDurability_BTreeSurvivesWALReopen tests the btree CREATE INDEX
// durability path (task #1343).
func TestIndexDurability_BTreeSurvivesWALReopen(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if err := idxCycle(t, dir,
		`CREATE INDEX person_age FOR (n:Person) ON (n.age) OPTIONS {indexType: 'btree'}`,
	); err != nil {
		t.Fatalf("cycle 1: %v", err)
	}

	res, err := recovery.Open[string, float64](dir, idxRecOpts())
	if err != nil {
		t.Fatalf("recovery.Open cycle 2: %v", err)
	}
	if len(res.Indexes) != 1 {
		t.Fatalf("expected 1 recovered index, got %d", len(res.Indexes))
	}
	if res.Indexes[0].Name != "person_age" {
		t.Fatalf("expected index name %q, got %q", "person_age", res.Indexes[0].Name)
	}
	if res.Indexes[0].Kind != txn.IndexKindBTree {
		t.Fatalf("expected btree index kind, got %v", res.Indexes[0].Kind)
	}
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	defer func() {
		if cerr := w.Close(); cerr != nil {
			t.Errorf("wal.Close: %v", cerr)
		}
	}()
	store := txn.NewStoreWithOptions[string, float64](res.Graph, w, idxStoreOpts())
	eng := cypher.NewEngineWithStoreAndSchema(store, res.Constraints, res.Indexes)

	names := eng.ListIndexes()
	found := false
	for _, n := range names {
		if n == "person_age" {
			found = true
		}
	}
	if !found {
		t.Fatalf("btree index %q not found after reopen; got %v", "person_age", names)
	}
	if serr := w.Sync(); serr != nil {
		t.Errorf("wal.Sync: %v", serr)
	}
}

// ─── 4. DROP INDEX is durable ─────────────────────────────────────────────────

// TestIndexDurability_DropSurvivesWALReopen confirms that a DROP INDEX op is
// also recorded in the WAL: after reopen the index must be absent.
func TestIndexDurability_DropSurvivesWALReopen(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Cycle 1: create then drop the index.
	if err := idxCycle(t, dir,
		`CREATE INDEX drop_me FOR (n:Thing) ON (n.val)`,
		`DROP INDEX drop_me`,
	); err != nil {
		t.Fatalf("cycle 1: %v", err)
	}

	res, err := recovery.Open[string, float64](dir, idxRecOpts())
	if err != nil {
		t.Fatalf("recovery.Open cycle 2: %v", err)
	}
	if len(res.Indexes) != 0 {
		t.Fatalf("expected 0 recovered indexes after DROP, got %d: %v", len(res.Indexes), res.Indexes)
	}
}

// ─── 5. Indexes() accessor works on store-less engine ────────────────────────

// TestIndexDurability_StorelessListIndexes guards the common case: a store-less
// Engine backed by an empty in-memory graph must return an empty list without
// panicking.
func TestIndexDurability_StorelessListIndexes(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)

	names := eng.ListIndexes()
	if len(names) != 0 {
		t.Fatalf("expected empty list on fresh store-less engine, got %v", names)
	}

	// Create an index and verify it appears.
	ctx := context.Background()
	r, err := eng.Run(ctx, `CREATE INDEX x FOR (n:A) ON (n.p)`, nil)
	if err != nil {
		t.Fatalf("CREATE INDEX: %v", err)
	}
	for r.Next() { //nolint:revive
	}
	if cerr := r.Close(); cerr != nil {
		t.Fatalf("Close: %v", cerr)
	}

	names = eng.ListIndexes()
	if len(names) != 1 || names[0] != "x" {
		t.Fatalf("expected [x], got %v", names)
	}
}
