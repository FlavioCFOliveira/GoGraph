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

	"gograph/graph"
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
// not in the supported on-disk encoding set (currently: string).
// Callers can convert their value type to string before registering
// the index for snapshot durability.
var ErrIndexValueTypeUnsupported = errors.New("index: value type not supported for serialization")

// Subscriber is implemented by every concrete index that wishes to
// receive change events from the [Manager]. The Apply method must
// be idempotent: replays of the same change must not produce
// duplicate state.
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

// ChangeOp tags the shape of a [Change].
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
type Change struct {
	Op       ChangeOp
	Node     graph.NodeID
	Dst      graph.NodeID // edge changes only
	Property uint32       // 0 when not a property change
	Label    uint32       // 0 when not a label change

	// OldValue and NewValue are present only for property changes.
	// They are typed as any so this package stays generic across
	// every PropertyValue kind without importing the lpg package.
	OldValue any
	NewValue any
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
	mu      sync.RWMutex
	indexes map[string]Subscriber
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

// GetIndex returns the subscriber registered under name.
func (m *Manager) GetIndex(name string) (Subscriber, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sub, ok := m.indexes[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrIndexNotFound, name)
	}
	return sub, nil
}

// ListIndexes returns the names of every currently registered index
// in unspecified order.
func (m *Manager) ListIndexes() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.indexes))
	for n := range m.indexes {
		out = append(out, n)
	}
	return out
}

// Count returns the number of currently registered indexes.
func (m *Manager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.indexes)
}

// Apply fans c out to every registered subscriber under a read lock
// so subscribers cannot be unregistered mid-update. The Manager
// itself does not enforce ordering across subscribers — each
// subscriber is expected to be order-independent on the change
// stream it observes.
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
