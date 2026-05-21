// Package expr defines the runtime value model for the Cypher executor.
//
// # Value model
//
// A [Value] is a typed, immutable datum produced or consumed by an executor
// operator. Every value carries a [Kind] tag used by the planner and executor
// to dispatch without interface boxing in hot paths.
//
// # Three-valued logic (3VL)
//
// NULL is a first-class singleton ([Null]). Comparisons involving NULL return
// NULL, not false, in accordance with openCypher 9 §4.1.3. Callers that need
// boolean truth must use [IsTruthy].
//
// # Ordering
//
// [Compare] implements the openCypher 9 total ordering for use in ORDER BY:
// NULLs sort last. Within a type the ordering matches Go's natural ordering.
// Across different non-null types the canonical sequence is:
//
//	Path < Node < Relationship < Map < List < String < Boolean < Float < Integer
//
// # Concurrency
//
// All Value implementations are immutable after construction. Concurrent reads
// are safe without external locking.
package expr

import (
	"fmt"
	"math"
)

// ─────────────────────────────────────────────────────────────────────────────
// Kind
// ─────────────────────────────────────────────────────────────────────────────

// Kind identifies the concrete type of a [Value].
type Kind uint8

const (
	// KindNull is the NULL kind.
	KindNull Kind = iota
	// KindInteger is the 64-bit signed integer kind.
	KindInteger
	// KindFloat is the 64-bit IEEE-754 float kind.
	KindFloat
	// KindString is the UTF-8 string kind.
	KindString
	// KindBool is the boolean kind.
	KindBool
	// KindList is the ordered list kind.
	KindList
	// KindMap is the property map kind.
	KindMap
	// KindNode is the graph node kind.
	KindNode
	// KindRelationship is the graph relationship kind.
	KindRelationship
	// KindPath is the graph path kind.
	KindPath
)

// String returns a human-readable label for the kind.
func (k Kind) String() string {
	switch k {
	case KindNull:
		return "Null"
	case KindInteger:
		return "Integer"
	case KindFloat:
		return "Float"
	case KindString:
		return "String"
	case KindBool:
		return "Bool"
	case KindList:
		return "List"
	case KindMap:
		return "Map"
	case KindNode:
		return "Node"
	case KindRelationship:
		return "Relationship"
	case KindPath:
		return "Path"
	case KindDate, KindLocalDateTime, KindDateTime, KindLocalTime, KindTime, KindDuration:
		return temporalKindLabel(k)
	default:
		return fmt.Sprintf("Kind(%d)", uint8(k))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Value interface
// ─────────────────────────────────────────────────────────────────────────────

// Value is a typed, immutable Cypher runtime datum. Implementations must be
// comparable by [Equal] and hashable by [Hash]. All implementations in this
// package are safe for concurrent reads without external locking.
type Value interface {
	// Kind returns the kind tag of this value.
	Kind() Kind

	// Equal returns the three-valued equality result: Null if either operand is
	// Null, BoolValue(true/false) otherwise. Callers must never compare Value
	// using Go == ; use Equal and then [IsTruthy].
	Equal(other Value) Value

	// Hash returns a hash suitable for use as a map key. Two values that are
	// equal (per openCypher semantics, i.e., IsTruthy(a.Equal(b))) must have
	// the same hash. NULL has a defined hash (0) but must never be used as a
	// map key in practice.
	Hash() uint64

	// String returns a human-readable representation of the value.
	String() string
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// IsTruthy returns true iff v is BoolValue(true). NULL and false both return
// false, matching Cypher predicate semantics.
func IsTruthy(v Value) bool {
	b, ok := v.(BoolValue)
	return ok && bool(b)
}

// IsNull reports whether v is the NULL singleton.
func IsNull(v Value) bool { return v.Kind() == KindNull }

// ─────────────────────────────────────────────────────────────────────────────
// nullValue
// ─────────────────────────────────────────────────────────────────────────────

// nullValue is the concrete type of the NULL singleton. It is unexported;
// callers use the [Null] variable.
type nullValue struct{}

// Null is the NULL singleton. Comparisons against NULL always return Null.
//
//nolint:gochecknoglobals // package-level singleton is the intended design (3VL null)
var Null Value = nullValue{}

func (nullValue) Kind() Kind     { return KindNull }
func (nullValue) Hash() uint64   { return 0 }
func (nullValue) String() string { return "null" }

// Equal always returns Null per three-valued logic: NULL compared to anything
// is NULL.
func (nullValue) Equal(_ Value) Value { return Null }

// ─────────────────────────────────────────────────────────────────────────────
// IntegerValue
// ─────────────────────────────────────────────────────────────────────────────

// IntegerValue is a 64-bit signed integer Cypher value.
type IntegerValue int64

// Kind implements [Value].
func (v IntegerValue) Kind() Kind { return KindInteger }

// Hash implements [Value].
func (v IntegerValue) Hash() uint64 {
	// XOR-fold so that integer and float hashes agree for representable values.
	return uint64(v) ^ (uint64(v) >> 32)
}

// String returns the decimal representation of the integer.
func (v IntegerValue) String() string { return fmt.Sprintf("%d", int64(v)) }

// Equal returns Null if other is Null, BoolValue(true) if both are
// IntegerValue with the same value, BoolValue(false) otherwise.
func (v IntegerValue) Equal(other Value) Value {
	if IsNull(other) {
		return Null
	}
	o, ok := other.(IntegerValue)
	return BoolValue(ok && v == o)
}

// ─────────────────────────────────────────────────────────────────────────────
// FloatValue
// ─────────────────────────────────────────────────────────────────────────────

// FloatValue is a 64-bit IEEE-754 floating-point Cypher value.
type FloatValue float64

// Kind implements [Value].
func (v FloatValue) Kind() Kind { return KindFloat }

// Hash implements [Value].
func (v FloatValue) Hash() uint64 {
	bits := math.Float64bits(float64(v))
	return bits ^ (bits >> 32)
}

// String returns the decimal representation of the float.
func (v FloatValue) String() string { return fmt.Sprintf("%g", float64(v)) }

// Equal returns Null if other is Null, BoolValue per IEEE-754 equality
// otherwise (NaN != NaN).
func (v FloatValue) Equal(other Value) Value {
	if IsNull(other) {
		return Null
	}
	o, ok := other.(FloatValue)
	return BoolValue(ok && float64(v) == float64(o))
}

// ─────────────────────────────────────────────────────────────────────────────
// StringValue
// ─────────────────────────────────────────────────────────────────────────────

// StringValue is a UTF-8 string Cypher value.
type StringValue string

// Kind implements [Value].
func (v StringValue) Kind() Kind { return KindString }

// String returns the value enclosed in double quotes.
func (v StringValue) String() string { return fmt.Sprintf("%q", string(v)) }

// Hash implements [Value] using FNV-1a for zero-alloc performance.
func (v StringValue) Hash() uint64 {
	const (
		offset64 uint64 = 14695981039346656037
		prime64  uint64 = 1099511628211
	)
	h := offset64
	for i := 0; i < len(v); i++ {
		h ^= uint64(v[i])
		h *= prime64
	}
	return h
}

// Equal returns Null if other is Null, BoolValue per string equality otherwise.
func (v StringValue) Equal(other Value) Value {
	if IsNull(other) {
		return Null
	}
	o, ok := other.(StringValue)
	return BoolValue(ok && v == o)
}

// ─────────────────────────────────────────────────────────────────────────────
// BoolValue
// ─────────────────────────────────────────────────────────────────────────────

// BoolValue is a Cypher boolean value (true or false). Note that Cypher's
// three-valued logic introduces a third truth state, NULL, which is not
// representable as BoolValue; use [Null] for that.
type BoolValue bool

// Kind implements [Value].
func (v BoolValue) Kind() Kind { return KindBool }

// String returns "true" or "false".
func (v BoolValue) String() string {
	if bool(v) {
		return "true"
	}
	return "false"
}

// Hash implements [Value].
func (v BoolValue) Hash() uint64 {
	if bool(v) {
		return 1
	}
	return 0
}

// Equal returns Null if other is Null, BoolValue per boolean equality otherwise.
func (v BoolValue) Equal(other Value) Value {
	if IsNull(other) {
		return Null
	}
	o, ok := other.(BoolValue)
	return BoolValue(ok && v == o)
}

// ─────────────────────────────────────────────────────────────────────────────
// ListValue
// ─────────────────────────────────────────────────────────────────────────────

// ListValue is an ordered list of Cypher values.
type ListValue []Value

// Kind implements [Value].
func (v ListValue) Kind() Kind { return KindList }

// String returns the Cypher list literal representation.
func (v ListValue) String() string {
	if len(v) == 0 {
		return "[]"
	}
	b := make([]byte, 0, 2+len(v)*8)
	b = append(b, '[')
	for i, elem := range v {
		if i > 0 {
			b = append(b, ',', ' ')
		}
		b = append(b, elem.String()...)
	}
	b = append(b, ']')
	return string(b)
}

// Hash combines element hashes using a polynomial rolling hash.
func (v ListValue) Hash() uint64 {
	h := uint64(14695981039346656037)
	for _, elem := range v {
		h = h*1099511628211 ^ elem.Hash()
	}
	return h
}

// Equal returns Null if either operand is Null or contains Null. Returns
// BoolValue(false) if lengths differ. Otherwise returns Null if any element
// comparison yields Null, else BoolValue(all elements equal).
func (v ListValue) Equal(other Value) Value {
	if IsNull(other) {
		return Null
	}
	o, ok := other.(ListValue)
	if !ok || len(v) != len(o) {
		return BoolValue(false)
	}
	for i := range v {
		r := v[i].Equal(o[i])
		if IsNull(r) {
			return Null
		}
		if !IsTruthy(r) {
			return BoolValue(false)
		}
	}
	return BoolValue(true)
}

// ─────────────────────────────────────────────────────────────────────────────
// MapValue
// ─────────────────────────────────────────────────────────────────────────────

// MapValue is a property map from string keys to Cypher values.
type MapValue map[string]Value

// Kind implements [Value].
func (v MapValue) Kind() Kind { return KindMap }

// String returns the Cypher map literal representation.
func (v MapValue) String() string {
	if len(v) == 0 {
		return "{}"
	}
	// Deterministic output: iterate keys in insertion order is not guaranteed,
	// but for diagnostics the output is acceptable.
	b := make([]byte, 0, 2+len(v)*16)
	b = append(b, '{')
	first := true
	for k, val := range v {
		if !first {
			b = append(b, ',', ' ')
		}
		first = false
		b = append(b, k...)
		b = append(b, ':', ' ')
		b = append(b, val.String()...)
	}
	b = append(b, '}')
	return string(b)
}

// Hash combines key and value hashes using XOR so the result is
// order-independent (map semantics).
func (v MapValue) Hash() uint64 {
	var h uint64
	for k, val := range v {
		kh := StringValue(k).Hash()
		h ^= kh*1099511628211 ^ val.Hash()
	}
	return h
}

// Equal returns Null if either operand is Null. Returns BoolValue(false) if
// key sets differ. Otherwise returns Null if any value comparison yields Null,
// else BoolValue(all values equal).
func (v MapValue) Equal(other Value) Value {
	if IsNull(other) {
		return Null
	}
	o, ok := other.(MapValue)
	if !ok || len(v) != len(o) {
		return BoolValue(false)
	}
	for k, val := range v {
		oval, exists := o[k]
		if !exists {
			return BoolValue(false)
		}
		r := val.Equal(oval)
		if IsNull(r) {
			return Null
		}
		if !IsTruthy(r) {
			return BoolValue(false)
		}
	}
	return BoolValue(true)
}

// ─────────────────────────────────────────────────────────────────────────────
// NodeValue
// ─────────────────────────────────────────────────────────────────────────────

// NodeValue represents a graph node at runtime. ID is the storage-layer node
// identifier. Labels and Properties are the node's metadata as returned by the
// persistence layer.
type NodeValue struct {
	ID         uint64
	Labels     []string
	Properties MapValue
}

// Kind implements [Value].
func (v NodeValue) Kind() Kind { return KindNode }

// Hash implements [Value]; identity is determined by ID.
func (v NodeValue) Hash() uint64 { return v.ID ^ (v.ID >> 32) }

// String returns a short diagnostic representation of the node.
func (v NodeValue) String() string {
	return fmt.Sprintf("(node#%d)", v.ID)
}

// Equal returns Null if other is Null, BoolValue(true) iff both NodeValues
// have the same ID (node identity per openCypher semantics).
func (v NodeValue) Equal(other Value) Value {
	if IsNull(other) {
		return Null
	}
	o, ok := other.(NodeValue)
	return BoolValue(ok && v.ID == o.ID)
}

// ─────────────────────────────────────────────────────────────────────────────
// RelationshipValue
// ─────────────────────────────────────────────────────────────────────────────

// RelationshipValue represents a graph relationship at runtime. ID is the
// storage-layer edge identifier. StartID and EndID are the endpoint node IDs.
type RelationshipValue struct {
	ID         uint64
	StartID    uint64
	EndID      uint64
	Type       string
	Properties MapValue
}

// Kind implements [Value].
func (v RelationshipValue) Kind() Kind { return KindRelationship }

// Hash implements [Value]; identity is determined by ID.
func (v RelationshipValue) Hash() uint64 { return v.ID ^ (v.ID >> 32) }

// String returns a short diagnostic representation of the relationship.
func (v RelationshipValue) String() string {
	return fmt.Sprintf("-[rel#%d:%s]->", v.ID, v.Type)
}

// Equal returns Null if other is Null, BoolValue(true) iff both
// RelationshipValues have the same ID (relationship identity per openCypher).
func (v RelationshipValue) Equal(other Value) Value {
	if IsNull(other) {
		return Null
	}
	o, ok := other.(RelationshipValue)
	return BoolValue(ok && v.ID == o.ID)
}

// ─────────────────────────────────────────────────────────────────────────────
// PathValue
// ─────────────────────────────────────────────────────────────────────────────

// PathValue represents an alternating sequence of nodes and relationships.
// Nodes has length len(Relationships)+1. An empty path contains exactly one
// node and no relationships.
type PathValue struct {
	Nodes         []NodeValue
	Relationships []RelationshipValue
}

// Kind implements [Value].
func (v PathValue) Kind() Kind { return KindPath }

// String returns a short diagnostic representation of the path.
func (v PathValue) String() string {
	if len(v.Relationships) == 0 {
		if len(v.Nodes) == 0 {
			return "<empty-path>"
		}
		return v.Nodes[0].String()
	}
	b := make([]byte, 0, (len(v.Nodes)+len(v.Relationships))*16)
	b = append(b, v.Nodes[0].String()...)
	for i, rel := range v.Relationships {
		b = append(b, rel.String()...)
		b = append(b, v.Nodes[i+1].String()...)
	}
	return string(b)
}

// Hash combines the hashes of the constituent nodes and relationships.
func (v PathValue) Hash() uint64 {
	h := uint64(14695981039346656037)
	for _, n := range v.Nodes {
		h = h*1099511628211 ^ n.Hash()
	}
	for _, r := range v.Relationships {
		h = h*1099511628211 ^ r.Hash()
	}
	return h
}

// Equal returns Null if other is Null. Two paths are equal iff they have the
// same sequence of node and relationship identities.
func (v PathValue) Equal(other Value) Value {
	if IsNull(other) {
		return Null
	}
	o, ok := other.(PathValue)
	if !ok || len(v.Nodes) != len(o.Nodes) || len(v.Relationships) != len(o.Relationships) {
		return BoolValue(false)
	}
	for i := range v.Nodes {
		r := v.Nodes[i].Equal(o.Nodes[i])
		if IsNull(r) {
			return Null
		}
		if !IsTruthy(r) {
			return BoolValue(false)
		}
	}
	for i := range v.Relationships {
		r := v.Relationships[i].Equal(o.Relationships[i])
		if IsNull(r) {
			return Null
		}
		if !IsTruthy(r) {
			return BoolValue(false)
		}
	}
	return BoolValue(true)
}

// ─────────────────────────────────────────────────────────────────────────────
// Compare — openCypher 9 total ordering
// ─────────────────────────────────────────────────────────────────────────────

// kindOrder returns the cross-type sort weight for ORDER BY. Lower weight
// sorts first. NULLs are last (highest weight) per openCypher 9.
//
// Within a type the canonical order is:
//
//	Path(0) < Node(1) < Relationship(2) < Map(3) < List(4) <
//	String(5) < Boolean(6) < Float(7) < Integer(8) <
//	Duration(20) < Date(21) < LocalTime(22) < Time(23) <
//	LocalDateTime(24) < DateTime(25) < Null(99)
//
// Temporal kinds occupy a range above the numeric kinds to keep the existing
// cross-type ordering stable; relative order among temporal kinds follows the
// openCypher 9 §3.4 total order.
//
//nolint:gocyclo // Flat lookup over every known kind; splitting hides the order table.
func kindOrder(k Kind) int {
	switch k {
	case KindPath:
		return 0
	case KindNode:
		return 1
	case KindRelationship:
		return 2
	case KindMap:
		return 3
	case KindList:
		return 4
	case KindString:
		return 5
	case KindBool:
		return 6
	case KindFloat:
		return 7
	case KindInteger:
		return 8
	case KindDuration:
		return 20
	case KindDate:
		return 21
	case KindLocalTime:
		return 22
	case KindTime:
		return 23
	case KindLocalDateTime:
		return 24
	case KindDateTime:
		return 25
	case KindNull:
		return 99
	default:
		return 100
	}
}

// Compare returns -1, 0, or +1 following openCypher 9 ORDER BY semantics.
// NULLs sort after all non-null values. Values of different non-null types
// are ordered by kindOrder. Values of the same type use natural ordering.
//
// Note: this is a total order for sorting purposes only; it does not
// replace [Value.Equal] for predicate evaluation.
func Compare(a, b Value) int {
	ka, kb := a.Kind(), b.Kind()

	// NULL handling: sort last.
	if ka == KindNull && kb == KindNull {
		return 0
	}
	if ka == KindNull {
		return 1
	}
	if kb == KindNull {
		return -1
	}

	// Different non-null types: order by kind weight.
	oa, ob := kindOrder(ka), kindOrder(kb)
	if oa != ob {
		if oa < ob {
			return -1
		}
		return 1
	}

	// Same type: delegate to the per-kind helper.
	return compareSameKind(ka, a, b)
}

// cmpInt64 returns -1, 0, or +1 for two int64 values.
func cmpInt64(a, b int64) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

// cmpFloat64 returns -1, 0, or +1 for two float64 values.
func cmpFloat64(a, b float64) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

// cmpUint64 returns -1, 0, or +1 for two uint64 values.
func cmpUint64(a, b uint64) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

// compareSameKind compares two non-null values that are already known to share
// the same Kind. Extracted from [Compare] to keep cyclomatic complexity within
// the project's gocyclo limit (≤15).
//
//nolint:gocyclo // One branch per kind; the table is the entire point of the function — extraction would obscure the per-kind comparator wiring.
func compareSameKind(k Kind, a, b Value) int {
	switch k {
	case KindInteger:
		return cmpInt64(int64(a.(IntegerValue)), int64(b.(IntegerValue))) //nolint:forcetypeassert // kind pre-checked
	case KindFloat:
		return cmpFloat64(float64(a.(FloatValue)), float64(b.(FloatValue))) //nolint:forcetypeassert // kind pre-checked
	case KindString:
		as, bs := string(a.(StringValue)), string(b.(StringValue)) //nolint:forcetypeassert // kind pre-checked
		if as < bs {
			return -1
		}
		if as > bs {
			return 1
		}
		return 0
	case KindBool:
		return compareBool(bool(a.(BoolValue)), bool(b.(BoolValue))) //nolint:forcetypeassert // kind pre-checked
	case KindList:
		return compareList(a.(ListValue), b.(ListValue)) //nolint:forcetypeassert // kind pre-checked
	case KindNode:
		return cmpUint64(a.(NodeValue).ID, b.(NodeValue).ID) //nolint:forcetypeassert // kind pre-checked
	case KindRelationship:
		return cmpUint64(a.(RelationshipValue).ID, b.(RelationshipValue).ID) //nolint:forcetypeassert // kind pre-checked
	case KindPath:
		return comparePath(a.(PathValue), b.(PathValue)) //nolint:forcetypeassert // kind pre-checked
	case KindMap:
		// Maps have no total natural order; use hash as a stable tiebreaker.
		return cmpUint64(a.Hash(), b.Hash())
	case KindDate:
		da, db := a.(DateValue), b.(DateValue) //nolint:forcetypeassert // kind pre-checked
		if c := cmpInt64(int64(da.Year), int64(db.Year)); c != 0 {
			return c
		}
		if c := cmpInt64(int64(da.Month), int64(db.Month)); c != 0 {
			return c
		}
		return cmpInt64(int64(da.Day), int64(db.Day))
	case KindLocalDateTime:
		la, lb := a.(LocalDateTimeValue), b.(LocalDateTimeValue) //nolint:forcetypeassert // kind pre-checked
		return cmpInt64(la.T.UnixNano(), lb.T.UnixNano())
	case KindDateTime:
		da, db := a.(DateTimeValue), b.(DateTimeValue) //nolint:forcetypeassert // kind pre-checked
		return cmpInt64(da.T.UnixNano(), db.T.UnixNano())
	case KindLocalTime:
		la, lb := a.(LocalTimeValue), b.(LocalTimeValue) //nolint:forcetypeassert // kind pre-checked
		return cmpInt64(la.Nanos, lb.Nanos)
	case KindTime:
		ta, tb := a.(TimeValue), b.(TimeValue) //nolint:forcetypeassert // kind pre-checked
		if c := cmpInt64(ta.Nanos, tb.Nanos); c != 0 {
			return c
		}
		return cmpInt64(int64(ta.OffsetSec), int64(tb.OffsetSec))
	case KindDuration:
		// Durations have no natural total ordering; use component-wise lex.
		da, db := a.(DurationValue), b.(DurationValue) //nolint:forcetypeassert // kind pre-checked
		if c := cmpInt64(da.Months, db.Months); c != 0 {
			return c
		}
		if c := cmpInt64(da.Days, db.Days); c != 0 {
			return c
		}
		if c := cmpInt64(da.Seconds, db.Seconds); c != 0 {
			return c
		}
		return cmpInt64(int64(da.Nanos), int64(db.Nanos))
	}
	return 0
}

// compareBool compares two booleans: false < true.
func compareBool(a, b bool) int {
	if a == b {
		return 0
	}
	if !a {
		return -1
	}
	return 1
}

// compareList compares two ListValues lexicographically.
func compareList(al, bl ListValue) int {
	minLen := len(al)
	if len(bl) < minLen {
		minLen = len(bl)
	}
	for i := range minLen {
		if c := Compare(al[i], bl[i]); c != 0 {
			return c
		}
	}
	if len(al) < len(bl) {
		return -1
	}
	if len(al) > len(bl) {
		return 1
	}
	return 0
}

// comparePath compares two PathValues by node count, then element-wise by ID.
func comparePath(ap, bp PathValue) int {
	if len(ap.Nodes) != len(bp.Nodes) {
		if len(ap.Nodes) < len(bp.Nodes) {
			return -1
		}
		return 1
	}
	for i := range ap.Nodes {
		if c := Compare(ap.Nodes[i], bp.Nodes[i]); c != 0 {
			return c
		}
	}
	for i := range ap.Relationships {
		if c := Compare(ap.Relationships[i], bp.Relationships[i]); c != 0 {
			return c
		}
	}
	return 0
}
