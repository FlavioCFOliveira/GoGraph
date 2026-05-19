package txn

import (
	"encoding"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// WeightCodec encodes and decodes edge-weight values of type W onto the
// transactional op log. It is the W-side dual of [Codec], used by the
// v2 [OpAddEdgeWeighted] frame layout to persist a typed weight between
// the codec-encoded endpoints and the trailing label.
//
// The contract mirrors [Codec]: Encode appends the wire form of w to
// buf and returns the extended slice; Decode reads a value from the
// head of buf, returning the decoded value, the unread tail, and any
// error. The append-style API keeps the Commit fast path zero-alloc
// when callers reuse a scratch buffer across ops.
//
// Concurrency: a WeightCodec value is expected to be cheap to copy and
// safe for concurrent use; the built-in codecs in this package are
// stateless and therefore inherently safe.
type WeightCodec[W any] interface {
	// Encode appends the wire form of w to buf and returns the
	// extended slice. The returned slice may alias buf.
	Encode(buf []byte, w W) []byte
	// Decode reads a value from the head of buf, returning the
	// decoded value, the remaining unread tail, and any error. On
	// error, value and tail are unspecified.
	Decode(buf []byte) (value W, rest []byte, err error)
}

// ErrNoWeightCodec is returned by [Tx.AddEdge] when the caller passes
// a non-zero weight to a store that was constructed without a typed
// [WeightCodec] (i.e. via [NewStore] or [NewStoreWithCodec]). Zero-
// weight calls remain accepted on those constructors and buffer an
// [OpAddEdge] (unweighted) record.
var ErrNoWeightCodec = errors.New("txn: store has no WeightCodec; cannot persist non-zero edge weight")

// ---- int64 weight codec ------------------------------------------------

type int64WeightCodec struct{}

// NewInt64WeightCodec returns the canonical [WeightCodec][int64]. The
// wire form is a signed varint, matching the [Codec][int64] used for
// endpoint encoding so that the on-disk shape is symmetrical for the
// common signed-integer instantiation.
func NewInt64WeightCodec() WeightCodec[int64] { return int64WeightCodec{} }

func (int64WeightCodec) Encode(buf []byte, w int64) []byte {
	return binary.AppendVarint(buf, w)
}

func (int64WeightCodec) Decode(buf []byte) (value int64, rest []byte, err error) {
	x, n := binary.Varint(buf)
	if n <= 0 {
		return 0, buf, fmt.Errorf("%w: int64 weight varint", ErrCodecDecode)
	}
	return x, buf[n:], nil
}

// ---- float64 weight codec ----------------------------------------------

type float64WeightCodec struct{}

// NewFloat64WeightCodec returns the canonical [WeightCodec][float64].
// The wire form is a fixed 8-byte little-endian IEEE 754 layout
// produced by [math.Float64bits] and reconstructed by
// [math.Float64frombits]. The codec round-trips bits losslessly,
// including +0.0 vs -0.0, ±Inf and every NaN payload — be aware that
// NaN comparison rules apply on read (NaN != NaN), so callers that
// store NaN weights must compare via bit pattern (or [math.IsNaN])
// rather than the equality operator.
func NewFloat64WeightCodec() WeightCodec[float64] { return float64WeightCodec{} }

func (float64WeightCodec) Encode(buf []byte, w float64) []byte {
	return binary.LittleEndian.AppendUint64(buf, math.Float64bits(w))
}

func (float64WeightCodec) Decode(buf []byte) (value float64, rest []byte, err error) {
	if len(buf) < 8 {
		return 0, buf, fmt.Errorf("%w: float64 weight body (want 8, have %d)", ErrCodecDecode, len(buf))
	}
	bits := binary.LittleEndian.Uint64(buf)
	return math.Float64frombits(bits), buf[8:], nil
}

// ---- BinaryMarshaler weight codec (arbitrary user weight types) --------

// NewBinaryMarshalerWeightCodec returns a [WeightCodec][W] that
// delegates encoding and decoding to the [encoding.BinaryMarshaler]
// and [encoding.BinaryUnmarshaler] methods on *W. The wire form is a
// uint32 little-endian length prefix followed by the marshaler's
// opaque payload.
//
// The type parameter P pins *W as the receiver for the marshaler
// methods, mirroring [NewBinaryMarshalerCodec] for endpoints. A
// typical instantiation:
//
//	codec := txn.NewBinaryMarshalerWeightCodec[MyWeight, *MyWeight]()
//
// where MyWeight is a value type with MarshalBinary / UnmarshalBinary
// methods declared on *MyWeight.
func NewBinaryMarshalerWeightCodec[W any, P interface {
	*W
	encoding.BinaryMarshaler
	encoding.BinaryUnmarshaler
}]() WeightCodec[W] {
	return binaryMarshalerWeightCodec[W, P]{}
}

type binaryMarshalerWeightCodec[W any, P interface {
	*W
	encoding.BinaryMarshaler
	encoding.BinaryUnmarshaler
}] struct{}

func (binaryMarshalerWeightCodec[W, P]) Encode(buf []byte, w W) []byte {
	// MarshalBinary is invoked on a pointer to a local copy of w so
	// callers see no observable mutation of their value.
	tmp := w
	data, err := P(&tmp).MarshalBinary()
	if err != nil {
		// MarshalBinary on a fresh local cannot meaningfully fail in
		// practice; if a user type does return an error, surface it
		// via a panic — the WAL append path has no error channel for
		// this case and silent corruption is worse than a crash. The
		// public API documents this contract in the codec godoc.
		panic(fmt.Errorf("txn/weight_codec: BinaryMarshaler returned error: %w", err))
	}
	if uint64(len(data)) > math.MaxUint32 {
		panic(fmt.Errorf("txn/weight_codec: BinaryMarshaler payload exceeds uint32 (%d bytes)", len(data)))
	}
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(data)))
	return append(buf, data...)
}

func (binaryMarshalerWeightCodec[W, P]) Decode(buf []byte) (value W, rest []byte, err error) {
	if len(buf) < 4 {
		return value, buf, fmt.Errorf("%w: BinaryMarshaler weight length prefix", ErrCodecDecode)
	}
	n := binary.LittleEndian.Uint32(buf)
	rest = buf[4:]
	if uint64(len(rest)) < uint64(n) {
		return value, buf, fmt.Errorf("%w: BinaryMarshaler weight body (want %d, have %d)", ErrCodecDecode, n, len(rest))
	}
	if uerr := P(&value).UnmarshalBinary(rest[:n]); uerr != nil {
		var zero W
		return zero, buf, fmt.Errorf("%w: BinaryUnmarshaler weight: %v", ErrCodecDecode, uerr) //nolint:errorlint // wrap with sentinel + verb message
	}
	return value, rest[n:], nil
}
