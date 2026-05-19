package txn

import (
	"encoding"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Codec encodes and decodes node-identifier values of type N onto the
// transactional op log. Implementations append the encoded form to the
// caller-supplied buffer and reverse the process from the head of a
// buffer, returning the decoded value plus the unread tail. The
// append-style API keeps the common path zero-alloc when callers reuse
// a scratch buffer across ops.
//
// Concurrency: a Codec value is expected to be cheap to copy and safe
// for concurrent use; the built-in codecs in this package are
// stateless and therefore inherently safe.
type Codec[N comparable] interface {
	// Encode appends the wire form of v to buf and returns the
	// extended slice. The returned slice may alias buf.
	Encode(buf []byte, v N) []byte
	// Decode reads a value from the head of buf, returning the
	// decoded value, the remaining unread tail, and any error. On
	// error, value and tail are unspecified.
	Decode(buf []byte) (value N, rest []byte, err error)
}

// ErrCodecDecode is the sentinel returned by built-in codecs whenever
// the input buffer is too short, malformed, or violates a length
// invariant (e.g. negative varint, oversize length prefix).
var ErrCodecDecode = errors.New("txn/codec: malformed payload")

// ---- string codec ------------------------------------------------------

type stringCodec struct{}

// NewStringCodec returns the canonical Codec[string] used for typed
// op records keyed by string node identifiers. The wire form is a
// uint32 little-endian length prefix followed by the utf-8 bytes.
func NewStringCodec() Codec[string] { return stringCodec{} }

func (stringCodec) Encode(buf []byte, v string) []byte {
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(v)))
	return append(buf, v...)
}

func (stringCodec) Decode(buf []byte) (value string, rest []byte, err error) {
	if len(buf) < 4 {
		return "", buf, fmt.Errorf("%w: string length prefix", ErrCodecDecode)
	}
	n := binary.LittleEndian.Uint32(buf)
	rest = buf[4:]
	if uint64(len(rest)) < uint64(n) {
		return "", buf, fmt.Errorf("%w: string body (want %d, have %d)", ErrCodecDecode, n, len(rest))
	}
	// Copy the payload so the returned value does not alias buf.
	value = string(rest[:n])
	return value, rest[n:], nil
}

// ---- integer codecs (signed via varint, unsigned via uvarint) ----------

type intCodec struct{}

// NewIntCodec returns the canonical Codec[int]. The wire form is a
// signed varint (encoding/binary's PutVarint).
func NewIntCodec() Codec[int] { return intCodec{} }

func (intCodec) Encode(buf []byte, v int) []byte {
	return binary.AppendVarint(buf, int64(v))
}

func (intCodec) Decode(buf []byte) (value int, rest []byte, err error) {
	x, n := binary.Varint(buf)
	if n <= 0 {
		return 0, buf, fmt.Errorf("%w: int varint", ErrCodecDecode)
	}
	if x > math.MaxInt || x < math.MinInt {
		return 0, buf, fmt.Errorf("%w: int out of range", ErrCodecDecode)
	}
	return int(x), buf[n:], nil
}

type int32Codec struct{}

// NewInt32Codec returns the canonical Codec[int32]. The wire form is a
// signed varint.
func NewInt32Codec() Codec[int32] { return int32Codec{} }

func (int32Codec) Encode(buf []byte, v int32) []byte {
	return binary.AppendVarint(buf, int64(v))
}

func (int32Codec) Decode(buf []byte) (value int32, rest []byte, err error) {
	x, n := binary.Varint(buf)
	if n <= 0 {
		return 0, buf, fmt.Errorf("%w: int32 varint", ErrCodecDecode)
	}
	if x > math.MaxInt32 || x < math.MinInt32 {
		return 0, buf, fmt.Errorf("%w: int32 out of range", ErrCodecDecode)
	}
	return int32(x), buf[n:], nil
}

type int64Codec struct{}

// NewInt64Codec returns the canonical Codec[int64]. The wire form is a
// signed varint.
func NewInt64Codec() Codec[int64] { return int64Codec{} }

func (int64Codec) Encode(buf []byte, v int64) []byte {
	return binary.AppendVarint(buf, v)
}

func (int64Codec) Decode(buf []byte) (value int64, rest []byte, err error) {
	x, n := binary.Varint(buf)
	if n <= 0 {
		return 0, buf, fmt.Errorf("%w: int64 varint", ErrCodecDecode)
	}
	return x, buf[n:], nil
}

type uint64Codec struct{}

// NewUint64Codec returns the canonical Codec[uint64]. The wire form is
// an unsigned varint.
func NewUint64Codec() Codec[uint64] { return uint64Codec{} }

func (uint64Codec) Encode(buf []byte, v uint64) []byte {
	return binary.AppendUvarint(buf, v)
}

func (uint64Codec) Decode(buf []byte) (value uint64, rest []byte, err error) {
	x, n := binary.Uvarint(buf)
	if n <= 0 {
		return 0, buf, fmt.Errorf("%w: uint64 uvarint", ErrCodecDecode)
	}
	return x, buf[n:], nil
}

// ---- UUID codec ([16]byte fixed-width) ---------------------------------

type uuidCodec struct{}

// NewUUIDCodec returns the canonical Codec[[16]byte]. The wire form is
// a fixed 16-byte big-endian copy of the value — no length prefix
// because the size is constant.
func NewUUIDCodec() Codec[[16]byte] { return uuidCodec{} }

func (uuidCodec) Encode(buf []byte, v [16]byte) []byte {
	return append(buf, v[:]...)
}

func (uuidCodec) Decode(buf []byte) (value [16]byte, rest []byte, err error) {
	if len(buf) < 16 {
		return value, buf, fmt.Errorf("%w: uuid body (want 16, have %d)", ErrCodecDecode, len(buf))
	}
	copy(value[:], buf[:16])
	return value, buf[16:], nil
}

// ---- BinaryMarshaler codec (arbitrary user types) ----------------------

// NewBinaryMarshalerCodec returns a Codec[N] that delegates encoding
// and decoding to the [encoding.BinaryMarshaler] and
// [encoding.BinaryUnmarshaler] methods on *N. The wire form is a
// uint32 little-endian length prefix followed by the marshaler's
// opaque payload.
//
// The type parameter P pins *N as the receiver for the marshaler
// methods, which is the standard Go pattern for "value type plus
// pointer-receiver methods". A typical instantiation:
//
//	codec := txn.NewBinaryMarshalerCodec[MyKey, *MyKey]()
//
// where MyKey is a value type with MarshalBinary / UnmarshalBinary
// methods declared on *MyKey.
func NewBinaryMarshalerCodec[N comparable, P interface {
	*N
	encoding.BinaryMarshaler
	encoding.BinaryUnmarshaler
}]() Codec[N] {
	return binaryMarshalerCodec[N, P]{}
}

type binaryMarshalerCodec[N comparable, P interface {
	*N
	encoding.BinaryMarshaler
	encoding.BinaryUnmarshaler
}] struct{}

func (binaryMarshalerCodec[N, P]) Encode(buf []byte, v N) []byte {
	// MarshalBinary is invoked on a pointer to a local copy of v so
	// callers see no observable mutation of their value.
	tmp := v
	data, err := P(&tmp).MarshalBinary()
	if err != nil {
		// MarshalBinary on a fresh local cannot meaningfully fail in
		// practice; if a user type does return an error, surface it
		// via a panic — the WAL append path has no error channel for
		// this case and silent corruption is worse than a crash. The
		// public API documents this contract in the codec godoc.
		panic(fmt.Errorf("txn/codec: BinaryMarshaler returned error: %w", err))
	}
	if uint64(len(data)) > math.MaxUint32 {
		panic(fmt.Errorf("txn/codec: BinaryMarshaler payload exceeds uint32 (%d bytes)", len(data)))
	}
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(data)))
	return append(buf, data...)
}

func (binaryMarshalerCodec[N, P]) Decode(buf []byte) (value N, rest []byte, err error) {
	if len(buf) < 4 {
		return value, buf, fmt.Errorf("%w: BinaryMarshaler length prefix", ErrCodecDecode)
	}
	n := binary.LittleEndian.Uint32(buf)
	rest = buf[4:]
	if uint64(len(rest)) < uint64(n) {
		return value, buf, fmt.Errorf("%w: BinaryMarshaler body (want %d, have %d)", ErrCodecDecode, n, len(rest))
	}
	if uerr := P(&value).UnmarshalBinary(rest[:n]); uerr != nil {
		var zero N
		return zero, buf, fmt.Errorf("%w: BinaryUnmarshaler: %v", ErrCodecDecode, uerr) //nolint:errorlint // wrap with sentinel + verb message
	}
	return value, rest[n:], nil
}

// ---- legacy fmt-based codec --------------------------------------------

// legacyFmtCodec is the fallback codec installed by [NewStore]. It is
// retained so existing callers continue to produce byte-identical v1
// WAL frames. The Encode path is unreachable for the legacy codec
// because [Store.encodeOp] short-circuits to the v1 layout when this
// codec is in use; the Decode path is unreachable because v1 frames
// are walked directly by the recovery layer.
type legacyFmtCodec[N comparable] struct{}

// isLegacy reports whether the codec is the v1 fmt.Sprintf fallback.
// The Store consults this method to decide whether to emit a v1
// untagged frame (legacy) or a v2 tagged frame (typed codec).
func (legacyFmtCodec[N]) isLegacy() bool { return true }

func (legacyFmtCodec[N]) Encode(buf []byte, v N) []byte {
	// Mirrors the historical fmt.Sprintf("%v") payload so that any
	// hand-crafted call site that wires the legacy codec into the
	// typed path still produces the original bytes.
	return append(buf, goFormat(v)...)
}

func (legacyFmtCodec[N]) Decode(buf []byte) (value N, rest []byte, err error) {
	// Legacy fmt.Sprintf is one-way: there is no general inverse for
	// "%v" output. Callers that need to load v1 frames must use the
	// string-keyed recovery path (which uses string(SrcBytes)) or
	// migrate the WAL to a typed codec first.
	return value, buf, fmt.Errorf("%w: legacy fmt codec cannot decode", ErrCodecDecode)
}

// legacyMarker is the interface used by [Store] to detect the legacy
// codec without depending on the concrete generic instantiation.
type legacyMarker interface{ isLegacy() bool }

func isLegacyCodec[N comparable](c Codec[N]) bool {
	if lm, ok := c.(legacyMarker); ok {
		return lm.isLegacy()
	}
	return false
}
