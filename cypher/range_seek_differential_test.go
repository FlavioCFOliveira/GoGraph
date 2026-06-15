package cypher_test

// range_seek_differential_test.go — #1505 result-identity gate.
//
// Every range query is run twice: once with the range-index seek ENABLED
// (default) and once DISABLED (EngineOptions.DisableRangeIndexSeek). The two
// result multisets must be byte-identical. This is the primary guarantee that
// the seek is result-identical to the NodeByLabelScan+Filter it replaces,
// across the openCypher hazards the cypher-expert-consultant flagged: null /
// missing properties, mixed-type properties under the same key, open vs closed
// bounds, two-sided ranges, empty ranges, and ORDER BY interaction.

import (
	"context"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// buildDiffGraph builds a :Person graph of n nodes whose "name" property
// ranges over name0000..nameNNNN, plus a handful of adversarial nodes that
// exercise the openCypher hazards:
//
//   - a node with NO "name" property (must be excluded by both plans),
//   - a node whose "name" is the wrong TYPE (an integer, not a string) under
//     the same key (a string range must exclude it — comparability),
//   - a node with a different label entirely (must never be matched).
//
// The population is ≥ rangeSeekMinLabelPopulation so the seek can fire.
func buildDiffGraph(t *testing.T, n int) *lpg.Graph[string, float64] {
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
		key := nameKey(i)
		add(key, "Person")
		if err := g.SetNodeProperty(key, "name", lpg.StringValue(nameVal(i))); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
	}
	// Adversarial: missing property.
	add("p_noprop", "Person")
	// Adversarial: wrong-typed property under the same key (integer).
	add("p_intname", "Person")
	if err := g.SetNodeProperty("p_intname", "name", lpg.Int64Value(42)); err != nil {
		t.Fatalf("SetNodeProperty int: %v", err)
	}
	// Adversarial: different label.
	add("c_city", "City")
	if err := g.SetNodeProperty("c_city", "name", lpg.StringValue("name0001")); err != nil {
		t.Fatalf("SetNodeProperty city: %v", err)
	}
	return g
}

func nameKey(i int) string {
	return "p" + pad4(i)
}

func nameVal(i int) string {
	return "name" + pad4(i)
}

func pad4(i int) string {
	const d = "0123456789"
	b := []byte{'0', '0', '0', '0'}
	for k := 3; k >= 0 && i > 0; k-- {
		b[k] = d[i%10]
		i /= 10
	}
	return string(b)
}

// newDiffEngine builds an engine over a fresh copy of the graph with the seek
// either enabled or disabled, with the bound string btree on (:Person, name).
func newDiffEngine(t *testing.T, n int, disableSeek bool) *cypher.Engine {
	t.Helper()
	g := buildDiffGraph(t, n)
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{DisableRangeIndexSeek: disableSeek})
	if _, err := eng.Run(context.Background(),
		`CREATE INDEX FOR (n:Person) ON (n.name) OPTIONS {indexType:'btree'}`, nil); err != nil {
		t.Fatalf("CREATE INDEX: %v", err)
	}
	return eng
}

// runRows executes q and returns every row's first column rendered as a stable
// string, sorted, so two runs are compared as multisets.
func runRows(t *testing.T, eng *cypher.Engine, q string) []string {
	t.Helper()
	res, err := eng.Run(context.Background(), q, nil)
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

func renderValue(v expr.Value) string {
	if v == nil || expr.IsNull(v) {
		return "<null>"
	}
	return v.String()
}

// assertIdentical runs q with the seek enabled and disabled and asserts the
// result multisets are identical. ordered controls whether order is also
// compared (for ORDER BY queries).
func assertIdentical(t *testing.T, n int, q string, ordered bool) {
	t.Helper()
	engOn := newDiffEngine(t, n, false)
	engOff := newDiffEngine(t, n, true)
	got := runRows(t, engOn, q)
	want := runRows(t, engOff, q)
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

func TestRangeSeekDifferential(t *testing.T) {
	t.Parallel()
	const n = 2000
	cases := []struct {
		name    string
		query   string
		ordered bool
	}{
		{"gt_open_selective", `MATCH (p:Person) WHERE p.name > "name0000" AND p.name < "name0010" RETURN p.name`, false},
		{"ge_closed_lo", `MATCH (p:Person) WHERE p.name >= "name0005" AND p.name <= "name0009" RETURN p.name`, false},
		{"lt_only_selective", `MATCH (p:Person) WHERE p.name < "name0007" RETURN p.name`, false},
		{"le_only_selective", `MATCH (p:Person) WHERE p.name <= "name0007" RETURN p.name`, false},
		{"gt_only_high_selective", `MATCH (p:Person) WHERE p.name > "name1995" RETURN p.name`, false},
		{"two_sided_mixed_incl", `MATCH (p:Person) WHERE p.name >= "name0003" AND p.name < "name0008" RETURN p.name`, false},
		{"empty_range", `MATCH (p:Person) WHERE p.name > "name0005" AND p.name < "name0005" RETURN p.name`, false},
		{"empty_range_no_match", `MATCH (p:Person) WHERE p.name > "zzzz" RETURN p.name`, false},
		{"matches_wrong_type_excluded", `MATCH (p:Person) WHERE p.name >= "0" AND p.name < "9" RETURN p.name`, false},
		{"order_by_asc", `MATCH (p:Person) WHERE p.name >= "name0000" AND p.name < "name0012" RETURN p.name ORDER BY p.name`, true},
		{"order_by_desc", `MATCH (p:Person) WHERE p.name >= "name0000" AND p.name < "name0012" RETURN p.name ORDER BY p.name DESC`, true},
		{"return_node_count", `MATCH (p:Person) WHERE p.name >= "name0000" AND p.name < "name0010" RETURN count(p)`, false},
		// Non-selective range (≈ whole population): both plans must agree, and
		// the seek must NOT fire (selectivity gate) — still identical results.
		{"non_selective_full", `MATCH (p:Person) WHERE p.name >= "name0000" RETURN p.name`, false},
		// Mirror form: literal on the left.
		{"mirror_form", `MATCH (p:Person) WHERE "name0010" > p.name RETURN p.name`, false},
		// Extra conjunct beyond the range: residual filter must still apply.
		{"extra_conjunct", `MATCH (p:Person) WHERE p.name >= "name0000" AND p.name < "name0050" AND p.name <> "name0003" RETURN p.name`, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertIdentical(t, n, tc.query, tc.ordered)
		})
	}
}

// TestRangeSeekDifferential_AfterMutation exercises the bound index's
// self-maintenance: inserts, updates (old key must be dropped), property
// deletes, and node deletes must all keep the seek result identical to the
// scan+filter result.
func TestRangeSeekDifferential_AfterMutation(t *testing.T) {
	t.Parallel()
	const n = 1500
	write := func(eng *cypher.Engine, q string) {
		res, err := eng.RunInTx(context.Background(), q, nil)
		if err != nil {
			t.Fatalf("write %q: %v", q, err)
		}
		for res.Next() { //nolint:revive // drain to commit the write
		}
		if err := res.Err(); err != nil {
			t.Fatalf("write iter %q: %v", q, err)
		}
	}
	mutate := func(eng *cypher.Engine) {
		// Insert a new in-range node.
		write(eng, `CREATE (p:Person {name: "name0001x"})`)
		// Update an existing node's name OUT of a selective range (old key must
		// be removed from the index, else the seek over-returns).
		write(eng, `MATCH (p:Person {name: "name0002"}) SET p.name = "zzz_moved"`)
		// Remove a property (node must drop out of the index).
		write(eng, `MATCH (p:Person {name: "name0004"}) REMOVE p.name`)
		// Delete a node (must drop out of the index).
		write(eng, `MATCH (p:Person {name: "name0006"}) DELETE p`)
	}
	engOn := newDiffEngine(t, n, false)
	engOff := newDiffEngine(t, n, true)
	mutate(engOn)
	mutate(engOff)

	q := `MATCH (p:Person) WHERE p.name >= "name0000" AND p.name < "name0010" RETURN p.name`
	got := runRows(t, engOn, q)
	want := runRows(t, engOff, q)
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("post-mutation count mismatch ON=%d OFF=%d\nON=%v\nOFF=%v", len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("post-mutation row %d mismatch ON=%q OFF=%q", i, got[i], want[i])
		}
	}
}
