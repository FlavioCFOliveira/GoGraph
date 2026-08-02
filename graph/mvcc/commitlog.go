package mvcc

// commitlog.go — which allocated commit timestamps have finished, so a reader
// can be handed an instant below which nothing is still in flight (rmp #2298).
//
// # The problem it exists to solve
//
// Committing is two steps: allocate a timestamp ([Clock.NextCommitTS]), then
// announce it ([Clock.PublishCommitTS]). While commits are serialised by a
// global write barrier those two steps happen in allocation order, so "the
// highest timestamp published" and "the highest timestamp below which
// everything is published" are the same number and one monotone counter
// suffices. That is what [Clock.ReadTS] used to return, and
// docs/audit-mvcc-sole-cc-2026-08-02.md §4.1 records it as the first thing that
// has to change before two writers may commit at once.
//
// Once they may, the two numbers diverge and the difference is a wrong answer.
// Writer A allocates 4 and starts its fsync; writer B allocates 5 and finishes
// first. A reader starting at "highest published" would take 5 — and see
// commit 5 while commit 4, which was allocated EARLIER, is still invisible. It
// straddles a commit, exactly the torn read Example 27's bank-transfer
// invariant caught during rmp #2290 (see [Clock.ReadTS]), and when A finally
// publishes, 4 becomes visible to a reader that already reported a state
// without it.
//
// # The shape, and why this one
//
// Two reference implementations solve this, and they solve it differently.
//
// PostgreSQL builds a per-snapshot LIST of what is in flight. GetSnapshotData
// (postgres/postgres, branch REL_17_STABLE, read 2026-08-02;
// src/backend/storage/ipc/procarray.c) walks the proc array and fills xmin,
// xmax and an xip[] array of running xids; visibility is then
// XidInMVCCSnapshot: below xmin visible, at or above xmax invisible, otherwise
// SEARCH THE ARRAY. The array is sized from GetMaxSnapshotXidCount(), i.e.
// max_connections, and the comment there is explicit that allocating for
// maxProcs is "usually overkill" but is done to avoid holding a lock.
//
// Memgraph keeps a BITSET of finished ids instead. CommitLog
// (memgraph/memgraph, branch master, read 2026-08-02;
// src/storage/v2/commit_log.hpp and .cpp) is a singly-linked chain of blocks,
// each `uint64_t field[kBlockSize]` with kBlockSize = 8192, so 524 288 ids per
// block; MarkFinished sets a bit and, when the id was the oldest active one,
// UpdateOldestActive rescans forward; a block whose every word is
// numeric_limits<uint64_t>::max() is deallocated and head_start_ advances by
// kIdsInBlock. OldestActive() is then a single field read under a spin lock.
//
// GoGraph takes MEMGRAPH'S SHAPE, for three reasons that are about this engine
// and not about taste:
//
//  1. THE READ PATH DOES NOT MOVE. With a contiguous frontier, [Clock.ReadTS]
//     stays one atomic load and [Visible] stays one comparison — the read-side
//     test that runs per version-chain node on every versioned read. Adopting
//     xip would put an xmin/xmax pair plus an array search on that path.
//     PostgreSQL can afford it: its visibility test runs per heap tuple behind
//     a current-transaction fast path, and its snapshots are long-lived enough
//     to amortise building the list.
//
//  2. GOGRAPH'S IN-FLIGHT WINDOW IS A COMMIT, NOT A TRANSACTION. The commit
//     timestamp is allocated at commit time (graph/lpg/mvcc_write.go:91), not
//     at BEGIN, so the set of allocated-but-unfinished timestamps only ever
//     holds transactions inside their commit critical section. PostgreSQL's
//     xip exists because a backend holds an xid from its first write until
//     commit — possibly minutes — and excluding everything above the oldest
//     running xid would make every snapshot uselessly stale. That reasoning
//     does not transfer.
//
//  3. THE MEMORY BOUND IS STRUCTURAL RATHER THAN CONFIGURED. A block is
//     released as soon as every id in it has finished, so what is retained is
//     the window between the oldest unfinished timestamp and the newest
//     allocated one — bounded by how many writers are committing at once.
//     PostgreSQL bounds xip by max_connections and pays that allocation per
//     snapshot; GoGraph would pay it per read.
//
// THE COST, STATED PLAINLY. A reader cannot observe a commit above the oldest
// unfinished one even when it has already published. The staleness is the
// duration of the longest in-flight commit — a WAL fsync, measured at 3.73 ms
// in docs/benchmarks/mvcc-write-scaling-2026-08-02.md. Memgraph accepts exactly
// this trade, and it buys a hot path that does not grow.

import "math/bits"

const (
	// clWordBits is how many timestamps one bitmap word carries.
	clWordBits = 64
	// clWordsPerBlock sizes one block at 512 bytes of bitmap, which is 4096
	// timestamps. Memgraph uses 8192 words (524 288 ids) per block because its
	// ids are transaction ids, allocated at BEGIN and therefore live for the
	// whole transaction. GoGraph's are commit timestamps allocated at commit,
	// so the live window is the number of writers inside their commit section —
	// tens, not hundreds of thousands. A block two orders of magnitude smaller
	// keeps the resident bitmap at half a kilobyte in the normal case while
	// still absorbing 4096 commits before a single allocation.
	clWordsPerBlock = 64
	// clIDsPerBlock is how many timestamps one block addresses.
	clIDsPerBlock = clWordsPerBlock * clWordBits
	// clAllFinished is a word in which every timestamp has finished.
	clAllFinished = ^uint64(0)
)

// clBlock is one run of [clIDsPerBlock] consecutive timestamps.
type clBlock struct {
	next  *clBlock
	words [clWordsPerBlock]uint64
}

// commitLog records which allocated commit timestamps have FINISHED — whether
// they committed or were abandoned — and reports the newest instant below which
// none is still in flight.
//
// It is NOT safe for concurrent use; [Clock] guards it with a mutex taken only
// on the publish path, once per commit, never on a read.
type commitLog struct {
	// head addresses timestamps [headStart, headStart+clIDsPerBlock).
	head *clBlock
	// headStart is the timestamp of bit 0 of head. It only ever increases, by
	// whole blocks, as fully-finished blocks are retired.
	headStart uint64
	// oldest is the smallest allocated timestamp that has NOT finished. Every
	// timestamp below it has, so oldest-1 is the frontier a reader may start
	// at. Timestamps are allocated from 1, so it starts at 1 and the initial
	// frontier is 0 — nothing committed.
	oldest uint64
}

// frontier is the newest timestamp below which nothing is in flight.
func (l *commitLog) frontier() uint64 {
	if l.oldest == 0 {
		return 0 // zero value: nothing allocated yet
	}
	return l.oldest - 1
}

// finish records that ts will never be published again — it either committed or
// was abandoned — and returns the resulting frontier.
//
// A timestamp already swept past is ignored rather than treated as an error:
// the frontier has moved beyond it, so re-marking it could only corrupt a block
// that no longer describes it.
func (l *commitLog) finish(ts uint64) uint64 {
	if l.oldest == 0 {
		// Zero value. Timestamps are allocated from 1.
		l.oldest, l.headStart = 1, 1
	}
	if ts < l.oldest {
		return l.frontier()
	}
	block, start := l.blockFor(ts)
	off := ts - start
	block.words[off/clWordBits] |= 1 << (off % clWordBits)
	if ts == l.oldest {
		l.advance()
	}
	return l.frontier()
}

// blockFor returns the block addressing ts, and the timestamp of its bit 0,
// extending the chain as far as necessary.
//
// It walks from head, which is O(live blocks) — one in the normal case, since a
// block covers 4096 commits and is retired as soon as it is fully finished.
func (l *commitLog) blockFor(ts uint64) (*clBlock, uint64) {
	if l.head == nil {
		l.head = &clBlock{}
	}
	b, start := l.head, l.headStart
	for ts >= start+clIDsPerBlock {
		if b.next == nil {
			b.next = &clBlock{}
		}
		b, start = b.next, start+clIDsPerBlock
	}
	return b, start
}

// advance moves oldest past every contiguously finished timestamp, retiring any
// block it leaves entirely behind.
//
// It is called only when the timestamp just finished WAS the oldest unfinished
// one, so the common case walks a handful of bits: the frontier is at most as
// far behind as the number of writers committing at once.
func (l *commitLog) advance() {
	for l.head != nil {
		off := l.oldest - l.headStart
		if off >= clIDsPerBlock {
			l.retireHead()
			continue
		}
		word, bit := off/clWordBits, off%clWordBits
		w := l.head.words[word]
		if bit == 0 && w == clAllFinished {
			// Whole word finished: skip it without touching 64 bits one by one.
			l.oldest += clWordBits
			continue
		}
		// The first clear bit at or above `bit` is where the frontier stops.
		// Inverting and masking off the bits below `bit` turns that into one
		// trailing-zeros instruction.
		unfinished := ^w &^ ((1 << bit) - 1)
		if unfinished == 0 {
			l.oldest += clWordBits - bit // rest of the word is finished
			continue
		}
		l.oldest += uint64(bits.TrailingZeros64(unfinished)) - bit
		return
	}
}

// retireHead drops the head block, whose every timestamp has finished, and
// rebases the log on the next one. This is what bounds the memory: what is
// retained is the window between the oldest unfinished timestamp and the newest
// allocated one, not the history of every commit.
func (l *commitLog) retireHead() {
	if l.head.next == nil {
		// Nothing follows: reuse the block rather than churn an allocation, and
		// rebase it on the next run of timestamps.
		*l.head = clBlock{}
		l.headStart += clIDsPerBlock
		return
	}
	l.head = l.head.next
	l.headStart += clIDsPerBlock
}

// liveBlocks reports how many blocks the log is holding. Exposed for the
// memory-bound test; a growing value means a timestamp finished late or never.
func (l *commitLog) liveBlocks() int {
	n := 0
	for b := l.head; b != nil; b = b.next {
		n++
	}
	return n
}
