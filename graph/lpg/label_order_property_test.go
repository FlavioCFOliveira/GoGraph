// Package lpg_test contains property-based tests for the lpg package.
package lpg_test

import (
	"fmt"
	"sort"
	"testing"

	"pgregory.net/rapid"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// labelAlphabet is the fixed set of label names used for the
// label-order property test. Eight labels keep the search space
// tractable while exercising realistic collision and removal patterns.
var labelAlphabet = []string{"L0", "L1", "L2", "L3", "L4", "L5", "L6", "L7"}

// labelOp encodes a single label mutation: set (true) or remove (false).
type labelOp struct {
	set   bool
	label string
}

// TestLPG_LabelOrder verifies that the set of labels reported by
// NodeLabels(0) after applying an arbitrary trace of SetNodeLabel /
// RemoveNodeLabel calls matches an in-process oracle built from the
// same trace.
//
// The oracle is a simple map[string]bool; NodeLabels must return
// exactly the keys whose value is true, in any order.
func TestLPG_LabelOrder(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(0, 32).Draw(rt, "n")

		ops := make([]labelOp, n)
		for i := range ops {
			ops[i] = labelOp{
				set:   rapid.Bool().Draw(rt, fmt.Sprintf("set_%d", i)),
				label: rapid.SampledFrom(labelAlphabet).Draw(rt, fmt.Sprintf("label_%d", i)),
			}
		}

		const nodeKey = 0
		g := lpg.New[int, int64](adjlist.Config{Directed: true})
		if err := g.AddNode(nodeKey); err != nil {
			rt.Fatalf("AddNode: %v", err)
		}

		// Oracle tracks the expected label membership.
		oracle := make(map[string]bool)

		for _, op := range ops {
			if op.set {
				if err := g.SetNodeLabel(nodeKey, op.label); err != nil {
					rt.Fatalf("SetNodeLabel(%q): %v", op.label, err)
				}
				oracle[op.label] = true
			} else {
				g.RemoveNodeLabel(nodeKey, op.label)
				delete(oracle, op.label)
			}
		}

		// Collect oracle expectation: sorted list of active labels.
		expected := make([]string, 0, len(oracle))
		for name, active := range oracle {
			if active {
				expected = append(expected, name)
			}
		}
		sort.Strings(expected)

		// Collect actual: NodeLabels returns names in unspecified order.
		got := g.NodeLabels(nodeKey)
		sort.Strings(got)

		// Length check first — easier to diagnose.
		if len(got) != len(expected) {
			rt.Fatalf("label count mismatch after %d ops: got %v, want %v", n, got, expected)
		}

		for i := range expected {
			if got[i] != expected[i] {
				rt.Fatalf("label[%d] = %q, want %q (got=%v expected=%v)", i, got[i], expected[i], got, expected)
			}
		}
	})
}
