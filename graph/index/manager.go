// Package index coordinates the secondary indexes attached to a
// labelled property graph.
//
// A [Manager] owns a set of named indexes (label bitmap, hash
// exact-match, B+ tree range) and fans out mutations to every index
// that subscribes to the affected property or label. The fan-out is
// best-effort sequential: failures in one subscriber do not abort
// the others (subscribers are independent and idempotent).
package index

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// ErrIndexExists is returned by [Manager.CreateIndex] when the name
// is already in use.
var ErrIndexExists = errors.New("index: an index by that name already exists")

// ErrIndexNotFound is returned by [Manager.DropIndex] or
// [Manager.GetIndex] when the named index does not exist.
var ErrIndexNotFound = errors.New("index: no index by that name")

// ErrIndexCorrupted is returned by [Serializer.Deserialize] when the
// serialised form is structurally malformed or its CRC32C trailer
// does not match the payload. Callers (snapshot recovery in
// particular) treat this as "rebuild from the LPG" rather than as a
// fatal error.
var ErrIndexCorrupted = errors.New("index: serialized form corrupted")

// ErrIndexValueTypeUnsupported is returned by a generic index's
// Serialize / Deserialize methods when the value-type parameter is
// not in the supported on-disk encoding set.
//
// The set is per implementation and is wider than one type. The B+ tree
// (graph/index/btree) encodes string, int64, int32, int, uint64, uint32, uint and
// float64; the hash index (graph/index/hash) additionally encodes []byte and
// bool. The engine relies on that breadth: its numeric companion index is keyed
// by float64, so a float64-keyed btree MUST be serialisable for a numeric index
// to survive a checkpoint. The authoritative list is the table under
// "Supported value-type encodings" in docs/persistence.md.
//
// Callers whose value type is outside the set can convert to one of the
// supported types before registering the index for snapshot durability.
var ErrIndexValueTypeUnsupported = errors.New("index: value type not supported for serialization")

// ErrIndexBuildOverflow is returned by [Manager.FinishBuild] when more than
// [MaxBuildLogChanges] changes were fanned out while the index was being built,
// so the recorded catch-up log is no longer complete.
//
// It is a saturation signal, not a corruption: nothing was registered, the
// half-built index is discarded, and the statement can simply be retried. It is
// reported rather than absorbed because absorbing it is precisely the defect
// [Manager.BeginBuild] exists to prevent — an index registered while some of the
// writes concurrent with its build were never applied to it.
var ErrIndexBuildOverflow = errors.New("index: concurrent-change log overflowed during an index build")

// ErrIndexStateUndefined is what [Manager.Undefined] wraps once a commit-time
// delivery has been cut short by a panic ([Manager.MarkUndefined]): the indexes
// may hold part of a commit's changes, or none of a durable commit's, so no answer
// read from them can be trusted until they are rebuilt — which a reopen does, from
// the recovered graph. Matchable with [errors.Is].
var ErrIndexStateUndefined = errors.New("index: index state undefined: a commit-time delivery did not complete")

// Subscriber is implemented by every concrete index that wishes to
// receive change events from the [Manager]. The Apply method must
// be idempotent: replays of the same change must not produce
// duplicate state.
//
// Implementations must be safe for concurrent use: the [Manager] fans
// changes out to Apply while query goroutines read the same index
// concurrently, so a concrete index synchronises its own state
// internally (the built-in hash and label indexes hold an RWMutex). The
// Manager itself does not serialise an index's reads against its Apply
// calls.
type Subscriber interface {
	Apply(Change)
	// Kind returns a short stable identifier of the underlying index
	// implementation, used for introspection (e.g. "label", "hash",
	// "btree").
	Kind() string
}

// ResolvedApplier is implemented by a [Subscriber] whose Apply resolves part of
// a [Change] against the graph rather than from the change alone, and which can
// instead be handed that resolution as data.
//
// # Why the interface exists
//
// A bound index answers two questions about the changed node that the Change
// does not carry: is the node currently ELIGIBLE for this index (live, and
// carrying the bound label), and what is its CURRENT value of the bound property
// (a label add/remove carries no property payload). On the live fan-out both are
// asked at the instant the change is fanned out, which is the instant the
// committing transaction's state is final — the state the index must converge
// to.
//
// The build-log replay ([Manager.FinishBuild]) applies a change LATER, and
// asking those questions then answers about a different instant. rmp #2793
// measured the consequence: a second transaction's eager, uncommitted mutation,
// opened after the change was recorded and still open at the replay, made the
// replay insert a value nothing had committed and suppress the value the graph
// did hold. ApplyResolved closes that by taking the two answers from the log,
// where they were captured at fan-out time by the log's [BuildResolver].
//
// current is the node's raw property value as of the recording, in whatever
// representation the subscriber's own value projection accepts, and is nil when
// the node was absent or carried no such property. eligible is the recorded
// answer to the eligibility question. A subscriber must apply c using exactly
// the rules its Apply uses, substituting these two values for its own reads, so
// that the replay produces precisely the effects the live fan-out would have
// produced.
//
// Implementations must be safe for concurrent use on the same terms as
// [Subscriber.Apply].
type ResolvedApplier interface {
	Subscriber
	ApplyResolved(c Change, current any, eligible bool)
}

// NodeState answers, for one committing transaction, the two questions a bound
// index asks about a changed node: its raw value of a property, and whether it is
// eligible (live, and carrying a label). It is how the commit-time fan-out
// ([Manager.ApplyBatchInState]) resolves a change against the state the commit
// PRODUCES rather than against the graph's present, which also holds other
// transactions' uncommitted writes (rmp #2931).
//
// propID and labelID are the interned ids a binding stores. value is returned in
// the representation the subscriber's projection accepts; ok is false when the
// node is absent or carries no such property.
//
// An implementation is called only from the goroutine performing the fan-out and
// need not be safe for concurrent use.
type NodeState interface {
	// NodeValue's result is valid only until the next call on the same
	// NodeState: an implementation may return a pointer to a scratch value so
	// that the lookup allocates nothing. A caller that keeps the value calls
	// NodeValueRetained instead.
	NodeValue(id graph.NodeID, propID uint32) (value any, ok bool)
	// NodeValueRetained is NodeValue for a caller that keeps the result.
	NodeValueRetained(id graph.NodeID, propID uint32) (value any, ok bool)
	NodeEligible(id graph.NodeID, labelID uint32) bool
}

// StateApplier is implemented by a [Subscriber] that can resolve a change from a
// [NodeState] instead of reading the graph itself. [Manager.ApplyBatchInState]
// delivers through it; a subscriber that does not implement it receives
// [Subscriber.Apply].
//
// ApplyInState must apply c with exactly the rules Apply uses, substituting st's
// answers for its own reads. Implementations must be safe for concurrent use on
// the same terms as Apply.
type StateApplier interface {
	Subscriber
	ApplyInState(c Change, st NodeState)
}

// ChangeFilter is implemented by a [Subscriber] that can tell, from a change
// alone, whether applying it could modify the subscriber. [Manager.Concerns]
// uses it to spare a commit that touches no indexed coordinate the ordered,
// state-resolved fan-out. A subscriber that does not implement it is assumed to
// be concerned by every node change.
//
// Implementations must be safe for concurrent use: [Manager.Concerns] calls
// Concerns from every committing goroutine at once, under the Manager's read
// lock only. Concerns must not mutate the subscriber; the module's
// implementations read only the immutable index binding.
type ChangeFilter interface {
	Concerns(c Change) bool
}

// Serializer is implemented by indexes that can persist and restore
// their internal state through an [io.Writer] / [io.Reader] pair.
// The Manager type-asserts every registered [Subscriber] to this
// interface during snapshot writes; subscribers that do not
// implement Serializer are silently skipped (rebuild-on-restart).
//
// Implementations must:
//
//   - Write a fixed self-describing header (magic + format version) so
//     a future format bump can be detected on read.
//   - Cover the entire on-disk payload with a CRC32C trailer (uint32
//     little-endian) so corruption surfaces as [ErrIndexCorrupted].
//   - Be safe for concurrent reads from other goroutines while
//     Serialize executes (typically by holding the index's own
//     RLock for the duration of the write).
//
// Deserialize replaces the receiver's state with the contents of r.
// On any structural problem or CRC mismatch the function returns a
// wrapped [ErrIndexCorrupted] and leaves the receiver in its
// previous state.
type Serializer interface {
	Serialize(w io.Writer) error
	Deserialize(r io.Reader) error
}

// ChangeOp tags the shape of a [Change]. It is an immutable scalar with no
// methods, so it is safe for concurrent use.
type ChangeOp uint8

// Mutation kinds the Manager can fan out.
const (
	OpAddNodeLabel ChangeOp = iota + 1
	OpRemoveNodeLabel
	OpSetNodeProperty
	OpDelNodeProperty
	OpAddEdgeLabel
	OpRemoveEdgeLabel
	OpSetEdgeProperty
	OpDelEdgeProperty
)

// Change describes a single mutation observed by the [Manager].
// Each subscriber inspects the relevant fields and decides whether
// to update its own state.
//
// Property and Label fields are interned identifiers from the
// owning graph's registries (lpg.PropertyKeyID / lpg.LabelID),
// surfaced as uint32 so this package does not import the lpg
// package and create a cycle.
//
// A Change is delivered by value: [Manager.Apply] and [Manager.ApplyBatch] copy
// it into each [Subscriber.Apply] call and hold only a read lock, so several
// goroutines can be fanning changes out at the same time, each working on its
// own copy. Change is therefore safe for concurrent use. The one caveat is
// OldValue and NewValue: they carry lpg.PropertyValue values, which are
// immutable after construction except that their bytes and list variants expose
// slices aliasing the value's backing store, so a subscriber that retains such
// a slice must not mutate it.
type Change struct {
	// OldValue and NewValue are present only for property changes.
	// They are typed as any so this package stays generic across
	// every PropertyValue kind without importing the lpg package.
	OldValue any
	NewValue any
	Node     graph.NodeID
	Dst      graph.NodeID // edge changes only
	Property uint32       // 0 when not a property change
	Label    uint32       // 0 when not a label change
	Op       ChangeOp
}

// IsEdgeChange reports whether the change concerns an edge.
func (c Change) IsEdgeChange() bool {
	switch c.Op {
	case OpAddEdgeLabel, OpRemoveEdgeLabel, OpSetEdgeProperty, OpDelEdgeProperty:
		return true
	}
	return false
}

// Manager owns the set of named indexes attached to a graph and
// fans out mutations to every subscriber.
//
// Manager is safe for concurrent use.
type Manager struct {
	indexes map[string]Subscriber
	// builds holds the change logs of the index builds currently in flight.
	// It is written only under mu held exclusively and read under mu held
	// shared, so [Manager.Apply] and [Manager.ApplyBatch] observe a stable
	// slice for the whole fan-out. It is nil in the overwhelmingly common
	// case that no build is running, which is the only cost the fan-out pays
	// for this mechanism: one length check per call.
	builds []*BuildLog
	mu     sync.RWMutex
	// active mirrors len(indexes)+len(builds), republished under mu held
	// exclusively by every method that changes either, so [Manager.Active] can
	// answer without taking mu. It is read on the raw lpg write path, once per
	// mutation, which is why it is an atomic and not a lock (rmp #2848).
	active atomic.Int64
	// drainedThrough is the highest commit timestamp whose changes have been
	// fanned out, or [DrainedUnknown] once a change has arrived with no
	// timestamp; applying counts the state-resolved deliveries in progress. See
	// [Manager.DescribesSnapshot].
	drainedThrough atomic.Uint64
	applying       atomic.Int64
	// frontier reports the owning graph's visible commit frontier; see
	// [Manager.SetFrontierSource]. nil until set.
	frontier atomic.Pointer[func() uint64]
	// decisions counts the commits between their index decision and their
	// publication; see [Manager.EnterCommit] (rmp #2936).
	decisions mvcc.Gate
	// undefined holds the cause recorded by [Manager.MarkUndefined], or nil.
	undefined atomic.Pointer[error]
}

// MarkUndefined records that the indexes' state is undefined because a
// commit-time delivery did not complete; cause says why. It is sticky: the first
// cause is kept and later calls change nothing. The owner of the manager — the
// Cypher engine — reads [Manager.Undefined] and fail-stops.
//
// Safe for concurrent use, and a no-op on a nil Manager.
func (m *Manager) MarkUndefined(cause string) {
	if m == nil {
		return
	}
	err := fmt.Errorf("%w: %s", ErrIndexStateUndefined, cause)
	m.undefined.CompareAndSwap(nil, &err)
}

// Undefined returns nil while the indexes' state is defined, and an error
// wrapping [ErrIndexStateUndefined] once [Manager.MarkUndefined] has run. It is
// one atomic load.
//
// Safe for concurrent use; a nil Manager reports nil.
func (m *Manager) Undefined() error {
	if m == nil {
		return nil
	}
	if p := m.undefined.Load(); p != nil {
		return *p
	}
	return nil
}

// SetFrontierSource tells the manager how to read the visible commit frontier
// of the graph it serves. The graph installs it when the manager is attached
// ([github.com/FlavioCFOliveira/GoGraph/graph/lpg.Graph.SetIndexManager]).
//
// It is what lets registering an index raise the commit watermark of
// [Manager.DescribesSnapshot]: an index registered now is built from, and caught
// up to, commits a transaction that started earlier cannot see, so for that
// transaction the index describes the wrong instant (rmp #2812). Without a
// source, registration raises the watermark to [DrainedUnknown], which no
// snapshot passes.
//
// Safe for concurrent use.
func (m *Manager) SetFrontierSource(frontier func() uint64) {
	if m == nil {
		return
	}
	if frontier == nil {
		m.frontier.Store(nil)
		return
	}
	m.frontier.Store(&frontier)
}

// noteRegistration raises the commit watermark to the graph's visible frontier:
// the index just registered describes every commit up to there, so no snapshot
// that started earlier may treat it as its answer. The caller holds mu
// exclusively, so no delivery can interleave with the registration.
func (m *Manager) noteRegistration() {
	if f := m.frontier.Load(); f != nil {
		m.raiseDrainedThrough((*f)())
		return
	}
	m.raiseDrainedThrough(DrainedUnknown)
}

// DrainedUnknown is what the commit watermark of [Manager.DescribesSnapshot]
// becomes once a change has been fanned out without a commit timestamp
// ([Manager.Apply], [Manager.ApplyBatch]). It is above every real timestamp, so
// no snapshot can ever be proved described afterwards — the sound answer to "I
// cannot tell which commit this was".
const DrainedUnknown = ^uint64(0)

// raiseDrainedThrough moves [Manager.drainedThrough] up to at least ts. It is a
// compare-and-swap loop because concurrent committers finish out of timestamp
// order; a plain store could move it backwards.
func (m *Manager) raiseDrainedThrough(ts uint64) {
	for {
		cur := m.drainedThrough.Load()
		if ts <= cur || m.drainedThrough.CompareAndSwap(cur, ts) {
			return
		}
	}
}

// DescribesSnapshot reports whether the indexes, as read by a caller BEFORE this
// call, held exactly the commits a snapshot started at startTS sees — no more and
// no fewer. A caller that reads an index and then gets true may treat what it
// read as that snapshot's answer; on false it must not. Safe for concurrent use;
// a nil Manager has no index and reports true.
//
// # What it proves, and why the order of the loads matters
//
// The indexes are written at commit time and read at the present, so a reader
// running at a snapshot cannot, on its own, tell whether an index describes that
// snapshot. A committer's state-resolved delivery ([Manager.ApplyBatchInState],
// closed by [Manager.FinishApplied]) counts itself in, applies its changes,
// publishes its commit timestamp, raises the commit watermark to that timestamp,
// and counts itself out, in that order. So, for a reader that loads the counter
// and then the watermark after reading an index:
//
//   - every commit at or below startTS was published before the reader began,
//     and its changes were applied before its publication;
//   - a delivery whose effects the index read observed at all had counted itself
//     in before making them, so either the counter is still non-zero, or it has
//     since counted out, in which case it raised the watermark first — to its
//     own timestamp, which is above startTS unless the reader's snapshot
//     includes that commit.
//
// Either way a commit the snapshot cannot see makes this false.
//
// REGISTERING an index raises the same watermark, to the frontier at the instant
// of registration ([Manager.SetFrontierSource]). An index is backfilled from a
// snapshot and caught up to its registration, so it reflects commits a snapshot
// that started earlier cannot see — a node deleted after that snapshot is already
// missing from it — and such a snapshot must not treat it as its answer. Measured
// before this rule (rmp #2812 audit): an explicit transaction that saw a node,
// followed by a committed delete and a CREATE INDEX, made the transaction's MERGE
// create a duplicate. It is a single
// process-wide answer rather than a per-index one, which makes it coarse under
// concurrent commits and never wrong.
func (m *Manager) DescribesSnapshot(startTS uint64) bool {
	if m == nil {
		return true
	}
	// An index whose commit-time delivery was cut short describes no snapshot at
	// all. A panic in the delivery's first step leaves applying at zero, so this
	// is not implied by the counter below (rmp #2936 audit, L1).
	if m.undefined.Load() != nil {
		return false
	}
	if m.applying.Load() != 0 {
		return false
	}
	return m.drainedThrough.Load() <= startTS
}

// publishActiveLocked republishes [Manager.active]. The caller holds mu
// exclusively.
func (m *Manager) publishActiveLocked() {
	m.active.Store(int64(len(m.indexes) + len(m.builds)))
}

// Active reports whether at least one index is registered or being built. It is
// safe to call on a nil Manager, which has none.
//
// It is the question the graph's raw, index-bypassing mutators ask before they
// write (rmp #2848): an index is maintained ONLY by the change fan-out
// ([Manager.Apply], [Manager.ApplyBatch]), so a write that delivers no change
// while an index exists — or while one is being built, whose build log records
// only fanned-out changes — leaves that index silently stale.
//
// Active reads one atomic and takes no lock, so it is safe for concurrent use
// and cheap enough for every mutation to ask. A registration or build that
// begins concurrently with a caller's check is ordered by that atomic alone: the
// caller observes either the state before it or the state after it.
func (m *Manager) Active() bool {
	if m == nil {
		return false
	}
	return m.active.Load() > 0
}

// NewManager returns an empty Manager.
func NewManager() *Manager {
	return &Manager{indexes: make(map[string]Subscriber)}
}

// CreateIndex registers sub under name. Returns [ErrIndexExists]
// when the name is already taken.
func (m *Manager) CreateIndex(name string, sub Subscriber) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.indexes[name]; ok {
		return fmt.Errorf("%w: %q", ErrIndexExists, name)
	}
	m.indexes[name] = sub
	m.noteRegistration()
	m.publishActiveLocked()
	return nil
}

// DropIndex removes the named index.
func (m *Manager) DropIndex(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.indexes[name]; !ok {
		return fmt.Errorf("%w: %q", ErrIndexNotFound, name)
	}
	delete(m.indexes, name)
	m.publishActiveLocked()
	return nil
}

// GetIndex returns the subscriber registered under name. It is safe to call
// on a nil Manager and returns ErrIndexNotFound in that case.
func (m *Manager) GetIndex(name string) (Subscriber, error) {
	if m == nil {
		return nil, fmt.Errorf("%w: %q", ErrIndexNotFound, name)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	sub, ok := m.indexes[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrIndexNotFound, name)
	}
	return sub, nil
}

// ListIndexes returns the names of every currently registered index
// in unspecified order. It is safe to call on a nil Manager and returns
// nil in that case.
func (m *Manager) ListIndexes() []string {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.indexes))
	for n := range m.indexes {
		out = append(out, n)
	}
	return out
}

// Count returns the number of currently registered indexes. It is safe to
// call on a nil Manager and returns 0 in that case.
func (m *Manager) Count() int {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.indexes)
}

// Apply fans c out to every registered subscriber under a read lock
// so subscribers cannot be unregistered mid-update. The Manager itself
// does not enforce ordering across subscribers.
//
// Ordering contract (what a subscriber may rely on). Changes are
// delivered in the order the write path emits them — the sole delivery
// path is an [IndexBuffer] appended in mutation order and drained
// through [Manager.ApplyBatch]; nothing sorts, coalesces, or
// parallelises the stream. A subscriber must be:
//   - idempotent (a replayed change produces no duplicate state), and
//   - order-independent across changes to DIFFERENT facets of a node —
//     a property SET interleaved with a label add/remove converges to
//     the same postings in either order, because inserts are gated on
//     the node's final [Binding.Eligible]/[Binding.CurrentValue] state.
//
// A subscriber need NOT be order-independent across MULTIPLE changes to
// the SAME property key: those carry old→new payloads and must be
// applied in mutation order (which the delivery path guarantees).
// Recovery does not replay this stream at all — it rebuilds each index
// from the live graph via BulkLoad — so no legal path ever delivers
// same-key changes out of mutation order.
//
// Apply carries no commit timestamp, so it raises the commit watermark of [Manager.DescribesSnapshot] to
// [DrainedUnknown]; the engine's commit path uses [Manager.ApplyBatchInState].
func (m *Manager) Apply(c Change) {
	m.raiseDrainedThrough(DrainedUnknown)
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, sub := range m.indexes {
		sub.Apply(c)
	}
	// Record for every build in flight, under the SAME lock hold as the
	// fan-out, so a change is either recorded here or delivered to an index
	// that FinishBuild has already registered — never neither. See
	// [Manager.BeginBuild].
	for _, b := range m.builds {
		b.record(c)
	}
}

// ApplyBatch fans an ordered slice of changes out to every subscriber
// in order. The whole batch is applied under one read lock; this is
// the substrate consumed by future transaction integration (Sprint 3).
//
// ApplyBatch carries no commit timestamp and no committed state: subscribers
// resolve against the graph as it stands, and the watermark of [Manager.DescribesSnapshot] is raised
// to [DrainedUnknown]. The engine's commit path uses [Manager.ApplyBatchInState].
// An empty batch delivers nothing and raises nothing.
func (m *Manager) ApplyBatch(changes []Change) {
	if len(changes) == 0 {
		return
	}
	m.raiseDrainedThrough(DrainedUnknown)
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, sub := range m.indexes {
		for k := range changes {
			sub.Apply(changes[k])
		}
	}
	// The whole batch is recorded contiguously for every build in flight; see
	// [Manager.Apply] for why this shares the fan-out's lock hold.
	for _, b := range m.builds {
		b.recordBatch(changes)
	}
}

// Concerns reports whether delivering changes could modify any registered index
// or any index being built — that is, whether the batch must be delivered at
// all.
//
// A batch that concerns nothing may be dropped instead of delivered: every
// registered subscriber would ignore it, and no build is in flight to record it.
// A subscriber that implements [ChangeFilter] answers for itself; one that does
// not is assumed concerned by every NODE change. Any build in flight concerns
// every node change, because a build carries no filter of its own.
//
// Safe for concurrent use and safe on a nil Manager, which is concerned by
// nothing.
func (m *Manager) Concerns(changes []Change) bool {
	if m == nil || len(changes) == 0 || !m.Active() {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for k := range changes {
		c := changes[k]
		if len(m.builds) > 0 && !c.IsEdgeChange() {
			return true
		}
		for _, sub := range m.indexes {
			if f, ok := sub.(ChangeFilter); ok {
				if f.Concerns(c) {
					return true
				}
				continue
			}
			if !c.IsEdgeChange() {
				return true
			}
		}
	}
	return false
}

// ApplyBatchInState is [Manager.ApplyBatch] with every change resolved against
// st, the state the committing transaction's commit produces: a subscriber that
// implements [StateApplier] receives ApplyInState, any other receives Apply, and
// a bound build in flight records st's answers (see [Manager.BeginBoundBuild]).
//
// This is the delivery the engine's commit path uses. Resolving against the
// graph's present instead would read other transactions' uncommitted writes, and
// an index entry derived from one of those outlives that transaction when it
// rolls back (rmp #2931). The caller is responsible for making st describe
// exactly the committed state — lpg.CommitApplier is how the engine does that.
//
// The delivery stays OPEN when this returns: the caller must publish the commit
// timestamp and then close the delivery with exactly one [Manager.FinishApplied]
// carrying it. The commit timestamp is not an argument here because the engine
// allocates it only after the fan-out and publishes it at once, which keeps the
// window in which an allocated timestamp holds back the visibility frontier to a
// few instructions. See [Manager.DescribesSnapshot] for what the open delivery tells
// a reader. An empty batch still opens a delivery, so the pairing never depends
// on the batch.
func (m *Manager) ApplyBatchInState(changes []Change, st NodeState) {
	m.applying.Add(1)
	if len(changes) == 0 {
		return
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, sub := range m.indexes {
		sa, stateful := sub.(StateApplier)
		for k := range changes {
			if stateful {
				sa.ApplyInState(changes[k], st)
				continue
			}
			sub.Apply(changes[k])
		}
	}
	for _, b := range m.builds {
		b.recordBatchInState(changes, st)
	}
}

// FinishApplied closes the delivery the matching [Manager.ApplyBatchInState]
// opened, for the transaction that has just published commitTS: it raises the
// commit watermark to commitTS and only then counts the delivery out, the order
// [Manager.DescribesSnapshot] depends on. A commitTS of 0 means unknown and
// raises the watermark to [DrainedUnknown].
func (m *Manager) FinishApplied(commitTS uint64) {
	if commitTS == 0 {
		commitTS = DrainedUnknown
	}
	m.raiseDrainedThrough(commitTS)
	m.applying.Add(-1)
}
