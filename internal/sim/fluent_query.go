package sim

// fluent_query.go — rmp #2492: the DST scenario for graph/query, the fluent
// pattern-query engine, driven as a SECOND read path over the same
// [lpg.Graph] as the Cypher engine.
//
// # What this scenario adjudicates
//
// graph/query is an independent reader of the same substrate the Cypher engine
// reads. It has its own working-set representation (a roaring64 bitmap of
// NodeIDs), its own label seeding ([lpg.Graph.NodeIndex] intersect, or a
// Mapper.Walk when no label is constrained), its own tombstone pruning
// (query.pruneTombstones), and its own index-seek decision logic
// (query/index_seek.go). None of that is shared with the Cypher planner, so the
// two engines can disagree — and until this scenario, nothing in internal/sim
// imported graph/query at all, so a disagreement could only be found by the
// package's own in-package tests over hand-built fixtures.
//
// # Why the oracle is the arbiter, and why that differs from differential.go
//
// This package already has a differential facility — [DifferentialTrace] in
// differential.go — and it deliberately validates the engine AGAINST ITSELF:
// [DefaultVariantPair] compares the default planner with the same planner with
// the disconnected-equi-join hash join disabled. That stance is sound there,
// because the engine GUARANTEES the two variants are result-equivalent
// (EngineOptions.DisableHashJoin exists for exactly that proof), so any
// divergence is by construction a regression and it does not matter which side
// is "right": both sides must be the same side.
//
// This scenario cannot borrow that stance. graph/query and cypher are two
// INDEPENDENT implementations with no equivalence guarantee between them, so:
//
//   - "they agree" is a WEAK claim — two independent readers can be wrong the
//     same way (both read the same label index, both read the same property
//     store), and agreement between them says nothing about the answer;
//   - "they disagree" would be UNATTRIBUTABLE — it names a divergence but not a
//     culprit.
//
// So the arbiter here is a MODEL: [GraphOracle], the harness's
// correct-by-construction shadow of what the graph must contain, computed from
// the operation stream alone and touching neither engine. Every probe therefore
// carries three separable clauses, so a red run names which path is wrong:
//
//	fluent-vs-oracle    the fluent engine's answer against the model
//	cypher-vs-oracle    the Cypher engine's answer against the model
//	fluent-vs-cypher    the two engines against each other
//
// The third is not redundant. It is the clause that fires when both engines
// deviate from the model in DIFFERENT directions, which the first two also
// catch, and — more usefully — it is the clause whose silence, alongside two
// red oracle clauses, says "both engines agree and the MODEL is wrong", i.e.
// look at the harness first.
//
// # What the differential is actually over, and what it is NOT over
//
// Both engines read the same [lpg.Graph]: the same Mapper, the same label
// bitmaps, the same property shards. What differs — and therefore what this
// scenario tests — is the WORKING-SET logic layered on that substrate: which
// candidate ids each engine seeds, which it prunes, which predicate it routes
// through an index, and how it expands a hop. A defect in the shared substrate
// (a property store that loses a value) moves BOTH engines together and shows up
// here as two red oracle clauses with a silent fluent-vs-cypher clause, which is
// the correct attribution: it is not a fluent-vs-Cypher divergence.
//
// The scenario adds one more independent channel that is neither engine: a
// SUBSTRATE view built by walking [graph.Mapper.Walk] directly, skipping
// [lpg.Graph.IsTombstoned] ids, and reading each survivor's `name` property.
// Its live-name set is held to the model's before any probe runs
// ("precondition:substrate-parity"), so a probe failure downstream cannot be
// explained away by a substrate that had already diverged.
//
// # Which CSR generation the engine is handed — and why the answer is MEASURED
//
// [query.New] takes BOTH an [lpg.Graph] and a [csr.CSR], so the scenario must
// choose a CSR generation and defend it. It builds a FRESH CSR at every probe
// point, on the single simulation goroutine, at a quiescent instant between two
// ticks — so the topology the fluent engine expands over and the state the model
// describes are the SAME instant, and a fluent-vs-oracle divergence is
// attributable to the engine rather than to snapshot staleness.
//
// It builds TWO of them, and asserts they are interchangeable:
//
//   - cLive = [csr.BuildFromAdjListLive] with [lpg.Graph.LiveNodeFilter] — the
//     live-filtered build (#1790), which omits every arc with a tombstoned
//     endpoint;
//   - cRaw  = [csr.BuildFromAdjList] — the tombstone-AGNOSTIC build, which
//     keeps those arcs as ghosts.
//
// The clause "csr-generation-invariance" requires [query.Pattern.Out] to return
// the IDENTICAL answer over both. That is not a coincidence to be hoped for, it
// is a theorem of the two prunes acting together: an arc whose SOURCE is
// tombstoned cannot contribute because the seed step already dropped that source
// from the working set, and an arc whose TARGET is tombstoned is dropped by
// Out's own pruneTombstones. Between them they remove exactly the arcs the live
// filter removes. If either prune regresses, the two builds diverge and this
// clause fires — which makes it a detector, not a tautology.
//
// The CSR is read by NOTHING except [query.Pattern.Out] (verified in
// graph/query/query.go: Vertex, filterByPreds, seekIndexablePreds, Cardinality,
// Collect and NodeIDs never touch Engine.csr), so the invariance clause is
// scoped to the Out probes and the label/property/range probes are provably
// CSR-independent.
//
// # The tombstone attack, and why its precondition is CONSTRUCTED not hoped for
//
// query.pruneTombstones is the only thing standing between the fluent engine and
// a deleted node, and query.seedFromPreds has TWO seeding paths that both depend
// on it. They differ in one respect that decides where the load-bearing gate can
// soundly go, and the difference was MEASURED rather than assumed.
//
// The LABEL-SEEDED path (`Vertex(WithLabel(...))`) intersects
// [lpg.Graph.NodeIndex]'s bitmaps. A deleted node's label-bitmap entry is NOT
// removed synchronously while MVCC is armed: lpg's stripLabelBitmaps /
// RemoveNodeLabel DEFER the removal (graph/lpg/mvcc_index.go), and it is applied
// later by the BACKGROUND VACUUM's applyDeferredIndexRemovals once the
// reclamation watermark passes it. So whether a corpse is still advertised by
// the label index at a given instant is a function of WHEN A GOROUTINE WOKE.
// MEASURED: two runs of the SAME seed in the SAME process observed 3 and 2
// label-advertised corpses at the same tick. That count is therefore recorded as
// TELEMETRY and is deliberately NOT gated and NOT compared across runs — a
// non-vacuity gate on a scheduler-dependent count is the flake this sprint spent
// two tasks (#2587, #2596) removing from other scenarios.
//
// The NO-PREDICATE path (`Vertex()` with no predicates) seeds from
// query.seedAllLive, which walks [graph.Mapper.Walk] and then prunes. The Mapper
// NEVER forgets a slot — NodeID stability is a hard contract, restated in
// lpg.RemoveNode's own godoc — so every id ever interned is still yielded by the
// walk and every tombstoned one MUST be removed by the prune, for as long as the
// run lasts. That makes the load-bearing claim DETERMINISTIC, and it is where
// the gate lives: [FluentQueryProbes.Finish] fails the run when no battery ever
// observed a positive [lpg.Graph.TombstoneCount], and the "all-live" probe is
// the one whose answer the prune has to earn. The tombstone count's determinism
// (measured above: identical at every tick across both runs) is what makes it a
// sound gate where the label-index count is not.
//
// The gate is also CONSTRUCTED rather than hoped for: the prologue commits one
// delete-then-recreate cycle before the FIRST battery, so the tombstone set is
// non-empty from tick 0 and the gate never fails a run merely because the
// workload's draws happened not to include a DELETE. The same discipline
// supplies the post-recovery coverage — when the seeded crash schedule never
// fires inside the budget, [fluentQueryForceCrash] performs one crash+recovery
// cycle at the end and the battery runs on the recovered graph — so neither gate
// can fail a run whose precondition was never constructed.
//
// The DETECTOR for a prune regression is the "unknown-id" clause, not any
// name-set comparison, and the distinction is load-bearing rather than
// stylistic. A corpse is UNNAMED in the substrate view by construction
// ([newFluentQuerySubstrate] skips every [lpg.Graph.IsTombstoned] id), so a
// corpse that leaks into a working set contributes NO name and cannot change a
// name set. MEASURED, by reproducing the broken output: with the prune omitted,
// "fluent-vs-oracle" stays silent and "unknown-id" fires alone. The name-set
// clauses are therefore the detectors for a working set that gained or lost a
// LIVE node, and the identity clause is the detector for a corpse; neither
// substitutes for the other, and
// [TestFluentQuery_ClausesAreFalsifiable] pins which one fires for which
// defect.
//
// A dedicated churn phase makes the corpses pile up deterministically:
// every [fluentQueryChurnEvery] ticks it DETACH DELETEs a live Person and
// re-CREATEs the same NAME, through the same modelled templates the oracle
// understands. Note precisely what that does and does not exercise: the Cypher
// CREATE mints a FRESH synthetic node key (`__cx_<hex>`), so the re-created
// Person is a NEW NodeID and the old one stays tombstoned for the rest of the
// run. This churn therefore drives pruneTombstones against a GROWING tombstone
// set; it does NOT drive [lpg.Graph.AddNode]'s resurrection path, which needs
// the same mapper KEY and is unreachable through Cypher CREATE. Claiming
// resurrection coverage here would be false, so it is not claimed.
//
// # Out's prune needs a CONSTRUCTED fixture — the live graph cannot reach it
//
// MEASURED on this tree: `DETACH DELETE` strips the deleted node's incident
// edges from the adjacency, so cRaw and cLive have the SAME arc count on the
// live graph and the ghost-arc branch of Out's prune is never reached there.
// Rather than let the invariance clause pass vacuously, the battery also runs
// [fluentQueryGhostFixture]: a small side [lpg.Graph] built with the plain Go
// API, where [lpg.Graph.RemoveNode] tombstones a node WITHOUT stripping its arcs
// — the one documented way to produce a ghost arc. The fixture asserts its own
// precondition (cRaw really does carry an arc into a tombstoned target, and
// cRaw.Size() > cLive.Size()) before asserting the answer, so it cannot pass by
// being empty.
//
// # Index seeks: which arms are REACHABLE, established by reading the guard
//
// query/index_seek.go serves a predicate from an index only when every condition
// of trySeekProperty/trySeekRange plus indexCovers holds. Those conditions are
// finitely enumerable and every one of them is observable from outside, so this
// scenario asserts them ("precondition:seek-eligibility") instead of hoping:
// a non-nil [lpg.Graph.IndexManager]; a constrained label (the probe passes
// [query.WithLabel] in the SAME Vertex call); an index whose Kind() is "hash"
// (equality) or "btree" (range); a BoundNode() of exactly (label, property); and
// a concrete index satisfying the TYPED read interface for the bound value's
// kind. When all hold, the seek is taken. The engine's own in-package spy
// (graph/query/index_seek_spy_test.go) is where the path is OBSERVED; from
// another package the guard enumeration is the available proof, and it is stated
// as such rather than dressed up as observation.
//
// The scan path is reached by the same predicate in a SECOND Vertex call:
// labelsInPreds of a predicate list holding no [query.WithLabel] is empty, and
// both trySeekProperty and trySeekRange return false immediately on an empty
// label list. So `Vertex(label, pred)` is the SEEK arm and
// `Vertex(label).Vertex(pred)` is the SCAN arm, separated by construction with
// no DDL churn and no instrumentation, and "seek-vs-scan" is a real clause over
// two genuinely different code paths.
//
// MEASURED on this tree, the reachable seek arms against engine-created indexes
// are exactly these:
//
//   - EQUALITY on a string property is hash-served: `CREATE INDEX ...
//     indexType:'hash'` builds a hash.Index[string], which satisfies
//     hashLookuper[string].
//   - EQUALITY on a NUMERIC property is btree-served by the same numeric
//     companion the ranges use, seeked as the DEGENERATE range [v, v], as a
//     SUPERSET with query.equalValue as the exact residual filter (rmp #2601).
//     It is NOT hash-served: a hash.Index[int64] or hash.Index[float64] holds
//     one numeric kind only, which under a unified equality is a SUBSET of the
//     answer, and a subset cannot be repaired by a residual filter — so #2601
//     removed those arms exactly as #2600 removed btreeRanger[int64]. No
//     engine-created hash index is numeric anyway (CREATE INDEX always builds a
//     hash.Index[string]), so nothing an engine builds lost a seek.
//   - A STRING RANGE on a string property is btree-served: `CREATE INDEX ...
//     indexType:'btree'` builds a btree.Index[string], which satisfies
//     btreeRanger[string].
//   - A NUMERIC RANGE — INT64 bounds, FLOAT64 bounds, or one of each — is
//     btree-served by the internal numeric companion
//     "<label>_<prop>_btree_num", a btree.Index[float64] the engine registers
//     alongside the user-named string btree (cypher/index_binding.go). Since
//     rmp #2600 query.seekRangeInto routes EVERY numeric bound pair to that
//     index and treats its answer as a SUPERSET rather than as the answer,
//     because its int64 keys are widened to float64 and round above 2^53;
//     query.valueInRange then runs over what the seek left as the exact
//     residual filter. Both the int64-range and the mixed-kind probes below
//     therefore adjudicate a real seek.
//     Before #2600 seekRangeInto asserted a btreeRanger[int64] that no
//     engine-created index satisfies and had no float64 route for int64 bounds,
//     so an [lpg.Int64Value]-bounded [query.WithRange] was never served at all.
//
// # The mixed-kind divergence this scenario found, and now asserts
//
// The measurement that built this scenario exposed a real divergence, recorded
// as rmp #2600 and closed by it. With FLOAT64 bounds over an INT64-valued
// property:
//
//   - the SEEK arm was served by the numeric companion btree, which indexes
//     PropInt64 and PropFloat64 under one float64 order
//     (cypher/index_binding.go: projectNumericPropValue), and returned the
//     numeric matches — the same answer the Cypher engine and the model give;
//   - the SCAN arm returned NOTHING, because query.valueInRange required v, lo
//     and hi to be the SAME PropertyValue kind.
//
// The two paths disagreed, contradicting index_seek.go's own claim that they
// cannot. The semantics were then settled from the primary sources — openCypher
// orders INTEGER and FLOAT in ONE numeric order, the sole off-diagonal entry of
// the comparability matrix in the normative CIP "Comparability and
// Orderability", pinned by the TCK in expressions/comparison/Comparison2.feature
// ("Comparing across types yields null, except numbers") and
// Comparison1.feature ("1 = 1.0" is true) — so the SCAN was the defective side.
//
// #2600 unified the comparison, made the numeric seek a superset with
// valueInRange as its exact residual filter, and this probe was promoted from
// telemetry to the asserted "range-mixed" clause: the mixed-kind window is now
// adjudicated three ways like every other probe, seek against scan included.
// While the semantics were still open the probe deliberately asserted nothing,
// so that pinning the old behaviour could not make the eventual fix look like a
// regression.
//
// TWO windows are driven under that clause, because #2600 had two halves:
//
//	range-mixed-point    FLOAT64 bounds over an INT64 property — the divergence
//	                     above, where the seek was right and the scan empty.
//	range-mixed-bounds   bounds of DIFFERENT kinds ([age, age+0.5]). This shape
//	                     used to be refused outright by query.trySeekRange,
//	                     which bailed whenever lo.Kind() != hi.Kind(), so it was
//	                     consistently wrong rather than divergent. The CIP makes
//	                     the two bound tests independent, so the bounds need not
//	                     share a kind.
//
// # The asymmetry #2600 CREATED, and #2601 closed
//
// Unifying the range and leaving the equality alone made the two disagree with
// each other. Over the same INT64-valued `age`:
//
//	WithRange("age", Float64Value(age), Float64Value(age))   matched
//	WithProperty("age", Float64Value(age))                   did NOT
//
// so the same data answered differently depending on whether the predicate was
// written as an equality or as a degenerate range. Before #2600 both refused, so
// they were at least coherent. The CIP that settles the order also says numbers
// of different types can be EQUAL, and the TCK has 1 = 1.0 as true
// (Comparison1.feature), so the equality was the side left wrong.
//
// #2601 unified query.equalValue through the SAME exact comparator the range
// uses, and this scenario gained a third window plus a clause that names the
// asymmetry directly:
//
//	eq-mixed-point       a FLOAT64 expected value over an INT64 property,
//	                     adjudicated three ways like every other probe under the
//	                     "eq-mixed" clause. Its seek arm is the only equality
//	                     probe here that reaches the numeric companion btree.
//
//	eq-mixed:equality-vs-degenerate-range
//	                     the equality answer held DIRECTLY against
//	                     WithRange(age, age) over the same value. The three-way
//	                     adjudication of the two probes already implies it, but
//	                     only while both have a non-empty answer; asserting it
//	                     directly makes the #2601 clause independent of that and
//	                     names the asymmetry instead of leaving it to be inferred
//	                     from two separate probe failures.
//
// The identity is deliberately NOT claimed for every kind. openCypher's
// equatability is WIDER than its comparability: BOOLEAN, BYTES and TIME are
// equal to themselves but are not ordered scalars, so over them WithProperty
// matches where the degenerate WithRange cannot. `age` is numeric, so this
// scenario only ever exercises the orderable case; the divergence for the other
// kinds is pinned in graph/query's own tests, not here.
//
// # Determinism: exactly what is reproducible
//
// [ExecMode.Reproducible] is true for this scenario and the report pins
// [FluentQueryEvidence.Digest]. The digest deliberately folds only MODEL and
// COUNT quantities — tick, clause id, and the oracle/fluent/Cypher cardinalities
// of every probe — and never a NodeID or a mapper key. That is not tidiness: the
// Cypher engine mints node keys from a PROCESS-GLOBAL counter
// (cypher/exec/create_node.go globalNodeCounter), so `__cx_<hex>` keys and the
// NodeIDs interned for them are not a function of the seed and would make any
// digest that folded them irreproducible across runs in the same process.
//
// Every draw the probes and the churn phase make comes from its own sub-seed
// ([fluentQueryProbeSeedMix], [fluentQueryChurnSeedMix],
// [fluentQueryPrologueSeedMix]), so the workload's actor/op/param stream stays
// byte-identical to what the same seed would produce without them — the
// convention every other custom-loop scenario in this package follows.
//
// # What this scenario CANNOT detect — read before assuming coverage
//
//   - RESURRECTION. As set out above, Cypher CREATE cannot reuse a mapper key,
//     so [lpg.Graph.AddNode]'s revive path is not reached. The churn phase
//     attacks pruneTombstones against a growing tombstone set, which is a
//     different thing.
//   - CONCURRENT use of graph/query. The package documents an [query.Engine] as
//     safe for concurrent use only while the graph and CSR are quiescent, and a
//     [query.Pattern] as owned by one goroutine. This scenario runs on the
//     single deterministic simulation goroutine and therefore proves nothing
//     about a concurrent reader. A concurrent arm would need the quiescence
//     contract relaxed first, which is a design question, not a test.
//   - MULTI-HOP chains. Only a single [query.Pattern.Out] hop is probed, because
//     the Cypher equivalent of a second hop over an unlabelled CSR expansion
//     needs a relationship-type-free pattern that the workload's single edge
//     type makes indistinguishable from the one-hop case. The one-hop
//     equivalence itself rests on an ASSERTED precondition
//     ("precondition:model-shape"): every modelled edge is :KNOWS and every
//     modelled node is :Person, so Out() over a type-blind CSR equals
//     -[:KNOWS]->. If a future workload adds a second edge type that
//     precondition fires rather than the comparison silently going wrong.
//   - WHICH index served a seek, observed rather than deduced. See the guard
//     enumeration above.
//   - WHICH index served a numeric range seek. The numeric companion is
//     internal (db.indexes() filters its name suffix), so the eligibility
//     enumeration below is again the available proof, not an observation.

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/graph/query"

	"github.com/RoaringBitmap/roaring/v2/roaring64"
)

// ScenarioFluentQuery is the catalogue key of the fluent-engine differential
// scenario (rmp #2492).
const ScenarioFluentQuery = "fluent-query"

// fluentQueryDefaultSeed is the catalogue seed. Like every other scenario's it
// is arbitrary; what matters is that the run is a pure function of it.
const fluentQueryDefaultSeed uint64 = 0x2492_F10E_7C05

// The sub-seed mixes. Each probe/churn/prologue stream draws from its own
// derived [Seed] so the workload's op stream stays byte-identical to a run
// without them — the convention runIndexDiversity established.
const (
	fluentQueryProbeSeedMix    uint64 = 0x2492_9B0B_E5EE_D1A1
	fluentQueryChurnSeedMix    uint64 = 0x2492_C801_5EED_2B2B
	fluentQueryPrologueSeedMix uint64 = 0x2492_9401_0607_3C3C
)

// The scenario's budgets. They are the SHORT-layer defaults: small enough that
// the battery (a Mapper walk, two CSR builds and a dozen queries per call)
// stays a small share of the package's 60s ceiling. The soak arm raises them.
const (
	// fluentQueryMaxTicks bounds the deterministic loop.
	fluentQueryMaxTicks = 320
	// fluentQueryPrologueNodes is how many Persons (and the KNOWS chain over
	// them) the prologue creates through the modelled templates before the loop
	// starts. It exists so the FIRST battery already has a non-empty label
	// answer and a non-empty one-hop answer, which is what makes the
	// corresponding non-vacuity gates STRUCTURAL rather than a hope about the
	// workload's draws.
	fluentQueryPrologueNodes = 40
	// fluentQueryBatteryEvery is the in-loop probe cadence in ticks. The battery
	// also runs once before the loop, after every crash recovery, and once at
	// the end.
	fluentQueryBatteryEvery = 40
	// fluentQueryChurnEvery is the delete-then-recreate cadence in ticks.
	fluentQueryChurnEvery = 25
)

// fluentQueryLabel is the single node label the workload writes, and
// fluentQueryEdgeLabel the single relationship type. Both are ASSERTED against
// the model on every battery ("precondition:model-shape"), because the
// Out()-vs-`-[:KNOWS]->` equivalence depends on them.
const (
	fluentQueryLabel     = "Person"
	fluentQueryEdgeLabel = "KNOWS"
)

// fluentQueryDDL creates the indexes the seek arms need. Two indexes on
// (Person, name) coexist deliberately: query.trySeekProperty only considers an
// index whose Kind() is "hash" and query.trySeekRange only one whose Kind() is
// "btree", so the hash serves the equality seek and the btree serves the string
// range seek over the SAME well-populated property. The (Person, age) btree is
// created for the NUMERIC range probes: it is what causes the engine to register
// the internal numeric companion btree.Index[float64] that serves every numeric
// bound pair — int64, float64, or one of each (see the file header).
var fluentQueryDDL = []string{
	"CREATE INDEX fq_person_name_hash FOR (n:Person) ON (n.name) OPTIONS {indexType:'hash'}",
	"CREATE INDEX fq_person_name_btree FOR (n:Person) ON (n.name) OPTIONS {indexType:'btree'}",
	"CREATE INDEX fq_person_age_btree FOR (n:Person) ON (n.age) OPTIONS {indexType:'btree'}",
}

// fluentQueryAbsentName is a name no template can ever produce: every workload
// name is either a bare capitalised first name or "<FirstName>-<u32>", and every
// prologue name is "fq-p<i>". It is used two ways, and both are structural
// rather than probabilistic:
//
//   - as the equality probe's guaranteed MISS;
//   - as a string-range window that is guaranteed EMPTY, because '~' (0x7E)
//     sorts above every byte any generated name starts with.
const fluentQueryAbsentName = "~fq-absent"

// The int64 range window that is guaranteed EMPTY: HonestWriter binds ages from
// seed.IntN(100), so no Person can carry an age in this interval. Pairing it
// with a guaranteed-non-empty window is what makes the "both directions were
// exercised" gate structural.
const (
	fluentQueryEmptyAgeLo int64 = 1000
	fluentQueryEmptyAgeHi int64 = 2000
)

// fqPredicate and fqPattern bind graph/query's type parameters to the
// simulator's node/weight types once, so the probe bodies read as pattern
// expressions rather than as instantiations.
type (
	fqPredicate = query.Predicate[string, float64]
	fqPattern   = query.Pattern[string, float64]
)

// fqLabel, fqProp and fqRange are the three predicate constructors this
// scenario uses, pre-instantiated.
func fqLabel(name string) fqPredicate { return query.WithLabel[string, float64](name) }
func fqProp(key string, v lpg.PropertyValue) fqPredicate {
	return query.WithProperty[string, float64](key, v)
}
func fqRange(key string, lo, hi lpg.PropertyValue) fqPredicate {
	return query.WithRange[string, float64](key, lo, hi)
}

// fqPerturb names a TEST-ONLY perturbation of one OBSERVED side of one clause,
// so a test can prove the clause fires rather than merely that it is silent.
//
// It is a PARAMETER, threaded from the caller down to the observation site, and
// never a package-level variable: that shape is a data race by construction and
// internal/sim/global_state_guard_test.go fails the build if it reappears
// (rmp #2597).
//
// The two prune perturbations do not fake a mismatch — they reproduce the
// OUTPUT a broken prune would produce, by recomputing the answer with the prune
// omitted. So a test using them proves the clause catches the real defect, not
// merely that two unequal sets compare unequal.
type fqPerturb uint8

// The perturbations. fqPerturbNone is what the scenario always passes.
const (
	fqPerturbNone fqPerturb = iota
	// fqPerturbSeedPruneDisabled replaces the no-predicate seed answer with every
	// id [graph.Mapper.Walk] yields — exactly what query.seedAllLive would return
	// if pruneTombstones stopped removing tombstoned ids. It is applied to the
	// no-predicate path rather than the label-seeded one because the Mapper never
	// forgets a slot, which makes the reproduction deterministic; the label
	// bitmap's corpses are swept by the background vacuum and would make it a
	// race (see the file header).
	fqPerturbSeedPruneDisabled
	// fqPerturbOutPruneDisabled replaces the one-hop answer with the unpruned
	// out-neighbourhood of the live sources over the RAW CSR — exactly what
	// query.Pattern.Out would return if its pruneTombstones call were a no-op.
	// It applies ONLY inside [fluentQueryGhostFixture]: on the live graph DETACH
	// DELETE strips arcs, so there the reproduction of "the prune did nothing"
	// reproduces the CORRECT answer and would be an inert perturbation.
	fqPerturbOutPruneDisabled
	// fqPerturbCypherDropRow drops one row from the Cypher label answer, so the
	// cypher-vs-oracle clause is proven to fire independently of the fluent one.
	fqPerturbCypherDropRow
	// fqPerturbScanArmDrop drops one name from the equality SCAN arm's answer,
	// so the seek-vs-scan clause is proven to fire.
	fqPerturbScanArmDrop
	// fqPerturbFluentDropName drops one name from the fluent SEEK arm's answer,
	// so the fluent-vs-oracle and fluent-vs-cypher clauses are proven to fire on
	// the FLUENT side. It exists because the two prune perturbations are caught
	// by the identity clause rather than by the name-set clauses (a corpse has no
	// name, so it cannot change a name set), and a clause that is never shown to
	// fire is a clause whose silence means nothing.
	fqPerturbFluentDropName
	// fqPerturbRawArmDrop drops one name from the RAW-CSR observation, so the
	// csr-generation-invariance clause is proven to fire — including inside the
	// ghost fixture, which is the only place the two CSR builds actually differ.
	fqPerturbRawArmDrop
	// fqPerturbDegenerateRangeDrop drops one name from the DEGENERATE-RANGE
	// observation the eq-mixed probe holds its equality answer against, so the
	// "equality-vs-degenerate-range" clause rmp #2601 added is proven to fire.
	//
	// It perturbs that arm rather than the equality arm because the equality arm
	// is already covered from two directions — the three-way model comparison and
	// fqPerturbScanArmDrop — while the degenerate-range arm is read nowhere else
	// and its silence would otherwise mean nothing.
	fqPerturbDegenerateRangeDrop
)

// fqPerson is the model's record of one live Person: its name is the map key,
// and age is optional because [tmplMergePerson] creates a Person with no age.
type fqPerson struct {
	age    int64
	hasAge bool
}

// fluentQueryModel is the MODEL view of the graph, computed from [GraphOracle]
// alone. It touches neither engine, which is what makes it the arbiter.
//
// shapeFindings carries the "precondition:model-shape" and
// "precondition:name-uniqueness" problems found while building it, so a model
// the probes cannot soundly adjudicate against fails loudly instead of
// producing a wrong expected answer.
type fluentQueryModel struct {
	persons map[string]fqPerson
	// knows is the live one-hop adjacency by NAME. Only edges whose BOTH
	// endpoints are live nodes are included, which is exactly what the oracle's
	// DETACH DELETE leaves behind.
	knows map[string]map[string]struct{}
	// sorted is every live name in ascending order, so a string-range window can
	// be drawn from real data rather than from a guess about the name space.
	sorted        []string
	shapeFindings []string
}

// newFluentQueryModel projects the oracle onto the view the probes need.
func newFluentQueryModel(o *GraphOracle) *fluentQueryModel {
	m := &fluentQueryModel{
		persons: make(map[string]fqPerson, o.NodeCount()),
		knows:   make(map[string]map[string]struct{}),
	}
	byID := make(map[uint64]string, o.NodeCount())
	for id, n := range o.nodes {
		if !slices.Equal(n.Labels, []string{fluentQueryLabel}) {
			m.shapeFindings = append(m.shapeFindings,
				fmt.Sprintf("modelled node %d carries labels %v, not exactly [%s]: Out() over a "+
					"type-blind CSR is no longer equivalent to MATCH (:%s)-[:%s]->()",
					id, n.Labels, fluentQueryLabel, fluentQueryLabel, fluentQueryEdgeLabel))
			continue
		}
		name, ok := n.Properties["name"].(string)
		if !ok {
			m.shapeFindings = append(m.shapeFindings,
				fmt.Sprintf("modelled node %d has no string name property (%v)", id, n.Properties["name"]))
			continue
		}
		if _, dup := m.persons[name]; dup {
			// The workload binds names uniquely (a bare first name from MERGE, or
			// "<FirstName>-<u32>" from CREATE) and the oracle keys MERGE on the
			// name, so a duplicate LIVE name would break the name-keyed
			// comparison every probe below rests on. Report it rather than
			// silently collapsing two nodes into one expected answer.
			m.shapeFindings = append(m.shapeFindings,
				fmt.Sprintf("modelled name %q is carried by more than one live node: the probes' "+
					"name-keyed comparison is unsound", name))
			continue
		}
		p := fqPerson{}
		if age, ok := n.Properties["age"].(int64); ok {
			p.age, p.hasAge = age, true
		}
		m.persons[name] = p
		byID[id] = name
	}
	for k, e := range o.edges {
		if e.Label != fluentQueryEdgeLabel {
			m.shapeFindings = append(m.shapeFindings,
				fmt.Sprintf("modelled edge %v carries type %q, not %q: Out() over a type-blind CSR "+
					"is no longer equivalent to -[:%s]->", k, e.Label, fluentQueryEdgeLabel, fluentQueryEdgeLabel))
			continue
		}
		src, srcOK := byID[e.SrcID]
		dst, dstOK := byID[e.DstID]
		if !srcOK || !dstOK {
			// An edge to a node the model no longer holds. ApplyDelete removes
			// incident edges with the node, so this cannot happen; if it does, the
			// model is inconsistent and every one-hop expectation is wrong.
			m.shapeFindings = append(m.shapeFindings,
				fmt.Sprintf("modelled edge %v has an endpoint that is not a live modelled node", k))
			continue
		}
		if m.knows[src] == nil {
			m.knows[src] = make(map[string]struct{})
		}
		m.knows[src][dst] = struct{}{}
	}
	m.sorted = make([]string, 0, len(m.persons))
	for name := range m.persons {
		m.sorted = append(m.sorted, name)
	}
	sort.Strings(m.sorted)
	return m
}

// liveNames returns the model's live name set as the probes compare it.
func (m *fluentQueryModel) liveNames() map[string]struct{} {
	out := make(map[string]struct{}, len(m.persons))
	for name := range m.persons {
		out[name] = struct{}{}
	}
	return out
}

// outAll is the model's one-hop out-neighbourhood of EVERY live Person: the
// union of knows over every source. It is the expected answer for
// Vertex(label).Out().
func (m *fluentQueryModel) outAll() map[string]struct{} {
	out := make(map[string]struct{})
	for _, dsts := range m.knows {
		for d := range dsts {
			out[d] = struct{}{}
		}
	}
	return out
}

// outOf is the model's one-hop out-neighbourhood of a single named Person.
func (m *fluentQueryModel) outOf(name string) map[string]struct{} {
	out := make(map[string]struct{}, len(m.knows[name]))
	for d := range m.knows[name] {
		out[d] = struct{}{}
	}
	return out
}

// firstAgedName returns the lexicographically-first live Person that carries an
// age, and ok=false when none does. It is what the int64-range and mixed-kind
// probes draw their window from, INSTEAD of the seeded pick: the seeded pick may
// land on a MERGE-created Person, which carries no age, and then the
// guaranteed-non-empty int window would silently not run. Using a deterministic
// aged Person makes "the int range probe exercised both directions" a fact of
// the construction rather than a property of the draw.
func (m *fluentQueryModel) firstAgedName() (string, bool) {
	for _, name := range m.sorted {
		if m.persons[name].hasAge {
			return name, true
		}
	}
	return "", false
}

// stringRange is the model's answer for a [query.WithRange] over `name`.
func (m *fluentQueryModel) stringRange(lo, hi string) map[string]struct{} {
	out := make(map[string]struct{})
	for name := range m.persons {
		if name >= lo && name <= hi {
			out[name] = struct{}{}
		}
	}
	return out
}

// intRange is the model's answer for a [query.WithRange] over `age`. A Person
// with NO age is excluded, which is what both engines must also do: the fluent
// scan's query.withRange.Match returns false when the property is absent, and
// Cypher's `n.age >= $lo` on a missing property is NULL and fails the filter.
func (m *fluentQueryModel) intRange(lo, hi int64) map[string]struct{} {
	out := make(map[string]struct{})
	for name, p := range m.persons {
		if p.hasAge && p.age >= lo && p.age <= hi {
			out[name] = struct{}{}
		}
	}
	return out
}

// fluentQuerySubstrate is the third, engine-independent view: the live nodes as
// the raw [lpg.Graph] holds them, obtained by walking the Mapper, skipping
// tombstoned ids, and reading each survivor's `name`.
//
// It exists to make a probe failure attributable. Held to the model before any
// probe runs, it separates "the substrate already diverged" from "one engine's
// working-set logic is wrong".
type fluentQuerySubstrate struct {
	// idToName maps every LIVE, named NodeID to its name. An id absent from this
	// map is either tombstoned or unnamed, and an engine that returns one has
	// returned something that is not a live Person.
	idToName map[graph.NodeID]string
	nameToID map[string]graph.NodeID
	// tombstonedInLabelIndex is how many ids in the RAW :Person label bitmap are
	// tombstoned at this instant. It is TELEMETRY, never a gate: the label-bitmap
	// removal is deferred and applied by the background vacuum, so this is a
	// function of when the sweeper woke (see the file header).
	tombstonedInLabelIndex int
	// mapperSlots is how many ids [graph.Mapper.Walk] yields — every id ever
	// interned, live or not, because the Mapper never forgets a slot.
	// tombstonedSlots is how many of them are tombstoned, which is the set
	// query.seedAllLive hands to pruneTombstones and is therefore the
	// DETERMINISTIC measure that the prune is load-bearing.
	mapperSlots     int
	tombstonedSlots int
	// tombstoneCount is [lpg.Graph.TombstoneCount] at this instant. When it is
	// zero, query.pruneTombstones takes its lock-free early return and prunes
	// nothing at all.
	tombstoneCount int
	// duplicateNames are names carried by more than one live node, which would
	// make the name-keyed comparison unsound.
	duplicateNames []string
	// unnamedLive is how many live ids carry no string `name`.
	unnamedLive int
}

// newFluentQuerySubstrate walks the graph and builds the substrate view.
//
// The Walk callback only appends to local state and never re-enters the Mapper,
// satisfying [graph.Mapper.Walk]'s re-entrancy contract; the property and
// tombstone reads happen after Walk returns.
func newFluentQuerySubstrate(g *lpg.Graph[string, float64]) *fluentQuerySubstrate {
	type entry struct {
		key string
		id  graph.NodeID
	}
	var entries []entry
	g.AdjList().Mapper().Walk(func(id graph.NodeID, key string) bool {
		entries = append(entries, entry{key: key, id: id})
		return true
	})

	s := &fluentQuerySubstrate{
		idToName:       make(map[graph.NodeID]string, len(entries)),
		nameToID:       make(map[string]graph.NodeID, len(entries)),
		tombstoneCount: g.TombstoneCount(),
	}
	s.mapperSlots = len(entries)
	for _, e := range entries {
		if g.IsTombstoned(e.id) {
			s.tombstonedSlots++
			continue
		}
		pv, ok := g.GetNodeProperty(e.key, "name")
		if !ok {
			s.unnamedLive++
			continue
		}
		name, ok := pv.String()
		if !ok {
			s.unnamedLive++
			continue
		}
		if prev, dup := s.nameToID[name]; dup && prev != e.id {
			s.duplicateNames = append(s.duplicateNames, name)
			continue
		}
		s.idToName[e.id] = name
		s.nameToID[name] = e.id
	}
	sort.Strings(s.duplicateNames)

	// The TELEMETRY measure: tombstoned ids still advertised by the label index,
	// which is the set query.pruneTombstones must remove from
	// query.seedFromPreds' bitmap on the label-seeded path. It is read through
	// the same helper the file's reproduction path uses, so the two cannot drift.
	it := fqUnprunedLabelIDs(g).Iterator()
	for it.HasNext() {
		if g.IsTombstoned(graph.NodeID(it.Next())) {
			s.tombstonedInLabelIndex++
		}
	}
	return s
}

// liveIDs returns the substrate's live NodeID set — the working set
// Vertex(label) must produce, and the source set an unpruned Out() reproduction
// expands from.
func (s *fluentQuerySubstrate) liveIDs() map[graph.NodeID]struct{} {
	out := make(map[graph.NodeID]struct{}, len(s.idToName))
	for id := range s.idToName {
		out[id] = struct{}{}
	}
	return out
}

// liveNames returns the substrate's live name set.
func (s *fluentQuerySubstrate) liveNames() map[string]struct{} {
	out := make(map[string]struct{}, len(s.nameToID))
	for name := range s.nameToID {
		out[name] = struct{}{}
	}
	return out
}

// FluentQueryEvidence is what the run MEASURED, handed back so a test asserts
// on numbers rather than on the mere absence of a violation, and so the report
// prints what actually happened.
//
// Following the shape [TxnOversizeEvidence] and [indexDiversityEvidence] use,
// the checker itself IS the record: the non-vacuity gates in
// [FluentQueryProbes.Finish] read these very fields, so a test asserting on
// them cannot drift from what the gates enforce.
type FluentQueryEvidence struct {
	// Batteries is how many times the full probe battery ran, and
	// BatteriesAfterRecovery how many of those ran immediately after a crash
	// recovery.
	Batteries              int
	BatteriesAfterRecovery int
	// ChurnCycles is how many delete-then-recreate pairs the churn phase
	// committed.
	ChurnCycles int
	// MaxLiveNames / MaxOutTargets are the largest label and one-hop answers the
	// model held at a battery, so "the probes were not comparing empty sets" is
	// a measured fact.
	MaxLiveNames  int
	MaxOutTargets int
	// MaxTombstonedSlots is the largest number of TOMBSTONED ids
	// [graph.Mapper.Walk] yielded at a battery, and MaxTombstoneCount the largest
	// [lpg.Graph.TombstoneCount]. Both are DETERMINISTIC (the Mapper never
	// forgets a slot and a tombstone is never cleared on this workload — Cypher
	// CREATE cannot reuse a mapper key), and together they are the proof that
	// query.pruneTombstones was load-bearing on the no-predicate seed path.
	MaxTombstonedSlots int
	MaxTombstoneCount  int
	// MaxTombstonedInLabelIndexObserved is the largest number of tombstoned ids
	// the RAW :Person label bitmap carried at a battery. It is TELEMETRY, not a
	// gate, and it is excluded from [FluentQueryEvidence.ReproducibleSummary]:
	// the label-bitmap removal is deferred and applied by lpg's background
	// vacuum, so this value depends on when that goroutine woke. MEASURED: the
	// same seed in the same process gave 3 and 2. See the file header.
	MaxTombstonedInLabelIndexObserved int
	// StringRangeNonEmpty / StringRangeEmpty and IntRangeNonEmpty /
	// IntRangeEmpty count how often each range probe's window matched something
	// and nothing. Both directions are driven by CONSTRUCTION (one window is a
	// live value, the other is out of the value space), so both must be
	// positive.
	StringRangeNonEmpty int
	StringRangeEmpty    int
	IntRangeNonEmpty    int
	IntRangeEmpty       int
	// SeekEligible / SeekIneligible count the batteries at which every condition
	// of query.trySeekProperty / query.trySeekRange held for the hash and the
	// string btree respectively. An ineligible battery means the seek arm
	// silently degraded to a scan and the seek-vs-scan clause compared a path
	// with itself.
	HashSeekEligible    int
	HashSeekIneligible  int
	BTreeSeekEligible   int
	BTreeSeekIneligible int
	// GhostFixtures is how many times the constructed ghost-arc fixture ran, and
	// GhostArcsSeen the total number of raw arcs into a tombstoned target it
	// constructed. The fixture is the only place Out()'s ghost-arc prune branch
	// is reachable (DETACH DELETE strips arcs on the live graph), so a zero here
	// means that branch was never exercised.
	GhostFixtures int
	GhostArcsSeen int
	// MixedKindProbes counts the mixed-kind probes — FLOAT64 bounds over an
	// INT64-valued property — and MixedKindNonEmpty how many of them had a
	// NON-EMPTY model answer. Since rmp #2600 this probe is a full three-way
	// clause, not telemetry, so the second counter is the one that matters: two
	// empty sets agree, and a probe that only ever compared empty sets would
	// make the clause silent rather than satisfied. Both are gated in
	// [FluentQueryProbes.Finish].
	//
	// MixedKindLastSeek and MixedKindLastOracle are the last probe's seek-arm
	// and model cardinalities, kept so the run REPORTS the number the clause
	// adjudicated instead of only whether it held.
	MixedKindProbes     int
	MixedKindNonEmpty   int
	MixedKindLastSeek   uint64
	MixedKindLastOracle int
	// EqMixed* are the same four quantities for the EQUALITY side of the same
	// unification (rmp #2601): a FLOAT64 expected value in a [query.WithProperty]
	// over an INT64-valued property. They are counted separately from the
	// MixedKind* range counters on purpose — the two predicates take different
	// index arms (a numeric equality is btree-served as a degenerate range, never
	// hash-served) and one being exercised says nothing about the other, so a
	// shared counter could not gate them independently.
	//
	// EqMixedNonEmpty is again the counter that matters: two empty answers agree
	// whatever the comparison does, so a probe that only ever compared empty sets
	// would make both the eq-mixed clause and the
	// equality-vs-degenerate-range clause silent rather than satisfied.
	EqMixedProbes     int
	EqMixedNonEmpty   int
	EqMixedLastSeek   uint64
	EqMixedLastOracle int
	// NumericSeekEligible / NumericSeekIneligible count the batteries at which
	// every condition of query.trySeekRange — and, since rmp #2601, of
	// query.trySeekProperty for a numeric value — held for the internal numeric
	// companion btree over (Person, age). An ineligible battery means every
	// numeric arm degraded to a scan and the range-int / range-mixed / eq-mixed
	// seek-vs-scan clauses compared one path with itself.
	NumericSeekEligible   int
	NumericSeekIneligible int
	// CSRRawArcs / CSRLiveArcs are the arc counts of the LAST battery's two CSR
	// builds, and CSRGenerationsDiffered counts how many batteries — over the
	// whole run, not just the last — saw the two builds disagree.
	//
	// The counter exists because "the two generations are equal on the live
	// graph" is a claim about EVERY battery, and the last battery's pair cannot
	// support it. It is expected to stay 0: DETACH DELETE strips the deleted
	// node's arcs, so the tombstone-agnostic build has no ghost to keep. That is
	// precisely why [fluentQueryGhostFixture] exists, and a non-zero value here
	// would mean the live graph had started producing ghost arcs — worth knowing,
	// which is why it is recorded rather than assumed.
	CSRRawArcs             uint64
	CSRLiveArcs            uint64
	CSRGenerationsDiffered int
	// Digest folds every probe's (tick, clause, cardinalities) triple. It is the
	// scenario's reproducibility claim: same seed, same digest. It folds no
	// NodeID and no mapper key, both of which come from a process-global counter
	// and are not a function of the seed.
	Digest uint64
}

// String renders the evidence for a report and for the run's own output.
func (e *FluentQueryEvidence) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "batteries=%d (post-recovery=%d) churn=%d", e.Batteries, e.BatteriesAfterRecovery, e.ChurnCycles)
	fmt.Fprintf(&b, " maxLive=%d maxOut=%d maxTombSlots=%d maxTombCount=%d",
		e.MaxLiveNames, e.MaxOutTargets, e.MaxTombstonedSlots, e.MaxTombstoneCount)
	fmt.Fprintf(&b, " strRange=%d/%d(non-empty/empty) intRange=%d/%d",
		e.StringRangeNonEmpty, e.StringRangeEmpty, e.IntRangeNonEmpty, e.IntRangeEmpty)
	fmt.Fprintf(&b, " seekEligible hash=%d/%d btree=%d/%d numeric=%d/%d",
		e.HashSeekEligible, e.HashSeekIneligible, e.BTreeSeekEligible, e.BTreeSeekIneligible,
		e.NumericSeekEligible, e.NumericSeekIneligible)
	fmt.Fprintf(&b, " ghostFixtures=%d ghostArcs=%d csrArcs raw=%d live=%d csrDiffered=%d",
		e.GhostFixtures, e.GhostArcsSeen, e.CSRRawArcs, e.CSRLiveArcs, e.CSRGenerationsDiffered)
	fmt.Fprintf(&b, " mixedKind=%d/%d(probes/non-empty) last(seek=%d oracle=%d)",
		e.MixedKindProbes, e.MixedKindNonEmpty, e.MixedKindLastSeek, e.MixedKindLastOracle)
	fmt.Fprintf(&b, " eqMixed=%d/%d(probes/non-empty) last(seek=%d oracle=%d)",
		e.EqMixedProbes, e.EqMixedNonEmpty, e.EqMixedLastSeek, e.EqMixedLastOracle)
	fmt.Fprintf(&b, " digest=%#016x", e.Digest)
	// Telemetry, printed LAST and labelled, so nobody mistakes it for a gated
	// quantity: it is scheduler-dependent (see the field's doc).
	fmt.Fprintf(&b, " [telemetry, scheduler-dependent] labelIndexCorpses=%d",
		e.MaxTombstonedInLabelIndexObserved)
	return b.String()
}

// ReproducibleSummary renders exactly the fields that are a pure function of the
// seed. It exists so the determinism test compares what the scenario CLAIMS is
// reproducible instead of the whole record: one field —
// [FluentQueryEvidence.MaxTombstonedInLabelIndexObserved] — depends on when
// lpg's background vacuum swept the deferred label-index removals, and asserting
// equality on it would be asserting a scheduler outcome.
func (e *FluentQueryEvidence) ReproducibleSummary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "batteries=%d post-recovery=%d churn=%d", e.Batteries, e.BatteriesAfterRecovery, e.ChurnCycles)
	fmt.Fprintf(&b, " maxLive=%d maxOut=%d maxTombSlots=%d maxTombCount=%d",
		e.MaxLiveNames, e.MaxOutTargets, e.MaxTombstonedSlots, e.MaxTombstoneCount)
	fmt.Fprintf(&b, " strRange=%d/%d intRange=%d/%d",
		e.StringRangeNonEmpty, e.StringRangeEmpty, e.IntRangeNonEmpty, e.IntRangeEmpty)
	fmt.Fprintf(&b, " seek=%d/%d,%d/%d,%d/%d",
		e.HashSeekEligible, e.HashSeekIneligible, e.BTreeSeekEligible, e.BTreeSeekIneligible,
		e.NumericSeekEligible, e.NumericSeekIneligible)
	fmt.Fprintf(&b, " ghost=%d/%d csr=%d/%d diff=%d",
		e.GhostFixtures, e.GhostArcsSeen, e.CSRRawArcs, e.CSRLiveArcs, e.CSRGenerationsDiffered)
	fmt.Fprintf(&b, " mixed=%d/%d last=%d/%d",
		e.MixedKindProbes, e.MixedKindNonEmpty, e.MixedKindLastSeek, e.MixedKindLastOracle)
	fmt.Fprintf(&b, " eqMixed=%d/%d last=%d/%d",
		e.EqMixedProbes, e.EqMixedNonEmpty, e.EqMixedLastSeek, e.EqMixedLastOracle)
	fmt.Fprintf(&b, " digest=%#016x", e.Digest)
	return b.String()
}

// FluentQueryProbes is the stateful three-way differential checker: it runs the
// probe battery on demand and accumulates the evidence its terminal
// non-vacuity gate reads.
//
// # Concurrency contract
//
// FluentQueryProbes is NOT safe for concurrent use. It draws from a [Seed] and
// issues engine reads that need a quiescent view of the graph, so it must be
// driven from the single simulation goroutine — the same contract
// [CheckSearch] carries and for the same reason.
type FluentQueryProbes struct {
	seed *Seed
	ev   *FluentQueryEvidence
}

// NewFluentQueryProbes returns a probe battery drawing its windows from seed.
// The seed must be derived from the run seed and must NOT be the workload's, so
// the workload's op stream stays byte-identical.
func NewFluentQueryProbes(seed *Seed) *FluentQueryProbes {
	return &FluentQueryProbes{seed: seed, ev: &FluentQueryEvidence{}}
}

// Evidence returns the accumulating record. The pointer is owned by the probes
// and is live for the whole run.
func (p *FluentQueryProbes) Evidence() *FluentQueryEvidence { return p.ev }

// fqOp renders a clause name as a report op label.
func fqOp(clause string) string { return "<fluent-query:" + clause + ">" }

// fqViolation builds a violation for a clause.
func fqViolation(kind ViolationKind, tick int64, clause, format string, args ...any) Violation {
	return Violation{Kind: kind, Tick: tick, Op: fqOp(clause), Message: fmt.Sprintf(format, args...)}
}

// fqNameSet renders a name set as a bounded, sorted, deterministic string for a
// violation message. It is bounded by [maxDivergenceSamples] so a wholesale
// divergence cannot produce an unbounded report — the same discipline
// search_check.go's sampleNames applies.
func fqNameSet(s map[string]struct{}) string {
	names := make([]string, 0, len(s))
	for n := range s {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) > maxDivergenceSamples {
		return fmt.Sprintf("%v … (+%d more)", names[:maxDivergenceSamples], len(names)-maxDivergenceSamples)
	}
	return fmt.Sprintf("%v", names)
}

// fqSetDiff returns the elements of a that are absent from b.
func fqSetDiff(a, b map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{})
	for k := range a {
		if _, ok := b[k]; !ok {
			out[k] = struct{}{}
		}
	}
	return out
}

// fqCompare emits one violation when got != want, naming both directions of the
// difference so the report says WHAT diverged and not merely that it did.
func fqCompare(tick int64, clause, probe string, got, want map[string]struct{}) []Violation {
	if len(got) == len(want) && len(fqSetDiff(got, want)) == 0 {
		return nil
	}
	missing := fqSetDiff(want, got)
	extra := fqSetDiff(got, want)
	return []Violation{fqViolation(ViolationOracleDeviation, tick, clause,
		"probe %s: %d name(s) missing %s, %d extra %s (got %d, want %d)",
		probe, len(missing), fqNameSet(missing), len(extra), fqNameSet(extra), len(got), len(want))}
}

// fqObservation is one arm's observation of one pattern: the name set it
// returned plus the three self-consistency counts, which must agree.
type fqObservation struct {
	names map[string]struct{}
	// cardinality is Pattern.Cardinality(), collected is len(Pattern.Collect())
	// and iterated is how many ids Pattern.NodeIDs() yielded. The three read the
	// same working set through three different accessors and must agree.
	cardinality uint64
	collected   int
	iterated    int
	// unknownIDs are ids the pattern returned that are NOT live named nodes in
	// the substrate view — a tombstoned corpse, or an unnamed node. This is the
	// identity-level detector for a prune that stopped working; a count-level
	// comparison could coincidentally match.
	unknownIDs []graph.NodeID
}

// fqObserve drives one pattern through all three public accessors and projects
// its working set onto names via the substrate view.
//
// [query.Pattern.Cardinality], [query.Pattern.Collect] and
// [query.Pattern.NodeIDs] are all read-only over the pattern's bitmap, so one
// pattern serves all three; the builder calls (Vertex/Out) are the mutating
// ones and each arm gets its own freshly built pattern.
func fqObserve(pat *fqPattern, sub *fluentQuerySubstrate) fqObservation {
	obs := fqObservation{names: make(map[string]struct{})}
	obs.cardinality = pat.Cardinality()
	obs.collected = len(pat.Collect())
	for id := range pat.NodeIDs() {
		obs.iterated++
		name, ok := sub.idToName[id]
		if !ok {
			if len(obs.unknownIDs) < maxDivergenceSamples {
				obs.unknownIDs = append(obs.unknownIDs, id)
			}
			continue
		}
		obs.names[name] = struct{}{}
	}
	return obs
}

// fqSelfConsistency checks the three accessors against each other and the
// live-identity of every returned id.
func fqSelfConsistency(tick int64, probe string, obs fqObservation) []Violation {
	var vs []Violation
	if uint64(obs.collected) != obs.cardinality || uint64(obs.iterated) != obs.cardinality {
		vs = append(vs, fqViolation(ViolationGraphIntegrity, tick, "self-consistency",
			"probe %s: Cardinality()=%d but len(Collect())=%d and NodeIDs() yielded %d; the three "+
				"accessors read the same working set and must agree (a short Collect means an id in "+
				"the working set no longer resolves through the Mapper)",
			probe, obs.cardinality, obs.collected, obs.iterated))
	}
	if len(obs.unknownIDs) > 0 {
		vs = append(vs, fqViolation(ViolationGraphIntegrity, tick, "unknown-id",
			"probe %s: returned %d NodeID(s) that are not live named nodes in the substrate view "+
				"(sample %v): a tombstoned or unnamed id reached the working set, which is what "+
				"query.pruneTombstones exists to prevent",
			probe, len(obs.unknownIDs), obs.unknownIDs))
	}
	return vs
}

// fqCypherNames drains a single-column Cypher read into a name set, through the
// same [EngineAdapter] the workload uses, so the Cypher arm is the real public
// read path and not a private shortcut.
//
// A duplicate name in the result is reported rather than silently collapsed:
// the probes' name-keyed comparison assumes live names are unique, and the
// model-shape and substrate preconditions assert that on their own sides.
func fqCypherNames(
	ctx context.Context, eng *EngineAdapter, q string, params map[string]any,
) (names map[string]struct{}, dups int, err error) {
	res, err := eng.Run(ctx, q, params)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = res.Close() }()
	names = make(map[string]struct{})
	for res.Next() {
		s, ok := res.StringAt(0)
		if !ok {
			return nil, 0, fmt.Errorf("sim: fluent-query: %q returned a non-string first column", q)
		}
		if _, seen := names[s]; seen {
			dups++
			continue
		}
		names[s] = struct{}{}
	}
	if err := res.Err(); err != nil {
		return nil, 0, err
	}
	return names, dups, nil
}

// fqSeekEligibility reports whether every condition query/index_seek.go requires
// to serve a predicate from an index holds for (label, property) at this
// instant, for the given index Kind and typed read interface.
//
// The conditions are exactly trySeekProperty's / trySeekRange's guard plus
// indexCovers: a non-nil manager, an index of the right Kind, a BoundNode()
// matching (label, property), and a concrete index satisfying the typed read
// interface for the bound value's kind. typed is supplied by the caller as a
// type-assertion closure so this function stays kind-agnostic.
func fqSeekEligibility(
	g *lpg.Graph[string, float64], kind, label, property string, typed func(index.Subscriber) bool,
) bool {
	mgr := g.IndexManager()
	if mgr == nil {
		return false
	}
	for _, name := range mgr.ListIndexes() {
		sub, err := mgr.GetIndex(name)
		if err != nil || sub.Kind() != kind {
			continue
		}
		b, ok := sub.(interface {
			BoundNode() (string, string, bool)
		})
		if !ok {
			continue
		}
		bl, bp, bound := b.BoundNode()
		if !bound || bl != label || bp != property {
			continue
		}
		if typed(sub) {
			return true
		}
	}
	return false
}

// fqUnprunedLabelIDs returns the RAW :Person label bitmap — the set
// query.seedFromPreds hands to pruneTombstones on the LABEL-seeded path. It is
// read only to MEASURE the telemetry described in the file header; it is not the
// input to any gate, because the label-bitmap removal is swept asynchronously.
func fqUnprunedLabelIDs(g *lpg.Graph[string, float64]) *roaring64.Bitmap {
	lid, ok := g.Registry().Lookup(fluentQueryLabel)
	if !ok {
		return roaring64.New()
	}
	return g.NodeIndex().Intersect(uint32(lid))
}

// fqUnprunedWalkNames is every id [graph.Mapper.Walk] yields, projected through
// the substrate view WITHOUT the tombstone prune — that is, exactly the output
// query.seedAllLive would produce if its pruneTombstones call were a no-op.
//
// This is the DETERMINISTIC reproduction of a broken prune: the Mapper never
// forgets a slot, so every id ever interned is yielded on every call and every
// tombstoned one shows up as an id with no live name.
func fqUnprunedWalkNames(g *lpg.Graph[string, float64], sub *fluentQuerySubstrate) fqObservation {
	obs := fqObservation{names: make(map[string]struct{})}
	var ids []graph.NodeID
	g.AdjList().Mapper().Walk(func(id graph.NodeID, _ string) bool {
		ids = append(ids, id)
		return true
	})
	for _, id := range ids {
		obs.iterated++
		name, ok := sub.idToName[id]
		if !ok {
			if len(obs.unknownIDs) < maxDivergenceSamples {
				obs.unknownIDs = append(obs.unknownIDs, id)
			}
			continue
		}
		obs.names[name] = struct{}{}
	}
	obs.cardinality = uint64(obs.iterated)
	obs.collected = obs.iterated
	return obs
}

// fqUnprunedOutNames is the one-hop out-neighbourhood of every LIVE source over
// the RAW CSR, WITHOUT the tombstone prune Out() applies to its result — that
// is, exactly the output query.Pattern.Out would produce if its
// pruneTombstones call were a no-op. Ids with no live name are reported as
// unknown, which is how the perturbation surfaces as a violation.
//
// It reads the CSR arrays the same way Out() does
// ([csr.CSR.VerticesSlice]/[csr.CSR.EdgesSlice]), so the reproduction is of the
// real code path and not an approximation of it.
func fqUnprunedOutNames(
	c *csr.CSR[float64], sources map[graph.NodeID]struct{}, sub *fluentQuerySubstrate,
) fqObservation {
	obs := fqObservation{names: make(map[string]struct{})}
	verts := c.VerticesSlice()
	edges := c.EdgesSlice()
	seen := make(map[graph.NodeID]struct{})
	for src := range sources {
		s := uint64(src)
		if s+1 >= uint64(len(verts)) {
			continue
		}
		for k := verts[s]; k < verts[s+1]; k++ {
			seen[edges[k]] = struct{}{}
		}
	}
	for id := range seen {
		obs.iterated++
		name, ok := sub.idToName[id]
		if !ok {
			if len(obs.unknownIDs) < maxDivergenceSamples {
				obs.unknownIDs = append(obs.unknownIDs, id)
			}
			continue
		}
		obs.names[name] = struct{}{}
	}
	obs.cardinality = uint64(len(seen))
	obs.collected = len(seen)
	return obs
}

// fqDropOne removes the lexicographically smallest name from a set, returning a
// copy. It is the perturbation primitive for the Cypher and scan arms: dropping
// a deterministic element keeps a perturbed run reproducible.
func fqDropOne(s map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(s))
	for k := range s {
		out[k] = struct{}{}
	}
	names := make([]string, 0, len(out))
	for k := range out {
		names = append(names, k)
	}
	sort.Strings(names)
	if len(names) > 0 {
		delete(out, names[0])
	}
	return out
}

// fqDigestSet folds a probe's clause id and the three cardinalities into the
// running digest. It reuses [genSwapMix] — the package's byte-at-a-time FNV-1a
// fold — rather than declaring a second identical mixer.
func fqDigestSet(h uint64, tick int64, clause string, fluent, cypher, oracle int) uint64 {
	h = genSwapMix(h, uint64(tick))
	for i := 0; i < len(clause); i++ {
		h = genSwapMix(h, uint64(clause[i]))
	}
	h = genSwapMix(h, uint64(fluent))
	h = genSwapMix(h, uint64(cypher))
	return genSwapMix(h, uint64(oracle))
}

// fqProbeSpec is one three-way probe: the fluent SEEK arm, an optional fluent
// SCAN arm, the equivalent Cypher read, and the model's expected answer.
//
// The two fluent arms are built by the caller and are already-constructed
// patterns: a [query.Pattern] is mutated in place by its builder calls, so each
// arm owns its own pattern and no pattern is ever reused.
type fqProbeSpec struct {
	// params are the Cypher parameters; nil for a literal-free query.
	params map[string]any
	// seek is the arm whose predicate an index may serve, and scan the arm whose
	// predicate is applied in a SECOND Vertex call and therefore reaches
	// query.filterByPreds with an empty label list, which both trySeekProperty
	// and trySeekRange refuse. scan is nil for a probe with no second arm.
	seek *fqPattern
	scan *fqPattern
	// want is the MODEL's answer, computed from [GraphOracle] alone.
	want map[string]struct{}
	name string
	// cypher is the equivalent read through the real engine, and armClause the
	// clause name for the seek-vs-scan comparison when scan is non-nil.
	cypher    string
	armClause string
}

// runProbe drives one three-way probe and returns its violations plus the SEEK
// arm's observation (which the caller needs for the CSR-invariance and
// perturbation clauses).
//
// perturb applies to the arms this probe carries: fqPerturbCypherDropRow
// perturbs the Cypher answer and fqPerturbScanArmDrop the scan arm's, and both
// are no-ops for a probe that has no such arm. The prune perturbations are
// applied by the caller, which owns the reproduction of the broken output.
func (p *FluentQueryProbes) runProbe(
	ctx context.Context, tick int64, eng *EngineAdapter, sub *fluentQuerySubstrate,
	spec *fqProbeSpec, perturb fqPerturb,
) ([]Violation, fqObservation, error) {
	seekObs := fqObserve(spec.seek, sub)
	vs := fqSelfConsistency(tick, spec.name, seekObs)

	cypherNames, dups, err := fqCypherNames(ctx, eng, spec.cypher, spec.params)
	if err != nil {
		return nil, seekObs, fmt.Errorf("sim: fluent-query probe %s: %w", spec.name, err)
	}
	if dups > 0 {
		vs = append(vs, fqViolation(ViolationGraphIntegrity, tick, "cypher-duplicate-name",
			"probe %s: the Cypher arm returned %d duplicate name(s); live names must be unique for the "+
				"name-keyed three-way comparison to be sound", spec.name, dups))
	}
	if perturb == fqPerturbCypherDropRow {
		cypherNames = fqDropOne(cypherNames)
	}

	fluentNames := seekObs.names
	if perturb == fqPerturbFluentDropName {
		fluentNames = fqDropOne(fluentNames)
	}
	vs = append(vs, fqCompare(tick, "fluent-vs-oracle", spec.name, fluentNames, spec.want)...)
	vs = append(vs, fqCompare(tick, "cypher-vs-oracle", spec.name, cypherNames, spec.want)...)
	vs = append(vs, fqCompare(tick, "fluent-vs-cypher", spec.name, fluentNames, cypherNames)...)

	if spec.scan != nil {
		scanObs := fqObserve(spec.scan, sub)
		vs = append(vs, fqSelfConsistency(tick, spec.name+":scan", scanObs)...)
		scanNames := scanObs.names
		if perturb == fqPerturbScanArmDrop {
			scanNames = fqDropOne(scanNames)
		}
		vs = append(vs, fqCompare(tick, spec.armClause+":scan-vs-oracle", spec.name, scanNames, spec.want)...)
		vs = append(vs, fqCompare(tick, spec.armClause+":seek-vs-scan", spec.name, seekObs.names, scanNames)...)
	}

	p.ev.Digest = fqDigestSet(p.ev.Digest, tick, spec.name,
		int(seekObs.cardinality), len(cypherNames), len(spec.want))
	return vs, seekObs, nil
}

// Check runs the whole probe battery once, at a quiescent instant, and returns
// every clause that failed (nil when all hold).
//
// It must be called from the single simulation goroutine: it builds two CSR
// snapshots of the live adjacency and issues engine reads, both of which need a
// consistent, quiescent view — the same contract [CheckSearch] carries.
//
// perturb is [fqPerturbNone] for every real run; the other values exist so a
// test can prove each clause family fires (see [fqPerturb]).
//
// scatter the shared model/substrate/CSR setup across helpers that each need all
// three, and the probe list is the readable form of what the scenario covers.
//
//nolint:gocyclo,maintidx // one battery of independent probes; splitting it would
func (p *FluentQueryProbes) Check(
	ctx context.Context, tick int64, g *lpg.Graph[string, float64], eng *EngineAdapter,
	o *GraphOracle, perturb fqPerturb,
) ([]Violation, error) {
	p.ev.Batteries++

	// --- (1) the MODEL: the arbiter, computed from the oracle alone. ---
	m := newFluentQueryModel(o)
	if len(m.shapeFindings) > 0 {
		shown := m.shapeFindings[:min(len(m.shapeFindings), maxDivergenceSamples)]
		vs := make([]Violation, 0, len(shown))
		for _, f := range shown {
			vs = append(vs, fqViolation(ViolationOracleDeviation, tick, "precondition:model-shape", "%s", f))
		}
		// The expected answers would be wrong, so no probe can be adjudicated.
		return vs, nil
	}

	// --- (2) the SUBSTRATE: the raw graph, read through neither engine. ---
	sub := newFluentQuerySubstrate(g)
	if len(sub.duplicateNames) > 0 || sub.unnamedLive > 0 {
		return []Violation{fqViolation(ViolationGraphIntegrity, tick, "precondition:name-uniqueness",
			"the substrate holds %d duplicate live name(s) %v and %d unnamed live node(s); the probes' "+
				"name-keyed comparison is unsound",
			len(sub.duplicateNames), sub.duplicateNames, sub.unnamedLive)}, nil
	}
	if vs := fqCompare(tick, "precondition:substrate-parity", "mapper-walk",
		sub.liveNames(), m.liveNames()); len(vs) > 0 {
		// The substrate had already diverged from the model, so a probe failure
		// below would be unattributable. Report the divergence itself.
		return vs, nil
	}
	if sub.tombstonedInLabelIndex > p.ev.MaxTombstonedInLabelIndexObserved {
		p.ev.MaxTombstonedInLabelIndexObserved = sub.tombstonedInLabelIndex
	}
	if sub.tombstonedSlots > p.ev.MaxTombstonedSlots {
		p.ev.MaxTombstonedSlots = sub.tombstonedSlots
	}
	if sub.tombstoneCount > p.ev.MaxTombstoneCount {
		p.ev.MaxTombstoneCount = sub.tombstoneCount
	}
	if len(m.persons) > p.ev.MaxLiveNames {
		p.ev.MaxLiveNames = len(m.persons)
	}

	// --- (3) the two CSR generations, built FRESH at this instant. ---
	cLive := csr.BuildFromAdjListLive(g.AdjList(), g.LiveNodeFilter())
	cRaw := csr.BuildFromAdjList(g.AdjList())
	p.ev.CSRLiveArcs, p.ev.CSRRawArcs = cLive.Size(), cRaw.Size()
	if cLive.Size() != cRaw.Size() {
		p.ev.CSRGenerationsDiffered++
	}
	engLive := query.New(g, cLive)
	engRaw := query.New(g, cRaw)

	// --- (4) seek eligibility, by enumerating query/index_seek.go's guard. ---
	var vs []Violation
	hashOK := fqSeekEligibility(g, "hash", fluentQueryLabel, "name", func(s index.Subscriber) bool {
		_, ok := s.(interface {
			Cardinality(string) uint64
			LookupAppend(string, []uint64) []uint64
			Lookup(string) *roaring64.Bitmap
		})
		return ok
	})
	btreeOK := fqSeekEligibility(g, "btree", fluentQueryLabel, "name", func(s index.Subscriber) bool {
		_, ok := s.(interface {
			Range(lo, hi string) *roaring64.Bitmap
		})
		return ok
	})
	// The NUMERIC companion over (Person, age). Since rmp #2600
	// query.seekRangeInto routes every numeric bound pair — int64, float64, or
	// one of each — to a btreeRanger[float64], and since rmp #2601
	// query.trySeekProperty routes a numeric EQUALITY to the same index as the
	// degenerate range [v, v]. So this one guard underwrites the range-int,
	// range-mixed AND eq-mixed seek arms, and its absence would silently turn all
	// three into scan-vs-scan.
	numericOK := fqSeekEligibility(g, "btree", fluentQueryLabel, "age", func(s index.Subscriber) bool {
		_, ok := s.(interface {
			Range(lo, hi float64) *roaring64.Bitmap
		})
		return ok
	})
	if hashOK {
		p.ev.HashSeekEligible++
	} else {
		p.ev.HashSeekIneligible++
	}
	if btreeOK {
		p.ev.BTreeSeekEligible++
	} else {
		p.ev.BTreeSeekIneligible++
	}
	if numericOK {
		p.ev.NumericSeekEligible++
	} else {
		p.ev.NumericSeekIneligible++
	}
	if !hashOK || !btreeOK || !numericOK {
		vs = append(vs, fqViolation(ViolationOracleDeviation, tick, "precondition:seek-eligibility",
			"no bound index satisfies query/index_seek.go's guard: hash[string](%s,name)=%v "+
				"btree[string](%s,name)=%v btree[float64](%s,age)=%v (the last one serves the "+
				"numeric ranges AND, since #2601, the numeric equality). The seek arm would "+
				"silently degrade to the scan, so the seek-vs-scan clause would compare one path "+
				"with itself",
			fluentQueryLabel, hashOK, fluentQueryLabel, btreeOK, fluentQueryLabel, numericOK))
	}

	label := fqLabel(fluentQueryLabel)

	// --- probe: the whole label. The seed path, and the tombstone prune. ---
	{
		spec := fqProbeSpec{
			name:   "label",
			seek:   engLive.Match().Vertex(label),
			cypher: "MATCH (n:Person) RETURN n.name",
			want:   m.liveNames(),
		}
		pv, _, err := p.runProbe(ctx, tick, eng, sub, &spec, perturb)
		if err != nil {
			return nil, err
		}
		vs = append(vs, pv...)
	}

	// --- probe: the NO-PREDICATE seed (query.seedAllLive), which walks the
	// Mapper and prunes. This is the deterministic home of the tombstone attack:
	// the Mapper never forgets a slot, so every tombstoned id is yielded on every
	// call and the prune has to remove all of them, for the whole run.
	{
		spec := fqProbeSpec{
			name:   "all-live",
			seek:   engLive.Match().Vertex(),
			cypher: "MATCH (n) RETURN n.name",
			want:   m.liveNames(),
		}
		pv, _, err := p.runProbe(ctx, tick, eng, sub, &spec, perturb)
		if err != nil {
			return nil, err
		}
		vs = append(vs, pv...)
		if perturb == fqPerturbSeedPruneDisabled {
			// Reproduce the OUTPUT of a pruneTombstones that stopped working, from
			// the same Mapper walk query.seedAllLive uses.
			broken := fqUnprunedWalkNames(g, sub)
			vs = append(vs, fqSelfConsistency(tick, "all-live(seed-prune-disabled)", broken)...)
			vs = append(vs, fqCompare(tick, "fluent-vs-oracle", "all-live(seed-prune-disabled)",
				broken.names, spec.want)...)
		}
	}

	// --- probe: equality on an existing name (hash-seekable) and its scan twin.
	var pick string
	if len(m.sorted) > 0 {
		pick = m.sorted[p.seed.IntN(len(m.sorted))]
	}
	if pick != "" {
		pv := lpg.StringValue(pick)
		spec := fqProbeSpec{
			name:      "eq-present",
			seek:      engLive.Match().Vertex(label, fqProp("name", pv)),
			scan:      engLive.Match().Vertex(label).Vertex(fqProp("name", pv)),
			cypher:    "MATCH (n:Person) WHERE n.name = $name RETURN n.name",
			params:    map[string]any{"name": pick},
			want:      map[string]struct{}{pick: {}},
			armClause: "eq",
		}
		v, _, err := p.runProbe(ctx, tick, eng, sub, &spec, perturb)
		if err != nil {
			return nil, err
		}
		vs = append(vs, v...)
	}

	// --- probe: equality on a name no template can produce (guaranteed miss).
	{
		pv := lpg.StringValue(fluentQueryAbsentName)
		spec := fqProbeSpec{
			name:      "eq-absent",
			seek:      engLive.Match().Vertex(label, fqProp("name", pv)),
			scan:      engLive.Match().Vertex(label).Vertex(fqProp("name", pv)),
			cypher:    "MATCH (n:Person) WHERE n.name = $name RETURN n.name",
			params:    map[string]any{"name": fluentQueryAbsentName},
			want:      map[string]struct{}{},
			armClause: "eq-absent",
		}
		v, _, err := p.runProbe(ctx, tick, eng, sub, &spec, perturb)
		if err != nil {
			return nil, err
		}
		vs = append(vs, v...)
	}

	// --- probe: one hop from a single source, over BOTH CSR generations. ---
	if pick != "" {
		pvv := lpg.StringValue(pick)
		spec := fqProbeSpec{
			name:   "out-one",
			seek:   engLive.Match().Vertex(label, fqProp("name", pvv)).Out(),
			cypher: "MATCH (a:Person {name:$name})-[:KNOWS]->(b) RETURN DISTINCT b.name",
			params: map[string]any{"name": pick},
			want:   m.outOf(pick),
		}
		v, obs, err := p.runProbe(ctx, tick, eng, sub, &spec, perturb)
		if err != nil {
			return nil, err
		}
		vs = append(vs, v...)
		rawObs := fqObserve(engRaw.Match().Vertex(label, fqProp("name", pvv)).Out(), sub)
		rawNames := rawObs.names
		if perturb == fqPerturbRawArmDrop {
			rawNames = fqDropOne(rawNames)
		}
		vs = append(vs, fqCompare(tick, "csr-generation-invariance", "out-one",
			rawNames, obs.names)...)
	}

	// --- probe: one hop from the whole label, over BOTH CSR generations. ---
	{
		spec := fqProbeSpec{
			name:   "out-all",
			seek:   engLive.Match().Vertex(label).Out(),
			cypher: "MATCH (a:Person)-[:KNOWS]->(b) RETURN DISTINCT b.name",
			want:   m.outAll(),
		}
		v, obs, err := p.runProbe(ctx, tick, eng, sub, &spec, perturb)
		if err != nil {
			return nil, err
		}
		vs = append(vs, v...)
		if len(obs.names) > p.ev.MaxOutTargets {
			p.ev.MaxOutTargets = len(obs.names)
		}
		rawObs := fqObserve(engRaw.Match().Vertex(label).Out(), sub)
		rawNames := rawObs.names
		if perturb == fqPerturbRawArmDrop {
			rawNames = fqDropOne(rawNames)
		}
		vs = append(vs, fqCompare(tick, "csr-generation-invariance", "out-all",
			rawNames, obs.names)...)
		// NOTE: fqPerturbOutPruneDisabled is deliberately NOT applied here. On the
		// live graph DETACH DELETE strips the deleted node's arcs (MEASURED: cRaw
		// and cLive report the same Size()), so there is no ghost arc for Out()'s
		// prune to remove and reproducing "the prune did nothing" would reproduce
		// the CORRECT answer — an inert perturbation dressed up as a proof. That
		// perturbation belongs to [fluentQueryGhostFixture], which constructs the
		// ghost arcs and asserts it did.
	}

	// --- probes: string range over `name`, served by the bound string btree. ---
	strWindows := []struct {
		name   string
		lo, hi string
	}{
		{name: "range-str-absent", lo: fluentQueryAbsentName, hi: fluentQueryAbsentName},
	}
	if pick != "" {
		strWindows = append(strWindows, struct {
			name   string
			lo, hi string
		}{name: "range-str-point", lo: pick, hi: pick})
	}
	if len(m.sorted) >= 2 {
		i := p.seed.IntN(len(m.sorted))
		j := p.seed.IntN(len(m.sorted))
		if i > j {
			i, j = j, i
		}
		strWindows = append(strWindows, struct {
			name   string
			lo, hi string
		}{name: "range-str-window", lo: m.sorted[i], hi: m.sorted[j]})
	}
	for _, w := range strWindows {
		lo, hi := lpg.StringValue(w.lo), lpg.StringValue(w.hi)
		want := m.stringRange(w.lo, w.hi)
		if len(want) > 0 {
			p.ev.StringRangeNonEmpty++
		} else {
			p.ev.StringRangeEmpty++
		}
		spec := fqProbeSpec{
			name:      w.name,
			seek:      engLive.Match().Vertex(label, fqRange("name", lo, hi)),
			scan:      engLive.Match().Vertex(label).Vertex(fqRange("name", lo, hi)),
			cypher:    "MATCH (n:Person) WHERE n.name >= $lo AND n.name <= $hi RETURN n.name",
			params:    map[string]any{"lo": w.lo, "hi": w.hi},
			want:      want,
			armClause: "range-str",
		}
		v, _, err := p.runProbe(ctx, tick, eng, sub, &spec, perturb)
		if err != nil {
			return nil, err
		}
		vs = append(vs, v...)
	}

	// --- probes: int64 range over `age`. Since rmp #2600 the SEEK arm is served
	// by the numeric companion btree.Index[float64] (see the header) as a
	// superset, with query.valueInRange as the exact residual filter, while the
	// SCAN arm reaches query.withRange.Match directly because its predicate sits
	// in a second Vertex call with no label. So this pair now compares two
	// genuinely different code paths, and it remains the probe that covers the
	// age-absent (MERGE-created) Persons the model excludes and 3VL filters out
	// of the Cypher answer.
	intWindows := []struct {
		name   string
		lo, hi int64
	}{
		{name: "range-int-absent", lo: fluentQueryEmptyAgeLo, hi: fluentQueryEmptyAgeHi},
	}
	agedName, haveAged := m.firstAgedName()
	if haveAged {
		age := m.persons[agedName].age
		intWindows = append(intWindows, struct {
			name   string
			lo, hi int64
		}{name: "range-int-point", lo: age, hi: age})
	}
	{
		i := int64(p.seed.IntN(100))
		j := int64(p.seed.IntN(100))
		if i > j {
			i, j = j, i
		}
		intWindows = append(intWindows, struct {
			name   string
			lo, hi int64
		}{name: "range-int-window", lo: i, hi: j})
	}
	for _, w := range intWindows {
		lo, hi := lpg.Int64Value(w.lo), lpg.Int64Value(w.hi)
		want := m.intRange(w.lo, w.hi)
		if len(want) > 0 {
			p.ev.IntRangeNonEmpty++
		} else {
			p.ev.IntRangeEmpty++
		}
		spec := fqProbeSpec{
			name:      w.name,
			seek:      engLive.Match().Vertex(label, fqRange("age", lo, hi)),
			scan:      engLive.Match().Vertex(label).Vertex(fqRange("age", lo, hi)),
			cypher:    "MATCH (n:Person) WHERE n.age >= $lo AND n.age <= $hi RETURN n.name",
			params:    map[string]any{"lo": w.lo, "hi": w.hi},
			want:      want,
			armClause: "range-int",
		}
		v, _, err := p.runProbe(ctx, tick, eng, sub, &spec, perturb)
		if err != nil {
			return nil, err
		}
		vs = append(vs, v...)
	}

	// --- probe: MIXED-KIND. FLOAT64 bounds over an INT64-valued property, the
	// divergence this scenario found and rmp #2600 closed (see the file header).
	// It is now a full three-way clause like every other probe: openCypher orders
	// INTEGER and FLOAT in ONE numeric order, so the model's int-keyed answer IS
	// the expected answer for a float-bounded window, and the seek arm (numeric
	// companion, superset) and the scan arm (query.valueInRange, exact) must both
	// reproduce it.
	if haveAged {
		age := m.persons[agedName].age
		lo, hi := lpg.Float64Value(float64(age)), lpg.Float64Value(float64(age))
		want := m.intRange(age, age)
		p.ev.MixedKindProbes++
		if len(want) > 0 {
			p.ev.MixedKindNonEmpty++
		}
		p.ev.MixedKindLastOracle = len(want)
		spec := fqProbeSpec{
			name:      "range-mixed-point",
			seek:      engLive.Match().Vertex(label, fqRange("age", lo, hi)),
			scan:      engLive.Match().Vertex(label).Vertex(fqRange("age", lo, hi)),
			cypher:    "MATCH (n:Person) WHERE n.age >= $lo AND n.age <= $hi RETURN n.name",
			params:    map[string]any{"lo": float64(age), "hi": float64(age)},
			want:      want,
			armClause: "range-mixed",
		}
		v, seekObs, err := p.runProbe(ctx, tick, eng, sub, &spec, perturb)
		if err != nil {
			return nil, err
		}
		p.ev.MixedKindLastSeek = seekObs.cardinality
		vs = append(vs, v...)

		// The other half of #2600: bounds of DIFFERENT kinds. query.trySeekRange
		// used to bail out whenever lo.Kind() != hi.Kind(), which made this shape
		// consistently wrong rather than merely divergent; the CIP makes the two
		// bound tests independent, so [age, age+0.5] is a well-formed numeric
		// window and — ages being integers — its answer is exactly the age-point
		// answer.
		loMix, hiMix := lpg.Int64Value(age), lpg.Float64Value(float64(age)+0.5)
		p.ev.MixedKindProbes++
		if len(want) > 0 {
			p.ev.MixedKindNonEmpty++
		}
		mixedSpec := fqProbeSpec{
			name:      "range-mixed-bounds",
			seek:      engLive.Match().Vertex(label, fqRange("age", loMix, hiMix)),
			scan:      engLive.Match().Vertex(label).Vertex(fqRange("age", loMix, hiMix)),
			cypher:    "MATCH (n:Person) WHERE n.age >= $lo AND n.age <= $hi RETURN n.name",
			params:    map[string]any{"lo": age, "hi": float64(age) + 0.5},
			want:      want,
			armClause: "range-mixed",
		}
		mv, _, err := p.runProbe(ctx, tick, eng, sub, &mixedSpec, perturb)
		if err != nil {
			return nil, err
		}
		vs = append(vs, mv...)

		// --- probe: the EQUALITY side of the same unification (rmp #2601). A
		// FLOAT64 expected value over an INT64-valued property. It is the exact
		// mirror of range-mixed-point, and until #2601 the two DISAGREED with each
		// other: the degenerate range [age, age] matched and the equality did not,
		// so the same data answered differently depending on how the predicate was
		// written. Both are now one relation over one comparator, so the model's
		// int-keyed answer is the expected answer for both.
		//
		// The SEEK arm here exercises a different index arm from every other
		// equality probe in this battery: a numeric equality is not hash-served at
		// all (a single-kind hash index is a SUBSET of a unified equality), so it
		// is served by the numeric companion btree as the degenerate range, with
		// query.equalValue as the exact residual filter. That is why the
		// numeric-seek eligibility gate now underwrites this probe too.
		eqMixed := lpg.Float64Value(float64(age))
		p.ev.EqMixedProbes++
		if len(want) > 0 {
			p.ev.EqMixedNonEmpty++
		}
		p.ev.EqMixedLastOracle = len(want)
		eqSpec := fqProbeSpec{
			name:      "eq-mixed-point",
			seek:      engLive.Match().Vertex(label, fqProp("age", eqMixed)),
			scan:      engLive.Match().Vertex(label).Vertex(fqProp("age", eqMixed)),
			cypher:    "MATCH (n:Person) WHERE n.age = $age RETURN n.name",
			params:    map[string]any{"age": float64(age)},
			want:      want,
			armClause: "eq-mixed",
		}
		ev, eqObs, err := p.runProbe(ctx, tick, eng, sub, &eqSpec, perturb)
		if err != nil {
			return nil, err
		}
		p.ev.EqMixedLastSeek = eqObs.cardinality
		vs = append(vs, ev...)

		// The SAME value as an equality and as a degenerate range must return the
		// same set. Adjudicating the two against the shared model already implies
		// it, but only while BOTH probes have a non-empty answer to disagree
		// about; asserting the identity DIRECTLY is what makes the #2601 clause
		// independent of that, and it names the asymmetry rather than leaving a
		// reader to infer it from two separate probe failures.
		degenerate := fqObserve(engLive.Match().Vertex(label, fqRange("age", eqMixed, eqMixed)), sub)
		vs = append(vs, fqSelfConsistency(tick, "eq-mixed-point:degenerate-range", degenerate)...)
		degenerateNames := degenerate.names
		if perturb == fqPerturbDegenerateRangeDrop {
			degenerateNames = fqDropOne(degenerateNames)
		}
		vs = append(vs, fqCompare(tick, "eq-mixed:equality-vs-degenerate-range", "eq-mixed-point",
			eqObs.names, degenerateNames)...)
	}

	// --- the constructed ghost-arc fixture: the ONLY place Out()'s ghost-arc
	// prune branch is reachable (DETACH DELETE strips arcs on the live graph).
	gv, ghostArcs := fluentQueryGhostFixture(tick, NewSeed(p.seed.Uint64N(1<<62)), perturb)
	p.ev.GhostFixtures++
	p.ev.GhostArcsSeen += ghostArcs
	vs = append(vs, gv...)

	return vs, nil
}

// fluentQueryGhostFixtureNodes is the fixture's node count and
// fluentQueryGhostFixtureRemoved how many of them are tombstoned WITHOUT their
// arcs being stripped. The values are small and fixed so the fixture is
// microseconds long and its precondition is CONSTRUCTIBLE for every seed: a
// path graph of N nodes has N-1 arcs, and tombstoning any interior node leaves
// at least one arc pointing INTO a tombstoned target.
const (
	fluentQueryGhostFixtureNodes   = 8
	fluentQueryGhostFixtureRemoved = 2
)

// fluentQueryGhostLabel is the fixture's node label, deliberately different from
// [fluentQueryLabel] so a fixture graph can never be confused with the live one.
const fluentQueryGhostLabel = "FQGhost"

// fluentQueryGhostFixture builds a small side graph in which
// [lpg.Graph.RemoveNode] tombstones nodes WITHOUT stripping their incident arcs
// — the one documented way to produce a ghost arc — and adjudicates
// graph/query's two prunes against a hand-computed model.
//
// It exists because the live graph provably cannot reach Out()'s ghost-arc
// branch: MEASURED on this tree, `DETACH DELETE` strips the deleted node's arcs,
// so the raw and live-filtered CSR builds have the same arc count and Out()'s
// pruneTombstones has nothing to remove. Rather than let the
// csr-generation-invariance clause pass vacuously on the live graph, this
// fixture constructs the precondition and ASSERTS it was constructed:
//
//	ghost-fixture:precondition   cRaw must carry at least one arc into a
//	                             tombstoned target, and cRaw.Size() must exceed
//	                             cLive.Size(). Without this the answer clauses
//	                             below would hold for a graph with no ghosts.
//	ghost-fixture:seed-prune     Vertex(label) must return exactly the live
//	                             nodes, even though the raw label bitmap still
//	                             advertises the tombstoned ones.
//	ghost-fixture:out-prune      Vertex(label).Out() over the RAW CSR must omit
//	                             every tombstoned target, i.e. must equal the
//	                             model's live one-hop set.
//	ghost-fixture:csr-invariance Out() over the raw and the live-filtered CSR
//	                             must agree, which on THIS graph is a real
//	                             comparison because the two builds differ.
//
// The returned int is how many ghost arcs the fixture constructed, so the
// caller can record that the branch was really exercised.
//
// seed selects which interior nodes are removed, so the fixture is a function
// of the run seed while its precondition holds for every seed: the removed set
// is drawn from the INTERIOR of a path graph, and an interior node always has an
// incoming arc.
func fluentQueryGhostFixture(tick int64, seed *Seed, perturb fqPerturb) ([]Violation, int) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	names := make([]string, fluentQueryGhostFixtureNodes)
	for i := range names {
		names[i] = fmt.Sprintf("fqg-%d", i)
		if err := g.AddNode(names[i]); err != nil {
			return []Violation{fqViolation(ViolationOracleDeviation, tick, "ghost-fixture:setup",
				"AddNode(%q): %v", names[i], err)}, 0
		}
		if err := g.SetNodeLabel(names[i], fluentQueryGhostLabel); err != nil {
			return []Violation{fqViolation(ViolationOracleDeviation, tick, "ghost-fixture:setup",
				"SetNodeLabel(%q): %v", names[i], err)}, 0
		}
		if err := g.SetNodeProperty(names[i], "name", lpg.StringValue(names[i])); err != nil {
			return []Violation{fqViolation(ViolationOracleDeviation, tick, "ghost-fixture:setup",
				"SetNodeProperty(%q): %v", names[i], err)}, 0
		}
	}
	// A path graph: every interior node has exactly one incoming and one
	// outgoing arc, which is what makes the ghost precondition seed-independent.
	for i := 0; i+1 < len(names); i++ {
		if err := g.AddEdge(names[i], names[i+1], 1); err != nil {
			return []Violation{fqViolation(ViolationOracleDeviation, tick, "ghost-fixture:setup",
				"AddEdge(%q,%q): %v", names[i], names[i+1], err)}, 0
		}
	}

	// Remove distinct INTERIOR nodes (indices 1 .. N-2), so each removal leaves
	// an arc pointing into a tombstoned target.
	interior := len(names) - 2
	removed := make(map[string]struct{}, fluentQueryGhostFixtureRemoved)
	for len(removed) < fluentQueryGhostFixtureRemoved {
		name := names[1+seed.IntN(interior)]
		if _, dup := removed[name]; dup {
			continue
		}
		removed[name] = struct{}{}
		g.RemoveNode(name)
	}

	// The MODEL, computed by hand from the construction above.
	live := make(map[string]struct{}, len(names))
	for _, n := range names {
		if _, gone := removed[n]; !gone {
			live[n] = struct{}{}
		}
	}
	// The one-hop set the engine must return: targets of LIVE sources over the
	// raw adjacency, minus tombstoned targets. The path arcs are i -> i+1, so a
	// live source i contributes i+1 when i+1 is live.
	wantOut := make(map[string]struct{})
	for i := 0; i+1 < len(names); i++ {
		if _, srcGone := removed[names[i]]; srcGone {
			continue
		}
		if _, dstGone := removed[names[i+1]]; dstGone {
			continue
		}
		wantOut[names[i+1]] = struct{}{}
	}

	cRaw := csr.BuildFromAdjList(g.AdjList())
	cLive := csr.BuildFromAdjListLive(g.AdjList(), g.LiveNodeFilter())

	// The PRECONDITION: count the raw arcs whose target is tombstoned.
	ghostArcs := 0
	verts, edges := cRaw.VerticesSlice(), cRaw.EdgesSlice()
	for src := 0; src+1 < len(verts); src++ {
		for k := verts[src]; k < verts[src+1]; k++ {
			if g.IsTombstoned(edges[k]) {
				ghostArcs++
			}
		}
	}
	var vs []Violation
	if ghostArcs == 0 || cRaw.Size() <= cLive.Size() {
		vs = append(vs, fqViolation(ViolationVacuousRun, tick, "ghost-fixture:precondition",
			"the fixture constructed %d ghost arc(s) and cRaw.Size()=%d vs cLive.Size()=%d; without a "+
				"raw arc into a tombstoned target the prune clauses below cannot fail and prove nothing",
			ghostArcs, cRaw.Size(), cLive.Size()))
		return vs, ghostArcs
	}

	sub := newFluentQueryGhostSubstrate(g, names)
	label := fqLabel(fluentQueryGhostLabel)
	engRaw := query.New(g, cRaw)
	engLive := query.New(g, cLive)

	seedObs := fqObserve(engRaw.Match().Vertex(label), sub)
	vs = append(vs, fqSelfConsistency(tick, "ghost-fixture:label", seedObs)...)
	vs = append(vs, fqCompare(tick, "ghost-fixture:seed-prune", "label", seedObs.names, live)...)

	rawOut := fqObserve(engRaw.Match().Vertex(label).Out(), sub)
	vs = append(vs, fqSelfConsistency(tick, "ghost-fixture:out", rawOut)...)
	vs = append(vs, fqCompare(tick, "ghost-fixture:out-prune", "out-all", rawOut.names, wantOut)...)

	liveOut := fqObserve(engLive.Match().Vertex(label).Out(), sub)
	invariantRaw := rawOut.names
	if perturb == fqPerturbRawArmDrop {
		invariantRaw = fqDropOne(invariantRaw)
	}
	vs = append(vs, fqCompare(tick, "ghost-fixture:csr-invariance", "out-all",
		invariantRaw, liveOut.names)...)

	if perturb == fqPerturbOutPruneDisabled {
		// Reproduce the OUTPUT of a query.Pattern.Out whose pruneTombstones call is
		// a no-op: the raw out-neighbourhood of the live sources, unpruned. This is
		// the only place the reproduction is meaningful, because it is the only
		// place ghost arcs exist — and the precondition above proved they do.
		broken := fqUnprunedOutNames(cRaw, sub.liveIDs(), sub)
		vs = append(vs, fqSelfConsistency(tick, "ghost-fixture:out(out-prune-disabled)", broken)...)
		vs = append(vs, fqCompare(tick, "ghost-fixture:out-prune", "out-all(out-prune-disabled)",
			broken.names, wantOut)...)
	}
	return vs, ghostArcs
}

// newFluentQueryGhostSubstrate is the fixture's substrate view. The fixture
// interns its nodes under keys that ARE their names, so the mapping is direct
// and needs no property read — but the tombstone filter still applies, which is
// what makes "an id the engine returned is not a live named node" meaningful.
func newFluentQueryGhostSubstrate(g *lpg.Graph[string, float64], names []string) *fluentQuerySubstrate {
	s := &fluentQuerySubstrate{
		idToName:       make(map[graph.NodeID]string, len(names)),
		nameToID:       make(map[string]graph.NodeID, len(names)),
		tombstoneCount: g.TombstoneCount(),
	}
	for _, n := range names {
		id, ok := g.AdjList().Mapper().Lookup(n)
		if !ok || g.IsTombstoned(id) {
			continue
		}
		s.idToName[id] = n
		s.nameToID[n] = id
	}
	return s
}

// Finish is the terminal non-vacuity gate: it fails the run when the battery
// never reached the state that makes its clauses capable of failing.
//
// Every gate here is STRUCTURAL — guaranteed by the prologue, by the churn
// phase, or by a window drawn outside the value space — rather than a rate the
// scheduler or the workload's draws might not deliver. That is the lesson
// #2587/#2596 taught this sprint: a threshold on a count nobody controls is a
// flake waiting to happen, and a coverage clause may only fail a run whose
// precondition was constructed.
func (p *FluentQueryProbes) Finish(tick int64) []Violation {
	e := p.ev
	var vs []Violation
	add := func(clause, format string, args ...any) {
		vs = append(vs, fqViolation(ViolationVacuousRun, tick, "vacuity:"+clause, format, args...))
	}
	if e.Batteries == 0 {
		add("batteries", "the probe battery never ran: no clause in this scenario was evaluated")
		return vs
	}
	if e.BatteriesAfterRecovery == 0 {
		add("post-recovery", "the battery never ran after a crash recovery, so nothing here was "+
			"adjudicated against a graph that survived WAL replay (crashes=%d)", e.Batteries)
	}
	if e.ChurnCycles == 0 {
		add("churn", "the delete-then-recreate churn phase never ran, so the tombstone set never grew "+
			"under the probes")
	}
	if e.MaxLiveNames == 0 {
		add("live-names", "every battery saw an EMPTY label answer: the three-way comparison only ever "+
			"compared empty sets, which the prologue exists to prevent")
	}
	if e.MaxOutTargets == 0 {
		add("out-targets", "every battery saw an EMPTY one-hop answer: query.Pattern.Out never expanded "+
			"a single arc, so no CSR was actually read")
	}
	if e.MaxTombstonedSlots == 0 || e.MaxTombstoneCount == 0 {
		add("tombstone-load-bearing", "no battery ever saw a tombstoned Mapper slot "+
			"(tombstonedSlots=%d tombstoneCount=%d), so query.seedAllLive's pruneTombstones removed "+
			"nothing and the all-live probe would have passed with the prune deleted. The gate is on "+
			"the Mapper walk, not on the label bitmap, because the label-bitmap removal is swept "+
			"asynchronously by lpg's background vacuum", e.MaxTombstonedSlots, e.MaxTombstoneCount)
	}
	if e.StringRangeNonEmpty == 0 || e.StringRangeEmpty == 0 {
		add("string-range", "the string range probe did not exercise both directions "+
			"(non-empty=%d empty=%d); one window is a live name and one is out of the name space, so "+
			"both are guaranteed by construction and a zero means the probe did not run",
			e.StringRangeNonEmpty, e.StringRangeEmpty)
	}
	if e.IntRangeNonEmpty == 0 || e.IntRangeEmpty == 0 {
		add("int-range", "the int64 range probe did not exercise both directions "+
			"(non-empty=%d empty=%d)", e.IntRangeNonEmpty, e.IntRangeEmpty)
	}
	if e.HashSeekEligible == 0 {
		add("hash-seek", "no battery found a bound hash index satisfying query/index_seek.go's guard, "+
			"so the equality seek arm was never actually index-served")
	}
	if e.BTreeSeekEligible == 0 {
		add("btree-seek", "no battery found a bound string btree satisfying query/index_seek.go's "+
			"guard, so the string range seek arm was never actually index-served")
	}
	if e.NumericSeekEligible == 0 {
		add("numeric-seek", "no battery found the internal numeric companion btree[float64] "+
			"satisfying query/index_seek.go's guard for (%s,age), so the int64-range, mixed-kind "+
			"range and mixed-kind EQUALITY seek arms were never actually index-served and their "+
			"seek-vs-scan clauses compared one path with itself", fluentQueryLabel)
	}
	if e.MixedKindProbes == 0 {
		add("mixed-kind", "the mixed-kind probe never ran, so the divergence rmp #2600 closed is no "+
			"longer being adjudicated at all")
	} else if e.MixedKindNonEmpty == 0 {
		add("mixed-kind-non-empty", "every mixed-kind probe compared EMPTY sets (probes=%d): two "+
			"empty answers agree whatever the comparison does, so the range-mixed clause could not "+
			"have failed. The window is drawn from the first AGED Person precisely so this cannot "+
			"happen", e.MixedKindProbes)
	}
	if e.EqMixedProbes == 0 {
		add("eq-mixed", "the mixed-kind EQUALITY probe never ran, so the asymmetry rmp #2601 closed "+
			"— a degenerate range matching where the equality did not — is not being adjudicated at "+
			"all")
	} else if e.EqMixedNonEmpty == 0 {
		add("eq-mixed-non-empty", "every mixed-kind equality probe compared EMPTY sets (probes=%d): "+
			"two empty answers agree whatever the comparison does, so neither the eq-mixed clause "+
			"nor the equality-vs-degenerate-range clause could have failed. The value is drawn from "+
			"the first AGED Person precisely so this cannot happen", e.EqMixedProbes)
	}
	if e.GhostArcsSeen == 0 {
		add("ghost-arcs", "the constructed fixture produced no ghost arc, so query.Pattern.Out's "+
			"tombstone prune branch was never reached (it is unreachable on the live graph, where "+
			"DETACH DELETE strips arcs)")
	}
	return vs
}

// FluentQueryConfig parameterises a fluent-query run. The zero value is not
// usable; [DefaultFluentQueryConfig] fills in the short-layer budgets and
// [FluentQueryConfig.normalise] repairs any field a caller left at zero, so a
// test can override one field without restating the rest.
type FluentQueryConfig struct {
	// Seed is the master seed. Every sub-stream (probes, churn, prologue,
	// workload, crash schedule, SimDisk) derives from it, so the whole run —
	// including [FluentQueryEvidence.Digest] — is a pure function of this value.
	Seed uint64
	// MaxTicks bounds the deterministic loop.
	MaxTicks int
	// PrologueNodes is how many Persons (plus a KNOWS path over them) are
	// created through the modelled templates before the loop. It is what makes
	// the non-vacuity gates on a non-empty label and one-hop answer STRUCTURAL.
	PrologueNodes int
	// BatteryEvery and ChurnEvery are the in-loop cadences in ticks.
	BatteryEvery int
	ChurnEvery   int
	// Crash is the crash/recovery schedule. It is a field rather than a constant
	// so a test can DISABLE the schedule and thereby guarantee that the forced
	// crash arm ([fluentQueryForceCrash]) is the one that runs: with the schedule
	// on, whether it fires inside a small budget is seed-dependent, and an arm
	// that only some seeds reach is an arm no test can pin.
	Crash CrashConfig
}

// DefaultFluentQueryConfig returns the short-layer configuration for seed.
func DefaultFluentQueryConfig(seed uint64) FluentQueryConfig {
	return FluentQueryConfig{
		Seed:          seed,
		MaxTicks:      fluentQueryMaxTicks,
		PrologueNodes: fluentQueryPrologueNodes,
		BatteryEvery:  fluentQueryBatteryEvery,
		ChurnEvery:    fluentQueryChurnEvery,
		Crash:         CrashConfig{Enabled: true, CrashProb: 1.0 / 70.0, StabilityWindow: 25},
	}
}

// normalise replaces any non-positive budget with its default, so a caller may
// override one field and leave the rest zero.
func (c *FluentQueryConfig) normalise() {
	if c.MaxTicks <= 0 {
		c.MaxTicks = fluentQueryMaxTicks
	}
	if c.PrologueNodes <= 0 {
		c.PrologueNodes = fluentQueryPrologueNodes
	}
	if c.BatteryEvery <= 0 {
		c.BatteryEvery = fluentQueryBatteryEvery
	}
	if c.ChurnEvery <= 0 {
		c.ChurnEvery = fluentQueryChurnEvery
	}
	// Crash is deliberately NOT defaulted here: a caller that set
	// Enabled:false meant it, and quietly re-enabling the schedule would take
	// away the only way to reach the forced-crash arm deterministically. A
	// zero-value CrashConfig means "no scheduled crashes", and the forced arm
	// then supplies the post-recovery coverage.
}

// fluentQuerySimConfig builds the simulator [Config] the scenario drives: the
// honest write-heavy workload (which already creates, links, ages, merges and
// DETACH DELETEs Persons), plus crash/recovery and in-loop checkpointing so the
// post-recovery battery adjudicates a graph that came back through real WAL
// replay — and, when a checkpoint preceded the crash, through the snapshot that
// carries the tombstone set.
func fluentQuerySimConfig(cfg FluentQueryConfig) Config {
	return Config{
		Seed:     cfg.Seed,
		MaxTicks: cfg.MaxTicks,
		Workload: WriteHeavyWorkload(NewSeed(cfg.Seed)),
		Crash:    cfg.Crash,
		// Well inside the crash stability window, so most crashes follow at least
		// one snapshot+WAL-truncate and the tombstone set crosses the snapshot
		// boundary rather than only the WAL.
		//
		// It also keeps the DURABLE store path selected even when
		// [FluentQueryConfig.Crash] is disabled (Simulator.New opts in on
		// Crash.Enabled || Disk.CapacityBytes > 0 || Checkpoint.Enabled), which is
		// what lets [fluentQueryForceCrash] have a store to crash.
		Checkpoint: CheckpointConfig{Enabled: true, Every: 45},
	}
}

// RunFluentQuery drives the scenario once and returns the evidence it measured
// alongside the report of the first violation (nil when the run is clean).
//
// It owns and closes the simulator, so no durable handle or goroutine leaks past
// the run. The evidence is returned even on a violation, because what the run
// managed to exercise before failing is part of the diagnosis.
func RunFluentQuery(ctx context.Context, cfg FluentQueryConfig) (*FluentQueryEvidence, *SimReport, error) {
	cfg.normalise()
	sm, err := New(fluentQuerySimConfig(cfg))
	if err != nil {
		return nil, nil, fmt.Errorf("sim: fluent-query new: %w", err)
	}
	defer func() { _ = sm.Close() }()

	probes := NewFluentQueryProbes(NewSeed(cfg.Seed ^ fluentQueryProbeSeedMix))
	report, err := fluentQueryLoop(ctx, sm, cfg, probes)
	return probes.Evidence(), report, err
}

// fluentQueryLoop is the scenario body over a simulator the caller owns.
//
// It mirrors [Simulator.Run]'s tick sequence deliberately — checkpoint, then
// the crash decision, then the workload op, then the per-tick parity check — so
// the run is the standard deterministic loop with the battery, the churn phase
// and the post-recovery battery inserted, rather than a different harness that
// happens to look similar.
//
// branch is a distinct, documented phase and inlining them is what makes the
// ordering auditable against Simulator.Run.
//
//nolint:gocyclo // the standard tick loop plus three inserted phases; every
func fluentQueryLoop(
	ctx context.Context, sm *Simulator, cfg FluentQueryConfig, probes *FluentQueryProbes,
) (*SimReport, error) {
	// --- prologue: a modelled Person path, so the FIRST battery has content. ---
	prologueSeed := NewSeed(cfg.Seed ^ fluentQueryPrologueSeedMix)
	names := make([]string, 0, cfg.PrologueNodes)
	for i := 0; i < cfg.PrologueNodes; i++ {
		// A namespace no template can collide with: the workload emits either a
		// bare capitalised first name (MERGE) or "<FirstName>-<u32>" (CREATE).
		name := fmt.Sprintf("fq-p%d", i)
		op := Op{Kind: OpCreate, Cypher: tmplCreatePerson,
			Params: map[string]any{"name": name, "age": int64(prologueSeed.IntN(100))}}
		committed := sm.execute(ctx, op)
		sm.applyToOracle(op, committed)
		if !committed {
			return nil, fmt.Errorf("sim: fluent-query prologue: CREATE %q was not committed", name)
		}
		names = append(names, name)
	}
	for i := 0; i+1 < len(names); i++ {
		op := Op{Kind: OpCreate, Cypher: tmplCreateKnows,
			Params: map[string]any{"a": names[i], "b": names[i+1]}}
		committed := sm.execute(ctx, op)
		sm.applyToOracle(op, committed)
		if !committed {
			return nil, fmt.Errorf("sim: fluent-query prologue: KNOWS %q->%q was not committed",
				names[i], names[i+1])
		}
	}
	for _, ddl := range fluentQueryDDL {
		if err := sm.engineRunDDL(ctx, ddl); err != nil {
			return nil, fmt.Errorf("sim: fluent-query DDL %q: %w", ddl, err)
		}
	}

	churnSeed := NewSeed(cfg.Seed ^ fluentQueryChurnSeedMix)
	// One CONSTRUCTED churn cycle before the first battery, so the tombstone set
	// is non-empty from battery ZERO. Without it, "the run saw a tombstoned
	// Mapper slot" would depend on the workload drawing a DELETE inside the
	// budget, and a non-vacuity gate on something nobody controls is a flake
	// rather than a gate (rmp #2587/#2596). The victim is an interior prologue
	// node, so the KNOWS path survives on both sides of it.
	if len(names) > 2 {
		if !fluentQueryChurnOnce(ctx, sm, probes, names[len(names)/2], churnSeed) {
			return nil, fmt.Errorf("sim: fluent-query prologue churn on %q did not commit",
				names[len(names)/2])
		}
	}
	if r, err := fluentQueryBattery(ctx, sm, probes, 0, "post-prologue"); err != nil || r != nil {
		return r, err
	}
	for i := 0; i < cfg.MaxTicks; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		tick := sm.clock.Tick()

		// Checkpoint BEFORE the crash decision, matching Simulator.Run: a
		// checkpoint that lands just before a crash is the realistic ordering the
		// snapshot+WAL recovery path must survive, and it is what makes the
		// tombstone set cross the snapshot boundary.
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
			// The battery on a graph that came back through real recovery. The
			// engine adapter and the graph were both replaced by the reopen, so
			// this must read them through sm rather than through anything cached.
			probes.ev.BatteriesAfterRecovery++
			if r, err := fluentQueryBattery(ctx, sm, probes, tick, "post-recovery"); err != nil || r != nil {
				return r, err
			}
		}

		actor := sm.workload.SelectActor(sm.seed)
		op := actor.NextOp(sm.seed, sm.oracle)
		committed, counters := sm.executeCounted(ctx, op)
		if violations := CheckOpCounters(tick, op, committed, counters, sm.oracle); len(violations) > 0 {
			return sm.report(tick, op, violations), nil
		}
		sm.applyToOracle(op, committed)

		if tick%int64(sm.cfg.CheckEvery) == 0 {
			if violations := sm.checker.Check(tick, sm.oracle, sm.engine); len(violations) > 0 {
				return sm.report(tick, op, violations), nil
			}
		}

		// --- churn: DETACH DELETE a live Person and re-CREATE the same NAME, so
		// the tombstone set grows deterministically under the probes. Both ops go
		// through the modelled templates, so the oracle stays the arbiter. The
		// draws come from the churn sub-seed, so the workload's op stream is
		// byte-identical to a run without this phase.
		if tick%int64(cfg.ChurnEvery) == 0 {
			live := sm.oracle.NodeNames()
			if len(live) > 0 {
				fluentQueryChurnOnce(ctx, sm, probes, live[churnSeed.IntN(len(live))], churnSeed)
			}
		}

		if tick%int64(cfg.BatteryEvery) == 0 {
			if r, err := fluentQueryBattery(ctx, sm, probes, tick, "periodic"); err != nil || r != nil {
				return r, err
			}
		}
	}

	// --- a CONSTRUCTED crash, if the schedule never fired. ---
	//
	// The post-recovery battery is a coverage claim, and gating it on a crash the
	// SCHEDULE happened to draw would make the gate fail runs whose seed simply
	// did not crash inside the budget — a non-vacuity gate that fails a run whose
	// precondition was never constructed. So the precondition is constructed:
	// when the run reaches the end with no crash, one is forced here and the
	// battery runs on the recovered graph. On a seed that did crash this is a
	// no-op, so the forced arm never changes a run that already had coverage.
	final := int64(cfg.MaxTicks)
	if sm.CrashCount() == 0 {
		if report, err := fluentQueryForceCrash(sm, final); err != nil {
			return nil, err
		} else if report != nil {
			return report, nil
		}
		probes.ev.BatteriesAfterRecovery++
		if r, err := fluentQueryBattery(ctx, sm, probes, final, "post-forced-recovery"); err != nil || r != nil {
			return r, err
		}
	}

	// --- terminal battery, then the non-vacuity gates. ---
	if r, err := fluentQueryBattery(ctx, sm, probes, final, "terminal"); err != nil || r != nil {
		return r, err
	}
	if v := probes.Finish(final); len(v) > 0 {
		return sm.report(final, Op{Kind: OpMatch, Cypher: fqOp("vacuity")}, v), nil
	}
	// Checkpoint non-vacuity: a CheckpointConfig is INERT unless the loop calls
	// maybeCheckpoint, so without this gate the scenario could claim a
	// snapshot-crossing tombstone set it never produced.
	if v := sm.checkCheckpointsFired(final); len(v) > 0 {
		return sm.report(final, Op{Kind: OpMatch, Cypher: fqOp("checkpoint-vacuity")}, v), nil
	}
	return nil, nil
}

// fluentQueryChurnOnce DETACH DELETEs the named Person and re-CREATEs the same
// NAME, through the modelled templates so the oracle stays the arbiter, and
// reports whether both halves committed.
//
// What it exercises, precisely: the Cypher CREATE mints a FRESH synthetic node
// key, so the re-created Person is a NEW NodeID and the deleted one stays
// tombstoned for the rest of the run. This grows the tombstone set the prunes
// must subtract; it does NOT reach [lpg.Graph.AddNode]'s resurrection path,
// which needs the same mapper KEY.
func fluentQueryChurnOnce(
	ctx context.Context, sm *Simulator, probes *FluentQueryProbes, victim string, churnSeed *Seed,
) bool {
	del := Op{Kind: OpDelete, Cypher: tmplDetachDelete, Params: map[string]any{"name": victim}}
	delOK := sm.execute(ctx, del)
	sm.applyToOracle(del, delOK)
	recreate := Op{Kind: OpCreate, Cypher: tmplCreatePerson,
		Params: map[string]any{"name": victim, "age": int64(churnSeed.IntN(100))}}
	addOK := sm.execute(ctx, recreate)
	sm.applyToOracle(recreate, addOK)
	if delOK && addOK {
		probes.ev.ChurnCycles++
		return true
	}
	return false
}

// fluentQueryForceCrash performs one crash+recovery cycle unconditionally and
// runs the harness's own durability check on the recovered engine.
//
// It delegates to [Simulator.forceCrash], which mirrors [Simulator.maybeCrash]'s
// body — a HOST crash on the SimDisk, a reopen with the SAME store configuration
// the crashed store used (crucially the same durable layout, or recovery would
// point at an empty root-level WAL and drop every committed op), a rebind of the
// engine adapter, and [InvariantChecker.CheckDurability] — because that is the
// sequence whose semantics the rest of this scenario's post-recovery clauses
// assume. It exists so the post-recovery coverage claim rests on a CONSTRUCTED
// crash rather than on one the seeded schedule may or may not have drawn.
func fluentQueryForceCrash(sm *Simulator, tick int64) (*SimReport, error) {
	// The body lives on the Simulator ([Simulator.forceCrash]) so this scenario
	// and typed_schema.go share one implementation of the sequence rather than
	// two copies that can drift; a run with no durable layer gets (nil, nil) and
	// Finish reports the missing post-recovery coverage.
	return sm.forceCrash(tick, fqOp("forced-crash-recovery"))
}

// fluentQueryBattery runs one battery and wraps any violation in a report
// labelled with the phase that ran it, so a failure says WHEN it was found
// (post-prologue / post-recovery / periodic / terminal) and not only what.
func fluentQueryBattery(
	ctx context.Context, sm *Simulator, probes *FluentQueryProbes, tick int64, phase string,
) (*SimReport, error) {
	v, err := probes.Check(ctx, tick, sm.graph(), sm.engine, sm.oracle, fqPerturbNone)
	if err != nil {
		return nil, err
	}
	if len(v) == 0 {
		return nil, nil
	}
	return sm.report(tick, Op{Kind: OpMatch, Cypher: fqOp(phase)}, v), nil
}

// fluentQueryScenario is the catalogue entry (rmp #2492).
//
// It carries a custom run override rather than using the standard deterministic
// dispatch because the battery needs the live [lpg.Graph] and a freshly built
// CSR at each probe point — neither of which the standard loop exposes to a
// [CheckSelection] check — and because the churn phase and the post-recovery
// battery have to be interleaved with the tick loop rather than appended to it.
func fluentQueryScenario() Scenario {
	return Scenario{
		Name: ScenarioFluentQuery,
		Description: "graph/query's fluent engine as a SECOND read path over the same graph: label / " +
			"property / Out() / WithRange probes adjudicated three ways — fluent vs the model, Cypher vs " +
			"the model, and the two engines against each other — through delete-then-recreate churn, a " +
			"constructed ghost-arc fixture for Out()'s tombstone prune, and crash+checkpoint recovery",
		Mode:        ModeDeterministic,
		DefaultSeed: fluentQueryDefaultSeed,
		MaxTicks:    fluentQueryMaxTicks,
		Workload:    WriteHeavyWorkload,
		Crash:       CrashConfig{Enabled: true, CrashProb: 1.0 / 70.0, StabilityWindow: 25},
		Checkpoint:  CheckpointConfig{Enabled: true, Every: 45},
		run:         runFluentQueryScenario,
	}
}

// runFluentQueryScenario is the scenario's run override: drive the loop, then
// attach the measured evidence to whatever report came back so an operator
// reading only the log sees what the run exercised — including the mixed-kind
// telemetry the scenario deliberately does not assert.
func runFluentQueryScenario(ctx context.Context, seed uint64) (*SimReport, error) {
	ev, report, err := RunFluentQuery(ctx, DefaultFluentQueryConfig(seed))
	if err != nil {
		return nil, err
	}
	if report == nil {
		return nil, nil
	}
	report.Scenario = ScenarioFluentQuery
	report.Mode = ModeDeterministic
	report.FailedOp.Cypher = report.FailedOp.Cypher + " " + ev.String()
	return report, nil
}
