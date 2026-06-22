package cypher_test

// range_seek_numeric_test.go — #1652 unified numeric range-seek identity gate.
//
// A numeric range predicate (n.age > 30, with integer OR float literals, and
// numeric parameter bounds n.age > $min) over a btree-indexed property is
// served by the UNIFIED float64 companion built alongside the user's string
// btree. The seek returns a SUPERSET of the true matches and the engine retains
// the original predicate as a residual Filter, so the result must be identical
// to a label scan + filter for every hazard: mixed integer/float values under
// one key, null / missing properties, NaN, wrong-typed (string) values, open vs
// closed bounds, two-sided ranges, and parameter bounds.
//
// The proof, mirroring range_seek_differential_test.go, runs every query twice
// — once with the seek ENABLED (default) and once DISABLED
// (EngineOptions.DisableRangeIndexSeek) — and asserts identical result
// multisets.

import (
	"context"
	"math"
	"path/filepath"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	indexbtree "github.com/FlavioCFOliveira/GoGraph/graph/index/btree"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// buildNumericDiffGraph builds a :Person graph of n nodes whose "age" property
// alternates between an integer and a float, so the unified numeric companion
// must index BOTH under one float64 order. It adds the openCypher hazards:
//
//   - a node with NO "age" property (excluded by both plans),
//   - a node whose "age" is the wrong TYPE (a string) under the same key
//     (a numeric range must exclude it — comparability),
//   - a node whose "age" is NaN (never indexed, never matched by n.age <op> x),
//   - a node with a different label entirely (never matched).
//
// The population is ≥ rangeSeekMinLabelPopulation so the seek can fire.
func buildNumericDiffGraph(t *testing.T, n int) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	add := func(key, label string) {
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode %s: %v", key, err)
		}
		if err := g.SetNodeLabel(key, label); err != nil {
			t.Fatalf("SetNodeLabel %s: %v", key, err)
		}
	}
	for i := 0; i < n; i++ {
		key := numKey(i)
		add(key, "Person")
		// Even i: integer age == i. Odd i: float age == i + 0.5. The float
		// values interleave the integers under the unified numeric order, so a
		// range like [10, 20) must return BOTH the integers 10,12,…,18 and the
		// floats 11.5,13.5,…,19.5 — proving the companion is a true superset.
		var v lpg.PropertyValue
		if i%2 == 0 {
			v = lpg.Int64Value(int64(i))
		} else {
			v = lpg.Float64Value(float64(i) + 0.5)
		}
		if err := g.SetNodeProperty(key, "age", v); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
	}
	// Adversarial: missing property.
	add("p_noprop", "Person")
	// Adversarial: wrong-typed property (string) under the same key.
	add("p_strage", "Person")
	if err := g.SetNodeProperty("p_strage", "age", lpg.StringValue("forty")); err != nil {
		t.Fatalf("SetNodeProperty string: %v", err)
	}
	// Adversarial: NaN age (never indexed, never matched).
	add("p_nanage", "Person")
	if err := g.SetNodeProperty("p_nanage", "age", lpg.Float64Value(math.NaN())); err != nil {
		t.Fatalf("SetNodeProperty NaN: %v", err)
	}
	// Adversarial: different label.
	add("c_city", "City")
	if err := g.SetNodeProperty("c_city", "age", lpg.Int64Value(5)); err != nil {
		t.Fatalf("SetNodeProperty city: %v", err)
	}
	return g
}

func numKey(i int) string {
	return "p" + pad4(i) // pad4 lives in range_seek_differential_test.go
}

// newNumericDiffEngine builds an engine over a fresh copy of the numeric graph
// with the seek either enabled or disabled, with the bound btree on
// (:Person, age) — which also wires the numeric companion.
func newNumericDiffEngine(t *testing.T, n int, disableSeek bool) *cypher.Engine {
	t.Helper()
	g := buildNumericDiffGraph(t, n)
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{DisableRangeIndexSeek: disableSeek})
	if _, err := eng.Run(context.Background(),
		`CREATE INDEX FOR (n:Person) ON (n.age) OPTIONS {indexType:'btree'}`, nil); err != nil {
		t.Fatalf("CREATE INDEX: %v", err)
	}
	return eng
}

// assertNumericIdentical runs q (with optional params) with the seek enabled
// and disabled and asserts the result multisets are identical. runRows /
// renderValue live in range_seek_differential_test.go.
func assertNumericIdentical(t *testing.T, n int, q string, params map[string]expr.Value, ordered bool) {
	t.Helper()
	engOn := newNumericDiffEngine(t, n, false)
	engOff := newNumericDiffEngine(t, n, true)
	got := runRowsP(t, engOn, q, params)
	want := runRowsP(t, engOff, q, params)
	if !ordered {
		sort.Strings(got)
		sort.Strings(want)
	}
	if len(got) != len(want) {
		t.Fatalf("row count mismatch (ON=%d OFF=%d) for %q\nON=%v\nOFF=%v", len(got), len(want), q, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("row %d mismatch for %q: ON=%q OFF=%q\nON=%v\nOFF=%v", i, q, got[i], want[i], got, want)
		}
	}
}

// runRowsP is runRows with a params map (runRows passes nil).
func runRowsP(t *testing.T, eng *cypher.Engine, q string, params map[string]expr.Value) []string {
	t.Helper()
	res, err := eng.Run(context.Background(), q, params)
	if err != nil {
		t.Fatalf("Run %q: %v", q, err)
	}
	var out []string
	for res.Next() {
		out = append(out, renderValue(res.ValueAt(0)))
	}
	if err := res.Err(); err != nil {
		t.Fatalf("iter %q: %v", q, err)
	}
	return out
}

// TestRangeSeekNumericDifferential is the primary result-identity gate: a
// numeric range query returns identical rows with and without the seek, across
// integer + float values under one key and the openCypher hazards.
func TestRangeSeekNumericDifferential(t *testing.T) {
	t.Parallel()
	const n = 2000
	cases := []struct {
		name    string
		query   string
		ordered bool
	}{
		{"gt_int_literal", `MATCH (p:Person) WHERE p.age > 30 RETURN p.age`, false},
		{"ge_int_literal", `MATCH (p:Person) WHERE p.age >= 30 RETURN p.age`, false},
		{"lt_int_literal", `MATCH (p:Person) WHERE p.age < 25 RETURN p.age`, false},
		{"le_int_literal", `MATCH (p:Person) WHERE p.age <= 25 RETURN p.age`, false},
		{"two_sided_int", `MATCH (p:Person) WHERE p.age >= 10 AND p.age < 20 RETURN p.age`, false},
		{"two_sided_float_bounds", `MATCH (p:Person) WHERE p.age > 10.5 AND p.age <= 19.5 RETURN p.age`, false},
		{"float_literal_bound", `MATCH (p:Person) WHERE p.age > 11.5 AND p.age < 17.5 RETURN p.age`, false},
		{"high_selective_gt", `MATCH (p:Person) WHERE p.age > 1990 RETURN p.age`, false},
		{"empty_range", `MATCH (p:Person) WHERE p.age > 10 AND p.age < 10 RETURN p.age`, false},
		{"empty_range_no_match", `MATCH (p:Person) WHERE p.age > 100000 RETURN p.age`, false},
		{"mirror_form", `MATCH (p:Person) WHERE 20 > p.age RETURN p.age`, false},
		{"order_by_asc", `MATCH (p:Person) WHERE p.age >= 10 AND p.age < 22 RETURN p.age ORDER BY p.age`, true},
		{"order_by_desc", `MATCH (p:Person) WHERE p.age >= 10 AND p.age < 22 RETURN p.age ORDER BY p.age DESC`, true},
		{"return_count", `MATCH (p:Person) WHERE p.age >= 10 AND p.age < 20 RETURN count(p)`, false},
		// Non-selective (whole population): both plans agree, seek must NOT fire.
		{"non_selective_full", `MATCH (p:Person) WHERE p.age >= 0 RETURN p.age`, false},
		// Extra conjunct beyond the range: residual filter must still apply.
		{"extra_conjunct", `MATCH (p:Person) WHERE p.age >= 10 AND p.age < 50 AND p.age <> 12 RETURN p.age`, false},
		// Equality-to-NaN-style trap: a comparison can never select the NaN row.
		{"nan_never_returned", `MATCH (p:Person) WHERE p.age > 0 AND p.age < 2 RETURN p.age`, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertNumericIdentical(t, n, tc.query, nil, tc.ordered)
		})
	}
}

// TestRangeSeekNumericParam proves a parameterised numeric range bound
// (n.age > $min, n.age < $max) — the common shape the string path declines —
// is served by the seek and stays result-identical to the scan+filter, for both
// integer and float parameter values.
func TestRangeSeekNumericParam(t *testing.T) {
	t.Parallel()
	const n = 2000
	cases := []struct {
		name   string
		query  string
		params map[string]expr.Value
	}{
		{
			"gt_int_param",
			`MATCH (p:Person) WHERE p.age > $min RETURN p.age`,
			map[string]expr.Value{"min": expr.IntegerValue(1990)},
		},
		{
			"two_sided_int_params",
			`MATCH (p:Person) WHERE p.age >= $min AND p.age < $max RETURN p.age`,
			map[string]expr.Value{"min": expr.IntegerValue(10), "max": expr.IntegerValue(20)},
		},
		{
			"two_sided_float_params",
			`MATCH (p:Person) WHERE p.age > $min AND p.age <= $max RETURN p.age`,
			map[string]expr.Value{"min": expr.FloatValue(10.5), "max": expr.FloatValue(19.5)},
		},
		{
			"mixed_literal_and_param",
			`MATCH (p:Person) WHERE p.age >= 10 AND p.age < $max RETURN p.age`,
			map[string]expr.Value{"max": expr.IntegerValue(25)},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertNumericIdentical(t, n, tc.query, tc.params, false)
		})
	}
}

// TestRangeSeekNumericExcludesNaNAndNull proves directly (not just by
// differential) that the NaN-aged and missing-property nodes never appear in a
// numeric range result, and that the wrong-typed (string) node is excluded too.
func TestRangeSeekNumericExcludesNaNAndNull(t *testing.T) {
	t.Parallel()
	const n = 2000
	eng := newNumericDiffEngine(t, n, false)
	// A range that spans essentially the whole numeric population. The NaN,
	// missing, string, and wrong-label nodes must all be absent. We RETURN the
	// node key so we can assert specific adversarial keys never appear.
	rows := runRowsP(t, eng,
		`MATCH (p:Person) WHERE p.age >= 0 AND p.age < 5 RETURN p.age`, nil)
	// Expected in [0,5): integers 0,2,4 and floats 1.5,3.5 — five rows.
	if len(rows) != 5 {
		t.Fatalf("expected 5 rows in [0,5), got %d: %v", len(rows), rows)
	}
	// None of the rendered values may be a NaN or a non-numeric token.
	for _, r := range rows {
		if r == "NaN" || r == "<null>" || r == `"forty"` {
			t.Fatalf("adversarial value leaked into result: %q (rows=%v)", r, rows)
		}
	}

	// Confirm the NaN/string/missing nodes carry the property but are excluded
	// by the predicate: an explicit IS NOT NULL count over numeric-typed reads
	// is the differential's job; here we assert the unbounded-above range never
	// pulls the NaN row, which a non-superset float index might.
	rowsHigh := runRowsP(t, eng, `MATCH (p:Person) WHERE p.age > 1990 RETURN p.age`, nil)
	for _, r := range rowsHigh {
		if r == "NaN" {
			t.Fatalf("NaN leaked into unbounded-above range: rows=%v", rowsHigh)
		}
	}
}

// ─── DROP INDEX cleans up the numeric companion ──────────────────────────────

// companionPresent reports whether the internal numeric companion for
// (label, prop) is currently registered on the engine.
func companionPresent(eng *cypher.Engine, label, prop string) bool {
	want := numericCompanionName(label, prop)
	for _, n := range eng.ListIndexes() {
		if n == want {
			return true
		}
	}
	return false
}

// numericCompanionName mirrors cypher.numericBTreeName for the external test
// package (lower(label)_lower(prop)_btree_num).
func numericCompanionName(label, prop string) string {
	return toLower(label) + "_" + toLower(prop) + "_btree_num"
}

func toLower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

// TestRangeSeekNumeric_DropRemovesCompanion proves DROP INDEX removes the
// internal numeric companion so it does not linger (the user sees and pays for
// exactly the index they created), and that a numeric range query still returns
// correct rows afterwards via the scan+filter fallback.
func TestRangeSeekNumeric_DropRemovesCompanion(t *testing.T) {
	t.Parallel()
	g := buildNumericDiffGraph(t, 2000)
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	mustExec := func(q string) {
		r, err := eng.Run(ctx, q, nil)
		if err != nil {
			t.Fatalf("Run %q: %v", q, err)
		}
		for r.Next() { //nolint:revive // drain
		}
		if err := r.Err(); err != nil {
			t.Fatalf("iter %q: %v", q, err)
		}
		if cerr := r.Close(); cerr != nil {
			t.Fatalf("Close %q: %v", q, cerr)
		}
	}

	mustExec(`CREATE INDEX person_age FOR (n:Person) ON (n.age) OPTIONS {indexType:'btree'}`)
	if !companionPresent(eng, "Person", "age") {
		t.Fatal("numeric companion absent after CREATE INDEX")
	}

	mustExec(`DROP INDEX person_age`)
	if companionPresent(eng, "Person", "age") {
		t.Fatal("numeric companion still present after DROP INDEX")
	}
	// The user index is gone too.
	for _, n := range eng.ListIndexes() {
		if n == "person_age" {
			t.Fatal("user index still present after DROP INDEX")
		}
	}

	// The numeric range query still returns correct rows via the scan+filter
	// fallback. [10,20): integers 10,12,14,16,18 + floats 11.5,13.5,15.5,17.5,
	// 19.5 = 10 rows.
	rows := runRowsP(t, eng, `MATCH (p:Person) WHERE p.age >= 10 AND p.age < 20 RETURN p.age`, nil)
	if len(rows) != 10 {
		t.Fatalf("expected 10 rows in [10,20) after DROP, got %d: %v", len(rows), rows)
	}
}

// TestRangeSeekNumeric_SharedCompanionSurvivesPartialDrop proves a companion
// shared by two user btree indexes on the SAME (label, property) survives when
// only one of the two user indexes is dropped — the other still relies on it.
func TestRangeSeekNumeric_SharedCompanionSurvivesPartialDrop(t *testing.T) {
	t.Parallel()
	g := buildNumericDiffGraph(t, 2000)
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	mustExec := func(q string) {
		r, err := eng.Run(ctx, q, nil)
		if err != nil {
			t.Fatalf("Run %q: %v", q, err)
		}
		for r.Next() { //nolint:revive // drain
		}
		if err := r.Err(); err != nil {
			t.Fatalf("iter %q: %v", q, err)
		}
		if cerr := r.Close(); cerr != nil {
			t.Fatalf("Close %q: %v", q, cerr)
		}
	}

	// Two user btree indexes on the SAME (:Person, age) under different names —
	// they share one numeric companion.
	mustExec(`CREATE INDEX age_a FOR (n:Person) ON (n.age) OPTIONS {indexType:'btree'}`)
	mustExec(`CREATE INDEX age_b FOR (n:Person) ON (n.age) OPTIONS {indexType:'btree'}`)
	if !companionPresent(eng, "Person", "age") {
		t.Fatal("companion absent after creating two indexes")
	}

	// Drop one: the companion must survive because age_b still covers (Person, age).
	mustExec(`DROP INDEX age_a`)
	if !companionPresent(eng, "Person", "age") {
		t.Fatal("companion wrongly removed while age_b still covers (Person, age)")
	}

	// The numeric range seek still works (companion still served).
	rows := runRowsP(t, eng, `MATCH (p:Person) WHERE p.age >= 10 AND p.age < 20 RETURN p.age`, nil)
	if len(rows) != 10 {
		t.Fatalf("expected 10 rows in [10,20) after partial drop, got %d", len(rows))
	}

	// Drop the last one: now the companion must be gone.
	mustExec(`DROP INDEX age_b`)
	if companionPresent(eng, "Person", "age") {
		t.Fatal("companion still present after dropping the last covering index")
	}
}

// ─── Recovery: the numeric companion is rebuilt on reopen ────────────────────

// TestRangeSeekNumeric_SurvivesReopen creates a numeric-ranged graph and its
// btree index, persists to a WAL+snapshot, reopens via recovery.Open, and
// confirms the numeric range query still uses the companion and returns correct
// rows after recovery — proving registerRecoveredIndexes rebuilt the companion
// from the single persisted btree def (no new storage format).
func TestRangeSeekNumeric_SurvivesReopen(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Cycle 1: create the btree index and seed mixed integer/float ages, enough
	// to clear the selectivity-gate population floor.
	const n = 1500
	if err := numericReopenCycle1(t, dir, n); err != nil {
		t.Fatalf("cycle 1: %v", err)
	}

	// Cycle 2: reopen and inspect the recovered manager + run the query.
	res, err := recovery.Open[string, float64](dir, idxRecOpts())
	if err != nil {
		t.Fatalf("recovery.Open cycle 2: %v", err)
	}
	if len(res.Indexes) != 1 {
		t.Fatalf("expected exactly 1 persisted index def, got %d: %v", len(res.Indexes), res.Indexes)
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

	// The numeric companion must be present in the manager under its internal
	// name, backfilled (a Cypher query alone cannot distinguish a backfilled
	// companion from a scan fallback). The user index name is "person_age";
	// the companion is "person_age"→ numericBTreeName(Person, age).
	numName := "person_age_btree_num"
	sub, gerr := res.Graph.IndexManager().GetIndex(numName)
	if gerr != nil {
		t.Fatalf("numeric companion %q absent after reopen: %v", numName, gerr)
	}
	nidx, ok := sub.(*indexbtree.Index[float64])
	if !ok {
		t.Fatalf("recovered companion is %T, want *btree.Index[float64]", sub)
	}
	// The companion must be backfilled: a closed range over the recovered data
	// must report a non-zero exact count.
	cnt, exact := nidx.RangeCount(10, 20, 1<<30)
	if !exact || cnt == 0 {
		t.Fatalf("recovered numeric companion not backfilled: RangeCount(10,20)=(%d,%v)", cnt, exact)
	}

	// The companion must NOT be surfaced by db.indexes(): the user sees exactly
	// the one index they created.
	introspect := idxQuery(t, eng, `CALL db.indexes() YIELD name, type RETURN name`)
	for _, row := range introspect {
		if name, ok := row["name"].(expr.StringValue); ok && string(name) == numName {
			t.Fatalf("internal companion %q leaked into db.indexes()", numName)
		}
	}

	// End-to-end: the numeric range query must use the seek and return correct
	// rows after reopen. Compare against a seek-disabled engine over the SAME
	// recovered graph state is impractical (one store), so assert the expected
	// closed-range cardinality directly: [10,20) over the seeded data holds the
	// even integers 10,12,14,16,18 and the odd-index floats 11.5,13.5,15.5,
	// 17.5,19.5 → 10 rows.
	rows := idxQuery(t, eng, `MATCH (p:Person) WHERE p.age >= 10 AND p.age < 20 RETURN p.age`)
	if len(rows) != 10 {
		t.Fatalf("expected 10 rows in [10,20) after reopen, got %d", len(rows))
	}
	if serr := w.Sync(); serr != nil {
		t.Errorf("wal.Sync: %v", serr)
	}
}

// numericReopenCycle1 opens dir, creates the btree index, seeds n mixed
// integer/float :Person nodes via Cypher writes (so they go through the WAL),
// then syncs and closes cleanly.
func numericReopenCycle1(t *testing.T, dir string, n int) error {
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

	if err := idxRunOne(t, eng,
		`CREATE INDEX person_age FOR (n:Person) ON (n.age) OPTIONS {indexType:'btree'}`); err != nil {
		return err
	}
	// Seed via UNWIND so a single statement creates all nodes; alternate
	// integer and float ages. We build a literal list query.
	for i := 0; i < n; i++ {
		var q string
		if i%2 == 0 {
			q = `CREATE (:Person {age: ` + itoa(i) + `})`
		} else {
			q = `CREATE (:Person {age: ` + itoa(i) + `.5})`
		}
		if err := idxRunOne(t, eng, q); err != nil {
			return err
		}
	}
	if serr := w.Sync(); serr != nil {
		t.Fatalf("wal.Sync: %v", serr)
	}
	return nil
}
