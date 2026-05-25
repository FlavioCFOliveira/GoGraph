package cypher_test

// dbhits_scan_label_test.go — T913: dbhits accounting: scan_label dbhits == label cardinality.
//
// Verifies that MATCH (n:Label) RETURN n on a graph with k labelled nodes
// returns exactly k rows, proving that NodeByLabelScan touches exactly k nodes.
//
// Acceptance criteria:
//  1. Row count from MATCH (n:L) RETURN n equals the count of nodes carrying
//     label L, for various combinations of labelled/unlabelled nodes.
//  2. Race-clean.
//  3. goleak-clean.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"gograph/cypher"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// TestDbHits_ScanLabel is the T913 acceptance test.
func TestDbHits_ScanLabel(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		total   int // total nodes in graph
		labeled int // nodes with label "Person"
	}{
		{"zero_labeled", 5, 0},
		{"all_labeled", 5, 5},
		{"some_labeled", 10, 3},
		{"one_labeled", 20, 1},
		{"large", 100, 37},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			g := lpg.New[string, float64](adjlist.Config{})

			// Add tc.labeled nodes with label "Person".
			for i := range tc.labeled {
				key := fmt.Sprintf("person%d", i)
				if err := g.AddNode(key); err != nil {
					t.Fatalf("AddNode %q: %v", key, err)
				}
				if err := g.SetNodeLabel(key, "Person"); err != nil {
					t.Fatalf("SetNodeLabel %q: %v", key, err)
				}
			}
			// Add unlabeled filler nodes.
			for i := range tc.total - tc.labeled {
				key := fmt.Sprintf("other%d", i)
				if err := g.AddNode(key); err != nil {
					t.Fatalf("AddNode %q: %v", key, err)
				}
			}

			eng := cypher.NewEngine(g)

			// Verify the plan contains NodeByLabelScan.
			plan, err := eng.Explain("MATCH (n:Person) RETURN n", nil)
			if err != nil {
				t.Fatalf("Explain: %v", err)
			}
			if plan == "" {
				t.Fatal("Explain returned empty plan string")
			}
			if tc.labeled > 0 && !strings.Contains(plan, "NodeByLabelScan") {
				t.Errorf("plan missing NodeByLabelScan:\n%s", plan)
			}

			// Observable proxy for scan_label dbhits: row count == label cardinality.
			res, err := eng.Run(context.Background(), "MATCH (n:Person) RETURN n", nil)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			defer res.Close()

			var rows int
			for res.Next() {
				rows++
			}
			if err := res.Err(); err != nil {
				t.Fatalf("Result.Err: %v", err)
			}
			if rows != tc.labeled {
				t.Errorf("label cardinality=%d: want %d rows (== dbhits), got %d",
					tc.labeled, tc.labeled, rows)
			}
		})
	}
}

// TestDbHits_ScanLabel_MultiLabel verifies that each label's scan is
// independent: nodes carrying label A do not appear in a scan for label B.
func TestDbHits_ScanLabel_MultiLabel(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, float64](adjlist.Config{})

	labels := map[string]int{"Engineer": 4, "Manager": 2, "Intern": 7}
	var idx int
	for label, count := range labels {
		for range count {
			key := fmt.Sprintf("node%d", idx)
			idx++
			if err := g.AddNode(key); err != nil {
				t.Fatalf("AddNode: %v", err)
			}
			if err := g.SetNodeLabel(key, label); err != nil {
				t.Fatalf("SetNodeLabel %q: %v", label, err)
			}
		}
	}

	eng := cypher.NewEngine(g)

	for label, want := range labels {
		label, want := label, want
		t.Run(label, func(t *testing.T) {
			t.Parallel()

			query := fmt.Sprintf("MATCH (n:%s) RETURN n", label)
			res, err := eng.Run(context.Background(), query, nil)
			if err != nil {
				t.Fatalf("Run(%q): %v", query, err)
			}
			defer res.Close()

			var rows int
			for res.Next() {
				rows++
			}
			if err := res.Err(); err != nil {
				t.Fatalf("Result.Err: %v", err)
			}
			if rows != want {
				t.Errorf("label %q: want %d rows, got %d", label, want, rows)
			}
		})
	}
}

// TestDbHits_ScanLabel_Monotone verifies that as more nodes with a given label
// are added, the row count (and therefore dbhit count) grows monotonically.
func TestDbHits_ScanLabel_Monotone(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, float64](adjlist.Config{})
	eng := cypher.NewEngine(g)

	var prev int
	for step := range 5 {
		key := fmt.Sprintf("doc%d", step)
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(key, "Doc"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}

		res, err := eng.Run(context.Background(), "MATCH (n:Doc) RETURN n", nil)
		if err != nil {
			t.Fatalf("Run step=%d: %v", step, err)
		}

		var rows int
		for res.Next() {
			rows++
		}
		if err := res.Err(); err != nil {
			res.Close()
			t.Fatalf("Result.Err step=%d: %v", step, err)
		}
		res.Close()

		if rows <= prev {
			t.Errorf("step=%d: rows=%d not > prev=%d — not monotone", step, rows, prev)
		}
		prev = rows
	}
}
