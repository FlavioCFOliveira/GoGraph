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
// non-nil error rather than silently returning a result. (YIELD / WHERE are now
// supported — see TestShow_Yield*.)
func TestShow_MalformedRejected(t *testing.T) {
	t.Parallel()
	eng := cypher.NewEngine(lpg.New[string, float64](adjlist.Config{}))
	ctx := context.Background()

	for _, q := range []string{
		`SHOW FOO`,
		`SHOW CONSTRAINTS BRIEF`,
		`SHOW INDEXES VERBOSE`,
		`SHOW CONSTRAINTS YIELD bogus`, // unknown column
		`SHOW CONSTRAINTS YIELD name WHERE type = 'UNIQUE'`, // scope barrier
		`SHOW CONSTRAINTS RETURN name`,                      // RETURN without YIELD
		`SHOW CONSTRAINTS YIELD name, type RETURN count(*)`, // aggregation
		`SHOW CONSTRAINTS YIELD name ORDER BY name`,         // YIELD-level ORDER BY
		`SHOW CONSTRAINTS YIELD toUpper(name)`,              // expression in YIELD
	} {
		if _, err := eng.Run(ctx, q, nil); err == nil {
			t.Errorf("Run(%q) = nil error, want a rejection", q)
		}
	}
}

// TestShow_YieldProjection verifies YIELD selects and orders the named columns
// (the scope barrier), for both an explicit list and YIELD *.
func TestShow_YieldProjection(t *testing.T) {
	t.Parallel()
	eng := cypher.NewEngine(lpg.New[string, float64](adjlist.Config{}))
	ctx := context.Background()
	if _, err := eng.Run(ctx, `CREATE CONSTRAINT c1 FOR (n:Person) REQUIRE n.email IS UNIQUE`, nil); err != nil {
		t.Fatalf("CREATE CONSTRAINT: %v", err)
	}

	// Explicit two-column YIELD: only those columns, in that order.
	cols, rows := mustShow(t, eng, `SHOW CONSTRAINTS YIELD name, type`)
	if want := []string{"name", "type"}; !reflect.DeepEqual(cols, want) {
		t.Errorf("columns = %v, want %v", cols, want)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d: %v", len(rows), rows)
	}
	assertScalar(t, rows[0], "name", "c1")
	assertScalar(t, rows[0], "type", "UNIQUE")

	// YIELD * is the driver-default projection: every default column, in order.
	starCols, starRows := mustShow(t, eng, `SHOW CONSTRAINTS YIELD *`)
	plainCols, plainRows := mustShow(t, eng, `SHOW CONSTRAINTS`)
	if !reflect.DeepEqual(starCols, plainCols) {
		t.Errorf("YIELD * columns %v != plain columns %v", starCols, plainCols)
	}
	if !reflect.DeepEqual(starRows, plainRows) {
		t.Errorf("YIELD * rows %v != plain rows %v", starRows, plainRows)
	}
}

// TestShow_YieldAlias verifies AS aliases rename the output columns while reading
// the underlying SHOW column.
func TestShow_YieldAlias(t *testing.T) {
	t.Parallel()
	eng := cypher.NewEngine(lpg.New[string, float64](adjlist.Config{}))
	ctx := context.Background()
	if _, err := eng.Run(ctx, `CREATE INDEX i1 FOR (n:Person) ON (n.email)`, nil); err != nil {
		t.Fatalf("CREATE INDEX: %v", err)
	}

	cols, rows := mustShow(t, eng, `SHOW INDEXES YIELD name AS idx, type AS kind`)
	if want := []string{"idx", "kind"}; !reflect.DeepEqual(cols, want) {
		t.Errorf("columns = %v, want %v", cols, want)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d: %v", len(rows), rows)
	}
	assertScalar(t, rows[0], "idx", "i1")
	assertScalar(t, rows[0], "kind", "hash")
}

// TestShow_YieldWhere verifies the WHERE predicate filters the projected rows,
// both after an explicit YIELD (scope = yielded columns) and standalone
// (scope = all default columns).
func TestShow_YieldWhere(t *testing.T) {
	t.Parallel()
	eng := cypher.NewEngine(lpg.New[string, float64](adjlist.Config{}))
	ctx := context.Background()
	for _, s := range []string{
		`CREATE CONSTRAINT uq FOR (n:Person) REQUIRE n.email IS UNIQUE`,
		`CREATE CONSTRAINT nn FOR (n:Person) REQUIRE n.name IS NOT NULL`,
	} {
		if _, err := eng.Run(ctx, s, nil); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}

	// YIELD then WHERE over a yielded column.
	_, rows := mustShow(t, eng, `SHOW CONSTRAINTS YIELD name, type WHERE type = 'UNIQUE'`)
	if len(rows) != 1 || rows[0].scalars["name"] != "uq" {
		t.Fatalf("YIELD…WHERE = %v, want only uq", rows)
	}

	// WHERE without YIELD: scope is every default column, output is every column.
	cols, allRows := mustShow(t, eng, `SHOW CONSTRAINTS WHERE type = 'NOT_NULL'`)
	if want := []string{"name", "type", "entityType", "labelsOrTypes", "properties"}; !reflect.DeepEqual(cols, want) {
		t.Errorf("columns = %v, want the full default set %v", cols, want)
	}
	if len(allRows) != 1 || allRows[0].scalars["name"] != "nn" {
		t.Fatalf("WHERE-only = %v, want only nn", allRows)
	}
	assertList(t, allRows[0], "labelsOrTypes", []string{"Person"})

	// A predicate over a list column (labelsOrTypes) with IN.
	_, listRows := mustShow(t, eng, `SHOW CONSTRAINTS YIELD name, labelsOrTypes WHERE 'Person' IN labelsOrTypes`)
	if len(listRows) != 2 {
		t.Fatalf("IN-list predicate matched %d rows, want 2: %v", len(listRows), listRows)
	}
}

// TestShow_YieldWhereParam verifies a WHERE predicate can reference a query
// parameter.
func TestShow_YieldWhereParam(t *testing.T) {
	t.Parallel()
	eng := cypher.NewEngine(lpg.New[string, float64](adjlist.Config{}))
	ctx := context.Background()
	for _, s := range []string{
		`CREATE INDEX ih FOR (n:Person) ON (n.email)`,
		`CREATE INDEX ib FOR (n:Person) ON (n.age) OPTIONS {indexType: 'btree'}`,
	} {
		if _, err := eng.Run(ctx, s, nil); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}

	res, err := eng.Run(ctx, `SHOW INDEXES YIELD name, type WHERE type = $kind`,
		map[string]expr.Value{"kind": expr.StringValue("btree")})
	if err != nil {
		t.Fatalf("Run with param: %v", err)
	}
	_, rows := collectShow(t, res)
	if len(rows) != 1 || rows[0].scalars["name"] != "ib" {
		t.Fatalf("param WHERE = %v, want only ib", rows)
	}
}

// TestShow_YieldReturn verifies a RETURN projection over the yielded scope,
// including aliasing and RETURN *.
func TestShow_YieldReturn(t *testing.T) {
	t.Parallel()
	eng := cypher.NewEngine(lpg.New[string, float64](adjlist.Config{}))
	ctx := context.Background()
	if _, err := eng.Run(ctx, `CREATE CONSTRAINT c1 FOR (n:Person) REQUIRE n.email IS UNIQUE`, nil); err != nil {
		t.Fatalf("CREATE CONSTRAINT: %v", err)
	}

	// RETURN with a subset and an alias.
	cols, rows := mustShow(t, eng, `SHOW CONSTRAINTS YIELD name, type RETURN name AS cname`)
	if want := []string{"cname"}; !reflect.DeepEqual(cols, want) {
		t.Errorf("columns = %v, want %v", cols, want)
	}
	if len(rows) != 1 || rows[0].scalars["cname"] != "c1" {
		t.Fatalf("RETURN alias = %v, want cname=c1", rows)
	}

	// RETURN * returns the yielded columns.
	starCols, starRows := mustShow(t, eng, `SHOW CONSTRAINTS YIELD name, type RETURN *`)
	if want := []string{"name", "type"}; !reflect.DeepEqual(starCols, want) {
		t.Errorf("RETURN * columns = %v, want %v", starCols, want)
	}
	if len(starRows) != 1 || starRows[0].scalars["name"] != "c1" || starRows[0].scalars["type"] != "UNIQUE" {
		t.Fatalf("RETURN * = %v, want name=c1 type=UNIQUE", starRows)
	}
}

// TestShow_YieldDeterministicOrder confirms a projected/filtered result preserves
// the deterministic name ordering of the underlying SHOW output.
func TestShow_YieldDeterministicOrder(t *testing.T) {
	t.Parallel()
	eng := cypher.NewEngine(lpg.New[string, float64](adjlist.Config{}))
	ctx := context.Background()
	for _, name := range []string{"zeta", "alpha", "mike"} {
		if _, err := eng.Run(ctx, `CREATE INDEX `+name+` FOR (n:L) ON (n.p_`+name+`)`, nil); err != nil {
			t.Fatalf("CREATE INDEX %s: %v", name, err)
		}
	}
	want := []string{"alpha", "mike", "zeta"}
	for run := 0; run < 3; run++ {
		_, rows := mustShow(t, eng, `SHOW INDEXES YIELD name`)
		got := make([]string, len(rows))
		for i, r := range rows {
			got[i] = r.scalars["name"]
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d: order = %v, want %v", run, got, want)
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
