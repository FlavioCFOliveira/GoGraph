// Package anomaly classifies an observed transaction history against the
// standard isolation phenomena, so a sighting names a MECHANISM instead of
// starting a search.
//
// # Why it exists
//
// When the DST and example batteries observe an isolation failure they report a
// domain symptom — "readers observed a torn total; first torn value X, expected
// Y" — and nothing else. That symptom is compatible with several distinct
// defects with different root causes, which is a direct cause of what rmp #2333
// cost and of why rmp #2336 is still open: the evidence names what LOOKED wrong,
// not what was VIOLATED.
//
// This package turns a recorded history into a named, classified violation.
//
// # The formalism, and its sources
//
// The model is Adya's dependency-graph formulation, which is the basis both of
// the ANSI-critique literature and of Elle, the checker Jepsen uses:
//
//   - Adya, Liskov & O'Neil, "Generalized Isolation Level Definitions",
//     ICDE 2000 — the direct serialization graph (DSG) over read-, write- and
//     anti-dependencies, and the phenomena G0, G1a, G1b, G1c, G2 and G2-item.
//     Snapshot isolation is PL-SI there.
//   - Berenson, Bernstein, Gray, Melton & O'Neil, "A Critique of ANSI SQL
//     Isolation Levels", SIGMOD 1995 — P4 lost update and A5B write skew, and
//     the observation that snapshot isolation permits the latter.
//   - Kingsbury & Alvaro, "Elle: Inferring Isolation Anomalies from
//     Experimental Observations", VLDB 2020, and its implementation
//     (github.com/jepsen-io/elle, src/elle/consistency_model.clj, read
//     2026-08-08) — the machine-checked anomaly lattice this package's level
//     boundaries were verified against rather than reconstructed from memory.
//   - Cerone, Bernardi & Gotsman, "A Framework for Transactional Consistency
//     Models with Atomic Visibility", CONCUR 2015 — the characterisation of
//     generalized snapshot isolation that makes G-nonadjacent, not merely
//     G-single, the right boundary. See [SnapshotIsolation].
//
// # Concurrency
//
// A [History] is plain data and safe to share once built. [Recorder] is safe for
// concurrent use; [Check] is a pure function of its input.
package anomaly

import (
	"fmt"
	"sort"
	"sync"
)

// TxID identifies a transaction within one history.
type TxID uint64

// Version identifies one version of one key. Versions of a key are totally
// ordered by their numeric value, which is what makes GoGraph's MVCC commit
// timestamp usable directly: the version order Adya's model requires is the
// commit order the engine already maintains.
type Version uint64

// InitVersion is the version every key holds before any transaction in the
// history writes it. It is written by no transaction, so a read of it creates
// no read-dependency — which is correct: nothing in the history produced it.
const InitVersion Version = 0

// OpKind distinguishes a read from a write.
type OpKind uint8

const (
	// Read observes a version of a key.
	Read OpKind = iota
	// Write installs a version of a key.
	Write
)

func (k OpKind) String() string {
	if k == Read {
		return "r"
	}
	return "w"
}

// Op is one read or write of one key.
type Op struct {
	// Key is the object read or written.
	Key string
	// Ver is the version observed (for a Read) or installed (for a Write).
	Ver Version
	// Kind selects read or write.
	Kind OpKind
}

func (o Op) String() string { return fmt.Sprintf("%s(%s=%d)", o.Kind, o.Key, o.Ver) }

// Txn is one transaction's observed history: what it read, what it wrote, when
// it began, and how it ended.
type Txn struct {
	// Ops are this transaction's operations in the order it issued them. Order
	// matters: an intermediate read (G1b) is defined by which of a transaction's
	// writes to a key was its LAST one.
	Ops []Op
	// ID identifies the transaction.
	ID TxID
	// Start is the instant the transaction took its snapshot.
	Start uint64
	// Commit is the instant the transaction became visible, or 0 when it aborted
	// or never finished.
	Commit uint64
	// Aborted records that the transaction did not commit. An aborted
	// transaction is NOT a node of the dependency graph — Adya's DSG is over
	// committed transactions — but its writes are still what a G1a aborted read
	// observes, which is why it stays in the history.
	Aborted bool
}

// History is a set of transactions observed from one execution.
type History struct {
	Txns []Txn
}

// Validate reports whether the history is self-consistent enough to be checked.
//
// It is not a formality. Every failure it catches would otherwise surface as a
// confident, wrong classification: two transactions claiming the same version of
// a key make the version order ambiguous, so the anti-dependency edges — and
// therefore every G2 verdict — would be drawn from a graph that does not
// describe any execution. A checker that answers from a malformed history is
// worse than no checker, because its answer is believed.
func (h *History) Validate() error {
	seenTx := make(map[TxID]struct{}, len(h.Txns))
	writer := make(map[verKey]TxID, len(h.Txns)*2)
	for i := range h.Txns {
		t := &h.Txns[i]
		if _, dup := seenTx[t.ID]; dup {
			return fmt.Errorf("anomaly: transaction %d appears twice", t.ID)
		}
		seenTx[t.ID] = struct{}{}
		if t.Aborted && t.Commit != 0 {
			return fmt.Errorf("anomaly: transaction %d is marked aborted but carries commit instant %d", t.ID, t.Commit)
		}
		for _, op := range t.Ops {
			if op.Key == "" {
				return fmt.Errorf("anomaly: transaction %d has an operation with no key", t.ID)
			}
			if op.Kind != Write {
				continue
			}
			if op.Ver == InitVersion {
				return fmt.Errorf("anomaly: transaction %d writes key %q at the initial version, "+
					"which by definition no transaction wrote", t.ID, op.Key)
			}
			k := verKey{op.Key, op.Ver}
			if other, dup := writer[k]; dup && other != t.ID {
				return fmt.Errorf("anomaly: key %q version %d is claimed by both transaction %d and %d; "+
					"the version order is ambiguous and every anti-dependency drawn from it would be fiction",
					op.Key, op.Ver, other, t.ID)
			}
			writer[k] = t.ID
		}
	}
	return nil
}

// verKey identifies one version of one key.
type verKey struct {
	key string
	ver Version
}

// Recorder accumulates a history from a running workload.
//
// It is SHARDED AND PRE-SIZED, and neither is a micro-optimisation. Measured
// (rmp #2341 AC5, interleaved arms, 30 repetitions of a 6-writer/6-reader
// defective workload, ratio of anomaly rate with recorder to without):
//
//	one mutex-guarded slice   0.877 0.844 0.817 0.814 0.937 0.898  mean 0.865
//	one shard per goroutine   0.993 1.021 0.854 0.841 0.776 0.993  mean 0.913
//	sharded and pre-sized     1.159 1.163 0.937 0.821 0.881 1.002  mean 0.994
//
// The first row is a REAL 13% suppression, not noise: every one of the six runs
// is below 1.0. A lock taken at the end of every transaction serialises the
// writers just enough to reduce their overlap, and reduced overlap is reduced
// tearing — the module's recorded lesson happening again in miniature, a probe
// that quietens the defect it was added to find. Sharding removed the shared
// word; pre-sizing removed the reallocation traffic that was left; the third row
// straddles 1.0 symmetrically and there is no effect left to find.
//
// It took an INTERLEAVED design to see any of this. Run as twelve recorded
// repetitions followed by twelve unrecorded ones, the same shared-lock recorder
// measured 0.95, 0.74, 1.27, 0.73, 0.86 — a spread wide enough to look like
// noise, because the ordering drift was larger than the effect.
//
// Recorder is safe for concurrent use. A [Shard] is NOT: it belongs to one
// goroutine, which is what makes it lock-free.
type Recorder struct {
	mu     sync.Mutex
	shards []*Shard
}

// Shard is one goroutine's private append buffer.
//
// Obtain one per producing goroutine with [Recorder.Shard] and use it for that
// goroutine's whole life. Recording through it touches no shared memory, so it
// cannot perturb the timing of what it observes.
//
// Shard is NOT safe for concurrent use.
type Shard struct {
	txns []Txn
	_    [64]byte // pad to a cache line so two shards never share one
}

// Shard returns a fresh private buffer belonging to the calling goroutine,
// pre-sized for hint transactions.
//
// The hint matters for the same reason the sharding does: a growing append
// reallocates and copies, and that memory traffic is the residual perturbation
// left after the shared lock was removed. Pass the number of transactions the
// goroutine will record when it is known; pass 0 and the buffer grows as usual.
//
// Safe for concurrent use: only the registration is shared, and it happens once
// per goroutine rather than once per transaction.
func (r *Recorder) Shard(hint int) *Shard {
	s := &Shard{}
	if hint > 0 {
		s.txns = make([]Txn, 0, hint)
	}
	r.mu.Lock()
	r.shards = append(r.shards, s)
	r.mu.Unlock()
	return s
}

// Record appends one finished transaction. No lock, no atomic, no shared write.
func (s *Shard) Record(t Txn) {
	if s == nil {
		return
	}
	s.txns = append(s.txns, t)
}

// Record appends one finished transaction directly, taking the registry lock.
//
// It exists for the callers that record from a single goroutine — a fixture
// build, a sequential replay — where a shard would be ceremony. Anything on a
// concurrent path must use [Recorder.Shard] instead; see the type doc for what
// the shared lock measured.
func (r *Recorder) Record(t Txn) {
	r.mu.Lock()
	if len(r.shards) == 0 {
		r.shards = append(r.shards, &Shard{})
	}
	r.shards[0].txns = append(r.shards[0].txns, t)
	r.mu.Unlock()
}

// History returns the accumulated history, with transactions in a deterministic
// order (by commit instant, then by id) so that two runs of the same execution
// yield the same history and therefore the same classification.
//
// Call it after the workload has finished: it reads every shard without
// synchronising against its owner, because the point of a shard is that nothing
// synchronises on the recording path.
func (r *Recorder) History() History {
	r.mu.Lock()
	shards := make([]*Shard, len(r.shards))
	copy(shards, r.shards)
	r.mu.Unlock()

	n := 0
	for _, s := range shards {
		n += len(s.txns)
	}
	out := make([]Txn, 0, n)
	for _, s := range shards {
		out = append(out, s.txns...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Commit != out[j].Commit {
			return out[i].Commit < out[j].Commit
		}
		return out[i].ID < out[j].ID
	})
	return History{Txns: out}
}

// Len reports how many transactions have been recorded.
func (r *Recorder) Len() int {
	r.mu.Lock()
	n := 0
	for _, s := range r.shards {
		n += len(s.txns)
	}
	r.mu.Unlock()
	return n
}
