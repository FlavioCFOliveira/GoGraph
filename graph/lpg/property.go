package lpg

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// PropertyKind tags a [PropertyValue] with its underlying Go type.
type PropertyKind uint8

// The supported property kinds. They are stable across releases — new
// kinds extend this enum; existing values must not be reordered or
// reused.
const (
	PropString PropertyKind = iota + 1
	PropInt64
	PropFloat64
	PropBool
	PropTime
	PropBytes
	PropList // ordered list of PropertyValue elements; v is []PropertyValue
)

// PropertyValue is a tagged union of typed property values. It is
// laid out as a single (kind, any) pair, totalling 24 bytes on a
// 64-bit platform regardless of the inhabited variant. The zero
// value is invalid; values are constructed via the typed
// constructors ([StringValue], [Int64Value], etc.).
//
// A PropertyValue is immutable after construction and is copied by value, so
// it is safe for concurrent reads by multiple goroutines without external
// locking. The one caveat is the slice-bearing variants: [PropertyValue.Bytes]
// and [PropertyValue.List] return slices that alias the value's backing store,
// so callers must not mutate the returned slice (doing so would mutate the
// otherwise-immutable value and break the concurrency guarantee).
type PropertyValue struct {
	kind PropertyKind
	v    any
}

// Kind returns the underlying type tag.
func (p PropertyValue) Kind() PropertyKind { return p.kind }

// String returns the string value and true when v carries a string,
// the zero value and false otherwise.
func (p PropertyValue) String() (string, bool) {
	if p.kind != PropString {
		return "", false
	}
	s, _ := p.v.(string)
	return s, true
}

// Int64 returns the int64 value and true when v carries an int64.
func (p PropertyValue) Int64() (int64, bool) {
	if p.kind != PropInt64 {
		return 0, false
	}
	i, _ := p.v.(int64)
	return i, true
}

// Float64 returns the float64 value and true when v carries a float64.
func (p PropertyValue) Float64() (float64, bool) {
	if p.kind != PropFloat64 {
		return 0, false
	}
	f, _ := p.v.(float64)
	return f, true
}

// Bool returns the bool value and true when v carries a bool.
func (p PropertyValue) Bool() (val, ok bool) {
	if p.kind != PropBool {
		return false, false
	}
	b, _ := p.v.(bool)
	return b, true
}

// Time returns the time.Time value and true when v carries one.
func (p PropertyValue) Time() (time.Time, bool) {
	if p.kind != PropTime {
		return time.Time{}, false
	}
	t, _ := p.v.(time.Time)
	return t, true
}

// Bytes returns the []byte value and true when v carries one. The
// returned slice aliases the value held by v.
func (p PropertyValue) Bytes() ([]byte, bool) {
	if p.kind != PropBytes {
		return nil, false
	}
	b, _ := p.v.([]byte)
	return b, true
}

// List returns the []PropertyValue elements and true when v carries a
// PropList. The returned slice aliases the value held by v; callers
// must not modify it.
func (p PropertyValue) List() ([]PropertyValue, bool) {
	if p.kind != PropList {
		return nil, false
	}
	elems, _ := p.v.([]PropertyValue)
	return elems, true
}

// String constructors.

// StringValue builds a PropString.
func StringValue(s string) PropertyValue { return PropertyValue{kind: PropString, v: s} }

// Int64Value builds a PropInt64.
func Int64Value(i int64) PropertyValue { return PropertyValue{kind: PropInt64, v: i} }

// Float64Value builds a PropFloat64.
func Float64Value(f float64) PropertyValue { return PropertyValue{kind: PropFloat64, v: f} }

// BoolValue builds a PropBool.
func BoolValue(b bool) PropertyValue { return PropertyValue{kind: PropBool, v: b} }

// TimeValue builds a PropTime.
func TimeValue(t time.Time) PropertyValue { return PropertyValue{kind: PropTime, v: t} }

// BytesValue builds a PropBytes wrapping b (no copy).
func BytesValue(b []byte) PropertyValue { return PropertyValue{kind: PropBytes, v: b} }

// ListValue builds a PropList from elems. The slice is stored directly
// (no copy); callers must not modify elems after calling ListValue.
func ListValue(elems []PropertyValue) PropertyValue {
	return PropertyValue{kind: PropList, v: elems}
}

// PropertyKeyID is the compact identifier of an interned property
// name.
type PropertyKeyID uint32

// propertyKeyNames is an immutable id→name table published by
// [PropertyKeyRegistry] via copy-on-write. Once stored it is never
// mutated; a new interning allocates a fresh table. Readers load the
// pointer once with zero synchronisation, so the read path (Resolve) is
// fully lock-free.
type propertyKeyNames struct {
	names []string
}

// PropertyKeyRegistry interns property names and assigns sequential
// PropertyKeyIDs. It is safe for concurrent use.
//
// The read path ([PropertyKeyRegistry.Resolve]) is lock-free: it loads
// an immutable id→name snapshot through an [atomic.Pointer] and indexes
// into it without taking any lock. The write path
// ([PropertyKeyRegistry.Intern] of a previously unseen name) serialises
// under a mutex, builds a new immutable snapshot extended by one entry,
// and atomically publishes it. The ordering guarantee is identical to
// [LabelRegistry]: Intern publishes the snapshot carrying names[id]
// before returning id, so any reader that observes id in a property bag
// observes (by release/acquire ordering through that bag's publication) a
// snapshot at least as new as the one Intern published. Resolve
// therefore never misses a live id.
type PropertyKeyRegistry struct {
	// mu serialises Intern (write path) and guards forward. It is never
	// taken on the read path.
	mu      sync.Mutex
	forward map[string]PropertyKeyID
	// snap holds the immutable id→name table. Loaded lock-free by
	// Resolve; swapped under mu by Intern.
	snap atomic.Pointer[propertyKeyNames]
}

// NewPropertyKeyRegistry returns an empty registry.
func NewPropertyKeyRegistry() *PropertyKeyRegistry {
	r := &PropertyKeyRegistry{forward: make(map[string]PropertyKeyID)}
	r.snap.Store(&propertyKeyNames{})
	return r
}

// Intern returns a stable PropertyKeyID for name. It runs on the write
// path only (property assignment), so it serialises under the write
// mutex; the steady-state property vocabulary is small and stable.
func (r *PropertyKeyRegistry) Intern(name string) PropertyKeyID {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id, ok := r.forward[name]; ok {
		return id
	}
	cur := r.snap.Load()
	id := PropertyKeyID(len(cur.names))
	next := &propertyKeyNames{names: make([]string, len(cur.names)+1)}
	copy(next.names, cur.names)
	next.names[id] = name
	r.snap.Store(next)
	r.forward[name] = id
	return id
}

// Lookup returns the PropertyKeyID for name and true when known.
func (r *PropertyKeyRegistry) Lookup(name string) (PropertyKeyID, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.forward[name]
	return id, ok
}

// Resolve returns the name interned under id. It is lock-free: it loads
// the immutable id→name snapshot once and indexes into it.
func (r *PropertyKeyRegistry) Resolve(id PropertyKeyID) (string, bool) {
	s := r.snap.Load()
	if uint64(id) >= uint64(len(s.names)) {
		return "", false
	}
	return s.names[id], true
}

// SetNodeProperty records the named property on n with the given
// value, inserting n into the graph if necessary. Returns the error
// from the underlying [adjlist.AdjList.AddNode] when present, or any
// error returned by the installed [SchemaValidator].
func (g *Graph[N, W]) SetNodeProperty(n N, key string, value PropertyValue) error {
	if v := g.validator.load(); v != nil {
		if err := v.Validate(key, value); err != nil {
			return err
		}
	}
	if err := g.adj.AddNode(n); err != nil {
		return err
	}
	id, _ := g.adj.Mapper().Lookup(n)
	keyID := g.propKeys().Intern(key)
	s := g.nodePropShardFor(id)
	s.mu.Lock()
	// propBag is stored by value, so mutate a local copy and write it back.
	// In the small tier set may append to (and thus reallocate) the pairs
	// slice, and a first Set on a new node starts from the zero bag; the
	// write-back makes both visible.
	bag := s.m[id]
	bag.set(keyID, value)
	s.m[id] = bag
	s.mu.Unlock()
	return nil
}

// GetNodeProperty returns the property value attached to n under
// key, and a bool reporting whether the property is set.
func (g *Graph[N, W]) GetNodeProperty(n N, key string) (PropertyValue, bool) {
	id, ok := g.adj.Mapper().Lookup(n)
	if !ok {
		return PropertyValue{}, false
	}
	keyID, ok := g.propKeys().Lookup(key)
	if !ok {
		return PropertyValue{}, false
	}
	s := g.nodePropShardFor(id)
	s.mu.RLock()
	defer s.mu.RUnlock()
	bag, ok := s.m[id]
	if !ok {
		return PropertyValue{}, false
	}
	return bag.get(keyID)
}

// DelNodeProperty removes the named property from n. No-op if absent.
func (g *Graph[N, W]) DelNodeProperty(n N, key string) {
	id, ok := g.adj.Mapper().Lookup(n)
	if !ok {
		return
	}
	keyID, ok := g.propKeys().Lookup(key)
	if !ok {
		return
	}
	s := g.nodePropShardFor(id)
	s.mu.Lock()
	if bag, ok2 := s.m[id]; ok2 {
		// propBag is stored by value; write the mutated copy back, dropping
		// the node entry entirely when the last property goes so an empty
		// bag never lingers (preserving the prior delete-when-empty contract).
		if bag.del(keyID) {
			delete(s.m, id)
		} else {
			s.m[id] = bag
		}
	}
	s.mu.Unlock()
}

// NodeProperties returns a snapshot of every property currently
// attached to n.
func (g *Graph[N, W]) NodeProperties(n N) map[string]PropertyValue {
	id, ok := g.adj.Mapper().Lookup(n)
	if !ok {
		return nil
	}
	s := g.nodePropShardFor(id)
	s.mu.RLock()
	bag, ok := s.m[id]
	if !ok {
		s.mu.RUnlock()
		return nil
	}
	out := make(map[string]PropertyValue, bag.len())
	bag.forEach(func(k PropertyKeyID, v PropertyValue) {
		if name, ok := g.propKeys().Resolve(k); ok {
			out[name] = v
		}
	})
	s.mu.RUnlock()
	return out
}

// NodePropertiesByID is the NodeID-keyed counterpart of [Graph.NodeProperties].
// It skips the external-key → NodeID Mapper lookup, so callers that already
// hold the NodeID — chiefly the Cypher result-materialisation path, which
// resolves the NodeID once for identity and then needs both properties and
// labels — avoid a redundant Mapper round-trip per node. The returned map is a
// fresh copy owned by the caller; it is nil when id has no recorded
// properties. Concurrency-safe under the same contract as NodeProperties.
func (g *Graph[N, W]) NodePropertiesByID(id graph.NodeID) map[string]PropertyValue {
	s := g.nodePropShardFor(id)
	s.mu.RLock()
	bag, ok := s.m[id]
	if !ok {
		s.mu.RUnlock()
		return nil
	}
	out := make(map[string]PropertyValue, bag.len())
	bag.forEach(func(k PropertyKeyID, v PropertyValue) {
		if name, ok := g.propKeys().Resolve(k); ok {
			out[name] = v
		}
	})
	s.mu.RUnlock()
	return out
}

// NodePropertiesByIDFunc invokes visit once per property attached to the node
// identified by id, passing the resolved property name and a value copy of the
// PropertyValue. It is the allocation-fusing counterpart of
// [Graph.NodePropertiesByID]: callers that immediately re-key every property
// into a different map (chiefly the Cypher result-materialisation path, which
// converts each lpg.PropertyValue into a cypher/expr value) would otherwise
// allocate a throwaway intermediate map[string]PropertyValue only to range over
// it once. Streaming the bag through visit lets the caller build its target map
// directly, removing that intermediate allocation per returned node.
//
// visit is called zero times for a node with no recorded properties (and for an
// unknown id). The iteration order is unspecified, matching Go map iteration.
//
// Concurrency and isolation: visit runs while the property shard's read lock is
// held, so it observes a consistent snapshot of the node's properties relative
// to any concurrent writer holding the shard write lock — identical to the
// guarantee of [Graph.NodePropertiesByID]. visit therefore MUST NOT call back
// into any Graph method that takes a property-shard lock (it would deadlock) and
// MUST NOT retain the PropertyValue beyond the callback in a way that aliases
// graph-internal state; the PropertyValue passed in is a value copy, so copying
// it out (or deriving an independent value from it) is safe and is the intended
// use.
func (g *Graph[N, W]) NodePropertiesByIDFunc(id graph.NodeID, visit func(name string, pv PropertyValue)) {
	s := g.nodePropShardFor(id)
	s.mu.RLock()
	bag, ok := s.m[id]
	if !ok {
		s.mu.RUnlock()
		return
	}
	bag.forEach(func(k PropertyKeyID, v PropertyValue) {
		if name, ok := g.propKeys().Resolve(k); ok {
			visit(name, v)
		}
	})
	s.mu.RUnlock()
}

// NodePropertyByID returns the single property keyed by name attached to the
// node identified by id, without materialising the node's full property map. It
// is the single-key counterpart of [Graph.NodePropertiesByID] and exists for
// the Cypher scalar-projection fast path: a predicate or projection that reads
// only n.name from a bound node fetches just that one value instead of copying
// every property into a fresh map per row.
//
// The boolean reports whether the property is present (false for both an
// unknown key name and a node that carries no such property), mirroring the
// missing-key-is-null semantics of openCypher property access. The returned
// PropertyValue is a value copy owned by the caller. Concurrency-safe under the
// same contract as [Graph.NodeProperties]: the read holds the property shard's
// read lock for the duration of the lookup, so it observes a consistent view of
// the node's properties relative to any concurrent writer holding the shard
// write lock.
func (g *Graph[N, W]) NodePropertyByID(id graph.NodeID, key string) (PropertyValue, bool) {
	// Resolve the key name to its interned id without interning a new one: an
	// unknown key cannot be present on any node, so a miss here is a definite
	// "absent" answer and avoids polluting the registry with query-time names.
	kid, ok := g.propKeys().Lookup(key)
	if !ok {
		return PropertyValue{}, false
	}
	s := g.nodePropShardFor(id)
	s.mu.RLock()
	bag, ok := s.m[id]
	if !ok {
		s.mu.RUnlock()
		return PropertyValue{}, false
	}
	v, ok := bag.get(kid)
	s.mu.RUnlock()
	return v, ok
}
