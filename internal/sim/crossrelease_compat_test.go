package sim

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/goldens"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// The SHA-256 of every byte of the frozen legacy snapshot fixture, pinned HERE,
// in source (rmp #2477).
//
// A digest in the test source is what a golden file cannot be: unwritable by the
// test run. internal/goldens exists and is used below — for the artefact that is
// SUPPOSED to track the current writer — but its whole contract is that
// `go test -update` rewrites the fixture to match current output. Applied to a
// deliberately OLD artefact that is a trapdoor: a fixture regenerated in the
// CURRENT format would carry the rmp #2520 trailer and the current csr.bin
// shape, every assertion below would still pass, and the test would be
// certifying the new format against itself. rmp #2520 avoided exactly this by
// leaving its own frozen fixture unframed; changing these constants has to be a
// deliberate edit.
const (
	legacyManifestSHA256   = "ae03cac522a3a44e1db3eaf03a6cd2f9d3e633ffa2b9cc10077536d1039bc69a"
	legacyCSRSHA256        = "3bd9ca2f57396656f715a166c02e1b4839f75db4fcbf832058fba14ff73f3c04"
	legacyLabelsSHA256     = "50ae7a8ec719f8e6377d11222e7150480d3ed4c4567c70491681f603d9cea39a"
	legacyPropertiesSHA256 = "b7c96a63800b0480a94e705b53356279feb084f4025ac14868b9cb5e65d31af0"
	legacyMapperSHA256     = "c1698589217017a2fb8a7a220e44784b50df041d0c13a8ebc549a0f5e1690c8a"
)

// legacyFixtureFiles maps each frozen component to its pinned digest.
var legacyFixtureFiles = map[string]string{
	"manifest.json":  legacyManifestSHA256,
	"csr.bin":        legacyCSRSHA256,
	"labels.bin":     legacyLabelsSHA256,
	"properties.bin": legacyPropertiesSHA256,
	"mapper.bin":     legacyMapperSHA256,
}

// legacySnapshotSubdir is the snapshot directory inside the frozen fixture's
// store root.
func legacySnapshotSubdir() string { return filepath.Join(LegacyFullSnapshotDir, "snapshot") }

// TestCrossReleaseCompat_FixtureIsGenuinelyOldFormat is the gate every other
// assertion in this file rests on: the frozen artefact really is in the
// PRE-rmp-#2520, PRE-rmp-#2526 shape, byte for byte.
//
// Nothing downstream is meaningful without it. "An old manifest loads and
// reports itself unverified" is a statement about an old manifest; if the
// fixture were quietly regenerated in the current format the same test would
// pass while asserting nothing about backward compatibility at all.
func TestCrossReleaseCompat_FixtureIsGenuinelyOldFormat(t *testing.T) {
	t.Parallel()
	dir := legacySnapshotSubdir()

	// ── Verdict: the bytes are the bytes ──────────────────────────────────────
	for name, want := range legacyFixtureFiles {
		b, err := ReadFixtureFile(dir, name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		sum := sha256.Sum256(b)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Fatalf("FROZEN FIXTURE MOVED: %s sha256 = %s, want %s (%d bytes). "+
				"If this fixture was regenerated with the current writer it is no longer "+
				"an old-format artefact and proves nothing about backward compatibility",
				name, got, want, len(b))
		}
	}

	// ── Verdict: the manifest carries no rmp #2520 framing ────────────────────
	manifest, err := ReadFixtureFile(dir, "manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	framing := InspectManifestBytes(manifest)
	if framing.Framed() {
		t.Fatalf("fixture manifest carries the rmp #2520 trailer: %+v", framing)
	}
	if framing.TrailerMagicPresent {
		t.Errorf("fixture manifest ends with the trailer magic: %+v", framing)
	}
	if framing.DeclaredIntegrity != "" {
		t.Errorf("fixture manifest declares integrity %q; a pre-#2520 manifest has no such key",
			framing.DeclaredIntegrity)
	}
	// Everything past the JSON value must be the single newline the encoder
	// appends — that is precisely the region store/snapshot reads as "no trailer".
	if framing.TrailerBytes != 1 {
		t.Errorf("fixture manifest has %d bytes past the JSON value, want exactly 1 (the newline)",
			framing.TrailerBytes)
	}

	// ── Verdict: csr.bin predates the rmp #2526 sentinel ──────────────────────
	csrBytes, err := ReadFixtureFile(dir, "csr.bin")
	if err != nil {
		t.Fatal(err)
	}
	h, err := ReadCSRHeaderBytes(csrBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !h.HasWeights {
		t.Fatalf("fixture csr.bin declares no weights; the whole weight-width contract is unreachable on it")
	}
	if h.Width == SentinelCSRWidth {
		t.Fatalf("fixture csr.bin carries the rmp #2526 sentinel width 0x%X; it is a NEW artefact", h.Width)
	}
	if h.Width != 8 {
		t.Fatalf("fixture csr.bin width = %d, want 8 (the dense float64 column)", h.Width)
	}
	if h.NEdges != 4 {
		t.Fatalf("fixture csr.bin declares %d edges, want 4", h.NEdges)
	}

	t.Logf("frozen fixture: manifest %d bytes, unframed (integrity=%q, %d trailing byte); "+
		"csr.bin %d bytes, hasWeights=%v width=%d nV=%d nE=%d",
		len(manifest), framing.DeclaredIntegrity, framing.TrailerBytes,
		len(csrBytes), h.HasWeights, h.Width, h.NVertices, h.NEdges)
}

// TestCrossReleaseCompat_LegacyManifestLoadsUnverified asserts the rmp #2520
// compatibility contract on a real pre-#2520 artefact: it still loads, and it
// reports itself UNVERIFIED rather than claiming an integrity it does not have.
//
// The assertion is two-sided in the same test. A manifest the CURRENT writer
// produces must come back verified; the frozen one must not. One direction alone
// would be satisfied by an IntegrityVerified that is hardwired either way.
func TestCrossReleaseCompat_LegacyManifestLoadsUnverified(t *testing.T) {
	t.Parallel()

	// ── Verdict: the old artefact loads, unverified ───────────────────────────
	legacyPath := filepath.Join(legacySnapshotSubdir(), "manifest.json")
	old, err := snapshot.ReadManifestFile(legacyPath)
	if err != nil {
		t.Fatalf("current reader refused a pre-#2520 manifest: %v", err)
	}
	if old.IntegrityVerified {
		t.Errorf("pre-#2520 manifest reported IntegrityVerified true; it carries no trailer to verify")
	}
	if old.Integrity != "" {
		t.Errorf("pre-#2520 manifest reported Integrity %q, want empty", old.Integrity)
	}
	if old.Version != 3 {
		t.Errorf("pre-#2520 fixture manifest version = %d, want 3", old.Version)
	}
	if len(old.Files) != 4 {
		t.Errorf("pre-#2520 fixture manifest lists %d component files, want 4 "+
			"(csr.bin, labels.bin, properties.bin, mapper.bin)", len(old.Files))
	}

	// ── Control: the current writer's manifest verifies ───────────────────────
	tmp := filepath.Join(t.TempDir(), "manifest.json")
	f, err := os.Create(tmp) //nolint:gosec // tmp is from t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	if err := snapshot.WriteManifest(f, snapshot.Manifest{Version: snapshot.ManifestVersion}); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	fresh, err := snapshot.ReadManifestFile(tmp)
	if err != nil {
		t.Fatalf("current writer's own manifest did not load: %v", err)
	}

	// ── Non-vacuity gate (shape only) ─────────────────────────────────────────
	// The control must differ from the fixture on BOTH observations — the decoded
	// flag and the raw framing — or "unverified" is not evidence of anything.
	if !fresh.IntegrityVerified {
		t.Fatalf("NON-VACUITY: the current writer's manifest also read back unverified, "+
			"so IntegrityVerified does not discriminate old from new (integrity=%q)", fresh.Integrity)
	}
	if fresh.Integrity != snapshot.IntegrityCRC32CTrailer {
		t.Fatalf("NON-VACUITY: control manifest declares integrity %q, want %q",
			fresh.Integrity, snapshot.IntegrityCRC32CTrailer)
	}
	freshBytes, err := os.ReadFile(tmp) //nolint:gosec // tmp is from t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	if fr := InspectManifestBytes(freshBytes); !fr.Framed() {
		t.Fatalf("NON-VACUITY: the current writer emitted an UNFRAMED manifest (%+v); "+
			"the fixture's unframed shape is then not distinctive", fr)
	}

	t.Logf("rmp #2520 cross-version contract: frozen manifest loads unverified "+
		"(v%d, integrity=%q); current writer's manifest verifies (integrity=%q)",
		old.Version, old.Integrity, fresh.Integrity)
}

// TestCrossReleaseCompat_LegacySnapshotRecoversFullStack drives the frozen
// pre-#2520/#2526 snapshot directory through the SAME full-stack recovery the
// cross-release reopen now uses, and asserts the graph comes back whole.
//
// It is the file-format twin of TestCrossRelease_FullStackReopenOpensSnapshot:
// that one proves the reopen opens a snapshot the CURRENT writer produced, this
// one proves it opens a snapshot written in the OLD shape.
func TestCrossReleaseCompat_LegacySnapshotRecoversFullStack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Recovery may write to the directory it opens (it promotes a stranded
	// snapshot backup, and a caller may append to the WAL), so it is never
	// pointed at the committed fixture. The copy is also what lets the fixture's
	// digests be re-checked afterwards.
	root := t.TempDir()
	copyDirInto(t, legacySnapshotSubdir(), filepath.Join(root, "snapshot"))

	res, err := recovery.OpenCtx[string, float64](ctx, root, recovery.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("current recovery refused a pre-#2520/#2526 snapshot directory: %v", err)
	}

	// ── Verdict ───────────────────────────────────────────────────────────────
	if !res.SnapshotHit {
		t.Fatalf("recovery did not load the legacy snapshot directory")
	}
	if res.SnapshotSchemaVersion != 3 {
		t.Errorf("recovered snapshot schema version = %d, want 3", res.SnapshotSchemaVersion)
	}
	if got := res.Graph.AdjList().Order(); got != 4 {
		t.Fatalf("recovered node count = %d, want 4", got)
	}
	if got := res.Graph.AdjList().Size(); got != 4 {
		t.Fatalf("recovered edge count = %d, want 4", got)
	}
	// Labels and typed properties must survive, not merely the topology.
	if !res.Graph.HasNodeLabel("p1", "Person") || !res.Graph.HasNodeLabel("c1", "City") {
		t.Errorf("labels lost: p1 labels %v, c1 labels %v",
			res.Graph.NodeLabels("p1"), res.Graph.NodeLabels("c1"))
	}
	pv, ok := res.Graph.GetNodeProperty("p2", "name")
	if s, sok := pv.String(); !ok || !sok || s != "grace" {
		t.Errorf("property p2.name = %q (present=%v, string=%v), want \"grace\"", s, ok, sok)
	}
	pv, ok = res.Graph.GetNodeProperty("p2", "age")
	if n, nok := pv.Int64(); !ok || !nok || n != 45 {
		t.Errorf("property p2.age = %d (present=%v, int=%v), want 45", n, ok, nok)
	}

	// ── Non-vacuity gate (shape only) ─────────────────────────────────────────
	// There is no WAL in this directory, so nothing but the snapshot bytes could
	// have produced the graph above. Assert that rather than assume it: a stray
	// WAL beside the fixture would make every assertion above WAL-satisfiable.
	if _, err := os.Stat(filepath.Join(root, "wal")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("NON-VACUITY: a WAL exists beside the legacy snapshot (stat err %v); "+
			"the recovered graph is no longer attributable to the snapshot bytes", err)
	}
	if res.WALOps != 0 {
		t.Fatalf("NON-VACUITY: recovery replayed %d WAL ops over a WAL-less directory", res.WALOps)
	}
	if res.SnapshotLabels == 0 || res.SnapshotProperties == 0 {
		t.Fatalf("NON-VACUITY: the snapshot contributed %d labels and %d properties; "+
			"the label/property assertions above did not come from the fixture",
			res.SnapshotLabels, res.SnapshotProperties)
	}

	// The committed fixture must be exactly as it was: recovery ran against the
	// copy, and a read path that mutated its input would be a defect in itself.
	assertFixtureDigestsUnchanged(t)

	t.Logf("legacy snapshot recovered full-stack: v%d, order=%d size=%d, "+
		"snapshotLabels=%d snapshotProperties=%d walOps=%d",
		res.SnapshotSchemaVersion, res.Graph.AdjList().Order(), res.Graph.AdjList().Size(),
		res.SnapshotLabels, res.SnapshotProperties, res.WALOps)
}

// TestCrossReleaseCompat_DenseWeightsLoadUnchanged asserts the second rmp #2526
// compatibility clause on the frozen artefact: a csr.bin whose weights predate
// the sentinel still parses through the dense native path, with the exact
// float64 values it was written with.
func TestCrossReleaseCompat_DenseWeightsLoadUnchanged(t *testing.T) {
	t.Parallel()

	raw, err := ReadFixtureFile(legacySnapshotSubdir(), "csr.bin")
	if err != nil {
		t.Fatal(err)
	}
	rb, err := snapshot.ReadCSR(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("current reader refused a pre-#2526 csr.bin: %v", err)
	}

	// ── Verdict ───────────────────────────────────────────────────────────────
	if rb.CodecWeights() {
		t.Fatalf("pre-#2526 csr.bin took the codec-encoded path (width %d)", rb.WeightSize)
	}
	if rb.WeightSize != 8 || !rb.HasWeights {
		t.Fatalf("readback width = %d hasWeights = %v, want 8 / true", rb.WeightSize, rb.HasWeights)
	}
	if len(rb.Edges) != 4 {
		t.Fatalf("readback edges = %d, want 4", len(rb.Edges))
	}
	if rb.WeightOffsets != nil {
		t.Errorf("dense readback carries a codec offsets array of %d entries", len(rb.WeightOffsets))
	}
	if len(rb.WeightBytes) != 8*len(rb.Edges) {
		t.Fatalf("dense weights section = %d bytes, want %d", len(rb.WeightBytes), 8*len(rb.Edges))
	}
	// Decode the column independently of the store's own decoder — the on-disk
	// contract is little-endian IEEE-754, so a second decoder can hold the first
	// to it rather than restating it.
	//
	// The values are pinned in SLOT order, which is the order the frozen bytes
	// carry: a CSR groups a node's out-edges together and orders the groups by
	// NodeID, which is not the order the fixture's writer created the edges in.
	// Pinning the slot order rather than the multiset is deliberate — it is what
	// makes a re-ordering of the column a failure rather than a silent pass.
	want := []float64{2.25, 4, 1.5, 0.5}
	for i, w := range want {
		got := math.Float64frombits(binary.LittleEndian.Uint64(rb.WeightBytes[i*8 : i*8+8]))
		if got != w {
			t.Errorf("weight slot %d = %v (dst %d), want %v", i, got, rb.Edges[i], w)
		}
	}

	// ── Non-vacuity gate (shape only) ─────────────────────────────────────────
	// The independent decoder must be reading the FIXTURE's bytes. A zero-filled
	// or absent section would decode to all-zeros and quietly agree with a
	// weightless graph — which is the exact loss rmp #2526 closed.
	nonZero := 0
	for _, w := range want {
		if w != 0 {
			nonZero++
		}
	}
	if nonZero != len(want) {
		t.Fatalf("NON-VACUITY: the expected weight set contains a zero, which a weightless "+
			"section would also produce: %v", want)
	}

	t.Logf("pre-#2526 dense csr.bin loads unchanged: width=%d, edges %v, weights %v",
		rb.WeightSize, rb.Edges, want)
}

// xreleaseWeight is a NAMED integer weight type: precisely the class rmp #2526
// added the codec-encoded section for. csrWeightSize's type switch matches
// concrete types only, so a named type falls through to its zero default and
// selects the sentinel path.
type xreleaseWeight int64

// xreleaseWeightCodec encodes [xreleaseWeight] as a fixed 8-byte little-endian
// value. It satisfies store/snapshot's unexported weightEncoder structurally,
// which is all [snapshot.WriteCSRWithWeightCodec] requires.
type xreleaseWeightCodec struct{}

// Encode appends the wire form of w to buf.
func (xreleaseWeightCodec) Encode(buf []byte, w xreleaseWeight) ([]byte, error) {
	var tmp [8]byte
	binary.LittleEndian.PutUint64(tmp[:], uint64(w))
	return append(buf, tmp[:]...), nil
}

// TestCrossReleaseCompat_NewSentinelCSRRefusedByOldReader asserts the OTHER
// direction of the rmp #2526 contract: a NEW artefact is refused by an OLD
// reader deterministically, rather than mis-read as a dense column.
//
// The old reader no longer exists in this tree, so what is applied is its
// documented DECISION RULE ([LegacyCSRReaderVerdict]), restated from the two
// guards that release contained. The rule is held to both directions in this one
// test — it must REFUSE the new artefact and ACCEPT the frozen old one — so a
// rule that refused everything cannot pass.
func TestCrossReleaseCompat_NewSentinelCSRRefusedByOldReader(t *testing.T) {
	t.Parallel()

	// A three-node, three-edge CSR with NAMED-integer weights, written by the
	// current writer through the codec path.
	c := csr.FromArrays[xreleaseWeight](
		[]uint64{0, 2, 3, 3},
		[]graph.NodeID{1, 2, 2},
		[]xreleaseWeight{7, -3, 1 << 40},
		3, 3,
	)
	var buf bytes.Buffer
	if _, _, err := snapshot.WriteCSRWithWeightCodec(&buf, c, xreleaseWeightCodec{}); err != nil {
		t.Fatalf("WriteCSRWithWeightCodec: %v", err)
	}
	newBytes := buf.Bytes()

	// The NEW artefact is golden-pinned, which is the legitimate use of
	// internal/goldens here: it is meant to track the current writer, so a
	// deliberate `-update` after a format change is the correct workflow. The OLD
	// fixture above is pinned by source digest for the opposite reason.
	goldens.Assert(t, filepath.Join(XReleaseFixtureDir, "sentinel_csr.golden"), newBytes)

	newHdr, err := ReadCSRHeaderBytes(newBytes)
	if err != nil {
		t.Fatal(err)
	}

	// ── Verdict: the new artefact carries the sentinel and the current reader
	//    takes the codec path on it ────────────────────────────────────────────
	if newHdr.Width != SentinelCSRWidth {
		t.Fatalf("current writer emitted width %d for a named-integer weight, want the 0x%X sentinel",
			newHdr.Width, SentinelCSRWidth)
	}
	rb, err := snapshot.ReadCSR(bytes.NewReader(newBytes))
	if err != nil {
		t.Fatalf("current reader refused its own sentinel-bearing csr.bin: %v", err)
	}
	if !rb.CodecWeights() {
		t.Fatalf("current reader did not take the codec-weights path (width %d)", rb.WeightSize)
	}
	if len(rb.WeightOffsets) != len(rb.Edges)+1 {
		t.Fatalf("codec offsets array = %d entries, want %d", len(rb.WeightOffsets), len(rb.Edges)+1)
	}

	// ── Verdict: an OLD reader refuses it, by both guards ─────────────────────
	newVerdict := LegacyCSRReaderVerdict(newHdr, int64(len(newBytes)), 8)
	if newVerdict.Accepted {
		t.Fatalf("a pre-#2526 reader would have ACCEPTED the sentinel artefact: %s", newVerdict.Reason)
	}
	if !newVerdict.WidthGuardRefused {
		t.Errorf("the width guard did not refuse the sentinel: %+v", newVerdict)
	}
	if !newVerdict.ExtentGuardRefused {
		t.Errorf("the extent guard did not refuse the sentinel: %+v", newVerdict)
	}

	// ── Non-vacuity gate (shape only) ─────────────────────────────────────────
	// The same rule, unchanged, must ACCEPT the frozen old artefact. Without this
	// a rule hardwired to refuse would satisfy every assertion above.
	oldRaw, err := ReadFixtureFile(legacySnapshotSubdir(), "csr.bin")
	if err != nil {
		t.Fatal(err)
	}
	oldHdr, err := ReadCSRHeaderBytes(oldRaw)
	if err != nil {
		t.Fatal(err)
	}
	oldVerdict := LegacyCSRReaderVerdict(oldHdr, int64(len(oldRaw)), 8)
	if !oldVerdict.Accepted {
		t.Fatalf("NON-VACUITY: the legacy reader rule REFUSED the pre-#2526 artefact it must "+
			"accept (%s); the refusal of the new artefact carries no information", oldVerdict.Reason)
	}

	t.Logf("rmp #2526 cross-version contract: new artefact (width 0x%X, %d bytes) refused by "+
		"width+extent guards [%s]; frozen dense artefact (width %d) accepted [%s]",
		newHdr.Width, len(newBytes), newVerdict.Reason, oldHdr.Width, oldVerdict.Reason)
}

// copyDirInto copies every regular file from src into a freshly created dst.
func copyDirInto(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.MkdirAll(dst, 0o750); err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(src, e.Name())) //nolint:gosec // repo-internal fixture path
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// assertFixtureDigestsUnchanged re-checks every frozen component against its
// pinned digest, so a test that accidentally wrote through to the committed
// fixture fails here rather than corrupting the evidence for the next run.
func assertFixtureDigestsUnchanged(t *testing.T) {
	t.Helper()
	for name, want := range legacyFixtureFiles {
		b, err := ReadFixtureFile(legacySnapshotSubdir(), name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		sum := sha256.Sum256(b)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Fatalf("the committed fixture %s was MUTATED by this test run (sha256 %s, want %s)",
				name, got, want)
		}
	}
}
