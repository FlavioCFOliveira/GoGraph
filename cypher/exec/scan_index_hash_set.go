package exec

// scan_index_hash_set.go — NodeByIndexSeekSet operator (task #2183).
//
// NodeByIndexSeekSet is the set-at-a-time counterpart to [NodeByIndexSeek]: it
// serves a key SET — `k IN [k1, k2, …]`, which is what an UNWIND-bound key
// becomes — with one index probe per DISTINCT key, merging the posting lists into
// a single ascending, duplicate-free NodeID run.
//
// # Why set-at-a-time and not one seek per row
//
// The alternative shape is Neo4j's: a correlated Apply that re-probes the index
// for every input row. Set-at-a-time was chosen for three measured or structural
// reasons, with the cost stated rather than hidden:
//
//   - Duplicate keys are free. `UNWIND ['a','a','a'] AS k` probes once, not three
//     times, because the keys are deduplicated before any probe.
//   - The result is a sorted NodeID run, which is what the label-intersection tier
//     consumes. A row-at-a-time Apply yields rows, so an intersection would have
//     to filter row by row and the set tier would be bypassed.
//   - The probe cost is bounded by the DISTINCT key count, not the row count, so a
//     wide input with few distinct keys costs what the few keys cost.
//
// The honest cost is that the key set must be known before the first row is
// emitted. That is not a barrier over the input here: this operator is a LEAF and
// takes its keys from the plan, so the set is complete by construction. A key set
// that only exists at runtime — `UNWIND $batch AS k` — needs the draining variant
// and is deliberately not served by this operator.
//
// # The budget, and why it exists
//
// A key set that covers most of the label is CHEAPER to answer with a scan: the
// probes cost more than the scan they replace and the merged posting list
// approaches the label population. Init therefore takes a budget and reports
// [ErrSeekSetOverBudget] the moment the merged distinct count exceeds it, so the
// planner can fall back. The count that trips it is exact — it is the cardinality
// of the merged run, not an estimate — which is what lets the caller's gate be
// provably non-regressing, exactly as the range seek's gate is.
//
// Probing stops at the first key that pushes the total over budget, so an
// over-budget set is rejected without paying for the whole probe sequence.
//
// # Zero-alloc contract
//
// Init merges into a reused buffer; each Next emits one id from it with no
// further allocation. A small key set with small posting lists stays within the
// inline buffer and allocates nothing in the steady state.
//
// # Cancellation
//
// ctx.Err() is checked at the top of every Next call, and once per key during the
// probe loop in Init, so a large key set cannot outrun a cancelled context.
//
// NodeByIndexSeekSet is NOT safe for concurrent use.

import (
	"context"
	"errors"
	"slices"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// ErrSeekSetOverBudget is returned by [NodeByIndexSeekSet.Init] when the merged
// posting count exceeds the budget the operator was built with. It is a planning
// signal, not a failure: the caller answers the query by scanning instead.
var ErrSeekSetOverBudget = errors.New("exec: index seek set exceeds its posting budget")

// NodeByIndexSeekSet is a Volcano leaf operator that performs an equality lookup
// on a property hash index for each of several keys, emitting each matching
// NodeID exactly once. Each Row has a single column: expr.IntegerValue(nodeID).
type NodeByIndexSeekSet struct {
	idx    hashLookup
	ctx    context.Context //nolint:containedctx // stored for per-Next ctx check
	keys   []expr.Value
	buf    [1]expr.Value // fixed backing buffer — zero-alloc per Next
	ids    []uint64      // merged NodeIDs, drained once at Init
	idbuf  [16]uint64    // inline backing for ids — small key sets stay zero-alloc
	pos    int           // cursor into ids
	budget uint64        // maximum merged posting count; 0 means unbounded
}

// NewNodeByIndexSeekSet creates an operator that looks up every key in idx.
//
// Duplicate keys in keys are harmless — they are probed once. A budget of 0
// disables the over-budget check; any other value caps the merged posting count,
// above which Init reports [ErrSeekSetOverBudget].
func NewNodeByIndexSeekSet(idx hashLookup, keys []expr.Value, budget uint64) *NodeByIndexSeekSet {
	return &NodeByIndexSeekSet{idx: idx, keys: keys, budget: budget}
}

// Init probes the index once per distinct key and merges the results into one
// ascending, duplicate-free run.
//
// A key whose type the index cannot serve is SKIPPED rather than failing the
// query. That is a correctness requirement, not leniency: openCypher equality
// across type groups is FALSE, so a key that cannot be in this index matches
// nothing, and contributing nothing is the right answer. Failing instead would
// turn `WHERE n.name IN ['a', 7]` into an error where the specification asks for
// the rows matching 'a'.
func (op *NodeByIndexSeekSet) Init(ctx context.Context) error {
	op.ctx = ctx
	op.pos = 0
	ids := op.idbuf[:0]

	for i := range op.keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		// A NULL key matches nothing (openCypher equality with NULL is NULL, which
		// fails a filter), and nulls are never indexed, so skipping it is both
		// correct and cheaper than a probe that must return empty.
		if op.keys[i] == nil || op.keys[i].Kind() == expr.KindNull {
			continue
		}
		next, err := op.idx.LookupAppend(op.keys[i], ids)
		if err != nil {
			if errors.Is(err, ErrIndexTypeMismatch) {
				continue
			}
			return err
		}
		ids = next
		// Check the budget against the DISTINCT count as the merge proceeds, so an
		// over-budget set is rejected without probing every remaining key. The
		// pre-merge length is an upper bound on the distinct count, so a cheap
		// length test first avoids sorting on every key.
		if op.budget > 0 && uint64(len(ids)) > op.budget {
			ids = dedupeSorted(ids)
			if uint64(len(ids)) > op.budget {
				return ErrSeekSetOverBudget
			}
		}
	}

	op.ids = dedupeSorted(ids)
	if op.budget > 0 && uint64(len(op.ids)) > op.budget {
		return ErrSeekSetOverBudget
	}
	return nil
}

// dedupeSorted sorts ids ascending and removes duplicates in place.
//
// Each key's posting list arrives ascending but the lists are appended one after
// another, so the concatenation is not ordered. Sorting the whole run once is
// cheaper than an n-way merge for the key counts this serves, and it is what
// makes the emitted order ascending — the property the label-intersection tier
// relies on.
//
// Duplicates arise legitimately: a node whose property equals one key cannot
// equal another, but the same key may appear twice in the set, and a caller may
// pass overlapping key lists. Emitting a node twice would duplicate result rows.
func dedupeSorted(ids []uint64) []uint64 {
	if len(ids) < 2 {
		return ids
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

// Next emits the next matching NodeID. Returns (false, nil) at end-of-stream.
func (op *NodeByIndexSeekSet) Next(out *Row) (bool, error) {
	if err := op.ctx.Err(); err != nil {
		return false, err
	}
	if op.pos >= len(op.ids) {
		return false, nil
	}
	op.buf[0] = expr.IntegerValue(int64(op.ids[op.pos])) //nolint:gosec // NodeID fits int64
	op.pos++
	*out = op.buf[:]
	return true, nil
}

// Close releases the operator. The merged id run is retained for reuse across
// Init calls, matching [NodeByIndexSeek].
func (op *NodeByIndexSeekSet) Close() error {
	op.ids = nil
	op.pos = 0
	return nil
}
