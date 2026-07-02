package packstream_test

// charge_upperbound_test.go — security engagement 2026-07-02 R2 (#1849).
//
// Finding A1 (CWE-789 / CWE-770): the decoded-memory cost model charged 48 B
// per map entry on the mistaken premise that map entries pack densely. Go
// allocates a whole hash bucket on the first insert, so a ~3.43 MiB pre-auth
// HELLO that is a list of tiny 1-entry maps forced ~403 MiB of live heap for a
// ~110 MiB charge (~3.7x under-count), breaching both the 128 MiB per-message
// contract and the #1845 aggregate ceiling. Analysis of the fix also showed
// lists of boxed scalars under-count similarly (16 B/elem charged vs ~24 B
// real). The costs are now UPPER bounds on Go's real allocation.
//
// This end-to-end test proves the exact crafted vector the finding described — a
// list of tiny 1-entry maps that the old model waved through — is now rejected.
// The empirical charge >= real-allocation invariant is proven separately in
// charge_upperbound_alloc_test.go (gated //go:build !race, because the race
// detector inflates allocation and would make the tight bound flaky).

import (
	"bytes"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
)

// TestDecoder_ListOfTinyMapsRejected drives the exact finding-A1 vector through
// the real decoder: a List of K tiny 1-entry maps. With K=400000 the old model
// charged ~38 MiB (list 6.4 MiB + 80 B/map) and waved it through while Go
// allocated ~137 MiB (~114x the 1.2 MiB wire size). The corrected map cost now
// exceeds the 128 MiB per-message budget partway through the list, so the
// decoder rejects it with ErrDecodedMemoryExceeded before that heap is realised.
func TestDecoder_ListOfTinyMapsRejected(t *testing.T) {
	t.Parallel()
	const k = 400_000
	// tinyMap1 = Map(1){ "" : null } : 0xA1 (TinyMap len 1), tinyStrZero (empty
	// string key), markerNull (null value) = 3 wire bytes each.
	tinyMap1 := []byte{0xA1, tinyStrZero, markerNull}

	frame := make([]byte, 0, 5+k*len(tinyMap1))
	frame = append(frame, markerList32)
	frame = appendUint32(frame, uint32(k))
	for i := 0; i < k; i++ {
		frame = append(frame, tinyMap1...)
	}

	dec := packstream.NewDecoder(bytes.NewReader(frame))
	v, err := dec.ReadValue()
	if !errors.Is(err, packstream.ErrDecodedMemoryExceeded) {
		t.Fatalf("decoding a list of %d tiny 1-entry maps: err = %v (value %T), want ErrDecodedMemoryExceeded "+
			"(the corrected map cost must catch the amplification the old 48 B/entry model missed)", k, err, v)
	}
}
