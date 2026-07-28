package cypher

// prefix_seek_plan_test.go — plan-shape and successor-construction gate for the
// STARTS WITH prefix range seek (#2127).
//
// This file proves the acceptance criteria of the implementation task: that a
// prefix predicate on an indexed string property reaches the sorted btree, that
// every excluded shape keeps the label scan, and that the successor construction
// satisfies the invariant the soundness proof rests on. The result-identity
// differential, the rapid property and the wider negative matrix live in
// prefix_seek_differential_test.go (#2128).
//
// The design and its proofs are in docs/design-prefix-range-seek.md.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// prefixSeekPop is above rangeSeekMinLabelPopulation so the gate's population
// floor is not what declines a case under test.
const prefixSeekPop = 2000

// buildPrefixSeekEngine seeds :PfxPerson nodes with a sortable "name" and an
// unindexed "note" carrying the same value, then creates the bound string btree
// on name through the engine's own CREATE INDEX path — the only way to obtain a
// self-maintaining, backfilled btree.
func buildPrefixSeekEngine(t *testing.T, disablePrefix bool) *Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	for i := 0; i < prefixSeekPop; i++ {
		key := fmt.Sprintf("p%04d", i)
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(key, "PfxPerson"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		val := lpg.StringValue(fmt.Sprintf("name%04d", i))
		if err := g.SetNodeProperty(key, "name", val); err != nil {
			t.Fatalf("SetNodeProperty name: %v", err)
		}
		if err := g.SetNodeProperty(key, "note", val); err != nil {
			t.Fatalf("SetNodeProperty note: %v", err)
		}
	}
	eng := NewEngineWithOptions(g, EngineOptions{DisablePrefixIndexSeek: disablePrefix})
	if _, err := eng.Run(context.Background(),
		`CREATE INDEX FOR (n:PfxPerson) ON (n.name) OPTIONS {indexType:'btree'}`, nil); err != nil {
		t.Fatalf("CREATE INDEX: %v", err)
	}
	return eng
}

// TestPrefixSeekPlanShape pins which shapes reach the index and which keep the
// label scan. Every "want a label scan" row is a scope boundary the design
// argues for; a regression here is a correctness regression, not a performance
// one, for the negation and mirror rows in particular.
func TestPrefixSeekPlanShape(t *testing.T) {
	t.Parallel()
	eng := buildPrefixSeekEngine(t, false)

	cases := []struct {
		name     string
		query    string
		wantSeek bool
		wantText string // substring required in the rendered plan when wantSeek
	}{{
		name:     "selective_prefix_seeks",
		query:    `MATCH (p:PfxPerson) WHERE p.name STARTS WITH "name000" RETURN p.name`,
		wantSeek: true,
		wantText: `range="name000".."name001"(excl)`,
	}, {
		// The successor increments the LAST byte below 0xFF: "name00" → "name01".
		name:     "shorter_prefix_still_selective",
		query:    `MATCH (p:PfxPerson) WHERE p.name STARTS WITH "name00" RETURN p.name`,
		wantSeek: true,
		wantText: `range="name00".."name01"(excl)`,
	}, {
		// A prefix conjoined with an upper bound merges to the tighter interval.
		name:     "prefix_and_upper_bound",
		query:    `MATCH (p:PfxPerson) WHERE p.name STARTS WITH "name000" AND p.name < "name0005" RETURN p.name`,
		wantSeek: true,
		wantText: `range="name000".."name0005"(excl)`,
	}, {
		// ── Scope boundaries: none of these may be rewritten. ──
		// NOT selects the COMPLEMENT of the prefix set; a range seek would be a
		// non-superset and would return wrong answers (design §5.1).
		name:  "negated_prefix_never_seeks",
		query: `MATCH (p:PfxPerson) WHERE NOT p.name STARTS WITH "name000" RETURN p.name`,
	}, {
		name:  "disjunction_never_seeks",
		query: `MATCH (p:PfxPerson) WHERE p.name STARTS WITH "name000" OR p.name STARTS WITH "name001" RETURN p.name`,
	}, {
		name:  "negated_prefix_in_conjunction_never_seeks",
		query: `MATCH (p:PfxPerson) WHERE NOT p.name STARTS WITH "name000" AND p.name < "name1" RETURN p.name`,
	}, {
		// STARTS WITH is not symmetric: the mirrored form tests the LITERAL
		// against a property-valued prefix and describes no range over p.name.
		name:  "mirrored_form_never_seeks",
		query: `MATCH (p:PfxPerson) WHERE "name0002" STARTS WITH p.name RETURN p.name`,
	}, {
		// ENDS WITH / CONTAINS admit no useful range (design §5.3).
		name:  "ends_with_never_seeks",
		query: `MATCH (p:PfxPerson) WHERE p.name ENDS WITH "0002" RETURN p.name`,
	}, {
		name:  "contains_never_seeks",
		query: `MATCH (p:PfxPerson) WHERE p.name CONTAINS "0002" RETURN p.name`,
	}, {
		// Parameters are out of scope for the string extractor, as for >= / <.
		name:  "parameter_prefix_never_seeks",
		query: `MATCH (p:PfxPerson) WHERE p.name STARTS WITH $pfx RETURN p.name`,
	}, {
		// No covering index on "note".
		name:  "unindexed_property_never_seeks",
		query: `MATCH (p:PfxPerson) WHERE p.note STARTS WITH "name000" RETURN p.name`,
	}, {
		// The empty prefix is true of every string, so it has no finite
		// successor and spans the whole index; the selectivity gate declines it.
		name:  "empty_prefix_vetoed_by_gate",
		query: `MATCH (p:PfxPerson) WHERE p.name STARTS WITH "" RETURN p.name`,
	}, {
		// A prefix every row satisfies is over the 10% ceiling.
		name:  "non_selective_prefix_vetoed_by_gate",
		query: `MATCH (p:PfxPerson) WHERE p.name STARTS WITH "name" RETURN p.name`,
	}, {
		// An empty range is correct but pointless to seek.
		name:  "zero_match_prefix_vetoed_by_gate",
		query: `MATCH (p:PfxPerson) WHERE p.name STARTS WITH "zzz" RETURN p.name`,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := eng.Explain(tc.query, nil)
			if err != nil {
				t.Fatalf("Explain: %v", err)
			}
			gotSeek := strings.Contains(plan, "NodeByIndexRangeScan")
			if gotSeek != tc.wantSeek {
				t.Fatalf("NodeByIndexRangeScan present = %v, want %v\nplan:\n%s", gotSeek, tc.wantSeek, plan)
			}
			if tc.wantSeek && !strings.Contains(plan, tc.wantText) {
				t.Fatalf("plan missing %q\nplan:\n%s", tc.wantText, plan)
			}
			if !tc.wantSeek && !strings.Contains(plan, "NodeByLabelScan") {
				t.Fatalf("expected the label-scan plan\nplan:\n%s", plan)
			}
		})
	}
}

// TestPrefixSeekDisabledKeepsLabelScan proves the flag is a real escape hatch:
// the same query that seeks with the rewrite enabled plans a label scan with it
// disabled, while the >= / < seek stays active in both. That is what lets the
// #2128 differential vary exactly one thing.
func TestPrefixSeekDisabledKeepsLabelScan(t *testing.T) {
	t.Parallel()
	const prefixQuery = `MATCH (p:PfxPerson) WHERE p.name STARTS WITH "name000" RETURN p.name`
	const rangeQuery = `MATCH (p:PfxPerson) WHERE p.name >= "name000" AND p.name < "name001" RETURN p.name`

	on, off := buildPrefixSeekEngine(t, false), buildPrefixSeekEngine(t, true)

	planOn, err := on.Explain(prefixQuery, nil)
	if err != nil {
		t.Fatalf("Explain on: %v", err)
	}
	if !strings.Contains(planOn, "NodeByIndexRangeScan") {
		t.Fatalf("enabled engine must seek:\n%s", planOn)
	}
	planOff, err := off.Explain(prefixQuery, nil)
	if err != nil {
		t.Fatalf("Explain off: %v", err)
	}
	if strings.Contains(planOff, "NodeByIndexRangeScan") {
		t.Fatalf("disabled engine must not seek:\n%s", planOff)
	}

	// The companion range predicate must still seek on the DISABLED engine —
	// otherwise the flag would be disabling more than the prefix rewrite.
	rangeOff, err := off.Explain(rangeQuery, nil)
	if err != nil {
		t.Fatalf("Explain range off: %v", err)
	}
	if !strings.Contains(rangeOff, "NodeByIndexRangeScan") {
		t.Fatalf("DisablePrefixIndexSeek must not disable the >= / < seek:\n%s", rangeOff)
	}
}

// TestPrefixSuccessor pins the construction and, more importantly, the
// invariant the soundness proof rests on: every string having p as a prefix
// sorts strictly below succ(p). A miss here is a wrong answer the residual
// Filter cannot rescue, so the invariant is checked directly and not merely
// implied by the table.
func TestPrefixSuccessor(t *testing.T) {
	t.Parallel()

	t.Run("table", func(t *testing.T) {
		cases := []struct {
			in     string
			want   string
			wantOK bool
		}{
			{in: "ab", want: "ac", wantOK: true},
			{in: "a", want: "b", wantOK: true},
			{in: "name002", want: "name003", wantOK: true},
			// Digit rollover is a BYTE increment, not decimal arithmetic: '9'+1 = ':'.
			{in: "name009", want: "name00:", wantOK: true},
			// U+00FF is C3 BF; the last byte below 0xFF is BF, so succ is C3 C0.
			{in: "ÿ", want: "\xc3\xc0", wantOK: true},
			// The maximum code point U+10FFFF is F4 8F BF BF — a successor DOES
			// exist (F4 8F BF C0), which a code-point increment could not give.
			{in: "\U0010ffff", want: "\xf4\x8f\xbf\xc0", wantOK: true},
			// A trailing 0xFF is skipped and an earlier byte carries the increment.
			{in: "a\xff", want: "b", wantOK: true},
			{in: "a\xff\xff", want: "b", wantOK: true},
			// No finite successor: the empty prefix and an all-0xFF prefix.
			{in: "", wantOK: false},
			{in: "\xff", wantOK: false},
			{in: "\xff\xff", wantOK: false},
		}
		for _, tc := range cases {
			got, ok := prefixSuccessor(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("prefixSuccessor(%q) ok = %v, want %v", tc.in, ok, tc.wantOK)
			}
			if ok && got != tc.want {
				t.Fatalf("prefixSuccessor(%q) = %q, want %q", tc.in, got, tc.want)
			}
		}
	})

	t.Run("invariant", func(t *testing.T) {
		prefixes := []string{
			"a", "ab", "name002", "name009", "é", "é", "ÿ",
			"\U0010ffff", "a\xff", " ", "\n", "z",
		}
		// Suffixes appended to a prefix to synthesise members of its prefix set,
		// including the boundary bytes and multi-byte sequences.
		suffixes := []string{"", "a", "z", "\x00", "\xff", "é", "\U0010ffff", "aaaa", "\xff\xff"}
		for _, p := range prefixes {
			succ, ok := prefixSuccessor(p)
			if !ok {
				continue // unbounded above; nothing to compare against
			}
			if p >= succ {
				t.Fatalf("prefixSuccessor(%q) = %q is not strictly greater", p, succ)
			}
			for _, s := range suffixes {
				member := p + s
				if !strings.HasPrefix(member, p) {
					t.Fatalf("internal: %q is not prefixed by %q", member, p)
				}
				if member >= succ {
					t.Fatalf("member %q of prefix %q does not sort below succ %q", member, p, succ)
				}
				if member < p {
					t.Fatalf("member %q of prefix %q sorts below the prefix itself", member, p)
				}
			}
		}
	})
}
