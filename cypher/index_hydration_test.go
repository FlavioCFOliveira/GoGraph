package cypher

// index_hydration_test.go — the behavioural gate for rmp #2490: a recovered
// secondary index is loaded from its snapshot payload when — and ONLY when —
// that payload still describes the recovered graph.
//
// Every test here drives the PUBLIC path end to end: a real WAL, a real
// checkpoint (so the WAL prefix is genuinely truncated and the snapshot is the
// only durable source of the folded state), a real reopen through
// recovery.Open, and the real engine constructor. The oracles are the index
// CONTENTS (an exact id set read straight out of the registered index, so the
// planner cannot mask a wrong index with a scan) plus the per-engine
// hydrated/rebuilt counters, asserted in BOTH directions.
//
// Before this change none of it could happen: recovery's index loader was
// unreachable (it required an index.Manager on a graph recovery never attaches
// one to), so `Result.SnapshotIndexes` was provably always 0 and every reopen
// rebuilt every index by scanning the whole mapper.
//
// Layer: short.

import (
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	indexbtree "github.com/FlavioCFOliveira/GoGraph/graph/index/btree"
	indexhash "github.com/FlavioCFOliveira/GoGraph/graph/index/hash"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/checkpoint"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/snapshot"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// ─── harness ────────────────────────────────────────────────────────────────

func hydRecOpts() recovery.Options[string, float64] {
	return recovery.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	}
}

func hydStoreOpts() txn.Options[string, float64] {
	return txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	}
}

// hydSession is one open→work→close cycle over a store directory: exactly what
// one process lifetime does, so a test that wants "and then it restarted" simply
// closes one session and opens the next.
type hydSession struct {
	t   testing.TB
	dir string
	w   *wal.Writer
	st  *txn.Store[string, float64]
	eng *Engine
	res recovery.Result[string, float64]
	mu  sync.Mutex
}

// hydOpen opens dir through the recommended constructor
// ([NewEngineWithStoreAndRecovery]), which is what a production reopen uses.
func hydOpen(t testing.TB, dir string) *hydSession {
	t.Helper()
	return hydOpenWith(t, dir, nil)
}

// hydOpenWith opens dir, optionally substituting the payload source the engine
// is built with. wrap is the seam every falsification arm below uses: returning a
// source that answers WALSuffixTouchesNodeIndex with a constant false reproduces
// a build that hydrates UNCONDITIONALLY, which is what makes the staleness gate
// a test with teeth rather than a test that cannot fail.
func hydOpenWith(t testing.TB, dir string, wrap func(recovery.Result[string, float64]) IndexPayloadSource) *hydSession {
	t.Helper()
	res, err := recovery.Open[string, float64](dir, hydRecOpts())
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	st := res.NewStore(w, hydStoreOpts())
	s := &hydSession{t: t, dir: dir, w: w, st: st, res: res}
	if wrap == nil {
		s.eng = NewEngineWithStoreAndRecovery(st, res)
		return s
	}
	s.eng = NewEngineWithOptions(st.Graph(), EngineOptions{
		Store:                  st,
		RecoveredConstraints:   ConstraintDefsFromRecovery(res.Constraints),
		RecoveredIndexes:       IndexDefsFromRecovery(res.Indexes),
		RecoveredIndexPayloads: wrap(res),
	})
	return s
}

// write runs a write query to completion, failing the test on any error.
func (s *hydSession) write(q string) {
	s.t.Helper()
	r, err := s.eng.RunInTxAny(context.Background(), q, nil)
	if err != nil {
		s.t.Fatalf("RunInTxAny(%q): %v", q, err)
	}
	for r.Next() { //nolint:revive // drain to commit the write
	}
	rerr := r.Err()
	if cerr := r.Close(); cerr != nil && rerr == nil {
		rerr = cerr
	}
	if rerr != nil {
		s.t.Fatalf("write %q: %v", q, rerr)
	}
}

// rows runs a read query and returns the named column of every row, sorted, so a
// caller can compare an exact multiset without depending on row order.
func (s *hydSession) rows(q, col string) []string {
	s.t.Helper()
	r, err := s.eng.Run(context.Background(), q, nil)
	if err != nil {
		s.t.Fatalf("Run(%q): %v", q, err)
	}
	var out []string
	for r.Next() {
		v, ok := r.Record()[col]
		if !ok {
			s.t.Fatalf("Run(%q): row has no column %q", q, col)
		}
		out = append(out, exprToString(s.t, v))
	}
	if rerr := r.Err(); rerr != nil {
		_ = r.Close()
		s.t.Fatalf("Run(%q): %v", q, rerr)
	}
	if cerr := r.Close(); cerr != nil {
		s.t.Fatalf("Close(%q): %v", q, cerr)
	}
	sort.Strings(out)
	return out
}

func exprToString(t testing.TB, v any) string {
	t.Helper()
	switch x := v.(type) {
	case string:
		return x
	case expr.StringValue:
		return string(x)
	default:
		t.Fatalf("column value %#v (%T) is not a string", v, v)
		return ""
	}
}

// mustSeekPlan fails unless q's plan uses op — the guard that keeps every
// row-level assertion below from passing through a full scan that would be
// correct even with a broken index.
func (s *hydSession) mustSeekPlan(q, op string) {
	s.t.Helper()
	plan, err := s.eng.Explain(q, nil)
	if err != nil {
		s.t.Fatalf("Explain(%q): %v", q, err)
	}
	if !strings.Contains(plan, op) {
		s.t.Fatalf("plan for %q does not use %s, so a row assertion over it would not exercise "+
			"the index at all:\n%s", q, op, plan)
	}
}

// checkpoint takes ONE real checkpoint: capture under the commit lock, publish a
// self-sufficient snapshot, prefix-truncate the WAL. The constraint and
// index-definition specs are wired because without them the snapshot is judged
// not self-sufficient and the WAL prefix is RETAINED — which would leave every
// payload stale and make the hydration tests silently vacuous.
func (s *hydSession) checkpoint() {
	s.t.Helper()
	cp := checkpoint.New[string, float64](checkpoint.Config{Dir: s.dir}, s.st.Graph(), s.w, &s.mu,
		checkpoint.WithCommitSerialiser[string, float64](s.st.RunUnderCommitLock),
		checkpoint.WithMapperCodec[string, float64](s.st.Codec()),
		checkpoint.WithWeightCodec[string, float64](s.st.WeightCodec()),
		checkpoint.WithConstraintSpecs[string, float64](s.eng.ConstraintSpecsForSnapshot),
		checkpoint.WithIndexSpecs[string, float64](s.eng.IndexSpecsForSnapshot))
	if err := cp.RunCheckpoint(); err != nil {
		s.t.Fatalf("RunCheckpoint: %v", err)
	}
}

// close ends the session the way a clean shutdown does.
func (s *hydSession) close() {
	s.t.Helper()
	if err := s.w.Close(); err != nil {
		s.t.Fatalf("wal.Close: %v", err)
	}
}

// hydManifest reads back the published manifest.
func hydManifest(t testing.TB, dir string) snapshot.Manifest {
	t.Helper()
	loaded, err := snapshot.LoadSnapshotFull(filepath.Join(dir, "snapshot"))
	if err != nil {
		t.Fatalf("LoadSnapshotFull: %v", err)
	}
	return loaded.Manifest
}

// hydPayloadFiles lists the index payload files a snapshot published.
func hydPayloadFiles(t testing.TB, dir string) []string {
	t.Helper()
	idxDir := filepath.Join(dir, "snapshot", snapshot.IndexesDir)
	ents, err := os.ReadDir(idxDir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", idxDir, err)
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		out = append(out, filepath.Join(idxDir, e.Name()))
	}
	sort.Strings(out)
	return out
}

// hydFlipTrailer flips the last byte of path, breaking the CRC32C the manifest
// recorded for it. It verifies the write actually changed the bytes: a no-op
// corruption would make every arm that depends on it vacuous.
func hydFlipTrailer(t testing.TB, path string) {
	t.Helper()
	before, err := os.ReadFile(path) //nolint:gosec // path under t.TempDir()
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	if len(before) == 0 {
		t.Fatalf("%s is empty, so flipping its trailer corrupts nothing", path)
	}
	after := append([]byte(nil), before...)
	after[len(after)-1] ^= 0xFF
	if err := os.WriteFile(path, after, 0o600); err != nil { //nolint:gosec // path under t.TempDir()
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
	readback, err := os.ReadFile(path) //nolint:gosec // path under t.TempDir()
	if err != nil {
		t.Fatalf("ReadFile(%s) after corruption: %v", path, err)
	}
	if reflect.DeepEqual(before, readback) {
		t.Fatalf("%s is byte-identical after the flip: the corruption did not land", path)
	}
}

// hydReplacePayloadKeepingManifestValid overwrites the named index payload with
// bytes that carry a VALID inner CRC32C trailer and a VALID manifest CRC32C — it
// rewrites manifest.json to match — but a header the index implementation
// rejects. It is the only way to reach the "payload survived every checksum and
// the index still refused it" branch, which is the documented
// inner-format-version-bump case: the manifest CRC covers the whole file, so
// simply flipping a byte can never produce it.
func hydReplacePayloadKeepingManifestValid(t testing.TB, dir, name string) {
	t.Helper()
	snapDir := filepath.Join(dir, "snapshot")
	loaded, err := snapshot.LoadSnapshotFull(snapDir)
	if err != nil {
		t.Fatalf("LoadSnapshotFull: %v", err)
	}
	// A body with a header no index accepts (all-ones magic), plus the inner
	// CRC32C trailer every payload format terminates with, so the index's own
	// checksum passes and its MAGIC check is what refuses the payload.
	castagnoli := crc32.MakeTable(crc32.Castagnoli)
	// Header layout shared by the hash and btree payload formats: a 4-byte magic,
	// a 4-byte format version, then an 8-byte entry count. The magic below matches
	// no index kind, so every implementation refuses it at its first check.
	body := []byte{
		0xFF, 0xFF, 0xFF, 0xFF,
		0x01, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
	payload := make([]byte, 0, len(body)+4)
	payload = append(payload, body...)
	var trailer [4]byte
	binary.LittleEndian.PutUint32(trailer[:], crc32.Checksum(body, castagnoli))
	payload = append(payload, trailer[:]...)

	path := filepath.Join(snapDir, snapshot.IndexesDir, name+".bin")
	if err := os.WriteFile(path, payload, 0o600); err != nil { //nolint:gosec // path under t.TempDir()
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
	// Re-stamp the manifest so the SNAPSHOT-level checks all pass and the only
	// thing left to refuse the payload is the index itself.
	m := loaded.Manifest
	patched := false
	for i := range m.Indexes {
		if m.Indexes[i].Name == name {
			m.Indexes[i].Size = int64(len(payload))
			m.Indexes[i].CRC32C = crc32.Checksum(payload, castagnoli)
			patched = true
		}
	}
	if !patched {
		t.Fatalf("manifest declares no index payload named %q", name)
	}
	mf, err := os.Create(filepath.Join(snapDir, "manifest.json")) //nolint:gosec // path under t.TempDir()
	if err != nil {
		t.Fatalf("Create manifest.json: %v", err)
	}
	if werr := snapshot.WriteManifest(mf, m); werr != nil {
		_ = mf.Close()
		t.Fatalf("WriteManifest: %v", werr)
	}
	if cerr := mf.Close(); cerr != nil {
		t.Fatalf("Close manifest.json: %v", cerr)
	}
}

// expectedIDs is the INDEPENDENT oracle: a full label scan of the recovered
// graph, returning the exact ids of the live nodes carrying label whose prop
// value satisfies match. It reads the graph directly and never touches an index,
// so comparing an index lookup against it is a genuine agreement check between
// the derived structure and its source — the same shape Memgraph's durability
// tests use (scan through the index, assert the exact GID set, with a non-zero
// expected count).
func (s *hydSession) expectedIDs(label, prop string, match func(lpg.PropertyValue) bool) []uint64 {
	s.t.Helper()
	g := s.st.Graph()
	var out []uint64
	g.AdjList().Mapper().Walk(func(id graph.NodeID, key string) bool {
		if g.IsTombstoned(id) {
			return true
		}
		if !g.HasNodeLabel(key, label) {
			return true
		}
		pv, ok := g.GetNodeProperty(key, prop)
		if !ok {
			return true
		}
		if match(pv) {
			out = append(out, uint64(id))
		}
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// eqString matches a string property value equal to want.
func eqString(want string) func(lpg.PropertyValue) bool {
	return func(pv lpg.PropertyValue) bool {
		got, ok := pv.String()
		return ok && got == want
	}
}

// intAbove matches an integer property value strictly greater than lo.
func intAbove(lo int64) func(lpg.PropertyValue) bool {
	return func(pv lpg.PropertyValue) bool {
		got, ok := pv.Int64()
		return ok && got > lo
	}
}

// nonEmptyIDs fails the test when want is empty: an "index equals oracle"
// assertion where both sides are empty holds for a completely broken index, so
// every positive comparison must first prove the oracle found something.
func (s *hydSession) nonEmptyIDs(what string, want []uint64) []uint64 {
	s.t.Helper()
	if len(want) == 0 {
		s.t.Fatalf("the independent scan found no node for %s, so comparing the index against it "+
			"would pass for an empty index", what)
	}
	return want
}

// hashIndexIDs reads the exact id set the registered hash index holds for value.
func (s *hydSession) hashIndexIDs(name, value string) []uint64 {
	s.t.Helper()
	sub, err := s.st.Graph().IndexManager().GetIndex(name)
	if err != nil {
		s.t.Fatalf("GetIndex(%q): %v", name, err)
	}
	idx, ok := sub.(*indexhash.Index[string])
	if !ok {
		s.t.Fatalf("index %q is a %T, want *hash.Index[string]", name, sub)
	}
	got := idx.Lookup(value).ToArray()
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	return got
}

// numericIndexIDs reads the exact id set the registered numeric companion btree
// holds for the half-open range [lo, hi).
func (s *hydSession) numericIndexIDs(name string, lo, hi float64) []uint64 {
	s.t.Helper()
	sub, err := s.st.Graph().IndexManager().GetIndex(name)
	if err != nil {
		s.t.Fatalf("GetIndex(%q): %v", name, err)
	}
	idx, ok := sub.(*indexbtree.Index[float64])
	if !ok {
		s.t.Fatalf("index %q is a %T, want *btree.Index[float64]", name, sub)
	}
	got := idx.Range(lo, hi).ToArray()
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	return got
}

// alwaysFreshSource is the falsification seam: it is a real recovery Result in
// every respect EXCEPT that it claims the replayed WAL suffix touched nothing, so
// an engine built over it hydrates unconditionally. It is how the staleness gate
// is shown to have teeth rather than being asserted to.
type alwaysFreshSource struct {
	recovery.Result[string, float64]
}

func (alwaysFreshSource) WALSuffixTouchesNodeIndex(string, string) bool { return false }

func freshWrap(res recovery.Result[string, float64]) IndexPayloadSource {
	return alwaysFreshSource{res}
}

// hydSeedPeople writes the fixture every test starts from: one hash index on
// (:Person, name), one btree index on (:Person, age), and four people.
func hydSeedPeople(s *hydSession) {
	s.write(`CREATE INDEX person_name FOR (n:Person) ON (n.name)`)
	s.write(`CREATE INDEX person_age FOR (n:Person) ON (n.age) OPTIONS {indexType: 'btree'}`)
	s.write(`CREATE (:Person {name: 'alice', age: 41})`)
	s.write(`CREATE (:Person {name: 'bob',   age: 22})`)
	s.write(`CREATE (:Person {name: 'carol', age: 55})`)
	s.write(`CREATE (:Person {name: 'dave',  age: 33})`)
}

// ─── 1. the positive path ───────────────────────────────────────────────────

// TestIndexHydration_CheckpointedReopen_SeeksExactRows is the headline gate. A
// graph with a hash index and a btree index is checkpointed so the WAL prefix is
// truncated and the suffix is empty; the reopen must then load BOTH indexes from
// their snapshot payloads and every seek must return the exact expected id set.
//
// It fails on the pre-#2490 tree because Result.SnapshotIndexes was 0 there for
// every possible input: the loader behind it required an index.Manager on the
// recovered graph and recovery never attaches one.
func TestIndexHydration_CheckpointedReopen_SeeksExactRows(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	s1 := hydOpen(t, dir)
	hydSeedPeople(s1)
	// Bulk filler with an age BELOW the range predicate. It exists only so the
	// planner's range-seek gate admits the seek: that gate needs the label
	// population at or above rangeSeekMinLabelPopulation AND the in-range
	// count within rangeSeekMaxSelectivity (10%) of it. On the four-person fixture
	// alone the plan is a label scan, and a row assertion over a scan would be
	// correct even with a wholly broken index — so without this the end-to-end
	// oracle below could not fail.
	s1.write(`UNWIND range(1, 2000) AS i CREATE (:Person {name: 'filler', age: 20})`)
	s1.checkpoint()
	s1.close()

	// The manifest must carry the watermark, or the reopen below would refuse to
	// hydrate for a reason unrelated to what this test is measuring.
	m := hydManifest(t, dir)
	if m.IndexesCommitTS == 0 {
		t.Fatalf("manifest carries no indexes_commit_ts after a real checkpoint, so hydration is "+
			"impossible and every assertion below would pass vacuously; manifest=%+v", m)
	}
	if len(m.Indexes) == 0 {
		t.Fatal("the checkpoint published no index payloads, so there is nothing to hydrate")
	}

	s2 := hydOpen(t, dir)
	defer s2.close()

	// The suffix must be empty: that is the state in which hydration is legal,
	// and asserting it makes the staleness gate's own test (below) meaningful by
	// contrast.
	if s2.res.WALOps != 0 {
		t.Fatalf("WALOps = %d after a full checkpoint, want 0: the prefix was not truncated, "+
			"so this is not the empty-suffix case", s2.res.WALOps)
	}
	if !s2.res.SnapshotSelfSufficient {
		t.Fatal("SnapshotSelfSufficient = false after a real checkpoint")
	}
	if s2.res.SnapshotIndexes != len(m.Indexes) {
		t.Fatalf("SnapshotIndexes = %d, want %d (every published payload must be certified "+
			"hydratable on a clean checkpointed reopen)", s2.res.SnapshotIndexes, len(m.Indexes))
	}

	// Path attribution: every index came from its payload, none from a scan.
	if s2.eng.recoveredIdx.hydrated != len(m.Indexes) || s2.eng.recoveredIdx.rebuilt != 0 {
		t.Fatalf("hydrated/rebuilt = %d/%d, want %d/0",
			s2.eng.recoveredIdx.hydrated, s2.eng.recoveredIdx.rebuilt, len(m.Indexes))
	}
	if s2.eng.recoveredIdx.backfillNodes != 0 {
		t.Fatalf("backfillNodes = %d, want 0: nothing should have been scanned",
			s2.eng.recoveredIdx.backfillNodes)
	}

	// CONTENTS, read straight out of the registered indexes so the planner cannot
	// substitute a scan for a wrong index.
	want := s2.nonEmptyIDs("(Person, name) = 'carol'", s2.expectedIDs("Person", "name", eqString("carol")))
	if got := s2.hashIndexIDs("person_name", "carol"); !reflect.DeepEqual(got, want) {
		t.Errorf("hydrated hash index person_name['carol'] = %v, want %v", got, want)
	}
	if got := s2.hashIndexIDs("person_name", "nobody"); len(got) != 0 {
		t.Errorf("hydrated hash index person_name['nobody'] = %v, want empty", got)
	}
	// age > 30 ⇒ alice(41), carol(55), dave(33).
	wantRange := s2.nonEmptyIDs("(Person, age) > 30", s2.expectedIDs("Person", "age", intAbove(30)))
	if got := s2.numericIndexIDs(numericBTreeName("Person", "age"), 30.000001, 1e18); !reflect.DeepEqual(got, wantRange) {
		t.Errorf("hydrated numeric companion for (Person, age) over (30, ∞) = %v, want %v", got, wantRange)
	}

	// End to end, through plans that genuinely use the indexes.
	eq := `MATCH (n:Person) WHERE n.name = 'carol' RETURN n.name AS name`
	s2.mustSeekPlan(eq, "NodeByIndexSeek")
	if got, want := s2.rows(eq, "name"), []string{"carol"}; !reflect.DeepEqual(got, want) {
		t.Errorf("%s = %v, want %v", eq, got, want)
	}
	rng := `MATCH (n:Person) WHERE n.age > 30 RETURN n.name AS name`
	s2.mustSeekPlan(rng, "NodeByIndexRangeScan")
	if got, want := s2.rows(rng, "name"), []string{"alice", "carol", "dave"}; !reflect.DeepEqual(got, want) {
		t.Errorf("%s = %v, want %v", rng, got, want)
	}
}

// ─── 2. path attribution, both directions ───────────────────────────────────

// TestIndexHydration_PathAttribution_BothDirections asserts the counters in both
// directions over the SAME fixture: a clean checkpointed reopen hydrates
// everything and scans nothing, and the same directory with every payload
// corrupted hydrates nothing and scans for everything. A counter only ever
// observed going one way proves nothing.
func TestIndexHydration_PathAttribution_BothDirections(t *testing.T) {
	t.Parallel()

	build := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		s := hydOpen(t, dir)
		hydSeedPeople(s)
		s.checkpoint()
		s.close()
		return dir
	}

	t.Run("clean", func(t *testing.T) {
		t.Parallel()
		dir := build(t)
		n := len(hydManifest(t, dir).Indexes)
		if n == 0 {
			t.Fatal("no payloads published")
		}
		s := hydOpen(t, dir)
		defer s.close()
		if s.eng.recoveredIdx.hydrated != n {
			t.Errorf("hydrated = %d, want %d", s.eng.recoveredIdx.hydrated, n)
		}
		if s.eng.recoveredIdx.rebuilt != 0 {
			t.Errorf("rebuilt = %d, want 0", s.eng.recoveredIdx.rebuilt)
		}
		if s.eng.recoveredIdx.payloadUnreadable != 0 || s.eng.recoveredIdx.payloadCorrupted != 0 {
			t.Errorf("payload faults = %d unreadable / %d corrupted, want 0/0",
				s.eng.recoveredIdx.payloadUnreadable, s.eng.recoveredIdx.payloadCorrupted)
		}
	})

	t.Run("every payload corrupted", func(t *testing.T) {
		t.Parallel()
		dir := build(t)
		files := hydPayloadFiles(t, dir)
		if len(files) == 0 {
			t.Fatal("no payload files to corrupt")
		}
		for _, f := range files {
			hydFlipTrailer(t, f)
		}
		s := hydOpen(t, dir)
		defer s.close()
		if s.eng.recoveredIdx.hydrated != 0 {
			t.Errorf("hydrated = %d, want 0 with every payload corrupted", s.eng.recoveredIdx.hydrated)
		}
		if s.eng.recoveredIdx.rebuilt != len(files) {
			t.Errorf("rebuilt = %d, want %d", s.eng.recoveredIdx.rebuilt, len(files))
		}
		if s.eng.recoveredIdx.payloadUnreadable != len(files) {
			t.Errorf("payloadUnreadable = %d, want %d: a flipped trailer must be caught by the "+
				"manifest CRC and reported, not silently ignored",
				s.eng.recoveredIdx.payloadUnreadable, len(files))
		}
		if s.eng.recoveredIdx.backfillNodes == 0 {
			t.Error("backfillNodes = 0 although every index was rebuilt: the rebuild did no work, " +
				"so the counter cannot distinguish the two paths")
		}
		if s.res.SnapshotIndexes != 0 {
			t.Errorf("SnapshotIndexes = %d, want 0 with every payload corrupted", s.res.SnapshotIndexes)
		}
		// Rebuilt indexes still answer correctly — corruption costs the
		// optimisation, never the result.
		want := s.nonEmptyIDs("(Person, name) = 'bob'", s.expectedIDs("Person", "name", eqString("bob")))
		if got := s.hashIndexIDs("person_name", "bob"); !reflect.DeepEqual(got, want) {
			t.Errorf("rebuilt hash index person_name['bob'] = %v, want %v", got, want)
		}
	})
}

// ─── 3. the staleness gate ──────────────────────────────────────────────────

// TestIndexHydration_StalenessGate is the load-bearing test. After the
// checkpoint, MORE mutations land on the indexed (label, property) and the
// process crashes without a second checkpoint, so the payload on disk describes a
// state the graph has left. The reopen must rebuild and return the POST-checkpoint
// row set exactly.
//
// The `hydrates unconditionally` sub-test is the falsification arm: it is the
// identical directory recovered through a source that reports the suffix as
// touching nothing — i.e. a build with the gate removed — and it must produce
// the STALE answer. Without it, the gate could be permanently closed (or absent)
// and the first sub-test would still pass.
//
// The `negative control` sub-test closes the other side: mutations to a
// DIFFERENT (label, property) must leave hydration switched ON, so the gate
// cannot pass by simply refusing everything.
func TestIndexHydration_StalenessGate(t *testing.T) {
	t.Parallel()

	// stale builds a directory whose snapshot payload is one commit behind: after
	// the checkpoint, `erin` is added and `bob` is renamed.
	stale := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		s1 := hydOpen(t, dir)
		hydSeedPeople(s1)
		s1.checkpoint()
		// Post-checkpoint, un-checkpointed mutations to the INDEXED property.
		s1.write(`CREATE (:Person {name: 'erin', age: 29})`)
		s1.write(`MATCH (n:Person {name: 'bob'}) SET n.name = 'robert'`)
		s1.close()
		return dir
	}

	t.Run("gate refuses the stale payload", func(t *testing.T) {
		t.Parallel()
		dir := stale(t)
		s := hydOpen(t, dir)
		defer s.close()

		// Non-vacuity: the suffix must genuinely have touched the indexed facets.
		if s.res.WALOps == 0 {
			t.Fatal("WALOps = 0: nothing was committed after the checkpoint, so there is no staleness to gate")
		}
		if !s.res.WALSuffixTouchesNodeIndex("Person", "name") {
			t.Fatalf("the suffix does not report touching (Person, name); labels=%q keys=%q",
				s.res.WALTouchedNodeLabels, s.res.WALTouchedNodePropertyKeys)
		}
		if s.eng.recoveredIdx.hydrated != 0 {
			t.Errorf("hydrated = %d, want 0: the suffix rewrote the indexed property",
				s.eng.recoveredIdx.hydrated)
		}
		if s.eng.recoveredIdx.rebuilt == 0 {
			t.Error("rebuilt = 0: nothing was rebuilt, so the index came from nowhere")
		}

		// EXACT post-checkpoint multiset, not a count.
		want := []string{"alice", "carol", "dave", "erin", "robert"}
		q := `MATCH (n:Person) WHERE n.name >= '' RETURN n.name AS name`
		if got := s.rows(q, "name"); !reflect.DeepEqual(got, want) {
			t.Errorf("post-crash name set = %v, want %v", got, want)
		}
		// And the index itself: `robert` present, `bob` gone.
		wRobert := s.nonEmptyIDs("(Person, name) = 'robert'", s.expectedIDs("Person", "name", eqString("robert")))
		if got := s.hashIndexIDs("person_name", "robert"); !reflect.DeepEqual(got, wRobert) {
			t.Errorf("person_name['robert'] = %v, want %v", got, wRobert)
		}
		if got := s.hashIndexIDs("person_name", "bob"); len(got) != 0 {
			t.Errorf("person_name['bob'] = %v, want empty: the rename was committed after the checkpoint", got)
		}
		wErin := s.nonEmptyIDs("(Person, name) = 'erin'", s.expectedIDs("Person", "name", eqString("erin")))
		if got := s.hashIndexIDs("person_name", "erin"); !reflect.DeepEqual(got, wErin) {
			t.Errorf("person_name['erin'] = %v, want %v", got, wErin)
		}
	})

	t.Run("without the gate the answer is WRONG", func(t *testing.T) {
		t.Parallel()
		dir := stale(t)
		s := hydOpenWith(t, dir, freshWrap)
		defer s.close()

		if s.eng.recoveredIdx.hydrated == 0 {
			t.Fatal("the falsification arm hydrated nothing, so it does not reproduce a " +
				"gate-less build and cannot show the gate has teeth")
		}
		// The stale payload predates the rename and the insert, so the index
		// disagrees with the graph in BOTH directions.
		if got := s.hashIndexIDs("person_name", "bob"); len(got) == 0 {
			t.Error("hydrating unconditionally did NOT resurrect the pre-rename key 'bob': " +
				"the payload is not actually stale, so the gate test above is vacuous")
		}
		if got := s.hashIndexIDs("person_name", "erin"); len(got) != 0 {
			t.Error("hydrating unconditionally already knew 'erin', which was committed after " +
				"the payload was captured: the payload is not actually stale")
		}
	})

	t.Run("negative control: an unrelated facet still hydrates", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		s1 := hydOpen(t, dir)
		hydSeedPeople(s1)
		s1.checkpoint()
		// Commit to a DIFFERENT label and a DIFFERENT property key, then crash.
		s1.write(`CREATE (:Company {title: 'acme'})`)
		s1.close()

		s := hydOpen(t, dir)
		defer s.close()
		if s.res.WALOps == 0 {
			t.Fatal("WALOps = 0: the control committed nothing after the checkpoint, so it " +
				"does not distinguish an always-closed gate from a working one")
		}
		if s.res.WALSuffixTouchesNodeIndex("Person", "name") {
			t.Fatalf("the suffix wrongly reports touching (Person, name); labels=%q keys=%q",
				s.res.WALTouchedNodeLabels, s.res.WALTouchedNodePropertyKeys)
		}
		if s.eng.recoveredIdx.hydrated == 0 {
			t.Fatalf("hydrated = 0 although the suffix touched neither the indexed label nor the "+
				"indexed property: the gate is permanently closed, which makes every other "+
				"hydration assertion vacuous (rebuilt=%d)", s.eng.recoveredIdx.rebuilt)
		}
		want := s.nonEmptyIDs("(Person, name) = 'carol'", s.expectedIDs("Person", "name", eqString("carol")))
		if got := s.hashIndexIDs("person_name", "carol"); !reflect.DeepEqual(got, want) {
			t.Errorf("person_name['carol'] = %v, want %v", got, want)
		}
	})
}

// ─── 4. the mapper-less path ────────────────────────────────────────────────

// TestIndexHydration_MapperlessSnapshotNeverHydrates covers the first
// precondition. A snapshot captured while the graph held NO nodes carries an
// empty mapper, so recovery re-derives every node id by interning during replay
// and the raw uint64 ids inside a payload name nothing. Every payload must
// therefore be reported stale, every index rebuilt, and every seek still correct.
//
// Attribution is exact because indexImageReason checks self-sufficiency FIRST:
// the reported reason names the mapper, not the watermark, whatever the watermark
// happens to be.
func TestIndexHydration_MapperlessSnapshotNeverHydrates(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	s1 := hydOpen(t, dir)
	// Indexes exist, so payloads are published, but the graph is EMPTY, so
	// mapper.bin carries no entries.
	s1.write(`CREATE INDEX person_name FOR (n:Person) ON (n.name)`)
	s1.checkpoint()
	// Now populate, and crash without a second checkpoint.
	s1.write(`CREATE (:Person {name: 'alice', age: 41})`)
	s1.write(`CREATE (:Person {name: 'bob', age: 22})`)
	s1.close()

	if len(hydManifest(t, dir).Indexes) == 0 {
		t.Fatal("the checkpoint published no index payloads, so this test cannot observe a stale one")
	}

	s := hydOpen(t, dir)
	defer s.close()

	if s.res.SnapshotSelfSufficient {
		t.Fatal("SnapshotSelfSufficient = true for a snapshot whose mapper carries no entries")
	}
	if len(s.res.SnapshotIndexPayloads) == 0 {
		t.Fatal("no payloads reported, so the per-payload assertions below are vacuous")
	}
	for _, p := range s.res.SnapshotIndexPayloads {
		if !errors.Is(p.Err, recovery.ErrIndexPayloadStale) {
			t.Errorf("payload %q Err = %v, want ErrIndexPayloadStale", p.Name, p.Err)
		}
		if !strings.Contains(p.Err.Error(), "not self-sufficient") {
			t.Errorf("payload %q Err = %q, want the mapper-less reason: the precedence between the "+
				"two staleness causes is what attributes this test", p.Name, p.Err)
		}
	}
	if s.res.SnapshotIndexes != 0 {
		t.Errorf("SnapshotIndexes = %d, want 0", s.res.SnapshotIndexes)
	}
	if s.eng.recoveredIdx.hydrated != 0 {
		t.Errorf("hydrated = %d, want 0 on a mapper-less image", s.eng.recoveredIdx.hydrated)
	}
	if s.eng.recoveredIdx.rebuilt == 0 {
		t.Error("rebuilt = 0: the indexes were neither hydrated nor rebuilt")
	}
	// Correct answers regardless.
	want := s.nonEmptyIDs("(Person, name) = 'alice'", s.expectedIDs("Person", "name", eqString("alice")))
	if got := s.hashIndexIDs("person_name", "alice"); !reflect.DeepEqual(got, want) {
		t.Errorf("rebuilt person_name['alice'] = %v, want %v", got, want)
	}
	q := `MATCH (n:Person) WHERE n.name = 'bob' RETURN n.name AS name`
	s.mustSeekPlan(q, "NodeByIndexSeek")
	if got, want := s.rows(q, "name"), []string{"bob"}; !reflect.DeepEqual(got, want) {
		t.Errorf("%s = %v, want %v", q, got, want)
	}
}

// ─── 5. partial hydration is per index ──────────────────────────────────────

// TestIndexHydration_PartialCorruption_IsPerIndex promotes the old
// "corrupt index logs a warning" assertion to a behavioural one: with three
// indexes and exactly ONE payload's trailer flipped, the two intact indexes are
// hydrated, the damaged one is rebuilt, and ALL THREE return correct rows. The
// fallback is per index, never per snapshot, and never fail-stop.
func TestIndexHydration_PartialCorruption_IsPerIndex(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	s1 := hydOpen(t, dir)
	s1.write(`CREATE INDEX person_name FOR (n:Person) ON (n.name)`)
	s1.write(`CREATE INDEX person_city FOR (n:Person) ON (n.city)`)
	s1.write(`CREATE INDEX person_email FOR (n:Person) ON (n.email)`)
	s1.write(`CREATE (:Person {name: 'alice', city: 'lisbon', email: 'a@x'})`)
	s1.write(`CREATE (:Person {name: 'bob', city: 'porto', email: 'b@x'})`)
	s1.checkpoint()
	s1.close()

	total := len(hydManifest(t, dir).Indexes)
	if total < 3 {
		t.Fatalf("only %d payloads published, want at least the three user indexes", total)
	}
	target := filepath.Join(dir, "snapshot", snapshot.IndexesDir, "person_city.bin")
	hydFlipTrailer(t, target)

	s := hydOpen(t, dir)
	defer s.close()

	// Exactly one payload refused, and it is the one that was damaged.
	if s.res.SnapshotIndexes != total-1 {
		t.Fatalf("SnapshotIndexes = %d, want %d (exactly one payload damaged)", s.res.SnapshotIndexes, total-1)
	}
	b, err := s.res.IndexPayloadFor("person_city")
	if b != nil || !errors.Is(err, recovery.ErrIndexPayloadUnreadable) {
		t.Fatalf("IndexPayloadFor(person_city) = (%d bytes, %v), want (nil, ErrIndexPayloadUnreadable)", len(b), err)
	}
	if s.eng.recoveredIdx.hydrated != total-1 {
		t.Errorf("hydrated = %d, want %d", s.eng.recoveredIdx.hydrated, total-1)
	}
	if s.eng.recoveredIdx.rebuilt != 1 {
		t.Errorf("rebuilt = %d, want exactly 1: the fallback must be per index, not per snapshot",
			s.eng.recoveredIdx.rebuilt)
	}
	if s.eng.recoveredIdx.payloadUnreadable != 1 {
		t.Errorf("payloadUnreadable = %d, want 1: the fallback must not be silent",
			s.eng.recoveredIdx.payloadUnreadable)
	}

	// All three answer correctly — the hydrated pair and the rebuilt one alike.
	for _, tc := range []struct{ idx, prop, value string }{
		{"person_name", "name", "alice"},
		{"person_city", "city", "porto"},
		{"person_email", "email", "a@x"},
	} {
		want := s.nonEmptyIDs("(Person, "+tc.prop+") = "+tc.value,
			s.expectedIDs("Person", tc.prop, eqString(tc.value)))
		if got := s.hashIndexIDs(tc.idx, tc.value); !reflect.DeepEqual(got, want) {
			t.Errorf("%s[%q] = %v, want %v", tc.idx, tc.value, got, want)
		}
	}
}

// TestIndexHydration_CRCValidPayloadRejectedByTheIndex covers the second
// corruption surface, the one a byte flip cannot reach: a payload that passes the
// manifest CRC32C and its own inner CRC trailer, and is still refused by the
// index implementation — the documented inner-format-version-bump case. It must
// take the same per-index rebuild path, be metered as a DISTINCT fault from an
// unreadable payload, and still answer correctly.
func TestIndexHydration_CRCValidPayloadRejectedByTheIndex(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	s1 := hydOpen(t, dir)
	s1.write(`CREATE INDEX person_name FOR (n:Person) ON (n.name)`)
	s1.write(`CREATE INDEX person_city FOR (n:Person) ON (n.city)`)
	s1.write(`CREATE (:Person {name: 'alice', city: 'lisbon'})`)
	s1.write(`CREATE (:Person {name: 'bob', city: 'porto'})`)
	s1.checkpoint()
	s1.close()

	total := len(hydManifest(t, dir).Indexes)
	hydReplacePayloadKeepingManifestValid(t, dir, "person_city")

	s := hydOpen(t, dir)
	defer s.close()

	// Recovery certified it hydratable: every snapshot-level check passed.
	if s.res.SnapshotIndexes != total {
		t.Fatalf("SnapshotIndexes = %d, want %d: the re-stamped payload must pass every "+
			"snapshot-level check, or this test is measuring the unreadable path instead",
			s.res.SnapshotIndexes, total)
	}
	if b, err := s.res.IndexPayloadFor("person_city"); err != nil || len(b) == 0 {
		t.Fatalf("IndexPayloadFor(person_city) = (%d bytes, %v), want bytes and no error", len(b), err)
	}
	// The INDEX refused it, so the engine rebuilt exactly that one.
	if s.eng.recoveredIdx.payloadCorrupted != 1 {
		t.Errorf("payloadCorrupted = %d, want 1", s.eng.recoveredIdx.payloadCorrupted)
	}
	if s.eng.recoveredIdx.payloadUnreadable != 0 {
		t.Errorf("payloadUnreadable = %d, want 0: this fault is not an unreadable payload and "+
			"must not be metered as one", s.eng.recoveredIdx.payloadUnreadable)
	}
	if s.eng.recoveredIdx.rebuilt != 1 {
		t.Errorf("rebuilt = %d, want exactly 1", s.eng.recoveredIdx.rebuilt)
	}
	if s.eng.recoveredIdx.hydrated != total-1 {
		t.Errorf("hydrated = %d, want %d", s.eng.recoveredIdx.hydrated, total-1)
	}
	// Correct rows from both the hydrated and the rebuilt index.
	for _, tc := range []struct{ idx, prop, value string }{
		{"person_name", "name", "alice"},
		{"person_city", "city", "porto"},
	} {
		want := s.nonEmptyIDs("(Person, "+tc.prop+") = "+tc.value,
			s.expectedIDs("Person", tc.prop, eqString(tc.value)))
		if got := s.hashIndexIDs(tc.idx, tc.value); !reflect.DeepEqual(got, want) {
			t.Errorf("%s[%q] = %v, want %v", tc.idx, tc.value, got, want)
		}
	}
}

// ─── 6. registration-order collision ───────────────────────────────────────

// TestIndexHydration_UniqueBackingIndexIsHydrated covers the collision case that
// the registration order creates: registerRecoveredConstraints runs BEFORE
// registerRecoveredIndexes, so a UNIQUE constraint's backing index is the
// incumbent under its own deterministic name, and it is that instance — not a
// rival built afterwards — that must carry the payload and answer queries.
func TestIndexHydration_UniqueBackingIndexIsHydrated(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	s1 := hydOpen(t, dir)
	s1.write(`CREATE CONSTRAINT person_email_unique FOR (n:Person) REQUIRE n.email IS UNIQUE`)
	s1.write(`CREATE (:Person {email: 'a@x'})`)
	s1.write(`CREATE (:Person {email: 'b@x'})`)
	s1.checkpoint()
	s1.close()

	backing := exec.UniqueIndexName("Person", "email")
	m := hydManifest(t, dir)
	found := false
	for _, e := range m.Indexes {
		if e.Name == backing {
			found = true
		}
	}
	if !found {
		t.Fatalf("the checkpoint published no payload named %q, so there is nothing for the "+
			"incumbent to be hydrated from; published=%+v", backing, m.Indexes)
	}

	s := hydOpen(t, dir)
	defer s.close()

	if s.eng.recoveredIdx.hydrated == 0 {
		t.Fatalf("hydrated = 0: the UNIQUE backing index was rebuilt rather than hydrated (rebuilt=%d)",
			s.eng.recoveredIdx.rebuilt)
	}
	// The registered instance is the one the constraint path installed, and it
	// holds the right ids.
	want := s.nonEmptyIDs("(Person, email) = 'a@x'", s.expectedIDs("Person", "email", eqString("a@x")))
	if got := s.hashIndexIDs(backing, "a@x"); !reflect.DeepEqual(got, want) {
		t.Errorf("hydrated %s['a@x'] = %v, want %v", backing, got, want)
	}
	// Enforcement still works: the constraint rejects a duplicate.
	r, err := s.eng.RunInTxAny(context.Background(), `CREATE (:Person {email: 'a@x'})`, nil)
	if err == nil {
		for r.Next() { //nolint:revive // drain
		}
		err = r.Err()
		if cerr := r.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	if err == nil {
		t.Fatal("a duplicate UNIQUE value was accepted after a hydrated reopen")
	}
}

// ─── 7. back-compat / the no-op guarantee ───────────────────────────────────

// TestIndexHydration_BackCompat_NoWatermarkNoHydration pins the format's
// back-compat guarantee: a snapshot written by the present-time writer
// ([snapshot.WriteSnapshotFull], the shape of every snapshot that existed before
// rmp #2490) carries no indexes_commit_ts, so NOTHING is hydrated and every index
// is rebuilt exactly as before.
//
// It also pins that recovery.Options was not touched — the whole design rests on
// existing callers being byte-identical — and that a nil payload source (the zero
// EngineOptions every pre-existing caller carries) preserves backfill-always.
func TestIndexHydration_BackCompat_NoWatermarkNoHydration(t *testing.T) {
	t.Parallel()

	t.Run("recovery.Options is unchanged", func(t *testing.T) {
		t.Parallel()
		got := reflect.TypeOf(recovery.Options[string, float64]{}).NumField()
		if got != 3 {
			t.Fatalf("recovery.Options has %d fields, want 3 (Codec, WeightCodec, MaxTxnOps): the "+
				"design requires every existing recovery caller to stay byte-identical", got)
		}
	})

	t.Run("present-time snapshot hydrates nothing", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		s1 := hydOpen(t, dir)
		hydSeedPeople(s1)
		// Publish through the PRESENT-TIME writer rather than the checkpointer:
		// it captures with a nil instant, so there is no MVCC instant to name and
		// therefore no watermark.
		if err := snapshot.WriteSnapshotFullWithConstraintsAndIndexDefs[string, float64](
			filepath.Join(dir, "snapshot"),
			csr.BuildFromAdjList(s1.st.Graph().AdjList()),
			s1.st.Graph(),
			s1.eng.ConstraintSpecsForSnapshot(),
			s1.eng.IndexSpecsForSnapshot(),
		); err != nil {
			t.Fatalf("WriteSnapshotFullWithConstraintsAndIndexDefs: %v", err)
		}
		s1.close()

		m := hydManifest(t, dir)
		if m.IndexesCommitTS != 0 {
			t.Fatalf("a present-time snapshot published indexes_commit_ts = %d, want none", m.IndexesCommitTS)
		}
		if len(m.Indexes) == 0 {
			t.Fatal("no payloads published, so the no-hydration assertion below is vacuous")
		}

		s := hydOpen(t, dir)
		defer s.close()
		if s.res.SnapshotIndexes != 0 {
			t.Errorf("SnapshotIndexes = %d, want 0 without a watermark", s.res.SnapshotIndexes)
		}
		if s.eng.recoveredIdx.hydrated != 0 {
			t.Errorf("hydrated = %d, want 0 without a watermark", s.eng.recoveredIdx.hydrated)
		}
		if s.eng.recoveredIdx.rebuilt == 0 {
			t.Error("rebuilt = 0: the indexes were neither hydrated nor rebuilt")
		}
		want := s.nonEmptyIDs("(Person, name) = 'dave'", s.expectedIDs("Person", "name", eqString("dave")))
		if got := s.hashIndexIDs("person_name", "dave"); !reflect.DeepEqual(got, want) {
			t.Errorf("rebuilt person_name['dave'] = %v, want %v", got, want)
		}
	})

	t.Run("a nil payload source preserves backfill-always", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		s1 := hydOpen(t, dir)
		hydSeedPeople(s1)
		s1.checkpoint()
		s1.close()

		// The pre-#2490 constructor: schema only, no payloads.
		res, err := recovery.Open[string, float64](dir, hydRecOpts())
		if err != nil {
			t.Fatalf("recovery.Open: %v", err)
		}
		w, err := wal.Open(filepath.Join(dir, "wal"))
		if err != nil {
			t.Fatalf("wal.Open: %v", err)
		}
		defer func() {
			if cerr := w.Close(); cerr != nil {
				t.Errorf("wal.Close: %v", cerr)
			}
		}()
		st := res.NewStore(w, hydStoreOpts())
		eng := NewEngineWithStoreAndSchema(st, res.Constraints, res.Indexes)
		if eng.recoveredIdx.hydrated != 0 {
			t.Errorf("hydrated = %d through NewEngineWithStoreAndSchema, want 0: that constructor "+
				"carries no payloads and must behave exactly as before", eng.recoveredIdx.hydrated)
		}
		if eng.recoveredIdx.rebuilt == 0 {
			t.Error("rebuilt = 0: the indexes were neither hydrated nor rebuilt")
		}
		// Non-vacuity: payloads WERE available on disk, so the zero above is the
		// constructor's choice and not an empty snapshot.
		if res.SnapshotIndexes == 0 {
			t.Fatal("SnapshotIndexes = 0: the directory offered no hydratable payload, so this " +
				"sub-test does not distinguish the two constructors")
		}
	})
}

// ─── 8. hydration cannot happen after publication ──────────────────────────

// TestIndexHydration_PanicsAfterEnginePublished pins the concurrency contract.
// hash.Index.Deserialize swaps its shards one at a time under per-shard locks, so
// it is not atomic across shards and a reader could observe a half-replaced
// index. Hydration is therefore confined to the constructor, and the confinement
// is ENFORCED rather than documented: a call afterwards panics.
//
// Run under -race with the rest of the package; the panic path itself is
// deterministic.
func TestIndexHydration_PanicsAfterEnginePublished(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s1 := hydOpen(t, dir)
	hydSeedPeople(s1)
	s1.checkpoint()
	s1.close()

	s := hydOpen(t, dir)
	defer s.close()
	// Sanity: this engine DID hydrate during construction, so the guard below is
	// refusing a call that would otherwise have done real work.
	if s.eng.recoveredIdx.hydrated == 0 {
		t.Fatal("the engine hydrated nothing during construction, so the post-publication guard " +
			"is being tested on a path that does nothing anyway")
	}

	idx, err := newBoundNodeHashIndex(s.eng.g.ReadAt(nil), "Person", "name")
	if err != nil {
		t.Fatalf("newBoundNodeHashIndex: %v", err)
	}
	rebuilt := false
	func() {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("populateRecoveredIndex after publication did NOT panic; a non-atomic " +
					"shard-by-shard Deserialize on a published index must never be reachable")
			}
			err, ok := r.(error)
			if !ok || !errors.Is(err, errHydrationAfterPublish) {
				t.Fatalf("panic value = %#v, want errHydrationAfterPublish", r)
			}
		}()
		s.eng.populateRecoveredIndex("person_name", "Person", "name", idx, func() { rebuilt = true })
	}()
	if rebuilt {
		t.Fatal("the guard fell through to the rebuild instead of panicking: a silent backfill " +
			"is exactly the degradation the panic exists to prevent")
	}
}
