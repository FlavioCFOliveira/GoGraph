package packstream

import (
	"errors"
	"fmt"
)

// maxValueDepth bounds how deeply ReadValue will recurse into nested
// composite values (List/Map/Structure). PackStream messages are decoded one
// Go stack frame per nesting level, and a single wire byte (e.g. 0x91, a
// TinyList of one element) is enough to open a new level. Without a bound, a
// crafted message can request millions of levels — far beyond Go's goroutine
// stack limit — and trigger a fatal, unrecoverable stack overflow that crashes
// the whole process. This is reachable pre-authentication during the first
// HELLO decode, so the bound is a hard security boundary, not a convenience.
//
// Well-formed Bolt values nest only a handful of levels (a Path holding a list
// of node/relationship structures, a map of lists, and so on), so 128 is far
// above any legitimate need while staying comfortably within the stack budget.
// The value matches typical Neo4j/driver practice.
const maxValueDepth = 128

// ErrNestingTooDeep is returned by ReadValue when a composite value
// (List/Map/Structure) nests deeper than maxValueDepth. It guards against
// stack-overflow denial-of-service from maliciously crafted messages; see
// maxValueDepth for the rationale.
var ErrNestingTooDeep = errors.New("packstream: value nesting too deep")

// Value is a sum type over all PackStream value kinds:
//
//	nil          → NULL
//	bool         → Boolean
//	int64        → Integer
//	float64      → Float
//	[]byte       → Bytes
//	string       → String
//	[]Value      → List
//	map[string]Value → Map
//	Struct       → Structure
type Value = any

// Struct represents a PackStream Structure with its tag byte and ordered fields.
type Struct struct {
	// Tag is the single-byte structure signature.
	Tag byte
	// Fields contains the structure's fields in declaration order.
	Fields []Value
}

// WriteValue encodes v into the stream using the Encoder.
// It dispatches on the concrete type of v.
//
//nolint:gocyclo // switch over PackStream's nine value kinds; complexity is irreducible.
func (e *Encoder) WriteValue(v Value) error {
	switch x := v.(type) {
	case nil:
		return e.WriteNull()
	case bool:
		return e.WriteBool(x)
	case int64:
		return e.WriteInt(x)
	case int:
		return e.WriteInt(int64(x))
	case int32:
		return e.WriteInt(int64(x))
	case float64:
		return e.WriteFloat(x)
	case []byte:
		return e.WriteBytes(x)
	case string:
		return e.WriteString(x)
	case []Value:
		if err := e.WriteListHeader(len(x)); err != nil {
			return err
		}
		for _, item := range x {
			if err := e.WriteValue(item); err != nil {
				return err
			}
		}
		return nil
	case map[string]Value:
		if err := e.WriteMapHeader(len(x)); err != nil {
			return err
		}
		for k, val := range x {
			if err := e.WriteString(k); err != nil {
				return err
			}
			if err := e.WriteValue(val); err != nil {
				return err
			}
		}
		return nil
	case Struct:
		if err := e.WriteStructHeader(x.Tag, len(x.Fields)); err != nil {
			return err
		}
		for _, f := range x.Fields {
			if err := e.WriteValue(f); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("packstream: unsupported value type %T", v)
	}
}

// ReadValue reads a single PackStream value from the stream, returning it as a
// Go value. The concrete types returned are:
//
//	nil          → NULL
//	bool         → Boolean
//	int64        → Integer
//	float64      → Float
//	[]byte       → Bytes
//	string       → String
//	[]Value      → List
//	map[string]Value → Map
//	Struct       → Structure
func (d *Decoder) ReadValue() (Value, error) {
	return d.readValue(0)
}

// readValue is the recursive worker behind ReadValue. depth is the current
// nesting level; each composite arm recurses with depth+1 and the bound is
// enforced at entry, so every recursive entry point (List elements, Map
// values, Struct fields) is covered by a single check. Exceeding maxValueDepth
// returns ErrNestingTooDeep instead of recursing, preventing a fatal stack
// overflow on adversarial input.
//
//nolint:gocyclo // switch over PackStream's nine value kinds; complexity is irreducible.
func (d *Decoder) readValue(depth int) (Value, error) {
	if depth > maxValueDepth {
		return nil, ErrNestingTooDeep
	}
	t, err := d.PeekType()
	if err != nil {
		return nil, err
	}
	switch t {
	case TypeNull:
		return nil, d.ReadNull()
	case TypeBool:
		return d.ReadBool()
	case TypeInt:
		return d.ReadInt()
	case TypeFloat:
		return d.ReadFloat()
	case TypeBytes:
		return d.ReadBytes()
	case TypeString:
		return d.ReadString()
	case TypeList:
		n, err := d.ReadListHeader()
		if err != nil {
			return nil, err
		}
		// Each list element occupies at least one wire byte, so a count
		// exceeding the bytes still available is impossible for a well-formed
		// message; reject it before make([]Value, n) commits ~16 bytes per
		// slot. See ErrLengthExceedsInput.
		if n > d.budget() {
			return nil, fmt.Errorf("%w: List count %d > %d", ErrLengthExceedsInput, n, d.budget())
		}
		items := make([]Value, n)
		for i := range items {
			items[i], err = d.readValue(depth + 1)
			if err != nil {
				return nil, err
			}
		}
		return items, nil
	case TypeMap:
		n, err := d.ReadMapHeader()
		if err != nil {
			return nil, err
		}
		// Each map entry is a key plus a value, so it occupies at least two
		// wire bytes; a count exceeding the bytes still available is
		// impossible for a well-formed message. Reject before make() commits
		// the map's backing store. See ErrLengthExceedsInput.
		if n > d.budget() {
			return nil, fmt.Errorf("%w: Map count %d > %d", ErrLengthExceedsInput, n, d.budget())
		}
		m := make(map[string]Value, n)
		for range n {
			k, err := d.ReadString()
			if err != nil {
				return nil, err
			}
			val, err := d.readValue(depth + 1)
			if err != nil {
				return nil, err
			}
			m[k] = val
		}
		return m, nil
	case TypeStruct:
		tag, n, err := d.ReadStructHeader()
		if err != nil {
			return nil, err
		}
		// ReadStructHeader caps n at 15 (TinyStruct only), so this guard is
		// defence-in-depth: each field is at least one wire byte, so a count
		// exceeding the bytes still available is rejected before make(). See
		// ErrLengthExceedsInput.
		if n > d.budget() {
			return nil, fmt.Errorf("%w: Struct field count %d > %d", ErrLengthExceedsInput, n, d.budget())
		}
		fields := make([]Value, n)
		for i := range fields {
			fields[i], err = d.readValue(depth + 1)
			if err != nil {
				return nil, err
			}
		}
		return Struct{Tag: tag, Fields: fields}, nil
	default:
		return nil, fmt.Errorf("packstream: unknown type %v", t)
	}
}
