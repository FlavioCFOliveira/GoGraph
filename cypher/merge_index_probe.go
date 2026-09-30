package cypher

// merge_index_probe.go — the property-index access path of the node MERGE match
// phase (rmp #2812).
//
// Without it, `MERGE (n:L {k: v})` enumerates every node carrying :L and
// re-checks each one, so with an index on (:L, k) the match phase still costs
// O(label population) per call. The probe answers the same question from the
// index: the ids the index files under v, each then re-checked against every
// label and every property exactly as the walk re-checks its candidates. The
// probe is a candidate FILTER, never the answer.
//
// # When the index is not the answer, and the probe declines
//
// A property index is written only when a transaction commits
// ([exec.IndexBuffer.CommitInState] → [index.Manager.ApplyBatchInState], run
// inside the publication; see index_commit_apply.go) and is read at the present. The MERGE transaction reads the graph at its own snapshot plus its own
// eager writes. The two agree only when two conditions hold, and the probe
// answers only when it has proved both; otherwise it declines and MERGE walks the
// label posting list, which reads the transaction's own view and is right by
// construction. Declining turns a missed node — which for MERGE is a DUPLICATE
// node — into a slower correct answer.
//
//  1. The transaction has not written the coordinate itself. Its own writes sit
//     in the change buffer until commit, so the index does not describe them:
//     the node an earlier row of `UNWIND $rows AS r MERGE (:L {k: r})` created
//     is not in it yet. This is the rmp #2814 predicate, taken over the SAME
//     keys a bound index's Apply uses to decide whether a change concerns it (a
//     property change on k, whatever the label; a label change on L, whatever
//     the property). It is evaluated at run time, per call, over the buffer as it
//     stands, because the rows of the statement itself write into it as they
//     flow — a plan-time decision cannot see them, which is why
//     [pendingIndexDelta.addPlanWrites] has to refuse every seek of a statement
//     that contains a MERGE. The scan is incremental, so a statement pays for
//     each buffered change once, not once per row.
//
//  2. No commit the transaction cannot see has reached the index. A concurrent
//     writer delivers its changes BEFORE it publishes its commit instant, so an
//     index can already describe a commit this snapshot does not include — a
//     node moved away from v would then be missing from the index yet still
//     match in this snapshot. [index.Manager.DescribesSnapshot], asked AFTER
//     the lookup with the transaction's start instant, is the proof that no
//     such commit is present; the derivation is on that method.
//
// # What the index can and cannot hold
//
// A Cypher CREATE INDEX builds a string-keyed index (hash, or the user btree) and,
// for a btree, the numeric companion keyed on float64 ([numericBTreeName]). A
// string value is probed in the first, an integer or float in the second; any
// other kind — boolean, list, temporal, NaN — is held by neither, so the probe
// declines. The float64 companion is what keeps cross-type numeric equality
// exact: if `a = b` holds under openCypher for integers and floats a and b, then
// float64(a) and float64(b) are the same key, so every equal node is a candidate;
// the lossy conversion above 2^53 can only ADD candidates, which the re-check
// removes. The argument is the one [exec.IndexNestedLoopJoin] states for the
// same companion.
//
// Only a BOUND index is used: an unbound index is not maintained by the change
// fan-out, so its contents prove nothing.
//
// # Concurrency
//
// A mergeIndexProbe belongs to one MERGE operator and is driven by the goroutine
// that owns the operator tree; it is NOT safe for concurrent use. The indexes it
// reads are safe for concurrent use.

import (
	"math"
	"strings"
	"sync/atomic"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// mergeIndexProbeCount counts the MERGE match phases answered from a property
// index, and mergeIndexProbeDeclineCount the probes that declined and left the
// search on the label walk. They are diagnostic seams read only by in-package
// tests, which snapshot them before and after a statement. Process-global and
// monotonic.
var (
	mergeIndexProbeCount        atomic.Uint64
	mergeIndexProbeDeclineCount atomic.Uint64
)

// mergeStringPointLookup is the allocation-light point lookup of a string-keyed
// bound index: hash.Index[string] and btree.Index[string] both satisfy it.
type mergeStringPointLookup interface {
	LookupAppend(value string, dst []uint64) []uint64
	BoundNode() (label, property string, ok bool)
}

// mergeNumericPointLookup is the same for the float64 numeric companion.
type mergeNumericPointLookup interface {
	LookupAppend(value float64, dst []uint64) []uint64
	BoundNode() (label, property string, ok bool)
}

// mergeIndexProbe implements [exec.MergeIndexProbe] for one (label, key) pair.
type mergeIndexProbe struct {
	mgr *index.Manager
	buf *exec.IndexBuffer
	// wtx points at the adapter's transaction handle, read at call time: the
	// handle is set when the bracket opens and names the transaction the MERGE
	// is running as.
	wtx *lpg.WriteTx
	str mergeStringPointLookup  // nil when no string index covers the pair
	num mergeNumericPointLookup // nil when no numeric index covers the pair
	// labelID and propID are the interned coordinates, compared against the
	// buffered changes exactly as a bound index's Apply compares them.
	labelID uint32
	propID  uint32
	// scanned is how many buffered changes have been examined; dirty records that
	// one of them concerned (labelID, propID). The buffer only grows while a
	// statement runs, so dirty never reverts.
	scanned int
	dirty   bool
}

// newMergeIndexProbe returns the probe for (label, key) on g, or nil when there
// is nothing to probe: no change buffer (a read-only adapter), no index, a
// coordinate never interned (so no index can be bound to it), or no bound index
// covering the pair.
func newMergeIndexProbe(g *lpg.Graph[string, float64], buf *exec.IndexBuffer, wtx *lpg.WriteTx, label, key string) exec.MergeIndexProbe {
	if g == nil || buf == nil || wtx == nil || label == "" || key == "" {
		return nil
	}
	mgr := g.IndexManager()
	if mgr == nil || mgr.Count() == 0 {
		return nil
	}
	lid, ok := g.Registry().Lookup(label)
	if !ok {
		return nil
	}
	pid, ok := g.PropertyKeys().Lookup(key)
	if !ok {
		return nil
	}
	str := findMergeStringIndex(mgr, label, key)
	num := findMergeNumericIndex(mgr, label, key)
	if str == nil && num == nil {
		return nil
	}
	return &mergeIndexProbe{
		mgr: mgr, buf: buf, wtx: wtx, str: str, num: num,
		labelID: uint32(lid), propID: uint32(pid),
	}
}

// findMergeStringIndex returns a bound string-keyed index covering exactly
// (label, key): the auto-named hash index first, then the auto-named btree, then
// any covering one.
func findMergeStringIndex(mgr *index.Manager, label, key string) mergeStringPointLookup {
	base := strings.ToLower(label) + "_" + strings.ToLower(key)
	for _, name := range [...]string{base + "_hash", base + "_btree"} {
		if sub, err := mgr.GetIndex(name); err == nil {
			if l, ok := asMergeStringLookup(sub, label, key); ok {
				return l
			}
		}
	}
	for _, name := range mgr.ListIndexes() {
		sub, err := mgr.GetIndex(name)
		if err != nil {
			continue
		}
		if l, ok := asMergeStringLookup(sub, label, key); ok {
			return l
		}
	}
	return nil
}

func asMergeStringLookup(sub index.Subscriber, label, key string) (mergeStringPointLookup, bool) {
	l, ok := sub.(mergeStringPointLookup)
	if !ok {
		return nil, false
	}
	bl, bp, bound := l.BoundNode()
	return l, bound && bl == label && bp == key
}

// findMergeNumericIndex returns a bound float64-keyed index covering exactly
// (label, key): the numeric companion by its deterministic name first, then any
// covering one.
func findMergeNumericIndex(mgr *index.Manager, label, key string) mergeNumericPointLookup {
	if sub, err := mgr.GetIndex(numericBTreeName(label, key)); err == nil {
		if l, ok := asMergeNumericLookup(sub, label, key); ok {
			return l
		}
	}
	for _, name := range mgr.ListIndexes() {
		sub, err := mgr.GetIndex(name)
		if err != nil {
			continue
		}
		if l, ok := asMergeNumericLookup(sub, label, key); ok {
			return l
		}
	}
	return nil
}

func asMergeNumericLookup(sub index.Subscriber, label, key string) (mergeNumericPointLookup, bool) {
	l, ok := sub.(mergeNumericPointLookup)
	if !ok {
		return nil, false
	}
	bl, bp, bound := l.BoundNode()
	return l, bound && bl == label && bp == key
}

// Candidates implements [exec.MergeIndexProbe]. See the file comment for the two
// conditions it proves before answering.
func (p *mergeIndexProbe) Candidates(v lpg.PropertyValue, dst []uint64) ([]uint64, bool) {
	// Condition 1 first: it costs no index read, and a statement that has written
	// the coordinate stays dirty for the rest of its life.
	if p.ownWritesTouch() {
		mergeIndexProbeDeclineCount.Add(1)
		return dst, false
	}
	var out []uint64
	switch v.Kind() {
	case lpg.PropString:
		s, _ := v.String()
		if p.str == nil {
			mergeIndexProbeDeclineCount.Add(1)
			return dst, false
		}
		// A temporal value travels as an SOH-tagged string that no string index
		// holds ([projectStringPropValue]), so the index cannot answer for it.
		if _, isTemporal := decodeTemporalString(s); isTemporal {
			mergeIndexProbeDeclineCount.Add(1)
			return dst, false
		}
		out = p.str.LookupAppend(s, dst)
	case lpg.PropInt64, lpg.PropFloat64:
		if p.num == nil {
			mergeIndexProbeDeclineCount.Add(1)
			return dst, false
		}
		// The same projection the companion files its entries under; NaN is never
		// filed, and equals nothing under `=`, so the walk is left to say so.
		f, ok := mergeNumericKey(v)
		if !ok {
			mergeIndexProbeDeclineCount.Add(1)
			return dst, false
		}
		out = p.num.LookupAppend(f, dst)
	default:
		mergeIndexProbeDeclineCount.Add(1)
		return dst, false
	}
	// Condition 2, asked AFTER the lookup: see [index.Manager.DescribesSnapshot] for
	// why that order is what makes the comparison a proof.
	if !p.wtx.Valid() || !p.mgr.DescribesSnapshot(p.wtx.StartTS()) {
		mergeIndexProbeDeclineCount.Add(1)
		return out[:len(dst)], false
	}
	mergeIndexProbeCount.Add(1)
	return out, true
}

// ownWritesTouch reports whether this transaction has buffered a change that a
// bound index on (labelID, propID) would consume — the rmp #2814 predicate at
// run time. It examines only the changes enqueued since the previous call.
func (p *mergeIndexProbe) ownWritesTouch() bool {
	if p.dirty {
		return true
	}
	pend := p.buf.Pending()
	if len(pend) < p.scanned {
		// The buffer was drained while this probe was live, which no statement
		// does. Refuse rather than reason about what the drain left behind.
		p.dirty = true
		return true
	}
	for _, c := range pend[p.scanned:] {
		switch c.Op {
		case index.OpSetNodeProperty, index.OpDelNodeProperty:
			if c.Property == p.propID {
				p.dirty = true
			}
		case index.OpAddNodeLabel, index.OpRemoveNodeLabel:
			if c.Label == p.labelID {
				p.dirty = true
			}
		default:
			// Edge changes: a node-bound index ignores them.
		}
		if p.dirty {
			return true
		}
	}
	p.scanned = len(pend)
	return false
}

// mergeNumericKey is [projectNumericPropValue] over a value rather than a change
// payload, without boxing it.
func mergeNumericKey(v lpg.PropertyValue) (float64, bool) {
	switch v.Kind() {
	case lpg.PropInt64:
		i, ok := v.Int64()
		return float64(i), ok
	case lpg.PropFloat64:
		f, ok := v.Float64()
		if !ok || math.IsNaN(f) {
			return 0, false
		}
		return f, true
	default:
		return 0, false
	}
}

// mergeIndexProberFor returns the MERGE index access path for mutator, or nil —
// the label walk — when the equality index seek is disabled
// ([EngineOptions.DisableIndexSeek], which therefore also turns this path off),
// when bopts is absent (the public [BuildPlanWithMutator] entry, which enables
// no substitution), or when the mutator is not an engine write adapter.
//
// A graph with no index registered gets nil as well, so an index-free workload
// builds no probe state at all. Registration is DDL, which excludes the
// statement being built, so the answer cannot change before the statement ends.
// It CAN change between two statements of an explicit transaction, which holds
// no schema gate between them; registering an index therefore raises the watermark
// [index.Manager.DescribesSnapshot] checks, so a transaction that started before
// the registration declines the probe and walks.
func mergeIndexProberFor(m exec.GraphMutator, bopts *buildOpts) exec.MergeIndexProber {
	if bopts == nil || !bopts.indexSeekEnabled {
		return nil
	}
	var g *lpg.Graph[string, float64]
	var prober exec.MergeIndexProber
	switch a := m.(type) {
	case *lpgMutatorAdapter:
		g, prober = a.g, a
	case *walMutatorAdapter:
		g, prober = a.g, a
	default:
		return nil
	}
	if g == nil || g.IndexManager().Count() == 0 {
		return nil
	}
	return prober
}

// MergeIndexProbe implements [exec.MergeIndexProber] for the in-memory engine's
// write adapter.
func (a *lpgMutatorAdapter) MergeIndexProbe(label, key string) exec.MergeIndexProbe {
	return newMergeIndexProbe(a.g, a.buf, &a.wtx, label, key)
}

// MergeIndexProbe implements [exec.MergeIndexProber] for the WAL-backed engine's
// write adapter.
func (a *walMutatorAdapter) MergeIndexProbe(label, key string) exec.MergeIndexProbe {
	return newMergeIndexProbe(a.g, a.buf, &a.wtx, label, key)
}
