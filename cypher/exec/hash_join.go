package exec

// hash_join.go — HashJoin operator for disconnected equi-join patterns (#1506).
//
// HashJoin is the order-insensitive, asymptotically-cheaper replacement for a
// nested-loop Apply followed by an equi-join Filter. It targets the canonical
// lowering of a disconnected multi-pattern MATCH with an equality predicate
// across the two otherwise-independent pattern parts, e.g.
//
//	MATCH (a:A), (b:B) WHERE a.x = b.y RETURN a, b
//
// which the IR builds as Selection(a.x = b.y, Apply(scanA, scanB)). The nested
// loop is O(|A|·|B|); the hash join is O(|A|+|B|).
//
// # Algorithm
//
//  1. Build phase (once, on first Next): fully drain the BUILD plan. For each
//     build row, evaluate the build-side key expression. Rows whose key is NULL
//     or NaN are DISCARDED (they can never satisfy `a.x = b.y` under openCypher
//     three-valued logic — NULL/NaN equal nothing). Surviving rows are bucketed
//     by a canonical hash of their key (see canonicalKeyHash).
//  2. Probe phase: stream the PROBE plan. For each probe row, evaluate the
//     probe-side key; skip NULL/NaN keys; look up the bucket; for every build
//     row in the bucket whose key is `=`-equal to the probe key (verified with
//     the canonical openCypher comparator, not Go ==), emit the combined row.
//
// # Output schema
//
// The emitted row is probeRow || buildRow when buildOnLeft is false, and
// buildRow || probeRow when buildOnLeft is true. The planner picks which input
// is build vs probe but ALWAYS arranges the output column order to match the
// Apply it replaces (outer || inner), so downstream column indices are
// unchanged. See NewHashJoin.
//
// # Result identity vs nested loop
//
//   - NULL / missing-property keys match nothing: identical to the `=` filter
//     (openCypher 9 §3.2, three-valued logic).
//   - NaN keys match nothing: `NaN = NaN` is false (openCypher 9 §3.4); a NaN
//     key is excluded from the build table and never matches on probe.
//   - Cross-type numeric equality: integer 1 `=` float 1.0 is TRUE, so the key
//     is canonicalised (integral floats fold to the integer hash) AND every
//     bucket hit is re-verified with expr.Value.Equal, never Go ==.
//   - Row multiset is identical to the nested loop; only emission ORDER differs,
//     which openCypher leaves unspecified absent ORDER BY. The planner guards
//     against the order-observing cases (bare LIMIT/SKIP, collect, …) before
//     ever substituting this operator.
//
// # Concurrency
//
// HashJoin is NOT safe for concurrent use. Each pipeline segment owns its own
// operator tree.
//
// # Cancellation
//
// ctx.Err() is checked at the top of every Next call and every 4096 iterations
// of the build drain.

import (
	"context"
	"errors"
	"math"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// ErrHashJoinMemoryExceeded is returned by the HashJoin build phase when the
// estimated retained size of the build table exceeds the configured byte budget
// (#1841). The build table is otherwise unbounded, so without a budget a large
// build side could exhaust memory before any drain-level guard fires.
var ErrHashJoinMemoryExceeded = errors.New("exec: hash join memory cap exceeded")

// KeyFn extracts the join-key value from a row. It returns the evaluated key
// (which may be expr.Null) or an error that halts the pipeline.
type KeyFn func(row Row) (expr.Value, error)

// buildBucketRow pairs a materialised build-side row with its already-evaluated
// join key, so the probe phase verifies equality without re-evaluating the key.
type buildBucketRow struct {
	key expr.Value
	row Row
}

// HashJoin is a Volcano pipeline operator that performs an order-insensitive
// equi-join between two independent (uncorrelated) plans. It replaces a
// nested-loop Apply + equi-join Filter when the planner has proven the
// substitution is result-identical and order-safe.
//
// HashJoin is NOT safe for concurrent use.
type HashJoin struct {
	build    Operator
	probe    Operator
	ctx      context.Context //nolint:containedctx // stored for per-Next ctx check
	probeKey expr.Value      // current probe key
	buildFn  KeyFn
	probeFn  KeyFn

	// hash table: canonical key hash → slice of build rows in that bucket.
	table map[uint64][]buildBucketRow

	budget byteBudget // estimated-byte cap on the build table (#1841)

	// probe-iteration state
	probeRow Row              // current probe row; nil when none active
	bucket   []buildBucketRow // current bucket being scanned
	outBuf   []expr.Value

	bucketIdx int // next index into bucket to test

	// buildOnLeft controls the output column order. When true the output is
	// buildRow || probeRow; when false it is probeRow || buildRow. The planner
	// sets this so the output layout matches the Apply (outer || inner) being
	// replaced regardless of which side became the build side.
	buildOnLeft bool
	built       bool
	probeEOS    bool
}

// NewHashJoin creates a HashJoin.
//   - build is the plan whose rows are fully materialised into the hash table.
//   - probe is the plan streamed against the table.
//   - buildFn / probeFn extract the join key from a build / probe row.
//   - buildOnLeft selects the output column order (see HashJoin.buildOnLeft).
//
// HashJoin takes ownership of both plans; callers must not use them afterwards.
func NewHashJoin(build, probe Operator, buildFn, probeFn KeyFn, buildOnLeft bool) *HashJoin {
	return &HashJoin{
		build:       build,
		probe:       probe,
		buildFn:     buildFn,
		probeFn:     probeFn,
		buildOnLeft: buildOnLeft,
	}
}

// WithByteBudget bounds the estimated retained size of the build table by
// maxBytes, returning [ErrHashJoinMemoryExceeded] when exceeded. The build table
// has no count cap, so this is the operator's memory bound (#1841). A
// non-positive maxBytes or nil estimateRow leaves it unbounded (prior
// behaviour). Returns op for chaining and must be called before Init.
func (op *HashJoin) WithByteBudget(maxBytes int64, estimateRow func(Row) int64) *HashJoin {
	op.budget.set(maxBytes, estimateRow)
	return op
}

// Init initialises both child plans and resets join state.
func (op *HashJoin) Init(ctx context.Context) error {
	op.ctx = ctx
	op.table = nil
	op.built = false
	op.probeRow = nil
	op.probeKey = nil
	op.bucket = nil
	op.bucketIdx = 0
	op.probeEOS = false
	op.outBuf = op.outBuf[:0]
	op.budget.reset()
	if err := op.build.Init(ctx); err != nil {
		return err
	}
	return op.probe.Init(ctx)
}

// buildTable drains the build plan and constructs the hash table. NULL and NaN
// keys are discarded (they can never satisfy the equi-join).
func (op *HashJoin) buildTable() error {
	op.table = make(map[uint64][]buildBucketRow)
	var iter int
	for {
		iter++
		if iter%4096 == 0 {
			if err := op.ctx.Err(); err != nil {
				return err
			}
		}
		var r Row
		ok, err := op.build.Next(&r)
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		key, err := op.buildFn(r)
		if err != nil {
			return err
		}
		if isUnjoinableKey(key) {
			continue
		}
		if op.budget.charge(r) {
			return ErrHashJoinMemoryExceeded
		}
		// Own a stable snapshot of the build row across the entire probe phase.
		cp := make(Row, len(r))
		copy(cp, r)
		h := canonicalKeyHash(key)
		op.table[h] = append(op.table[h], buildBucketRow{row: cp, key: key})
	}
	op.built = true
	return nil
}

// Next advances the hash join.
func (op *HashJoin) Next(out *Row) (bool, error) {
	if err := op.ctx.Err(); err != nil {
		return false, err
	}
	if !op.built {
		if err := op.buildTable(); err != nil {
			return false, err
		}
	}

	for {
		if err := op.ctx.Err(); err != nil {
			return false, err
		}

		// Drain the current bucket against the active probe row.
		if op.probeRow != nil {
			for op.bucketIdx < len(op.bucket) {
				cand := op.bucket[op.bucketIdx]
				op.bucketIdx++
				if eq := cand.key.Equal(op.probeKey); expr.IsTruthy(eq) {
					op.emit(out, cand.row)
					return true, nil
				}
			}
			// Bucket exhausted for this probe row.
			op.probeRow = nil
			op.bucket = nil
			op.bucketIdx = 0
		}

		if op.probeEOS {
			return false, nil
		}

		// Pull the next probe row.
		var pr Row
		ok, err := op.probe.Next(&pr)
		if err != nil {
			return false, err
		}
		if !ok {
			op.probeEOS = true
			return false, nil
		}
		key, err := op.probeFn(pr)
		if err != nil {
			return false, err
		}
		if isUnjoinableKey(key) {
			// NULL/NaN probe key matches nothing — skip without a table lookup.
			continue
		}
		// Snapshot the probe row so it stays valid while we scan the bucket.
		cp := make(Row, len(pr))
		copy(cp, pr)
		op.probeRow = cp
		op.probeKey = key
		op.bucket = op.table[canonicalKeyHash(key)]
		op.bucketIdx = 0
	}
}

// emit writes the combined output row honouring buildOnLeft so the column order
// matches the Apply being replaced.
func (op *HashJoin) emit(out *Row, buildRow Row) {
	var left, right Row
	if op.buildOnLeft {
		left, right = buildRow, op.probeRow
	} else {
		left, right = op.probeRow, buildRow
	}
	need := len(left) + len(right)
	if cap(op.outBuf) < need {
		op.outBuf = make([]expr.Value, need)
	}
	op.outBuf = op.outBuf[:need]
	copy(op.outBuf, left)
	copy(op.outBuf[len(left):], right)
	*out = op.outBuf
}

// Close releases the hash table and closes both child plans.
func (op *HashJoin) Close() error {
	buildErr := op.build.Close()
	probeErr := op.probe.Close()
	op.table = nil
	op.bucket = nil
	op.probeRow = nil
	op.outBuf = nil
	if buildErr != nil {
		return buildErr
	}
	return probeErr
}

// isUnjoinableKey reports whether a join key can never satisfy `a.x = b.y` and
// must therefore be excluded from the hash table / produce no probe match.
// This is exactly the set of values for which `key = anything` is never TRUE:
//   - NULL (and missing properties, which evaluate to NULL),
//   - NaN (scalar float NaN; NaN = NaN is false per openCypher 9 §3.4).
//
// Mixed-type non-numeric values are NOT excluded here: they simply land in their
// own buckets and fail the per-bucket Equal check against a different type, so
// they never over-match (string "1" never equals integer 1).
func isUnjoinableKey(v expr.Value) bool {
	if expr.IsNull(v) {
		return true
	}
	if f, ok := v.(expr.FloatValue); ok && math.IsNaN(float64(f)) {
		return true
	}
	return false
}

// canonicalKeyHash returns a hash that is identical for any two values that
// openCypher `=` treats as equal. The decisive case is cross-type numeric
// equality: integer 1 and float 1.0 are equal, but their default Value.Hash()
// implementations diverge (the integer folds its int bits, the float folds
// its IEEE-754 bits).
//
// Delegates to [expr.EquivalentHash] rather than reimplementing the
// cross-type fold independently: an earlier, independent version of this
// function converted an integral float to an int64 and hashed THAT (folding
// int64 bits), the opposite direction from EquivalentHash, which routes an
// integer through the float64 domain — so the two hash domains disagreed for
// an EQUAL cross-type pair whose Value.Hash implementations diverge, e.g.
// integer 1 and float 1.0. That mismatch silently dropped a matching row from
// the join output — a bucket miss is indistinguishable from "no match" to a
// caller, so the failure was never an error, just a wrong (too-small) result
// (rmp #1865). Note that expr.Value.Equal's cross-type numeric comparison is
// EXACT: values ≥ 2^53 that share a float64 image but are unequal (e.g. int
// 2^53+1 vs float 2^53.0) simply share a bucket here and are correctly
// separated by the per-bucket Equal check (line ~224), never over-matched.
//
// EquivalentHash's contract — Equivalent(a,b) ⟹ same hash — is safe to reuse
// here even though HashJoin's own equality semantics are Equal (`=`), not
// Equivalent: EquivalentHash's Equivalent-specific behaviour (NaN≡NaN,
// null≡null) only differs from Equal for a NaN or null operand, and both are
// unconditionally excluded from the build table before canonicalKeyHash is
// ever called (see the package doc's Algorithm section) — so within
// canonicalKeyHash's actual input domain, Equivalent(a,b) and
// IsTruthy(a.Equal(b)) coincide for every type, making EquivalentHash a
// correct (and now single-source-of-truth) hash for HashJoin's bucketing too.
//
// Callers MUST still verify a bucket hit with expr.Value.Equal — the hash only
// guarantees equal values collide, never that colliding values are equal.
func canonicalKeyHash(v expr.Value) uint64 {
	return expr.EquivalentHash(v)
}
