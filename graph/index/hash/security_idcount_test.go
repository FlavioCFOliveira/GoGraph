package hash

// security_idcount_test.go — regression gate for #1885: the per-entry idCount
// deserialization bound must be len(body)/8 (each node id is 8 wire bytes), not
// len(body). The looser bound admitted an ~8x over-declaration that forced
// make([]uint64, idCount) (plus binary.Read's transient buffer, ~16x total)
// before the short read failed. Defense-in-depth against a forged index blob
// reaching this decoder via store/recovery on a hostile snapshot directory.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/index"
)

// craftHashForgedIDCount builds a CRC-valid one-entry hash payload (int64 key,
// no id bytes) whose per-entry idCount field is set to forged.
func craftHashForgedIDCount(forged uint64) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, hashMagic)
	_ = binary.Write(&b, binary.LittleEndian, hashFormatVersion)
	_ = binary.Write(&b, binary.LittleEndian, uint64(1)) // entryCount
	_ = binary.Write(&b, binary.LittleEndian, uint32(8)) // keyLen (int64 key)
	_ = binary.Write(&b, binary.LittleEndian, int64(42)) // 8-byte key bytes
	_ = binary.Write(&b, binary.LittleEndian, forged)    // idCount (no ids follow)
	body := b.Bytes()
	crc := crc32.Checksum(body, castagnoli)
	var tr [4]byte
	binary.LittleEndian.PutUint32(tr[:], crc)
	return append(append([]byte{}, body...), tr[:]...)
}

// TestSec_HashDeserialize_RejectsOverDeclaredIdCount forges a per-entry idCount
// of 36 for a 36-byte body carrying no id bytes (len(body)/8 = 4). The value
// sits in the (4, 36] gap the prior len(body) bound admitted — which then made
// make([]uint64, 36) before EOFing — so the tightened bound must reject it up
// front with the "implausible idCount" error.
func TestSec_HashDeserialize_RejectsOverDeclaredIdCount(t *testing.T) {
	t.Parallel()
	payload := craftHashForgedIDCount(36) // body is 36 bytes; 36 > 36/8 = 4
	err := New[int64]().Deserialize(bytes.NewReader(payload))
	if !errors.Is(err, index.ErrIndexCorrupted) {
		t.Fatalf("err = %v, want wrapped index.ErrIndexCorrupted", err)
	}
	if !strings.Contains(err.Error(), "implausible idCount") {
		t.Fatalf("err = %v, want the up-front implausible-idCount rejection "+
			"(idCount bound must be len(body)/8, not len(body))", err)
	}
}
