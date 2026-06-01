package plan

import "github.com/FlavioCFOliveira/GoGraph/graph/index"

// IndexKind classifies the physical implementation of a registered index
// as seen by the planner.
type IndexKind uint8

// Recognised IndexKind values. IndexKindUnknown is returned for any
// subscriber whose Kind() string is not in the set {"label","hash","btree"}.
const (
	IndexKindUnknown IndexKind = iota
	IndexKindLabel             // label bitmap — supports label existence checks
	IndexKindHash              // hash exact-match — supports (prop == value) predicates
	IndexKindBTree             // B+ tree range — supports (lo <= prop <= hi) predicates
)

// IndexEntry describes one registered index from the planner's perspective.
type IndexEntry struct {
	Name string
	Kind IndexKind
	// Subscriber is the raw subscriber; planner rules can type-assert to
	// obtain kind-specific query APIs (e.g. hash.Lookup, btree.Range).
	Subscriber index.Subscriber
}

// IndexRegistry is a per-query snapshot of the registered indexes.
// It is created once per plan cycle and held for the duration of
// planning; it must NOT be shared across concurrent plan cycles.
type IndexRegistry struct {
	entries []IndexEntry
}

// classifyKind maps a subscriber's Kind() string to an IndexKind constant.
func classifyKind(s index.Subscriber) IndexKind {
	switch s.Kind() {
	case "label":
		return IndexKindLabel
	case "hash":
		return IndexKindHash
	case "btree":
		return IndexKindBTree
	default:
		return IndexKindUnknown
	}
}

// NewIndexRegistry snapshots the current state of mgr into an IndexRegistry.
// If mgr is nil, an empty registry is returned.
func NewIndexRegistry(mgr *index.Manager) *IndexRegistry {
	if mgr == nil {
		return &IndexRegistry{}
	}
	names := mgr.ListIndexes()
	entries := make([]IndexEntry, 0, len(names))
	for _, name := range names {
		sub, err := mgr.GetIndex(name)
		if err != nil {
			// Index was concurrently removed between ListIndexes and GetIndex;
			// skip it — the snapshot reflects the state at snapshot time.
			continue
		}
		entries = append(entries, IndexEntry{
			Name:       name,
			Kind:       classifyKind(sub),
			Subscriber: sub,
		})
	}
	return &IndexRegistry{entries: entries}
}

// All returns all registered entries in unspecified order.
func (r *IndexRegistry) All() []IndexEntry {
	if len(r.entries) == 0 {
		return nil
	}
	out := make([]IndexEntry, len(r.entries))
	copy(out, r.entries)
	return out
}

// ByKind returns entries whose Kind matches k.
func (r *IndexRegistry) ByKind(k IndexKind) []IndexEntry {
	var out []IndexEntry
	for i := range r.entries {
		if r.entries[i].Kind == k {
			out = append(out, r.entries[i])
		}
	}
	return out
}

// Lookup returns the first entry whose Name matches name, and whether found.
func (r *IndexRegistry) Lookup(name string) (IndexEntry, bool) {
	for i := range r.entries {
		if r.entries[i].Name == name {
			return r.entries[i], true
		}
	}
	return IndexEntry{}, false
}

// HasHash reports whether at least one hash index is registered.
func (r *IndexRegistry) HasHash() bool {
	for i := range r.entries {
		if r.entries[i].Kind == IndexKindHash {
			return true
		}
	}
	return false
}

// HasBTree reports whether at least one btree index is registered.
func (r *IndexRegistry) HasBTree() bool {
	for i := range r.entries {
		if r.entries[i].Kind == IndexKindBTree {
			return true
		}
	}
	return false
}
