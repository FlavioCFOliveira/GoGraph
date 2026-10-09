package exec

// export_constraint_straddle_test_support.go — the seam the parent package's
// straddler enumeration uses to prove that it detects a seeded defect (rmp
// #2936 audit, R5-1). The seam restores that defect inside the real validation
// rather than reimplementing the validation in the test: an oracle that is
// checked against a restatement proves only that two restatements agree.

// SetStraddleAnyMarkForTest makes [ConstraintRegistry.ValidateStraddler] treat
// every reservation mark as an insertion, which is the R5-1 defect: a value a
// write took without inserting it is then skipped by the commit's check and
// released by its commit. Call it before the registry is shared.
func (r *ConstraintRegistry) SetStraddleAnyMarkForTest(on bool) {
	r.mu.Lock()
	r.straddleAnyMarkForTest = on
	r.mu.Unlock()
}
