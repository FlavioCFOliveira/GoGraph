package recovery

// index_payloads.go — what recovery reports about the durable secondary-index
// payloads a snapshot carried, and nothing more (rmp #2490).
//
// # Why recovery does not load an index itself
//
// It cannot. Loading an index means registering it on an index.Manager and
// then keeping it correct while the WAL suffix replays on top of the image — and
// WAL replay CANNOT maintain a registered index. The only production
// index.Manager.ApplyBatch call site in the module is the engine's commit-time
// write-back (cypher/exec/index_writeback.go), store/txn does not import
// graph/index at all, and the replay loop in this package calls
// lpg.Graph.AddNode / SetNodeProperty directly and constructs no index.Change.
// An index registered before replay would therefore be frozen at the snapshot
// instant while the planner, which seeks any index whose BoundNode() reports ok,
// happily served it: silent wrong answers, which is strictly worse than a
// rebuild.
//
// Worse, recovery could not even construct the right index. An index.Manager
// has no "build an index from this definition" entry point, and the binding that
// makes an index self-maintaining (the interned property/label ids, the value
// projection, the liveness and label gates) is owned by the cypher engine.
//
// So recovery reports FACTS — one payload per indexes/<name>.bin the manifest
// declared, plus the reason any of them must not be used, plus the node facets
// the replayed WAL suffix touched — and the engine applies the semantics when it
// re-registers its indexes. That split is what lets the engine decide PER INDEX,
// by name, whether to hydrate the payload or rebuild from the recovered graph.

import (
	"errors"
	"fmt"
	"slices"

	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
)

// ErrIndexPayloadUnreadable reports that a snapshot declared an index payload in
// its manifest but the payload could not be read back intact: the
// indexes/<name>.bin file was missing, or its bytes did not match the CRC32C the
// manifest recorded for them (both detected by [snapshot.LoadIndexes], which
// meters them under `store.snapshot.indexes.corrupted`).
//
// It is a PER-PAYLOAD reason code, never the function error of [Open] /
// [OpenCtx] — exactly like [Result.TailErr]. An index is DERIVED data: a pure
// function of an already-recovered, independently integrity-checked graph, so
// rebuilding it restores byte-identical content and loses nothing. Fail-stopping
// a recovery over it would refuse service for a condition with a complete,
// lossless local repair, which is why every reference engine rebuilds instead:
// PostgreSQL discards and rebuilds pg_internal.init, Memgraph rebuilds its label
// indexes unconditionally, and Neo4j coerces an unreadable index header to
// POPULATING and repopulates.
//
// The one index-related condition that IS fail-stop stays fail-stop: a manifest
// index name that would escape the indexes/ directory raises
// [snapshot.ErrManifestCorrupted] from [snapshot.LoadIndexes] and fails the open.
// That is a security event (path traversal from attacker-controlled manifest
// bytes), not benign corruption, and it is deliberately not reachable through
// this sentinel.
var ErrIndexPayloadUnreadable = errors.New("recovery: snapshot index payload unreadable")

// ErrIndexPayloadStale reports that a snapshot index payload was read back
// intact and CRC-valid but describes a state the recovered graph has left, so
// hydrating from it would install an index that disagrees with the graph.
//
// Two whole-image conditions raise it:
//
//   - The snapshot was NOT self-sufficient — it carried no mapper.bin, so the
//     node ids were re-derived by WAL-replay interning rather than restored.
//     The raw uint64 NodeIDs inside a payload then name nothing: graph.Mapper
//     has no un-intern, and a single discarded transaction shifts every later
//     id. There is also nothing to gain, because a non-self-sufficient snapshot
//     never lets the checkpointer truncate the WAL prefix.
//   - The manifest carried no `indexes_commit_ts`. Without the instant the
//     payloads describe there is no way to tell whether the WAL replayed on top
//     of them invalidates them, so they must not be used. Every snapshot written
//     by a present-time writer ([snapshot.WriteSnapshotFull] and friends), and
//     every snapshot that existed before that field, is in this state — which is
//     the format's back-compat guarantee: absent watermark means never hydrate.
//
// A THIRD staleness condition is deliberately NOT folded in here, because
// recovery cannot evaluate it: whether the replayed WAL suffix touched a
// PARTICULAR index's (label, property). Recovery reports the facts for that
// decision — [Result.WALTouchedNodeLabels],
// [Result.WALTouchedNodePropertyKeys], and the
// [Result.WALSuffixTouchesNodeIndex] predicate over them — and the caller, which
// alone knows which (label, property) each index name covers, applies it.
//
// Like [ErrIndexPayloadUnreadable] this is a per-payload reason code and never
// the function error of [Open] / [OpenCtx].
var ErrIndexPayloadStale = errors.New("recovery: snapshot index payload stale")

// ErrIndexPayloadNotFound reports that the snapshot declared no payload at all
// under the requested index name. It is the ordinary answer for an index created
// after the last checkpoint, for a directory with no snapshot, and for a
// snapshot whose indexes were not serialisable — none of which is a fault, and
// all of which mean "rebuild this index from the recovered graph".
//
// It is distinct from [ErrIndexPayloadUnreadable] on purpose: "the manifest never
// named this index" and "the manifest named it and the bytes are damaged" are
// different events, and only the second is worth a corruption metric or an
// operator-visible warning.
var ErrIndexPayloadNotFound = errors.New("recovery: no snapshot index payload")

// IndexPayload is the raw, verified byte payload of ONE indexes/<name>.bin
// component the snapshot's manifest declared, together with the reason it must
// not be used when that is the case.
//
// Bytes is non-nil and Err is nil exactly when recovery certified the payload
// hydratable on every ground it can judge alone (readable, CRC-valid, from a
// self-sufficient image that named the instant its payloads describe). Otherwise
// Bytes is nil and Err is one of [ErrIndexPayloadUnreadable] or
// [ErrIndexPayloadStale], wrapped with the index name.
//
// The bytes are opaque here: recovery does not interpret them, and only the
// registering engine — which knows which concrete index implementation each name
// belongs to — can feed them to index.Serializer.Deserialize.
//
// IndexPayload is an immutable value populated once by [Open] / [OpenCtx] and
// read-only thereafter, so it is safe for concurrent reads without external
// locking. Bytes is NOT copied on read: treat the slice as read-only, since
// every caller shares it.
type IndexPayload struct {
	// Name is the index name the manifest recorded, which is the name the
	// producing [index.Manager.CreateIndex] used.
	Name string
	// Bytes is the verified payload, or nil when Err is non-nil.
	Bytes []byte
	// Err is nil for a hydratable payload, or the reason it must not be used.
	Err error
}

// IndexPayloadFor returns the snapshot payload recovery certified hydratable for
// the index registered under name, or the reason no payload may be used.
//
// It is the ONLY supported lookup: [Result.SnapshotIndexPayloads] is exported
// for reporting and assertion, but a caller that walks it itself has to
// re-implement the Err check, and the whole point of the reason codes is that
// skipping that check is a silent wrong-answer bug. A non-nil error is always
// paired with nil bytes, so `b, err := res.IndexPayloadFor(n); if err != nil {
// rebuild }` is the complete contract.
//
// The returned slice is the payload itself, not a copy, and is shared by every
// caller: treat it as read-only.
//
// It is safe to call concurrently on a completed Result (which is read-only),
// and it is O(number of payloads) — a linear scan, because the payload count is
// the number of registered indexes and a map field on an exported struct would
// be externally mutable state.
func (r Result[N, W]) IndexPayloadFor(name string) ([]byte, error) {
	for i := range r.SnapshotIndexPayloads {
		p := &r.SnapshotIndexPayloads[i]
		if p.Name != name {
			continue
		}
		if p.Err != nil {
			return nil, p.Err
		}
		return p.Bytes, nil
	}
	return nil, fmt.Errorf("%w: %q", ErrIndexPayloadNotFound, name)
}

// WALSuffixTouchesNodeIndex reports whether the WAL suffix this recovery
// replayed could have changed the contents of a node index on
// (label, property) — the third hydration precondition, the one only the caller
// can pose because only the caller knows which pair an index name covers.
//
// It answers from the facts recovery collected while replaying:
// [Result.WALTouchedNodeLabels] and [Result.WALTouchedNodePropertyKeys]. True
// means the payload for such an index describes a state the suffix has left and
// the index must be rebuilt; false means nothing the suffix committed can have
// altered that index, so a CRC-valid payload from a self-sufficient, watermarked
// image is exactly its current content.
//
// # It is a conservative over-approximation, in the safe direction
//
// The two sets are unioned across the whole suffix rather than correlated per
// node, so a property write to key P on a node of some OTHER label still
// reports true for every index on (·, P). That refuses a hydration that would
// have been sound; it never permits one that is not. The alternative —
// per-(label, property) correlation — would have to model label and property
// writes arriving in either order across separate transactions, and buys nothing
// for the case that matters, which is an empty or index-irrelevant suffix.
//
// Both slices are sorted and de-duplicated, so this is O(log n) per side.
func (r Result[N, W]) WALSuffixTouchesNodeIndex(label, property string) bool {
	if _, found := slices.BinarySearch(r.WALTouchedNodeLabels, label); found {
		return true
	}
	_, found := slices.BinarySearch(r.WALTouchedNodePropertyKeys, property)
	return found
}

// indexImageReason decides whether the IMAGE as a whole permits its index
// payloads to be used, returning nil when it does and the wrapped
// [ErrIndexPayloadStale] reason when it does not.
//
// The two conditions are documented on [ErrIndexPayloadStale]: the image must be
// self-sufficient (so payload NodeIDs mean what they say), and its manifest must
// have named the instant the payloads describe (so the WAL replayed on top can be
// compared against them). Both are properties of the whole image, so one answer
// covers every payload in it.
//
// A zero indexesCommitTS is exactly "no watermark": the field is omitempty, so an
// absent watermark and a watermark of 0 encode identically, and both are refused.
// Refusing a genuine instant 0 costs nothing — a graph at instant 0 has committed
// nothing, so its indexes are empty and a rebuild is free.
func indexImageReason(selfSufficient bool, indexesCommitTS uint64) error {
	switch {
	case !selfSufficient:
		return fmt.Errorf("%w: snapshot is not self-sufficient (no mapper.bin), so payload node ids "+
			"were re-derived by replay interning and name nothing", ErrIndexPayloadStale)
	case indexesCommitTS == 0:
		return fmt.Errorf("%w: manifest carries no indexes_commit_ts, so the instant the payloads "+
			"describe is unknown and the replayed WAL cannot be compared against them", ErrIndexPayloadStale)
	default:
		return nil
	}
}

// classifyIndexPayloads converts the raw readbacks a snapshot load produced into
// the reported [IndexPayload] set, applying the two whole-image hydration
// preconditions recovery can judge on its own.
//
// imageReason is nil when the image itself is hydratable, or the wrapped
// [ErrIndexPayloadStale] explaining why none of its payloads may be used (not
// self-sufficient, or no `indexes_commit_ts` watermark). A per-payload
// unreadable condition (nil Bytes from [snapshot.LoadIndexes]) always wins over
// imageReason being nil, and imageReason always wins over a readable payload:
// an unreadable payload of an unhydratable image is reported as stale, because
// its unreadability is not what stops the caller using it.
//
// It also returns how many payloads came out hydratable, which is what
// [Result.SnapshotIndexes] reports.
func classifyIndexPayloads(rb []snapshot.IndexReadback, imageReason error) (out []IndexPayload, hydratable int) {
	if len(rb) == 0 {
		return nil, 0
	}
	out = make([]IndexPayload, 0, len(rb))
	for i := range rb {
		p := IndexPayload{Name: rb[i].Name}
		switch {
		case imageReason != nil:
			p.Err = fmt.Errorf("%w: %q", imageReason, rb[i].Name)
		case rb[i].Bytes == nil:
			p.Err = fmt.Errorf("%w: %q", ErrIndexPayloadUnreadable, rb[i].Name)
		default:
			p.Bytes = rb[i].Bytes
			hydratable++
		}
		out = append(out, p)
	}
	return out, hydratable
}

// touchSet accumulates the node-index-relevant facets a WAL replay touched: the
// node labels it added or removed, and the node property keys it wrote or
// deleted. It is the raw material behind [Result.WALTouchedNodeLabels] /
// [Result.WALTouchedNodePropertyKeys] and therefore behind
// [Result.WALSuffixTouchesNodeIndex].
//
// # What is in scope, and why the omissions are sound
//
// Only NODE label and NODE property facets can change a node index's contents,
// so those are what is collected:
//
//   - txn.OpSetNodeLabel / txn.OpRemoveNodeLabel contribute their label.
//   - txn.OpSetNodeProperty / txn.OpDelNodeProperty contribute their key.
//   - txn.OpRemoveNode contributes the removed node's labels AND property
//     keys. The removal branch already enumerates both to strip them, so
//     collecting them costs nothing extra. A node that a payload holds under
//     (L, P) necessarily carries L and P at the moment it is removed — unless an
//     earlier op in the same suffix already stripped them, and that op
//     contributed them itself.
//   - txn.OpAddNode is index-neutral: a freshly added (or revived) node
//     carries no label and no property until a later op gives it one, and that
//     op is collected.
//   - Every edge op is index-neutral, because the engine registers only NODE
//     indexes (bound hash and bound btree on a (label, property) pair); the
//     label bitmap index is never registered on the engine's manager.
//   - Index and constraint DDL ops never reach the apply path at all: the
//     accumulators intercept them upstream.
//
// The accumulation is allocation-free for a repeated facet: the label and key
// strings are already materialised by the apply path's own decode, and a map
// insert of an existing key allocates nothing.
//
// touchSet is used by a single replay goroutine and is NOT safe for concurrent
// use. A nil *touchSet is a valid no-op receiver, which is what a caller that
// does not want the facts passes.
type touchSet struct {
	labels map[string]struct{}
	keys   map[string]struct{}
}

// newTouchSet returns an empty accumulator.
func newTouchSet() *touchSet {
	return &touchSet{labels: make(map[string]struct{}), keys: make(map[string]struct{})}
}

// addLabel records that the replay added or removed label on some node.
func (t *touchSet) addLabel(label string) {
	if t == nil {
		return
	}
	if _, ok := t.labels[label]; !ok {
		t.labels[label] = struct{}{}
	}
}

// addKey records that the replay wrote or deleted the node property key.
func (t *touchSet) addKey(key string) {
	if t == nil {
		return
	}
	if _, ok := t.keys[key]; !ok {
		t.keys[key] = struct{}{}
	}
}

// sortedLabels returns the touched labels in deterministic ascending order, or
// nil when none were touched.
func (t *touchSet) sortedLabels() []string { return sortedKeysOf(t.labelSet()) }

// sortedKeys returns the touched property keys in deterministic ascending order,
// or nil when none were touched.
func (t *touchSet) sortedKeys() []string { return sortedKeysOf(t.keySet()) }

func (t *touchSet) labelSet() map[string]struct{} {
	if t == nil {
		return nil
	}
	return t.labels
}

func (t *touchSet) keySet() map[string]struct{} {
	if t == nil {
		return nil
	}
	return t.keys
}

// sortedKeysOf materialises m's keys in ascending order, returning nil for an
// empty set so a Result carrying no touched facet has nil slices rather than
// empty non-nil ones (which a reflect.DeepEqual assertion would distinguish).
func sortedKeysOf(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
