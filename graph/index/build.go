package index

// build.go — concurrent index build: the catch-up log (rmp #2738).
//
// # The defect
//
// Building a secondary index is two steps that cannot be one: SCAN the existing
// graph into a fresh index, then REGISTER that index so the change fan-out
// starts maintaining it. A write that commits between the two reaches neither.
// The scan's node list predates it, and the fan-out finds no index to write it
// to, so the committed write is missing from the index PERMANENTLY: a label scan
// returns the node and an index seek over the same predicate does not.
//
// The engine used to close that window by EXCLUSION — the DDL holds its schema
// gate across the whole scan-and-register sequence, so no writer runs inside it.
// That works for an autocommit statement, which takes the gate for its own
// duration. It does NOT work for an explicit transaction, which is a registered
// store writer from BEGIN and therefore cannot block on that gate without
// closing a three-way cycle with the store's quiesce. Measured at 20 losses in
// 20 trials on the explicit-transaction path against 0 in 20 for the identical
// autocommit workload.
//
// # The mechanism
//
// A [BuildLog] records every change the manager fans out while a build is in
// flight, and [Manager.FinishBuild] replays the recording into the freshly built
// index BEFORE registering it — all under the manager's exclusive lock, so no
// fan-out can interleave between the replay and the registration.
//
// The result is that every change is applied to the new index exactly once by
// one of three routes, and the three are exhaustive:
//
//   - committed BEFORE [Manager.BeginBuild] — already in the graph, so the scan
//     reads it;
//   - committed DURING the build — recorded here and replayed by FinishBuild;
//   - committed AFTER FinishBuild returns — the index is registered, so the
//     fan-out delivers it.
//
// There is no fourth case, because recording and fan-out happen under one hold
// of the manager's lock: a change is recorded, or it is delivered to an index
// already registered, and FinishBuild's exclusive hold means it can never fall
// between the two.
//
// # WHEN a recorded change is resolved, and why it is not at replay (rmp #2793)
//
// A [Change] does not describe its own effect on an index. A bound index answers
// two further questions about the changed node from the GRAPH: whether the node
// is eligible for the index (live, and carrying the bound label), and — for a
// label add or remove, which carries no property payload — what its current
// value of the bound property is.
//
// Those answers belong to the INSTANT THE CHANGE WAS FANNED OUT. Asking them at
// replay time instead asks about a later instant, and the graph at that later
// instant is not the committed state: a statement on an explicit transaction
// applies EAGERLY, so the live property bag, label bitmap and node-life records
// carry the writes of every open transaction, and [Manager.FinishBuild] runs
// long after the recording. rmp #2793 measured all three doors on the hash path
// — an open transaction's uncommitted SET made the replay of a label add insert
// 'ghost' (1 entry, want 0) and lose the committed 'real-x' (0 entries, want 1);
// an uncommitted DETACH DELETE made the replay of a property set drop the
// committed value (0 entries, want 1); an uncommitted label add made it
// fabricate an entry for a node the graph holds no such label for. The
// interfering transaction then rolled back, and because its own
// [exec.IndexBuffer] describes changes that were never fanned out, nothing
// inverted what the replay had written.
//
// So the log resolves at RECORD time and replays the answer as data: a
// [BuildResolver] supplied to [Manager.BeginBuild] captures the two answers
// beside the change, and [ResolvedApplier] hands them back at replay. The
// property that buys is exact rather than approximate — the replay produces
// PRECISELY the effects the live fan-out would have produced had the index been
// registered all along, because it uses the same rules, the same binding and the
// same resolution instant. This is the same discipline rmp #2778 applied to the
// backfill scan, through the other door: that one bound the scan to a snapshot,
// this one binds the log to the instant it recorded.
//
// A change may legitimately be counted TWICE — committed after BeginBuild but
// before the scan reads its node, so the scan sees it and the replay repeats it.
// That is harmless and is a property the replay relies on rather than avoids: an
// index entry is a set membership, so a repeated Insert is idempotent, and a
// Delete of a value that is not present is a no-op.
//
// # Why a log, where PostgreSQL uses a second table scan
//
// PostgreSQL's CREATE INDEX CONCURRENTLY solves the same problem with a
// three-phase flag ladder and TWO full table scans: the first indexes an MVCC
// snapshot, then indisready is published so writers maintain the index, then a
// reference snapshot is taken and validate_index merge-joins the sorted index
// TIDs against the heap to insert what is missing (postgres REL_17_2, commit
// 6304632eaa2107bb1763d29e213ff166ff6104c0 — src/backend/commands/indexcmds.c
// DefineIndex, src/backend/catalog/index.c validate_index and its header
// comment at index.c:3225-3287).
//
// GoGraph cannot borrow that shape, and does not need to. It cannot, because
// PostgreSQL's second pass ONLY INSERTS: validate_index_callback "never actually
// delete anything" (index.c:3421-3430), which is sound there only because an
// index entry points at a heap tuple whose visibility is rechecked on every
// fetch, so a stale entry is harmless. A GoGraph index entry is answered without
// any such recheck, so a design that tolerates stale entries would answer with
// rows the graph does not hold.
//
// It does not need to, because the two systems' windows differ by orders of
// magnitude. PostgreSQL's build may run for hours over a multi-terabyte table
// with unrestricted concurrent DML, so there is nowhere to put a log of the
// concurrent writes — hence the brute-force second scan its own source calls
// exactly that (index.c:3283-3286). GoGraph's window is one backfill of one
// in-memory graph, during which the DDL's schema gate already excludes every
// autocommit writer; only explicit transactions can commit into it. Recording
// those changes is cheap, exact, and needs no snapshot machinery, no transaction
// horizon to wait on, and no partially-usable index state for a reader to
// mistakenly consult.
//
// That last point is the reason this design is preferred here rather than merely
// cheaper. PostgreSQL must publish the index into its catalogue before the build
// and then keep readers away from it with indisvalid=false, checked by the
// planner (src/backend/optimizer/util/plancat.c:256-267). Every reader route has
// to honour that flag. Here the half-built index is simply NOT IN the manager's
// map: [Manager.GetIndex], [Manager.ListIndexes] and [Manager.Count] are the only
// ways to reach a registered subscriber, and an index under construction is in
// none of them. A partially built index is unreachable by construction rather
// than by a flag every consulting site must remember to check.

import (
	"fmt"
	"sync"
)

// MaxBuildLogChanges bounds the number of changes one [BuildLog] retains.
//
// The bound is required rather than defensive: without it a sustained write
// workload concurrent with a long build would grow the log without limit. A
// recorded entry is a [Change] plus the resolution captured beside it, measured
// at 88 bytes against the Change's own 64, so the ceiling costs about 11 MiB of
// transient memory for a build that is saturated with concurrent writes, and is
// released when the build ends.
//
// Reaching it is a saturation condition, and it is answered with
// [ErrIndexBuildOverflow] rather than by silently truncating: a truncated log
// would register an index missing exactly the writes this mechanism exists to
// catch. Only explicit transactions can commit into the window at all — the
// DDL's schema gate excludes every autocommit writer for the duration — so the
// ceiling corresponds to more than a hundred thousand index changes committed by
// concurrent explicit transactions during a single backfill.
const MaxBuildLogChanges = 1 << 17

// BuildResolver answers, AT THE INSTANT A CHANGE IS FANNED OUT, the two
// questions a bound index would otherwise ask the graph when the change is
// replayed: the changed node's current raw value of the property under build,
// and whether that node is eligible for the index being built (live, and
// carrying the label under build).
//
// current is returned in whatever representation the subscriber's own value
// projection accepts — for the engine's indexes an lpg.PropertyValue — and is
// nil when the node is absent, carries no such property, or the change concerns
// neither the property nor the label under build. eligible carries the same
// verdict the index's own Binding.Eligible would return for the node at this
// instant.
//
// A resolver's calls are SERIALISED — this log's own mutex is held across every
// one — so an implementation need not itself be safe for concurrent use. It is
// nonetheless entered from whichever goroutine fans the change out, so it must
// assume no goroutine affinity, and it runs concurrently with graph writers.
//
// A resolver is called by [Manager.Apply] and [Manager.ApplyBatch], from every
// goroutine that fans a change out, while the manager's lock is held SHARED and
// this log's own mutex is held. It must therefore read the graph exactly as a
// registered subscriber's Apply already does at that point and take no lock of
// its own: the log's mutex is a leaf, and a resolver that acquired something a
// graph writer holds while waiting on the manager's lock would close a cycle.
//
// A nil resolver is legitimate ONLY when nothing to be registered against the
// log resolves anything from the graph — an unbound index, or a test double.
// Every bound index build must supply one; without it [Manager.FinishBuild]
// falls back to [Subscriber.Apply], which re-reads the graph at replay time and
// is exactly the defect rmp #2793 closed (see the file comment).
type BuildResolver func(c Change) (current any, eligible bool)

// recordedChange is one entry of a [BuildLog]: the change as it was fanned out,
// plus the [BuildResolver]'s answers captured at that same instant.
//
// current and eligible are meaningless when the log has no resolver, and
// [Manager.FinishBuild] never reads them in that case.
type recordedChange struct {
	Change
	current  any
	eligible bool
}

// BuildLog records the changes fanned out by a [Manager] while an index is being
// built, so [Manager.FinishBuild] can replay them into that index before it
// becomes reachable. See the file comment for the argument that the replay is
// exact, and for why each change is RESOLVED as it is recorded rather than as it
// is replayed.
//
// A BuildLog is created by [Manager.BeginBuild] and is valid only until
// [Manager.FinishBuild] or [Manager.AbandonBuild] retires it. It is safe for
// concurrent use: the manager records into it from every goroutine that fans a
// change out.
type BuildLog struct {
	// resolve captures each change's graph-dependent facts at the instant it is
	// recorded. It is set once by [Manager.BeginBuild], before the log is
	// reachable by any fan-out, and never mutated afterwards.
	resolve BuildResolver
	changes []recordedChange
	mu      sync.Mutex
	// overflowed latches once more than MaxBuildLogChanges changes have been
	// offered. Once set, the log is no longer a complete account of the window
	// and FinishBuild refuses to register anything built against it.
	overflowed bool
}

// record appends one change. It is called by [Manager.Apply] under the manager's
// shared lock, which several goroutines can hold at once, so the log carries its
// own mutex.
func (b *BuildLog) record(c Change) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.appendLocked(c)
}

// recordBatch appends a whole batch contiguously, mirroring the delivery order
// [Manager.ApplyBatch] gives a registered subscriber.
func (b *BuildLog) recordBatch(changes []Change) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for k := range changes {
		b.appendLocked(changes[k])
	}
}

// appendLocked appends one change together with its resolution, latching
// overflow at the ceiling. Once overflowed the log stops growing: it is already
// useless for its purpose, and continuing to accumulate would spend memory — and
// resolver calls — to no end, which is why the resolver runs only after both
// guards.
func (b *BuildLog) appendLocked(c Change) {
	if b.overflowed {
		return
	}
	if len(b.changes) >= MaxBuildLogChanges {
		b.overflowed = true
		b.changes = nil
		return
	}
	r := recordedChange{Change: c}
	if b.resolve != nil {
		r.current, r.eligible = b.resolve(c)
	}
	b.changes = append(b.changes, r)
}

// Len reports how many changes the log currently holds. It returns 0 for a log
// that has overflowed, whose contents were released. Intended for tests and
// observability.
func (b *BuildLog) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.changes)
}

// Overflowed reports whether more than [MaxBuildLogChanges] changes were fanned
// out during the build, which makes the log an incomplete account of the window.
func (b *BuildLog) Overflowed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.overflowed
}

// RegisterFunc registers one already-built subscriber under name. It is supplied
// by [Manager.FinishBuild] to the closure it runs, and differs from
// [Manager.CreateIndex] in two ways that matter: it replays the build log into
// sub before registering it, and it runs under a lock the caller already holds,
// so several registrations performed through it are indivisible with respect to
// the change fan-out.
//
// It returns the same errors [Manager.CreateIndex] does, so a caller can absorb
// [ErrIndexExists] for an IF NOT EXISTS statement exactly as before.
//
// Concurrency: NOT safe for concurrent use, and valid only for the dynamic
// extent of the [Manager.FinishBuild] call that supplied it. It closes over the
// manager's exclusive lock hold and over the captured replay slice, so it
// carries no synchronisation of its own — that is deliberate, and is what makes
// several registrations through one instance indivisible with respect to the
// change fan-out. Calling it from another goroutine, or retaining it beyond the
// closure it was handed to, escapes the lock hold it assumes and races the
// manager's index map.
type RegisterFunc func(name string, sub Subscriber) error

// BeginBuild starts recording every change the manager fans out, and returns the
// log to pass to [Manager.FinishBuild].
//
// resolve captures, as each change is recorded, the graph-dependent facts a
// bound index would otherwise re-read when the change is replayed; see
// [BuildResolver] for what those are and for the one case in which nil is
// correct.
//
// Call it BEFORE the backfill scan reads anything. A change committed between
// this call and the scan is recorded AND seen by the scan, which is harmless
// (the replay is idempotent); a change committed before this call is seen by the
// scan alone, which is correct. What must not happen is a change committed after
// the scan has read its node and before the index is registered, and that is
// exactly what the recording catches.
//
// Every BeginBuild must be retired by [Manager.FinishBuild] or
// [Manager.AbandonBuild]; a log left active makes every subsequent fan-out pay
// to record into it and never releases the memory. The idiom is a deferred
// AbandonBuild, which is a no-op once FinishBuild has retired the log.
func (m *Manager) BeginBuild(resolve BuildResolver) *BuildLog {
	b := &BuildLog{resolve: resolve}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.builds = append(m.builds, b)
	return b
}

// AbandonBuild retires l without registering anything, discarding the recording.
// It is idempotent and safe on a log [Manager.FinishBuild] has already retired,
// so it can be deferred unconditionally at the point the build starts.
func (m *Manager) AbandonBuild(l *BuildLog) {
	if l == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.retireLocked(l)
	l.mu.Lock()
	l.changes = nil
	l.mu.Unlock()
}

// FinishBuild runs fn with the manager's lock held EXCLUSIVELY, so no change
// fan-out can interleave with anything fn does, and retires l afterwards
// whatever the outcome.
//
// The [RegisterFunc] handed to fn replays l's recording into a subscriber before
// registering it, so an index built while those changes were being fanned out
// catches up on them at the instant it becomes reachable. Deriving the catch-up
// from the registration rather than taking it as a separate argument is
// deliberate: it makes it impossible to register an index and forget to catch it
// up.
//
// The replay goes through [ResolvedApplier.ApplyResolved] whenever the
// subscriber implements it AND the log was given a [BuildResolver], so the
// change is applied from the state captured when it was fanned out. Otherwise it
// goes through [Subscriber.Apply], which resolves against the graph as it stands
// NOW — correct only for a subscriber that resolves nothing from the graph. See
// the file comment for the measurement behind that distinction (rmp #2793).
//
// Registrations performed through that function are indivisible with respect to
// the fan-out — the property rmp #2703 established for an index and its numeric
// companion, here supplied by the manager's own lock rather than by a caller's
// barrier, so it holds on every path that builds an index.
//
// FinishBuild returns [ErrIndexBuildOverflow] without running fn at all when the
// recording is incomplete (see [MaxBuildLogChanges]); nothing is registered and
// the caller's half-built index is simply discarded.
func (m *Manager) FinishBuild(l *BuildLog, fn func(reg RegisterFunc) error) error {
	if l == nil {
		return fmt.Errorf("index: FinishBuild: nil build log")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	defer m.retireLocked(l)

	// The log is stable here: recording happens only under the shared lock this
	// exclusive hold excludes, so no goroutine can be inside record while this
	// runs and l.mu is uncontended.
	l.mu.Lock()
	overflowed, recorded, resolved := l.overflowed, l.changes, l.resolve != nil
	l.changes = nil
	l.mu.Unlock()

	if overflowed {
		return fmt.Errorf("%w: more than %d changes were fanned out during the build",
			ErrIndexBuildOverflow, MaxBuildLogChanges)
	}

	return fn(func(name string, sub Subscriber) error {
		if sub == nil {
			return fmt.Errorf("index: FinishBuild: nil subscriber for %q", name)
		}
		if _, ok := m.indexes[name]; ok {
			return fmt.Errorf("%w: %q", ErrIndexExists, name)
		}
		// Catch up BEFORE the index is reachable: the replay must not be
		// observable as a sequence of partial states to a reader, and inside
		// this lock it cannot be.
		//
		// The type assertion is hoisted out of the loop: it is one answer for
		// the whole replay, and a subscriber cannot change its own type
		// half-way through one.
		ra, isResolved := sub.(ResolvedApplier)
		useResolved := isResolved && resolved
		for k := range recorded {
			if useResolved {
				ra.ApplyResolved(recorded[k].Change, recorded[k].current, recorded[k].eligible)
				continue
			}
			sub.Apply(recorded[k].Change)
		}
		m.indexes[name] = sub
		return nil
	})
}

// retireLocked removes l from the set of builds in flight. The caller holds mu
// exclusively. Retiring a log that is not present is a no-op, which is what
// makes AbandonBuild safe to defer alongside FinishBuild.
func (m *Manager) retireLocked(l *BuildLog) {
	for i, b := range m.builds {
		if b != l {
			continue
		}
		m.builds = append(m.builds[:i], m.builds[i+1:]...)
		if len(m.builds) == 0 {
			m.builds = nil
		}
		return
	}
}
