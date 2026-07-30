package exec_test

// expand_intersect_cellform_test.go — the node-cell REPRESENTATION parity gate for
// the fused cyclic expand (rmp #2267, the planner audit's F3).
//
// Layer: short.
//
// # The invariant
//
// ExpandIntersect replaces a two-Expand chain, so it must read its input row the
// same way that chain does. Not "correctly" in the abstract — the SAME. If the
// fused operator accepts a cell form the chain rejects, the fusion invents rows; if
// it rejects one the chain accepts, the fusion loses them. Either way the flag
// changes the answer, which is the one thing an opt-in physical optimisation may
// never do.
//
// The failure is silent by construction: an unreadable cell makes the operator skip
// the input row, and a skipped row is indistinguishable from a row that legitimately
// closes no cycle. That is precisely how #2267 shipped — the boxed expr.NodeValue a
// projection produces was read as a malformed column, so
// `MATCH (a) WITH a MATCH (a)-->(b)-->(c)-->(a)` returned nothing.
//
// # Why the REJECTED forms are asserted too
//
// Pinning only the accepted forms would let the two readers drift apart again the
// next time a node representation is added: a new form silently rejected by one
// side and accepted by the other reintroduces exactly this defect. Asserting that
// both readers make the same decision about EVERY form — including *LazyNodeValue,
// which is node-Kind and which neither accepts — turns that future divergence into
// a red test instead of a wrong answer.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// sameNodeCell reports whether two row cells denote the same node under
// openCypher node identity, which compares the underlying id across
// representations rather than comparing the boxes.
//
// It mirrors [expr.NodeValue.Equal]. Both cells being non-node makes it false: two
// unreadable cells are not "the same node", they are not nodes at all.
func sameNodeCell(x, y expr.Value) bool {
	xi, xok := nodeCellID(x)
	yi, yok := nodeCellID(y)
	return xok && yok && xi == yi
}

// nodeCellID resolves a row cell to a node id, accepting the representations the
// engine's row readers accept.
func nodeCellID(v expr.Value) (uint64, bool) {
	switch n := v.(type) {
	case expr.IntegerValue:
		return uint64(int64(n)), true
	case expr.NodeValue:
		return n.ID, true
	}
	return 0, false
}

// cellRowText renders a row with node cells resolved to their ids, so two rows
// carrying the same nodes in DIFFERENT representations render alike. The shared
// rowText renders any non-IntegerValue as "?", which would make boxed seed columns
// compare equal to each other no matter what they held.
func cellRowText(r exec.Row) string {
	s := "["
	for i, v := range r {
		if i > 0 {
			s += " "
		}
		if id, ok := nodeCellID(v); ok {
			s += itoaTest(int64(id))
		} else {
			s += "<non-node>"
		}
	}
	return s + "]"
}

// cellForm is one way a node id can be represented in a row column.
type cellForm struct {
	// make builds the cell for node id.
	make func(id int64) expr.Value
	name string
	// why records what puts this form in a node column in practice.
	why string
	// readable says whether a node-column reader can resolve an id from it. It is
	// asserted against BOTH readers, so it states an invariant of the engine rather
	// than of either operator.
	readable bool
}

var nodeCellForms = []cellForm{{
	name:     "IntegerValue",
	make:     func(id int64) expr.Value { return expr.IntegerValue(id) },
	why:      "the canonical in-pipeline encoding emitted by every scan and Expand",
	readable: true,
}, {
	name:     "NodeValue",
	make:     func(id int64) expr.Value { return expr.NodeValue{ID: uint64(id)} },
	why:      "a projection alias — any WITH between the anchor and the cycle produces this",
	readable: true,
}, {
	name: "LazyNodeValue",
	make: func(id int64) expr.Value { return expr.NewLazyNodeValue(uint64(id), nil) },
	why: "node-Kind, but confined to the pooled non-escaping RowContext used for " +
		"predicate evaluation; it never reaches an operator's Row, and neither reader accepts it",
	readable: false,
}, {
	name:     "StringValue",
	make:     func(int64) expr.Value { return expr.StringValue("not a node") },
	why:      "a genuinely malformed column — the case both readers must skip",
	readable: false,
}, {
	name:     "Null",
	make:     func(int64) expr.Value { return expr.Null },
	why:      "an OPTIONAL MATCH that did not bind the variable",
	readable: false,
}}

// TestExpandIntersect_NodeCellAcceptanceMatchesExpand drives the fused operator and
// the two-Expand chain it replaces over identical seed rows whose node cells are
// written in each representation in turn, and requires that the two agree row for
// row every time.
//
// FAILS ON THE PRE-#2267 CODE at the NodeValue form: the chain emitted the
// triangle's rows and the fused operator emitted none.
func TestExpandIntersect_NodeCellAcceptanceMatchesExpand(t *testing.T) {
	// One directed triangle with a parallel edge on each fused leg, so a readable
	// seed yields several rows and an unreadable one yields none — a gap wide
	// enough that a mis-read cell cannot hide inside it.
	edges := [][2]int{{0, 1}, {1, 2}, {1, 2}, {2, 0}, {2, 0}}
	fwd, rev := orderedPair(4, edges)

	// The seed the upstream Expand(a→b) would have produced, in canonical form,
	// establishes how many rows a READABLE representation must yield.
	baseline := fusedRows(t, fwd, rev, seedsFor(edges), "", "", nil, nil)
	if len(baseline) == 0 {
		t.Fatal("the canonical seeds produced no rows, so every comparison below " +
			"would be vacuously green")
	}

	for _, form := range nodeCellForms {
		t.Run(form.name, func(t *testing.T) {
			seeds := make([]exec.Row, 0, len(edges))
			for _, e := range edges {
				seeds = append(seeds, exec.Row{form.make(int64(e[0])), form.make(int64(e[1]))})
			}

			want := referenceChain(t, fwd, rev, seeds, "", "", nil, nil)
			got := fusedRows(t, fwd, rev, seeds, "", "", nil, nil)

			// The parity assertion. This is the invariant; the readable flag below
			// only says which side of it this form falls on.
			if len(got) != len(want) {
				t.Fatalf("the fused operator and the two-Expand chain DISAGREE on a %s cell "+
					"(%s): fused emitted %d rows, the chain emitted %d. The fusion must read "+
					"its input exactly as the plan it replaces does",
					form.name, form.why, len(got), len(want))
			}
			for i := range want {
				if cellRowText(got[i]) != cellRowText(want[i]) {
					t.Fatalf("row %d differs on a %s cell:\n  fused %s\n  chain %s",
						i, form.name, cellRowText(got[i]), cellRowText(want[i]))
				}
			}

			// Anti-degeneracy: agreement at zero rows proves nothing on its own, so
			// each form must land on the side it claims.
			switch {
			case form.readable && len(got) != len(baseline):
				t.Fatalf("%s is documented as readable (%s) but yielded %d rows against the "+
					"canonical %d, so the id it carries is not being resolved",
					form.name, form.why, len(got), len(baseline))
			case !form.readable && len(got) != 0:
				t.Fatalf("%s is documented as unreadable (%s) but yielded %d rows; a cell no "+
					"reader can resolve must skip its input row, not expand it",
					form.name, form.why, len(got))
			}
		})
	}
}
