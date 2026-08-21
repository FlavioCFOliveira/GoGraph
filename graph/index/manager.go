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

	"github.com/FlavioCFOliveira/GoGraph/graph"
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
	mu      sync.RWMutex
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
func (m *Manager) Apply(c Change) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, sub := range m.indexes {
		sub.Apply(c)
	}
}

// ApplyBatch fans an ordered slice of changes out to every subscriber
// in order. The whole batch is applied under one read lock; this is
// the substrate consumed by future transaction integration (Sprint 3).
func (m *Manager) ApplyBatch(changes []Change) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, sub := range m.indexes {
		for k := range changes {
			sub.Apply(changes[k])
		}
	}
}
