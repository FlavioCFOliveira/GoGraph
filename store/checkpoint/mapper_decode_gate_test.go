package checkpoint

// mapper_decode_gate_test.go — regression tests for rmp #2780.
//
// THE DEFECT (the half rmp #2749 left open). #2749 made phase 2 parse the
// published snapshot with snapshot.LoadSnapshotFull — the reader recovery uses
// — before the WAL prefix is discarded. That closed the READER half.
//
// The APPLIER half stayed open. After loading, recovery calls
// snapshot.ApplyMapperToGraphWithCodec, which decodes every raw mapper key
// THROUGH THE CODEC (store/snapshot/apply.go). snapshot.ReadMapperBytes does no
// codec work at all: it validates magic, format version, record framing and the
// per-key length cap, and hands the key bytes back verbatim. So key bytes the
// reader accepts can still be refused at decode time — an undecodable encoding,
// or bytes the codec does not consume in full — and the outcome was precisely
// what #2749 set out to prevent: WAL truncated, store never opens again.
//
// THE FIX. VerifySnapshotReadable now runs the SAME per-record decode recovery
// runs, over the mapper readback it already holds in memory
// (snapshot.VerifyMapperDecodable, sharing decodeMapperKey with
// ApplyMapperToGraphWithCodec), and refuses the checkpoint before any WAL byte
// is released.
//
// WHY THESE TESTS USE int KEYS. snapshot.WriteMapper emits the frozen
// version-1 layout for N=string, whose keys carry no codec framing and land in
// MapperReadback.Pairs; the codec-decode path exists only for the version-2
// layout, which the writer emits for every other N. A string-keyed fixture
// would exercise nothing here.
//
// The two tests are complementary:
//
//   - TestCheckpoint_CodecMapperKeyUndecodable_DoesNotTruncateWAL proves the
//     gate now REFUSES an image whose mapper the codec cannot decode, and that
//     the data survives the refusal. It fails on the pre-fix code.
//   - TestCheckpoint_CodecMapperDecodes_PermittedTruncation_RecoversFromSnapshotAlone
//     proves the extended gate still PERMITS a good version-2 image and that
//     what it permitted is genuinely recoverable from the snapshot alone.
//     Without it the first test would be satisfied by a gate that refuses every
//     non-string store.

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// The version-2 mapper.bin layout, restated here so the fixture depends on the
// documented on-disk contract (store/snapshot/mapper.go, WriteMapper) rather
// than on the writer's internals:
//
//	uint32 magic ('GMAP') | uint16 formatVersion (2) | uint64 pairCount
//	per pair: uint64 nodeID | uint32 keyLen | [keyLen]byte codec-encoded key
const (
	mapperGateMagic     uint32 = 0x50414D47
	mapperGateVersionV2 uint16 = 2
	mapperGateHeaderLen        = 4 + 2 + 8
	mapperGateRecPfxLen        = 8 + 4
)

// mapperKeyRecord is one parsed (nodeID, codec-encoded key) record.
type mapperKeyRecord struct {
	id  uint64
	key []byte
}

// parseCodecMapperFile parses a version-2 mapper.bin image into its records.
// It refuses anything but the codec layout, so a fixture that accidentally
// produced a string-keyed (version-1) mapper fails loudly instead of poisoning
// nothing.
func parseCodecMapperFile(b []byte) ([]mapperKeyRecord, error) {
	if len(b) < mapperGateHeaderLen {
		return nil, fmt.Errorf("mapper.bin is %d bytes, shorter than its %d-byte header",
			len(b), mapperGateHeaderLen)
	}
	if got := binary.LittleEndian.Uint32(b[0:4]); got != mapperGateMagic {
		return nil, fmt.Errorf("mapper.bin magic %#x, want %#x", got, mapperGateMagic)
	}
	if got := binary.LittleEndian.Uint16(b[4:6]); got != mapperGateVersionV2 {
		return nil, fmt.Errorf("mapper.bin format version %d, want %d: the fixture needs the "+
			"codec layout, so the store's key type must not be string", got, mapperGateVersionV2)
	}
	n := binary.LittleEndian.Uint64(b[6:mapperGateHeaderLen])
	recs := make([]mapperKeyRecord, 0, n)
	off := mapperGateHeaderLen
	for i := uint64(0); i < n; i++ {
		if off+mapperGateRecPfxLen > len(b) {
			return nil, fmt.Errorf("mapper.bin truncated in record %d prefix", i)
		}
		id := binary.LittleEndian.Uint64(b[off : off+8])
		keyLen := int(binary.LittleEndian.Uint32(b[off+8 : off+12]))
		off += mapperGateRecPfxLen
		// keyLen came from a uint32, so it is only negative where int is 32 bits
		// wide — where the slice expression below would panic rather than fail.
		if keyLen < 0 || off+keyLen > len(b) {
			return nil, fmt.Errorf("mapper.bin truncated in record %d key (len %d)", i, keyLen)
		}
		recs = append(recs, mapperKeyRecord{id: id, key: append([]byte(nil), b[off:off+keyLen]...)})
		off += keyLen
	}
	if off != len(b) {
		return nil, fmt.Errorf("mapper.bin has %d bytes after its %d records", len(b)-off, n)
	}
	return recs, nil
}

// buildCodecMapperFile serialises records back into a version-2 mapper.bin
// image. The poisoning helper asserts it round-trips the writer's own bytes
// byte-for-byte before it mutates anything, so the fixture cannot silently
// change the framing it is supposed to leave intact.
func buildCodecMapperFile(recs []mapperKeyRecord) []byte {
	var buf bytes.Buffer
	var hdr [mapperGateHeaderLen]byte
	binary.LittleEndian.PutUint32(hdr[0:4], mapperGateMagic)
	binary.LittleEndian.PutUint16(hdr[4:6], mapperGateVersionV2)
	binary.LittleEndian.PutUint64(hdr[6:mapperGateHeaderLen], uint64(len(recs)))
	buf.Write(hdr[:])
	var pfx [mapperGateRecPfxLen]byte
	for _, r := range recs {
		binary.LittleEndian.PutUint64(pfx[0:8], r.id)
		binary.LittleEndian.PutUint32(pfx[8:12], uint32(len(r.key)))
		buf.Write(pfx[:])
		buf.Write(r.key)
	}
	return buf.Bytes()
}

// poisonMapperKeys rewrites the published snapshot's mapper.bin through mutate
// and re-stamps the manifest entry's size and CRC32C, so the image stays
// internally consistent and passes every integrity check the reader performs.
// The damage is confined to the CONTENT of the codec-encoded key bytes: the
// magic, the format version and the record framing are all left exactly as the
// writer produced them, which is what makes the reader accept the image and the
// codec refuse it.
func poisonMapperKeys(dir string, mutate func([]mapperKeyRecord) []mapperKeyRecord) error {
	mPath := filepath.Join(dir, "manifest.json")
	m, err := snapshot.ReadManifestFile(mPath)
	if err != nil {
		return err
	}
	idx := -1
	for i := range m.Files {
		if m.Files[i].Name == snapshot.MapperFile {
			idx = i
			break
		}
	}
	if idx < 0 {
		// A fixture that poisons nothing would pass vacuously: fail loudly.
		return errors.New("poisonMapperKeys: snapshot declares no " + snapshot.MapperFile +
			" — the fixture published no mapper to poison")
	}

	kPath := filepath.Join(dir, snapshot.MapperFile)
	b, err := os.ReadFile(kPath) //nolint:gosec // test-owned path under t.TempDir()
	if err != nil {
		return err
	}
	recs, err := parseCodecMapperFile(b)
	if err != nil {
		return err
	}
	if len(recs) == 0 {
		return errors.New("poisonMapperKeys: mapper.bin carries no records — nothing to poison")
	}
	if rt := buildCodecMapperFile(recs); !bytes.Equal(rt, b) {
		return fmt.Errorf("poisonMapperKeys: re-serialisation is not byte-identical to the "+
			"writer's image (%d vs %d bytes) — the fixture would be changing the framing, "+
			"not just the key content", len(rt), len(b))
	}

	out := buildCodecMapperFile(mutate(recs))
	if err := os.WriteFile(kPath, out, 0o600); err != nil {
		return err
	}
	m.Files[idx].Size = int64(len(out))
	m.Files[idx].CRC32C = crc32.Checksum(out, gateCastagnoli)

	f, err := os.OpenFile(mPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // test-owned path under t.TempDir()
	if err != nil {
		return err
	}
	if err := snapshot.WriteManifest(f, m); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// undecodableVarintKey replaces the first record's key with a single 0x80 byte:
// a lone varint continuation byte with nothing following it. encoding/binary's
// Varint reports n == 0 for that input, so txn.NewIntCodec().Decode refuses it
// with txn.ErrCodecDecode — while snapshot.ReadMapperBytes accepts the record
// verbatim (keyLen 1, one byte present, far below maxMapperKeyLen). This is the
// "a codec the two sides disagree on" case reaching the DECODE step instead of
// the reader.
func undecodableVarintKey(recs []mapperKeyRecord) []mapperKeyRecord {
	recs[0].key = []byte{0x80}
	return recs
}

// trailingKeyBytes appends one byte to the first record's key. The codec
// decodes the leading varint successfully and hands back a non-empty tail, so
// ApplyMapperToGraphWithCodec refuses the record on its framing check — the
// on-disk record and the codec disagree about where the key ends. The reader,
// which only checks that keyLen bytes are present, accepts it.
func trailingKeyBytes(recs []mapperKeyRecord) []mapperKeyRecord {
	recs[0].key = append(append([]byte(nil), recs[0].key...), 0x00)
	return recs
}

// mapperPoisonBackend is the production snapshot backend with ONE behaviour
// changed: after publishing, the mapper's key bytes are made undecodable.
// Everything else — the capture, the write, the manifest read, and above all
// the readback under test — is the embedded [osSnapshotBackend], so the test
// drives the real production path rather than a mock of it.
type mapperPoisonBackend[N comparable, W any] struct {
	osSnapshotBackend[N, W]
	mutate func([]mapperKeyRecord) []mapperKeyRecord
	err    *error
}

func (p mapperPoisonBackend[N, W]) WriteCapture(snapDir string, capt *snapshot.Capture[W], constraints []snapshot.ConstraintSpec, indexDefs []snapshot.IndexDefSpec) error {
	if err := p.osSnapshotBackend.WriteCapture(snapDir, capt, constraints, indexDefs); err != nil {
		return err
	}
	*p.err = poisonMapperKeys(snapDir, p.mutate)
	return nil
}

// seedCodecKeyedStore builds an int-keyed store on walPath, commits edges with
// labels and properties, and returns the graph, the WAL writer and the edges it
// committed. int keys make the writer emit the version-2 (codec) mapper layout,
// which is the only layout the decode pass under test applies to.
func seedCodecKeyedStore(t *testing.T, walPath string) (*lpg.Graph[int, int64], *wal.Writer, [][2]int) {
	t.Helper()
	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[int, int64](adjlist.Config{Directed: true})
	store := txn.NewStoreWithOptions[int, int64](g, w, txn.Options[int, int64]{
		Codec:       txn.NewIntCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	edges := [][2]int{{1, 2}, {2, 3}, {3, 4}}
	for _, e := range edges {
		tx := store.Begin()
		if err := tx.AddEdge(e[0], e[1], 1); err != nil {
			t.Fatalf("AddEdge(%d->%d): %v", e[0], e[1], err)
		}
		if err := tx.SetNodeLabel(e[0], "Person"); err != nil {
			t.Fatalf("SetNodeLabel(%d): %v", e[0], err)
		}
		if err := tx.SetNodeProperty(e[0], "n", lpg.Int64Value(int64(e[0]))); err != nil {
			t.Fatalf("SetNodeProperty(%d): %v", e[0], err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}
	return g, w, edges
}

// TestCheckpoint_CodecMapperKeyUndecodable_DoesNotTruncateWAL is the rmp #2780
// regression gate: a snapshot LoadSnapshotFull ACCEPTS and the codec REJECTS
// must not cost the WAL a single byte.
func TestCheckpoint_CodecMapperKeyUndecodable_DoesNotTruncateWAL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func([]mapperKeyRecord) []mapperKeyRecord
	}{
		{"undecodable key encoding", undecodableVarintKey},
		{"trailing bytes after the key", trailingKeyBytes},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			walPath := filepath.Join(dir, "wal")
			g, w, edges := seedCodecKeyedStore(t, walPath)

			walBefore := fileSize(t, walPath)
			if walBefore == 0 {
				t.Fatal("WAL is empty before the checkpoint: the fixture committed nothing, " +
					"so a 'WAL not truncated' assertion could not fail")
			}

			var poisonErr error
			var mu sync.Mutex
			cp := New[int, int64](Config{Dir: dir}, g, w, &mu,
				WithMapperCodec[int, int64](txn.NewIntCodec()),
				WithWeightCodec[int, int64](txn.NewInt64WeightCodec()),
				WithSnapshotFS[int, int64](mapperPoisonBackend[int, int64]{
					mutate: tc.mutate,
					err:    &poisonErr,
				}),
			)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cp.Start(ctx)
			trigErr := cp.Trigger()
			cp.Stop()

			if poisonErr != nil {
				t.Fatalf("fixture failed to poison the published mapper: %v", poisonErr)
			}

			snapDir := filepath.Join(dir, "snapshot")

			// PREMISE 1 — the READER accepts this image. Without this the test
			// would be a restatement of #2749 rather than evidence about the
			// decode half: the #2749 readback would already have refused it.
			loaded, lerr := snapshot.LoadSnapshotFull(snapDir)
			if lerr != nil {
				t.Fatalf("LoadSnapshotFull rejected the poisoned image (%v): the fixture "+
					"produced a READER-level failure, which rmp #2749 already gates, so this "+
					"test would say nothing about the codec-decode half", lerr)
			}
			if len(loaded.Mapper.RawPairs) == 0 {
				t.Fatalf("the published mapper carries no RawPairs (Pairs=%d): the store did "+
					"not produce a version-2 mapper, so the decode pass has nothing to check",
					len(loaded.Mapper.Pairs))
			}
			// PREMISE 2 — the CODEC refuses it, established through the exact
			// call recovery makes on the readback (store/recovery/recovery.go).
			probe := lpg.New[int, int64](adjlist.Config{Directed: true})
			if err := snapshot.ApplyMapperToGraphWithCodec(probe, loaded.Mapper, txn.NewIntCodec()); err == nil {
				t.Fatal("ApplyMapperToGraphWithCodec accepted the poisoned mapper: the fixture " +
					"did not make the keys undecodable, so nothing under test is exercised")
			}

			// 1. The checkpoint must fail LOUDLY: an image recovery cannot apply
			//    is corruption, not a supported degraded mode.
			if trigErr == nil {
				t.Error("Trigger returned nil for a snapshot whose mapper the codec cannot " +
					"decode: the checkpoint reported success on an image recovery would refuse")
			}
			if got := cp.Stats().LastError; got == "" {
				t.Error("Stats().LastError is empty after publishing an undecodable mapper")
			}

			// 2. THE DURABILITY ASSERTION — this is what fails on pre-fix code.
			if got := cp.Stats().WALTruncBytes; got != 0 {
				t.Errorf("WALTruncBytes = %d, want 0: the checkpointer discarded the WAL prefix "+
					"behind a snapshot recovery cannot apply — committed data is unrecoverable", got)
			}
			if got := fileSize(t, walPath); got != walBefore {
				t.Errorf("WAL size = %d, want %d (unchanged): the WAL was truncated behind a "+
					"snapshot whose mapper does not decode", got, walBefore)
			}
			if err := w.Close(); err != nil {
				t.Fatalf("wal.Close: %v", err)
			}

			// 3. THE HARM, demonstrated: with this snapshot in place the store
			//    does not open at all. Pre-fix that state was permanent, because
			//    the WAL prefix behind it had been released.
			if _, err := recovery.Open[int, int64](dir, recovery.Options[int, int64]{
				Codec:       txn.NewIntCodec(),
				WeightCodec: txn.NewInt64WeightCodec(),
			}); err == nil {
				t.Error("recovery.Open succeeded with the undecodable snapshot in place: the " +
					"fixture does not model the failure it claims to")
			}

			// 4. And the retention was WORTH something: with the bad snapshot
			//    removed, the retained WAL alone still reconstructs every
			//    committed transaction. This is what the truncation destroyed.
			if err := os.RemoveAll(snapDir); err != nil {
				t.Fatalf("remove poisoned snapshot: %v", err)
			}
			res, err := recovery.Open[int, int64](dir, recovery.Options[int, int64]{
				Codec:       txn.NewIntCodec(),
				WeightCodec: txn.NewInt64WeightCodec(),
			})
			if err != nil {
				t.Fatalf("recovery.Open from the retained WAL: %v", err)
			}
			if res.WALOps == 0 {
				t.Error("WALOps = 0 recovering from the WAL alone: nothing was replayed, so " +
					"this check could not have detected a truncation")
			}
			for _, e := range edges {
				if !res.Graph.AdjList().HasEdge(e[0], e[1]) {
					t.Errorf("edge %d->%d lost: the retained WAL did not restore committed state",
						e[0], e[1])
				}
			}
		})
	}
}

// TestCheckpoint_CodecMapperDecodes_PermittedTruncation_RecoversFromSnapshotAlone
// is the other half of the extended gate's contract, and the reason the test
// above cannot be satisfied by a gate that refuses every version-2 mapper.
//
// It runs an ordinary checkpoint on an undamaged int-keyed store, asserts the
// published mapper really is the codec layout (so the decode pass ran on
// something), asserts the gate PERMITTED the truncation, then erases the WAL and
// asserts recovery rebuilds the exact committed graph from the snapshot alone.
func TestCheckpoint_CodecMapperDecodes_PermittedTruncation_RecoversFromSnapshotAlone(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")
	g, w, edges := seedCodecKeyedStore(t, walPath)
	wantOrder := g.AdjList().Order()
	wantSize := g.AdjList().Size()

	var mu sync.Mutex
	cp := New[int, int64](Config{Dir: dir}, g, w, &mu,
		WithMapperCodec[int, int64](txn.NewIntCodec()),
		WithWeightCodec[int, int64](txn.NewInt64WeightCodec()),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cp.Start(ctx)
	if err := cp.Trigger(); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	cp.Stop()

	// The decode pass must have had real work: a version-1 (string) mapper
	// would leave RawPairs empty and this test would prove nothing about it.
	loaded, err := snapshot.LoadSnapshotFull(filepath.Join(dir, "snapshot"))
	if err != nil {
		t.Fatalf("LoadSnapshotFull on the approved snapshot: %v", err)
	}
	if len(loaded.Mapper.RawPairs) == 0 {
		t.Fatalf("approved mapper carries no RawPairs (Pairs=%d): the codec layout was not "+
			"published, so the decode pass was a no-op here", len(loaded.Mapper.Pairs))
	}

	if got := cp.Stats().WALTruncBytes; got == 0 {
		t.Fatalf("WALTruncBytes = 0 (LastError=%q): the gate refused a perfectly good "+
			"version-2 snapshot, so the decode pass rejects images it must accept",
			cp.Stats().LastError)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}

	// Erase the WAL: whatever recovery finds now came from the snapshot the
	// extended gate approved, and from nowhere else.
	if err := os.Truncate(walPath, 0); err != nil {
		t.Fatalf("truncate WAL: %v", err)
	}
	res, err := recovery.Open[int, int64](dir, recovery.Options[int, int64]{
		Codec:       txn.NewIntCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("recovery.Open from the snapshot alone: %v", err)
	}
	if !res.SnapshotHit {
		t.Fatal("SnapshotHit = false: recovery did not read the snapshot at all")
	}
	if res.WALOps != 0 {
		t.Fatalf("WALOps = %d, want 0: the WAL still contributed, so this is not a "+
			"snapshot-alone recovery", res.WALOps)
	}
	if got := res.Graph.AdjList().Order(); got != wantOrder {
		t.Errorf("Order = %d, want %d", got, wantOrder)
	}
	if got := res.Graph.AdjList().Size(); got != wantSize {
		t.Errorf("Size = %d, want %d", got, wantSize)
	}
	for _, e := range edges {
		if !res.Graph.AdjList().HasEdge(e[0], e[1]) {
			t.Errorf("edge %d->%d missing after snapshot-alone recovery", e[0], e[1])
		}
		if !res.Graph.HasNodeLabel(e[0], "Person") {
			t.Errorf("label Person on %d missing after snapshot-alone recovery", e[0])
		}
		v, ok := res.Graph.GetNodeProperty(e[0], "n")
		if !ok {
			t.Errorf("property %d.n missing after snapshot-alone recovery", e[0])
			continue
		}
		if i, _ := v.Int64(); i != int64(e[0]) {
			t.Errorf("property %d.n = %d, want %d", e[0], i, e[0])
		}
	}
}
