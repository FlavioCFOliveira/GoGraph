package cypher

// prefix_seek_differential_test.go — result-identity gate for the STARTS WITH
// prefix range seek (#2128).
//
// Every case is checked THREE ways, not two:
//
//  1. against the rewrite DISABLED (DisablePrefixIndexSeek), which catches a
//     rewrite that changes an answer;
//  2. against an ABSOLUTE oracle computed in Go straight from the fixture with
//     strings.HasPrefix, which catches a defect the two arms would SHARE — both
//     arms run the same residual Filter, so a wrong filter is invisible to the
//     differential alone;
//  3. against the PLAN, asserting the two arms actually differ where the rewrite
//     is meant to fire. A differential whose arms silently take the same plan is
//     green for the wrong reason and proves nothing.
//
// The design, its superset proof and its scope boundaries are in
// docs/design-prefix-range-seek.md.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// pfxDiffPop is the number of ordinary "name%04d" nodes. Comfortably above
// rangeSeekMinLabelPopulation (1024) so the population floor never explains a
// declined case, and small enough to keep the suite inside the short layer.
const pfxDiffPop = 2000

// pfxDiffLabel is the label under test; pfxDiffOther is a decoy label carrying
// the same property so a label-crossing bug is visible.
const (
	pfxDiffLabel = "PfxDiff"
	pfxDiffOther = "PfxOther"
)

// pfxExtra are the adversarial string values appended to the ordinary
// population. They cover the input classes the rewrite must not get wrong:
// multi-byte UTF-8, a combining sequence against its precomposed twin, the
// maximum code point, values that are proper prefixes of other values, and the
// empty string.
var pfxExtra = []string{
	"",                // empty value — matched by the empty prefix only
	"na",              // proper prefix of "name…"
	"nam",             // proper prefix of "name…"
	"name",            // proper prefix of "name0000"
	"café",            // precomposed é (U+00E9)
	"café",           // e + combining acute (U+0301) — a DIFFERENT string
	"caf",             // proper prefix of both café spellings
	"naïve",           // multi-byte inside the value
	"日本語",             // wholly multi-byte
	"日本",              // proper prefix of the above
	"z\U0010ffff",     // ends at the maximum code point
	"z\U0010ffffTAIL", // extends past it
	"ÿ",               // U+00FF — the byte-successor edge (C3 BF → C3 C0)
	"ÿz",
	"Ā", // U+0100 — sorts just above the ÿ range; must NOT be captured
}

// buildPrefixDiffGraph seeds the fixture and returns the ground-truth mapping
// from node key to its "name" value for every node that carries a STRING name
// under pfxDiffLabel. That map is the oracle's input: it is built from what the
// test itself wrote, independently of anything the engine does.
func buildPrefixDiffGraph(t *testing.T) (*lpg.Graph[string, float64], map[string]string) {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	truth := make(map[string]string, pfxDiffPop+len(pfxExtra))

	add := func(key, label string) {
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode %q: %v", key, err)
		}
		if err := g.SetNodeLabel(key, label); err != nil {
			t.Fatalf("SetNodeLabel %q: %v", key, err)
		}
	}
	setName := func(key, val string) {
		if err := g.SetNodeProperty(key, "name", lpg.StringValue(val)); err != nil {
			t.Fatalf("SetNodeProperty %q: %v", key, err)
		}
	}

	for i := 0; i < pfxDiffPop; i++ {
		key := fmt.Sprintf("p%04d", i)
		val := fmt.Sprintf("name%04d", i)
		add(key, pfxDiffLabel)
		setName(key, val)
		// An unindexed twin of the same value, so a query on "note" exercises the
		// no-covering-index path over identical data.
		if err := g.SetNodeProperty(key, "note", lpg.StringValue(val)); err != nil {
			t.Fatalf("SetNodeProperty note: %v", err)
		}
		truth[key] = val
	}
	for i, val := range pfxExtra {
		key := fmt.Sprintf("x%03d", i)
		add(key, pfxDiffLabel)
		setName(key, val)
		truth[key] = val
	}

	// Adversarial rows that must never appear in a prefix result.
	//
	// A missing property and a non-string property are excluded by BOTH sides by
	// construction: they are not in the string btree (projectStringPropValue) and
	// `x STARTS WITH p` on a non-string yields NULL, which WHERE drops (TCK
	// String8 [8]). Neither is entered into `truth`.
	add("p_noname", pfxDiffLabel)
	add("p_intname", pfxDiffLabel)
	if err := g.SetNodeProperty("p_intname", "name", lpg.Int64Value(42)); err != nil {
		t.Fatalf("SetNodeProperty int: %v", err)
	}
	add("p_boolname", pfxDiffLabel)
	if err := g.SetNodeProperty("p_boolname", "name", lpg.BoolValue(true)); err != nil {
		t.Fatalf("SetNodeProperty bool: %v", err)
	}
	// A different label carrying a name that WOULD match several prefixes under
	// test — so a rewrite that lost the label restriction would be caught.
	add("o_other", pfxDiffOther)
	setName("o_other", "name0001")

	return g, truth
}

// newPrefixDiffEngine builds an engine over a fresh fixture with the prefix
// rewrite enabled or disabled, and the bound string btree on (:PfxDiff, name).
func newPrefixDiffEngine(t *testing.T, disablePrefix bool) (*Engine, map[string]string) {
	t.Helper()
	g, truth := buildPrefixDiffGraph(t)
	eng := NewEngineWithOptions(g, EngineOptions{
		DisablePrefixIndexSeek: disablePrefix,
		MaxResultRows:          MaxResultRowsUnlimited,
	})
	if _, err := eng.Run(context.Background(),
		`CREATE INDEX FOR (n:`+pfxDiffLabel+`) ON (n.name) OPTIONS {indexType:'btree'}`, nil); err != nil {
		t.Fatalf("CREATE INDEX: %v", err)
	}
	return eng, truth
}

// prefixOracle is the ABSOLUTE expected answer for `n.name STARTS WITH prefix`
// over the fixture: the sorted multiset of name values satisfying Go's own
// strings.HasPrefix. It never consults the engine, so it cannot share a defect
// with either differential arm.
func prefixOracle(truth map[string]string, prefix string) []string {
	out := make([]string, 0, 16)
	for _, val := range truth {
		if strings.HasPrefix(val, prefix) {
			out = append(out, val)
		}
	}
	sort.Strings(out)
	return out
}

// runPrefixRows executes q and returns the first column as sorted strings.
func runPrefixRows(t *testing.T, eng *Engine, q string, params map[string]expr.Value) []string {
	t.Helper()
	res, err := eng.Run(context.Background(), q, params)
	if err != nil {
		t.Fatalf("Run %q: %v", q, err)
	}
	out := make([]string, 0, 16)
	for res.Next() {
		v := res.ValueAt(0)
		if v == nil || expr.IsNull(v) {
			out = append(out, "<null>")
			continue
		}
		sv, ok := v.(expr.StringValue)
		if !ok {
			t.Fatalf("non-string row value %v (%T) for %q", v, v, q)
		}
		out = append(out, string(sv))
	}
	if err := res.Err(); err != nil {
		t.Fatalf("iter %q: %v", q, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("close %q: %v", q, err)
	}
	sort.Strings(out)
	return out
}

func assertSameStrings(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d rows, want %d\ngot  = %q\nwant = %q", what, len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: row %d = %q, want %q\ngot  = %q\nwant = %q", what, i, got[i], want[i], got, want)
		}
	}
}

// TestPrefixSeekDifferential is the core gate: for every input class, the
// rewritten plan, the un-rewritten plan and the Go oracle must all agree, and
// the plans must actually differ wherever the rewrite is meant to fire.
func TestPrefixSeekDifferential(t *testing.T) {
	t.Parallel()

	engOn, truth := newPrefixDiffEngine(t, false)
	engOff, _ := newPrefixDiffEngine(t, true)

	cases := []struct {
		name string
		// prefix is the STARTS WITH operand; the query is derived from it so the
		// oracle and the query can never drift apart.
		prefix string
		// wantSeek is whether the ENABLED engine must plan NodeByIndexRangeScan.
		wantSeek bool
	}{
		{name: "selective_prefix", prefix: "name000", wantSeek: true},
		{name: "selective_prefix_deeper", prefix: "name0001", wantSeek: true},
		// Digit-boundary prefixes: the byte successor of "name009" is "name00:",
		// which is exactly what must NOT swallow "name0100".
		{name: "digit_boundary", prefix: "name009", wantSeek: true},
		{name: "two_char_tail", prefix: "name01", wantSeek: true},
		// Values that are proper prefixes of other values must be included.
		{name: "value_is_prefix_of_others", prefix: "nam", wantSeek: false},
		// Multi-byte and combining-character classes.
		{name: "multibyte_precomposed", prefix: "café", wantSeek: true},
		{name: "multibyte_decomposed", prefix: "café", wantSeek: true},
		{name: "multibyte_shared_stem", prefix: "caf", wantSeek: true},
		{name: "multibyte_cjk", prefix: "日本", wantSeek: true},
		{name: "multibyte_inner", prefix: "naïve", wantSeek: true},
		// U+00FF is the byte-successor edge: succ("ÿ") is C3 C0, which must
		// capture "ÿz" and must NOT capture U+0100 (C4 80).
		{name: "byte_successor_edge", prefix: "ÿ", wantSeek: true},
		// The maximum code point still has a byte successor.
		{name: "max_code_point", prefix: "z\U0010ffff", wantSeek: true},
		// Gate vetoes — correct answers, no seek.
		{name: "empty_prefix_all_rows", prefix: "", wantSeek: false},
		{name: "prefix_matching_every_row", prefix: "name", wantSeek: false},
		{name: "prefix_matching_zero_rows", prefix: "zzz", wantSeek: false},
		{name: "prefix_longer_than_any_value", prefix: "name0000zzzzzzzz", wantSeek: false},
		// A prefix whose successor region is empty in the index.
		{name: "prefix_between_values", prefix: "name0001x", wantSeek: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			query := `MATCH (p:` + pfxDiffLabel + `) WHERE p.name STARTS WITH ` +
				cypherStringLit(tc.prefix) + ` RETURN p.name`

			planOn, err := engOn.Explain(query, nil)
			if err != nil {
				t.Fatalf("Explain on: %v", err)
			}
			planOff, err := engOff.Explain(query, nil)
			if err != nil {
				t.Fatalf("Explain off: %v", err)
			}
			seekOn := strings.Contains(planOn, "NodeByIndexRangeScan")
			if seekOn != tc.wantSeek {
				t.Fatalf("enabled arm seek = %v, want %v\nplan:\n%s", seekOn, tc.wantSeek, planOn)
			}
			// Anti-degeneracy: where the rewrite fires, the arms MUST differ.
			if tc.wantSeek && strings.Contains(planOff, "NodeByIndexRangeScan") {
				t.Fatalf("disabled arm also seeks — the differential is degenerate\nplan:\n%s", planOff)
			}

			want := prefixOracle(truth, tc.prefix)
			gotOn := runPrefixRows(t, engOn, query, nil)
			gotOff := runPrefixRows(t, engOff, query, nil)

			// The absolute oracle first: it is the assertion that survives a defect
			// shared by both arms.
			assertSameStrings(t, "enabled vs Go oracle", gotOn, want)
			assertSameStrings(t, "disabled vs Go oracle", gotOff, want)
			assertSameStrings(t, "enabled vs disabled", gotOn, gotOff)
		})
	}
}

// TestPrefixSeekDifferentialNegatives pins the shapes that must NOT be
// rewritten. Each is checked for result identity against the oracle AND for the
// absence of a seek in BOTH arms — a rewrite leaking into any of these would be
// a wrong answer, most acutely for the negated form, which selects the
// complement of the range.
func TestPrefixSeekDifferentialNegatives(t *testing.T) {
	t.Parallel()

	engOn, truth := newPrefixDiffEngine(t, false)
	engOff, _ := newPrefixDiffEngine(t, true)

	const lbl = `MATCH (p:` + pfxDiffLabel + `) WHERE `

	// negatedOracle is the complement of the prefix set among STRING-valued
	// names: `NOT (s STARTS WITH p)` is NULL, not TRUE, when s is null or
	// non-string, so those rows stay out (TCK String8 [7], [9]).
	negatedOracle := func(prefix string) []string {
		out := make([]string, 0, 16)
		for _, val := range truth {
			if !strings.HasPrefix(val, prefix) {
				out = append(out, val)
			}
		}
		sort.Strings(out)
		return out
	}
	containsOracle := func(sub string) []string {
		out := make([]string, 0, 16)
		for _, val := range truth {
			if strings.Contains(val, sub) {
				out = append(out, val)
			}
		}
		sort.Strings(out)
		return out
	}
	suffixOracle := func(sfx string) []string {
		out := make([]string, 0, 16)
		for _, val := range truth {
			if strings.HasSuffix(val, sfx) {
				out = append(out, val)
			}
		}
		sort.Strings(out)
		return out
	}
	unionOracle := func(a, b string) []string {
		out := make([]string, 0, 16)
		for _, val := range truth {
			if strings.HasPrefix(val, a) || strings.HasPrefix(val, b) {
				out = append(out, val)
			}
		}
		sort.Strings(out)
		return out
	}

	cases := []struct {
		name  string
		query string
		want  []string
	}{{
		// The single most dangerous shape: the complement is not a range.
		name:  "negated_prefix",
		query: lbl + `NOT p.name STARTS WITH "name000" RETURN p.name`,
		want:  negatedOracle("name000"),
	}, {
		name:  "negated_prefix_in_conjunction",
		query: lbl + `NOT p.name STARTS WITH "name000" AND p.name < "name0100" RETURN p.name`,
		want: func() []string {
			out := make([]string, 0, 16)
			for _, val := range truth {
				if !strings.HasPrefix(val, "name000") && val < "name0100" {
					out = append(out, val)
				}
			}
			sort.Strings(out)
			return out
		}(),
	}, {
		name:  "disjunction_of_prefixes",
		query: lbl + `p.name STARTS WITH "name000" OR p.name STARTS WITH "name001" RETURN p.name`,
		want:  unionOracle("name000", "name001"),
	}, {
		// STARTS WITH is not symmetric; the mirrored form is a different predicate.
		name:  "mirrored_operands",
		query: lbl + `"name0001" STARTS WITH p.name RETURN p.name`,
		want: func() []string {
			out := make([]string, 0, 16)
			for _, val := range truth {
				// The reversed argument order is the POINT: the mirrored predicate
				// asks whether the LITERAL starts with the property value.
				if strings.HasPrefix("name0001", val) { //nolint:gocritic // deliberate: mirrored operands
					out = append(out, val)
				}
			}
			sort.Strings(out)
			return out
		}(),
	}, {
		name:  "ends_with",
		query: lbl + `p.name ENDS WITH "0009" RETURN p.name`,
		want:  suffixOracle("0009"),
	}, {
		name:  "contains",
		query: lbl + `p.name CONTAINS "ame000" RETURN p.name`,
		want:  containsOracle("ame000"),
	}, {
		// No covering index on "note", so the seek has nothing to descend.
		name:  "unindexed_property",
		query: lbl + `p.note STARTS WITH "name000" RETURN p.note`,
		want: func() []string {
			out := make([]string, 0, 16)
			for key, val := range truth {
				// "note" is only set on the ordinary population, not on pfxExtra.
				if strings.HasPrefix(key, "p0") || strings.HasPrefix(key, "p1") {
					if strings.HasPrefix(val, "name000") {
						out = append(out, val)
					}
				}
			}
			sort.Strings(out)
			return out
		}(),
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for arm, eng := range map[string]*Engine{"enabled": engOn, "disabled": engOff} {
				plan, err := eng.Explain(tc.query, nil)
				if err != nil {
					t.Fatalf("Explain %s: %v", arm, err)
				}
				if strings.Contains(plan, "NodeByIndexRangeScan") {
					t.Fatalf("%s arm must NOT seek for this shape\nplan:\n%s", arm, plan)
				}
				if !strings.Contains(plan, "NodeByLabelScan") {
					t.Fatalf("%s arm: expected the label-scan plan\nplan:\n%s", arm, plan)
				}
				assertSameStrings(t, arm+" vs Go oracle", runPrefixRows(t, eng, tc.query, nil), tc.want)
			}
		})
	}
}

// TestPrefixSeekParameterised proves a parameterised prefix is declined (the
// string extractor admits literals only, as it does for >= / <) and still
// returns the right answer.
func TestPrefixSeekParameterised(t *testing.T) {
	t.Parallel()

	engOn, truth := newPrefixDiffEngine(t, false)
	const query = `MATCH (p:` + pfxDiffLabel + `) WHERE p.name STARTS WITH $pfx RETURN p.name`

	plan, err := engOn.Explain(query, map[string]expr.Value{"pfx": expr.StringValue("name000")})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if strings.Contains(plan, "NodeByIndexRangeScan") {
		t.Fatalf("a parameterised prefix must not seek\nplan:\n%s", plan)
	}
	for _, pfx := range []string{"name000", "", "caf", "zzz"} {
		got := runPrefixRows(t, engOn, query, map[string]expr.Value{"pfx": expr.StringValue(pfx)})
		assertSameStrings(t, "param "+pfx, got, prefixOracle(truth, pfx))
	}
}

// TestPrefixSeekNullAndNonStringOperands pins the openCypher NULL semantics the
// rewrite must not disturb: a NULL prefix matches nothing and its negation
// matches nothing either (TCK String8 [6], [7]).
func TestPrefixSeekNullAndNonStringOperands(t *testing.T) {
	t.Parallel()

	engOn, _ := newPrefixDiffEngine(t, false)
	for _, q := range []string{
		`MATCH (p:` + pfxDiffLabel + `) WHERE p.name STARTS WITH null RETURN p.name`,
		`MATCH (p:` + pfxDiffLabel + `) WHERE NOT p.name STARTS WITH null RETURN p.name`,
	} {
		plan, err := engOn.Explain(q, nil)
		if err != nil {
			t.Fatalf("Explain %q: %v", q, err)
		}
		if strings.Contains(plan, "NodeByIndexRangeScan") {
			t.Fatalf("a NULL prefix must not seek\nplan:\n%s", plan)
		}
		if got := runPrefixRows(t, engOn, q, nil); len(got) != 0 {
			t.Fatalf("%q returned %d rows, want 0: %q", q, len(got), got)
		}
	}
}

// pfxRapidAlphabet is deliberately tiny so random values share long prefixes and
// a random prefix has a realistic chance of selecting a non-trivial subset.
const pfxRapidAlphabet = "abc"

// TestPrefixSeekRapid is the property: for a random value set and a random
// prefix, the rewritten plan returns exactly the values satisfying
// strings.HasPrefix — the same absolute oracle, over inputs nobody chose.
//
// Not parallel: it keeps a counter across rapid iterations to prove the property
// was not vacuous (that the seek did fire for some inputs).
func TestPrefixSeekRapid(t *testing.T) {
	// The population must clear rangeSeekMinLabelPopulation for the seek to be
	// reachable at all, so each iteration seeds this many nodes.
	const pop = 1100

	fired := 0
	rapid.Check(t, func(rt *rapid.T) {
		vals := rapid.SliceOfN(
			rapid.StringOfN(rapid.SampledFrom([]rune(pfxRapidAlphabet)), 0, 6, -1),
			pop, pop,
		).Draw(rt, "values")
		prefix := rapid.StringOfN(rapid.SampledFrom([]rune(pfxRapidAlphabet)), 0, 4, -1).Draw(rt, "prefix")

		g := lpg.New[string, float64](adjlist.Config{Directed: true})
		truth := make(map[string]string, pop)
		for i, v := range vals {
			key := fmt.Sprintf("r%05d", i)
			if err := g.AddNode(key); err != nil {
				rt.Fatalf("AddNode: %v", err)
			}
			if err := g.SetNodeLabel(key, pfxDiffLabel); err != nil {
				rt.Fatalf("SetNodeLabel: %v", err)
			}
			if err := g.SetNodeProperty(key, "name", lpg.StringValue(v)); err != nil {
				rt.Fatalf("SetNodeProperty: %v", err)
			}
			truth[key] = v
		}
		eng := NewEngineWithOptions(g, EngineOptions{MaxResultRows: MaxResultRowsUnlimited})
		if _, err := eng.Run(context.Background(),
			`CREATE INDEX FOR (n:`+pfxDiffLabel+`) ON (n.name) OPTIONS {indexType:'btree'}`, nil); err != nil {
			rt.Fatalf("CREATE INDEX: %v", err)
		}

		query := `MATCH (p:` + pfxDiffLabel + `) WHERE p.name STARTS WITH ` +
			cypherStringLit(prefix) + ` RETURN p.name`

		plan, err := eng.Explain(query, nil)
		if err != nil {
			rt.Fatalf("Explain: %v", err)
		}
		if strings.Contains(plan, "NodeByIndexRangeScan") {
			fired++
		}

		res, err := eng.Run(context.Background(), query, nil)
		if err != nil {
			rt.Fatalf("Run: %v", err)
		}
		got := make([]string, 0, 16)
		for res.Next() {
			sv, ok := res.ValueAt(0).(expr.StringValue)
			if !ok {
				rt.Fatalf("non-string row value")
			}
			got = append(got, string(sv))
		}
		if err := res.Err(); err != nil {
			rt.Fatalf("iter: %v", err)
		}
		if err := res.Close(); err != nil {
			rt.Fatalf("close: %v", err)
		}
		sort.Strings(got)

		want := prefixOracle(truth, prefix)
		if len(got) != len(want) {
			rt.Fatalf("prefix %q: %d rows, want %d", prefix, len(got), len(want))
		}
		for i := range got {
			if got[i] != want[i] {
				rt.Fatalf("prefix %q: row %d = %q, want %q", prefix, i, got[i], want[i])
			}
		}
	})

	if fired == 0 {
		t.Fatal("the seek never fired across the whole property run — the property is vacuous")
	}
	t.Logf("prefix seek fired in %d rapid iterations", fired)
}

// cypherStringLit renders s as a double-quoted Cypher string literal, escaping
// the characters the parser treats specially. Used so the query under test is
// DERIVED from the same value the oracle uses and the two cannot drift.
func cypherStringLit(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
