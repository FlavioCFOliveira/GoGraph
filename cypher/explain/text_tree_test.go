package explain

import (
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
)

// ─────────────────────────────────────────────────────────────────────────────
// TestTextTree — 5 sample plans compared to fixed expected strings
// ─────────────────────────────────────────────────────────────────────────────

func TestTextTree(t *testing.T) {
	tests := []struct {
		name     string
		plan     ir.LogicalPlan
		wantSubs []string // substrings that must appear in the output
	}{
		{
			name: "leaf only — AllNodesScan",
			plan: ir.NewAllNodesScan("n"),
			wantSubs: []string{
				"AllNodesScan",
				"Operator",
				"Est.Rows",
				"Vars",
				"n",
			},
		},
		{
			name: "single unary — ProduceResults over NodeByLabelScan",
			plan: ir.NewProduceResults(
				[]string{"n"},
				ir.NewNodeByLabelScan("n", "Person"),
			),
			wantSubs: []string{
				"ProduceResults",
				"NodeByLabelScan",
				"└─",
				"n",
			},
		},
		{
			name: "two levels — ProduceResults > Selection > NodeByLabelScan",
			plan: ir.NewProduceResults(
				[]string{"n"},
				ir.NewSelection(
					"n.age > 18",
					ir.NewNodeByLabelScan("n", "Person"),
				),
			),
			wantSubs: []string{
				"ProduceResults",
				"Selection",
				"NodeByLabelScan",
				"└─",
			},
		},
		{
			name: "binary — Union",
			plan: ir.NewUnion(
				ir.NewNodeByLabelScan("n", "Person"),
				ir.NewNodeByLabelScan("m", "Animal"),
			),
			wantSubs: []string{
				"Union",
				"NodeByLabelScan",
				"├─",
				"└─",
			},
		},
		{
			name: "index seek leaf",
			plan: ir.NewNodeByIndexSeek("n", "Person", "name", "'Alice'"),
			wantSubs: []string{
				"NodeByIndexSeek",
				"n",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := TextTree(tc.plan)
			if got == "" {
				t.Fatal("TextTree returned empty string")
			}
			for _, sub := range tc.wantSubs {
				if !strings.Contains(got, sub) {
					t.Errorf("output missing %q\nfull output:\n%s", sub, got)
				}
			}
			// Verify table structure: at least 4 lines (sep + header + sep + at
			// least one row + sep).
			lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
			if len(lines) < 4 {
				t.Errorf("expected at least 4 lines, got %d\n%s", len(lines), got)
			}
			// Every line must start with '+' (separator) or '|' (data row).
			for i, l := range lines {
				if l == "" {
					continue
				}
				if l[0] != '+' && l[0] != '|' {
					t.Errorf("line %d has unexpected prefix %q: %s", i, string(l[0]), l)
				}
			}
		})
	}
}

// TestTextTree_Deterministic verifies that repeated calls on the same plan
// produce identical output.
func TestTextTree_Deterministic(t *testing.T) {
	plan := ir.NewProduceResults(
		[]string{"n"},
		ir.NewNodeByLabelScan("n", "Person"),
	)
	first := TextTree(plan)
	for range 10 {
		if got := TextTree(plan); got != first {
			t.Fatal("TextTree produced different output on repeated call")
		}
	}
}
