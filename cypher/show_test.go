package cypher_test

// show_test.go — engine-integration tests for SHOW CONSTRAINTS / SHOW INDEXES
// (#1922).
//
// These tests fail on the pre-change behaviour: before #1922, SHOW routed to
// neither the DDL path nor the ANTLR planner, so Engine.Run(`SHOW CONSTRAINTS`)
// returned a parse error.

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// showRow is a decoded SHOW result row: scalar columns as strings, the
// labelsOrTypes/properties list columns as []string.
type showRow struct {
	scalars map[string]string
	lists   map[string][]string
}

// collectShow drains a SHOW Result, decoding StringValue columns into scalars
// and ListValue columns into lists. It returns the column order and the rows.
func collectShow(t *testing.T, res *cypher.Result) (cols []string, rows []showRow) {
	t.Helper()
	defer func() {
		if err := res.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	cols = res.Columns()
	for res.Next() {
		rec := res.Record()
		row := showRow{scalars: map[string]string{}, lists: map[string][]string{}}
		for _, c := range cols {
			switch v := rec[c].(type) {
			case expr.StringValue:
				row.scalars[c] = string(v)
			case expr.ListValue:
				elems := make([]string, 0, len(v))
				for _, e := range v {
					sv, ok := e.(expr.StringValue)
					if !ok {
						t.Fatalf("column %q list element %v is not a StringValue (%T)", c, e, e)
					}
					elems = append(elems, string(sv))
				}
				row.lists[c] = elems
			default:
				t.Fatalf("column %q has unexpected value %v (%T)", c, rec[c], rec[c])
			}
		}
		rows = append(rows, row)
	}
	if err := res.Err(); err != nil {
		t.Fatalf("iteration error: %v", err)
	}
	return cols, rows
}

func mustShow(t *testing.T, eng *cypher.Engine, q string) (cols []string, rows []showRow) {
	t.Helper()
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", q, err)
	}
	return collectShow(t, res)
}

// TestShowConstraints_EmptySchema confirms an empty schema yields zero rows with
// the exact Neo4j-aligned column set.
func TestShowConstraints_EmptySchema(t *testing.T) {
	t.Parallel()
	eng := cypher.NewEngine(lpg.New[string, float64](adjlist.Config{}))

	cols, rows := mustShow(t, eng, `SHOW CONSTRAINTS`)
	want := []string{"name", "type", "entityType", "labelsOrTypes", "properties"}
	if !reflect.DeepEqual(cols, want) {
		t.Errorf("columns = %v, want %v", cols, want)
	}
	if len(rows) != 0 {
		t.Errorf("expected 0 rows on empty schema, got %d: %v", len(rows), rows)
	}
}

// TestShowIndexes_EmptySchema confirms an empty schema yields zero rows with the
// exact Neo4j-aligned column set.
func TestShowIndexes_EmptySchema(t *testing.T) {
	t.Parallel()
	eng := cypher.NewEngine(lpg.New[string, float64](adjlist.Config{}))

	cols, rows := mustShow(t, eng, `SHOW INDEXES`)
	want := []string{"name", "state", "type", "entityType", "labelsOrTypes", "properties"}
	if !reflect.DeepEqual(cols, want) {
		t.Errorf("columns = %v, want %v", cols, want)
	}
	if len(rows) != 0 {
		t.Errorf("expected 0 rows on empty schema, got %d: %v", len(rows), rows)
	}
}

// TestShowConstraints_ListsCreated verifies a created constraint is listed with
// its declared name and the correct column values, for both the plural and the
// singular form.
func TestShowConstraints_ListsCreated(t *testing.T) {
	t.Parallel()
	eng := cypher.NewEngine(lpg.New[string, float64](adjlist.Config{}))
	ctx := context.Background()

	if _, err := eng.Run(ctx, `CREATE CONSTRAINT person_email_unique FOR (n:Person) REQUIRE n.email IS UNIQUE`, nil); err != nil {
		t.Fatalf("CREATE CONSTRAINT (unique): %v", err)
	}
	if _, err := eng.Run(ctx, `CREATE CONSTRAINT person_name_notnull FOR (n:Person) REQUIRE n.name IS NOT NULL`, nil); err != nil {
		t.Fatalf("CREATE CONSTRAINT (not null): %v", err)
	}

	for _, q := range []string{`SHOW CONSTRAINTS`, `SHOW CONSTRAINT`} {
		_, rows := mustShow(t, eng, q)
		byName := indexShowRowsByName(t, rows)
		if len(byName) != 2 {
			t.Fatalf("%s: expected 2 constraints, got %d: %v", q, len(byName), rows)
		}

		uq, ok := byName["person_email_unique"]
		if !ok {
			t.Fatalf("%s: missing constraint person_email_unique in %v", q, rows)
		}
		assertScalar(t, uq, "type", "UNIQUE")
		assertScalar(t, uq, "entityType", "NODE")
		assertList(t, uq, "labelsOrTypes", []string{"Person"})
		assertList(t, uq, "properties", []string{"email"})

		nn, ok := byName["person_name_notnull"]
		if !ok {
			t.Fatalf("%s: missing constraint person_name_notnull in %v", q, rows)
		}
		assertScalar(t, nn, "type", "NOT_NULL")
		assertScalar(t, nn, "entityType", "NODE")
		assertList(t, nn, "labelsOrTypes", []string{"Person"})
		assertList(t, nn, "properties", []string{"name"})
	}
}

// TestShowIndexes_ListsCreated verifies created indexes (hash and btree) are
// listed with their declared name and the correct column values.
func TestShowIndexes_ListsCreated(t *testing.T) {
	t.Parallel()
	eng := cypher.NewEngine(lpg.New[string, float64](adjlist.Config{}))
	ctx := context.Background()

	if _, err := eng.Run(ctx, `CREATE INDEX person_email FOR (n:Person) ON (n.email)`, nil); err != nil {
		t.Fatalf("CREATE INDEX (hash): %v", err)
	}
	if _, err := eng.Run(ctx, `CREATE INDEX person_age FOR (n:Person) ON (n.age) OPTIONS {indexType: 'btree'}`, nil); err != nil {
		t.Fatalf("CREATE INDEX (btree): %v", err)
	}

	for _, q := range []string{`SHOW INDEXES`, `SHOW INDEX`} {
		_, rows := mustShow(t, eng, q)
		byName := indexShowRowsByName(t, rows)
		// Exactly the two user indexes: the numeric companion of the btree is
		// filtered (shared with db.indexes()), and no UNIQUE constraint exists so
		// there is no __uniq__ backing index.
		if len(byName) != 2 {
			t.Fatalf("%s: expected 2 indexes, got %d: %v", q, len(byName), rows)
		}

		hash, ok := byName["person_email"]
		if !ok {
			t.Fatalf("%s: missing index person_email in %v", q, rows)
		}
		assertScalar(t, hash, "state", "ONLINE")
		assertScalar(t, hash, "type", "hash")
		assertScalar(t, hash, "entityType", "NODE")
		assertList(t, hash, "labelsOrTypes", []string{"Person"})
		assertList(t, hash, "properties", []string{"email"})

		btree, ok := byName["person_age"]
		if !ok {
			t.Fatalf("%s: missing index person_age in %v", q, rows)
		}
		assertScalar(t, btree, "state", "ONLINE")
		assertScalar(t, btree, "type", "btree")
		assertScalar(t, btree, "entityType", "NODE")
		assertList(t, btree, "labelsOrTypes", []string{"Person"})
		assertList(t, btree, "properties", []string{"age"})
	}
}

// TestShow_MalformedRejected confirms unsupported SHOW forms are rejected with a
// non-nil error rather than silently returning a result.
func TestShow_MalformedRejected(t *testing.T) {
	t.Parallel()
	eng := cypher.NewEngine(lpg.New[string, float64](adjlist.Config{}))
	ctx := context.Background()

	for _, q := range []string{
		`SHOW FOO`,
		`SHOW CONSTRAINTS BRIEF`,
		`SHOW INDEXES VERBOSE`,
		`SHOW INDEXES YIELD name, type`,
		`SHOW CONSTRAINTS WHERE type = 'UNIQUE'`,
	} {
		if _, err := eng.Run(ctx, q, nil); err == nil {
			t.Errorf("Run(%q) = nil error, want a rejection", q)
		}
	}
}

// TestShow_DeterministicOrder confirms SHOW output is sorted by name and stable
// across repeated runs.
func TestShow_DeterministicOrder(t *testing.T) {
	t.Parallel()
	eng := cypher.NewEngine(lpg.New[string, float64](adjlist.Config{}))
	ctx := context.Background()

	// Create in a non-sorted order.
	for _, name := range []string{"zeta", "alpha", "mike"} {
		if _, err := eng.Run(ctx, `CREATE INDEX `+name+` FOR (n:L) ON (n.p_`+name+`)`, nil); err != nil {
			t.Fatalf("CREATE INDEX %s: %v", name, err)
		}
	}
	wantOrder := []string{"alpha", "mike", "zeta"}

	var first []string
	for run := 0; run < 3; run++ {
		_, rows := mustShow(t, eng, `SHOW INDEXES`)
		got := make([]string, len(rows))
		for i, r := range rows {
			got[i] = r.scalars["name"]
		}
		if !reflect.DeepEqual(got, wantOrder) {
			t.Fatalf("run %d: order = %v, want %v", run, got, wantOrder)
		}
		if run == 0 {
			first = got
		} else if !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d: order %v differs from first run %v", run, got, first)
		}
	}
}

// TestShow_MatchesDbProcedures is the non-divergence regression: SHOW must list
// exactly the same constraints/indexes (by name) as db.constraints()/
// db.indexes(), because they share the same enumeration. A UNIQUE constraint's
// backing index is listed by both.
func TestShow_MatchesDbProcedures(t *testing.T) {
	t.Parallel()
	eng := cypher.NewEngine(lpg.New[string, float64](adjlist.Config{}))
	ctx := context.Background()

	stmts := []string{
		`CREATE CONSTRAINT c_unique FOR (n:Person) REQUIRE n.email IS UNIQUE`,
		`CREATE CONSTRAINT c_notnull FOR (n:Person) REQUIRE n.name IS NOT NULL`,
		`CREATE INDEX i_hash FOR (n:Movie) ON (n.title)`,
		`CREATE INDEX i_btree FOR (n:Movie) ON (n.year) OPTIONS {indexType: 'btree'}`,
	}
	for _, s := range stmts {
		if _, err := eng.Run(ctx, s, nil); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}

	// Constraints: SHOW names == db.constraints() names.
	_, cRows := mustShow(t, eng, `SHOW CONSTRAINTS`)
	showConstraintNames := showNameSet(cRows)
	dbConstraintNames := procNameSet(t, eng, `CALL db.constraints() YIELD name`)
	if !reflect.DeepEqual(showConstraintNames, dbConstraintNames) {
		t.Errorf("SHOW CONSTRAINTS names %v != db.constraints() names %v", showConstraintNames, dbConstraintNames)
	}

	// Indexes: SHOW names == db.indexes() names (includes the __uniq__ backing
	// index and excludes the filtered numeric companion, for both views).
	_, iRows := mustShow(t, eng, `SHOW INDEXES`)
	showIndexNames := showNameSet(iRows)
	dbIndexNames := procNameSet(t, eng, `CALL db.indexes() YIELD name`)
	if !reflect.DeepEqual(showIndexNames, dbIndexNames) {
		t.Errorf("SHOW INDEXES names %v != db.indexes() names %v", showIndexNames, dbIndexNames)
	}
}

// TestShow_ReadOnlyTransaction confirms SHOW runs on a read-only transaction
// (it is a pure read) while a schema-writing DDL statement is still rejected.
func TestShow_ReadOnlyTransaction(t *testing.T) {
	t.Parallel()
	eng := cypher.NewEngine(lpg.New[string, float64](adjlist.Config{}))
	ctx := context.Background()

	if _, err := eng.Run(ctx, `CREATE CONSTRAINT c_ro FOR (n:Person) REQUIRE n.email IS UNIQUE`, nil); err != nil {
		t.Fatalf("CREATE CONSTRAINT: %v", err)
	}

	tx, err := eng.BeginReadTx(ctx)
	if err != nil {
		t.Fatalf("BeginReadTx: %v", err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil {
			t.Errorf("Rollback: %v", err)
		}
	}()

	// SHOW is permitted on a read-only transaction.
	res, err := tx.Exec(`SHOW CONSTRAINTS`, nil)
	if err != nil {
		t.Fatalf("Exec(SHOW CONSTRAINTS) on read-only tx: %v", err)
	}
	_, rows := collectShow(t, res)
	if len(rows) != 1 || rows[0].scalars["name"] != "c_ro" {
		t.Fatalf("expected the single constraint c_ro, got %v", rows)
	}

	// A schema-writing DDL statement is still rejected on a read-only tx.
	if _, err := tx.Exec(`CREATE INDEX bad FOR (n:Person) ON (n.email)`, nil); err == nil {
		t.Error("expected CREATE INDEX to be rejected on a read-only tx, got nil")
	}
}

// TestShow_WriteTransaction confirms SHOW is permitted inside an explicit write
// transaction (it reads the committed schema without touching the tx's barrier
// or WAL), whereas a schema-writing DDL statement is rejected there.
func TestShow_WriteTransaction(t *testing.T) {
	t.Parallel()
	eng := cypher.NewEngine(lpg.New[string, float64](adjlist.Config{}))
	ctx := context.Background()

	if _, err := eng.Run(ctx, `CREATE INDEX i_wt FOR (n:Person) ON (n.email)`, nil); err != nil {
		t.Fatalf("CREATE INDEX: %v", err)
	}

	tx, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	committed := false
	defer func() {
		if !committed {
			if err := tx.Rollback(); err != nil {
				t.Errorf("Rollback: %v", err)
			}
		}
	}()

	res, err := tx.Exec(`SHOW INDEXES`, nil)
	if err != nil {
		t.Fatalf("Exec(SHOW INDEXES) inside write tx: %v", err)
	}
	_, rows := collectShow(t, res)
	if len(rows) != 1 || rows[0].scalars["name"] != "i_wt" {
		t.Fatalf("expected the single index i_wt, got %v", rows)
	}

	// DDL that writes schema is still rejected inside an explicit tx.
	if _, err := tx.Exec(`CREATE INDEX bad FOR (n:Person) ON (n.name)`, nil); err == nil {
		t.Error("expected CREATE INDEX to be rejected inside a write tx, got nil")
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	committed = true
}

// ─────────────────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────────────────

func indexShowRowsByName(t *testing.T, rows []showRow) map[string]showRow {
	t.Helper()
	byName := make(map[string]showRow, len(rows))
	for _, r := range rows {
		name, ok := r.scalars["name"]
		if !ok {
			t.Fatalf("row missing name column: %v", r)
		}
		if _, dup := byName[name]; dup {
			t.Fatalf("duplicate name %q in %v", name, rows)
		}
		byName[name] = r
	}
	return byName
}

func assertScalar(t *testing.T, r showRow, col, want string) {
	t.Helper()
	if got := r.scalars[col]; got != want {
		t.Errorf("column %q = %q, want %q", col, got, want)
	}
}

func assertList(t *testing.T, r showRow, col string, want []string) {
	t.Helper()
	if got := r.lists[col]; !reflect.DeepEqual(got, want) {
		t.Errorf("column %q = %v, want %v", col, got, want)
	}
}

func showNameSet(rows []showRow) []string {
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		names = append(names, r.scalars["name"])
	}
	sort.Strings(names)
	return names
}

func procNameSet(t *testing.T, eng *cypher.Engine, q string) []string {
	t.Helper()
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", q, err)
	}
	rows := collectProc(t, res)
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		names = append(names, r["name"])
	}
	sort.Strings(names)
	return names
}
