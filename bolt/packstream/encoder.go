// Package packstream implements the PackStream binary serialisation format
// used by the Bolt protocol.
//
// PackStream is a strongly typed, binary format built around a set of marker
// bytes that describe the type and size of the value that follows. The
// Encoder and Decoder types in this package provide low-level, zero-copy
// access to individual values; the higher-level WriteValue/ReadValue helpers
// operate on the Value sum type for convenience.
//
// Concurrency: Encoder and Decoder are NOT safe for concurrent use. Each
// goroutine must hold its own instance. Use EncodePool/DecodePool to amortise
// allocation costs.
package packstream

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

// Marker bytes as defined by the PackStream v2 / Bolt v5 specification.
const (
	markerNull    byte = 0xC0
	markerFalse   byte = 0xC2
	markerTrue    byte = 0xC3
	markerFloat64 byte = 0xC1
	markerInt8    byte = 0xC8
	markerInt16   byte = 0xC9
	markerInt32   byte = 0xCA
	markerInt64   byte = 0xCB
	markerBytes8  byte = 0xCC
	markerBytes16 byte = 0xCD
	markerBytes32 byte = 0xCE
	markerStr8    byte = 0xD0
	markerStr16   byte = 0xD1
	markerStr32   byte = 0xD2
	markerList8   byte = 0xD4
	markerList16  byte = 0xD5
	markerList32  byte = 0xD6
	markerMap8    byte = 0xD8
	markerMap16   byte = 0xD9
	markerMap32   byte = 0xDA
)

const (
	tinyIntLow  int64 = -16
	tinyIntHigh int64 = 127

	tinyStrBase    byte = 0x80
	tinyListBase   byte = 0x90
	tinyMapBase    byte = 0xA0
	tinyStructBase byte = 0xB0

	tinyStrMax    = 15
	tinyListMax   = 15
	tinyMapMax    = 15
	tinyStructMax = 15
)

// Encoder writes PackStream-encoded values to an underlying buffered writer.
// It is NOT safe for concurrent use.
type Encoder struct {
	w   *bufio.Writer
	buf [8]byte // scratch buffer for numeric encoding; avoids heap allocation
}

// NewEncoder returns a new Encoder that writes to w.
// The caller retains ownership of w.
func NewEncoder(w io.Writer) *Encoder {
	return &Encoder{w: bufio.NewWriter(w)}
}

// newEncoderFromBufio creates an Encoder wrapping an existing bufio.Writer.
// Used by pool helpers to reuse the underlying writer.
func newEncoderFromBufio(w *bufio.Writer) *Encoder {
	return &Encoder{w: w}
}

// Flush flushes the underlying bufio.Writer to the wire.
func (e *Encoder) Flush() error {
	return e.w.Flush()
}

// Reset points the encoder at a new underlying writer.
// Used by pool helpers to reuse Encoder objects.
func (e *Encoder) Reset(w io.Writer) {
	e.w.Reset(w)
}

// writeByte writes a single marker byte.
func (e *Encoder) writeByte(b byte) error {
	return e.w.WriteByte(b)
}

// ErrPayloadTooLarge is returned by WriteBytes/WriteString/WriteListHeader/
// WriteMapHeader when the supplied length exceeds the largest size the
// PackStream wire format can encode in its 32-bit length field. Bolt
// payloads in practice are bounded well below this limit by the server's
// frame-size limit; the explicit check guards against silent truncation
// when callers pass an oversize slice from upstream batching code.
var ErrPayloadTooLarge = fmt.Errorf("packstream: payload length exceeds 32-bit length prefix (max %d)", uint64(math.MaxUint32))

// checkUint32Length reports ErrPayloadTooLarge when n cannot be encoded
// in a 32-bit unsigned length prefix. n is expected to be non-negative
// (the calling site validates n >= 0 first via a dedicated case); a
// hypothetical negative n is also correctly rejected here because the
// uint64 reinterpretation lifts it well above MaxUint32.
func checkUint32Length(n int) error {
	// The int -> uint64 cast is deliberate bit-reinterpretation: it
	// preserves nonnegative values exactly and turns any negative
	// argument into a value > MaxUint32, both of which trip the guard
	// below. gosec G115 does not model this safety property.
	if uint64(n) > math.MaxUint32 { //nolint:gosec // G115: int->uint64 reinterpretation is the intended check; negative n correctly rejected
		return ErrPayloadTooLarge
	}
	return nil
}

// WriteNull encodes the PackStream NULL value.
func (e *Encoder) WriteNull() error {
	return e.writeByte(markerNull)
}

// WriteBool encodes a PackStream Boolean value.
func (e *Encoder) WriteBool(v bool) error {
	if v {
		return e.writeByte(markerTrue)
	}
	return e.writeByte(markerFalse)
}

// WriteInt encodes a PackStream Integer value using the most compact
// representation: TinyInt for values in [-16, 127], then INT8/16/32/64.
//
// The numeric reinterpretation casts below (byte(v), int8(v), int16(v),
// int32(v), uint64(v)) are bit-pattern-preserving two's-complement
// conversions mandated by the PackStream wire format; gosec G115 is a
// false positive here because the enclosing switch has already bounded
// v to the destination type's range before each cast.
func (e *Encoder) WriteInt(v int64) error {
	switch {
	case v >= tinyIntLow && v <= tinyIntHigh:
		// TinyInt: value fits in the low 7 bits (negative values use two's complement).
		return e.writeByte(byte(v)) //nolint:gosec // G115: range pre-validated by switch [-16,127]
	case v >= math.MinInt8 && v <= math.MaxInt8:
		if err := e.writeByte(markerInt8); err != nil {
			return err
		}
		return e.writeByte(byte(int8(v))) //nolint:gosec // G115: range pre-validated by switch [MinInt8,MaxInt8]
	case v >= math.MinInt16 && v <= math.MaxInt16:
		if err := e.writeByte(markerInt16); err != nil {
			return err
		}
		binary.BigEndian.PutUint16(e.buf[:2], uint16(int16(v))) //nolint:gosec // G115: range pre-validated by switch [MinInt16,MaxInt16]
		_, err := e.w.Write(e.buf[:2])
		return err
	case v >= math.MinInt32 && v <= math.MaxInt32:
		if err := e.writeByte(markerInt32); err != nil {
			return err
		}
		binary.BigEndian.PutUint32(e.buf[:4], uint32(int32(v))) //nolint:gosec // G115: range pre-validated by switch [MinInt32,MaxInt32]
		_, err := e.w.Write(e.buf[:4])
		return err
	default:
		if err := e.writeByte(markerInt64); err != nil {
			return err
		}
		// int64 -> uint64 is a lossless bit-pattern reinterpretation.
		binary.BigEndian.PutUint64(e.buf[:8], uint64(v)) //nolint:gosec // G115: int64 -> uint64 is lossless reinterpretation
		_, err := e.w.Write(e.buf[:8])
		return err
	}
}

// WriteFloat encodes a PackStream Float64 value (8-byte big-endian IEEE-754).
func (e *Encoder) WriteFloat(v float64) error {
	if err := e.writeByte(markerFloat64); err != nil {
		return err
	}
	binary.BigEndian.PutUint64(e.buf[:8], math.Float64bits(v))
	_, err := e.w.Write(e.buf[:8])
	return err
}

// WriteBytes encodes a PackStream Bytes value.
// It selects BYTES8, BYTES16, or BYTES32 based on the slice length.
// Returns [ErrPayloadTooLarge] when len(v) exceeds math.MaxUint32.
func (e *Encoder) WriteBytes(v []byte) error {
	n := len(v)
	switch {
	case n <= math.MaxUint8:
		if err := e.writeByte(markerBytes8); err != nil {
			return err
		}
		if err := e.writeByte(byte(n)); err != nil {
			return err
		}
	case n <= math.MaxUint16:
		if err := e.writeByte(markerBytes16); err != nil {
			return err
		}
		binary.BigEndian.PutUint16(e.buf[:2], uint16(n))
		if _, err := e.w.Write(e.buf[:2]); err != nil {
			return err
		}
	default:
		if err := checkUint32Length(n); err != nil {
			return err
		}
		if err := e.writeByte(markerBytes32); err != nil {
			return err
		}
		binary.BigEndian.PutUint32(e.buf[:4], uint32(n)) //nolint:gosec // G115: bounded by checkUint32Length above
		if _, err := e.w.Write(e.buf[:4]); err != nil {
			return err
		}
	}
	_, err := e.w.Write(v)
	return err
}

// WriteString encodes a PackStream String value.
// It selects TinyString (len 0..15), STRING8, STRING16, or STRING32.
// Returns [ErrPayloadTooLarge] when len(v) exceeds math.MaxUint32.
func (e *Encoder) WriteString(v string) error {
	n := len(v)
	switch {
	case n <= tinyStrMax:
		if err := e.writeByte(tinyStrBase | byte(n)); err != nil {
			return err
		}
	case n <= math.MaxUint8:
		if err := e.writeByte(markerStr8); err != nil {
			return err
		}
		if err := e.writeByte(byte(n)); err != nil {
			return err
		}
	case n <= math.MaxUint16:
		if err := e.writeByte(markerStr16); err != nil {
			return err
		}
		binary.BigEndian.PutUint16(e.buf[:2], uint16(n))
		if _, err := e.w.Write(e.buf[:2]); err != nil {
			return err
		}
	default:
		if err := checkUint32Length(n); err != nil {
			return err
		}
		if err := e.writeByte(markerStr32); err != nil {
			return err
		}
		binary.BigEndian.PutUint32(e.buf[:4], uint32(n)) //nolint:gosec // G115: bounded by checkUint32Length above
		if _, err := e.w.Write(e.buf[:4]); err != nil {
			return err
		}
	}
	_, err := e.w.WriteString(v)
	return err
}

// WriteListHeader writes the PackStream list marker for a list with n elements.
// The caller is responsible for encoding exactly n elements after this call.
// n must be non-negative and at most math.MaxUint32; otherwise the function
// returns [ErrPayloadTooLarge].
func (e *Encoder) WriteListHeader(n int) error {
	switch {
	case n < 0:
		return fmt.Errorf("packstream: negative list length %d", n)
	case n <= tinyListMax:
		return e.writeByte(tinyListBase | byte(n))
	case n <= math.MaxUint8:
		if err := e.writeByte(markerList8); err != nil {
			return err
		}
		return e.writeByte(byte(n))
	case n <= math.MaxUint16:
		if err := e.writeByte(markerList16); err != nil {
			return err
		}
		binary.BigEndian.PutUint16(e.buf[:2], uint16(n))
		_, err := e.w.Write(e.buf[:2])
		return err
	default:
		if err := checkUint32Length(n); err != nil {
			return err
		}
		if err := e.writeByte(markerList32); err != nil {
			return err
		}
		binary.BigEndian.PutUint32(e.buf[:4], uint32(n)) //nolint:gosec // G115: bounded by checkUint32Length above
		_, err := e.w.Write(e.buf[:4])
		return err
	}
}

// WriteMapHeader writes the PackStream map marker for a map with n key/value pairs.
// The caller is responsible for encoding exactly 2n items (alternating keys and values).
// Returns [ErrPayloadTooLarge] when n exceeds math.MaxUint32.
func (e *Encoder) WriteMapHeader(n int) error {
	switch {
	case n < 0:
		return fmt.Errorf("packstream: negative map length %d", n)
	case n <= tinyMapMax:
		return e.writeByte(tinyMapBase | byte(n))
	case n <= math.MaxUint8:
		if err := e.writeByte(markerMap8); err != nil {
			return err
		}
		return e.writeByte(byte(n))
	case n <= math.MaxUint16:
		if err := e.writeByte(markerMap16); err != nil {
			return err
		}
		binary.BigEndian.PutUint16(e.buf[:2], uint16(n))
		_, err := e.w.Write(e.buf[:2])
		return err
	default:
		if err := checkUint32Length(n); err != nil {
			return err
		}
		if err := e.writeByte(markerMap32); err != nil {
			return err
		}
		binary.BigEndian.PutUint32(e.buf[:4], uint32(n)) //nolint:gosec // G115: bounded by checkUint32Length above
		_, err := e.w.Write(e.buf[:4])
		return err
	}
}

// WriteStructHeader writes the PackStream struct marker byte (TinyStruct) and
// the tag byte. PackStream v2 supports only TinyStruct (0..15 fields).
// n must be in [0, 15].
func (e *Encoder) WriteStructHeader(tag byte, n int) error {
	if n < 0 || n > tinyStructMax {
		return fmt.Errorf("packstream: struct field count %d out of range [0,15]", n)
	}
	if err := e.writeByte(tinyStructBase | byte(n)); err != nil {
		return err
	}
	return e.writeByte(tag)
}
