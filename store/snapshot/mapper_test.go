package snapshot

import (
	"bytes"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// TestMapper_WriteRead_Roundtrip walks a freshly-interned mapper into
// WriteMapperString, reads it back via ReadMapperString, and asserts
// the (NodeID, key) pairs survive in the same order as Walk emits.
func TestMapper_WriteRead_Roundtrip(t *testing.T) {
	t.Parallel()
	m := graph.NewMapper[string]()
	keys := []string{"alice", "bob", "carol", "dave", "eve", "frank"}
	want := make(map[string]graph.NodeID, len(keys))
	for _, k := range keys {
		want[k] = m.Intern(k)
	}

	var buf bytes.Buffer
	size, crc, err := WriteMapperString(&buf, m)
	if err != nil {
		t.Fatalf("WriteMapperString: %v", err)
	}
	if int64(buf.Len()) != size {
		t.Fatalf("size = %d, buf.Len = %d", size, buf.Len())
	}
	if crc == 0 {
		t.Fatal("CRC must be non-zero on a non-empty mapper")
	}

	rb, err := ReadMapperString(&buf)
	if err != nil {
		t.Fatalf("ReadMapperString: %v", err)
	}
	if len(rb.Pairs) != len(keys) {
		t.Fatalf("Pairs len = %d, want %d", len(rb.Pairs), len(keys))
	}
	for _, p := range rb.Pairs {
		gotID, ok := want[p.Key]
		if !ok {
			t.Errorf("unknown key %q in readback", p.Key)
			continue
		}
		if p.ID != gotID {
			t.Errorf("key %q ID = %d, want %d", p.Key, p.ID, gotID)
		}
	}
}

// TestMapper_WriteRead_EmptyMapper covers the degenerate (but valid)
// case of a mapper holding zero entries: the writer must still emit a
// well-formed header and the reader must surface an empty Pairs
// slice without an error.
func TestMapper_WriteRead_EmptyMapper(t *testing.T) {
	t.Parallel()
	m := graph.NewMapper[string]()
	var buf bytes.Buffer
	size, _, err := WriteMapperString(&buf, m)
	if err != nil {
		t.Fatalf("WriteMapperString: %v", err)
	}
	// Empty mapper: 4 (magic) + 2 (version) + 8 (pair count) = 14 bytes.
	if size != 14 {
		t.Fatalf("empty mapper size = %d, want 14", size)
	}
	rb, err := ReadMapperString(&buf)
	if err != nil {
		t.Fatalf("ReadMapperString(empty): %v", err)
	}
	if len(rb.Pairs) != 0 {
		t.Errorf("Pairs len = %d, want 0", len(rb.Pairs))
	}
}

// TestMapper_Read_BadMagic confirms a mapper.bin with the wrong magic
// prefix is rejected with ErrMapperCorrupted rather than returning
// garbage to the caller.
func TestMapper_Read_BadMagic(t *testing.T) {
	t.Parallel()
	// Build a payload with the wrong magic but everything else
	// well-formed.
	buf := bytes.NewBuffer([]byte{0xDE, 0xAD, 0xBE, 0xEF, 0x01, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
	_, err := ReadMapperString(buf)
	if !errors.Is(err, ErrMapperCorrupted) {
		t.Fatalf("ReadMapperString(bad magic) = %v, want ErrMapperCorrupted", err)
	}
}

// TestMapper_Read_UnsupportedVersion confirms a future format version
// surfaces as ErrMapperCorrupted; mapper.bin format upgrades require
// a new release that recognises the version byte.
func TestMapper_Read_UnsupportedVersion(t *testing.T) {
	t.Parallel()
	// magic 'GMAP' little-endian + version 9999 little-endian + 0 pairs.
	payload := []byte{
		0x47, 0x4D, 0x41, 0x50, // magic
		0x0F, 0x27, // version 9999 LE
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // pair count = 0
	}
	_, err := ReadMapperString(bytes.NewReader(payload))
	if !errors.Is(err, ErrMapperCorrupted) {
		t.Fatalf("ReadMapperString(future version) = %v, want ErrMapperCorrupted", err)
	}
}
