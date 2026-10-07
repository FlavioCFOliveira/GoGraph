package wal

import (
	"math/rand/v2"
	"sync"
	"time"
)

// SyncLatency adds a seeded, randomised delay before every WAL data fsync and
// every directory fsync a [Writer] performs. It is a TESTING instrument
// (rmp #3022), not a production setting.
//
// # Why it exists
//
// A RAM drive shrinks an fsync to microseconds, and with it the window between a
// commit's timestamp allocation and its visibility publish, which is where
// commit-ordering races live. A test that needs the RAM drive's speed but must
// still open that window as a real device does installs a SyncLatency: each
// fsync then waits a duration drawn uniformly from [min, max] before it runs.
// Measured on rmp #2971's reproduction (the sessionless body of
// cypher.TestIndexBuild_CreateDropUnderCommittingWriters, 30 runs each): 0
// failures on the RAM drive without it, 3 with 1-5 ms, 2 on an APFS SSD.
//
// # How it is activated
//
// Explicitly, per Writer: [OpenWithSyncLatency], or store.Options.SyncLatency for
// store.Open. There is no global switch and no environment variable in this
// package. A Writer opened any other way carries a nil SyncLatency and pays one
// nil-pointer test per fsync, nothing more.
//
// # Reproducibility
//
// The delays are drawn from a PCG generator seeded with [SyncLatency.Seed], so a
// seed reproduces the sequence of delays. It does not reproduce goroutine
// scheduling, so it makes a failing interleaving likely again rather than
// certain.
//
// Concurrency: safe for concurrent use; one SyncLatency may be shared by several
// Writers, which then draw from one sequence.
type SyncLatency struct {
	seed     uint64
	min, max time.Duration
	mu       sync.Mutex
	rng      *rand.Rand
}

// NewSyncLatency returns a SyncLatency that delays each fsync by a duration drawn
// uniformly from [lo, hi], seeded with seed. A negative bound is treated as 0, and
// hi below lo is raised to lo.
func NewSyncLatency(seed uint64, lo, hi time.Duration) *SyncLatency {
	lo = max(lo, 0)
	hi = max(hi, lo)
	return &SyncLatency{
		seed: seed,
		min:  lo,
		max:  hi,
		rng:  rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)), //nolint:gosec // a test delay, not a secret
	}
}

// Seed returns the seed the delays are drawn from, for a test to report on
// failure.
func (l *SyncLatency) Seed() uint64 { return l.seed }

// Bounds returns the delay interval [lo, hi].
func (l *SyncLatency) Bounds() (lo, hi time.Duration) { return l.min, l.max }

// next draws the next delay in the sequence.
func (l *SyncLatency) next() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	d := l.min
	if span := l.max - l.min; span > 0 {
		d += time.Duration(l.rng.Int64N(int64(span) + 1))
	}
	return d
}

// wait sleeps for the next delay in the sequence.
func (l *SyncLatency) wait() { time.Sleep(l.next()) }

// OpenWithSyncLatency is [Open] with lat installed on the returned Writer: every
// data fsync of the WAL and every directory fsync it performs, including those
// of the open itself, first waits a delay drawn from lat. A nil lat makes it
// exactly [Open]. It is a testing entry point; see [SyncLatency].
func OpenWithSyncLatency(path string, lat *SyncLatency) (*Writer, error) {
	return OpenWithOptions(path, Options{SyncLatency: lat})
}

// dataSyncFile is the commit-path data fsync: [dataSync] on the Writer's file,
// after the injected delay when a [SyncLatency] is installed.
func (w *Writer) dataSyncFile() error {
	if l := w.syncLatency; l != nil {
		l.wait()
	}
	return dataSync(w.f)
}
