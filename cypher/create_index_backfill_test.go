package cypher_test

// create_index_backfill_test.go — task #1340 gate tests.
//
// CREATE INDEX used to register a permanently empty hash index: no backfill
// of pre-existing data and a no-op Apply, while the planner still rewrote the
// matching equality predicate into a NodeByIndexSeek — silently returning
// zero rows. These tests pin the fixed contract:
//
//   - pre-existing nodes are backfilled at CREATE INDEX time;
//   - nodes written AFTER index creation are indexed via the change fan-out;
//   - property updates move the entry, property/label removals and node
//     deletions drop it;
//   - the planner keeps using NodeByIndexSeek (the optimisation is preserved,
//     now over a correctly populated index);
//   - an index never serves a predicate over a different (label, property)
//     pair (no wrong-label rows from the any-index fallback).

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// queryNames runs a parameterised name equality MATCH for label and returns
// the matched n.name values.
func queryNames(t *testing.T, eng *cypher.Engine, label, name string) []string {
	t.Helper()
	res, err := eng.Run(context.Background(),
		`MATCH (n:`+label+`) WHERE n.name = $n RETURN n.name`,
		map[string]expr.Value{"n": expr.StringValue(name)})
	if err != nil {
		t.Fatalf("MATCH %s name=%q: %v", label, name, err)
	}
	defer res.Close() //nolint:errcheck // test teardown
	rows := collectRecords(t, res)
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		v, ok := row["n.name"].(expr.StringValue)
		if !ok {
			t.Fatalf("n.name: expected StringValue, got %T", row["n.name"])
		}
		out = append(out, string(v))
	}
	return out
}

func mustWrite(t *testing.T, eng *cypher.Engine, q string) {
	t.Helper()
	res, err := eng.RunInTxAny(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("write %q: %v", q, err)
	}
	drainResult(t, res)
}

func mustExplainSeek(t *testing.T, eng *cypher.Engine, q string, params map[string]expr.Value) {
	t.Helper()
	plan, err := eng.Explain(q, params)
	if err != nil {
		t.Fatalf("Explain %q: %v", q, err)
	}
	if !strings.Contains(plan, "NodeByIndexSeek") {
		t.Fatalf("expected NodeByIndexSeek for %q; got:\n%s", q, plan)
	}
}

// TestCreateIndexBackfill_PreExistingData is the mandatory gate: a node
// created BEFORE the index exists must be found through the indexed predicate
// (it returned zero rows before the fix), and the plan must still be a
// NodeByIndexSeek so the assertion exercises the index, not the scan.
func TestCreateIndexBackfill_PreExistingData(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)

	mustWrite(t, eng, `CREATE (n:Person {name: 'Alice'})`)
	mustWrite(t, eng, `CREATE (n:Person {name: 'Bob'})`)
	drainResult(t, mustRun(t, context.Background(), eng,
		`CREATE INDEX person_name_hash FOR (n:Person) ON (n.name)`))

	mustExplainSeek(t, eng, `MATCH (n:Person) WHERE n.name = $n RETURN n`,
		map[string]expr.Value{"n": expr.StringValue("probe")})

	if got := queryNames(t, eng, "Person", "Alice"); len(got) != 1 || got[0] != "Alice" {
		t.Fatalf("pre-existing node: want [Alice], got %v", got)
	}
	if got := queryNames(t, eng, "Person", "Bob"); len(got) != 1 || got[0] != "Bob" {
		t.Fatalf("pre-existing node: want [Bob], got %v", got)
	}
}

// TestCreateIndexBackfill_PostCreationInsert is the second half of the
// mandatory gate: a node inserted AFTER index creation must be findable via
// the same parameterised query (the change fan-out maintains the index).
func TestCreateIndexBackfill_PostCreationInsert(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)

	drainResult(t, mustRun(t, context.Background(), eng,
		`CREATE INDEX person_name_hash FOR (n:Person) ON (n.name)`))
	mustWrite(t, eng, `CREATE (n:Person {name: 'Carol'})`)

	mustExplainSeek(t, eng, `MATCH (n:Person) WHERE n.name = $n RETURN n`,
		map[string]expr.Value{"n": expr.StringValue("probe")})

	if got := queryNames(t, eng, "Person", "Carol"); len(got) != 1 || got[0] != "Carol" {
		t.Fatalf("post-creation node: want [Carol], got %v", got)
	}
}

// TestCreateIndexBackfill_WALBackedEngine repeats the gate on a WAL-backed
// engine, which serialises the CREATE INDEX backfill on the store's
// single-writer mutex rather than the engine's writeMu.
func TestCreateIndexBackfill_WALBackedEngine(t *testing.T) {
	t.Parallel()
	eng, _, _ := newWALStoreEngine(t)

	mustWrite(t, eng, `CREATE (n:Person {name: 'Alice'})`)
	drainResult(t, mustRun(t, context.Background(), eng,
		`CREATE INDEX person_name_hash FOR (n:Person) ON (n.name)`))
	mustWrite(t, eng, `CREATE (n:Person {name: 'Dave'})`)

	if got := queryNames(t, eng, "Person", "Alice"); len(got) != 1 {
		t.Fatalf("pre-existing node on WAL engine: want 1 row, got %v", got)
	}
	if got := queryNames(t, eng, "Person", "Dave"); len(got) != 1 {
		t.Fatalf("post-creation node on WAL engine: want 1 row, got %v", got)
	}
}

// TestCreateIndexBackfill_UpdateMovesEntry: updating the indexed property must
// move the node from the old value to the new one.
func TestCreateIndexBackfill_UpdateMovesEntry(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)

	mustWrite(t, eng, `CREATE (n:Person {name: 'Alice'})`)
	drainResult(t, mustRun(t, context.Background(), eng,
		`CREATE INDEX person_name_hash FOR (n:Person) ON (n.name)`))
	mustWrite(t, eng, `MATCH (n:Person) WHERE n.name = 'Alice' SET n.name = 'Alicia'`)

	if got := queryNames(t, eng, "Person", "Alice"); len(got) != 0 {
		t.Fatalf("old value must not match after update, got %v", got)
	}
	if got := queryNames(t, eng, "Person", "Alicia"); len(got) != 1 || got[0] != "Alicia" {
		t.Fatalf("new value: want [Alicia], got %v", got)
	}
}

// TestCreateIndexBackfill_DeleteRemovesEntry: a deleted node must never be
// served by the index again.
func TestCreateIndexBackfill_DeleteRemovesEntry(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)

	mustWrite(t, eng, `CREATE (n:Person {name: 'Alice'})`)
	drainResult(t, mustRun(t, context.Background(), eng,
		`CREATE INDEX person_name_hash FOR (n:Person) ON (n.name)`))
	mustWrite(t, eng, `MATCH (n:Person) WHERE n.name = 'Alice' DETACH DELETE n`)

	if got := queryNames(t, eng, "Person", "Alice"); len(got) != 0 {
		t.Fatalf("deleted node must not be served by the index, got %v", got)
	}
}

// TestCreateIndexBackfill_RemoveLabelDropsEntry: removing the indexed label
// detaches the node from the index; re-adding the label re-attaches it.
func TestCreateIndexBackfill_RemoveLabelDropsEntry(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)

	mustWrite(t, eng, `CREATE (n:Person {name: 'Alice'})`)
	drainResult(t, mustRun(t, context.Background(), eng,
		`CREATE INDEX person_name_hash FOR (n:Person) ON (n.name)`))

	mustWrite(t, eng, `MATCH (n:Person) WHERE n.name = 'Alice' REMOVE n:Person`)
	if got := queryNames(t, eng, "Person", "Alice"); len(got) != 0 {
		t.Fatalf("unlabelled node must not be served by the index, got %v", got)
	}

	mustWrite(t, eng, `MATCH (n) WHERE n.name = 'Alice' SET n:Person`)
	if got := queryNames(t, eng, "Person", "Alice"); len(got) != 1 {
		t.Fatalf("re-labelled node must be indexed again, got %v", got)
	}
}

// TestCreateIndexBackfill_WrongLabelNotServed: an index bound to Person.name
// must not serve a Company.name predicate — neither hiding the company
// (silent zero rows) nor leaking the person (wrong-label rows).
func TestCreateIndexBackfill_WrongLabelNotServed(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)

	mustWrite(t, eng, `CREATE (n:Person {name: 'Acme'})`)
	mustWrite(t, eng, `CREATE (n:Company {name: 'Acme'})`)
	drainResult(t, mustRun(t, context.Background(), eng,
		`CREATE INDEX person_name_hash FOR (n:Person) ON (n.name)`))

	// The Person predicate is served by the index and returns only the person.
	if got := queryNames(t, eng, "Person", "Acme"); len(got) != 1 {
		t.Fatalf("Person Acme: want 1 row, got %v", got)
	}
	// The Company predicate must fall back to the scan and find the company.
	if got := queryNames(t, eng, "Company", "Acme"); len(got) != 1 {
		t.Fatalf("Company Acme: want 1 row (scan fallback), got %v", got)
	}
	// An unlabelled match must see both nodes, not just the indexed label.
	res, err := eng.Run(context.Background(),
		`MATCH (n) WHERE n.name = $n RETURN n.name`,
		map[string]expr.Value{"n": expr.StringValue("Acme")})
	if err != nil {
		t.Fatalf("unlabelled MATCH: %v", err)
	}
	defer res.Close() //nolint:errcheck // test teardown
	if rows := collectRecords(t, res); len(rows) != 2 {
		t.Fatalf("unlabelled Acme: want 2 rows, got %d", len(rows))
	}
}

// TestCreateIndexBackfill_NonStringValuesSkipped: the string-keyed index only
// carries plain string values; nodes whose property holds another kind are
// left to the scan path and equality still answers correctly.
func TestCreateIndexBackfill_NonStringValuesSkipped(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)

	mustWrite(t, eng, `CREATE (n:Person {name: 'Alice'})`)
	mustWrite(t, eng, `CREATE (n:Person {name: 42})`)
	drainResult(t, mustRun(t, context.Background(), eng,
		`CREATE INDEX person_name_hash FOR (n:Person) ON (n.name)`))

	if got := queryNames(t, eng, "Person", "Alice"); len(got) != 1 {
		t.Fatalf("string value: want 1 row, got %v", got)
	}
	// The integer-valued node is found via the scan fallback (the seek
	// declines a non-string seek value against the string-keyed index).
	res, err := eng.Run(context.Background(),
		`MATCH (n:Person) WHERE n.name = 42 RETURN n.name`, nil)
	if err != nil {
		t.Fatalf("integer equality: %v", err)
	}
	defer res.Close() //nolint:errcheck // test teardown
	if rows := collectRecords(t, res); len(rows) != 1 {
		t.Fatalf("integer equality: want 1 row, got %d", len(rows))
	}
}

// TestCreateIndexBackfill_FailedWriteNotIndexed: a write statement that fails
// after its eager mutations rolls back atomically; the index must not retain
// entries for the rolled-back node (the buffered changes are discarded).
func TestCreateIndexBackfill_FailedWriteNotIndexed(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)

	drainResult(t, mustRun(t, context.Background(), eng,
		`CREATE INDEX person_name_hash FOR (n:Person) ON (n.name)`))

	// The statement creates the node (buffering the index change for 'Ghost'),
	// then fails deterministically on the rejected x write: the whole write
	// rolls back, so the index must not serve 'Ghost'.
	g.SetValidator(&nthSetRejector{key: "x", rejN: 1})
	res, err := eng.RunInTxAny(context.Background(),
		`CREATE (n:Person {name: 'Ghost'}) WITH n SET n.x = 1`, nil)
	if err == nil {
		for res.Next() {
		}
		err = res.Err()
		_ = res.Close()
	}
	if err == nil {
		t.Fatal("expected the write statement to fail")
	}

	if got := queryNames(t, eng, "Person", "Ghost"); len(got) != 0 {
		t.Fatalf("rolled-back node must not be indexed, got %v", got)
	}
}

// TestCreateIndexBackfill_ConcurrentWrites races CREATE INDEX against
// concurrent writers: every node — written before, during, or after the DDL —
// must be findable through the indexed predicate afterwards. Run under -race
// this also pins the writer-serialised backfill (no torn scan/registration).
func TestCreateIndexBackfill_ConcurrentWrites(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	const writers = 4
	const perWriter = 25

	// Seed some pre-existing rows so the backfill scan has work to do.
	for i := 0; i < 10; i++ {
		mustWrite(t, eng, fmt.Sprintf(`CREATE (n:Person {name: 'seed%d'})`, i))
	}

	var wg sync.WaitGroup
	errCh := make(chan error, writers+1)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				q := fmt.Sprintf(`CREATE (n:Person {name: 'w%d_%d'})`, w, i)
				res, err := eng.RunInTxAny(ctx, q, nil)
				if err != nil {
					errCh <- fmt.Errorf("writer %d: %w", w, err)
					return
				}
				for res.Next() {
				}
				if err := res.Err(); err != nil {
					errCh <- fmt.Errorf("writer %d drain: %w", w, err)
					return
				}
				_ = res.Close()
			}
		}(w)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		res, err := eng.Run(ctx, `CREATE INDEX person_name_hash FOR (n:Person) ON (n.name)`, nil)
		if err != nil {
			errCh <- fmt.Errorf("CREATE INDEX: %w", err)
			return
		}
		for res.Next() {
		}
		_ = res.Close()
	}()
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	for i := 0; i < 10; i++ {
		name := fmt.Sprintf("seed%d", i)
		if got := queryNames(t, eng, "Person", name); len(got) != 1 {
			t.Fatalf("%s: want 1 row, got %v", name, got)
		}
	}
	for w := 0; w < writers; w++ {
		for i := 0; i < perWriter; i++ {
			name := fmt.Sprintf("w%d_%d", w, i)
			if got := queryNames(t, eng, "Person", name); len(got) != 1 {
				t.Fatalf("%s: want 1 row, got %v", name, got)
			}
		}
	}
}

// TestCreateIndexBackfill_DuplicateAndIfNotExists pins the unchanged DDL
// surface: a duplicate CREATE INDEX errors, IF NOT EXISTS absorbs it.
func TestCreateIndexBackfill_DuplicateAndIfNotExists(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	drainResult(t, mustRun(t, ctx, eng, `CREATE INDEX dup_idx FOR (n:T) ON (n.p)`))

	if _, err := eng.Run(ctx, `CREATE INDEX dup_idx FOR (n:T) ON (n.p)`, nil); err == nil {
		t.Fatal("duplicate CREATE INDEX must error")
	}
	res, err := eng.Run(ctx, `CREATE INDEX IF NOT EXISTS dup_idx FOR (n:T) ON (n.p)`, nil)
	if err != nil {
		t.Fatalf("CREATE INDEX IF NOT EXISTS: %v", err)
	}
	drainResult(t, res)
}
