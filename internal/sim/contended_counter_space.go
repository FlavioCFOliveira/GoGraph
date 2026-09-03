package sim

// contended_counter_space.go — the shared counter key space of ONE concurrent
// run (rmp #2729).
//
// The contended transactional role exists to make transactions COLLIDE: the
// connections of one [RunConcurrent] call read-modify-write a small set of
// shared counters, and `ContendedFinal[k] == ContendedAcked[k]` is the
// zero-lost-updates oracle that proves the engine refused every lost update.
//
// That sharing is the point WITHIN a run, and a defect BETWEEN runs. The
// counters were named by a fixed string, `mv-wire-counter-<k>`, carrying nothing
// that identified the run, so two [RunConcurrent] calls driving one shared
// [SimServer] at the same time collided on the same nodes. Measured on that
// fixture — six concurrent calls, four connections each, eight operations, one
// shared server — every one of the 192 contended transactions FAILED and none
// committed, because concurrent check-then-CREATE seeding had left three nodes
// answering `mv-wire-counter-0` and two answering `mv-wire-counter-1`, which
// breaks the single-row contract of the read half of every read-modify-write.
// The run reported no violation: with nothing acknowledged and nothing readable,
// the oracle compared 0 against 0 and passed. The role had been retired in
// silence.
//
// # The unit of privacy is the RUN, not the caller and not the connection
//
// Making the counters private per CONNECTION would delete the contention the
// role exists to create, leaving a role that can no longer lose an update and
// therefore an oracle that can no longer catch one. Declining to assert under
// sharing — the precedent [ConcurrentResult.Consistent] follows for the
// node-count oracle, which bench/contention's dst-concurrent-bolt deliberately
// does not check — would keep the run honest but blind, and a gate that cannot
// fail is a defect in its own right.
//
// A [counterSpace] takes the third option: the counters stay shared by every
// connection of one run, and no two runs share them. The contention is intact,
// the oracle is exact, and a lost update is still detectable. Measured on the
// same six-call battery once the space is private: 192 transactions issued, 67
// committed, 111 refused with the typed serialization conflict, none failed, and
// all twelve counter comparisons exact. The role went from exercising nothing to
// colliding on 57.8% of its transactions.
//
// The two failure modes were measured separately, so neither rests on the other.
// Holding the seeding race out of the picture — the counters pre-seeded once,
// then four runs sharing one namespace, census confirming exactly one node per
// counter — gave 8 of 8 comparisons wrong: every run read final=[13 13] against
// its own acknowledged [3 4], [3 6], [4 0] and [3 2]. The counters held the
// exact sum of all four runs' increments, which is also the honest reading of
// that experiment: the ENGINE lost nothing. Only the harness's attribution was
// wrong, and it was wrong in the direction that invents a violation.
//
// # Which runs are "the same run"
//
// A logical run is not always one call. [runProductionProfileEvidence] drives
// several sequential [RunConcurrent] calls, across crash and recovery, over ONE
// durable store, and adjudicates each counter's accumulated total against its
// value after recovery: for it the counters must keep the same names for the
// whole scenario. So the namespace is the CALLER's to name
// ([ConcurrentConfig.ContendedNamespace]), and is derived from the run seed only
// when the caller says nothing.
//
// # Why the seed, and why that is not the whole guarantee
//
// A seed-derived default is deterministic — it is a pure function of the
// caller's own input, not of process-global allocation order, which a
// deterministic simulator needs — and it is distinct for every caller that
// drives one shared server concurrently, because such a caller must already give
// each call a distinct seed (bench/contention's dstConcurrentOp: "Disjoint seed
// per operation"). Two calls replaying one seed would collide on their MARKER
// names too, not only on their counters.
//
// A convention is not a guarantee, so [contendedSpaceLeases] enforces it: a
// namespace is leased against the server for the duration of the run, and a
// second overlapping run that asks for the same one is refused with a typed
// error instead of quietly corrupting both oracles. Fail-stop, never
// fail-silent.
//
// # Why the space is not recycled, unlike the parameter probe's slot
//
// rmp #2728 bounds its fixture slots by RECYCLING them through a free list,
// because the probe DELETES its node before releasing the slot: the next holder
// inherits an empty label. A counter cannot be recycled on those terms. Its node
// must survive the run — it is an acknowledged create, counted by the
// eventual-consistency oracle, and read again after recovery by the scenario
// that persists — so handing the same name to a later run would give that run a
// counter already holding someone else's total and adjudicate its acknowledged
// increments against it. That is the very corruption this file removes.
//
// The cost is honest and bounded rather than free: a contended run adds
// ContendedCounters nodes that persist, where the fixed name added two for the
// whole process. It is dominated by what the same run already creates — every
// committed transaction leaves one to three marker nodes that also persist, so
// at the sizes this harness runs the counters are a single-digit percentage of
// the population — and it registers no new LABEL, since a counter is a
// `:Person` like every marker. There is no working baseline being traded away:
// on the shared name the contended role committed nothing at all.

import (
	"fmt"
	"sync"
)

// contendedCounterPrefix prefixes every shared contended-counter node name. The
// full name is this prefix, the run's namespace, and the counter index, so a
// counter node always names the run that owns it.
const contendedCounterPrefix = "mv-wire-counter"

// contendedCounterName is the node name of counter k in namespace ns. It is the
// single place the name is formed, so a caller that must read a counter outside
// [RunConcurrent] — [runProductionProfileEvidence] reads them after recovery —
// cannot drift from the name the run actually wrote.
func contendedCounterName(ns string, k int) string {
	return fmt.Sprintf("%s-%s-%d", contendedCounterPrefix, ns, k)
}

// counterSpace is one run's shared contended-counter key space: how many
// counters the run collides on, and the namespace that separates them from every
// other run's.
//
// It is an immutable value passed by copy down to the connection goroutines, so
// it carries no synchronisation of its own.
type counterSpace struct {
	ns string
	n  int
}

// name returns the node name of counter k in this space.
func (s counterSpace) name(k int) string { return contendedCounterName(s.ns, k) }

// defaultContendedNamespace derives a run's namespace from its seed, for a
// caller that named none. See the file comment for why the seed is the right
// default and why [contendedSpaceLeases] still has to enforce it.
func defaultContendedNamespace(seed uint64) string { return fmt.Sprintf("s%016x", seed) }

// contendedSpaceLeases is the set of (server, namespace) pairs a run currently
// holds. It exists so that two overlapping runs sharing a counter namespace fail
// loudly rather than silently invalidating each other's zero-lost-updates
// oracle.
//
// It is keyed by the server as well as the namespace: two runs against DIFFERENT
// servers share no nodes, so they may hold the same namespace at the same time
// without any interference.
var contendedSpaceLeases contendedSpaceLeaseSet

// contendedSpaceLeaseSet is a registry of held counter namespaces. It is safe
// for concurrent use by any number of goroutines. Its size is bounded by the
// number of contended runs in flight, and every acquire is paired with a release
// on every return path, so it holds nothing once the runs have drained.
type contendedSpaceLeaseSet struct {
	mu   sync.Mutex
	held map[contendedSpaceLease]bool
}

// contendedSpaceLease identifies one held key space.
type contendedSpaceLease struct {
	srv *SimServer
	ns  string
}

// acquire reserves ns against srv, reporting false when another run already
// holds it.
func (s *contendedSpaceLeaseSet) acquire(srv *SimServer, ns string) bool {
	key := contendedSpaceLease{srv: srv, ns: ns}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held[key] {
		return false
	}
	if s.held == nil {
		s.held = make(map[contendedSpaceLease]bool)
	}
	s.held[key] = true
	return true
}

// release drops the reservation. Calling it without a matching acquire is a
// no-op, so a caller may release unconditionally on its error paths.
func (s *contendedSpaceLeaseSet) release(srv *SimServer, ns string) {
	key := contendedSpaceLease{srv: srv, ns: ns}
	s.mu.Lock()
	delete(s.held, key)
	s.mu.Unlock()
}
