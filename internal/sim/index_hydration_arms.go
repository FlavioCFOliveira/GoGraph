package sim

import (
	"context"
	"fmt"
	"strings"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
)

// Deserialize-not-rebuild recovery arms (rmp #2490).
//
// A recovered secondary index is populated one of two ways: HYDRATED from the
// indexes/<name>.bin payload the snapshot carried, or REBUILT by scanning the
// recovered graph. Which one happens is decided per index, by name, against
// three preconditions (see cypher/index_hydration.go): the snapshot was
// self-sufficient, the payload is readable and CRC-valid, and nothing the
// replayed WAL suffix committed touched that index's (label, property).
//
// Before this task the DST could not reach the hydrate side at all — the
// simulator built its engine through a constructor that carries no payloads, so
// every reopen rebuilt — and it could not have told the two apart if it had,
// because a hydrated index and a rebuilt one must answer IDENTICALLY. That is the
// whole difficulty: the correct behaviour is indistinguishable in the answers, so
// the only sound oracle is the engine-scoped population counter
// ([SimStore.RecoveredIndexPopulation]) asserted in BOTH directions, with the
// answers verified independently on top.
//
// # Two CONSTRUCTED arms, not two hoped-for coincidences
//
// The hydrate side needs a reopen whose WAL suffix touches nothing; the refused
// side needs one whose suffix touches an indexed property. Under this scenario's
// churn the second is the overwhelmingly likely outcome of any crash — the loop
// writes :Person {name, age, city} every tick — and the first happens only if a
// crash lands on the very tick a checkpoint published, before the tick's write.
// Waiting for that coincidence would make the harness, not the engine, decide
// whether the arm ran; so both arms are constructed deterministically and the
// terminal gate requires both to have been driven.
//
// The hydrate arm reuses [crossSnapshotBoundaryOn], the forced crossing rmp
// #2468 already built, and adjudicates it with [checkSnapshotSourcedRecovery].
// That is not decoration: the crossing MEASURES the WAL going to zero and the
// following recovery replaying zero ops, which is the substrate-level reason
// hydration is permitted. So the arm carries two independent witnesses — the
// durable byte image, and the engine's own count of what it did with it — rather
// than one flag reporting on itself.
//
// # It deliberately does NOT count towards the run statistics
//
// The arms perform a genuine checkpoint and two genuine crashes, but they do not
// touch [Simulator.checkpointCount] or [Simulator.crashCount], which is why they
// call the store-level [crossSnapshotBoundaryOn] rather than the Simulator
// method that wraps it. Those two counters are what
// [Simulator.checkCheckpointsFired] and the short-layer wiring test assert on,
// and both exist to prove that the IN-LOOP cadence fires. An arm that ran
// unconditionally and incremented them would satisfy those gates by itself and
// silence exactly the defect they were written to catch (rmp #2457/#2464).

// hydrationArmsOp is the op label every hydration-arm violation carries.
const hydrationArmsOp = "index hydration arms"

// The write the refused arm commits AFTER the snapshot is published: one :Person
// carrying all three indexed properties, with values chosen to fall inside a
// selective seek window on each of them so the post-crash read-back can be served
// BY the indexes under test.
//
// It is the concrete thing a wrongly-hydrated index would be missing: the payload
// was serialised before this node existed, so an index restored from it cannot
// contain the node, while an index rebuilt from the recovered graph must.
//
// Constants rather than a package-level struct var, so the harness carries no
// mutable package state a concurrently-running swarm worker could observe
// changing.
const (
	hydrationProbeName = "hyd-stale-probe"
	hydrationProbeAge  = 499
	hydrationProbeCity = "c99"
)

// indexHydrationArms drives and records the two constructed recovery arms.
//
// # Concurrency contract
//
// indexHydrationArms is NOT safe for concurrent use; the simulator drives it
// from the single simulation goroutine.
type indexHydrationArms struct {
	// applicable records that the store really was full-stack, so the arms could
	// run at all. It is what keeps the terminal gate from firing on a scenario
	// configuration that has no snapshot directory to publish into — a coverage
	// clause may only fail a run whose precondition was constructed.
	applicable bool

	// hydrateRan records that the hydrate arm completed; hydrated / rebuilt /
	// registered are what that reopen measured, and boundary the forced
	// crossing's own measurements.
	hydrateRan                            bool
	hydrateHydrated, hydrateRebuilt       int
	hydrateRegistered, hydrateBackfillled int
	boundary                              snapshotBoundary

	// staleRan records that the refused arm completed; the counters mirror the
	// hydrate arm's, staleWALOps is what its recovery replayed out of the WAL
	// suffix (the independent witness that the suffix was NOT empty), and
	// staleProbeSeen records that the post-checkpoint node was found through
	// every index under test.
	staleRan                         bool
	staleHydrated, staleRebuilt      int
	staleRegistered, staleBackfilled int
	staleWALOps                      int
	staleProbeSeen                   bool

	// loopReopens counts the ordinary in-loop crash recoveries whose branch was
	// recorded, and loopHydrated / loopRebuilt how many indexes each branch
	// populated across them. They are OBSERVATIONS, never assertions: under this
	// scenario's churn the WAL suffix normally carries a :Person write, so the
	// rebuild branch is the correct answer for almost every in-loop crash, and
	// requiring either branch here would make the harness's crash schedule decide
	// whether the run passes.
	loopReopens               int
	loopHydrated, loopRebuilt int
}

// record notes which branch one ordinary in-loop reopen took. It asserts
// nothing; see the field docs for why.
func (a *indexHydrationArms) record(pop cypher.RecoveredIndexPopulation) {
	a.loopReopens++
	a.loopHydrated += pop.Hydrated
	a.loopRebuilt += pop.Rebuilt
}

// runHydrateArm forces the run across the snapshot boundary and requires the
// reopen that follows to have HYDRATED every index it registered.
//
// verify is the caller's own post-recovery battery (index consistency, schema
// introspection, seek results, intersect probes …) run against the reopened
// engine. Passing it in rather than re-implementing a subset here is the point:
// a hydrated index must answer exactly as a rebuilt one does, so the assertion
// that matters is that the scenario's EXISTING checks still hold — on an engine
// whose indexes came out of the snapshot instead of out of a graph scan.
//
// It replaces sm.store and sm.engine with the reopened pair. An error return is
// a harness or scenario fault (no full-stack store, a failed checkpoint); a
// violation return is a real finding.
func (a *indexHydrationArms) runHydrateArm(
	sm *Simulator, tick int64, verify func(int64) []Violation,
) ([]Violation, error) {
	if sm.store == nil || sm.store.Config().dir == "" {
		// WAL-only configuration: there is no snapshot directory, so no payload
		// can exist and neither arm is constructible. Leave applicable false so
		// the terminal gate stays silent, and let the scenario's own checkpoint
		// gate report the misconfiguration.
		return nil, nil
	}
	a.applicable = true

	store, b, err := crossSnapshotBoundaryOn(sm.disk, sm.store, "index-diversity hydrate arm")
	if err != nil {
		return nil, err
	}
	sm.store = store
	sm.engine = NewEngineAdapter(store.Engine())
	a.boundary = b

	// The substrate-level witness FIRST: the checkpoint emptied the WAL and the
	// recovery replayed nothing out of it. Without that the hydration counters
	// below would be reporting on a precondition nobody established.
	if v := checkSnapshotSourcedRecovery(tick, b); len(v) > 0 {
		return v, nil
	}

	pop := store.RecoveredIndexPopulation()
	registered := len(store.Engine().ListIndexes())
	a.hydrateRan = true
	a.hydrateHydrated, a.hydrateRebuilt = pop.Hydrated, pop.Rebuilt
	a.hydrateRegistered, a.hydrateBackfillled = registered, pop.BackfillNodes

	if v := checkHydrationPopulation(tick, "hydrate", registered, pop, hydrationExpectHydrated, b.summary()); len(v) > 0 {
		return v, nil
	}
	// A hydrated index must be indistinguishable from a rebuilt one in every
	// answer it gives, which is exactly what the caller's battery asserts.
	return verify(tick), nil
}

// runStaleArm commits one write to the indexed properties on top of the snapshot
// the hydrate arm published, crashes, and requires the reopen to have REBUILT
// every index — the staleness precondition refusing the payload — and then to
// serve the post-checkpoint write through those very indexes.
//
// The second half is what makes the first half matter. "Rebuilt" is only the
// right answer because the payload describes a state the graph has left; the
// consequence of getting it wrong is an index that silently omits every write
// committed after the checkpoint. So the arm looks for exactly that: each
// indexed property is queried through a predicate the planner serves FROM the
// index, and the answer must both equal an independent full-scan reference and
// contain the node the payload cannot know about.
func (a *indexHydrationArms) runStaleArm(
	ctx context.Context, sm *Simulator, tick int64, verify func(int64) []Violation,
) ([]Violation, error) {
	if !a.applicable || !a.hydrateRan {
		return nil, nil
	}
	create := fmt.Sprintf("CREATE (:Person {name:'%s', age:%d, city:'%s'})",
		hydrationProbeName, hydrationProbeAge, hydrationProbeCity)
	if !sm.execute(ctx, Op{Kind: OpCreate, Cypher: create}) {
		return nil, fmt.Errorf("sim: index-diversity stale arm: the post-checkpoint write %q was not committed", create)
	}

	cfg := sm.store.Config()
	sm.store.Crash()
	store, err := OpenSimStore(sm.disk, cfg)
	if err != nil {
		return nil, fmt.Errorf("sim: index-diversity stale arm: recovery reopen: %w", err)
	}
	sm.store = store
	sm.engine = NewEngineAdapter(store.Engine())

	pop := store.RecoveredIndexPopulation()
	registered := len(store.Engine().ListIndexes())
	a.staleRan = true
	a.staleHydrated, a.staleRebuilt = pop.Hydrated, pop.Rebuilt
	a.staleRegistered, a.staleBackfilled = registered, pop.BackfillNodes
	a.staleWALOps = store.WALOps()

	// The independent witness that the suffix really was non-empty. Without a
	// replayed op the payload would have been hydratable and "rebuilt" would be
	// the WRONG expectation, so this is a precondition and not a statistic.
	if a.staleWALOps == 0 {
		return []Violation{{
			Kind: ViolationOracleDeviation, Tick: tick, Op: hydrationArmsOp,
			Message: "stale arm: the reopen replayed 0 WAL ops although a :Person write was committed after" +
				" the checkpoint, so the WAL suffix was empty and this arm cannot test the staleness gate",
		}}, nil
	}
	if v := checkHydrationPopulation(tick, "stale", registered, pop,
		hydrationExpectRebuilt, fmt.Sprintf("replayed %d WAL ops", a.staleWALOps)); len(v) > 0 {
		return v, nil
	}
	if v := a.checkStaleProbeVisible(tick, sm.engine); len(v) > 0 {
		return v, nil
	}
	return verify(tick), nil
}

// hydrationExpectHydrated / hydrationExpectRebuilt name the side of the decision
// an arm requires, so [checkHydrationPopulation] can assert the population in
// both directions from one body instead of two near-identical ones.
const (
	hydrationExpectHydrated = "hydrated"
	hydrationExpectRebuilt  = "rebuilt"
)

// checkHydrationPopulation adjudicates one reopen's population counters against
// the side the arm requires, in BOTH directions: the required side must account
// for every index registered, and the other side must be zero.
//
// A one-sided assertion would be worthless here. "At least one index hydrated"
// is satisfied by a reopen that hydrated one and rebuilt five, which is the
// symptom of a precondition evaluated per image instead of per index; and "no
// index hydrated" is satisfied by a reopen that registered nothing at all. So
// the count is anchored to an INDEPENDENT measure of how many indexes exist —
// the manager's own registered-name list — rather than to a constant the
// scenario would have to keep in step with the engine's internal companions.
func checkHydrationPopulation(
	tick int64, arm string, registered int, pop cypher.RecoveredIndexPopulation, want, witness string,
) []Violation {
	fail := func(format string, args ...any) []Violation {
		return []Violation{{
			Kind: ViolationOracleDeviation, Tick: tick, Op: hydrationArmsOp,
			Message: fmt.Sprintf("%s arm (%s): ", arm, witness) + fmt.Sprintf(format, args...),
		}}
	}
	if registered == 0 {
		return fail("the reopened engine registered NO index, so a population assertion of either" +
			" side is vacuous: there was nothing to populate")
	}
	if pop.Hydrated+pop.Rebuilt != registered {
		return fail("the reopen populated %d indexes (hydrated %d + rebuilt %d) but registered %d:"+
			" an index that is registered without being populated is seekable while empty",
			pop.Hydrated+pop.Rebuilt, pop.Hydrated, pop.Rebuilt, registered)
	}
	switch want {
	case hydrationExpectHydrated:
		if pop.Hydrated != registered || pop.Rebuilt != 0 {
			return fail("want every one of the %d registered indexes HYDRATED from its snapshot payload,"+
				" got hydrated=%d rebuilt=%d (payload unreadable=%d, corrupted=%d, backfilled node refs=%d)",
				registered, pop.Hydrated, pop.Rebuilt, pop.PayloadUnreadable, pop.PayloadCorrupted, pop.BackfillNodes)
		}
		if pop.BackfillNodes != 0 {
			return fail("every index hydrated, yet %d node references were backfilled: a hydration that"+
				" also scans the graph has not avoided the work it exists to avoid", pop.BackfillNodes)
		}
	case hydrationExpectRebuilt:
		if pop.Rebuilt != registered || pop.Hydrated != 0 {
			return fail("want every one of the %d registered indexes REBUILT from the recovered graph"+
				" (the WAL suffix touched an indexed property, so no payload may be used),"+
				" got hydrated=%d rebuilt=%d", registered, pop.Hydrated, pop.Rebuilt)
		}
		if pop.BackfillNodes == 0 {
			return fail("every index reported rebuilt, yet 0 node references were backfilled:" +
				" the rebuild scanned nothing, so the indexes cannot hold the recovered graph")
		}
	default:
		return fail("unknown expectation %q", want)
	}
	return nil
}

// staleProbeArm is one indexed property's post-crash read-back: the predicate
// the planner must serve from the index, the leaf operator that proves it did,
// and the client-side filter that reproduces the answer from a plain scan.
type staleProbeArm struct {
	property string
	query    string
	wantLeaf string
	keep     func(age int64, ageOK bool, city string, cityOK bool, name string, nameOK bool) bool
}

// checkStaleProbeVisible requires the write committed after the checkpoint to be
// reachable through EVERY index under test, and the index-served answer to equal
// an independent full-scan reference.
//
// Both halves are load-bearing. The reference comparison is what catches a
// stale index — one restored from a payload that predates the write would return
// a strictly smaller multiset. The leaf-operator assertion is what stops the
// comparison passing for the wrong reason: if the planner declined to seek and
// scanned instead, the answer would be right while the index was never
// consulted, and a stale index would go unnoticed.
func (a *indexHydrationArms) checkStaleProbeVisible(tick int64, engine PlanEngine) []Violation {
	c := &InvariantChecker{}
	arms := []staleProbeArm{{
		property: "name (hash)",
		query:    fmt.Sprintf("MATCH (n:Person) WHERE n.name = '%s' RETURN id(n)", hydrationProbeName),
		wantLeaf: "NodeByIndexSeek",
		keep: func(_ int64, _ bool, _ string, _ bool, name string, nameOK bool) bool {
			return nameOK && name == hydrationProbeName
		},
	}, {
		property: "age (numeric btree)",
		query:    fmt.Sprintf("MATCH (n:Person) WHERE n.age >= %d RETURN id(n)", hydrationProbeAge),
		wantLeaf: intersectRangeScanOp,
		keep: func(age int64, ageOK bool, _ string, _ bool, _ string, _ bool) bool {
			return ageOK && age >= hydrationProbeAge
		},
	}, {
		property: "city (string btree)",
		query:    fmt.Sprintf("MATCH (n:Person) WHERE n.city >= '%s' RETURN id(n)", hydrationProbeCity),
		wantLeaf: intersectRangeScanOp,
		keep: func(_ int64, _ bool, city string, cityOK bool, _ string, _ bool) bool {
			return cityOK && city >= hydrationProbeCity
		},
	}}

	probeID, refs, err := hydrationStaleReference(engine, arms)
	if err != nil {
		c.add(ViolationOracleDeviation, tick, hydrationArmsOp,
			fmt.Sprintf("stale arm: reference scan %q failed: %v", seekReferenceQuery, err))
		return c.violations
	}
	if probeID < 0 {
		c.add(ViolationACIDDurability, tick, hydrationArmsOp,
			fmt.Sprintf("stale arm: the :Person {name:%q} committed after the checkpoint is absent from the"+
				" recovered graph itself, so the WAL suffix carrying it was lost", hydrationProbeName))
		return c.violations
	}

	for i := range arms {
		arm := &arms[i]
		got, qerr := runProbeIDs(engine, arm.query, nil)
		if qerr != nil {
			c.add(ViolationOracleDeviation, tick, hydrationArmsOp,
				fmt.Sprintf("stale arm: probe on %s (%q) failed: %v", arm.property, arm.query, qerr))
			continue
		}
		plan, perr := engine.Explain(arm.query, nil)
		if perr != nil {
			c.add(ViolationOracleDeviation, tick, hydrationArmsOp,
				fmt.Sprintf("stale arm: Explain(%q) failed: %v", arm.query, perr))
			continue
		}
		if !strings.Contains(plan, arm.wantLeaf) {
			c.add(ViolationOracleDeviation, tick, hydrationArmsOp,
				fmt.Sprintf("stale arm: the probe on %s was NOT served by the index (no %s in the plan), so a"+
					" stale index would answer it correctly by scanning and this arm would prove nothing\nquery: %s\nplan:\n%s",
					arm.property, arm.wantLeaf, arm.query, plan))
			continue
		}
		if !containsID(refs[i], probeID) {
			c.add(ViolationOracleDeviation, tick, hydrationArmsOp,
				fmt.Sprintf("stale arm: the independent reference for %s does not contain the post-checkpoint"+
					" node, so requiring the index to return it asserts nothing", arm.property))
			continue
		}
		if !equalIDMultisets(got, refs[i]) {
			c.add(ViolationACIDConsistency, tick, hydrationArmsOp,
				fmt.Sprintf("stale arm: the index-served read on %s disagrees with the independent full-scan"+
					" reference — an index restored from the pre-checkpoint payload omits every write committed"+
					" after it\nquery: %s\nscan reference: %s\nindex answer:   %s",
					arm.property, arm.query, summariseIDs(refs[i]), summariseIDs(got)))
		}
	}
	if len(c.violations) == 0 {
		a.staleProbeSeen = true
	}
	return c.violations
}

// hydrationStaleReference runs ONE plain label scan and derives, client-side,
// both the id of the post-checkpoint probe node and the reference multiset for
// every arm. It returns -1 for the probe id when the scan does not carry the
// node at all, which is a durability fault rather than an index fault.
func hydrationStaleReference(engine Engine, arms []staleProbeArm) (int64, [][]int64, error) {
	res, err := engine.Run(context.Background(), seekReferenceQuery, nil)
	if err != nil {
		return -1, nil, err
	}
	defer func() { _ = res.Close() }()

	probeID := int64(-1)
	refs := make([][]int64, len(arms))
	for res.Next() {
		id, ok := res.IntAt(0)
		if !ok {
			continue
		}
		age, ageOK := res.IntAt(1)
		city, cityOK := res.StringAt(2)
		name, nameOK := res.StringAt(3)
		if nameOK && name == hydrationProbeName {
			probeID = id
		}
		for i := range arms {
			if arms[i].keep(age, ageOK, city, cityOK, name, nameOK) {
				refs[i] = append(refs[i], id)
			}
		}
	}
	if err := res.Err(); err != nil {
		return -1, nil, err
	}
	for i := range refs {
		sortIDs(refs[i])
	}
	return probeID, refs, nil
}

// containsID reports whether the sorted id slice holds want.
func containsID(ids []int64, want int64) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// finish asserts that BOTH sides of the hydration decision were actually
// reached, and must be called once at the end of the run.
//
// It is silent for a run whose store was never full-stack: a coverage clause may
// only fail a run whose precondition was constructed, and a WAL-only
// configuration has no snapshot to hydrate from. Every other silence is a
// finding — a run that drove only the hydrate arm never tested the staleness
// gate, and one that drove only the stale arm never proved the deserialize path
// runs at all.
func (a *indexHydrationArms) finish(tick int64) []Violation {
	if !a.applicable {
		return nil
	}
	var out []Violation
	add := func(msg string) {
		out = append(out, Violation{Kind: ViolationOracleDeviation, Tick: tick, Op: hydrationArmsOp, Message: msg})
	}
	if !a.hydrateRan {
		add("vacuous run: the hydrate arm never ran, so no reopen was proven to have loaded an index" +
			" from its snapshot payload instead of rebuilding it")
	}
	if !a.staleRan {
		add("vacuous run: the stale arm never ran, so the staleness precondition was never observed" +
			" to REFUSE a payload and nothing distinguishes hydration-when-safe from hydration-always")
	}
	if a.hydrateRan && a.staleRan && a.hydrateHydrated == 0 && a.staleRebuilt == 0 {
		add("vacuous run: both arms ran yet neither hydrated nor rebuilt anything, so the population" +
			" counters were satisfied by an engine that populated no index at all")
	}
	if a.staleRan && !a.staleProbeSeen {
		add("vacuous run: the stale arm never confirmed the post-checkpoint write was reachable through" +
			" the rebuilt indexes, so nothing measured the consequence of hydrating a stale payload")
	}
	return out
}

// summary renders both arms' measured numbers for a test log or a report.
func (a *indexHydrationArms) summary() string {
	if !a.applicable {
		return "index hydration arms: not applicable (WAL-only store, no snapshot directory)"
	}
	return fmt.Sprintf("index hydration arms: hydrate[ran=%t hydrated=%d rebuilt=%d registered=%d backfilled=%d; %s]"+
		" stale[ran=%t hydrated=%d rebuilt=%d registered=%d backfilled=%d walOps=%d probeVisible=%t]",
		a.hydrateRan, a.hydrateHydrated, a.hydrateRebuilt, a.hydrateRegistered, a.hydrateBackfillled, a.boundary.summary(),
		a.staleRan, a.staleHydrated, a.staleRebuilt, a.staleRegistered, a.staleBackfilled, a.staleWALOps, a.staleProbeSeen) +
		fmt.Sprintf(" loop[reopens=%d hydrated=%d rebuilt=%d]", a.loopReopens, a.loopHydrated, a.loopRebuilt)
}
