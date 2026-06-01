package packstream

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
)

// ErrLengthExceedsInput is returned by the decoder when a length or count
// prefix declares more elements than can possibly remain in the current
// message. Inspect with [errors.Is].
//
// PackStream sizes Bytes/String payloads and List/Map/Struct collections with
// a length prefix that is up to a uint32 (~4.29e9). The 16 MiB message cap in
// [github.com/FlavioCFOliveira/GoGraph/bolt/proto.ChunkedReader] bounds the bytes a client may send, but
// it does NOT bound the allocation those bytes can *request*: a 5-byte frame
// such as 0xCE 0xFF 0xFF 0xFF 0xFF claims a ~4.29 GB Bytes payload, and a
// 5-byte List32 header claims billions of 16-byte interface slots (~64 GB).
// The eager make() that follows would OOM the process before the inevitable
// short read failed.
//
// The decoder defends against this by carrying a per-message byte budget (see
// [Decoder.remaining]) and rejecting any prefix whose minimum on-wire cost
// exceeds the bytes still available, BEFORE allocating. Every byte/string of
// length n needs n payload bytes; every collection of count n needs at least
// n bytes (each element is at least one wire byte). This is reachable
// pre-authentication during the first HELLO decode, so the bound is a hard
// security boundary, not a convenience.
var ErrLengthExceedsInput = errors.New("packstream: declared length exceeds remaining input")

// Type identifies the PackStream type of the next value in the stream.
type Type uint8

const (
	// TypeNull represents the PackStream NULL type.
	TypeNull Type = iota
	// TypeBool represents the PackStream Boolean type.
	TypeBool
	// TypeInt represents the PackStream Integer type.
	TypeInt
	// TypeFloat represents the PackStream Float type (Float64).
	TypeFloat
	// TypeBytes represents the PackStream Bytes type.
	TypeBytes
	// TypeString represents the PackStream String type.
	TypeString
	// TypeList represents the PackStream List type.
	TypeList
	// TypeMap represents the PackStream Map type.
	TypeMap
	// TypeStruct represents the PackStream Structure type.
	TypeStruct
)

// Decoder reads PackStream-encoded values from an underlying buffered reader.
// It is NOT safe for concurrent use.
type Decoder struct {
	r   *bufio.Reader
	buf [8]byte // scratch buffer; avoids per-call heap allocation
	// remaining is a conservative upper bound on the number of payload bytes
	// still consumable from the current message. It starts at the message
	// size when that size is known (a *bytes.Reader, *bytes.Buffer, or
	// *strings.Reader source, or an explicit limit), is decremented by
	// readFull/readByte as bytes are consumed, and gates every length-prefixed
	// allocation: a prefix n is rejected with ErrLengthExceedsInput when its
	// minimum on-wire cost exceeds remaining. A value of unknownRemaining means
	// the source length could not be determined; in that case the bound falls
	// back to the configured maxMessageBytes ceiling so allocations stay
	// capped rather than unbounded. See ErrLengthExceedsInput.
	remaining int
	// maxMessageBytes is the fallback allocation ceiling used when the source
	// length is unknown. It mirrors proto.DefaultMaxMessageBytes so a streaming
	// reader can never coerce an allocation larger than a single legal message.
	maxMessageBytes int
}

// defaultMaxMessageBytes mirrors proto.DefaultMaxMessageBytes (16 MiB). It is
// duplicated here rather than imported to avoid a packstream→proto dependency
// cycle: proto imports packstream for wire decoding. It is the allocation
// ceiling the decoder applies when the source length is unknown.
const defaultMaxMessageBytes = 16 << 20

// unknownRemaining marks a Decoder whose source length could not be
// determined (e.g. a raw streaming reader). In this state the byte budget is
// the maxMessageBytes ceiling rather than an exact remaining count.
const unknownRemaining = -1

// NewDecoder returns a new Decoder that reads from r.
//
// When r exposes its length (it is a *bytes.Reader, *bytes.Buffer, or
// *strings.Reader — as the Bolt server's per-message decode path is, reading
// a reassembled message from a bytes.Reader), the decoder uses that length as
// an exact byte budget and rejects any length/count prefix that exceeds the
// bytes actually remaining. For any other reader the length is unknown and the
// decoder falls back to capping allocations at the default 16 MiB message
// ceiling.
func NewDecoder(r io.Reader) *Decoder {
	d := &Decoder{r: bufio.NewReader(r), maxMessageBytes: defaultMaxMessageBytes}
	d.remaining = sourceLen(r)
	return d
}

// newDecoderFromBufio creates a Decoder wrapping an existing bufio.Reader.
// The byte budget starts unknown; callers reuse the Decoder via Reset, which
// recomputes the budget from the new source.
func newDecoderFromBufio(r *bufio.Reader) *Decoder {
	return &Decoder{r: r, maxMessageBytes: defaultMaxMessageBytes, remaining: unknownRemaining}
}

// Reset points the decoder at a new underlying reader and recomputes the
// per-message byte budget from r (see [NewDecoder]). It is used by the decode
// pool to reuse Decoder objects across messages.
func (d *Decoder) Reset(r io.Reader) {
	d.r.Reset(r)
	if d.maxMessageBytes == 0 {
		d.maxMessageBytes = defaultMaxMessageBytes
	}
	d.remaining = sourceLen(r)
}

// sourceLen reports the number of unread bytes in r when r is a length-bearing
// in-memory reader, or unknownRemaining otherwise. The supported types cover
// every in-memory source the codebase decodes from; a streaming reader (e.g. a
// net.Conn) reports unknownRemaining and is handled by the maxMessageBytes
// fallback.
func sourceLen(r io.Reader) int {
	switch s := r.(type) {
	case *bytes.Reader:
		return s.Len()
	case *bytes.Buffer:
		return s.Len()
	case *strings.Reader:
		return s.Len()
	default:
		return unknownRemaining
	}
}

// budget returns the current allocation budget in bytes: the exact remaining
// count when the source length is known, or the maxMessageBytes ceiling when
// it is not. It is the upper bound a single length/count prefix may claim.
func (d *Decoder) budget() int {
	if d.remaining == unknownRemaining {
		return d.maxMessageBytes
	}
	return d.remaining
}

// consume decrements the known-length budget by n bytes. It is a no-op when
// the source length is unknown. n is always the count just read via the
// bufio.Reader, so the budget tracks true bytes-remaining for known sources.
func (d *Decoder) consume(n int) {
	if d.remaining != unknownRemaining {
		d.remaining -= n
	}
}

// readByte reads one byte through the budget so remaining stays accurate.
func (d *Decoder) readByte() (byte, error) {
	b, err := d.r.ReadByte()
	if err == nil {
		d.consume(1)
	}
	return b, err
}

// readFull reads len(p) bytes through the budget so remaining stays accurate.
func (d *Decoder) readFull(p []byte) (int, error) {
	n, err := io.ReadFull(d.r, p)
	d.consume(n)
	return n, err
}

// peekByte returns the next byte without consuming it.
func (d *Decoder) peekByte() (byte, error) {
	b, err := d.r.Peek(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

// PeekType returns the PackStream type of the next value without consuming it.
func (d *Decoder) PeekType() (Type, error) {
	b, err := d.peekByte()
	if err != nil {
		return TypeNull, err
	}
	return markerToType(b), nil
}

// markerTypeTable maps every possible marker byte to its PackStream Type.
// Built once at init time for O(1) PeekType lookup with zero branches.
var markerTypeTable = func() [256]Type {
	var t [256]Type
	// TinyInt range: 0x00..0x7F (positive) and 0xF0..0xFF (negative two's complement).
	for i := 0x00; i <= 0x7F; i++ {
		t[i] = TypeInt
	}
	for i := 0xF0; i <= 0xFF; i++ {
		t[i] = TypeInt
	}
	// Specific markers.
	t[markerNull] = TypeNull
	t[markerTrue] = TypeBool
	t[markerFalse] = TypeBool
	t[markerFloat64] = TypeFloat
	t[markerInt8] = TypeInt
	t[markerInt16] = TypeInt
	t[markerInt32] = TypeInt
	t[markerInt64] = TypeInt
	t[markerBytes8] = TypeBytes
	t[markerBytes16] = TypeBytes
	t[markerBytes32] = TypeBytes
	t[markerStr8] = TypeString
	t[markerStr16] = TypeString
	t[markerStr32] = TypeString
	t[markerList8] = TypeList
	t[markerList16] = TypeList
	t[markerList32] = TypeList
	t[markerMap8] = TypeMap
	t[markerMap16] = TypeMap
	t[markerMap32] = TypeMap
	// TinyString: 0x80..0x8F.
	for i := tinyStrBase; i <= tinyStrBase+tinyStrMax; i++ {
		t[i] = TypeString
	}
	// TinyList: 0x90..0x9F.
	for i := tinyListBase; i <= tinyListBase+tinyListMax; i++ {
		t[i] = TypeList
	}
	// TinyMap: 0xA0..0xAF.
	for i := tinyMapBase; i <= tinyMapBase+tinyMapMax; i++ {
		t[i] = TypeMap
	}
	// TinyStruct: 0xB0..0xBF.
	for i := tinyStructBase; i <= tinyStructBase+tinyStructMax; i++ {
		t[i] = TypeStruct
	}
	return t
}()

// markerToType classifies a marker byte into a Type constant using a
// pre-computed lookup table — O(1), branch-free.
func markerToType(b byte) Type {
	return markerTypeTable[b]
}

// ReadNull consumes the NULL marker. Returns an error if the next value is
// not NULL.
func (d *Decoder) ReadNull() error {
	b, err := d.readByte()
	if err != nil {
		return err
	}
	if b != markerNull {
		return fmt.Errorf("packstream: expected NULL marker 0x%02X, got 0x%02X", markerNull, b)
	}
	return nil
}

// ReadBool reads and returns a Boolean value.
func (d *Decoder) ReadBool() (bool, error) {
	b, err := d.readByte()
	if err != nil {
		return false, err
	}
	switch b {
	case markerTrue:
		return true, nil
	case markerFalse:
		return false, nil
	default:
		return false, fmt.Errorf("packstream: expected Bool marker, got 0x%02X", b)
	}
}

// ReadInt reads and returns an Integer value, regardless of width.
//
// PackStream defines INT_8/INT_16/INT_32/INT_64 as fixed-width signed
// two's-complement integers. The byte/uint→int reinterpretation casts
// below preserve the wire bit pattern: they are the canonical decode and
// not unchecked overflows; gosec G115 is a false positive at each site.
func (d *Decoder) ReadInt() (int64, error) {
	b, err := d.readByte()
	if err != nil {
		return 0, err
	}
	switch b {
	case markerInt8:
		raw, err := d.readByte()
		if err != nil {
			return 0, err
		}
		return int64(int8(raw)), nil //nolint:gosec // G115: two's-complement INT_8 per Bolt PackStream spec
	case markerInt16:
		if _, err := d.readFull(d.buf[:2]); err != nil {
			return 0, err
		}
		return int64(int16(binary.BigEndian.Uint16(d.buf[:2]))), nil //nolint:gosec // G115: two's-complement INT_16 per Bolt PackStream spec
	case markerInt32:
		if _, err := d.readFull(d.buf[:4]); err != nil {
			return 0, err
		}
		return int64(int32(binary.BigEndian.Uint32(d.buf[:4]))), nil //nolint:gosec // G115: two's-complement INT_32 per Bolt PackStream spec
	case markerInt64:
		if _, err := d.readFull(d.buf[:8]); err != nil {
			return 0, err
		}
		return int64(binary.BigEndian.Uint64(d.buf[:8])), nil //nolint:gosec // G115: two's-complement INT_64 per Bolt PackStream spec (lossless bit reinterpretation)
	default:
		// TinyInt: high nibble 0xF (i.e., 0xF0..0xFF) → negative; 0x00..0x7F → positive.
		if b <= 0x7F || b >= 0xF0 {
			return int64(int8(b)), nil //nolint:gosec // G115: two's-complement TinyInt per Bolt PackStream spec
		}
		return 0, fmt.Errorf("packstream: expected Int marker, got 0x%02X", b)
	}
}

// ReadFloat reads and returns a Float64 value.
func (d *Decoder) ReadFloat() (float64, error) {
	b, err := d.readByte()
	if err != nil {
		return 0, err
	}
	if b != markerFloat64 {
		return 0, fmt.Errorf("packstream: expected Float64 marker, got 0x%02X", b)
	}
	if _, err := d.readFull(d.buf[:8]); err != nil {
		return 0, err
	}
	return math.Float64frombits(binary.BigEndian.Uint64(d.buf[:8])), nil
}

// ReadBytes reads and returns a Bytes value.
func (d *Decoder) ReadBytes() ([]byte, error) {
	b, err := d.readByte()
	if err != nil {
		return nil, err
	}
	var n int
	switch b {
	case markerBytes8:
		raw, err := d.readByte()
		if err != nil {
			return nil, err
		}
		n = int(raw)
	case markerBytes16:
		if _, err := d.readFull(d.buf[:2]); err != nil {
			return nil, err
		}
		n = int(binary.BigEndian.Uint16(d.buf[:2]))
	case markerBytes32:
		if _, err := d.readFull(d.buf[:4]); err != nil {
			return nil, err
		}
		n = int(binary.BigEndian.Uint32(d.buf[:4]))
	default:
		return nil, fmt.Errorf("packstream: expected Bytes marker, got 0x%02X", b)
	}
	// A Bytes payload of length n needs n payload bytes to follow; reject a
	// prefix that exceeds the bytes still available before the make(), so a
	// tiny frame cannot coerce a multi-gigabyte allocation. See
	// ErrLengthExceedsInput.
	if n > d.budget() {
		return nil, fmt.Errorf("%w: Bytes length %d > %d", ErrLengthExceedsInput, n, d.budget())
	}
	out := make([]byte, n)
	_, err = d.readFull(out)
	return out, err
}

// ReadString reads and returns a String value.
func (d *Decoder) ReadString() (string, error) {
	b, err := d.readByte()
	if err != nil {
		return "", err
	}
	var n int
	switch {
	case b >= tinyStrBase && b <= tinyStrBase+tinyStrMax:
		n = int(b & 0x0F)
	case b == markerStr8:
		raw, err := d.readByte()
		if err != nil {
			return "", err
		}
		n = int(raw)
	case b == markerStr16:
		if _, err := d.readFull(d.buf[:2]); err != nil {
			return "", err
		}
		n = int(binary.BigEndian.Uint16(d.buf[:2]))
	case b == markerStr32:
		if _, err := d.readFull(d.buf[:4]); err != nil {
			return "", err
		}
		n = int(binary.BigEndian.Uint32(d.buf[:4]))
	default:
		return "", fmt.Errorf("packstream: expected String marker, got 0x%02X", b)
	}
	if n == 0 {
		return "", nil
	}
	// A String payload of length n needs n payload bytes to follow; reject a
	// prefix that exceeds the bytes still available before the make(). See
	// ErrLengthExceedsInput.
	if n > d.budget() {
		return "", fmt.Errorf("%w: String length %d > %d", ErrLengthExceedsInput, n, d.budget())
	}
	out := make([]byte, n)
	_, err = d.readFull(out)
	return string(out), err
}

// ReadListHeader reads the list marker and returns the number of elements.
// The caller is responsible for reading exactly that many values.
func (d *Decoder) ReadListHeader() (int, error) {
	b, err := d.readByte()
	if err != nil {
		return 0, err
	}
	switch {
	case b >= tinyListBase && b <= tinyListBase+tinyListMax:
		return int(b & 0x0F), nil
	case b == markerList8:
		raw, err := d.readByte()
		if err != nil {
			return 0, err
		}
		return int(raw), nil
	case b == markerList16:
		if _, err := d.readFull(d.buf[:2]); err != nil {
			return 0, err
		}
		return int(binary.BigEndian.Uint16(d.buf[:2])), nil
	case b == markerList32:
		if _, err := d.readFull(d.buf[:4]); err != nil {
			return 0, err
		}
		return int(binary.BigEndian.Uint32(d.buf[:4])), nil
	default:
		return 0, fmt.Errorf("packstream: expected List marker, got 0x%02X", b)
	}
}

// ReadMapHeader reads the map marker and returns the number of key/value pairs.
// The caller is responsible for reading exactly 2*n items.
func (d *Decoder) ReadMapHeader() (int, error) {
	b, err := d.readByte()
	if err != nil {
		return 0, err
	}
	switch {
	case b >= tinyMapBase && b <= tinyMapBase+tinyMapMax:
		return int(b & 0x0F), nil
	case b == markerMap8:
		raw, err := d.readByte()
		if err != nil {
			return 0, err
		}
		return int(raw), nil
	case b == markerMap16:
		if _, err := d.readFull(d.buf[:2]); err != nil {
			return 0, err
		}
		return int(binary.BigEndian.Uint16(d.buf[:2])), nil
	case b == markerMap32:
		if _, err := d.readFull(d.buf[:4]); err != nil {
			return 0, err
		}
		return int(binary.BigEndian.Uint32(d.buf[:4])), nil
	default:
		return 0, fmt.Errorf("packstream: expected Map marker, got 0x%02X", b)
	}
}

// ReadStructHeader reads the struct marker and returns the tag byte and the
// number of fields. PackStream v2 supports only TinyStruct (0..15 fields).
func (d *Decoder) ReadStructHeader() (tag byte, n int, err error) {
	b, err := d.readByte()
	if err != nil {
		return 0, 0, err
	}
	if b < tinyStructBase || b > tinyStructBase+tinyStructMax {
		return 0, 0, fmt.Errorf("packstream: expected Struct marker, got 0x%02X", b)
	}
	n = int(b & 0x0F)
	tag, err = d.readByte()
	return tag, n, err
}
