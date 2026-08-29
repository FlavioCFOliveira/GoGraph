package exec

// This file answers one question the plan builder asks at the root of every
// read plan: does this operator ALREADY emit exactly the result columns, so that
// laying a final projection on top of it would be a strict identity? (rmp #2239)
//
// It lives beside the operators rather than in the cypher package for two
// reasons. First, the answer depends on each operator's row-shape contract, so
// the whitelist below belongs where a new operator's author will see it. Second,
// a profiled build wraps every operator, and only this package can see through
// the wrapper — [profiledNode.planUnwrap] is unexported, and a walk that could
// not unwrap would answer differently under PROFILE than under EXPLAIN, which is
// exactly the divergence [Profiler] documents as forbidden. [UnwrapProfiled] is
// how that is done here and by the plan builder alike.

// EmitsExactly reports whether op is statically known to emit exactly cols, in
// that order — both the column NAMES and the row ARITY.
//
// The arity half is what makes it safe to act on. A caller holding only a
// variable-to-index schema can tell where a column SITS but not how WIDE the row
// is, so `MATCH (a)-[r]->(b:P) RETURN a, r` looks like an identity — both columns
// are already at their own index — while the row that actually arrives carries a
// third column that a passthrough projection must still narrow away.
// [Project.Columns] reports the real output arity, which closes that gap.
//
// The walk descends through operators that re-emit their input row UNCHANGED in
// width and column order (see rowShapePreserving), so `RETURN n LIMIT 3` —
// Limit over Project — is recognised as readily as a bare `RETURN n`. Any
// operator that neither declares its columns nor preserves the row shape stops
// the descent and yields false, so a shape-changing operator can never be walked
// through by omission.
func EmitsExactly(op Operator, cols []string) bool {
	for {
		op = UnwrapProfiled(op)
		if declarer, ok := op.(columnDeclarer); ok {
			return declarer.columnsAre(cols)
		}
		if !rowShapePreserving(op) {
			return false
		}
		kids, ok := op.(PlanChildren)
		if !ok {
			return false
		}
		children := kids.PlanChildren()
		if len(children) != 1 {
			return false
		}
		op = children[0]
	}
}

// rowShapePreserving reports whether op re-emits its input row with the same
// width and the same column order, which is the precondition [EmitsExactly]
// needs to look past op at what its child declares.
//
// Membership is an explicit whitelist, verified against each operator's Next and
// never inferred:
//
//   - Distinct, Filter, Limit and Skip hand the caller's Row straight to the
//     child and return it untouched; they drop rows, never columns.
//   - Eager, Sort and Top copy each child row verbatim
//     (cp := make(Row, len(row)); copy(cp, row)) before retaining it, and Sort
//     and Top reorder or DROP whole rows, never the columns within one. Top was
//     absent from this set until #2509, which is why `RETURN n ORDER BY n.age
//     LIMIT 3` rendered two Project operators where the same query without the
//     LIMIT rendered one.
//
// Anything absent from this set is treated as shape-changing. Adding an operator
// here requires reading its Next and confirming the same property; the cost of
// omitting one is a missed optimisation, while the cost of admitting a
// shape-changing operator is a wrong result schema.
func rowShapePreserving(op Operator) bool {
	switch op.(type) {
	case *Distinct, *Eager, *Filter, *Limit, *Skip, *Sort, *Top:
		return true
	default:
		return false
	}
}

// columnDeclarer is an operator that knows its own output schema and can compare
// it against a candidate column list.
//
// The comparison is a method rather than a getter so the check allocates
// NOTHING: [Project.Columns] materialises a fresh []string on every call, and
// EmitsExactly runs on every read-plan build, so a getter would add one heap
// allocation per query built — measured as +1 allocs/op on
// bench/cypher_scale.BenchmarkFilterProject before this was changed.
//
// Only [Project] implements it, so [ColumnarProject] — which embeds *Project —
// is covered by promotion.
type columnDeclarer interface {
	columnsAre(cols []string) bool
}

// columnsAre reports whether this projection emits exactly cols, in that order,
// without materialising the slice [Project.Columns] would allocate.
func (op *Project) columnsAre(cols []string) bool {
	if len(op.items) != len(cols) {
		return false
	}
	for i, item := range op.items {
		if item.Alias != cols[i] {
			return false
		}
	}
	return true
}
