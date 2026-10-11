package cypher

import (
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
)

// seekProbeKind names the index access path [indexSeekWouldFire] found for a
// Selection, in the order the Selection build tries them.
type seekProbeKind uint8

const (
	seekProbeNone  seekProbeKind = iota // no verdict recorded
	seekProbeHash                       // equality seek (tryBuildIndexSeekFromSelection)
	seekProbeSet                        // key-set seek (tryBuildIndexSeekSetFromSelection)
	seekProbeRange                      // range seek (buildRangeSeekIfEnabled)
)

// seekProbeVerdict hands the access path [indexSeekWouldFire] built to the
// Selection build of the same statement, so the seek is built once per execution
// instead of twice (rmp #3096).
//
// Before this hand-off the probe built the seek against a copy of the schema,
// discarded it, and the columnar recogniser that asked then declined, so the
// serial Selection build built the identical seek again — 473 B, 12.7
// allocations and ~0.26 µs per execution of an indexed point read (example 35,
// docs/design-plan-pure-separation.md §1).
//
// # Why the handed-off operator is the one the Selection build would build
//
// The probe calls the same three builders with the same arguments the Selection
// build passes: the same *ir.Selection, parameters, read view, index manager,
// label resolver and buildOpts. The schema is the one argument that differs, and
// the verdict is recorded and consumed only when it is EMPTY at both points (both
// probe callers decline a non-empty schema), so the builder saw the same layout
// in both places. The index catalog is read once instead of twice: the plan then
// reflects one reading of it, which is what a statement built a moment earlier
// would have seen.
//
// The verdict also records that the builders tried BEFORE the winning one
// declined, so the Selection build skips them; the probe ignores an error from
// the equality builder, so hashErr keeps the Selection build re-running it to
// surface that error exactly as before.
//
// # Concurrency
//
// Build-time state of the ONE goroutine that builds the statement. It is never
// shared between executions: it lives in the statement's buildOpts, forWorker
// clears it, and forSubquery does not carry it.
type seekProbeVerdict struct {
	sel     *ir.Selection
	op      exec.Operator
	after   map[string]int // the probe's schema copy after the winning build
	kind    seekProbeKind
	hashErr bool
}

// recordSeekProbe stores the probe's winning access path for sel. A verdict
// still pending for another Selection is released first.
func (b *buildOpts) recordSeekProbe(sel *ir.Selection, kind seekProbeKind, op exec.Operator, after map[string]int, hashErr bool) {
	if b == nil {
		closeProbe(op)
		return
	}
	if b.seekProbe.op != nil {
		closeProbe(b.seekProbe.op)
	}
	b.seekProbe = seekProbeVerdict{sel: sel, op: op, after: after, kind: kind, hashErr: hashErr}
}

// takeSeekProbe returns and clears the verdict recorded for sel when it is still
// valid against schema. A verdict for sel that no longer applies is released.
func (b *buildOpts) takeSeekProbe(sel *ir.Selection, schema map[string]int) (seekProbeVerdict, bool) {
	if b == nil || b.seekProbe.sel == nil || b.seekProbe.sel != sel {
		return seekProbeVerdict{}, false
	}
	v := b.seekProbe
	b.seekProbe = seekProbeVerdict{}
	if len(schema) != 0 {
		closeProbe(v.op)
		return seekProbeVerdict{}, false
	}
	return v, true
}

// adopt writes the probe's schema entries into schema and returns the operator,
// transferring ownership to the caller.
func (v *seekProbeVerdict) adopt(schema map[string]int) exec.Operator {
	for k, c := range v.after {
		schema[k] = c
	}
	op := v.op
	v.op = nil
	return op
}

// release closes an operator the Selection build did not use.
func (v *seekProbeVerdict) release() {
	closeProbe(v.op)
	v.op = nil
}

// skipsHash reports whether the probe proved the equality builder declines.
func (v *seekProbeVerdict) skipsHash() bool {
	return (v.kind == seekProbeSet || v.kind == seekProbeRange) && !v.hashErr
}

// skipsSet reports whether the probe proved the key-set builder declines.
func (v *seekProbeVerdict) skipsSet() bool {
	return v.kind == seekProbeRange
}
