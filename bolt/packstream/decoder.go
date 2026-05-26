package packstream

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

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
}

// NewDecoder returns a new Decoder that reads from r.
func NewDecoder(r io.Reader) *Decoder {
	return &Decoder{r: bufio.NewReader(r)}
}

// newDecoderFromBufio creates a Decoder wrapping an existing bufio.Reader.
func newDecoderFromBufio(r *bufio.Reader) *Decoder {
	return &Decoder{r: r}
}

// Reset points the decoder at a new underlying reader.
func (d *Decoder) Reset(r io.Reader) {
	d.r.Reset(r)
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
	b, err := d.r.ReadByte()
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
	b, err := d.r.ReadByte()
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
	b, err := d.r.ReadByte()
	if err != nil {
		return 0, err
	}
	switch b {
	case markerInt8:
		raw, err := d.r.ReadByte()
		if err != nil {
			return 0, err
		}
		return int64(int8(raw)), nil //nolint:gosec // G115: two's-complement INT_8 per Bolt PackStream spec
	case markerInt16:
		if _, err := io.ReadFull(d.r, d.buf[:2]); err != nil {
			return 0, err
		}
		return int64(int16(binary.BigEndian.Uint16(d.buf[:2]))), nil //nolint:gosec // G115: two's-complement INT_16 per Bolt PackStream spec
	case markerInt32:
		if _, err := io.ReadFull(d.r, d.buf[:4]); err != nil {
			return 0, err
		}
		return int64(int32(binary.BigEndian.Uint32(d.buf[:4]))), nil //nolint:gosec // G115: two's-complement INT_32 per Bolt PackStream spec
	case markerInt64:
		if _, err := io.ReadFull(d.r, d.buf[:8]); err != nil {
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
	b, err := d.r.ReadByte()
	if err != nil {
		return 0, err
	}
	if b != markerFloat64 {
		return 0, fmt.Errorf("packstream: expected Float64 marker, got 0x%02X", b)
	}
	if _, err := io.ReadFull(d.r, d.buf[:8]); err != nil {
		return 0, err
	}
	return math.Float64frombits(binary.BigEndian.Uint64(d.buf[:8])), nil
}

// ReadBytes reads and returns a Bytes value.
func (d *Decoder) ReadBytes() ([]byte, error) {
	b, err := d.r.ReadByte()
	if err != nil {
		return nil, err
	}
	var n int
	switch b {
	case markerBytes8:
		raw, err := d.r.ReadByte()
		if err != nil {
			return nil, err
		}
		n = int(raw)
	case markerBytes16:
		if _, err := io.ReadFull(d.r, d.buf[:2]); err != nil {
			return nil, err
		}
		n = int(binary.BigEndian.Uint16(d.buf[:2]))
	case markerBytes32:
		if _, err := io.ReadFull(d.r, d.buf[:4]); err != nil {
			return nil, err
		}
		n = int(binary.BigEndian.Uint32(d.buf[:4]))
	default:
		return nil, fmt.Errorf("packstream: expected Bytes marker, got 0x%02X", b)
	}
	out := make([]byte, n)
	_, err = io.ReadFull(d.r, out)
	return out, err
}

// ReadString reads and returns a String value.
func (d *Decoder) ReadString() (string, error) {
	b, err := d.r.ReadByte()
	if err != nil {
		return "", err
	}
	var n int
	switch {
	case b >= tinyStrBase && b <= tinyStrBase+tinyStrMax:
		n = int(b & 0x0F)
	case b == markerStr8:
		raw, err := d.r.ReadByte()
		if err != nil {
			return "", err
		}
		n = int(raw)
	case b == markerStr16:
		if _, err := io.ReadFull(d.r, d.buf[:2]); err != nil {
			return "", err
		}
		n = int(binary.BigEndian.Uint16(d.buf[:2]))
	case b == markerStr32:
		if _, err := io.ReadFull(d.r, d.buf[:4]); err != nil {
			return "", err
		}
		n = int(binary.BigEndian.Uint32(d.buf[:4]))
	default:
		return "", fmt.Errorf("packstream: expected String marker, got 0x%02X", b)
	}
	if n == 0 {
		return "", nil
	}
	out := make([]byte, n)
	_, err = io.ReadFull(d.r, out)
	return string(out), err
}

// ReadListHeader reads the list marker and returns the number of elements.
// The caller is responsible for reading exactly that many values.
func (d *Decoder) ReadListHeader() (int, error) {
	b, err := d.r.ReadByte()
	if err != nil {
		return 0, err
	}
	switch {
	case b >= tinyListBase && b <= tinyListBase+tinyListMax:
		return int(b & 0x0F), nil
	case b == markerList8:
		raw, err := d.r.ReadByte()
		if err != nil {
			return 0, err
		}
		return int(raw), nil
	case b == markerList16:
		if _, err := io.ReadFull(d.r, d.buf[:2]); err != nil {
			return 0, err
		}
		return int(binary.BigEndian.Uint16(d.buf[:2])), nil
	case b == markerList32:
		if _, err := io.ReadFull(d.r, d.buf[:4]); err != nil {
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
	b, err := d.r.ReadByte()
	if err != nil {
		return 0, err
	}
	switch {
	case b >= tinyMapBase && b <= tinyMapBase+tinyMapMax:
		return int(b & 0x0F), nil
	case b == markerMap8:
		raw, err := d.r.ReadByte()
		if err != nil {
			return 0, err
		}
		return int(raw), nil
	case b == markerMap16:
		if _, err := io.ReadFull(d.r, d.buf[:2]); err != nil {
			return 0, err
		}
		return int(binary.BigEndian.Uint16(d.buf[:2])), nil
	case b == markerMap32:
		if _, err := io.ReadFull(d.r, d.buf[:4]); err != nil {
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
	b, err := d.r.ReadByte()
	if err != nil {
		return 0, 0, err
	}
	if b < tinyStructBase || b > tinyStructBase+tinyStructMax {
		return 0, 0, fmt.Errorf("packstream: expected Struct marker, got 0x%02X", b)
	}
	n = int(b & 0x0F)
	tag, err = d.r.ReadByte()
	return tag, n, err
}
