package hash

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"math/rand/v2"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// The tests in this file exist because of a RECORDED failure, not a
// hypothetical one.
//
// The per-value-entry geometry this package adopted in rmp #2692 was first
// built in the sibling graph/index/label index (rmp #2685). That first
// implementation re-read the SPINE while holding an ENTRY lock, to confirm the
// entry it held was still the published one. The reaper takes the same two
// locks the other way round — spine first, entry second — so the two together
// are an ABBA inversion, and it DEADLOCKED.
//
// What matters more than the bug is why it survived so long:
//
//   - Eight clean throughput sweeps missed it, because their workload only ever
//     CREATED keys. reap never ran, so one half of the inversion was never
//     executed. A workload that exercises one operation cannot validate a lock
//     protocol that spans several.
//   - `go test -race` missed it. A deadlock is not a data race; the detector has
//     nothing to report. It surfaces as a hang, and a hang in a suite is killed
//     by the go test timeout with no attribution.
//
// So these tests drive creation, mutation, reaping, re-creation, reading and
// serialisation CONCURRENTLY, they check the result against an exact sequential
// oracle, and they detect a hang with an explicit watchdog.

// lockOrderWatchdog runs body and fails the test if it has not finished within
// limit, dumping every goroutine stack so a hang is attributable to a lock pair
// rather than merely reported.
//
// It is the failure channel for an ABBA inversion: a deadlocked body never
// returns, and nothing else in the suite would notice.
func lockOrderWatchdog(t *testing.T, limit time.Duration, body func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		body()
	}()
	select {
	case <-done:
	case <-time.After(limit):
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Fatalf("DEADLOCK: workload did not finish within %s — a lock-order "+
			"inversion between hashShard.mu (spine) and entry.mu is the expected "+
			"cause; the lock order is spine-then-entry and must never invert.\n"+
			"all goroutine stacks follow:\n%s", limit, buf[:n])
	}
}

const (
	// parityWorkers is the number of concurrent mutators. Each owns a disjoint
	// band of NodeIDs, which is what makes an EXACT differential oracle possible
	// under real concurrency: no two workers ever touch the same (value, node)
	// pair, so each pair's final state is that worker's last write to it, and
	// the union of the per-worker models is the exact expected index state.
	parityWorkers = 8
	// parityBand is how many NodeIDs each worker owns.
	parityBand = 4
	// parityValues is the total key space the workers share.
	parityValues = 48
	// parityChurnValues are keys 0..parityChurnValues-1, on which every worker
	// inserts and immediately deletes. They spend much of their life empty, so
	// the reaper runs on entries every other worker is simultaneously creating,
	// mutating and re-creating — the exact interleaving the inversion needed,
	// and the only way the reap-then-recreate path gets exercised at all.
	parityChurnValues = 4
	// parityRounds is the per-worker operation count.
	parityRounds = 3000
	// parityFrozen is a key populated before the workers start and touched by
	// none of them, so a concurrent reader has an EXACT oracle to check while
	// the mutators run.
	parityFrozen = int64(1_000_000)
	// parityAbsent is a NodeID outside every worker's band, so Contains must
	// report false for it under every key.
	parityAbsent = graph.NodeID(999_999)
)

// parityModel is the reference state one worker is responsible for: the set of
// nodes from its OWN band that carry each value, computed sequentially in the
// worker's own program order.
type parityModel map[int64]map[uint64]bool

func (m parityModel) insert(value int64, node uint64) {
	s := m[value]
	if s == nil {
		s = make(map[uint64]bool)
		m[value] = s
	}
	s[node] = true
}

func (m parityModel) remove(value int64, node uint64) {
	if s := m[value]; s != nil {
		delete(s, node)
	}
}

// parityUnion folds the per-worker models into the exact expected index state,
// as a sorted id slice per value.
func parityUnion(models []parityModel) map[int64][]uint64 {
	want := make(map[int64][]uint64)
	for _, m := range models {
		for value, s := range m {
			for id, on := range s {
				if on {
					want[value] = append(want[value], id)
				}
			}
		}
	}
	for value := range want {
		sort.Slice(want[value], func(a, b int) bool { return want[value][a] < want[value][b] })
	}
	return want
}

// parityVerify checks the whole exported read surface of idx against want, and
// returns how many of the worker keys the oracle holds populated and empty. The
// caller asserts on that split, because a phase in which every key came out the
// same way makes the comparison weak: an all-empty oracle compared against an
// all-empty index would pass on an index that had lost every id.
func parityVerify(t *testing.T, phase string, idx *Index[int64], want map[int64][]uint64) (populatedKeys, emptyKeys int) {
	t.Helper()

	populated, empty := 0, 0
	for value := range int64(parityValues) {
		exp := want[value]
		if len(exp) > 0 {
			populated++
		} else {
			empty++
		}

		got := idx.Lookup(value).ToArray()
		if len(got) != len(exp) {
			t.Fatalf("%s: value %d: Lookup returned %d ids, oracle says %d (got %v, want %v)",
				phase, value, len(got), len(exp), got, exp)
		}
		for k := range got {
			if got[k] != exp[k] {
				t.Fatalf("%s: value %d: Lookup[%d] = %d, oracle says %d",
					phase, value, k, got[k], exp[k])
			}
		}
		if n := idx.Cardinality(value); n != uint64(len(exp)) {
			t.Fatalf("%s: value %d: Cardinality = %d, oracle says %d", phase, value, n, len(exp))
		}
		if app := idx.LookupAppend(value, nil); len(app) != len(exp) {
			t.Fatalf("%s: value %d: LookupAppend returned %d ids, oracle says %d",
				phase, value, len(app), len(exp))
		}
		for _, id := range exp {
			if !idx.Contains(value, graph.NodeID(id)) {
				t.Fatalf("%s: value %d: Contains(%d) = false, oracle says present", phase, value, id)
			}
		}
		if idx.Contains(value, parityAbsent) {
			t.Fatalf("%s: value %d: Contains(%d) = true for a node no worker owns",
				phase, value, parityAbsent)
		}
		// The reaper's own invariant, and the assertion that would catch a
		// reaper which stopped running: an emptied key leaves NO entry behind,
		// and a populated one keeps its entry. No exported call can see this.
		if present := idx.keyPresent(value); present != (len(exp) > 0) {
			t.Fatalf("%s: value %d: map entry present = %v, but the oracle holds %d ids — "+
				"an emptied key must be reaped and a non-empty one must be kept",
				phase, value, present, len(exp))
		}
	}

	// DistinctValues counts non-empty keys; the frozen key is one of them and is
	// outside the worker key space.
	wantDistinct := uint64(populated) + 1
	if got := idx.DistinctValues(); got != wantDistinct {
		t.Fatalf("%s: DistinctValues = %d, oracle says %d (%d populated worker keys + the frozen key)",
			phase, got, wantDistinct, populated)
	}
	if raw := idx.nonEmptySum(); raw != int64(wantDistinct) {
		t.Fatalf("%s: raw non-empty counter sum = %d, want %d", phase, raw, wantDistinct)
	}
	t.Logf("%s: oracle holds %d populated and %d empty worker keys", phase, populated, empty)
	return populated, empty
}

// TestIndex_ConcurrentMutateReapAndRead_Parity is the differential parity test
// the acceptance criteria call for. It runs creation, mutation, reaping,
// re-creation, reading and serialisation concurrently, checks the index against
// an exact sequential oracle, and is guarded by a watchdog so an ABBA inversion
// fails it as a deadlock rather than hanging the suite.
//
// It is deliberately staged, because a single end-state assertion would be
// VACUOUS: a workload that drains everything and only then compares against the
// oracle compares empty against empty, and would pass on an index that lost
// every id. So parity is asserted after a MIXED phase (populated and empty keys
// coexisting, verified by the populated/empty split parityVerify logs and
// asserts on), then again after a drain phase that forces every key through the
// reaper, then again after keys are re-created.
func TestIndex_ConcurrentMutateReapAndRead_Parity(t *testing.T) {
	t.Parallel()

	idx := New[int64]()

	// Frozen oracle: populated up front, touched by no worker.
	frozenIDs := []uint64{9001, 9002, 9003, 9004, 9005}
	for _, id := range frozenIDs {
		idx.Insert(parityFrozen, graph.NodeID(id))
	}

	models := make([]parityModel, parityWorkers)
	for w := range models {
		models[w] = make(parityModel)
	}

	stopReaders := make(chan struct{})
	var readerWG, writerWG sync.WaitGroup
	readerErr := make(chan error, 8)
	var serializeRuns, readerRuns int64
	var statMu sync.Mutex

	failReader := func(err error) {
		select {
		case readerErr <- err:
		default:
		}
	}

	// Concurrent readers. They cannot predict a churning key's contents, but
	// they CAN check the frozen one exactly, and every read they issue takes the
	// same entry locks the mutators and the reaper are fighting over.
	for r := range 4 {
		readerWG.Add(1)
		go func(seed uint64) {
			defer readerWG.Done()
			rng := rand.New(rand.NewPCG(seed, 0xF00D)) //nolint:gosec // deterministic test RNG
			buf := make([]uint64, 0, 8)
			local, serialized := int64(0), int64(0)
			defer func() {
				statMu.Lock()
				readerRuns += local
				serializeRuns += serialized
				statMu.Unlock()
			}()
			for {
				select {
				case <-stopReaders:
					return
				default:
				}
				local++
				if got := idx.Cardinality(parityFrozen); got != uint64(len(frozenIDs)) {
					failReader(fmt.Errorf("frozen key Cardinality = %d, want %d", got, len(frozenIDs)))
					return
				}
				if got := idx.Lookup(parityFrozen).ToArray(); len(got) != len(frozenIDs) {
					failReader(fmt.Errorf("frozen key Lookup len = %d, want %d", len(got), len(frozenIDs)))
					return
				}
				if got := idx.LookupAppend(parityFrozen, buf[:0]); len(got) != len(frozenIDs) {
					failReader(fmt.Errorf("frozen key LookupAppend len = %d, want %d", len(got), len(frozenIDs)))
					return
				}
				if !idx.Contains(parityFrozen, graph.NodeID(frozenIDs[0])) {
					failReader(fmt.Errorf("frozen key lost member %d", frozenIDs[0]))
					return
				}
				// The frozen key is always populated, so the count can never be
				// zero however hard the workers churn the rest.
				if idx.DistinctValues() == 0 {
					failReader(fmt.Errorf("DistinctValues = 0 while the frozen key is populated"))
					return
				}
				if raw := idx.nonEmptySum(); raw < 0 {
					failReader(fmt.Errorf("raw non-empty counter sum went negative: %d", raw))
					return
				}
				// Drive the churn keys' read paths too, so a reader is inside an
				// entry lock while the reaper is trying to detach that entry.
				v := int64(rng.IntN(parityValues))
				_ = idx.Lookup(v)
				_ = idx.Cardinality(v)
				_ = idx.Contains(v, parityAbsent)
				buf = idx.LookupAppend(v, buf[:0])
				// Serialize holds the SPINE read lock and takes ENTRY read locks
				// under it — the permitted order, and the third acquirer of the
				// pair. Rate-limited: it walks all 256 shards.
				if rng.IntN(64) == 0 {
					serialized++
					if err := idx.Serialize(io.Discard); err != nil {
						failReader(fmt.Errorf("concurrent Serialize: %w", err))
						return
					}
				}
			}
		}(uint64(r) + 1)
	}

	// Phase 1 — mixed churn. Every worker drives creation, mutation, deletion
	// that empties a key (so the reaper runs), and re-insertion of a key the
	// reaper has just dropped.
	lockOrderWatchdog(t, 120*time.Second, func() {
		for w := range parityWorkers {
			writerWG.Add(1)
			go func(w int, m parityModel) {
				defer writerWG.Done()
				lo := uint64(w) * parityBand
				rng := rand.New(rand.NewPCG(uint64(w)+1, 0xC0FFEE)) //nolint:gosec // deterministic test RNG
				for range parityRounds {
					v := int64(rng.IntN(parityValues))
					id := lo + uint64(rng.IntN(parityBand))
					if v < parityChurnValues {
						// Insert then immediately delete: the key converges on
						// empty, the reaper fires under concurrent mutation, and
						// the next round's insert re-creates a reaped key.
						idx.Insert(v, graph.NodeID(id))
						m.insert(v, id)
						idx.Delete(v, graph.NodeID(id))
						m.remove(v, id)
						continue
					}
					if rng.IntN(2) == 0 {
						idx.Insert(v, graph.NodeID(id))
						m.insert(v, id)
					} else {
						idx.Delete(v, graph.NodeID(id))
						m.remove(v, id)
					}
				}
			}(w, models[w])
		}
		writerWG.Wait()
	})
	pop1, empty1 := parityVerify(t, "phase 1 (mixed churn)", idx, parityUnion(models))
	// Non-vacuity of the phase itself: the churn must have left BOTH populated and
	// empty keys, or this phase degenerates into one of the other two and the
	// mixed-state comparison it exists for never happened.
	if pop1 == 0 || empty1 == 0 {
		t.Fatalf("phase 1 left %d populated and %d empty keys: a mixed state is the "+
			"whole point of this phase, and one of the two is missing", pop1, empty1)
	}

	// Phase 2 — drain. Every worker withdraws everything it holds, so EVERY key
	// is driven to empty and must be reaped. Concurrent, so the reaper races
	// itself across workers on the same key.
	lockOrderWatchdog(t, 120*time.Second, func() {
		for w := range parityWorkers {
			writerWG.Add(1)
			go func(w int, m parityModel) {
				defer writerWG.Done()
				lo := uint64(w) * parityBand
				for v := range int64(parityValues) {
					for id := lo; id < lo+parityBand; id++ {
						idx.Delete(v, graph.NodeID(id))
						m.remove(v, id)
					}
				}
			}(w, models[w])
		}
		writerWG.Wait()
	})
	drained := parityUnion(models)
	for v, ids := range drained {
		if len(ids) != 0 {
			t.Fatalf("phase 2: oracle still holds %d ids for value %d after the drain", len(ids), v)
		}
	}
	if pop2, empty2 := parityVerify(t, "phase 2 (fully drained)", idx, drained); pop2 != 0 || empty2 != parityValues {
		t.Fatalf("phase 2 left %d populated and %d empty keys, want 0 and %d", pop2, empty2, parityValues)
	}

	// Phase 3 — re-create every reaped key, concurrently. This is the assertion
	// that a key the reaper dropped can be inserted again and answers correctly;
	// phase 1's churn keys exercise the same path thousands of times, but only
	// under a race, so it is pinned deterministically here.
	lockOrderWatchdog(t, 120*time.Second, func() {
		for w := range parityWorkers {
			writerWG.Add(1)
			go func(w int, m parityModel) {
				defer writerWG.Done()
				lo := uint64(w) * parityBand
				for v := range int64(parityValues) {
					idx.Insert(v, graph.NodeID(lo))
					m.insert(v, lo)
				}
			}(w, models[w])
		}
		writerWG.Wait()
	})
	if pop3, empty3 := parityVerify(t, "phase 3 (keys re-created after reap)", idx, parityUnion(models)); pop3 != parityValues || empty3 != 0 {
		t.Fatalf("phase 3 left %d populated and %d empty keys, want %d and 0 — every "+
			"reaped key must be re-creatable", pop3, empty3, parityValues)
	}

	close(stopReaders)
	readerWG.Wait()
	select {
	case err := <-readerErr:
		t.Fatalf("concurrent reader observed a corrupted index: %v", err)
	default:
	}

	// Non-vacuity: the readers must actually have run, and Serialize must
	// actually have been driven, or their assertions proved nothing.
	statMu.Lock()
	gotReaderRuns, gotSerializeRuns := readerRuns, serializeRuns
	statMu.Unlock()
	if gotReaderRuns == 0 {
		t.Fatal("no reader iteration ran: every concurrent read assertion above was vacuous")
	}
	if gotSerializeRuns == 0 {
		t.Fatal("Serialize never ran concurrently with the workload: the spine-then-entry " +
			"read path was not exercised under contention")
	}
	t.Logf("readers ran %d iterations, %d of them serialising the whole index",
		gotReaderRuns, gotSerializeRuns)

	// The frozen key is the control: untouched, still exact.
	if got := idx.Cardinality(parityFrozen); got != uint64(len(frozenIDs)) {
		t.Fatalf("frozen key Cardinality = %d, want %d", got, len(frozenIDs))
	}
}

// TestIndex_MutateRetriesAfterEntryReaped covers the branches in [Index.Insert]
// and [Index.Delete] that production reaches only through a race: the entry
// looked up under the shard lock is reaped or displaced before the entry lock is
// acquired.
//
// It uses the test-only markEntryDead seam to make that state deterministic,
// because a scheduled race cannot be relied on to hit a specific branch.
func TestIndex_MutateRetriesAfterEntryReaped(t *testing.T) {
	t.Parallel()

	t.Run("Insert recreates the entry", func(t *testing.T) {
		t.Parallel()
		idx := New[int64]()
		idx.Insert(1, graph.NodeID(10))
		if !idx.markEntryDead(1) {
			t.Fatal("markEntryDead(1) found no entry to kill")
		}
		idx.Insert(1, graph.NodeID(11))
		got := idx.Lookup(1).ToArray()
		if len(got) != 1 || got[0] != 11 {
			t.Fatalf("Lookup(1) = %v, want [11]: the retry must land in a FRESH entry, "+
				"not in the reaped one and not alongside its stale contents", got)
		}
		if !idx.keyPresent(1) {
			t.Fatal("value 1 should have been re-created by the retrying Insert")
		}
		if got := idx.DistinctValues(); got != 1 {
			t.Fatalf("DistinctValues = %d, want 1 after the retry re-created the key", got)
		}
	})

	t.Run("Delete reports absent and does not resurrect", func(t *testing.T) {
		t.Parallel()
		idx := New[int64]()
		idx.Insert(2, graph.NodeID(20))
		if !idx.markEntryDead(2) {
			t.Fatal("markEntryDead(2) found no entry to kill")
		}
		// Delete must terminate and must not resurrect the key.
		idx.Delete(2, graph.NodeID(20))
		if idx.keyPresent(2) {
			t.Fatal("Delete on a reaped value must not re-create its entry")
		}
		if got := idx.Cardinality(2); got != 0 {
			t.Fatalf("Cardinality(2) = %d, want 0", got)
		}
		if got := idx.DistinctValues(); got != 0 {
			t.Fatalf("DistinctValues = %d, want 0", got)
		}
	})
}

// TestShardReap_RefusesRepopulatedEntry pins the empty re-check in
// [hashShard.reap]. The state it constructs is the one a race produces: a
// [Index.Delete] emptied the set and released the entry lock, and a concurrent
// [Index.Insert] re-added a node before the reap acquired the shard lock. Reap
// must then leave the key alone, or those ids are silently lost.
//
// The window is CONSTRUCTED rather than raced for: reap is called directly with
// the entry pointer the emptying Delete would have handed it, which makes the
// refusal arithmetic instead of scheduler-dependent.
func TestShardReap_RefusesRepopulatedEntry(t *testing.T) {
	t.Parallel()

	idx := New[int64]()
	const value = int64(7)
	idx.Insert(value, graph.NodeID(1))
	e, ok := idx.entryFor(value)
	if !ok {
		t.Fatal("entryFor(7) found no published entry")
	}
	// Stand in for the concurrent Insert that lands in the window.
	idx.Insert(value, graph.NodeID(2))

	func() { s, h := idx.locate(value); s.reap(h, value, e) }()

	if !idx.keyPresent(value) {
		t.Fatal("reap dropped a key whose set is NOT empty: the ids a concurrent " +
			"Insert added in the window between Delete's removal and the reap are lost")
	}
	got := idx.Lookup(value).ToArray()
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("Lookup(7) = %v, want [1 2]", got)
	}
	if n := idx.DistinctValues(); n != 1 {
		t.Fatalf("DistinctValues = %d, want 1", n)
	}
}

// TestShardReap_RefusesDisplacedEntry pins the pointer-identity half of
// [hashShard.reap]'s guard: when the key's published entry is no longer the one
// the caller emptied — because [Index.Deserialize] displaced it and a mutator
// re-created the key since — the caller's observation says nothing about the
// entry published now, and reap must decline.
func TestShardReap_RefusesDisplacedEntry(t *testing.T) {
	t.Parallel()

	idx := New[int64]()
	const value = int64(8)
	idx.Insert(value, graph.NodeID(1))
	stale, ok := idx.entryFor(value)
	if !ok {
		t.Fatal("entryFor(8) found no published entry")
	}
	// Displace it the way Deserialize does, then re-create the key.
	if !idx.markEntryDead(value) {
		t.Fatal("markEntryDead(8) found no entry to kill")
	}
	idx.Insert(value, graph.NodeID(2))
	fresh, ok := idx.entryFor(value)
	if !ok {
		t.Fatal("the re-created key has no published entry")
	}
	if fresh == stale {
		t.Fatal("the re-created key reused the displaced entry: the test cannot " +
			"distinguish the pointer identity check it exists to exercise")
	}

	func() { s, h := idx.locate(value); s.reap(h, value, stale) }()

	if !idx.keyPresent(value) {
		t.Fatal("reap acted on an entry other than the one it was asked about")
	}
	got := idx.Lookup(value).ToArray()
	if len(got) != 1 || got[0] != 2 {
		t.Fatalf("Lookup(8) = %v, want [2]", got)
	}
}

// TestDistinctValues_SettlesToZeroUnderConcurrentDelete is the regression gate
// for the third state the per-value geometry made observable.
//
// [Index.Delete] empties the set under the entry lock and drops the key under
// the shard write lock, in that order, so a key can transiently be present with
// an empty set. cypher.hashIndexKind reads DistinctValues() == 0 as the
// authoritative "this string hash index holds no data" test; a false non-zero
// pins a parameter compared against that property to String and rejects an
// integer parameter with a spurious ParamTypeError, which is rmp #1983. So this
// deletes everything from many goroutines while readers poll, and asserts the
// count settles to EXACTLY zero.
//
// Every value's nodes are split across the deleters, so each value is emptied
// by whichever goroutine happens to remove its last node — the reap races the
// other deleters on the same entry rather than running unopposed.
func TestDistinctValues_SettlesToZeroUnderConcurrentDelete(t *testing.T) {
	t.Parallel()

	const (
		values   = 512
		deleters = 8
	)

	idx := New[int64]()
	for v := range int64(values) {
		for d := range uint64(deleters) {
			idx.Insert(v, graph.NodeID(d))
		}
	}
	if got := idx.DistinctValues(); got != values {
		t.Fatalf("setup: DistinctValues = %d, want %d — nothing to delete makes the "+
			"whole test vacuous", got, values)
	}

	stop := make(chan struct{})
	var readerWG, deleterWG sync.WaitGroup
	var statMu sync.Mutex
	polls := 0
	minRaw := int64(1 << 62)
	maxSeen := uint64(0)

	for range 4 {
		readerWG.Add(1)
		go func() {
			defer readerWG.Done()
			localPolls := 0
			localMin := int64(1 << 62)
			localMax := uint64(0)
			for {
				select {
				case <-stop:
					statMu.Lock()
					polls += localPolls
					if localMin < minRaw {
						minRaw = localMin
					}
					if localMax > maxSeen {
						maxSeen = localMax
					}
					statMu.Unlock()
					return
				default:
				}
				localPolls++
				if n := idx.DistinctValues(); n > localMax {
					localMax = n
				}
				if raw := idx.nonEmptySum(); raw < localMin {
					localMin = raw
				}
			}
		}()
	}

	lockOrderWatchdog(t, 120*time.Second, func() {
		for d := range uint64(deleters) {
			deleterWG.Add(1)
			go func(d uint64) {
				defer deleterWG.Done()
				for v := range int64(values) {
					idx.Delete(v, graph.NodeID(d))
				}
			}(d)
		}
		deleterWG.Wait()
	})

	close(stop)
	readerWG.Wait()

	statMu.Lock()
	gotPolls, gotMinRaw, gotMaxSeen := polls, minRaw, maxSeen
	statMu.Unlock()

	if gotPolls == 0 {
		t.Fatal("no reader poll ran: the concurrent-observation assertions were vacuous")
	}
	if gotMinRaw < 0 {
		t.Fatalf("raw non-empty counter sum went negative (%d): a transition was applied "+
			"more than once, and DistinctValues only hides it because it clamps", gotMinRaw)
	}
	if gotMaxSeen > values {
		t.Fatalf("DistinctValues peaked at %d, above the %d values ever inserted", gotMaxSeen, values)
	}

	if got := idx.DistinctValues(); got != 0 {
		t.Fatalf("DistinctValues = %d after every node was deleted, want 0 — an "+
			"empty-but-present entry is being counted, which is rmp #1983", got)
	}
	if raw := idx.nonEmptySum(); raw != 0 {
		t.Fatalf("raw non-empty counter sum = %d after every node was deleted, want 0", raw)
	}
	for v := range int64(values) {
		if idx.keyPresent(v) {
			t.Fatalf("value %d still has a map entry after every node was deleted: reap did not run", v)
		}
	}
	t.Logf("%d reader polls; peak DistinctValues %d, minimum raw sum %d", gotPolls, gotMaxSeen, gotMinRaw)
}

// TestIndex_DeserializeDisplacesInFlightMutators checks that a spine swap does
// not silently swallow a concurrent mutation into a detached entry: the
// displaced entries are marked dead, so an in-flight mutator retries against
// the new map. It also drives Deserialize concurrently with mutation and
// reaping, which is the third acquirer of the spine-then-entry pair, and
// asserts the non-empty counter is still exact afterwards.
func TestIndex_DeserializeDisplacesInFlightMutators(t *testing.T) {
	t.Parallel()

	const keys = 8

	src := New[int64]()
	for v := range int64(keys) {
		src.Insert(v, graph.NodeID(uint64(v)+1000))
	}
	var buf bytes.Buffer
	if err := src.Serialize(&buf); err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	img := buf.Bytes()

	idx := New[int64]()
	stop := make(chan struct{})
	var wg sync.WaitGroup

	lockOrderWatchdog(t, 120*time.Second, func() {
		for w := range 4 {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for r := range 3000 {
					v := int64((w + r) % keys)
					idx.Insert(v, graph.NodeID(uint64(w)))
					idx.Delete(v, graph.NodeID(uint64(w)))
					select {
					case <-stop:
						return
					default:
					}
				}
			}(w)
		}
		for range 200 {
			if err := idx.Deserialize(bytes.NewReader(img)); err != nil {
				t.Errorf("Deserialize: %v", err)
				break
			}
		}
		close(stop)
		wg.Wait()
	})

	// The last Deserialize wins for every key it names; concurrent mutators may
	// have inserted or deleted their own ids afterwards, so the checkable
	// invariant is that the index is intact and every read answers consistently.
	nonEmpty := uint64(0)
	for v := range int64(keys) {
		n := idx.Cardinality(v)
		if got := uint64(len(idx.Lookup(v).ToArray())); got != n {
			t.Fatalf("value %d: Lookup len %d disagrees with Cardinality %d", v, got, n)
		}
		if got := uint64(len(idx.LookupAppend(v, nil))); got != n {
			t.Fatalf("value %d: LookupAppend len %d disagrees with Cardinality %d", v, got, n)
		}
		if n > 0 {
			nonEmpty++
			if !idx.keyPresent(v) {
				t.Fatalf("value %d holds %d ids but has no map entry", v, n)
			}
		}
	}
	// The counter must agree with what the reads report, whatever the race left
	// behind. An empty-but-present entry is allowed here (a mutator can empty a
	// key after the last swap and lose the reap race to the test's end), but it
	// must NOT be counted.
	if got := idx.DistinctValues(); got != nonEmpty {
		t.Fatalf("DistinctValues = %d but %d of the %d keys actually hold ids: the "+
			"non-empty counter and the read surface disagree after a concurrent Deserialize",
			got, nonEmpty, keys)
	}
	if nonEmpty == 0 {
		t.Fatal("every key ended empty: the counter agreement asserted above is vacuous")
	}
	t.Logf("%d of %d keys populated after the concurrent Deserialize storm", nonEmpty, keys)
}

// TestIndex_SerializeSkipsEmptyEntry pins the on-disk contract across the new
// third state. Before rmp #2692 an empty entry could not exist — Delete removed
// the last id and dropped the key in ONE critical section — so no image has ever
// carried a zero-length posting list. The per-value geometry makes an
// empty-but-present entry reachable, and [Index.Serialize] must not start
// emitting one: Deserialize would install it as a key nothing ever reaps.
//
// The state is constructed with the reap seam rather than raced for.
func TestIndex_SerializeSkipsEmptyEntry(t *testing.T) {
	t.Parallel()

	idx := New[int64]()
	idx.Insert(1, graph.NodeID(10))
	idx.Insert(2, graph.NodeID(20))

	// Reproduce the window: empty value 2's set, and hold back its reap by
	// handing reap a pointer that is no longer the published entry.
	e, ok := idx.entryFor(2)
	if !ok {
		t.Fatal("entryFor(2) found no published entry")
	}
	idx.Delete(2, graph.NodeID(20))
	if idx.keyPresent(2) {
		t.Fatal("Delete left value 2's key behind: this test needs to re-create the " +
			"empty-but-present state deliberately, not inherit a broken reap")
	}
	// Re-create the key and empty it again, this time defeating the reap the way
	// a displaced pointer does, so the empty entry stays published.
	idx.Insert(2, graph.NodeID(21))
	fresh, ok := idx.entryFor(2)
	if !ok {
		t.Fatal("entryFor(2) found no re-created entry")
	}
	fresh.mu.Lock()
	nowEmpty := fresh.deleteLocked(21)
	fresh.mu.Unlock()
	if !nowEmpty {
		t.Fatal("removing the only id did not empty the set")
	}
	idx.shard(int64(2)).nonEmpty.Add(-1)
	func() { s, h := idx.locate(int64(2)); s.reap(h, 2, e) }() // stale pointer: declines, key stays, set empty
	if !idx.keyPresent(2) {
		t.Fatal("could not construct an empty-but-present entry")
	}
	if got := idx.DistinctValues(); got != 1 {
		t.Fatalf("DistinctValues = %d, want 1: the empty-but-present entry must not be counted", got)
	}

	var buf bytes.Buffer
	if err := idx.Serialize(&buf); err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	dst := New[int64]()
	if err := dst.Deserialize(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Deserialize: %v", err)
	}
	if dst.keyPresent(2) {
		t.Fatal("Serialize emitted a zero-length posting list for the empty entry, and " +
			"Deserialize installed it as a key nothing will ever reap")
	}
	if got := dst.DistinctValues(); got != 1 {
		t.Fatalf("round-tripped DistinctValues = %d, want 1", got)
	}
	if got := dst.Lookup(1).ToArray(); len(got) != 1 || got[0] != 10 {
		t.Fatalf("round-tripped Lookup(1) = %v, want [10]", got)
	}
}

// TestIndex_DeserializeMarksDisplacedEntryDead pins the half of
// [Index.Deserialize] that a concurrency storm does NOT reliably reach.
//
// The storm in TestIndex_DeserializeDisplacesInFlightMutators was measured
// against a build in which Deserialize stopped marking displaced entries dead:
// 200 swaps against 4 mutators left the whole suite GREEN. The window — a
// mutator between its shard lookup and its entry lock, spanning a swap — is too
// narrow to be hit by scheduling, so waiting for the event is not a test.
//
// So the precondition is CONSTRUCTED instead: the test itself holds the entry
// lock an in-flight mutator would hold, and asserts two things a build that
// skipped the marking cannot satisfy.
//
//  1. Deserialize CANNOT complete while that lock is held. Publishing the dead
//     flag requires the entry's write lock, so this is guaranteed by the lock
//     rather than by timing — a build that skips the marking sails straight
//     past and finishes.
//  2. Once released, the displaced entry IS dead. That assertion needs no
//     timing at all and is the direct falsification.
func TestIndex_DeserializeMarksDisplacedEntryDead(t *testing.T) {
	t.Parallel()

	src := New[int64]()
	src.Insert(100, graph.NodeID(1))
	src.Insert(200, graph.NodeID(2))
	var buf bytes.Buffer
	if err := src.Serialize(&buf); err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	img := buf.Bytes()

	idx := New[int64]()
	idx.Insert(1, graph.NodeID(10))
	stale, ok := idx.entryFor(1)
	if !ok {
		t.Fatal("entryFor(1) found no published entry")
	}
	if stale.isDead() {
		t.Fatal("a freshly published entry is already dead")
	}

	// Stand in for a mutator that has looked the entry up and taken its lock.
	// NOTHING may call an idx method between here and the Unlock below: the
	// lock order is spine-then-entry, and the test must not invert it either.
	stale.mu.Lock()

	done := make(chan error, 1)
	go func() { done <- idx.Deserialize(bytes.NewReader(img)) }()

	select {
	case err := <-done:
		stale.mu.Unlock()
		t.Fatalf("Deserialize completed (err=%v) while a displaced entry's write lock "+
			"was held. Publishing entry.dead requires that lock, so completing means "+
			"the entry was NOT marked: an in-flight mutator will write into a detached "+
			"entry, its write will be lost, and its non-empty counter update will land "+
			"after the counter was restated", err)
	case <-time.After(100 * time.Millisecond):
		// Expected: parked on the entry lock in order to publish dead.
	}

	stale.mu.Unlock()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Deserialize: %v", err)
		}
	case <-time.After(30 * time.Second):
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Fatalf("Deserialize did not complete after the entry lock was released:\n%s", buf[:n])
	}

	if !stale.isDead() {
		t.Fatal("the displaced entry was not marked dead: a mutator holding it would " +
			"never learn to retry against the new map")
	}
	// The swap itself must have landed, counter included.
	if got := idx.DistinctValues(); got != 2 {
		t.Fatalf("DistinctValues = %d after the swap, want 2", got)
	}
	if raw := idx.nonEmptySum(); raw != 2 {
		t.Fatalf("raw non-empty counter sum = %d after the swap, want 2", raw)
	}
	if idx.keyPresent(1) {
		t.Fatal("the displaced key survived the swap")
	}
	if got := idx.Lookup(100).ToArray(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("Lookup(100) = %v, want [1]", got)
	}
}

// craftHashDuplicateKey builds a CRC-valid hash payload that names the SAME
// int64 key twice, each with one id. Serialize cannot produce this — its source
// is a map — so it models a crafted or corrupt image.
func craftHashDuplicateKey() []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, hashMagic)
	_ = binary.Write(&b, binary.LittleEndian, hashFormatVersion)
	_ = binary.Write(&b, binary.LittleEndian, uint64(2)) // entryCount
	for _, id := range []uint64{11, 22} {
		_ = binary.Write(&b, binary.LittleEndian, uint32(8)) // keyLen (int64 key)
		_ = binary.Write(&b, binary.LittleEndian, int64(42)) // the SAME key both times
		_ = binary.Write(&b, binary.LittleEndian, uint64(1)) // idCount
		_ = binary.Write(&b, binary.LittleEndian, id)
	}
	body := b.Bytes()
	crc := crc32.Checksum(body, castagnoli)
	var tr [4]byte
	binary.LittleEndian.PutUint32(tr[:], crc)
	return append(append([]byte{}, body...), tr[:]...)
}

// TestDeserialize_DuplicateKeyCountedOnce pins the non-empty counter against a
// payload that names one key twice.
//
// The second entry REPLACES the first in the shard map, so exactly one key is
// indexed. A counter incremented as entries were parsed would report two, and
// an over-reported [Index.DistinctValues] is the rmp #1983 failure direction:
// cypher.hashIndexKind would call a string index populated when it holds less
// than it claims. The count is therefore taken from the maps that were built,
// not from the parse.
func TestDeserialize_DuplicateKeyCountedOnce(t *testing.T) {
	t.Parallel()

	idx := New[int64]()
	if err := idx.Deserialize(bytes.NewReader(craftHashDuplicateKey())); err != nil {
		t.Fatalf("Deserialize: %v", err)
	}
	if got := idx.DistinctValues(); got != 1 {
		t.Fatalf("DistinctValues = %d after a payload naming one key twice, want 1: "+
			"the duplicate replaced the first entry, so only one key is indexed", got)
	}
	if raw := idx.nonEmptySum(); raw != 1 {
		t.Fatalf("raw non-empty counter sum = %d, want 1", raw)
	}
	// The last entry for the key wins, matching map-assignment semantics.
	if got := idx.Lookup(42).ToArray(); len(got) != 1 || got[0] != 22 {
		t.Fatalf("Lookup(42) = %v, want [22] (the later duplicate)", got)
	}
	if !idx.keyPresent(42) {
		t.Fatal("value 42 has no map entry")
	}
}

// TestDeserialize_ZeroLengthPostingListNotCounted pins the other crafted-payload
// shape the counter has to survive: a key declared with no ids at all.
//
// [Index.Serialize] never writes one (see TestIndex_SerializeSkipsEmptyEntry),
// so this too is only reachable from a crafted or corrupt image. The key is
// installed — the decoder's acceptance rules are unchanged by rmp #2692 — but it
// holds nothing, so [Index.DistinctValues], which counts NON-EMPTY entries, must
// not count it.
func TestDeserialize_ZeroLengthPostingListNotCounted(t *testing.T) {
	t.Parallel()

	idx := New[int64]()
	if err := idx.Deserialize(bytes.NewReader(craftHashForgedIDCount(0))); err != nil {
		t.Fatalf("Deserialize: %v", err)
	}
	if !idx.keyPresent(42) {
		t.Fatal("the zero-length entry was not installed: this test's premise no longer holds")
	}
	if got := idx.DistinctValues(); got != 0 {
		t.Fatalf("DistinctValues = %d, want 0: a key holding no ids is not a distinct value", got)
	}
	if raw := idx.nonEmptySum(); raw != 0 {
		t.Fatalf("raw non-empty counter sum = %d, want 0", raw)
	}
	if got := idx.Cardinality(42); got != 0 {
		t.Fatalf("Cardinality(42) = %d, want 0", got)
	}
}
