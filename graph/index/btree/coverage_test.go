package btree

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"testing"

	"gograph/graph"
	"gograph/graph/index"
)

// TestCardinality_NotFound covers the early-return 0 branch when the
// value is not present in the index.
func TestCardinality_NotFound(t *testing.T) {
	t.Parallel()
	idx := New[string]()
	if c := idx.Cardinality("ghost"); c != 0 {
		t.Errorf("Cardinality(ghost) = %d, want 0", c)
	}
}

// TestDelete_NotFound covers the early return when deleting a value
// that is not present in the index.
func TestDelete_NotFound(t *testing.T) {
	t.Parallel()
	idx := New[string]()
	idx.Delete("ghost", graph.NodeID(1)) // must not panic
}

// TestDeserialize_ShortPayload covers the len(all)<4 branch.
func TestDeserialize_ShortPayload(t *testing.T) {
	t.Parallel()
	idx := New[string]()
	err := idx.Deserialize(bytes.NewReader([]byte("x")))
	if !errors.Is(err, index.ErrIndexCorrupted) {
		t.Fatalf("short payload = %v, want ErrIndexCorrupted", err)
	}
}

// TestDeserialize_BadMagic covers the magic-mismatch branch.
// Payload: wrong magic (0xDEADBEEF) + version + entryCount=0 + correct CRC.
func TestDeserialize_BadMagic(t *testing.T) {
	t.Parallel()
	var body bytes.Buffer
	_ = binary.Write(&body, binary.LittleEndian, uint32(0xDEADBEEF)) // wrong magic
	_ = binary.Write(&body, binary.LittleEndian, btreeFormatVersion)
	_ = binary.Write(&body, binary.LittleEndian, uint64(0)) // entryCount
	checksum := crc32.Checksum(body.Bytes(), castagnoli)
	var payload bytes.Buffer
	payload.Write(body.Bytes())
	_ = binary.Write(&payload, binary.LittleEndian, checksum)
	idx := New[string]()
	err := idx.Deserialize(bytes.NewReader(payload.Bytes()))
	if !errors.Is(err, index.ErrIndexCorrupted) {
		t.Fatalf("bad magic = %v, want ErrIndexCorrupted", err)
	}
}

// TestDeserialize_BadVersion covers the version-mismatch branch.
func TestDeserialize_BadVersion(t *testing.T) {
	t.Parallel()
	var body bytes.Buffer
	_ = binary.Write(&body, binary.LittleEndian, btreeMagic)
	_ = binary.Write(&body, binary.LittleEndian, uint32(999)) // wrong version
	_ = binary.Write(&body, binary.LittleEndian, uint64(0))   // entryCount
	checksum := crc32.Checksum(body.Bytes(), castagnoli)
	var payload bytes.Buffer
	payload.Write(body.Bytes())
	_ = binary.Write(&payload, binary.LittleEndian, checksum)
	idx := New[string]()
	err := idx.Deserialize(bytes.NewReader(payload.Bytes()))
	if !errors.Is(err, index.ErrIndexCorrupted) {
		t.Fatalf("bad version = %v, want ErrIndexCorrupted", err)
	}
}
