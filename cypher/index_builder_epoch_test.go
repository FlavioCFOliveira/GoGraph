package cypher

// index_builder_epoch_test.go — rmp #2797 gate: a durable index payload written
// by a build whose backfill fabricated entries is REFUSED at reopen and the
// index is rebuilt, so the rmp #2778 / rmp #2792 fixes reach a store that was
// already on disk.
//
// # The defect these tests pin
//
// rmp #2778 and rmp #2792 fixed two CREATE INDEX / CREATE CONSTRAINT backfills
// that read the LIVE property bag and therefore indexed values an explicit
// transaction had eagerly written and rolled back. Neither fix heals a store
// that was already checkpointed by the defective build, because the fabricated
// entry is PERSISTED in indexes/<name>.bin and rehydrated verbatim — it is
// loaded, not rebuilt, so no change to the builder can reach it.
//
// Measured with a two-build experiment before this fix (one goroutine, no
// concurrency; the pre-fix build is HEAD with the two fixes reverse-applied):
//
//	STEP1  pre-fix build, durable store, checkpointed:
//	         person_name['ghost'] = [242]        <- carol's id under a value
//	         person_name['carol'] = []              nothing ever committed
//	STEP2  FIXED build, same directory:
//	         hydrated=2 rebuilt=0                <- the payload was LOADED
//	         n.name = 'ghost' -> rows=[carol]    <- FABRICATED ROW, post-fix
//	         n.name = 'carol' -> rows=[]         <- and the real one still lost
//
// A pure-WAL pre-fix store is NOT affected: recovery rebuilds from the committed
// graph, so there is no payload to rehydrate. Only a CHECKPOINTED store is.
//
// # How the pre-fix state is reconstructed here, and why that is faithful
//
// A single-build test cannot run the defective backfill — the defect is fixed.
// What it can do is reproduce the DURABLE STATE that backfill left, which is the
// only thing a reopen sees. Two artefacts define it:
//
//  1. indexes/person_name.bin holds carol's id under 'ghost' and nothing under
//     'carol'. [epochFabricatePayload] produces exactly that by deserialising
//     the CORRECT payload the checkpoint wrote, moving one id from one value to
//     the other, and re-serialising — a single-entry perturbation of real bytes,
//     not a hand-written file.
//  2. manifest.json carries no `index_builder_epoch`, which is what every build
//     before this one wrote. [epochStampManifest] removes it and re-stamps the
//     CRC32C trailer so the manifest is intact rather than corrupt.
//
// The reconstruction is not asserted to be faithful, it is SHOWN: the
// "hydratable epoch" arm of each test below reopens the very same directory with
// the epoch left at [snapshot.CurrentIndexBuilderEpoch] and reproduces the
// two-build measurement quoted above — hydrated non-zero, rebuilt zero, and the
// fabricated row returned. That arm is also the falsification seam: without it,
// the correct answer in the refusal arm could come from the fabrication never
// having landed rather than from the refusal.
//
// Layer: short.

import (
	"bytes"
	"context"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	indexhash "github.com/FlavioCFOliveira/GoGraph/graph/index/hash"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
)

// epochGhost is a value NO committed node ever holds. It stands for the value an
// open transaction wrote eagerly and rolled back, and a seek for it must always
// return no rows.
const epochGhost = "ghost"

// epochReal is the committed name of the node whose entry the fabrication moves.
// It is the other half of the defect: the entry a correct index must hold.
const epochReal = "carol"

// epochFillerNodes pads the fixture so the planner prefers an index seek over a
// label scan. A row assertion served by a scan would hold even with a completely
// wrong index, so without the padding every query-level oracle below would be
// unable to fail.
const epochFillerNodes = 2000

// ─── fixture and manipulation helpers ───────────────────────────────────────

// epochSeedCheckpointedStore builds a checkpointed store directory carrying the
// named hash index over (:Person, name) with four distinct people plus filler,
// and returns the single node id the index holds under [epochReal].
//
// ddl selects the DDL under test: a plain CREATE INDEX or a CREATE CONSTRAINT
// (whose synthetic backing index rmp #2792 governs). Every write is COMMITTED,
// so the payload the checkpoint publishes is correct — the fabrication is
// applied afterwards, to those real bytes.
func epochSeedCheckpointedStore(t testing.TB, dir, ddl, indexName string) uint64 {
	t.Helper()
	s := hydOpen(t, dir)
	s.write(ddl)
	for _, name := range []string{"alice", "bob", epochReal, "dave"} {
		s.write(`CREATE (:Person {name: '` + name + `'})`)
	}
	s.write(`UNWIND range(1, ` + strconv.Itoa(epochFillerNodes) + `) AS i CREATE (:Person {name: 'filler-' + toString(i)})`)

	ids := s.hashIndexIDs(indexName, epochReal)
	if len(ids) != 1 {
		t.Fatalf("index %q holds %v for %q before the checkpoint, want exactly one id: the "+
			"fabrication below moves that id, so anything else makes this fixture meaningless",
			indexName, ids, epochReal)
	}
	s.checkpoint()
	s.close()

	m := hydManifest(t, dir)
	if m.IndexesCommitTS == 0 {
		t.Fatalf("manifest carries no indexes_commit_ts after a real checkpoint, so the reopen "+
			"would refuse the payload for the WATERMARK and never reach the epoch: manifest=%+v", m)
	}
	if m.IndexBuilderEpoch != snapshot.CurrentIndexBuilderEpoch {
		t.Fatalf("manifest index_builder_epoch = %d, want %d: this build must stamp its own epoch, "+
			"or the arms below cannot tell a stripped epoch from a never-written one",
			m.IndexBuilderEpoch, snapshot.CurrentIndexBuilderEpoch)
	}
	if !epochManifestNames(&m, indexName) {
		t.Fatalf("the checkpoint published no payload named %q, so there is nothing to fabricate "+
			"or hydrate: manifest indexes=%+v", indexName, m.Indexes)
	}
	return ids[0]
}

// epochManifestNames reports whether m declares an index payload called name.
func epochManifestNames(m *snapshot.Manifest, name string) bool {
	for i := range m.Indexes {
		if m.Indexes[i].Name == name {
			return true
		}
	}
	return false
}

// epochFabricatePayload rewrites indexes/<name>.bin so it holds id under
// [epochGhost] instead of under [epochReal] — the exact durable shape the
// pre-#2778 backfill left — and re-stamps the manifest's size and CRC32C for it
// so the payload is INTACT rather than corrupt. Only the epoch may make a reader
// refuse it.
//
// It works on the real payload the checkpoint wrote: the bytes are deserialised
// into a standalone hash index, one id is moved between two values, and the
// result is re-serialised through the same [indexhash.Index.Serialize] the
// checkpoint used. The starting content is asserted first, so a payload that did
// not hold what this test believes it holds fails here rather than producing a
// silently different fabrication.
func epochFabricatePayload(t testing.TB, dir, name string, id uint64) {
	t.Helper()
	snapDir := filepath.Join(dir, "snapshot")
	path := filepath.Join(snapDir, snapshot.IndexesDir, name+".bin")

	raw, err := os.ReadFile(path) //nolint:gosec // path under t.TempDir()
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	idx := indexhash.New[string]()
	if derr := idx.Deserialize(bytes.NewReader(raw)); derr != nil {
		t.Fatalf("Deserialize the published payload %s: %v", path, derr)
	}
	if got := idx.Lookup(epochReal).ToArray(); !reflect.DeepEqual(got, []uint64{id}) {
		t.Fatalf("the published payload holds %v under %q, want [%d]: the checkpoint did not "+
			"publish the content this fabrication perturbs", got, epochReal, id)
	}
	if got := idx.Lookup(epochGhost).ToArray(); len(got) != 0 {
		t.Fatalf("the published payload already holds %v under %q, so the fabrication would be "+
			"indistinguishable from the correct content", got, epochGhost)
	}

	idx.Delete(epochReal, graph.NodeID(id))
	idx.Insert(epochGhost, graph.NodeID(id))

	var buf bytes.Buffer
	if serr := idx.Serialize(&buf); serr != nil {
		t.Fatalf("Serialize the fabricated payload: %v", serr)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil { // path under t.TempDir()
		t.Fatalf("WriteFile(%s): %v", path, err)
	}

	m := hydManifest(t, dir)
	patched := false
	for i := range m.Indexes {
		if m.Indexes[i].Name == name {
			m.Indexes[i].Size = int64(buf.Len())
			m.Indexes[i].CRC32C = crc32.Checksum(buf.Bytes(), crc32.MakeTable(crc32.Castagnoli))
			patched = true
		}
	}
	if !patched {
		t.Fatalf("manifest declares no index payload named %q", name)
	}
	epochWriteManifest(t, dir, &m)
}

// epochStampManifest sets the manifest's index_builder_epoch to epoch and
// re-stamps the CRC32C trailer. Passing 0 reproduces every manifest written
// before the field existed, because the field is `omitempty`: absence and zero
// are the same bytes.
func epochStampManifest(t testing.TB, dir string, epoch uint64) {
	t.Helper()
	m := hydManifest(t, dir)
	m.IndexBuilderEpoch = epoch
	epochWriteManifest(t, dir, &m)
	if got := hydManifest(t, dir); got.IndexBuilderEpoch != epoch {
		t.Fatalf("manifest index_builder_epoch reads back %d after stamping %d",
			got.IndexBuilderEpoch, epoch)
	}
}

// epochWriteManifest replaces manifest.json with m, framed by a fresh trailer so
// the file stays integrity-verifiable. Everything the arms below manipulate goes
// through here, so no arm can accidentally be measuring a CORRUPT manifest
// instead of a valid one carrying a different field value.
func epochWriteManifest(t testing.TB, dir string, m *snapshot.Manifest) {
	t.Helper()
	path := filepath.Join(dir, "snapshot", "manifest.json")
	f, err := os.Create(path) //nolint:gosec // path under t.TempDir()
	if err != nil {
		t.Fatalf("Create(%s): %v", path, err)
	}
	if werr := snapshot.WriteManifest(f, *m); werr != nil {
		_ = f.Close()
		t.Fatalf("WriteManifest: %v", werr)
	}
	if cerr := f.Close(); cerr != nil {
		t.Fatalf("Close(%s): %v", path, cerr)
	}
	if got := hydManifest(t, dir); !got.IntegrityVerified {
		t.Fatalf("the rewritten manifest is not integrity-verified, so every arm reading it would "+
			"be measuring a corrupt file: %+v", got)
	}
}

// ─── 1. the headline gate: a pre-epoch payload is rebuilt, not hydrated ─────

// TestIndexBuilderEpoch_PreEpochPayloadIsRebuiltNotHydrated is the rmp #2797
// acceptance gate for a plain CREATE INDEX.
//
// The two arms differ in ONE byte-level fact — whether manifest.json names this
// build's index builder — over an otherwise identical directory holding an
// identical fabricated payload:
//
//	epoch absent  -> the payload is refused, the index is REBUILT from the
//	                 recovered graph, and every answer matches the graph.
//	epoch current -> the payload is hydrated verbatim, reproducing the two-build
//	                 measurement in this file's header, fabricated row included.
//
// The second arm is what makes the first non-vacuous.
func TestIndexBuilderEpoch_PreEpochPayloadIsRebuiltNotHydrated(t *testing.T) {
	t.Parallel()
	const indexName = "person_name"
	const ddl = `CREATE INDEX person_name FOR (n:Person) ON (n.name)`

	t.Run("epoch absent: refused and rebuilt", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		id := epochSeedCheckpointedStore(t, dir, ddl, indexName)
		epochFabricatePayload(t, dir, indexName, id)
		epochStampManifest(t, dir, 0)

		s := hydOpen(t, dir)
		defer s.close()

		// RECOVERY's report: nothing certified hydratable, and the reason names
		// the epoch rather than the watermark or the mapper.
		if s.res.SnapshotIndexes != 0 {
			t.Fatalf("SnapshotIndexes = %d, want 0: a payload from an unknown index builder "+
				"must not be certified hydratable", s.res.SnapshotIndexes)
		}
		if len(s.res.SnapshotIndexPayloads) == 0 {
			t.Fatal("recovery reported no payloads at all, so the count above is vacuous")
		}
		for _, p := range s.res.SnapshotIndexPayloads {
			if p.Bytes != nil {
				t.Errorf("payload %q handed out %d bytes for a refused image", p.Name, len(p.Bytes))
			}
			if !errors.Is(p.Err, recovery.ErrIndexPayloadStale) {
				t.Errorf("payload %q Err = %v, want recovery.ErrIndexPayloadStale", p.Name, p.Err)
			}
			if p.Err == nil || !strings.Contains(p.Err.Error(), "index builder epoch") {
				t.Errorf("payload %q Err = %v does not attribute the refusal to the builder epoch, "+
					"so an operator cannot tell it from the watermark refusal", p.Name, p.Err)
			}
		}

		// The ENGINE's path attribution: nothing hydrated, and the rebuild
		// actually scanned, which is the work the refusal buys.
		if s.eng.recoveredIdx.hydrated != 0 {
			t.Fatalf("hydrated = %d, want 0", s.eng.recoveredIdx.hydrated)
		}
		if s.eng.recoveredIdx.rebuilt == 0 {
			t.Fatal("rebuilt = 0: the index was neither hydrated nor rebuilt, so it was never populated")
		}
		if s.eng.recoveredIdx.backfillNodes == 0 {
			t.Fatal("backfillNodes = 0: the rebuild walked nothing, so it cannot have re-derived " +
				"the index from the graph")
		}

		// CONTENTS, read straight out of the registered index against an
		// independent label scan of the recovered graph.
		want := s.nonEmptyIDs("(Person, name) = "+epochReal,
			s.expectedIDs("Person", "name", eqString(epochReal)))
		if got := s.hashIndexIDs(indexName, epochReal); !reflect.DeepEqual(got, want) {
			t.Errorf("rebuilt index %s[%q] = %v, want %v", indexName, epochReal, got, want)
		}
		if got := s.hashIndexIDs(indexName, epochGhost); len(got) != 0 {
			t.Errorf("rebuilt index %s[%q] = %v, want empty: the graph never committed that value",
				indexName, epochGhost, got)
		}

		// ROWS, which is the acceptance criterion. Both probes must go through
		// the index, or a label scan would answer them correctly regardless.
		s.mustSeekPlan(`MATCH (n:Person) WHERE n.name = '`+epochGhost+`' RETURN n.name AS name`, "NodeByIndexSeek")
		s.mustSeekPlan(`MATCH (n:Person) WHERE n.name = '`+epochReal+`' RETURN n.name AS name`, "NodeByIndexSeek")
		if rows := s.rows(`MATCH (n:Person) WHERE n.name = '`+epochGhost+`' RETURN n.name AS name`, "name"); len(rows) != 0 {
			t.Errorf("seek for the rolled-back value returned %v, want no rows", rows)
		}
		if rows := s.rows(`MATCH (n:Person) WHERE n.name = '`+epochReal+`' RETURN n.name AS name`, "name"); !reflect.DeepEqual(rows, []string{epochReal}) {
			t.Errorf("seek for the real value returned %v, want [%q]", rows, epochReal)
		}
	})

	t.Run("epoch current: hydrated verbatim", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		id := epochSeedCheckpointedStore(t, dir, ddl, indexName)
		epochFabricatePayload(t, dir, indexName, id)
		// Deliberately NOT stripped. The manifest already names this build's
		// epoch, so the only difference from the arm above is that one field.
		epochStampManifest(t, dir, snapshot.CurrentIndexBuilderEpoch)

		s := hydOpen(t, dir)
		defer s.close()

		// The field does not force a rebuild for ever: a current-epoch payload
		// is still loaded from disk.
		if s.eng.recoveredIdx.hydrated == 0 || s.eng.recoveredIdx.rebuilt != 0 {
			t.Fatalf("hydrated/rebuilt = %d/%d, want non-zero/0 — a payload this build's own "+
				"builder produced must still hydrate, or the epoch has turned every reopen into "+
				"a permanent rebuild", s.eng.recoveredIdx.hydrated, s.eng.recoveredIdx.rebuilt)
		}
		if s.res.SnapshotIndexes == 0 {
			t.Fatal("SnapshotIndexes = 0 for a current-epoch, watermarked, self-sufficient image")
		}

		// And hydration is VERBATIM, which is why the epoch is needed at all:
		// the fabricated entry comes back exactly as the two-build experiment
		// measured it, so the correct answers in the arm above are produced by
		// the refusal and by nothing else.
		if got := s.hashIndexIDs(indexName, epochGhost); !reflect.DeepEqual(got, []uint64{id}) {
			t.Fatalf("hydrated index %s[%q] = %v, want [%d]: the fabrication did not survive the "+
				"round trip, so the refusal arm proves nothing", indexName, epochGhost, got, id)
			return
		}
		if got := s.hashIndexIDs(indexName, epochReal); len(got) != 0 {
			t.Fatalf("hydrated index %s[%q] = %v, want empty (the fabricated payload lost it)",
				indexName, epochReal, got)
		}
		s.mustSeekPlan(`MATCH (n:Person) WHERE n.name = '`+epochGhost+`' RETURN n.name AS name`, "NodeByIndexSeek")
		if rows := s.rows(`MATCH (n:Person) WHERE n.name = '`+epochGhost+`' RETURN n.name AS name`, "name"); !reflect.DeepEqual(rows, []string{epochReal}) {
			t.Fatalf("seek for the rolled-back value returned %v, want [%q] — this arm reproduces "+
				"the defect on purpose; if it no longer does, the refusal arm above is no longer "+
				"gating anything", rows, epochReal)
		}
	})
}

// ─── 2. the UNIQUE constraint: value-set and backing index must agree ───────

// TestIndexBuilderEpoch_UniqueValueSetAndBackingIndexAgree covers what rmp #2792
// measured about a reopen: the two structures a UNIQUE constraint is enforced by
// heal DIFFERENTLY.
//
// [Engine.registerRecoveredConstraints] always re-seeds the value-set from a
// fresh scan of the recovered graph ([Engine.scanLabelProperty]), so the
// value-set self-heals on every reopen. The backing index does not: it goes
// through [Engine.populateRecoveredIndex], which hydrates the payload when
// recovery certifies it. On an affected store the two therefore disagree, and
// measured on the two-build experiment that disagreement is an ACID Consistency
// breach that SURVIVES the reopen:
//
//	__uniq__Person.name['ghost'] = [242]   (the index says a node holds it)
//	committed write of name='ghost' -> ACCEPTED   (the value-set says none does)
//	__uniq__Person.name['ghost'] = [242 243]      <- TWO ids under one UNIQUE value
//
// The refusal closes it by making the index heal the same way the value-set
// does. Both structures are asserted here, each against an oracle that is not
// the other one: the index against an independent label scan, the value-set
// against the enforcement verdict on a committed write.
func TestIndexBuilderEpoch_UniqueValueSetAndBackingIndexAgree(t *testing.T) {
	t.Parallel()
	const ddl = `CREATE CONSTRAINT c_name FOR (n:Person) REQUIRE n.name IS UNIQUE`
	indexName := exec.UniqueIndexName("Person", "name")

	t.Run("epoch absent: both structures agree", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		id := epochSeedCheckpointedStore(t, dir, ddl, indexName)
		epochFabricatePayload(t, dir, indexName, id)
		epochStampManifest(t, dir, 0)

		s := hydOpen(t, dir)
		defer s.close()

		if s.eng.recoveredIdx.hydrated != 0 || s.eng.recoveredIdx.rebuilt == 0 {
			t.Fatalf("hydrated/rebuilt = %d/%d, want 0/non-zero: a constraint's backing index is "+
				"populated through the same path as a user index and must refuse the same payload",
				s.eng.recoveredIdx.hydrated, s.eng.recoveredIdx.rebuilt)
		}

		// The INDEX, against an independent label scan.
		want := s.nonEmptyIDs("(Person, name) = "+epochReal,
			s.expectedIDs("Person", "name", eqString(epochReal)))
		if got := s.hashIndexIDs(indexName, epochReal); !reflect.DeepEqual(got, want) {
			t.Errorf("rebuilt backing index %s[%q] = %v, want %v", indexName, epochReal, got, want)
		}
		if got := s.hashIndexIDs(indexName, epochGhost); len(got) != 0 {
			t.Errorf("rebuilt backing index %s[%q] = %v, want empty", indexName, epochGhost, got)
		}

		// The VALUE-SET, through the only observable it has: the enforcement
		// verdict on a COMMITTED write. It must say the same thing the index
		// says — nothing holds the ghost, one node holds the real value.
		if err := epochTryCreatePerson(t, s, epochGhost); err != nil {
			t.Fatalf("a committed write of the rolled-back value was REFUSED (%v), but no node "+
				"holds it: the value-set and the index disagree", err)
		}
		if err := epochTryCreatePerson(t, s, epochReal); err == nil {
			t.Fatalf("a committed write duplicating %q was ACCEPTED under an active UNIQUE "+
				"constraint: that is a Consistency breach", epochReal)
		}
		// And after the accepted write the index holds exactly the ONE node that
		// now carries the ghost value — not two, which is what a hydrated
		// fabricated entry plus a self-healed value-set produces.
		after := s.expectedIDs("Person", "name", eqString(epochGhost))
		if len(after) != 1 {
			t.Fatalf("the graph holds %v under %q after one accepted write, want exactly one node",
				after, epochGhost)
		}
		if got := s.hashIndexIDs(indexName, epochGhost); !reflect.DeepEqual(got, after) {
			t.Errorf("backing index %s[%q] = %v after the accepted write, want %v — the index and "+
				"the graph must agree once the constraint has enforced against it",
				indexName, epochGhost, got, after)
		}
	})

	t.Run("epoch current: the two structures diverge", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		id := epochSeedCheckpointedStore(t, dir, ddl, indexName)
		epochFabricatePayload(t, dir, indexName, id)
		epochStampManifest(t, dir, snapshot.CurrentIndexBuilderEpoch)

		s := hydOpen(t, dir)
		defer s.close()

		if s.eng.recoveredIdx.hydrated == 0 || s.eng.recoveredIdx.rebuilt != 0 {
			t.Fatalf("hydrated/rebuilt = %d/%d, want non-zero/0: this arm needs the payload "+
				"hydrated to show what the refusal prevents",
				s.eng.recoveredIdx.hydrated, s.eng.recoveredIdx.rebuilt)
		}
		// The index carries the fabricated entry ...
		if got := s.hashIndexIDs(indexName, epochGhost); !reflect.DeepEqual(got, []uint64{id}) {
			t.Fatalf("hydrated backing index %s[%q] = %v, want [%d]", indexName, epochGhost, got, id)
		}
		// ... while the value-set, re-seeded from the graph, does not — so the
		// write is accepted and the index ends up with TWO ids under one UNIQUE
		// value. This is the divergence rmp #2792 measured, reproduced here so
		// the arm above is known to be closing it.
		if err := epochTryCreatePerson(t, s, epochGhost); err != nil {
			t.Fatalf("the value-set did not self-heal on reopen (write refused: %v), so this arm "+
				"is not reproducing the divergence it exists to document", err)
		}
		if got := s.hashIndexIDs(indexName, epochGhost); len(got) != 2 {
			t.Fatalf("backing index %s[%q] = %v after the accepted write, want two ids — the "+
				"divergence this arm documents is no longer reachable, so the refusal arm above "+
				"is no longer gating it", indexName, epochGhost, got)
		}
	})
}

// ─── 3. a store this build wrote carries the epoch ──────────────────────────

// TestIndexBuilderEpoch_CurrentBuildStampsAndHydrates is the "the field does not
// force a rebuild for ever" gate at the level a user sees: a store written and
// checkpointed by THIS build, with no manifest surgery of any kind, carries the
// epoch and hydrates every payload on reopen.
//
// It complements TestIndexHydration_CheckpointedReopen_SeeksExactRows, which
// asserts the same hydration without knowing the field exists — so that test
// going red would be the other witness that the epoch had broken the happy path.
func TestIndexBuilderEpoch_CurrentBuildStampsAndHydrates(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	id := epochSeedCheckpointedStore(t, dir, `CREATE INDEX person_name FOR (n:Person) ON (n.name)`, "person_name")

	m := hydManifest(t, dir)
	if m.IndexBuilderEpoch != snapshot.CurrentIndexBuilderEpoch {
		t.Fatalf("manifest index_builder_epoch = %d, want %d", m.IndexBuilderEpoch, snapshot.CurrentIndexBuilderEpoch)
	}

	s := hydOpen(t, dir)
	defer s.close()

	if s.eng.recoveredIdx.hydrated != len(m.Indexes) || s.eng.recoveredIdx.rebuilt != 0 {
		t.Fatalf("hydrated/rebuilt = %d/%d, want %d/0",
			s.eng.recoveredIdx.hydrated, s.eng.recoveredIdx.rebuilt, len(m.Indexes))
	}
	if got := s.hashIndexIDs("person_name", epochReal); !reflect.DeepEqual(got, []uint64{id}) {
		t.Fatalf("hydrated index person_name[%q] = %v, want [%d]", epochReal, got, id)
	}
	if got := s.hashIndexIDs("person_name", epochGhost); len(got) != 0 {
		t.Fatalf("hydrated index person_name[%q] = %v, want empty", epochGhost, got)
	}
}

// ─── small local helpers ────────────────────────────────────────────────────

// epochTryCreatePerson runs a COMMITTED `CREATE (:Person {name: value})` and
// returns the error the write failed with, or nil when it was accepted. It is
// the value-set's only observable: the registry exports no membership reader, so
// the enforcement verdict is what says whether a value is in the set.
func epochTryCreatePerson(t testing.TB, s *hydSession, value string) error {
	t.Helper()
	r, err := s.eng.RunInTxAny(context.Background(), `CREATE (:Person {name: '`+value+`'})`, nil)
	if err != nil {
		return err
	}
	for r.Next() { // drain to commit
	}
	rerr := r.Err()
	if cerr := r.Close(); cerr != nil && rerr == nil {
		rerr = cerr
	}
	return rerr
}

// ─── 4. the accepted cost, measured ─────────────────────────────────────────

// BenchmarkIndexBuilderEpochFirstOpen measures the price of the remediation as
// an operator experiences it: the FIRST open of a store whose manifest predates
// the epoch, against the same store carrying it.
//
// Both arms are a complete reopen — recovery.Open, wal.Open, the engine
// constructor — over an identical checkpointed fixture of
// [benchHydrationNodes] nodes with four registered indexes, differing only in
// the value of one manifest field. It is deliberately the WHOLE open rather than
// the engine-construction step alone, because that is the latency a first start
// after an upgrade actually pays; the isolated rebuild-versus-hydrate delta is
// what [BenchmarkRecoveredIndexPopulation] next door reports.
//
// The cost is ONE-TIME: the next checkpoint stamps the current epoch, after
// which the store hydrates again. Run it as:
//
//	go test -run='^$' -bench=BenchmarkIndexBuilderEpochFirstOpen -benchmem -count=10 ./cypher/
func BenchmarkIndexBuilderEpochFirstOpen(b *testing.B) {
	arms := []struct {
		name  string
		epoch uint64
	}{
		{"current_epoch", snapshot.CurrentIndexBuilderEpoch},
		{"pre_epoch", 0},
	}
	for _, arm := range arms {
		b.Run(arm.name, func(b *testing.B) {
			dir := buildHydrationFixture(b)
			epochStampManifest(b, dir, arm.epoch)

			// ATTRIBUTION, before any timing: each arm must actually take the
			// path it claims, or the comparison measures nothing.
			probe := hydOpen(b, dir)
			hydrated, rebuilt := probe.eng.recoveredIdx.hydrated, probe.eng.recoveredIdx.rebuilt
			probe.close()
			if arm.epoch == 0 && (hydrated != 0 || rebuilt == 0) {
				b.Fatalf("pre-epoch arm: hydrated/rebuilt = %d/%d, want 0/non-zero", hydrated, rebuilt)
			}
			if arm.epoch != 0 && (hydrated == 0 || rebuilt != 0) {
				b.Fatalf("current-epoch arm: hydrated/rebuilt = %d/%d, want non-zero/0", hydrated, rebuilt)
			}

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				s := hydOpen(b, dir)
				s.close()
			}
		})
	}
}
