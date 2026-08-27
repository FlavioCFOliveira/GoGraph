package sim

// count_store.go — rmp #2494: the derived relationship count-store
// ([github.com/FlavioCFOliveira/GoGraph/graph/index/count.Store]) adjudicated
// cell by cell against the shadow model, and the reopen-time
// `RecomputeReset` + replay path ([cypher.Engine.recomputeCountStore]) asserted
// CORRECT rather than merely executed.
//
// # What was unreached, and why the gap was invisible
//
// The count-store is the planner's exact-cardinality source: `E(relType)`,
// `D(label, relType, dir)` and `T(labelA, relType, labelB)` (design
// docs/count-store-design.md). Before this file:
//
//   - `count.Store.Snapshot` had NO production caller. It existed for
//     "observability and differential testing" and only the count package's own
//     in-package tests ever called it, so nothing outside `cypher` could read a
//     cell at all;
//   - the DST's repertoire held no `CountE` / `CountT` / `TDirty` query shape.
//     Every count the sim ever issued was `count(n)` / `count(r)` over a bound
//     variable, which the planner does not serve from this store;
//   - so the reopen path was never ASSERTED. It RUNS on every recovery already
//     — `OpenSimStore` builds a fresh [cypher.Engine] on each reopen and
//     `NewEngineWithOptions` calls `recomputeCountStore` once at construction —
//     but nothing compared its result to anything.
//
// That is the fail-silent class the DST exists for. A wrong count store raises
// no error and fails no test: it yields a PLAUSIBLE-BUT-WRONG plan. The engine
// still returns correct rows, just from the wrong access path, so the whole
// existing suite stays green.
//
// The gap was invisible for the ordinary reason. `cypher` has good in-package
// coverage (`count_maintenance_test.go`'s `diffCounts` against an O(V+E)
// recount, `count_rapid_test.go`'s rapid property test and its same-seed
// determinism gate), but all of it runs on hand-built graphs inside the package
// with no crash injection, no WAL, no snapshot, and no reopen: the recount
// oracle there compares the store with the GRAPH, which is exactly what
// `recomputeCountStore` itself does, so it cannot witness the recompute.
//
// # The one accessor this task added, and why it is a snapshot
//
// The requirement needs an exported route from the engine to its count store and
// there was none: `Engine.CountStoreCells() int` is a size indicator, and
// `lpgLabelResolver.Counts()` sits on an unexported type. `cypher.Engine` now
// carries [cypher.Engine.CountSnapshot], returning a `count.Snapshot` — a COPY
// of the cells and the dirty markings. It deliberately does not return the
// `*count.Store`: a store handle would hand every caller `Apply`, `MarkDirty`
// and `RecomputeReset` over the engine's own derived state.
//
// # The model is the SHADOW MODEL, and it is keyed by NAME
//
// [GraphOracle.countStoreModel] recomputes E/D/T from the oracle's own
// `nodes`/`edges` maps — the op stream alone — and never asks the engine or the
// graph what the answer should be. It is the internal/sim rule against
// validating the engine with the engine; a model that recounted the GRAPH would
// be a second copy of `recomputeCountStore` and would agree with it by
// construction on exactly the reopen this scenario is here to check.
//
// The store's keys are the graph's INTERNED ids
// ([lpg.LabelRegistry]), assigned in first-intern order, so whether they survive
// a reopen is an empirical question. MEASURED, on this harness: they did — a
// pre-crash registry of `0=Person 1=KNOWS 2=Vip 3=Gold` came back identical
// after a WAL-only replay AND after a snapshot+WAL reopen, and `Vip` kept id 2
// even in a run where no live node carried it any more. Nothing DOCUMENTS that,
// so the model keys by NAME and resolves through the recovered graph's own
// registry at comparison time. The measurement is the reason the choice is
// cheap, not the reason it is safe.
//
// # A dirty cell is not a wrong cell
//
// `countRelabel` (cypher/count_maintenance.go) cannot enumerate a node's
// IN-edges in O(delta) — there is no reverse index — so every `SET n:X` /
// `REMOVE n:X` on a graph that has any edge marks `D(X,*,IN)` and `T(*,*,X)`
// non-exact instead of writing a wrong exact (design §3.3.1). MEASURED: one
// `SET p2:Vip` on a five-edge path graph left `dirty{DIn:[Vip] TB:[Vip]}`, with
// `DIn[Vip,KNOWS]` and `T[Person,KNOWS,Vip]` ABSENT where ground truth says 1.
// That is the contract, so the LIVE phase skips a dirty-covered cell exactly as
// `diffCounts` does. Asserting exactness there would fail correct code.
//
// How easily that state is reached was itself a finding. A `DETACH DELETE`
// reaches `countRelabel` too: MEASURED, one `DETACH DELETE` of a Person at tick 5
// of a run left `dirty{DIn:[Person Vip] TB:[Person Vip]}`, and the dirty sets only
// ever GROW until the next `RecomputeReset`. So on any graph that has ever deleted
// a labelled node — which is every realistic graph — the planner's IN-side degree
// and triple statistics for that label are vetoed for the rest of the session.
// That is what makes the reopen's heal consequential rather than cosmetic, and it
// is why this scenario carries the never-churned [csLabelHub] label: without it
// the live `DIn` and `T` clauses would have compared two empty maps while their
// evidence still reported the families as populated.
//
// # A NEGATIVE cell is legitimate, and it is constructed on purpose
//
// `Store.add` retains a cell driven negative rather than clamping it, because
// that is what makes the aggregate order-insensitive under concurrent writers
// (rmp #2303). MEASURED, that is reachable from ordinary Cypher with no
// concurrency at all: with `a:Person`, `b:Person`, an edge `a->b`, then
// `SET a:X`, `SET b:X`, `REMOVE a:X`, the store held `T[X,KNOWS,X] = -1`. The
// `SET b:X` increment was never applied (b has no out-edge, so the OUT recount
// returns early and the IN side is covered by the `TB(X)` marking), while the
// `REMOVE a:X` decrement DID land, because by then b carried `X` and the OUT
// recount reads the endpoint's CURRENT labels.
//
// [countStoreArmNegativeCell] builds exactly that sequence, under a label
// ([csLabelNeg]) the fixture owns exclusively so unrelated churn cannot cancel
// the cell, and it is re-armed after every recovery because the reopen
// legitimately removes it. Two things are then asserted: the negative cell must
// be dirty-COVERED (an uncovered negative would break the store's own
// order-insensitivity claim), and it must be GONE after the reopen.
//
// It also exposed a documentation defect. `Store.Snapshot`'s godoc said it
// returns "every live cell (value > 0)"; the code has always used `v != 0`, and
// must, for the reason above. Corrected in this task's change.
//
// # The sharpest claim: the reopen HEALS
//
// `recomputeCountStore` is documented as the self-heal point — "the recomputed
// store equals a ground-truth recount of the recovered graph on every cell — E,
// D and T exact, zero dirty". So the RECOVERED phase skips NOTHING: every cell
// must match the model and all four dirty sets must be EMPTY. MEASURED on the
// constructed fixture: pre-recovery `dirty{DIn:[Neg] TB:[Neg]}` with one
// negative cell and two cells absent where ground truth says non-zero;
// post-recovery `dirty{}` with `DIn[Neg,KNOWS]=1`, `T[Person,KNOWS,Neg]=1` and no
// negative cell at all. The heal is a real difference on real cells, not a no-op
// that would make the clause unfalsifiable.
//
// The non-vacuity gate is built on that: it is not enough for a recovery to have
// happened. [CountStoreEvidence.HealedFromDirty] and
// [CountStoreEvidence.HealedNegative] require a recovery whose IMMEDIATELY
// PRECEDING live observation was itself dirty (respectively, held a negative
// cell), so "the reopen healed it" rests on there having been something to heal.
// MEASURED before the fixture was re-armed per recovery: a 1500-tick soak run
// reported 19 recoveries and healed exactly ONE negative cell, because the first
// reopen removed the prologue's cell and nothing rebuilt it. With the re-arm the
// same run reports 19 dirty heals and 19 negative heals, and every one of its 152
// live observations holds a negative cell to classify.
//
// # The six query shapes, and which of them can discriminate
//
// The two shapes the requirement names are `MATCH ()-[:KNOWS]->()` (the `E`
// shape) and `MATCH (:Person)-[:KNOWS]->(:Person)` (the `T` shape), both with
// anonymous pattern elements and `count(*)`; they are added to the shared
// surface battery ([CheckCypherSurface]). Their references are derived
// independently, by [GraphOracle.knowsPatternCount], and NOT from
// `knowsCount()`.
//
// One honest limitation, stated rather than papered over: in a workload where
// every node carries `Person` the two references are the SAME NUMBER, so the
// labelled clause cannot fail where the unlabelled one passes. What it adds
// there is the PLAN shape, not a second oracle. This scenario therefore probes
// two more shapes whose references genuinely differ — a `Vip`-constrained
// source and a `Vip`-constrained destination — because it churns the `Vip` label
// over a subset of the population, and each is cross-checked against the very
// `T` cell that serves it. That closes the loop the task cares about: the store
// cell, the model, and the query answer must all be the same number.
//
// # The defect this scenario surfaced, and why the Vip shapes are anonymous
//
// The two `Vip` shapes deliberately use ANONYMOUS pattern elements —
// `(:Person)-[:KNOWS]->(:Vip)` — because that spelling once returned the WRONG
// ANSWER, and the reason was the count store itself.
//
// MEASURED, with no store, no recovery and no simulator (a plain
// [cypher.NewEngine] over one `(:Person)-[:KNOWS]->(:Person:Vip)` edge plus
// forty bare Persons):
//
//	MATCH (:Person)-[:KNOWS]->(:Vip)   RETURN count(*)  => 0   (WRONG, want 1)
//	MATCH (:Person)-[:KNOWS]->(b:Vip)  RETURN count(*)  => 0   (WRONG, want 1)
//	MATCH (a:Person)-[:KNOWS]->(:Vip)  RETURN count(*)  => 1
//	MATCH (a:Person)-[:KNOWS]->(b:Vip) RETURN count(*)  => 1
//
// All four rendered the IDENTICAL plan under EXPLAIN
// (`NodeByLabelScan [Vip] -> Expand -> Filter`), so the plan text could not tell
// them apart; PROFILE localised the loss exactly — the `Filter` above the
// re-rooted `Expand` received one row and emitted zero. The discriminator was the
// SOURCE node's anonymity, not the destination's.
//
// Attributed by A/B: `EngineOptions{DisableAnchorSwap: true}` made all four
// return 1. The culprit was the single-edge anchor-swap peephole
// (cypher/anchor_swap_plan.go, rmp #2090/#2150). The reversal moves the
// from-label off the ACCESS PATH and onto a predicate: the written plan enforces
// it in `NodeByLabelScan{fromVar, fromLabel}`, which needs no variable name,
// while the mirror re-checks it as `Selection{LabelPredicate(fromVar, …)}` above
// the re-rooted expand, which can only reach the node THROUGH its name. An
// anonymous pattern HEAD has no name — `matchNodeScan` (cypher/ir/match.go)
// leaves its `NodeVar` empty, while every non-head node is given a synthetic
// `__anon_N` — so the mirror carried `LabelPredicate{Receiver: Variable{Name:
// ""}}`, which resolved to no column, evaluated to NULL, and dropped every row.
//
// The count store is what made it reachable. The swap is admitted only when
// every cost input is `EstExact ∧ ¬dirty`, so while a relabel kept the `Vip`
// IN-families dirty the swap was VETOED and the anonymous spelling answered
// correctly; the moment a reopen cleared the dirty flags — this scenario's own
// central claim — the swap became admissible and the same query started
// answering 0. That is the task's fail-silent thesis reached from the other
// side: not a wrong count store producing a bad plan, but a CORRECT one
// unlocking a broken plan.
//
// Why the swap's own suite missed it: MEASURED, cypher/anchor_swap_diff_test.go
// and cypher/anchor_swap_symmetric_test.go held 24 `MATCH` patterns between them
// and the only anonymous labelled nodes in either file were in fixture-building
// CREATE clauses. Every read probe those differential suites issued named both
// endpoints, so the spelling that breaks was never driven.
//
// FIXED in rmp #2603: `matchAnchorSite` now declines any site whose endpoint
// variable is empty, so an anonymous-head pattern keeps the written order. These
// shapes therefore use the anonymous spelling, which is the genuinely different
// plan shape the requirement names, and
// [TestCountStore_AnchorSwapRetainsAnonymousSourceRows] is the regression gate
// that replaced the defect pin.
//
// # What this scenario does NOT reach
//
// The OUT-side dirty branch is UNREACHABLE here. `countRelabel` dirties
// `D(X,*,OUT)` and `T(X,*,*)` only when the relabelled node's out-degree exceeds
// `EngineOptions.MaxLabelRecountEdges`, whose default is 4096, and that option
// does not reach the durable store at all: `OpenSimStore` builds its engine with
// `EngineOptions{Store, RecoveredConstraints, RecoveredIndexes,
// RecoveredIndexPayloads}` and nothing else, while [Config.EngineOpts] is applied
// only on the plain in-memory path. So `DirtyDOut` and `DirtyTA` are EMPTY for
// every observation this scenario takes — which is why `parity:DOut` is always
// compared live, and why the over-budget branch is left to `cypher`'s own
// in-package `TestCountStore_BudgetTripDirtiesOut`. Threading the budget through
// would mean widening `simStoreConfig` and `OpenSimStore`, a harness API change
// outside this task.
//
// One relationship type is driven (`KNOWS`), so `E` has a single term and the
// multi-type `T` fan-out is not exercised here either; `cypher`'s rapid property
// test drives three types over four labels for that.
//
// # Coverage is constructed, not drawn
//
// The prologue builds the population, the `Vip` churn and the negative-cell
// fixture unconditionally; the crash is FORCED at the end when the seeded
// schedule did not fire ([Simulator.forceCrash]). The seed decides which nodes
// are linked and relabelled; it does not decide whether a cell family, a dirty
// marking, a negative cell or a recovery is reached.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/FlavioCFOliveira/GoGraph/graph/index/count"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// ScenarioCountStore is the catalogue key of the count-store oracle scenario
// (rmp #2494).
const ScenarioCountStore = "count-store"

// countStoreDefaultSeed is the catalogue seed. Like every other scenario's it is
// arbitrary; what matters is that the run is a pure function of it.
const countStoreDefaultSeed uint64 = 0x2494_C0DE_5701

// countStoreSeedMix derives the probe stream's sub-seed from the master seed, so
// the durable workload's op stream stays a pure function of the master seed and
// is unaffected by the probes' own draws.
const countStoreSeedMix uint64 = 0x2494_51A7_5E11_D0BC

// The scenario's budgets. They are the SHORT-layer defaults: small enough that
// the whole run is a small share of the package's ceiling. The soak arm raises
// them (see count_store_soak_test.go).
const (
	// countStoreMaxTicks bounds the deterministic loop.
	countStoreMaxTicks = 220
	// countStorePersons is how many Persons the prologue creates through the
	// modelled template before the loop starts, so the very first parity check
	// already has a non-trivial graph.
	countStorePersons = 10
	// countStoreHubs is how many of those carry the never-churned Hub label. It is
	// small — the point is that SOME cells stay exact under churn, not that many
	// do — but at least two, so a Hub->Hub edge is constructible and the T family
	// reaches a (Hub, KNOWS, Hub) cell as well as the mixed ones.
	countStoreHubs = 4
	// countStoreParityEvery is the tick cadence of the cell-by-cell parity check.
	// It is coarser than 1 because the check walks the whole modelled edge set;
	// the terminal check and every post-recovery check run regardless of cadence.
	countStoreParityEvery = 10
	// countStoreShapesEvery is the tick cadence of the six count(*) query shapes.
	// Each is one scalar read, so it is cheap enough to run more often than the
	// full parity walk.
	countStoreShapesEvery = 6
)

// The label and relationship-type vocabulary this scenario drives. It is
// deliberately tiny: four labels and one relationship type make the
// combinatorial `Cells()` ceiling 1 + 2*4*1 + 4*4*1 = 25, while the graph itself
// grows without limit — which is exactly the claim the boundedness clause states
// (MEASURED in soak: Cells() peaked at 20 with |E| at 227, i.e. 8.8% of |E|).
const (
	// csLabelPerson is the base label every modelled node carries.
	csLabelPerson = "Person"
	// csLabelVip is the CHURNED label: the scenario adds and removes it under
	// load, which is the only way to reach `countRelabel` and therefore the only
	// way a dirty marking or a negative cell can arise.
	csLabelVip = "Vip"
	// csLabelHub is the NEVER-CHURNED label: it is assigned at CREATE time and no
	// op ever adds it, removes it, or deletes a node carrying it.
	//
	// It exists because of a MEASURED property of the maintenance path that would
	// otherwise have made two of the four parity clauses vacuous while LIVE.
	// `countRelabel` dirties the relabelled label's IN families, and a
	// `DETACH DELETE` reaches it: MEASURED, one `DETACH DELETE` of a Person at
	// tick 5 of a run left `DIn:[Person Vip] TB:[Person Vip]`, and the dirty sets
	// only ever GROW until the next `RecomputeReset`. So on any graph that has
	// ever deleted a labelled node — which is every realistic graph —
	// `D(label,*,IN)` and `T(*,*,label)` are non-exact for that label for the rest
	// of the session, and a live parity check that skips every dirty cell would
	// have compared two empty maps for DIn and T.
	//
	// A label no delete and no relabel ever touches keeps its IN families exact,
	// so the live DIn and T clauses have something to compare. The per-family
	// COMPARED counters ([CountStoreEvidence.ComparedLive] and
	// [CountStoreEvidence.ComparedRecovered]) are what turn that from an intention
	// into an assertion.
	csLabelHub = "Hub"
	// csRelKnows is the only relationship type, so `sum(E)` has exactly one term
	// and can be cross-checked against the modelled edge count directly.
	csRelKnows = "KNOWS"
)

// tmplCreatePersonHub creates a Person that also carries the never-churned
// [csLabelHub] label. It is modelled by [GraphOracle.createPersonHub], reached
// from [GraphOracle.ApplyCreate].
const tmplCreatePersonHub = "CREATE (n:Person:Hub {name:$name, age:$age})"

// csHubNamePrefix is the name prefix of every Hub node. The delete branch of
// [CountStoreWriter] excludes it, which is what keeps the Hub label out of the
// dirty sets: a DETACH DELETE of a Hub node would dirty `DIn(Hub)` and
// `TB(Hub)` exactly as it does for Person.
const csHubNamePrefix = "cs-hub"

// csLabelNeg is the label the NEGATIVE-CELL fixture owns exclusively, and
// csNegNamePrefix the prefix of the two nodes that carry it.
//
// It is a dedicated label rather than a reuse of [csLabelVip] for a reason the
// first attempt got wrong: `T(Vip, KNOWS, Vip)` is a SHARED cell, so whether the
// fixture's -1 leaves it negative depends on how many Vip->Vip edges the churn
// happened to have created. MEASURED, the default seed held a negative cell at
// every live observation while a second seed did not, and the heal clause fired
// as vacuous on the second — the run had passed by luck. `T(Neg, KNOWS, Neg)` is
// touched by nothing but the fixture, and the two Neg nodes are excluded from
// both the delete and the link draws, so the cell can only ever move DOWN.
const (
	csLabelNeg      = "Neg"
	csNegNamePrefix = "cs-neg"
)

// The two churn-free label templates the negative-cell fixture uses. They are
// modelled by [GraphOracle.applyCountStoreRelabel], reached from
// [GraphOracle.ApplyMatch].
const (
	tmplAddNeg    = "MATCH (n:Person {name:$name}) SET n:Neg"
	tmplRemoveNeg = "MATCH (n:Person {name:$name}) REMOVE n:Neg"
)

// applyCountStoreRelabel models [tmplAddNeg] / [tmplRemoveNeg]: the matched
// Person gains or loses the Neg label. A name miss is a committed no-op, exactly
// as the engine's MATCH-found-nothing behaves. It returns ok=false for a template
// that is not one of these two, so [GraphOracle.ApplyMatch] falls through.
func (o *GraphOracle) applyCountStoreRelabel(cypher string, params map[string]any) (OracleResult, bool) {
	switch cypher {
	case tmplAddNeg, tmplRemoveNeg:
	default:
		return OracleResult{}, false
	}
	name, ok := paramString(params, "name")
	if !ok {
		return OracleResult{ErrorMsg: "oracle: count-store relabel missing name"}, true
	}
	if id, found := o.byName[name]; found {
		if cypher == tmplAddNeg {
			o.addLabel(id, csLabelNeg)
		} else {
			o.removeLabel(id, csLabelNeg)
		}
	}
	return OracleResult{Committed: true}, true
}

// createPersonHub models [tmplCreatePersonHub]: a Person carrying the additional
// Hub label. It delegates the whole Person model to [GraphOracle.createPerson]
// and then adds the second label, so the two templates cannot drift apart on
// anything but the label set.
func (o *GraphOracle) createPersonHub(params map[string]any) OracleResult {
	res := o.createPerson(params)
	if !res.Committed {
		return res
	}
	name, _ := paramString(params, "name")
	if id, ok := o.byName[name]; ok {
		o.addLabel(id, csLabelHub)
	}
	return res
}

// -----------------------------------------------------------------------------
// The model — recomputed from the shadow model, keyed by NAME
// -----------------------------------------------------------------------------

// csDKey is a degree-sum cell key in NAME space: the count-store keys `D` by
// `dkey(labelID, relTypeID)`, and this is the same cell addressed by the names
// the shadow model holds.
type csDKey struct{ label, rel string }

// csTKey is a triple cell key in NAME space, the name-space counterpart of the
// count-store's `[3]uint32{labelA, relType, labelB}`.
type csTKey struct{ a, rel, b string }

// csModel is the count-store state the shadow model implies: one entry per
// cell that should be live, keyed by name. An ABSENT key means zero, matching
// the store, where a cell is deleted the moment its counter returns to zero.
type csModel struct {
	e    map[string]int64
	dOut map[csDKey]int64
	dIn  map[csDKey]int64
	t    map[csTKey]int64
	// labels and rels are the vocabulary this model observed, which the
	// boundedness clause turns into a combinatorial ceiling.
	labels map[string]struct{}
	rels   map[string]struct{}
	// edges is how many modelled edges contributed, the independent term the
	// sum(E) cross-check compares against.
	edges int64
}

// countStoreModel recomputes the count-store's three statistics from the shadow
// model alone: for every modelled edge, `E(relType)` gains one, `D(label,
// relType, OUT)` gains one per label of the SOURCE, `D(label, relType, IN)` one
// per label of the DESTINATION, and `T(a, relType, b)` one per (source label,
// destination label) pair. That fan-out mirrors `enqueueEdgeDeltas`, which is
// the definition of the statistics, not a copy of their maintenance: the
// maintenance path enqueues deltas incrementally per write, while this walks the
// modelled edge set once.
//
// An edge whose endpoint the model no longer holds is skipped, so a stale edge
// entry could not silently inflate a cell. In the modelled workload no such
// entry exists — `ApplyDelete` removes a deleted node's incident edges — and the
// guard is what makes that a checked assumption rather than an unchecked one.
func (o *GraphOracle) countStoreModel() csModel {
	m := csModel{
		e:      make(map[string]int64),
		dOut:   make(map[csDKey]int64),
		dIn:    make(map[csDKey]int64),
		t:      make(map[csTKey]int64),
		labels: make(map[string]struct{}),
		rels:   make(map[string]struct{}),
	}
	// Every modelled node's labels count towards the observed vocabulary, even a
	// node with no incident edge: the store's ceiling is a function of the SCHEMA
	// the workload has exercised, and a label on an edgeless node is one relabel
	// away from producing cells.
	for _, n := range o.nodes {
		for _, l := range n.Labels {
			m.labels[l] = struct{}{}
		}
	}
	for k, e := range o.edges {
		src, srcOK := o.nodes[k.src]
		dst, dstOK := o.nodes[k.dst]
		if !srcOK || !dstOK {
			continue
		}
		m.rels[e.Label] = struct{}{}
		m.e[e.Label]++
		m.edges++
		for _, la := range src.Labels {
			m.dOut[csDKey{label: la, rel: e.Label}]++
		}
		for _, lb := range dst.Labels {
			m.dIn[csDKey{label: lb, rel: e.Label}]++
		}
		for _, la := range src.Labels {
			for _, lb := range dst.Labels {
				m.t[csTKey{a: la, rel: e.Label, b: lb}]++
			}
		}
	}
	return m
}

// knowsPatternCount returns how many modelled KNOWS edges run from a node
// carrying srcLabel to a node carrying dstLabel, counting an edge only when the
// model still holds BOTH endpoints. An empty label means "no constraint", so
// `knowsPatternCount("", "")` is the reference for `MATCH ()-[:KNOWS]->()` and
// `knowsPatternCount("Person", "Person")` the reference for
// `MATCH (:Person)-[:KNOWS]->(:Person)`.
//
// It is deliberately a DIFFERENT derivation from [GraphOracle.knowsCount], which
// counts edge-map keys by label and consults no endpoint at all: a reference
// that reused knowsCount would make the labelled shape a restatement of the
// unlabelled one rather than a second oracle. On a simple graph one
// (src, dst, KNOWS) triple is at most one edge, so this is a pattern-match
// cardinality, matching `count(*)`'s one-row-per-binding semantics.
func (o *GraphOracle) knowsPatternCount(srcLabel, dstLabel string) int64 {
	var n int64
	for k, e := range o.edges {
		if e.Label != csRelKnows {
			continue
		}
		src, srcOK := o.nodes[k.src]
		dst, dstOK := o.nodes[k.dst]
		if !srcOK || !dstOK {
			continue
		}
		if srcLabel != "" && !hasLabel(src, srcLabel) {
			continue
		}
		if dstLabel != "" && !hasLabel(dst, dstLabel) {
			continue
		}
		n++
	}
	return n
}

// personNamesWithLabel returns the ascending-sorted names of every modelled
// Person that currently carries label, so the churn actor can pick a REMOVE
// target that actually has the label (a REMOVE of an absent label reaches
// `countRelabel` and returns immediately, exercising nothing).
func (o *GraphOracle) personNamesWithLabel(label string) []string {
	out := make([]string, 0, len(o.nodes))
	for _, n := range o.nodes {
		if !hasLabel(n, label) {
			continue
		}
		if name, ok := n.Properties["name"].(string); ok {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// -----------------------------------------------------------------------------
// The observation — the store's own snapshot, resolved into NAME space
// -----------------------------------------------------------------------------

// csObserved is one `count.Snapshot` translated into the same name space as
// [csModel], plus the four dirty label sets and the store's cell count. An id
// the registry cannot resolve is recorded rather than dropped: dropping it would
// silently narrow the comparison to whatever happened to resolve.
type csObserved struct {
	e    map[string]int64
	dOut map[csDKey]int64
	dIn  map[csDKey]int64
	t    map[csTKey]int64

	dirtyDOut map[string]struct{}
	dirtyDIn  map[string]struct{}
	dirtyTA   map[string]struct{}
	dirtyTB   map[string]struct{}

	// unresolved names the raw ids whose registry lookup failed, with the family
	// they came from.
	unresolved []string
	// negatives lists the cells whose counter is below zero, in a stable rendered
	// form. `Store.add` retains such a cell deliberately (rmp #2303) and
	// `Snapshot` returns it, so it is observed rather than filtered.
	negatives []string
	// negativeUncovered lists the negative cells NO dirty marking covers, which
	// is the defect case.
	negativeUncovered []string
	// cells is `Store.Cells()`, the store's own footprint indicator.
	cells int
}

// csResolve resolves one interned id to its name through reg, recording a
// failure in *bad rather than guessing.
func csResolve(reg *lpg.LabelRegistry, id uint32, family string, bad *[]string) (string, bool) {
	name, ok := reg.Resolve(lpg.LabelID(id))
	if !ok || name == "" {
		*bad = append(*bad, fmt.Sprintf("%s id=%d", family, id))
		return "", false
	}
	return name, true
}

// csObserve translates snap into name space through the registry of the graph
// the snapshot was taken from, and classifies every negative cell as
// dirty-covered or not.
//
// The registry MUST be the one belonging to that same graph generation: a
// recovered graph re-interns from scratch, so resolving a pre-crash snapshot
// through a post-crash registry would compare two different id spaces. Every
// caller here takes both from the same [Simulator] in one step.
func csObserve(reg *lpg.LabelRegistry, snap *count.Snapshot, cells int) csObserved {
	obs := csObserved{
		e:         make(map[string]int64, len(snap.E)),
		dOut:      make(map[csDKey]int64, len(snap.DOut)),
		dIn:       make(map[csDKey]int64, len(snap.DIn)),
		t:         make(map[csTKey]int64, len(snap.T)),
		dirtyDOut: csDirtySet(reg, snap.DirtyDOut),
		dirtyDIn:  csDirtySet(reg, snap.DirtyDIn),
		dirtyTA:   csDirtySet(reg, snap.DirtyTA),
		dirtyTB:   csDirtySet(reg, snap.DirtyTB),
		cells:     cells,
	}
	for id, v := range snap.E {
		if rel, ok := csResolve(reg, id, "E.relType", &obs.unresolved); ok {
			obs.e[rel] = v
		}
	}
	readD := func(m map[uint64]int64, dst map[csDKey]int64, family string) {
		for k, v := range m {
			label, ok1 := csResolve(reg, uint32(k>>32), family+".label", &obs.unresolved)
			rel, ok2 := csResolve(reg, uint32(k), family+".relType", &obs.unresolved)
			if ok1 && ok2 {
				dst[csDKey{label: label, rel: rel}] = v
			}
		}
	}
	readD(snap.DOut, obs.dOut, "DOut")
	readD(snap.DIn, obs.dIn, "DIn")
	for k, v := range snap.T {
		a, ok1 := csResolve(reg, k[0], "T.labelA", &obs.unresolved)
		rel, ok2 := csResolve(reg, k[1], "T.relType", &obs.unresolved)
		b, ok3 := csResolve(reg, k[2], "T.labelB", &obs.unresolved)
		if ok1 && ok2 && ok3 {
			obs.t[csTKey{a: a, rel: rel, b: b}] = v
		}
	}
	sort.Strings(obs.unresolved)
	obs.classifyNegatives()
	return obs
}

// classifyNegatives records every cell below zero and, of those, the ones no
// dirty marking covers. It runs after the maps are populated so it sees the same
// name space every clause does.
func (obs *csObserved) classifyNegatives() {
	note := func(rendered string, covered bool) {
		obs.negatives = append(obs.negatives, rendered)
		if !covered {
			obs.negativeUncovered = append(obs.negativeUncovered, rendered)
		}
	}
	for rel, v := range obs.e {
		if v < 0 {
			// E is never dirty — the store has no E-scoped dirty set — so a negative
			// E cell is uncoverable by construction.
			note(fmt.Sprintf("E[%s]=%d", rel, v), false)
		}
	}
	for k, v := range obs.dOut {
		if v < 0 {
			note(fmt.Sprintf("DOut[%s,%s]=%d", k.label, k.rel, v), obs.dOutDirty(k.label))
		}
	}
	for k, v := range obs.dIn {
		if v < 0 {
			note(fmt.Sprintf("DIn[%s,%s]=%d", k.label, k.rel, v), obs.dInDirty(k.label))
		}
	}
	for k, v := range obs.t {
		if v < 0 {
			note(fmt.Sprintf("T[%s,%s,%s]=%d", k.a, k.rel, k.b, v), obs.tDirty(k))
		}
	}
	sort.Strings(obs.negatives)
	sort.Strings(obs.negativeUncovered)
}

// csDirtySet resolves a dirty label-id slice into a name set. The slices come
// back in UNSPECIFIED order, so nothing downstream may depend on their sequence;
// a set is the shape every consumer here wants anyway.
func csDirtySet(reg *lpg.LabelRegistry, ids []uint32) map[string]struct{} {
	out := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if name, ok := reg.Resolve(lpg.LabelID(id)); ok && name != "" {
			out[name] = struct{}{}
		}
	}
	return out
}

// dOutDirty / dInDirty / tDirty report whether a cell is covered by a dirty
// marking and therefore NOT required to be exact. `T` is covered when either
// position's label is marked, matching [count.Store.TDirty].
func (obs *csObserved) dOutDirty(label string) bool {
	_, ok := obs.dirtyDOut[label]
	return ok
}

func (obs *csObserved) dInDirty(label string) bool {
	_, ok := obs.dirtyDIn[label]
	return ok
}

func (obs *csObserved) tDirty(k csTKey) bool {
	if _, ok := obs.dirtyTA[k.a]; ok {
		return true
	}
	_, ok := obs.dirtyTB[k.b]
	return ok
}

// anyDirty reports whether any of the four sets holds a label.
func (obs *csObserved) anyDirty() bool {
	return len(obs.dirtyDOut)+len(obs.dirtyDIn)+len(obs.dirtyTA)+len(obs.dirtyTB) > 0
}

// dirtyString renders the four sets deterministically for a violation message.
func (obs *csObserved) dirtyString() string {
	one := func(m map[string]struct{}) string {
		names := make([]string, 0, len(m))
		for n := range m {
			names = append(names, n)
		}
		sort.Strings(names)
		return "[" + strings.Join(names, " ") + "]"
	}
	return fmt.Sprintf("DOut:%s DIn:%s TA:%s TB:%s",
		one(obs.dirtyDOut), one(obs.dirtyDIn), one(obs.dirtyTA), one(obs.dirtyTB))
}

// -----------------------------------------------------------------------------
// The phases and the perturbation seam
// -----------------------------------------------------------------------------

// csPhase selects how strict the parity clauses are.
type csPhase uint8

const (
	// csPhaseLive is a check on the incrementally-maintained store. A
	// dirty-covered cell is SKIPPED: the maintenance path deliberately marks the
	// IN-side families non-exact instead of writing a wrong exact (design
	// §3.3.1), so requiring exactness there would fail correct code.
	csPhaseLive csPhase = iota
	// csPhaseRecovered is a check immediately after a reopen, where
	// `recomputeCountStore` has run. NOTHING is skipped and every dirty set must
	// be EMPTY: the reopen is the documented self-heal point.
	csPhaseRecovered
)

// String renders a phase for a violation message.
func (p csPhase) String() string {
	if p == csPhaseRecovered {
		return "recovered"
	}
	return "live"
}

// csPerturb is the sensitivity seam: a deliberate corruption threaded in as a
// PARAMETER so a falsifiability test can prove each named clause fires, and so
// the perturbation can never leak into a scenario run (there is no package
// variable to forget to reset).
type csPerturb uint8

// The perturbations. Each targets ONE clause, so a test can assert that the
// clause it names fires and that the others stay silent.
const (
	// csPerturbNone is the control: the check runs exactly as the scenario runs it.
	csPerturbNone csPerturb = iota
	// csPerturbDropE deletes one entry from the MODEL's E map, so the store looks
	// like it over-counts. It targets `parity:E`.
	csPerturbDropE
	// csPerturbInflateT adds one to a modelled T cell. It targets `parity:T`.
	csPerturbInflateT
	// csPerturbDropDOut deletes one modelled DOut cell. It targets `parity:DOut`.
	csPerturbDropDOut
	// csPerturbDropDIn deletes one modelled DIn cell. It targets `parity:DIn`.
	csPerturbDropDIn
	// csPerturbFakeDirty injects a label into the OBSERVED post-recovery dirty
	// sets. It targets `dirty-after-recovery`.
	csPerturbFakeDirty
	// csPerturbUncoverNegative clears the observed dirty sets while leaving the
	// negative cells in place, so a legitimately-covered negative becomes an
	// uncovered one. It targets `negative-uncovered`.
	csPerturbUncoverNegative
	// csPerturbShrinkBound lowers the combinatorial ceiling to 1, below any
	// non-trivial footprint. It targets `cells-bound`.
	csPerturbShrinkBound
	// csPerturbSumE inflates the model's edge total. It targets `sum-E`.
	csPerturbSumE
	// csPerturbUnresolvedID injects an unresolvable id into the observation. It
	// targets `unresolvable-id`.
	csPerturbUnresolvedID
)

// String renders a perturbation for a test name and a report.
func (p csPerturb) String() string {
	switch p {
	case csPerturbNone:
		return "none"
	case csPerturbDropE:
		return "drop-E"
	case csPerturbInflateT:
		return "inflate-T"
	case csPerturbDropDOut:
		return "drop-DOut"
	case csPerturbDropDIn:
		return "drop-DIn"
	case csPerturbFakeDirty:
		return "fake-dirty"
	case csPerturbUncoverNegative:
		return "uncover-negative"
	case csPerturbShrinkBound:
		return "shrink-bound"
	case csPerturbSumE:
		return "sum-E"
	case csPerturbUnresolvedID:
		return "unresolved-id"
	default:
		return fmt.Sprintf("csPerturb(%d)", uint8(p))
	}
}

// csPerturbed returns a perturbed COPY of the model, the observation and the
// bound. It never mutates its inputs: every map it touches is cloned first, so a
// perturbation cannot leak into the caller's state through a shared map — which
// is exactly what an in-place version did, and what made the whole seam depend on
// the caller happening to read its evidence before calling the check.
//
// The map-targeting perturbations pick the LOWEST key in a deterministic sort
// order rather than ranging: Go's map iteration order is randomised, and a
// falsifiability test that perturbed a different cell on each run would be a
// flaky test rather than a proof.
func csPerturbed(m *csModel, obs *csObserved, bound int, p csPerturb) (csModel, csObserved, int) {
	mc, oc := *m, *obs
	switch p {
	case csPerturbNone:
	case csPerturbDropE:
		mc.e = csCloneStringMap(m.e)
		if k, ok := csLowestString(mc.e); ok {
			delete(mc.e, k)
		}
	case csPerturbInflateT:
		mc.t = csCloneTMap(m.t)
		if k, ok := csLowestT(mc.t); ok {
			mc.t[k]++
		}
	case csPerturbDropDOut:
		mc.dOut = csCloneDMap(m.dOut)
		if k, ok := csLowestD(mc.dOut); ok {
			delete(mc.dOut, k)
		}
	case csPerturbDropDIn:
		mc.dIn = csCloneDMap(m.dIn)
		if k, ok := csLowestD(mc.dIn); ok {
			delete(mc.dIn, k)
		}
	case csPerturbFakeDirty:
		oc.dirtyDIn = csCloneLabelSet(obs.dirtyDIn)
		oc.dirtyDIn["<perturb-fake-dirty>"] = struct{}{}
	case csPerturbUncoverNegative:
		oc.dirtyDOut = map[string]struct{}{}
		oc.dirtyDIn = map[string]struct{}{}
		oc.dirtyTA = map[string]struct{}{}
		oc.dirtyTB = map[string]struct{}{}
		oc.negatives = nil
		oc.negativeUncovered = nil
		oc.classifyNegatives()
	case csPerturbShrinkBound:
		// One, not zero: a zero bound DISABLES the clause, so zeroing it would have
		// proved the opposite of what this perturbation is for.
		bound = 1
	case csPerturbSumE:
		mc.edges++
	case csPerturbUnresolvedID:
		oc.unresolved = append(append([]string(nil), obs.unresolved...),
			"<perturb-unresolvable> id=4294967295")
	}
	return mc, oc, bound
}

// The clone helpers the perturbations use. Each returns a fresh map with the same
// contents, so the perturbed copy and the original are independent.
func csCloneStringMap(m map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func csCloneDMap(m map[csDKey]int64) map[csDKey]int64 {
	out := make(map[csDKey]int64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func csCloneTMap(m map[csTKey]int64) map[csTKey]int64 {
	out := make(map[csTKey]int64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func csCloneLabelSet(m map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(m))
	for k := range m {
		out[k] = struct{}{}
	}
	return out
}

// csLowestString returns the lexicographically smallest key of a string-keyed
// map, for deterministic perturbation targeting.
func csLowestString(m map[string]int64) (string, bool) {
	best, found := "", false
	for k := range m {
		if !found || k < best {
			best, found = k, true
		}
	}
	return best, found
}

// csLowestD returns the smallest [csDKey] under (label, rel) ordering.
func csLowestD(m map[csDKey]int64) (csDKey, bool) {
	var best csDKey
	found := false
	for k := range m {
		if !found || csDKeyLess(k, best) {
			best, found = k, true
		}
	}
	return best, found
}

// csLowestT returns the smallest [csTKey] under (a, rel, b) ordering.
func csLowestT(m map[csTKey]int64) (csTKey, bool) {
	var best csTKey
	found := false
	for k := range m {
		if !found || csTKeyLess(k, best) {
			best, found = k, true
		}
	}
	return best, found
}

func csDKeyLess(a, b csDKey) bool {
	if a.label != b.label {
		return a.label < b.label
	}
	return a.rel < b.rel
}

func csTKeyLess(a, b csTKey) bool {
	if a.a != b.a {
		return a.a < b.a
	}
	if a.rel != b.rel {
		return a.rel < b.rel
	}
	return a.b < b.b
}

// -----------------------------------------------------------------------------
// The clauses
// -----------------------------------------------------------------------------

// csOp renders a clause name as a report op label.
func csOp(clause string) string { return "<count-store:" + clause + ">" }

// csViolation builds a violation for a clause.
func csViolation(kind ViolationKind, tick int64, clause, format string, args ...any) Violation {
	return Violation{Kind: kind, Tick: tick, Op: csOp(clause), Message: fmt.Sprintf(format, args...)}
}

// checkCountStoreParity adjudicates one observation of the count store against
// the model, cell by cell, in both directions.
//
// Every family is a SEPARATE clause, because they fail for different reasons: a
// wrong `E` is a broken edge-lifecycle delta, a wrong `D` a broken endpoint-label
// fan-out, a wrong `T` a broken pair fan-out, and a leftover dirty marking after
// a reopen a broken `RecomputeReset`. A single "counts disagree" clause would
// tell an operator nothing about which of the four to look at.
//
// Both directions are checked for each family. Comparing only model→store would
// miss a LEAK — a stray cell the model does not produce — which is exactly what
// a missed decrement or a failed reset looks like.
//
// bound is the combinatorial `Cells()` ceiling the caller computed from the
// vocabulary observed so far; a non-positive bound disables that clause (the
// caller passes one only where it has a vocabulary to compute it from).
func checkCountStoreParity(
	tick int64, phase csPhase, m *csModel, obs *csObserved, bound int, p csPerturb,
) ([]Violation, csCompared) {
	if p != csPerturbNone {
		// A perturbation runs on COPIES, with every map it touches cloned, so it
		// cannot leak into the caller's model or observation. The unperturbed path —
		// every scenario run — allocates nothing here.
		mc, oc, b := csPerturbed(m, obs, bound, p)
		m, obs, bound = &mc, &oc, b
	}

	var cmp csCompared
	var vs []Violation
	add := func(clause, format string, args ...any) {
		vs = append(vs, csViolation(ViolationOracleDeviation, tick, clause, format, args...))
	}

	// An id nothing could resolve makes every other clause on this observation
	// unattributable, so it is reported first and loudly.
	if len(obs.unresolved) > 0 {
		add("unresolvable-id", "%s phase: the count store holds %d cell key(s) whose interned id the "+
			"graph's registry cannot resolve: %v. Every other clause on this observation is "+
			"unattributable, because a cell that cannot be named cannot be compared",
			phase, len(obs.unresolved), obs.unresolved)
	}

	// E: exact in both directions, in EVERY phase. The store carries no E-scoped
	// dirty set, so there is nothing that could excuse a wrong E cell.
	for rel, want := range m.e {
		cmp[csFamilyE]++
		if got := obs.e[rel]; got != want {
			add("parity:E", "%s phase: E(%s) store=%d model=%d", phase, rel, got, want)
		}
	}
	for rel, got := range obs.e {
		if want := m.e[rel]; want != got {
			add("parity:E", "%s phase: E(%s) store=%d model=%d (store cell the model does not produce)",
				phase, rel, got, want)
		}
	}

	// D and T: exact unless the phase permits a dirty skip.
	skipD := func(label string, dirty func(string) bool) bool {
		return phase == csPhaseLive && dirty(label)
	}
	cmpD := func(clause string, fam csFamily, want, got map[csDKey]int64, dirty func(string) bool) {
		for k, w := range want {
			if skipD(k.label, dirty) {
				continue
			}
			cmp[fam]++
			if g := got[k]; g != w {
				add(clause, "%s phase: %s(label=%s, rel=%s) store=%d model=%d (dirty=%s)",
					phase, clause, k.label, k.rel, g, w, obs.dirtyString())
			}
		}
		for k, g := range got {
			if skipD(k.label, dirty) {
				continue
			}
			if w := want[k]; w != g {
				add(clause, "%s phase: %s(label=%s, rel=%s) store=%d model=%d (store cell the model "+
					"does not produce; dirty=%s)", phase, clause, k.label, k.rel, g, w, obs.dirtyString())
			}
		}
	}
	cmpD("parity:DOut", csFamilyDOut, m.dOut, obs.dOut, obs.dOutDirty)
	cmpD("parity:DIn", csFamilyDIn, m.dIn, obs.dIn, obs.dInDirty)

	skipT := func(k csTKey) bool { return phase == csPhaseLive && obs.tDirty(k) }
	for k, w := range m.t {
		if skipT(k) {
			continue
		}
		cmp[csFamilyT]++
		if g := obs.t[k]; g != w {
			add("parity:T", "%s phase: T(%s, %s, %s) store=%d model=%d (dirty=%s)",
				phase, k.a, k.rel, k.b, g, w, obs.dirtyString())
		}
	}
	for k, g := range obs.t {
		if skipT(k) {
			continue
		}
		if w := m.t[k]; w != g {
			add("parity:T", "%s phase: T(%s, %s, %s) store=%d model=%d (store cell the model does not "+
				"produce; dirty=%s)", phase, k.a, k.rel, k.b, g, w, obs.dirtyString())
		}
	}

	// sum(E) against the modelled edge total: an INDEPENDENT cross-check that does
	// not go through the per-cell comparison above. Every modelled edge is typed,
	// so the two must be the same number.
	var sumE int64
	for _, v := range obs.e {
		sumE += v
	}
	if sumE != m.edges {
		add("sum-E", "%s phase: sum over E cells = %d, modelled edge count = %d. Every modelled edge "+
			"carries a relationship type, so the two are the same quantity reached two ways",
			phase, sumE, m.edges)
	}

	// A negative cell is legitimate ONLY while a dirty marking covers it: that is
	// what the store's order-insensitivity design buys (rmp #2303, Store.add). An
	// uncovered negative is a lost decrement presented as an exact count.
	if len(obs.negativeUncovered) > 0 {
		add("negative-uncovered", "%s phase: %d negative cell(s) no dirty marking covers: %v "+
			"(dirty=%s). Store.add retains a negative cell deliberately, but only a DIRTY family may "+
			"report one: an uncovered negative is a decrement whose increment was lost, offered to the "+
			"planner as exact", phase, len(obs.negativeUncovered), obs.negativeUncovered, obs.dirtyString())
	}

	// The reopen's self-heal claim: after `recomputeCountStore` every family is
	// exact, so no dirty marking may survive.
	if phase == csPhaseRecovered && obs.anyDirty() {
		add("dirty-after-recovery", "the reopen left dirty markings behind (%s). RecomputeReset clears "+
			"every cell AND every dirty flag before replaying the live edges, so a recovered store must "+
			"be exact on every cell — this is the documented self-heal point (design §4.3, §6.2)",
			obs.dirtyString())
	}

	// The footprint claim: bounded by observed schema cardinality, never by |V| or
	// |E| (design §2.3).
	if bound > 0 && obs.cells > bound {
		add("cells-bound", "%s phase: Store.Cells()=%d exceeds the combinatorial ceiling %d implied by "+
			"the observed vocabulary (%d label(s) x %d relationship type(s)): |R| + 2*|L|*|R| + "+
			"|L|^2*|R|. The store's footprint is a function of schema cardinality, never of |V| or |E|",
			phase, obs.cells, bound, len(m.labels), len(m.rels))
	}
	return vs, cmp
}

// csCompared counts, per cell family, how many model cells one parity
// observation actually COMPARED — that is, did not skip as dirty-covered.
//
// It exists because "the family held cells" and "the family was checked" are
// different claims, and only the second is coverage. MEASURED: after one
// DETACH DELETE the DIn and T families are dirty for the deleted node's labels
// for the rest of the session, so a naive live check compares two empty maps
// while its evidence still reports the family as populated. The per-phase gate
// on this counter is what makes that impossible to miss.
type csCompared [csFamilyCount]int

// csCellsBound returns the combinatorial ceiling on [count.Store.Cells] implied
// by a vocabulary of nLabels labels and nRels relationship types: one E cell per
// type, one D cell per (label, type) in each of two directions, and one T cell
// per (label, type, label) triple.
//
// It returns 0 for an empty vocabulary, which disables the clause: a run that has
// observed no relationship type yet has no ceiling to state.
func csCellsBound(nLabels, nRels int) int {
	if nRels == 0 {
		return 0
	}
	return nRels + 2*nLabels*nRels + nLabels*nLabels*nRels
}

// -----------------------------------------------------------------------------
// The query shapes
// -----------------------------------------------------------------------------

// csShape is one `count(*)` pattern shape: the query, the endpoint labels its
// reference is computed from, and whether a count-store `T` cell serves it.
type csShape struct {
	// name is the clause suffix a violation is reported under.
	name string
	// query is the exact statement issued.
	query string
	// srcLabel / dstLabel are the endpoint label constraints the reference is
	// derived from; "" means the shape constrains that endpoint not at all.
	srcLabel, dstLabel string
}

// csShapes returns the six `count(*)` shapes this scenario adjudicates.
//
// The first two are the shapes the requirement names, and they are the shapes
// the count store's `E` and `T` statistics serve. Both use ANONYMOUS pattern
// elements, which is a genuinely different plan shape from the bound-variable
// `MATCH (a:Person)-[:KNOWS]->(b:Person)` the surface battery already drove.
//
// The remaining four exist because the first two cannot discriminate in a
// single-labelled population: where every node carries `Person`, the
// `(:Person)-[:KNOWS]->(:Person)` reference is numerically the `()-[:KNOWS]->()`
// reference and the labelled clause can only fail when the unlabelled one
// already has. This scenario churns `Vip` over a subset and pins `Hub` on a
// never-deleted pool, so each of those four has a reference that genuinely
// differs — and each is the shape whose `T` cell the parity check is watching.
// The two `Hub` shapes are additionally the only ones whose cell is comparable
// while LIVE (see [csLabelHub]).
func csShapes() []csShape {
	return []csShape{
		{name: "any-knows-any", query: "MATCH ()-[:KNOWS]->() RETURN count(*)"},
		{name: "person-knows-person",
			query:    "MATCH (:Person)-[:KNOWS]->(:Person) RETURN count(*)",
			srcLabel: csLabelPerson, dstLabel: csLabelPerson},
		// ANONYMOUS pattern elements, deliberately. This spelling used to return
		// the wrong answer whenever the anchor swap was admissible — which is
		// exactly after a reopen clears the dirty flags — and it is the spelling
		// this scenario surfaced that defect with. rmp #2603 fixed it, so the
		// shapes are back to the anonymous form and this scenario drives it again.
		// See the defect section in this file's header.
		{name: "vip-knows-person",
			query:    "MATCH (:Vip)-[:KNOWS]->(:Person) RETURN count(*)",
			srcLabel: csLabelVip, dstLabel: csLabelPerson},
		{name: "person-knows-vip",
			query:    "MATCH (:Person)-[:KNOWS]->(:Vip) RETURN count(*)",
			srcLabel: csLabelPerson, dstLabel: csLabelVip},
		// The two HUB shapes are the ones whose serving T cell is comparable while
		// LIVE. Every other labelled shape's cell is dirty-covered for most of a
		// churning run — MEASURED, a single DETACH DELETE dirties `TB(Person)` for
		// the rest of the session — so without these the three-way
		// query/model/cell comparison would only ever happen just after a reopen.
		{name: "person-knows-hub",
			query:    "MATCH (a:Person)-[:KNOWS]->(b:Hub) RETURN count(*)",
			srcLabel: csLabelPerson, dstLabel: csLabelHub},
		{name: "hub-knows-hub",
			query:    "MATCH (a:Hub)-[:KNOWS]->(b:Hub) RETURN count(*)",
			srcLabel: csLabelHub, dstLabel: csLabelHub},
	}
}

// csShapeCellCheck reports whether the count-store cell that serves shape can be
// compared against the query answer, and what that cell says.
//
// It is comparable only for a fully-labelled shape whose `T` cell is EXACT: a
// dirty-covered cell is documented non-exact, and an unlabelled endpoint is not
// a `T` cell at all (it is served, if at all, by `E`). Returning the
// comparability verdict separately is what keeps the clause from silently
// skipping: the caller counts the comparisons it actually made.
func csShapeCellCheck(sh csShape, obs *csObserved) (cell int64, ok bool) {
	if sh.srcLabel == "" || sh.dstLabel == "" {
		return 0, false
	}
	k := csTKey{a: sh.srcLabel, rel: csRelKnows, b: sh.dstLabel}
	if obs.tDirty(k) {
		return 0, false
	}
	return obs.t[k], true
}

// -----------------------------------------------------------------------------
// The evidence
// -----------------------------------------------------------------------------

// CountStoreEvidence is what the run measured, and the basis of its terminal
// assert-something-was-seen gate.
type CountStoreEvidence struct {
	// LiveChecks / RecoveredChecks count the parity observations in each phase.
	// They are separate because the two phases assert DIFFERENT things: a run
	// with no recovered check never evaluated the self-heal claim at all.
	LiveChecks      int
	RecoveredChecks int
	// DirtyLiveChecks is how many live observations found a non-empty dirty set.
	// Without one, "the reopen cleared the dirty sets" is satisfied by a run that
	// never dirtied anything.
	DirtyLiveChecks int
	// NegativeLiveChecks is how many live observations held a negative cell, and
	// NegativeCellsSeen the total number of such cells across the run.
	NegativeLiveChecks int
	NegativeCellsSeen  int
	// NegativeArms is how many negative-cell fixtures the run constructed: one in
	// the prologue and one after every recovery, so the heal is witnessed on every
	// reopen rather than only on the first.
	NegativeArms int
	// HealedFromDirty counts recoveries whose IMMEDIATELY PRECEDING live
	// observation was dirty and whose post-recovery observation was clean — the
	// clause that makes the heal a measured transition rather than a static
	// property. HealedNegative is the same for a negative cell.
	HealedFromDirty int
	HealedNegative  int
	// CellFamilies[i] counts the live observations in which family i held at least
	// one cell, indexed by [csFamily]. A run that never populated `T` proved
	// nothing about the pair fan-out.
	CellFamilies [csFamilyCount]int
	// ComparedLive[i] / ComparedRecovered[i] count the model cells of family i that
	// were actually COMPARED — not skipped as dirty-covered — in each phase.
	//
	// These are the counters that matter, and they are separate from CellFamilies
	// for a measured reason: after a single DETACH DELETE the DIn and T families
	// are dirty for the deleted node's labels for the rest of the session, so a
	// live check can hold plenty of cells and compare none of them. The Hub label
	// exists to keep some of them exact; these counters are what prove it worked.
	ComparedLive      csCompared
	ComparedRecovered csCompared
	// DistinctTLabelsMax is the largest number of distinct labels ever seen in the
	// a-position of a `T` cell. One means the multi-label fan-out never happened,
	// so the `T` clause only ever compared the trivial single-label case.
	DistinctTLabelsMax int
	// ShapeProbes[i] counts the adjudications of shape i, ShapeCellChecks[i] the
	// subset that also compared the serving count-store cell, and
	// ShapeCellSkipped[i] the subset where that cell was dirty-covered and so
	// could not be compared. The skip counter is recorded rather than dropped: a
	// skip is a coverage deletion, and the only way to see how much of the run it
	// removed is to count it.
	ShapeProbes      []int
	ShapeCellChecks  []int
	ShapeCellSkipped []int
	// MaxCells / MaxEdges / MaxBound are the footprint measurements the
	// boundedness clause rests on. MaxEdgesAtMaxCells records the modelled edge
	// count at the observation that held the most cells, so the report can state
	// the ratio the design claims.
	MaxCells           int
	MaxEdges           int64
	MaxBound           int
	MaxEdgesAtMaxCells int64
	// Relabels is how many `SET`/`REMOVE` label ops the churn actor emitted (the
	// only route to `countRelabel`, hence to any dirty marking).
	Relabels int
	// Crashes / ForcedCrashes / Checkpoints are the recovery coverage the
	// recovered-phase clauses depend on.
	Crashes       int
	ForcedCrashes int
	Checkpoints   int
	// Digest folds every clause's (tick, clause, measured pair). It is the
	// scenario's reproducibility claim: same seed, same digest. It folds no
	// NodeID and no mapper key, both of which come from a process-global counter
	// and are not a function of the seed.
	Digest uint64
}

// csFamily indexes the four count-store cell families for the coverage counters.
type csFamily uint8

// The cell families.
const (
	csFamilyE csFamily = iota
	csFamilyDOut
	csFamilyDIn
	csFamilyT
	csFamilyCount
)

// String renders a family for a vacuity message.
func (f csFamily) String() string {
	switch f {
	case csFamilyE:
		return "E"
	case csFamilyDOut:
		return "DOut"
	case csFamilyDIn:
		return "DIn"
	case csFamilyT:
		return "T"
	default:
		return fmt.Sprintf("csFamily(%d)", uint8(f))
	}
}

// String renders the evidence for a report and for the run's own output.
func (e *CountStoreEvidence) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "checks(live=%d recovered=%d) dirtyLive=%d negLive=%d negCells=%d negArms=%d",
		e.LiveChecks, e.RecoveredChecks, e.DirtyLiveChecks, e.NegativeLiveChecks,
		e.NegativeCellsSeen, e.NegativeArms)
	fmt.Fprintf(&b, " healed(dirty=%d negative=%d)", e.HealedFromDirty, e.HealedNegative)
	fmt.Fprintf(&b, " families=%v distinctTLabels=%d", e.CellFamilies, e.DistinctTLabelsMax)
	fmt.Fprintf(&b, " compared(live=%v recovered=%v)", e.ComparedLive, e.ComparedRecovered)
	fmt.Fprintf(&b, " shapes=%v shapeCells=%v shapeCellsSkipped=%v",
		e.ShapeProbes, e.ShapeCellChecks, e.ShapeCellSkipped)
	fmt.Fprintf(&b, " cells(max=%d bound=%d atEdges=%d) maxEdges=%d relabels=%d",
		e.MaxCells, e.MaxBound, e.MaxEdgesAtMaxCells, e.MaxEdges, e.Relabels)
	fmt.Fprintf(&b, " crashes=%d (forced=%d) checkpoints=%d", e.Crashes, e.ForcedCrashes, e.Checkpoints)
	fmt.Fprintf(&b, " digest=%#016x", e.Digest)
	return b.String()
}

// ReproducibleSummary renders exactly the fields that are a pure function of the
// seed. Every field of this evidence is, so it is the full rendering; it exists
// so the determinism test compares what the scenario CLAIMS is reproducible, and
// so a future field that is NOT reproducible has an obvious place to be excluded
// from.
func (e *CountStoreEvidence) ReproducibleSummary() string { return e.String() }

// Finish is the terminal assert-something-was-seen gate: it reports a violation
// for every clause the run did NOT reach.
//
// It is deliberately unconditional on the configuration — it fires just as
// loudly when a budget was lowered as when a clause was deleted — so it cannot
// be silenced by the very change it exists to catch.
func (e *CountStoreEvidence) Finish(tick int64) []Violation {
	var vs []Violation
	add := func(clause, format string, args ...any) {
		vs = append(vs, csViolation(ViolationVacuousRun, tick, "vacuity:"+clause, format, args...))
	}
	if e.LiveChecks == 0 {
		add("live-checks", "no LIVE parity observation ran, so the incrementally-maintained store was "+
			"never compared with the model at all")
	}
	if e.RecoveredChecks == 0 {
		add("recovered-checks", "no parity observation ran on a RECOVERED store, so the reopen-time "+
			"RecomputeReset + replay path — the whole point of this scenario — was executed but never "+
			"asserted. The loop forces a crash at the end precisely so this cannot depend on the "+
			"seeded schedule")
	}
	for f := csFamily(0); f < csFamilyCount; f++ {
		if e.CellFamilies[f] == 0 {
			add("family", "the %s family never held a single cell in any live observation, so its "+
				"parity clause compared two empty maps", f)
		}
		// The clause that matters: a family can be POPULATED and still be entirely
		// skipped as dirty-covered. MEASURED, one DETACH DELETE dirties the deleted
		// node's labels' IN families for the rest of the session, so without the
		// never-churned Hub label the live DIn and T clauses compare nothing at all.
		if e.ComparedLive[f] == 0 {
			add("compared-live", "the %s family had NO cell compared in any live observation (it held "+
				"cells in %d of them): every cell was skipped as dirty-covered, so the live clause for "+
				"this family could not have failed. The Hub label exists to keep some of these cells "+
				"exact under churn", f, e.CellFamilies[f])
		}
		if e.ComparedRecovered[f] == 0 {
			add("compared-recovered", "the %s family had NO cell compared in any RECOVERED observation, "+
				"so the reopen's exactness claim was never tested on this family", f)
		}
	}
	if e.DistinctTLabelsMax < 2 {
		add("t-fan-out", "the T family never saw more than %d distinct label in the a-position, so the "+
			"(source label x destination label) fan-out — the part of enqueueEdgeDeltas a single-label "+
			"population cannot exercise — was never driven", e.DistinctTLabelsMax)
	}
	if e.Relabels == 0 {
		add("relabels", "the run emitted no SET/REMOVE label op, so countRelabel never ran and no "+
			"dirty marking could exist: every dirty clause below was structurally unreachable")
	}
	if e.DirtyLiveChecks == 0 {
		add("dirty-observed", "no live observation ever found a dirty marking (relabels=%d), so "+
			"\"the reopen clears every dirty flag\" was satisfied by there being nothing to clear",
			e.Relabels)
	}
	if e.NegativeLiveChecks == 0 {
		add("negative-observed", "no live observation ever held a negative cell, so the "+
			"negative-uncovered clause never had a negative to classify. The prologue constructs one "+
			"deliberately (SET a:Vip, SET b:Vip, REMOVE a:Vip over an a->b edge), so a zero here means "+
			"that construction stopped working, not that the draws were unlucky")
	}
	if e.HealedFromDirty == 0 {
		add("healed-dirty", "no recovery was ever preceded by a DIRTY live observation, so the run "+
			"never witnessed the transition the self-heal claim is about (dirtyLive=%d recovered=%d)",
			e.DirtyLiveChecks, e.RecoveredChecks)
	}
	if e.HealedNegative == 0 {
		add("healed-negative", "no recovery was ever preceded by a live observation holding a NEGATIVE "+
			"cell, so the reopen was never shown to remove one (negLive=%d negArms=%d recovered=%d)",
			e.NegativeLiveChecks, e.NegativeArms, e.RecoveredChecks)
	}
	// Arm accounting: one arm in the prologue, plus one after every MID-LOOP
	// recovery. The terminal forced recovery is deliberately not re-armed — the run
	// ends there, so a fresh fixture would be constructed and never observed — and
	// since that forced recovery always happens, the two counts come out equal.
	//
	// The check exists because a recovery that slipped past its re-arm would leave
	// the remainder of the run with no negative cell to classify while every
	// counter above still looked healthy: negLive would simply stop rising, which
	// no other clause here would notice.
	if e.NegativeArms != e.RecoveredChecks {
		add("negative-arms", "the negative-cell fixture was armed %d time(s) against %d recovered "+
			"observation(s); one arm in the prologue plus one after each mid-loop recovery, with the "+
			"terminal forced recovery deliberately not re-armed, makes those two counts equal",
			e.NegativeArms, e.RecoveredChecks)
	}
	for i, n := range e.ShapeProbes {
		if n == 0 {
			add("shape", "the %q count(*) shape was never issued, so the query the count store serves "+
				"was never compared with the model", csShapes()[i].name)
		}
	}
	for i, n := range e.ShapeCellChecks {
		sh := csShapes()[i]
		if sh.srcLabel == "" || sh.dstLabel == "" {
			continue // an unlabelled endpoint is not served by a T cell at all.
		}
		if n == 0 {
			add("shape-cell", "the %q shape never had its serving count-store T cell compared against "+
				"the query answer, so the store cell and the row count were never shown to be the same "+
				"number", sh.name)
		}
	}
	if e.MaxBound == 0 {
		add("cells-bound", "no observation ever had a vocabulary to state a Cells() ceiling from, so "+
			"the boundedness clause was disabled for the whole run")
	}
	if e.Crashes == 0 {
		add("crashes", "the run performed NO crash+recovery cycle")
	}
	if e.Checkpoints == 0 {
		add("checkpoints", "the run published NO checkpoint, so no recovery crossed the SNAPSHOT "+
			"boundary and every recovered-phase observation followed a pure WAL replay")
	}
	return vs
}

// csDigest folds a clause's identity and two integers into the running digest.
// It reuses [genSwapMix], the package's byte-at-a-time FNV-1a fold.
func csDigest(h uint64, tick int64, clause string, a, b int64) uint64 {
	h = genSwapMix(h, uint64(tick))
	for i := 0; i < len(clause); i++ {
		h = genSwapMix(h, uint64(clause[i]))
	}
	h = genSwapMix(h, uint64(a))
	return genSwapMix(h, uint64(b))
}

// -----------------------------------------------------------------------------
// The probes
// -----------------------------------------------------------------------------

// CountStoreProbes is the stateful checker: it takes parity observations and
// query-shape adjudications on demand, tracks the monotone vocabulary the
// boundedness ceiling is computed from, and accumulates the evidence its
// terminal gate reads.
//
// # Concurrency contract
//
// CountStoreProbes is NOT safe for concurrent use; the deterministic loop drives
// it from a single goroutine.
type CountStoreProbes struct {
	ev   CountStoreEvidence
	seed *Seed
	// labelsSeen / relsSeen are the MONOTONE vocabulary: names accumulate and are
	// never removed. The ceiling must be monotone because a cell can outlive the
	// last node carrying its label — MEASURED, a `REMOVE n:Vip` that drives a T
	// cell to -1 leaves that cell in the store while the model holds no Vip node
	// at all — so a ceiling recomputed from the CURRENT model would shrink below
	// the store's legitimate footprint and the clause would fire on correct code.
	labelsSeen map[string]struct{}
	relsSeen   map[string]struct{}
	// lastLiveDirty / lastLiveNegative carry the most recent LIVE observation's
	// dirty and negative state forward, so a post-recovery check can assert a
	// TRANSITION rather than a static property.
	lastLiveDirty    bool
	lastLiveNegative bool
	// lastLiveSeen guards the two flags above: before the first live observation
	// there is no transition to claim.
	lastLiveSeen bool
	// negArms is how many negative-cell fixtures have been armed, and supplies the
	// unique node names each arm needs.
	negArms int
}

// NewCountStoreProbes returns a fresh probe set drawing from seed.
func NewCountStoreProbes(seed *Seed) *CountStoreProbes {
	return &CountStoreProbes{
		seed:       seed,
		labelsSeen: make(map[string]struct{}),
		relsSeen:   make(map[string]struct{}),
		ev: CountStoreEvidence{
			ShapeProbes:      make([]int, len(csShapes())),
			ShapeCellChecks:  make([]int, len(csShapes())),
			ShapeCellSkipped: make([]int, len(csShapes())),
		},
	}
}

// Evidence returns the accumulated evidence.
func (p *CountStoreProbes) Evidence() *CountStoreEvidence { return &p.ev }

// observe builds the (model, observation, bound) triple for one check point,
// taking the snapshot and the registry from the SAME simulator in one step so
// they cannot come from different graph generations.
func (p *CountStoreProbes) observe(sm *Simulator) (csModel, csObserved, int) {
	m := sm.oracle.countStoreModel()
	for l := range m.labels {
		p.labelsSeen[l] = struct{}{}
	}
	for r := range m.rels {
		p.relsSeen[r] = struct{}{}
	}
	snap := sm.engine.CountSnapshot()
	obs := csObserve(sm.graph().Registry(), &snap, sm.engine.CountStoreCells())
	return m, obs, csCellsBound(len(p.labelsSeen), len(p.relsSeen))
}

// Parity takes one parity observation in the given phase, records the evidence
// it implies, and returns the clauses that fired.
//
// The recovered phase additionally credits a HEAL when the immediately preceding
// live observation was dirty (or held a negative cell) and this one is not: that
// is the transition the self-heal claim describes, and crediting it only on the
// transition is what stops a clean-all-along run from claiming it.
func (p *CountStoreProbes) Parity(sm *Simulator, tick int64, phase csPhase, perturb csPerturb) []Violation {
	m, obs, bound := p.observe(sm)

	switch phase {
	case csPhaseLive:
		p.ev.LiveChecks++
		if obs.anyDirty() {
			p.ev.DirtyLiveChecks++
		}
		if len(obs.negatives) > 0 {
			p.ev.NegativeLiveChecks++
			p.ev.NegativeCellsSeen += len(obs.negatives)
		}
		p.creditFamilies(&obs)
		p.lastLiveDirty = obs.anyDirty()
		p.lastLiveNegative = len(obs.negatives) > 0
		p.lastLiveSeen = true
	case csPhaseRecovered:
		p.ev.RecoveredChecks++
		if p.lastLiveSeen && p.lastLiveDirty && !obs.anyDirty() {
			p.ev.HealedFromDirty++
		}
		if p.lastLiveSeen && p.lastLiveNegative && len(obs.negatives) == 0 {
			p.ev.HealedNegative++
		}
		// A recovered observation replaces the carried-forward live state: the next
		// heal must be earned by a fresh live observation that dirties again.
		p.lastLiveDirty = obs.anyDirty()
		p.lastLiveNegative = len(obs.negatives) > 0
	}

	if obs.cells > p.ev.MaxCells {
		p.ev.MaxCells = obs.cells
		p.ev.MaxEdgesAtMaxCells = m.edges
	}
	if m.edges > p.ev.MaxEdges {
		p.ev.MaxEdges = m.edges
	}
	if bound > p.ev.MaxBound {
		p.ev.MaxBound = bound
	}
	p.ev.Digest = csDigest(p.ev.Digest, tick, "parity:"+phase.String(), int64(obs.cells), m.edges)

	vs, cmp := checkCountStoreParity(tick, phase, &m, &obs, bound, perturb)
	for f := csFamily(0); f < csFamilyCount; f++ {
		if phase == csPhaseRecovered {
			p.ev.ComparedRecovered[f] += cmp[f]
		} else {
			p.ev.ComparedLive[f] += cmp[f]
		}
	}
	return vs
}

// creditFamilies records which cell families a live observation actually held,
// and how many distinct labels reached the a-position of a T cell.
func (p *CountStoreProbes) creditFamilies(obs *csObserved) {
	if len(obs.e) > 0 {
		p.ev.CellFamilies[csFamilyE]++
	}
	if len(obs.dOut) > 0 {
		p.ev.CellFamilies[csFamilyDOut]++
	}
	if len(obs.dIn) > 0 {
		p.ev.CellFamilies[csFamilyDIn]++
	}
	if len(obs.t) > 0 {
		p.ev.CellFamilies[csFamilyT]++
	}
	aLabels := make(map[string]struct{}, len(obs.t))
	for k := range obs.t {
		aLabels[k.a] = struct{}{}
	}
	if n := len(aLabels); n > p.ev.DistinctTLabelsMax {
		p.ev.DistinctTLabelsMax = n
	}
}

// Shapes adjudicates the six `count(*)` pattern shapes against references
// derived from the shadow model, and — where the shape is served by an EXACT
// count-store T cell — against that cell too.
//
// Three numbers per fully-labelled shape: what the query returned, what the
// model says, and what the store cell holds. The three-way comparison is the
// point: it is what connects "the count store is wrong" to something an operator
// can see, since a wrong store changes the PLAN and not the rows.
func (p *CountStoreProbes) Shapes(
	ctx context.Context, sm *Simulator, tick int64, perturb csPerturb,
) []Violation {
	_, obs, _ := p.observe(sm)
	var vs []Violation
	for i, sh := range csShapes() {
		want := sm.oracle.knowsPatternCount(sh.srcLabel, sh.dstLabel)
		if perturb == csPerturbSumE {
			// The one perturbation that bites here: a model reference off by one must
			// make the shape clause fire, proving the reference is load-bearing.
			want++
		}
		got, err := csScalar(ctx, sm.engine, sh.query)
		if err != nil {
			vs = append(vs, csViolation(ViolationGraphIntegrity, tick, "shape:"+sh.name,
				"%s: %v", sh.query, err))
			continue
		}
		p.ev.ShapeProbes[i]++
		p.ev.Digest = csDigest(p.ev.Digest, tick, "shape:"+sh.name, got, want)
		if got != want {
			vs = append(vs, csViolation(ViolationOracleDeviation, tick, "shape:"+sh.name,
				"%s: engine=%d, model=%d", sh.query, got, want))
			continue
		}
		cell, ok := csShapeCellCheck(sh, &obs)
		if !ok && sh.srcLabel != "" && sh.dstLabel != "" {
			// A labelled shape whose serving cell is dirty-covered: recorded, not
			// dropped, so the report shows how much of the run the skip removed.
			p.ev.ShapeCellSkipped[i]++
		}
		if ok {
			p.ev.ShapeCellChecks[i]++
			if cell != got {
				vs = append(vs, csViolation(ViolationOracleDeviation, tick, "shape-cell:"+sh.name,
					"%s: the query counted %d row(s) but the count-store cell T(%s, %s, %s) that serves "+
						"this shape holds %d. A store that disagrees with the rows it is meant to "+
						"estimate yields a plausible-but-wrong plan, which no result comparison detects",
					sh.query, got, sh.srcLabel, csRelKnows, sh.dstLabel, cell))
			}
		}
	}
	return vs
}

// csScalar runs a single-row scalar read through the engine adapter and returns
// the integer in its first column.
func csScalar(ctx context.Context, engine *EngineAdapter, query string) (int64, error) {
	res, err := engine.Run(ctx, query, nil)
	if err != nil {
		return 0, err
	}
	var n int64
	if res.Next() {
		n, _ = res.ScalarInt()
	}
	drainErr := res.Err()
	closeErr := res.Close()
	if drainErr != nil {
		return 0, fmt.Errorf("drain: %w", drainErr)
	}
	if closeErr != nil {
		return 0, fmt.Errorf("close: %w", closeErr)
	}
	return n, nil
}

// -----------------------------------------------------------------------------
// The actor
// -----------------------------------------------------------------------------

// CountStoreWriter drives exactly the write families the count-store's
// maintenance path hooks, and nothing else: a Person CREATE, a KNOWS CREATE
// between two modelled Persons that are not already linked, a `SET n:Vip` /
// `REMOVE n:Vip` relabel, and a `DETACH DELETE`.
//
// Every template it emits is one the shared [GraphOracle] already models
// ([tmplCreatePerson], [tmplCreateKnows], [tmplAddVip], [tmplRemoveVip],
// [tmplDetachDelete]), so this scenario adds no modelling of its own and cannot
// drift from the oracle.
//
// It never emits a duplicate KNOWS. MEASURED on this harness's simple
// (non-multigraph) durable store, a second `CREATE (a)-[:KNOWS]->(b)` on an
// already-linked pair is REFUSED — `committed=false`, the store unchanged — so
// emitting one would spend a tick on a rejection instead of on a count delta.
// [GraphOracle.HasKnowsByName] is the same guard the edge-property writer uses.
//
// The relabel is the load-bearing family: it is the ONLY route to
// `countRelabel`, and therefore the only way a dirty marking or a negative cell
// can exist at all.
//
// # Concurrency contract
//
// CountStoreWriter is NOT safe for concurrent use; it is invoked from the single
// simulation goroutine.
type CountStoreWriter struct{ counter int64 }

// Name returns the actor's identifier.
func (*CountStoreWriter) Name() string { return "CountStoreWriter" }

// NextOp picks the next write. It is a pure function of (seed state, oracle
// state): every branch either draws from seed or reads the model, never both a
// clock nor a map iteration order.
func (w *CountStoreWriter) NextOp(seed *Seed, oracle *GraphOracle) Op {
	names := oracle.NodeNames()
	if len(names) < 2 {
		return w.opCreatePerson(seed)
	}
	switch seed.IntN(10) {
	case 0, 1:
		return w.opCreatePerson(seed)
	case 2, 3, 4:
		if op, ok := w.opCreateKnows(seed, names, oracle); ok {
			return op
		}
		return w.opCreatePerson(seed)
	case 5, 6:
		return Op{Kind: OpUpdate, Cypher: tmplAddVip,
			Params: map[string]any{"name": names[seed.IntN(len(names))]}}
	case 7, 8:
		// A REMOVE targets a node that ACTUALLY carries the label where one exists:
		// `countRelabel` returns immediately for a label the node does not have, so
		// a blind REMOVE would exercise nothing.
		if vips := oracle.personNamesWithLabel(csLabelVip); len(vips) > 0 {
			return Op{Kind: OpUpdate, Cypher: tmplRemoveVip,
				Params: map[string]any{"name": vips[seed.IntN(len(vips))]}}
		}
		return Op{Kind: OpUpdate, Cypher: tmplAddVip,
			Params: map[string]any{"name": names[seed.IntN(len(names))]}}
	default:
		// A DETACH DELETE draws from the ORDINARY population only. Deleting a Hub node
		// would dirty `DIn(Hub)` and `TB(Hub)` — MEASURED, a DETACH DELETE reaches
		// countRelabel and the dirty sets never shrink until the next reopen — which
		// is precisely what the Hub label exists not to have happen; deleting a Neg
		// node would dismantle the negative-cell fixture. The draw is made
		// unconditionally so the op stream does not depend on whether the filtered set
		// happened to be empty.
		pick := names[seed.IntN(len(names))]
		if csIsProtectedName(pick) {
			return w.opCreatePerson(seed)
		}
		return Op{Kind: OpDelete, Cypher: tmplDetachDelete, Params: map[string]any{"name": pick}}
	}
}

// csIsProtectedName reports whether name belongs to a pool no op may DELETE: the
// Hub pool (whose IN-side cells must stay exact) or the Neg pair (which holds the
// negative-cell fixture).
func csIsProtectedName(name string) bool {
	return strings.HasPrefix(name, csHubNamePrefix) || strings.HasPrefix(name, csNegNamePrefix)
}

// csIsNegName reports whether name belongs to the negative-cell fixture. Those
// two nodes are excluded from the link draw as well: a new edge incident on a Neg
// node would add a POSITIVE delta to the very cell the fixture drives negative.
func csIsNegName(name string) bool { return strings.HasPrefix(name, csNegNamePrefix) }

// opCreatePerson creates a Person under a name unique to this actor, so the
// population never collides and the oracle's name key stays a key.
func (w *CountStoreWriter) opCreatePerson(seed *Seed) Op {
	name := fmt.Sprintf("cs%d", w.counter)
	w.counter++
	return Op{Kind: OpCreate, Cypher: tmplCreatePerson,
		Params: map[string]any{"name": name, "age": int64(seed.IntN(100))}}
}

// opCreateKnows returns a KNOWS create between two distinct, not-already-linked
// modelled Persons, or ok=false when the seeded pair is unusable. It draws
// exactly twice either way, so the caller's fallback does not perturb the draw
// stream relative to a run where the pair was usable.
func (w *CountStoreWriter) opCreateKnows(seed *Seed, names []string, oracle *GraphOracle) (Op, bool) {
	a := names[seed.IntN(len(names))]
	b := names[seed.IntN(len(names))]
	if a == b || oracle.HasKnowsByName(a, b) || csIsNegName(a) || csIsNegName(b) {
		return Op{}, false
	}
	return Op{Kind: OpCreate, Cypher: tmplCreateKnows, Params: map[string]any{"a": a, "b": b}}, true
}

// countStoreWorkload is a 100% [CountStoreWriter] mix. The scenario drives every
// op itself, so the workload exists to keep [New] from building the default one
// from the master seed instance.
func countStoreWorkload(_ *Seed) *Workload {
	return &Workload{Actors: []Actor{&CountStoreWriter{}}, Weights: []float64{1.0}}
}

// -----------------------------------------------------------------------------
// The scenario
// -----------------------------------------------------------------------------

// CountStoreConfig parameterises one count-store run.
type CountStoreConfig struct {
	Crash      CrashConfig
	Checkpoint CheckpointConfig
	Seed       uint64
	MaxTicks   int
	// ParityEvery is the tick cadence of the full cell-by-cell parity walk.
	ParityEvery int
	// ShapesEvery is the tick cadence of the four count(*) shapes.
	ShapesEvery int
	// Persons is how many Persons the prologue creates before the loop.
	Persons int
	// Hubs is how many additional Persons carry the never-churned [csLabelHub]
	// label. They are the nodes whose IN-side count cells stay EXACT under churn,
	// so the live DIn and T clauses have something to compare.
	Hubs int
	// MinEdgesForBoundClaim, when > 0, requires the run to have modelled at least
	// this many edges at some point, so the boundedness clause's "never |E|" half
	// rests on an |E| that actually grew. The short arm leaves it 0 (its budget
	// cannot grow |E| far); the soak arm sets it.
	MinEdgesForBoundClaim int64
}

// DefaultCountStoreConfig returns the SHORT-layer configuration for seed,
// including the crash and checkpoint schedules.
//
// The schedules live HERE and not in [CountStoreConfig.normalise] on purpose. A
// normalise that re-enabled a disabled schedule would make it impossible for a
// caller to drive the forced-crash-only arm — the arm that proves the forced
// crash is not merely a fallback the catalogue seed never takes — and would
// silently override an explicit choice. normalise fills only the budgets a zero
// value leaves meaningless.
func DefaultCountStoreConfig(seed uint64) CountStoreConfig {
	cfg := CountStoreConfig{
		Seed:       seed,
		Crash:      CrashConfig{Enabled: true, CrashProb: 1.0 / 60.0, StabilityWindow: 20},
		Checkpoint: CheckpointConfig{Enabled: true, Every: 40},
	}
	cfg.normalise()
	return cfg
}

// normalise fills every unset BUDGET with its short-layer default. It never
// touches the crash or checkpoint schedule — see [DefaultCountStoreConfig].
func (c *CountStoreConfig) normalise() {
	if c.Seed == 0 {
		c.Seed = countStoreDefaultSeed
	}
	if c.MaxTicks <= 0 {
		c.MaxTicks = countStoreMaxTicks
	}
	if c.ParityEvery <= 0 {
		c.ParityEvery = countStoreParityEvery
	}
	if c.ShapesEvery <= 0 {
		c.ShapesEvery = countStoreShapesEvery
	}
	if c.Persons <= 1 {
		c.Persons = countStorePersons
	}
	if c.Hubs <= 1 {
		c.Hubs = countStoreHubs
	}
}

// countStoreSimConfig builds the simulator [Config] the run drives.
//
// The workload is supplied explicitly even though the loop never selects an
// actor from it: [New] would otherwise build [DefaultWorkload] from the master
// [Seed] instance, and passing a separate one keeps that instance untouched.
func countStoreSimConfig(cfg CountStoreConfig) Config {
	return Config{
		Seed:       cfg.Seed,
		MaxTicks:   cfg.MaxTicks,
		Workload:   countStoreWorkload(NewSeed(cfg.Seed)),
		Crash:      cfg.Crash,
		Checkpoint: cfg.Checkpoint,
	}
}

// RunCountStore drives the scenario once and returns the evidence it measured
// alongside the report of the first violation (nil when the run is clean).
//
// It owns and closes the simulator, so no durable handle or goroutine leaks past
// the run. The evidence is returned even on a violation, because what the run
// managed to exercise before failing is part of the diagnosis.
func RunCountStore(
	ctx context.Context, cfg CountStoreConfig,
) (*CountStoreEvidence, *SimReport, error) {
	cfg.normalise()
	sm, err := New(countStoreSimConfig(cfg))
	if err != nil {
		return nil, nil, fmt.Errorf("sim: count-store new: %w", err)
	}
	defer func() { _ = sm.Close() }()

	probes := NewCountStoreProbes(NewSeed(cfg.Seed ^ countStoreSeedMix))
	report, err := countStoreLoop(ctx, sm, cfg, probes)
	return probes.Evidence(), report, err
}

// countStorePrologue creates the initial population, links it into a path so the
// very first parity check has D and T cells to compare, and then CONSTRUCTS the
// negative-cell fixture.
//
// The negative cell is built rather than hoped for. With `a -> b` and both nodes
// plain Persons: `SET a:Vip` recounts a's OUT side exactly (b is Person at that
// moment) and dirties `DIn(Vip)`/`TB(Vip)`; `SET b:Vip` reaches `countRelabel`
// but returns before the OUT recount because b has no out-edge, so
// `T(Vip,KNOWS,Vip)` is never incremented; `REMOVE a:Vip` then recounts a's OUT
// side with sign -1 against b's CURRENT labels, which now include `Vip`, so
// `T(Vip,KNOWS,Vip)` lands at -1. MEASURED exactly that, covered by the
// `TB(Vip)` marking and cleared by the next reopen.
//
// It is split out of [countStoreLoop] so the falsifiability tests can build the
// identical fixture instead of a second, drifting copy of it.
func countStorePrologue(
	ctx context.Context, sm *Simulator, cfg CountStoreConfig, probes *CountStoreProbes,
) ([]Violation, error) {
	create := func(name string, age int64) error {
		op := Op{Kind: OpCreate, Cypher: tmplCreatePerson,
			Params: map[string]any{"name": name, "age": age}}
		committed, _ := sm.executeCounted(ctx, op)
		if !committed {
			return fmt.Errorf("sim: count-store prologue: CREATE %q was not committed", name)
		}
		sm.applyToOracle(op, committed)
		return nil
	}
	link := func(a, b string) error {
		op := Op{Kind: OpCreate, Cypher: tmplCreateKnows, Params: map[string]any{"a": a, "b": b}}
		committed, _ := sm.executeCounted(ctx, op)
		if !committed {
			return fmt.Errorf("sim: count-store prologue: CREATE (%s)-[:KNOWS]->(%s) was not committed", a, b)
		}
		sm.applyToOracle(op, committed)
		return nil
	}
	for i := 0; i < cfg.Persons; i++ {
		if err := create(fmt.Sprintf("cs-seed%d", i), int64(20+i)); err != nil {
			return nil, err
		}
	}
	for i := 0; i+1 < cfg.Persons; i++ {
		if err := link(fmt.Sprintf("cs-seed%d", i), fmt.Sprintf("cs-seed%d", i+1)); err != nil {
			return nil, err
		}
	}
	// The Hub pool: Persons that also carry the never-churned Hub label, chained
	// to each other AND cross-linked to the plain population, so every T cell the
	// Hub label participates in — (Hub,KNOWS,Hub), (Person,KNOWS,Hub) and
	// (Hub,KNOWS,Person) — is live from the first observation. No op ever relabels
	// or deletes a Hub node, so those cells stay EXACT for the whole run.
	hub := func(i int) string { return fmt.Sprintf("%s%d", csHubNamePrefix, i) }
	for i := 0; i < cfg.Hubs; i++ {
		op := Op{Kind: OpCreate, Cypher: tmplCreatePersonHub,
			Params: map[string]any{"name": hub(i), "age": int64(60 + i)}}
		committed, _ := sm.executeCounted(ctx, op)
		if !committed {
			return nil, fmt.Errorf("sim: count-store prologue: CREATE Hub %q was not committed", hub(i))
		}
		sm.applyToOracle(op, committed)
	}
	for i := 0; i+1 < cfg.Hubs; i++ {
		if err := link(hub(i), hub(i+1)); err != nil {
			return nil, err
		}
	}
	// One edge in each direction between the Hub pool and the plain population, so
	// the mixed T cells exist too.
	if err := link(hub(0), "cs-seed0"); err != nil {
		return nil, err
	}
	if err := link("cs-seed1", hub(1)); err != nil {
		return nil, err
	}
	// The negative-cell fixture.
	if err := countStoreArmNegativeCell(ctx, sm, probes); err != nil {
		return nil, err
	}
	// The first observation runs here, before any tick, so the constructed
	// negative cell and its dirty cover are recorded even if the very first tick
	// draws a crash.
	return probes.Parity(sm, 0, csPhaseLive, csPerturbNone), nil
}

// countStoreArmNegativeCell constructs one NEGATIVE count cell, on a fresh pair
// of nodes under the fixture's own [csLabelNeg] label.
//
// The construction, and why each step is needed: with `a -> b` and both nodes
// plain Persons, `SET a:Neg` recounts a's OUT side exactly (b carries only Person
// at that moment) and dirties `DIn(Neg)`/`TB(Neg)`; `SET b:Neg` reaches
// `countRelabel` but returns before the OUT recount because b has no out-edge, so
// `T(Neg,KNOWS,Neg)` is never incremented; `REMOVE a:Neg` then recounts a's OUT
// side with sign -1 against b's CURRENT labels, which now include `Neg`, so
// `T(Neg,KNOWS,Neg)` lands at -1. MEASURED exactly that.
//
// It is called once by the prologue and again after EVERY recovery, because a
// reopen legitimately removes the cell — that is the heal being asserted — and
// without re-arming the run would witness the removal once and then run for
// hundreds of ticks with nothing negative to classify. MEASURED before re-arming
// was added: a 1500-tick soak run reported 19 recoveries and healed exactly ONE
// negative cell.
//
// Each call uses a fresh pair (the arm counter is part of the names), so the two
// nodes a previous arm left carrying `Neg` are never re-used; they have no
// out-edge and are excluded from the link draw, so they cannot contribute a
// positive delta to the cell a later arm drives negative.
func countStoreArmNegativeCell(ctx context.Context, sm *Simulator, probes *CountStoreProbes) error {
	arm := probes.negArms
	probes.negArms++
	a := fmt.Sprintf("%s-%d-a", csNegNamePrefix, arm)
	b := fmt.Sprintf("%s-%d-b", csNegNamePrefix, arm)
	run := func(op Op, what string) error {
		committed, _ := sm.executeCounted(ctx, op)
		if !committed {
			return fmt.Errorf("sim: count-store negative-cell arm %d: %s was not committed", arm, what)
		}
		sm.applyToOracle(op, committed)
		return nil
	}
	for i, name := range []string{a, b} {
		op := Op{Kind: OpCreate, Cypher: tmplCreatePerson,
			Params: map[string]any{"name": name, "age": int64(41 + i)}}
		if err := run(op, "CREATE "+name); err != nil {
			return err
		}
	}
	if err := run(Op{Kind: OpCreate, Cypher: tmplCreateKnows,
		Params: map[string]any{"a": a, "b": b}}, "CREATE ("+a+")-[:KNOWS]->("+b+")"); err != nil {
		return err
	}
	for _, step := range []struct{ tmpl, name string }{
		{tmplAddNeg, a},
		{tmplAddNeg, b},
		{tmplRemoveNeg, a},
	} {
		op := Op{Kind: OpUpdate, Cypher: step.tmpl, Params: map[string]any{"name": step.name}}
		if err := run(op, step.tmpl+" on "+step.name); err != nil {
			return err
		}
		probes.ev.Relabels++
	}
	probes.ev.NegativeArms++
	return nil
}

// countStoreLoop is the scenario body over a simulator the caller owns.
//
// It mirrors [Simulator.Run]'s tick sequence deliberately — checkpoint, then the
// crash decision, then the op, then the periodic checks — so the run is the
// standard deterministic loop with the count-store batteries inserted, rather
// than a different harness that happens to look similar.
//
// Every branch is a distinct, documented phase; inlining them is what makes the
// ordering auditable against Simulator.Run.
//
//nolint:gocyclo // the standard tick loop plus three inserted phases, each documented above.
func countStoreLoop(
	ctx context.Context, sm *Simulator, cfg CountStoreConfig, probes *CountStoreProbes,
) (*SimReport, error) {
	if vs, err := countStorePrologue(ctx, sm, cfg, probes); err != nil {
		return nil, err
	} else if len(vs) > 0 {
		return sm.report(0, Op{Kind: OpMatch, Cypher: csOp("prologue")}, vs), nil
	}

	for i := 0; i < cfg.MaxTicks; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		tick := sm.clock.Tick()

		// Checkpoint BEFORE the crash decision, matching [Simulator.Run]: a
		// checkpoint that lands just before a crash is the realistic ordering the
		// snapshot+WAL recovery path must survive.
		if err := sm.maybeCheckpoint(tick); err != nil {
			return nil, err
		}
		crashesBefore := sm.CrashCount()
		if report, err := sm.maybeCrash(ctx, tick); err != nil {
			return nil, err
		} else if report != nil {
			return report, nil
		}
		if sm.CrashCount() != crashesBefore {
			if vs := probes.Parity(sm, tick, csPhaseRecovered, csPerturbNone); len(vs) > 0 {
				return sm.report(tick, Op{Kind: OpMatch, Cypher: csOp("post-recovery-parity")}, vs), nil
			}
			if vs := probes.Shapes(ctx, sm, tick, csPerturbNone); len(vs) > 0 {
				return sm.report(tick, Op{Kind: OpMatch, Cypher: csOp("post-recovery-shapes")}, vs), nil
			}
			// RE-ARM, after the recovered check has read the healed state: the reopen
			// legitimately removed the negative cell, so without this the run would
			// witness the removal once and then have nothing negative to classify for
			// the rest of the budget.
			if err := countStoreArmNegativeCell(ctx, sm, probes); err != nil {
				return nil, err
			}
		}

		op := sm.workload.SelectActor(sm.seed).NextOp(sm.seed, sm.oracle)
		if sm.cfg.OnOp != nil {
			sm.cfg.OnOp(tick, op)
		}
		if op.Cypher == tmplAddVip || op.Cypher == tmplRemoveVip {
			probes.ev.Relabels++
		}
		committed, counters := sm.executeCounted(ctx, op)
		if !committed {
			if op.Kind.IsWrite() {
				sm.rejectedWrites++
			} else {
				sm.rejectedReads++
			}
		}
		if vs := CheckOpCounters(tick, op, committed, counters, sm.oracle); len(vs) > 0 {
			return sm.report(tick, op, vs), nil
		}
		sm.applyToOracle(op, committed)

		if tick%int64(cfg.ParityEvery) == 0 {
			if vs := probes.Parity(sm, tick, csPhaseLive, csPerturbNone); len(vs) > 0 {
				return sm.report(tick, op, vs), nil
			}
		}
		if tick%int64(cfg.ShapesEvery) == 0 {
			if vs := probes.Shapes(ctx, sm, tick, csPerturbNone); len(vs) > 0 {
				return sm.report(tick, op, vs), nil
			}
		}
		if tick%int64(sm.cfg.CheckEvery) == 0 {
			if vs := sm.checker.Check(tick, sm.oracle, sm.engine); len(vs) > 0 {
				return sm.report(tick, op, vs), nil
			}
		}
	}

	final := int64(cfg.MaxTicks)

	// One terminal LIVE observation, so a run whose last ticks fell between
	// cadences still compares its final state — and so the forced recovery below
	// has a live observation immediately before it to claim a heal against.
	if vs := probes.Parity(sm, final, csPhaseLive, csPerturbNone); len(vs) > 0 {
		return sm.report(final, Op{Kind: OpMatch, Cypher: csOp("terminal-parity")}, vs), nil
	}
	if vs := probes.Shapes(ctx, sm, final, csPerturbNone); len(vs) > 0 {
		return sm.report(final, Op{Kind: OpMatch, Cypher: csOp("terminal-shapes")}, vs), nil
	}

	// --- a CONSTRUCTED crash. ---
	//
	// It runs UNCONDITIONALLY, not only when the seeded schedule never fired: the
	// heal clauses need a recovery whose immediately preceding live observation
	// was dirty, and only a forced crash placed right after the terminal live
	// observation guarantees that ordering. A run that already crashed mid-loop
	// keeps those earlier recovered observations too.
	if report, err := sm.forceCrash(final, csOp("forced-crash-recovery")); err != nil {
		return nil, err
	} else if report != nil {
		return report, nil
	}
	probes.ev.ForcedCrashes++
	if vs := probes.Parity(sm, final, csPhaseRecovered, csPerturbNone); len(vs) > 0 {
		return sm.report(final, Op{Kind: OpMatch, Cypher: csOp("forced-recovery-parity")}, vs), nil
	}
	if vs := probes.Shapes(ctx, sm, final, csPerturbNone); len(vs) > 0 {
		return sm.report(final, Op{Kind: OpMatch, Cypher: csOp("forced-recovery-shapes")}, vs), nil
	}

	// --- the non-vacuity gates. ---
	probes.ev.Crashes = sm.CrashCount()
	probes.ev.Checkpoints = sm.CheckpointCount()
	if cfg.MinEdgesForBoundClaim > 0 && probes.ev.MaxEdges < cfg.MinEdgesForBoundClaim {
		return sm.report(final, Op{Kind: OpMatch, Cypher: csOp("bound-vacuity")}, []Violation{
			csViolation(ViolationVacuousRun, final, "vacuity:bound-edges",
				"the run modelled at most %d edge(s), below the %d this arm requires. The boundedness "+
					"claim is that Cells() is a function of SCHEMA cardinality and never of |E|, so it "+
					"means nothing until |E| has actually grown well past the ceiling (cells=%d bound=%d)",
				probes.ev.MaxEdges, cfg.MinEdgesForBoundClaim, probes.ev.MaxCells, probes.ev.MaxBound),
		}), nil
	}
	if vs := probes.ev.Finish(final); len(vs) > 0 {
		return sm.report(final, Op{Kind: OpMatch, Cypher: csOp("vacuity")}, vs), nil
	}
	// A CheckpointConfig is INERT unless the loop calls maybeCheckpoint, so this
	// gate is what stops the scenario claiming a snapshot-crossing recovery it
	// never produced (rmp #2457/#2464).
	if vs := sm.checkCheckpointsFired(final); len(vs) > 0 {
		return sm.report(final, Op{Kind: OpMatch, Cypher: csOp("checkpoint-vacuity")}, vs), nil
	}
	return nil, nil
}

// countStoreScenario is the catalogue entry (rmp #2494).
//
// It carries a custom run override rather than using the standard deterministic
// dispatch because the parity check must run IMMEDIATELY after each recovery —
// while the recovered store is still untouched by any subsequent write — which no
// [CheckSelection] check can express, and because the forced crash at the end has
// to be ordered right after a live observation for the heal clauses to be
// meaningful.
func countStoreScenario() Scenario {
	return Scenario{
		Name: ScenarioCountStore,
		Description: "the derived relationship count-store adjudicated cell by cell against the shadow " +
			"model — E/DOut/DIn/T in both directions, dirty-covered cells skipped while live and " +
			"NOTHING skipped after a reopen, so the RecomputeReset self-heal is asserted rather than " +
			"merely executed; a constructed negative cell must be dirty-covered and must be gone after " +
			"recovery; Cells() held to the combinatorial ceiling of the observed vocabulary; and six " +
			"count(*) pattern shapes checked three ways — query rows, model, and the serving " +
			"count-store cell",
		Mode:        ModeDeterministic,
		DefaultSeed: countStoreDefaultSeed,
		MaxTicks:    countStoreMaxTicks,
		Crash:       CrashConfig{Enabled: true, CrashProb: 1.0 / 60.0, StabilityWindow: 20},
		Checkpoint:  CheckpointConfig{Enabled: true, Every: 40},
		run:         runCountStoreScenario,
	}
}

// runCountStoreScenario is the scenario's run override: drive the loop, then
// attach the measured evidence to whatever report came back so an operator
// reading only the log sees what the run exercised.
func runCountStoreScenario(ctx context.Context, seed uint64) (*SimReport, error) {
	ev, report, err := RunCountStore(ctx, DefaultCountStoreConfig(seed))
	if err != nil {
		return nil, err
	}
	if report == nil {
		return nil, nil
	}
	report.Scenario = ScenarioCountStore
	report.Mode = ModeDeterministic
	report.FailedOp.Cypher = report.FailedOp.Cypher + " " + ev.String()
	return report, nil
}
