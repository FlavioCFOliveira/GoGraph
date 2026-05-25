package lpg

import (
	"sync"
	"time"
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
)

// PropertyValue is a tagged union of typed property values. It is
// laid out as a single (kind, any) pair, totalling 24 bytes on a
// 64-bit platform regardless of the inhabited variant. The zero
// value is invalid; values are constructed via the typed
// constructors ([StringValue], [Int64Value], etc.).
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

// PropertyKeyID is the compact identifier of an interned property
// name.
type PropertyKeyID uint32

// PropertyKeyRegistry interns property names and assigns sequential
// PropertyKeyIDs. It is safe for concurrent use.
type PropertyKeyRegistry struct {
	mu      sync.RWMutex
	forward map[string]PropertyKeyID
	reverse []string
}

// NewPropertyKeyRegistry returns an empty registry.
func NewPropertyKeyRegistry() *PropertyKeyRegistry {
	return &PropertyKeyRegistry{forward: make(map[string]PropertyKeyID)}
}

// Intern returns a stable PropertyKeyID for name.
func (r *PropertyKeyRegistry) Intern(name string) PropertyKeyID {
	r.mu.RLock()
	if id, ok := r.forward[name]; ok {
		r.mu.RUnlock()
		return id
	}
	r.mu.RUnlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if id, ok := r.forward[name]; ok {
		return id
	}
	id := PropertyKeyID(len(r.reverse))
	r.reverse = append(r.reverse, name)
	r.forward[name] = id
	return id
}

// Lookup returns the PropertyKeyID for name and true when known.
func (r *PropertyKeyRegistry) Lookup(name string) (PropertyKeyID, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.forward[name]
	return id, ok
}

// Resolve returns the name interned under id.
func (r *PropertyKeyRegistry) Resolve(id PropertyKeyID) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if uint64(id) >= uint64(len(r.reverse)) {
		return "", false
	}
	return r.reverse[id], true
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
	bag, ok := s.m[id]
	if !ok {
		bag = make(map[PropertyKeyID]PropertyValue)
		s.m[id] = bag
	}
	bag[keyID] = value
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
	v, ok := bag[keyID]
	return v, ok
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
		delete(bag, keyID)
		if len(bag) == 0 {
			delete(s.m, id)
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
	out := make(map[string]PropertyValue, len(bag))
	for k, v := range bag {
		if name, ok := g.propKeys().Resolve(k); ok {
			out[name] = v
		}
	}
	s.mu.RUnlock()
	return out
}
