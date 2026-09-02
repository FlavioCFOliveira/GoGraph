// Package entryheap is the committed heap-and-GC instrument for the per-key
// payload of the B+ tree property index (rmp sprint 353, task #2684).
//
// # Why it exists
//
// Task #2683 converted graph/index/btree to a copy-on-write snapshot whose
// per-key payload — the node-set plus the lock that guards it — lives behind a
// pointer in a separate heap object, so a path-copied snapshot and its
// predecessor address the SAME lock. That bought 12.37x throughput at 8
// goroutines and cut resident bytes per key by 11.9%, and it spent exactly one
// vector to do it: one extra heap OBJECT per distinct key. At 10M keys that
// object count was measured to more than double GC mark time.
//
// The harness that produced those numbers was never committed, so the claim
// could not be re-run. This package is that harness, rebuilt to the same
// discipline and committed so every number in the record is reproducible.
//
// # What it measures, and why these instruments
//
// For one live index of Keys distinct values, each carrying exactly ONE node —
// deliberately the case most ADVERSE to a per-key payload object, because it
// maximises the object count per resident byte — it reports:
//
//   - resident bytes per key, from /memory/classes/heap/objects:bytes. Chosen
//     over an inuse_space pprof profile because that samples at 512 KB and
//     would systematically under-count a large population of small uniform
//     objects, which is exactly the shape under test.
//   - live objects per key, from /gc/heap/objects:objects.
//   - scannable bytes per key, from /gc/scan/heap:bytes. This separates the two
//     candidate cost drivers: a change that cuts object COUNT while leaving
//     scannable BYTES alone proves the mark cost is per-object, not per-byte.
//   - GC mark cost, by TWO independent instruments that must agree:
//     the wall-clock of a forced [runtime.GC] (which blocks until the cycle
//     completes), and the mark CPU-seconds drawn from
//     /cpu/classes/gc/mark/{dedicated,assist,idle}. Wall clock alone is
//     sensitive to how many workers the scheduler granted; CPU-seconds alone
//     hides a change in parallelism. Reporting both makes a discrepancy visible
//     instead of silently picking the flattering one.
//
// Every counter is BRACKETED: read immediately before and immediately after the
// forced cycle it describes, so nothing that happens outside that window can be
// folded into the number.
//
// # The deletion phase, and the retention question it answers
//
// Any design that carves per-key payloads out of a shared slab trades object
// count for RETENTION: a slab stays reachable while any single one of its
// entries is still live, so a sufficiently sparse survivor set pins slabs that
// are almost entirely dead. Config.KeepOneIn drives exactly that worst case.
// Entries are handed out in insertion order, so deleting every key except every
// KeepOneIn-th INSERTION leaves at most one survivor per slab of that size —
// the maximum a slab allocator can pin. The phase reports resident bytes per
// SURVIVING key, which is the number the pinning question needs.
//
// # Discipline
//
// One process measures ONE configuration. The heap is cumulative and cannot be
// reset, so a second configuration in the same process inherits the first one's
// warm heap, its raised GC goal and its mapped pages. Arms are compared by
// running the binary repeatedly, interleaved, never by looping inside it.
//
// Before any timing, the heap is settled with three forced collections spaced
// by a short sleep, and the index is pinned across the whole measurement with
// [runtime.KeepAlive], so nothing under test can be collected early and flatter
// the result.
package entryheap

import (
	"fmt"
	"runtime"
	"runtime/metrics"
	"slices"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/btree"
)

// BuildMode selects how the index under measurement is populated.
type BuildMode string

const (
	// BuildInsert populates the index one key at a time through
	// [btree.Index.Insert], the incremental path every live write takes.
	BuildInsert BuildMode = "insert"
	// BuildBulk populates it through [btree.Index.BulkLoadSorted], the O(n)
	// bottom-up packer that snapshot recovery and Deserialize use.
	BuildBulk BuildMode = "bulk"
)

// KeyOrder selects the order in which keys are inserted. It changes the order
// the per-key payloads are ALLOCATED in, which is the variable a slab allocator
// is sensitive to; it does not change the resulting index.
type KeyOrder string

const (
	// OrderAscending inserts 0, 1, 2, ... so allocation order equals key order
	// and payloads adjacent in memory are adjacent in the tree.
	OrderAscending KeyOrder = "asc"
	// OrderStrided inserts (step * stride) mod Keys for a stride coprime with
	// Keys. That is a bijection, so the index ends up holding exactly the same
	// keys, but consecutive insertions land stride apart in key space — the
	// case most ADVERSE to a slab allocator, because payloads that share a slab
	// are then maximally far apart in the tree and share no locality with it.
	//
	// It is deliberately an affine cycle and not a materialised random
	// permutation: a permutation of 10M int64 is 80 MB that would still be
	// reachable while the heap is weighed, contaminating bytes-per-key by ~8 B.
	// The stride is computed, so the order costs no memory at all.
	OrderStrided KeyOrder = "strided"
)

// Config describes one measurement. The zero value is not usable; see
// [Config.normalise] for the defaults applied to unset fields.
type Config struct {
	// Keys is the number of distinct keys the index holds.
	Keys int
	// GCs is the number of forced collections timed after the heap settles.
	GCs int
	// Build selects the population path.
	Build BuildMode
	// Order selects the insertion order (BuildInsert only; a bulk load is
	// sorted by contract).
	Order KeyOrder
	// KeepOneIn, when >= 2, runs the deletion phase described in the package
	// documentation, keeping every KeepOneIn-th insertion and deleting the
	// rest. Zero or one skips the phase.
	KeepOneIn int
}

func (c *Config) normalise() {
	if c.Keys <= 0 {
		c.Keys = 1_000_000
	}
	if c.GCs <= 0 {
		c.GCs = 9
	}
	if c.Build == "" {
		c.Build = BuildInsert
	}
	if c.Order == "" {
		c.Order = OrderAscending
	}
}

// Sample is one bracketed observation of the live heap.
type Sample struct {
	// Phase names the point in the measurement: "built" or "pruned".
	Phase string
	// LiveKeys is the number of distinct keys the index holds at this phase.
	LiveKeys int

	HeapObjectBytes uint64
	HeapObjects     uint64
	ScanHeapBytes   uint64

	BytesPerKey     float64
	ObjectsPerKey   float64
	ScanBytesPerKey float64

	// GCWallMillis holds every timed forced-collection wall clock, ascending.
	GCWallMillis []float64
	// MarkCPUMillis holds the mark CPU-seconds of each timed collection,
	// converted to milliseconds, ascending. Index i does NOT correspond to
	// GCWallMillis[i]; both are sorted independently so the minimum and median
	// of each are directly readable.
	MarkCPUMillis []float64
}

// Min returns the smallest value of s, or 0 when s is empty. The minimum is the
// least-contaminated sample of a repeated timing: every source of interference
// on a shared host — a competing process, a frequency drop, a migration to an
// efficiency core — can only make a sample slower, never faster.
func Min(s []float64) float64 {
	if len(s) == 0 {
		return 0
	}
	return s[0]
}

// Median returns the middle value of the ascending slice s, or 0 when empty.
func Median(s []float64) float64 {
	if len(s) == 0 {
		return 0
	}
	return s[len(s)/2]
}

// counter names read for every bracketed observation. The order is fixed; the
// index constants below address them.
var counters = []string{
	"/memory/classes/heap/objects:bytes",
	"/gc/heap/objects:objects",
	"/gc/scan/heap:bytes",
	"/cpu/classes/gc/mark/dedicated:cpu-seconds",
	"/cpu/classes/gc/mark/assist:cpu-seconds",
	"/cpu/classes/gc/mark/idle:cpu-seconds",
}

const (
	cHeapBytes = iota
	cHeapObjects
	cScanHeap
	cMarkDedicated
	cMarkAssist
	cMarkIdle
)

func readCounters() []metrics.Sample {
	s := make([]metrics.Sample, len(counters))
	for i, n := range counters {
		s[i].Name = n
	}
	metrics.Read(s)
	return s
}

func u64(s metrics.Sample) uint64 {
	if s.Value.Kind() == metrics.KindUint64 {
		return s.Value.Uint64()
	}
	return 0
}

func secs(s metrics.Sample) float64 {
	if s.Value.Kind() == metrics.KindFloat64 {
		return s.Value.Float64()
	}
	return 0
}

func markSeconds(s []metrics.Sample) float64 {
	return secs(s[cMarkDedicated]) + secs(s[cMarkAssist]) + secs(s[cMarkIdle])
}

// settle drives the heap to a quiet, fully collected state: three forced
// collections, each followed by a short sleep so the scavenger and the
// background sweeper finish before the next one starts. Without the settle the
// first timed collection also pays for whatever the build left behind.
func settle() {
	for range 3 {
		runtime.GC()
		time.Sleep(80 * time.Millisecond)
	}
}

// observe settles the heap, times cfg.GCs forced collections with the mark
// counters bracketed around each one, and returns the resulting sample. It does
// not pin anything: the caller owns the lifetime of what is being measured and
// must keep it alive across the call.
func observe(phase string, liveKeys, gcs int) Sample {
	settle()

	walls := make([]float64, 0, gcs)
	marks := make([]float64, 0, gcs)
	for range gcs {
		before := readCounters()
		start := time.Now()
		runtime.GC()
		wall := time.Since(start)
		after := readCounters()
		walls = append(walls, wall.Seconds()*1000)
		marks = append(marks, (markSeconds(after)-markSeconds(before))*1000)
	}
	slices.Sort(walls)
	slices.Sort(marks)

	final := readCounters()
	s := Sample{
		Phase:           phase,
		LiveKeys:        liveKeys,
		HeapObjectBytes: u64(final[cHeapBytes]),
		HeapObjects:     u64(final[cHeapObjects]),
		ScanHeapBytes:   u64(final[cScanHeap]),
		GCWallMillis:    walls,
		MarkCPUMillis:   marks,
	}
	if liveKeys > 0 {
		n := float64(liveKeys)
		s.BytesPerKey = float64(s.HeapObjectBytes) / n
		s.ObjectsPerKey = float64(s.HeapObjects) / n
		s.ScanBytesPerKey = float64(s.ScanHeapBytes) / n
	}
	return s
}

// gcd is the ordinary Euclidean greatest common divisor.
func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// strideFor returns an odd multiplier coprime with n, so step -> (step*stride)
// mod n is a bijection over [0, n). It starts from Knuth's golden-ratio
// constant reduced into range and walks up by two until the gcd is one, which
// terminates because n has finitely many prime factors.
func strideFor(n int) int {
	if n <= 2 {
		return 1
	}
	s := int(2654435761 % uint64(n)) //nolint:gosec // G115: the modulus is a positive int
	if s%2 == 0 {
		s++
	}
	for gcd(s, n) != 1 {
		s += 2
		if s >= n {
			s = 1
		}
	}
	return s
}

// keyFunc returns the key inserted at each step, so the deletion phase can
// prune by ALLOCATION position rather than by key. It materialises nothing: see
// [OrderStrided] for why that matters to the measurement.
func keyFunc(cfg Config) func(step int) int64 {
	if cfg.Order != OrderStrided {
		return func(step int) int64 { return int64(step) }
	}
	stride, n := strideFor(cfg.Keys), cfg.Keys
	return func(step int) int64 { return int64((step * stride) % n) }
}

// InsertionKeys returns the key inserted at each step, in order, for cfg.
//
// It exists so the harness's OWN insertion order can be tested for
// bijectivity: a stride sharing a factor with the key count would silently
// build a smaller index than the configuration asked for, and every per-key
// number would then be divided by the wrong denominator. It materialises a
// slice and is therefore never used inside a measurement — see [OrderStrided].
func InsertionKeys(cfg Config) ([]int64, error) {
	cfg.normalise()
	if cfg.Keys > 1<<24 {
		return nil, fmt.Errorf("entryheap: InsertionKeys is a test helper; %d keys is too many to materialise", cfg.Keys)
	}
	at := keyFunc(cfg)
	out := make([]int64, cfg.Keys)
	for step := range out {
		out[step] = at(step)
	}
	return out, nil
}

// buildBulk populates idx through the O(n) bottom-up packer. The two input
// slices are scoped to this function so they are unreachable — and therefore
// not weighed with the index — by the time the caller settles the heap.
func buildBulk(idx *btree.Index[int64], keys int) error {
	values := make([]int64, keys)
	nodes := make([]graph.NodeID, keys)
	for i := range values {
		values[i] = int64(i)
		nodes[i] = graph.NodeID(uint64(i)) //nolint:gosec // G115: i is a bounded non-negative loop index
	}
	if err := idx.BulkLoadSorted(values, nodes); err != nil {
		return fmt.Errorf("entryheap: bulk load: %w", err)
	}
	return nil
}

// Measure builds one index to cfg and returns one Sample per phase: always
// "built", plus "pruned" when cfg.KeepOneIn asks for the deletion phase.
//
// The index is kept alive across every observation, so no sample can be
// flattered by the collector reclaiming the very thing under measurement.
func Measure(cfg Config) ([]Sample, error) {
	cfg.normalise()
	if cfg.Build == BuildBulk && cfg.Order == OrderStrided {
		return nil, fmt.Errorf("entryheap: %s build takes sorted input, %s order is not applicable", BuildBulk, OrderStrided)
	}

	idx := btree.New[int64]()
	keyAt := keyFunc(cfg)

	switch cfg.Build {
	case BuildBulk:
		if err := buildBulk(idx, cfg.Keys); err != nil {
			return nil, err
		}
	case BuildInsert:
		for step := range cfg.Keys {
			k := keyAt(step)
			idx.Insert(k, graph.NodeID(uint64(k))) //nolint:gosec // G115: k is a bounded non-negative key
		}
	default:
		return nil, fmt.Errorf("entryheap: unknown build mode %q", cfg.Build)
	}

	samples := []Sample{observe("built", cfg.Keys, cfg.GCs)}

	if cfg.KeepOneIn >= 2 {
		survivors := 0
		for step := range cfg.Keys {
			if step%cfg.KeepOneIn == 0 {
				survivors++
				continue
			}
			k := keyAt(step)
			idx.Delete(k, graph.NodeID(uint64(k))) //nolint:gosec // G115: k is a bounded non-negative key
		}
		if got := idx.DistinctValues(); got != survivors {
			return nil, fmt.Errorf("entryheap: pruned index holds %d keys, want %d", got, survivors)
		}
		samples = append(samples, observe("pruned", survivors, cfg.GCs))
	}

	runtime.KeepAlive(idx)
	return samples, nil
}
