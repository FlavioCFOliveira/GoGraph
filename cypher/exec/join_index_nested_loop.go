package exec

// join_index_nested_loop.go — IndexNestedLoopJoin, the Θ(B log N) plan for a
// disconnected equi-join whose key varies per outer row (rmp #2233).
//
// # What it replaces, and when it wins
//
// The same shape [HashJoin] serves:
//
//	UNWIND $rows AS r MATCH (b:B) WHERE b.age = r.a RETURN r.a, b.id
//
// which the IR lowers to Selection(b.age = r.a, Apply(unwind, scan b)). Three
// plans exist for it, with three cost profiles over a batch of B outer rows
// against a population of N indexed nodes:
//
//	nested loop            Θ(B·N)      — the pre-#1506 plan
//	hash join              Θ(N+B)      — #1506 / #2228, builds the whole population
//	index nested-loop join Θ(B·log N)  — this operator, one seek per outer row
//
// Neither of the last two dominates. At B=5000, N=20000 (the three-way bulk-load
// harness) Θ(N+B) ≈ 25 000 beats Θ(B log N) ≈ 71 500; at B=500 the arithmetic
// reverses, 7 150 against 20 500. So this operator ships WITH a cost gate rather
// than as a replacement — see the planner side (tryBuildIndexNestedLoopJoin).
// Small-batch incremental ingest is the regime it serves.
//
// # Order preservation
//
// The emitted sequence is row-for-row identical to the nested loop's, which is
// what lets a writing statement use it (`SET` is last-write-wins) and what means
// it needs no order-safety guard on either path — the same property
// [hashJoinBuildOnLeft] establishes for the hash join, and for the same two
// reasons:
//
//  1. OUTER-MAJOR. One outer row is drained to exhaustion before the next is
//     pulled, so the outer arm's own order is the output's major order — exactly
//     the Apply's.
//  2. ASCENDING NODE IDS WITHIN AN OUTER ROW. The nested loop's inner arm is a
//     label scan, which emits its label bitmap in ascending node-id order. Both
//     of this operator's paths do the same: the btree's LookupAppend appends a
//     posting list in ascending order, and the fallback path IS a label scan.
//
// The output row is outer||inner, the column order the Apply emits, so
// downstream operators address columns identically.
//
// # The hybrid, and why the seek alone would return wrong rows
//
// The join key's TYPE is not known at plan time: `r.a` over an UNWIND of a
// parameter may be numeric on one row and a string on the next. The index that
// makes this plan possible is the numeric btree companion a Cypher CREATE INDEX
// builds alongside the user's index — it is keyed on float64, which is what makes
// cross-type numeric equality (3 = 3.0) exact. It holds no non-numeric value, so
// a seek for a string key would find nothing while the nested loop would happily
// match a string-valued property. Seeking unconditionally would therefore be
// silently WRONG, not merely narrow.
//
// So the operator carries both paths and picks per row:
//
//	NULL or NaN key       → emit nothing. Neither can equal anything under
//	                        openCypher `=`, so the nested loop emits nothing
//	                        either ([isUnjoinableKey], shared with the hash join).
//	numeric key           → seek the companion. Exact, Θ(log N).
//	any other kind        → drive the inner arm and apply the equality filter for
//	                        THAT ROW ONLY. This is precisely the nested loop, so
//	                        the answer is right by construction and the cost is
//	                        what the query would have paid anyway.
//
// An integer key too large for float64 to hold exactly takes the fallback as
// well: past 2^53 the conversion is lossy, so a seek could land on a DIFFERENT
// integer. That is [exactFloat64Int] below, and it is the one case where the
// fallback is protecting correctness rather than coverage.
//
// # Concurrency
//
// IndexNestedLoopJoin is NOT safe for concurrent use.
//
// # Cancellation
//
// ctx.Err() is checked at the top of every Next call and once per outer row.

import (
	"context"
	"math"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// maxExactFloat64Int is the largest magnitude an int64 can carry that float64
// represents exactly (2^53). Beyond it, int64→float64 rounds, so two distinct
// integers can share one float64 key.
const maxExactFloat64Int = int64(1) << 53

// NumericPointLookup is the minimal capability this operator needs from the
// numeric btree companion: an allocation-free ascending point lookup.
//
// It is deliberately narrower than [rangeLookup], whose RangeBitmap allocates a
// roaring bitmap per call. This operator performs one lookup per OUTER ROW, so a
// per-call allocation would be charged B times; LookupAppend reuses the caller's
// buffer instead. btree.Index[float64] satisfies it directly.
type NumericPointLookup interface {
	LookupAppend(value float64, dst []uint64) []uint64
}

// IndexNestedLoopJoin joins an outer arm against an indexed population by
// seeking the index once per outer row. It emits outer||inner rows in the
// nested loop's exact sequence.
//
// IndexNestedLoopJoin is NOT safe for concurrent use.
type IndexNestedLoopJoin struct {
	outer Operator
	// inner is the plain inner arm (a label scan). It serves the FALLBACK path
	// only — a row whose key the numeric index cannot represent — and is re-Init'd
	// per such row, exactly as an Apply drives an uncorrelated inner arm.
	inner Operator

	ctx context.Context //nolint:containedctx // stored for per-Next ctx check

	// outerKeyFn evaluates the join key against an outer row; innerKeyFn
	// evaluates it against an inner-only row. The pair mirrors the hash join's
	// probeFn/buildFn so both operators derive the key identically.
	outerKeyFn KeyFn
	innerKeyFn KeyFn

	// outerRow is the current outer row, owned for the duration of its inner
	// drain. It is a snapshot: the outer operator may reuse its row buffer on the
	// next Next, and the fallback path pulls from a DIFFERENT operator in between.
	outerRow Row
	outerKey expr.Value

	idx NumericPointLookup

	// ids holds the current outer row's seek result, drained once per row.
	ids   []uint64
	idbuf [8]uint64 // inline backing — the dominant small posting list stays zero-alloc
	pos   int

	outBuf []expr.Value

	// fallback is true while the current outer row is being served by the inner
	// arm rather than by a seek.
	fallback bool
	// provenNumericCoverage records that the PLANNER proved every node the inner
	// scan produces carries a numeric value for the join property (see
	// numericIndexCoversScan). Under that proof a key of any non-numeric kind
	// matches nothing, so it needs no scan to discover that.
	//
	// It is a performance guarantee, not a correctness one: with it false the
	// operator still answers correctly, by scanning. What it removes is a Θ(B·N)
	// exposure — a batch of B string keys would otherwise drive B full label scans
	// to return nothing at all, which an adversarial or merely odd batch could turn
	// into the pre-#1506 cost.
	provenNumericCoverage bool
	// innerLive is true once inner has been Init'd at least once, so Close knows
	// to close it.
	innerLive bool
	// outerEOS records that the outer arm is drained.
	outerEOS bool
	// haveOuter records that outerRow holds a row whose inner side is mid-drain.
	haveOuter bool
}

// NewIndexNestedLoopJoin creates an IndexNestedLoopJoin.
//   - outer is the probe arm; its rows drive the join and set the output's major
//     order.
//   - inner is the plain inner arm, used only for the non-numeric-key fallback.
//   - idx is the numeric btree companion covering the inner arm's (label,
//     property).
//   - outerKeyFn / innerKeyFn evaluate the join key against an outer row and an
//     inner-only row respectively.
func NewIndexNestedLoopJoin(outer, inner Operator, idx NumericPointLookup, outerKeyFn, innerKeyFn KeyFn) *IndexNestedLoopJoin {
	return &IndexNestedLoopJoin{
		outer:      outer,
		inner:      inner,
		idx:        idx,
		outerKeyFn: outerKeyFn,
		innerKeyFn: innerKeyFn,
	}
}

// WithProvenNumericCoverage records that every node the inner scan produces is
// known to carry a numeric value for the join property. The operator then answers
// a non-numeric key with no rows instead of scanning for them. It returns op for
// chaining.
//
// Only a caller that has actually PROVED this may set it — the planner does, by
// comparing the numeric index's entry count against the scan's row count. Setting
// it without the proof would drop rows.
func (op *IndexNestedLoopJoin) WithProvenNumericCoverage(proven bool) *IndexNestedLoopJoin {
	op.provenNumericCoverage = proven
	return op
}

// Init initialises the outer arm. The inner arm is Init'd lazily, on the first
// row that needs the fallback: a query whose keys are all numeric never touches
// it, so it never pays the scan's set-up.
func (op *IndexNestedLoopJoin) Init(ctx context.Context) error {
	op.ctx = ctx
	op.outerRow = nil
	op.outerKey = nil
	op.ids = op.idbuf[:0]
	op.pos = 0
	op.fallback = false
	op.outerEOS = false
	op.haveOuter = false
	return op.outer.Init(ctx)
}

// Next emits the next outer||inner row.
func (op *IndexNestedLoopJoin) Next(out *Row) (bool, error) {
	if err := op.ctx.Err(); err != nil {
		return false, err
	}
	for {
		// Drain the current outer row's inner side before pulling another.
		if op.haveOuter {
			ok, err := op.nextInner(out)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
			op.haveOuter = false
		}
		if op.outerEOS {
			return false, nil
		}
		if err := op.ctx.Err(); err != nil {
			return false, err
		}
		if err := op.advanceOuter(); err != nil {
			return false, err
		}
	}
}

// advanceOuter pulls the next outer row and prepares its inner side: a seek for a
// numeric key, the inner arm for any other kind, and nothing at all for a key
// that cannot match.
func (op *IndexNestedLoopJoin) advanceOuter() error {
	var row Row
	ok, err := op.outer.Next(&row)
	if err != nil {
		return err
	}
	if !ok {
		op.outerEOS = true
		return nil
	}
	// Own the outer row for the whole inner drain. The outer operator is free to
	// reuse its buffer on the next pull, and on the fallback path a different
	// operator runs in between.
	op.outerRow = append(op.outerRow[:0], row...)

	key, err := op.outerKeyFn(op.outerRow)
	if err != nil {
		return err
	}
	op.outerKey = key
	op.pos = 0
	op.fallback = false

	// A NULL or NaN key matches nothing under openCypher `=`, so the nested loop
	// emits nothing for this row either. Skip both paths.
	if isUnjoinableKey(key) {
		op.ids = op.ids[:0]
		op.haveOuter = true
		return nil
	}
	f, exact := exactFloat64Key(key)
	if !exact {
		// Not a numeric key the companion can represent.
		//
		// When the planner proved numeric coverage, no scanned node holds a
		// non-numeric value, so a non-numeric key cannot equal any of them and the
		// answer is no rows — reached without touching the graph. An integer past
		// 2^53 is the exception: a node CAN hold it, the conversion is lossy, and
		// only a scan settles it.
		if op.provenNumericCoverage && !isOversizedInteger(key) {
			op.ids = op.ids[:0]
			op.haveOuter = true
			return nil
		}
		// Serve this row from the inner arm — the nested loop, for one row.
		op.fallback = true
		op.haveOuter = true
		if err := op.inner.Init(op.ctx); err != nil {
			return err
		}
		op.innerLive = true
		return nil
	}
	op.ids = op.idx.LookupAppend(f, op.idbuf[:0])
	op.haveOuter = true
	return nil
}

// nextInner emits the next inner match for the current outer row, from whichever
// path that row took.
func (op *IndexNestedLoopJoin) nextInner(out *Row) (bool, error) {
	if op.fallback {
		return op.nextFallback(out)
	}
	if op.pos >= len(op.ids) {
		return false, nil
	}
	id := op.ids[op.pos]
	op.pos++
	op.emit(out, Row{expr.IntegerValue(int64(id))})
	return true, nil
}

// nextFallback advances the inner arm until a row whose key equals the outer
// key, applying exactly the equality the join's key semantics define — the same
// [expr.Value.Equal] test the hash join uses to confirm a bucket candidate, so
// the two operators agree on every key.
func (op *IndexNestedLoopJoin) nextFallback(out *Row) (bool, error) {
	for {
		var innerRow Row
		ok, err := op.inner.Next(&innerRow)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
		innerKey, err := op.innerKeyFn(innerRow)
		if err != nil {
			return false, err
		}
		if isUnjoinableKey(innerKey) {
			continue
		}
		if !expr.IsTruthy(innerKey.Equal(op.outerKey)) {
			continue
		}
		op.emit(out, innerRow)
		return true, nil
	}
}

// emit writes outer||inner into the reused output buffer.
func (op *IndexNestedLoopJoin) emit(out *Row, innerRow Row) {
	need := len(op.outerRow) + len(innerRow)
	if cap(op.outBuf) < need {
		op.outBuf = make([]expr.Value, need)
	}
	buf := op.outBuf[:0]
	buf = append(buf, op.outerRow...)
	buf = append(buf, innerRow...)
	op.outBuf = buf
	*out = buf
}

// Close closes both arms. The inner arm is closed only if it was ever Init'd.
func (op *IndexNestedLoopJoin) Close() error {
	op.outerRow = nil
	op.outBuf = nil
	op.ids = nil
	err := op.outer.Close()
	if op.innerLive {
		if cerr := op.inner.Close(); err == nil {
			err = cerr
		}
	}
	return err
}

// isOversizedInteger reports whether v is an integer whose magnitude exceeds the
// exactly-representable float64 range. Such a key is the one non-seekable case a
// node's property can genuinely equal, so it needs the scan even under proven
// numeric coverage.
func isOversizedInteger(v expr.Value) bool {
	iv, ok := v.(expr.IntegerValue)
	if !ok {
		return false
	}
	i := int64(iv)
	return i > maxExactFloat64Int || i < -maxExactFloat64Int
}

// exactFloat64Key converts a numeric join key to the float64 the numeric btree
// companion is keyed on, and reports whether the conversion is EXACT.
//
// It is false for every non-numeric kind (the companion holds no such value) and
// for an integer of magnitude beyond 2^53, where int64→float64 rounds and a seek
// could land on a different integer. Both cases take the fallback path; the
// second is the one where the fallback is protecting correctness rather than
// merely widening coverage.
func exactFloat64Key(v expr.Value) (float64, bool) {
	switch n := v.(type) {
	case expr.IntegerValue:
		i := int64(n)
		if i > maxExactFloat64Int || i < -maxExactFloat64Int {
			return 0, false
		}
		return float64(i), true
	case expr.FloatValue:
		f := float64(n)
		if math.IsNaN(f) {
			// Already excluded by isUnjoinableKey; guarded here too so this
			// function is safe to call on its own.
			return 0, false
		}
		return f, true
	default:
		return 0, false
	}
}
