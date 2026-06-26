package exec

// rowcount_hint.go — optional, best-effort upper-bound row-count hint (#1720).
//
// The materialisation path (cypher.Result.materialize) drains a fully-built
// plan into a single flat backing slice. Without a capacity hint that slice
// grows geometrically, reallocating and copying O(log N) times across a large
// scan. Some plans expose a knowable upper bound on their output cardinality
// after Init — most importantly a full-node scan, whose leaf has already
// collected every NodeID by the time materialise runs. Threading that bound to
// the drain lets it presize the backing slice once.
//
// The hint is strictly a performance aid and never affects correctness:
//
//   - It is an UPPER BOUND, not an exact count. A consumer must still drain the
//     plan and use the real row count; over-estimating only wastes a little
//     capacity, never produces wrong rows.
//   - It is forwarded only through operators whose output cardinality equals
//     their input cardinality (a strict 1:1 pass-through such as Project) or is
//     produced by a leaf scan that has materialised its candidate set in Init.
//     Any operator that can drop rows (Selection), multiply them (UnwindList),
//     collapse them (aggregation), or cap them (Limit/Top) does NOT implement
//     the interface, so the chain returns "unknown" the moment cardinality can
//     change. This keeps the bound sound: when reported, it is a true upper
//     bound on the rows the plan can yield.
//
// rowCountHinter is unexported: it is an internal optimisation contract between
// the operator tree and the materialise drain, not part of the public Operator
// surface.
type rowCountHinter interface {
	// rowCountHint reports an upper bound on the number of rows this operator
	// (and its sub-tree) can yield, valid only after Init has run. ok is false
	// when no sound upper bound is known, in which case n must be ignored.
	rowCountHint() (n int, ok bool)
}

// RowCountHint reports a best-effort upper bound on the number of rows this
// ResultSet will yield, valid after Run (which calls Init). It returns ok=false
// when the plan exposes no sound upper bound. Callers use it purely to presize
// buffers; the value is never an exact count and must not be used to decide how
// many rows exist. See [rowCountHinter].
func (rs *ResultSet) RowCountHint() (n int, ok bool) {
	if rs == nil || rs.plan == nil {
		return 0, false
	}
	h, isHinter := rs.plan.(rowCountHinter)
	if !isHinter {
		return 0, false
	}
	return h.rowCountHint()
}
