package snapshot

// Security fix-regression battery for SECURITY-GAP #1486.
//
// SECURITY-GAP #1486 (rmp): the manifest FileEntry.Size was not threaded to the
// labels / properties / mapper / edgehandles readers, so a hostile snapshot
// could declare a record count far larger than the file's real on-disk length
// and the structural reader would grow its slice via append to the declared
// count before the post-parse CRC gate ran. The csr.bin path already bounded
// itself via readCSRLimited(tee, FileEntry.Size); the other four did not.
//
// THE FIX: each readVerified{Labels,Properties,Mapper,EdgeHandles} now wraps the
// component file in boundedComponentReader(f, entry.Size) (an io.LimitReader for
// a positive size), exactly mirroring the readVerifiedCSR -> readCSRLimited size
// discipline. A declared count that exceeds what FileEntry.Size could hold now
// fails fail-stop on the first read past the recorded size, before the append
// loop can grow past the real on-disk size. For a VALID snapshot the recorded
// size equals the on-disk size, so the CRC over the bounded stream still covers
// exactly the component bytes and the component loads unchanged (ACID preserved).
//
// These tests assert the SECURE contract end-to-end through LoadSnapshotFull
// (the recovery.Open path): a manifest whose FileEntry.Size is smaller than the
// genuinely-large body it points at must now be REJECTED with ErrCorrupted, and
// a valid snapshot must still load. All counts are host-safe (just past the
// capHint clamp); no multi-GB allocation is ever attempted.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fixRegBuildLabelsBody builds a STRUCTURALLY COMPLETE labels.bin whose
// nodeCount node-label records all actually follow (each 12 bytes: uint64
// NodeID + uint32 StringIdx). The single string-table entry "L" lets every
// record carry StringIdx 0 (a valid index), so ReadLabels would parse the whole
// body to completion if it were allowed to read past the recorded size.
func fixRegBuildLabelsBody(nodeCount uint64) []byte {
	var b []byte
	b = secStorePutU32(b, labelsMagic)
	b = secStorePutU32(b, labelsFormatVersion)
	// string table: one entry "L" so StringIdx 0 is in range.
	b = secStorePutU64(b, 1)         // stringCount: one entry
	b = secStorePutU32(b, 1)         // utf8Len: one byte
	b = append(b, 'L')               // the name
	b = secStorePutU64(b, nodeCount) // nodeCount real records follow
	for i := uint64(0); i < nodeCount; i++ {
		b = secStorePutU64(b, i) // NodeID
		b = secStorePutU32(b, 0) // StringIdx into "L"
	}
	b = secStorePutU64(b, 0) // edgeCount: none
	return b
}

// fixRegBuildPropertiesBody builds a complete properties.bin whose nodeCount
// node-property records all follow. Each record is a PropBool (kind 4, value
// len 1, one value byte) keyed at index 0 of a one-entry key table.
func fixRegBuildPropertiesBody(nodeCount uint64) []byte {
	var b []byte
	b = secStorePutU32(b, propertiesMagic)
	b = secStorePutU32(b, propertiesFormatVersion)
	b = secStorePutU64(b, 1) // keyCount: one entry
	b = secStorePutU32(b, 1) // utf8Len: one byte
	b = append(b, 'k')       // key name
	b = secStorePutU64(b, nodeCount)
	for i := uint64(0); i < nodeCount; i++ {
		b = secStorePutU64(b, i) // NodeID
		b = secStorePutU32(b, 0) // KeyIdx into "k"
		b = append(b, byte(4))   // kind PropBool
		b = secStorePutU32(b, 1) // valueLen: one byte (fixed for bool)
		b = append(b, 0x01)      // value
	}
	b = secStorePutU64(b, 0) // edgeCount: none
	return b
}

// fixRegBuildMapperStringBody builds a complete v1 mapper.bin whose pairCount
// records all follow. Each record is a uint64 nodeID + uint32 keyLen(=0).
func fixRegBuildMapperStringBody(pairCount uint64) []byte {
	var b []byte
	b = secStorePutU32(b, mapperMagic)
	b = binary.LittleEndian.AppendUint16(b, mapperFormatVersionString)
	b = secStorePutU64(b, pairCount)
	for i := uint64(0); i < pairCount; i++ {
		b = secStorePutU64(b, i) // nodeID
		b = secStorePutU32(b, 0) // keyLen = 0
	}
	return b
}

// fixRegBuildEdgeHandlesBody builds a complete edgehandles.bin whose recordCount
// records all follow. Each record is src+dst+handle (24 bytes) followed by a
// zero labelCount and a zero propCount (8 bytes), so the whole body parses.
func fixRegBuildEdgeHandlesBody(recordCount uint64) []byte {
	var b []byte
	b = secStorePutU32(b, edgeHandlesMagic)
	b = secStorePutU32(b, edgeHandlesFormatVersion)
	b = secStorePutU64(b, 0) // empty label table
	b = secStorePutU64(b, 0) // empty key table
	b = secStorePutU64(b, recordCount)
	for i := uint64(0); i < recordCount; i++ {
		b = secStorePutU64(b, i)   // src
		b = secStorePutU64(b, i+1) // dst
		b = secStorePutU64(b, i)   // handle
		b = secStorePutU32(b, 0)   // labelCount: none
		b = secStorePutU32(b, 0)   // propCount: none
	}
	return b
}

// minimalEmptyCSR returns a structurally valid empty csr.bin (0 vertices, 0
// edges, no weights) so a snapshot directory always carries the mandatory CSR.
func minimalEmptyCSR() []byte {
	var csr []byte
	csr = secStorePutU64(csr, 0)
	csr = secStorePutU64(csr, 0)
	csr = append(csr, 0, 0)
	return csr
}

// writeSnapshotDir lays out a snapshot directory with the given csr.bin and one
// extra component file, writing a manifest whose extra-component FileEntry.Size
// is forced to declaredSize (which the tests deliberately set smaller than the
// real body). All CRCs are truthful.
func writeSnapshotDir(t *testing.T, dir, compName string, csr, comp []byte, declaredSize int64) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, CSRFile), csr, 0o600); err != nil {
		t.Fatalf("write csr.bin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, compName), comp, 0o600); err != nil {
		t.Fatalf("write %s: %v", compName, err)
	}
	m := Manifest{
		Version:   manifestVersionV2,
		CreatedAt: time.Now().UTC(),
		Files: []FileEntry{
			{Name: CSRFile, Size: int64(len(csr)), CRC32C: crc32.Checksum(csr, castagnoli)},
			{Name: compName, Size: declaredSize, CRC32C: crc32.Checksum(comp, castagnoli)},
		},
	}
	mf, err := os.OpenFile(filepath.Join(dir, "manifest.json"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("open manifest: %v", err)
	}
	if werr := WriteManifest(mf, m); werr != nil {
		_ = mf.Close()
		t.Fatalf("write manifest: %v", werr)
	}
	if cerr := mf.Close(); cerr != nil {
		t.Fatalf("close manifest: %v", cerr)
	}
}

// TestSec_FixReg_SizeBoundRejectsOversizedComponent is the security regression
// gate for #1486. For each of the four affected components it lays out a real
// snapshot directory whose component body is genuinely large (just past the
// capHint clamp) but whose manifest FileEntry.Size LIES that the file is only 64
// bytes. LoadSnapshotFull threads that size into boundedComponentReader, so the
// structural reader EOFs at byte 64 and the parse must fail fail-stop with
// ErrCorrupted — before the append loop can grow past the recorded size.
//
// Before the fix LoadSnapshotFull ignored FileEntry.Size for these four readers
// and parsed the whole oversized body (then rejected on the post-parse CRC only
// because the bytes drained exceeded the declared coverage — not before
// materialization). This test FAILS on the unfixed code (the body is fully
// materialized) and PASSES on the fixed code (rejected at the size boundary).
func TestSec_FixReg_SizeBoundRejectsOversizedComponent(t *testing.T) {
	t.Parallel()
	const over = labelsCapHintMax + 50000 // just past the 1<<20 clamp
	cases := []struct {
		name string
		file string
		body []byte
	}{
		{"labels", LabelsFile, fixRegBuildLabelsBody(over)},
		{"properties", PropertiesFile, fixRegBuildPropertiesBody(over)},
		{"mapper", MapperFile, fixRegBuildMapperStringBody(over)},
		{"edgehandles", EdgeHandlesFile, fixRegBuildEdgeHandlesBody(over)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			// Declare a tiny size (64 bytes) while the real body is multi-MB.
			writeSnapshotDir(t, dir, tc.file, minimalEmptyCSR(), tc.body, 64)

			_, err := LoadSnapshotFull(dir)
			if err == nil {
				t.Fatalf("%s: LoadSnapshotFull accepted a %d-byte body whose FileEntry.Size declared only 64 bytes; "+
					"the size bound was not enforced (#1486 regressed)", tc.file, len(tc.body))
			}
			if !errors.Is(err, ErrCorrupted) {
				t.Fatalf("%s: LoadSnapshotFull err = %v; want wrapped ErrCorrupted", tc.file, err)
			}
		})
	}
}

// TestSec_FixReg_ValidSnapshotStillLoads is the ACID-preservation companion: a
// snapshot whose manifest FileEntry.Size is TRUTHFUL (equals the on-disk body)
// must still load to completion through LoadSnapshotFull. This guards against an
// over-tight bound that would break legitimate recovery — the bound only changes
// the failure mode of a hostile artifact, never the success path of a valid one.
func TestSec_FixReg_ValidSnapshotStillLoads(t *testing.T) {
	t.Parallel()
	const n = 1000 // small, fully-honest body
	dir := t.TempDir()
	labels := fixRegBuildLabelsBody(n)
	// Truthful size: exactly the on-disk length.
	writeSnapshotDir(t, dir, LabelsFile, minimalEmptyCSR(), labels, int64(len(labels)))

	loaded, err := LoadSnapshotFull(dir)
	if err != nil {
		t.Fatalf("LoadSnapshotFull rejected a valid snapshot with a truthful FileEntry.Size: %v", err)
	}
	if uint64(len(loaded.Labels.NodeLabels)) != n {
		t.Fatalf("loaded %d node-label records; want %d (valid body must load unchanged)",
			len(loaded.Labels.NodeLabels), n)
	}
}

// TestSec_FixReg_TruncatedBodyFastFail confirms the other half — the low-level
// readers still fail fast on a hostile count with a TRUNCATED body, a typed
// corruption error and bounded allocation. This guards against losing the
// fast-fail path while the focus was on the genuinely-large-body gap.
func TestSec_FixReg_TruncatedBodyFastFail(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		hdr  func() []byte
		dec  func([]byte) error
	}{
		{
			name: "labels-nodecount-truncated",
			hdr: func() []byte {
				var b []byte
				b = secStorePutU32(b, labelsMagic)
				b = secStorePutU32(b, labelsFormatVersion)
				b = secStorePutU64(b, 0)             // empty string table
				b = secStorePutU64(b, uint64(1)<<40) // at-ceiling count, NO body
				return b
			},
			dec: func(p []byte) error { _, e := ReadLabels(bytes.NewReader(p)); return e },
		},
		{
			name: "properties-nodecount-truncated",
			hdr: func() []byte {
				var b []byte
				b = secStorePutU32(b, propertiesMagic)
				b = secStorePutU32(b, propertiesFormatVersion)
				b = secStorePutU64(b, 0)
				b = secStorePutU64(b, uint64(1)<<40)
				return b
			},
			dec: func(p []byte) error { _, e := ReadProperties(bytes.NewReader(p)); return e },
		},
		{
			name: "mapper-paircount-truncated",
			hdr: func() []byte {
				var b []byte
				b = secStorePutU32(b, mapperMagic)
				b = binary.LittleEndian.AppendUint16(b, mapperFormatVersionString)
				b = secStorePutU64(b, uint64(1)<<40)
				return b
			},
			dec: func(p []byte) error { _, e := ReadMapperString(bytes.NewReader(p)); return e },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := tc.dec(tc.hdr()); err == nil {
				t.Fatalf("%s: truncated body with at-ceiling count was accepted; want a corruption error", tc.name)
			}
		})
	}
}
