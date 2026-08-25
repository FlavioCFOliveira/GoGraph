package sim

// typed_schema.go — rmp #2493: the typed-schema validator surface —
// [lpg.Graph.SetValidator], [lpg.Graph.ValidateNode] and the
// [github.com/FlavioCFOliveira/GoGraph/graph/lpg/schema] package — driven as a
// runtime ENFORCEMENT hook rather than as a declaration.
//
// # What was unreached, and why the gap was invisible
//
// Before this file nothing under internal/sim called [lpg.Graph.SetValidator]
// and nothing imported graph/lpg/schema, so the DST ran with NO validator
// installed on any graph, in any scenario. That made three separate claims
// unfalsifiable under simulation:
//
//   - the accept/reject VERDICT of a declared property kind, on any of the five
//     write paths that consult the hook;
//   - the all-or-nothing contract of a REJECTED write (the hook sits before the
//     mutation, so a rejection must change nothing observable);
//   - the RECOVERY asymmetry: a graph rebuilt from durable state carries no
//     validator, because the schema is not persisted with snapshots.
//
// The gap was invisible to a green suite for the ordinary reason: no test
// referenced the surface at all. graph/lpg and graph/lpg/schema have their own
// in-package tests over hand-built fixtures (validator_bypass_test.go,
// validate_node_finalise_test.go, schema/enforce_writes_test.go), and
// store/recovery has one regression gate for a single op
// (edge_property_recovery_test.go, task #1418), but none of them runs under
// crash injection, none runs the paths side by side against one declaration,
// and none asks what a RECOVERED graph does.
//
// # The five hook sites, VERIFIED in source rather than taken from the brief
//
// The task's functional requirement said the hook "sits inside the edge-property
// write paths". MEASURED, it is consulted on FIVE paths, node-side included:
//
//	graph/lpg/property.go            setNodePropertyInfo
//	graph/lpg/edge_property.go       setEdgePropertyInfo            (columnar, per pair)
//	graph/lpg/edge_handle.go         setEdgePropertyByHandleInfo    (per stable handle)
//	graph/lpg/edge_instance_props.go setEdgePropertyAtInfo          (per CREATE ordinal)
//	graph/lpg/lpg.go                 AddEdgeLabeledWithProperty     (fused create+property)
//
// Each is exercised as its own arm, because the four property stores are
// genuinely different stores: MEASURED on one pair (a,b) carrying one write
// through each path, EdgeProperties(a,b) returned only the columnar value,
// EdgePropertiesByHandle only the per-handle one and EdgePropertiesAt only the
// per-instance one. A single arm would therefore have proved nothing about the
// other three.
//
// # Why the oracle is a DECLARATION TABLE and not the schema
//
// The verdict oracle is [typedSchemaModel], computed from this file's own
// declaration table ([typedSchemaDecls] and [typedSchemaRequired]) — the same
// table that is fed to [schema.Schema.RegisterProperty] and
// [schema.Schema.RequireProperty]. It NEVER calls Validate or ValidateNode to
// decide what the answer should be, which is the internal/sim rule against
// validating the engine with the engine: a model that asked the schema would
// agree with it by construction.
//
// The observed side is classified by SENTINEL, not by non-nilness:
// [schema.ErrTypeMismatch], [schema.ErrUnknownProperty] and
// [schema.ErrMissingRequired] are three different refusals and an arm that
// accepted any error would pass while the wrong one was raised. An error that
// matches none of them is itself a violation.
//
// # What a rejection must NOT do, and what was MEASURED about it
//
// Every rejected write is followed by a no-mutation battery, and the
// interesting clause is the last one: the hook runs BEFORE the property key is
// interned, so a rejected write must not leave its key in the graph's
// [lpg.PropertyKeyRegistry]. MEASURED: after a rejected write under an
// unregistered key, Graph.PropertyKeys().Lookup of that key still reports
// absent, and the fused path additionally interned NO endpoint node and added
// NO edge. Those are asserted rather than assumed, because "the write returned
// an error" and "the write changed nothing" are different claims and only the
// second is the contract.
//
// The Cypher engine adds a sixth, coarser observation the direct API cannot
// make. MEASURED: a rejected `CREATE (n:Person {name:$name, age:$age})` with a
// bad age INTERNS a mapper slot and then TOMBSTONES it — the statement's undo
// runs — so the engine's node count is unchanged and the name is unfindable
// while the mapper slot leaks, which is the documented mapper contract
// (NodeID stability) and not a defect. The arm asserts the count and the
// unfindability; it does not assert the slot is reclaimed, because it is not.
//
// # Why Graph.ValidateNode needs its own arm
//
// Required-property existence is enforced ONLY where an embedder invokes
// [lpg.Graph.ValidateNode] — never by the engine. MEASURED across the tree before
// this task, the only call anywhere was graph/lpg's own internal dispatch
// (lpg.go, nv.ValidateNode(labels, props) on the [lpg.NodeValidator] interface)
// plus that package's own tests; every hit in graph/lpg/schema is
// Schema.ValidateNode, a different receiver. So no Cypher statement, no store
// commit and no recovery replay has ever evaluated a RequireProperty
// declaration, and this scenario is the first caller outside graph/lpg.
//
// The split it enforces is not an implementation detail. Typing is decidable
// from one value at the mutation point; existence is not, because a node acquires
// its label before the property that label requires — so a mid-build node
// LEGITIMATELY fails and only a finalised one is adjudicated. The arm pins both
// halves, plus an unlabelled control (no requirement applies), the documented
// nothing-to-check exit for a never-interned key, and a fixture whose forbidden
// value was written BEFORE installation. That last one is the only route, short
// of the recovery bypass, to ValidateNode's kind RE-CHECK over already-present
// properties: with the validator installed, no write can produce a forbidden
// stored value, so the branch is otherwise unreachable.
//
// # The recovery asymmetry, and the one it EXPOSED
//
// Two durable paths order the validator differently, and the difference is not
// cosmetic:
//
//   - The Cypher engine validates BEFORE the WAL. `walMutatorAdapter.SetNodeProperty`
//     (cypher/api.go) calls the validated [lpg.WriteView] write and only then
//     buffers the WAL op, so a rejected value never reaches the log. MEASURED
//     across a crash, on both channels: the accepted value came back and the
//     refused one was absent — asserted as a DIFFERENCE, because "absent" alone
//     is satisfied by a recovery that replayed nothing.
//   - The pure store path validates AFTER the fsync. `txn.Tx.Commit`
//     (store/txn/txn.go) appends and fsyncs every buffered op and only then
//     applies them through the WriteView, so a rejection there returns
//     [txn.ErrCommittedNotApplied] with the frame ALREADY DURABLE.
//
// MEASURED, and this is the finding this scenario surfaced: on that second path
// the rejected value is RESURRECTED by recovery. A commit of
// AddNode + SetNodeProperty("age", StringValue) under a schema declaring `age`
// as PropInt64 returned ErrCommittedNotApplied wrapping
// [schema.ErrTypeMismatch], left the LIVE graph without the property — and after
// a host crash and a reopen, the recovered graph carried `age` as a STRING (four
// WAL ops replayed, the two transactions' two ops each); the replay path
// installs no validator and, by
// [lpg.Graph.SetEdgePropertyByHandleID]'s stated design, deliberately does not
// consult one. So the durable image can hold a value the live validator refused,
// and `ErrCommittedNotApplied`'s own promise that "recovery will reconcile" is
// what materialises it.
//
// [typedSchemaPureStoreArm] pins that behaviour as it stands, in both
// directions: the accepted value must survive (or recovery did nothing at all
// and the arm would be vacuous) and the rejected value must be present. It is a
// PIN on measured behaviour, not an endorsement: if the ordering is ever fixed
// the arm fails loudly and says so in its message, which is the point.
//
// # The reopened graph carries no validator — asserted, not documented away
//
// [SimStore.Graph] returns a graph rebuilt by recovery, and nothing re-installs
// a validator on it: the schema is not among the snapshot's components. The arm
// asserts that positively — the write the live validator rejected is ACCEPTED on
// the freshly recovered graph — and then, having re-installed a schema bound to
// the RECOVERED registries (which is what [schema.New] requires for ids to stay
// consistent), asserts three more clauses from the same probe: the identical
// write is now refused, [lpg.Graph.ValidateNode] now reports the planted value
// as a type mismatch, and it reports clean once the value is repaired. Five
// clauses, each falsifiable, from one constructed pin.
//
// The plant is a DIRECT lpg write, so it is not WAL-logged and cannot
// contaminate the durable image; the repair happens before control returns to
// the tick loop, so no checkpoint can capture it either. Both facts are why the
// pin is safe to run on the live store rather than on a copy.
//
// # Why the direct-API arms run on a SIDE graph
//
// Arms A, B and C drive [lpg.Graph] directly, and a direct write does not go
// through the WAL. Running them on the durable store's own graph would put
// nodes in the engine that the [GraphOracle] does not model, breaking the
// harness's node/edge count parity, and a checkpoint would then make those
// unmodelled nodes durable. So they run on a graph this scenario OWNS
// ([typedSchemaSide]), and the durable store is driven only through modelled
// Cypher templates, exactly as every other scenario drives it. The separation is
// what keeps [InvariantChecker.Check] and [InvariantChecker.CheckDurability]
// meaningful here instead of disabled.
//
// # Coverage is CONSTRUCTED, not drawn
//
// The five write paths times the three verdict classes are fifteen cells, and a
// non-vacuity gate on fifteen randomly drawn cells is a gate that fails a run
// whose draws were unlucky. So the battery SWEEPS: each epoch visits every cell
// exactly once, in a seed-shuffled order. The seed decides the order and the
// values; it does not decide the coverage. The same discipline supplies the
// post-recovery coverage — when the seeded crash schedule never fires inside the
// budget, one crash is FORCED at the end (see [Simulator.forceCrash]).

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg/schema"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// ScenarioTypedSchema is the catalogue key of the typed-schema validator
// scenario (rmp #2493).
const ScenarioTypedSchema = "typed-schema"

// typedSchemaDefaultSeed is the catalogue seed. Like every other scenario's it
// is arbitrary; what matters is that the run is a pure function of it.
const typedSchemaDefaultSeed uint64 = 0x2493_5CE1_A70F

// The sub-seed mixes. Each stream draws from its own derived [Seed] so the
// durable workload's op stream stays a pure function of the master seed and is
// unaffected by the side battery's draws — the convention runIndexDiversity
// established and fluent_query.go follows.
const (
	typedSchemaSideSeedMix  uint64 = 0x2493_51DE_B4E7_7E51
	typedSchemaEngineSeeMix uint64 = 0x2493_E461_1EAF_09C3
	typedSchemaPureSeedMix  uint64 = 0x2493_9002_57A5_1CE0
)

// The scenario's budgets. They are the SHORT-layer defaults: small enough that
// the whole run stays a small share of the package's 60s ceiling. The soak arm
// raises them.
const (
	// typedSchemaMaxTicks bounds the deterministic loop. It is a multiple of the
	// fifteen (path, verdict) cells plus slack, so several full sweeps complete.
	typedSchemaMaxTicks = 330
	// typedSchemaSideNodes is how many nodes the side fixture chains together
	// with handle-bearing edges before the loop starts.
	typedSchemaSideNodes = 6
	// typedSchemaNodeEvery is the cadence, in ticks, of the ValidateNode
	// boundary battery (arm C). It is coarser than the write battery because its
	// clauses are fixture-shaped and do not need per-tick repetition.
	typedSchemaNodeEvery = 8
	// typedSchemaWitnessEvery is the cadence at which a durable WITNESS is armed:
	// a Person created with an accepted age, immediately followed by a rejected
	// age on the same node. Every armed witness is re-verified after every
	// recovery and at the end.
	typedSchemaWitnessEvery = 20
	// typedSchemaPersons is how many Persons the durable prologue creates through
	// the modelled template, and typedSchemaPins how many of them are reserved as
	// post-recovery pin targets.
	typedSchemaPersons = 16
	typedSchemaPins    = 4
)

// The declared property keys of the SIDE schema (arms A, B, C). Four kinds are
// declared so a type mismatch can always be constructed from a value of a
// DIFFERENT declared kind rather than from an exotic one, and one key is
// deliberately left UNREGISTERED so [schema.ErrUnknownProperty] is reachable.
const (
	tsKeyStr   = "ts_str"
	tsKeyInt   = "ts_int"
	tsKeyFloat = "ts_flt"
	tsKeyBool  = "ts_bool"
	// tsKeyGhost is never registered with the schema and must never be interned
	// into the graph's property-key registry: every write under it is refused
	// before the intern. The no-mutation battery asserts exactly that.
	tsKeyGhost = "ts_ghost"
)

// tsSideLabel is the side fixture's label, and it REQUIRES tsKeyStr — the
// whole-node invariant [lpg.Graph.ValidateNode] enforces and
// [schema.Schema.Validate] structurally cannot.
const tsSideLabel = "Doc"

// The side fixture's named nodes. Each exists to make one ValidateNode clause
// permanently reachable, so arm C never depends on the workload's draws.
const (
	// tsPreInstallNode carries a WRONG-KIND value under tsKeyInt, written BEFORE
	// the validator was installed. It is the only way a graph can legitimately
	// hold a value the schema forbids without going through the recovery bypass,
	// and it makes ValidateNode's second loop — the kind re-check over PRESENT
	// properties, which Validate never sees because the write already happened —
	// reachable for the whole run.
	tsPreInstallNode = "ts-preinstall"
	// tsBareNode carries no label at all, so no requirement applies to it and
	// ValidateNode must report clean. It is the control for the mid-build clause.
	tsBareNode = "ts-bare"
	// tsGhostNode is never interned. ValidateNode must report clean for it (there
	// is no node to validate), which is the documented benign case.
	tsGhostNode = "ts-ghost-node"
)

// The engine (durable) schema's keys. `name` and `age` are what the modelled
// templates bind, `created` is what [tmplMergePerson]'s ON CREATE SET writes,
// and `city` — bound by [tmplCreatePersonCity] — is deliberately UNREGISTERED so
// the unknown-property refusal is reachable through the Cypher path too.
const (
	tsEngineKeyName    = "name"
	tsEngineKeyAge     = "age"
	tsEngineKeyCreated = "created"
	tsEngineKeyCity    = "city"
)

// tsEngineLabel is the label the modelled templates create, and it requires
// `name` in the durable schema.
const tsEngineLabel = "Person"

// typedSchemaPinAge is the value a post-recovery pin repairs its target to. It
// is a constant so the repair is deterministic and so the pin can never be
// confused with a witness (whose ages are seed-drawn from a disjoint range).
const typedSchemaPinAge int64 = 4931

// typedSchemaWitnessAgeBase is the base of the witness age range. Witness ages
// are typedSchemaWitnessAgeBase + a small draw, which keeps them disjoint from
// [typedSchemaPinAge] and from the ordinary workload's ages.
const typedSchemaWitnessAgeBase int64 = 1000

// tsDecl is one declared property key and the kind the schema requires of it.
type tsDecl struct {
	key  string
	kind lpg.PropertyKind
}

// typedSchemaDecls returns the SIDE schema's declaration table. It is the
// single source both the installed [schema.Schema] and the verdict model are
// built from, so the two cannot drift apart.
func typedSchemaDecls() []tsDecl {
	return []tsDecl{
		{tsKeyStr, lpg.PropString},
		{tsKeyInt, lpg.PropInt64},
		{tsKeyFloat, lpg.PropFloat64},
		{tsKeyBool, lpg.PropBool},
	}
}

// typedSchemaEngineDecls returns the DURABLE schema's declaration table: the
// keys the modelled Cypher templates actually bind. `city` is absent on
// purpose.
func typedSchemaEngineDecls() []tsDecl {
	return []tsDecl{
		{tsEngineKeyName, lpg.PropString},
		{tsEngineKeyAge, lpg.PropInt64},
		{tsEngineKeyCreated, lpg.PropBool},
	}
}

// tsKindName renders a [lpg.PropertyKind] for a violation message. lpg does not
// give PropertyKind a String method, and a bare integer in a report tells the
// reader nothing.
func tsKindName(k lpg.PropertyKind) string {
	switch k {
	case lpg.PropString:
		return "STRING"
	case lpg.PropInt64:
		return "INTEGER"
	case lpg.PropFloat64:
		return "FLOAT"
	case lpg.PropBool:
		return "BOOLEAN"
	case lpg.PropTime:
		return "TIME"
	case lpg.PropBytes:
		return "BYTES"
	case lpg.PropList:
		return "LIST"
	default:
		return fmt.Sprintf("PropertyKind(%d)", int(k))
	}
}

// tsRender renders a [lpg.PropertyValue] as kind-and-value, so a report says
// WHICH value was stored and not merely that one was.
func tsRender(v lpg.PropertyValue, present bool) string {
	if !present {
		return "<absent>"
	}
	switch v.Kind() {
	case lpg.PropString:
		s, _ := v.String()
		return fmt.Sprintf("STRING(%q)", s)
	case lpg.PropInt64:
		i, _ := v.Int64()
		return fmt.Sprintf("INTEGER(%d)", i)
	case lpg.PropFloat64:
		f, _ := v.Float64()
		return fmt.Sprintf("FLOAT(%g)", f)
	case lpg.PropBool:
		b, _ := v.Bool()
		return fmt.Sprintf("BOOLEAN(%t)", b)
	default:
		return fmt.Sprintf("%s(<%v>)", tsKindName(v.Kind()), v)
	}
}

// -----------------------------------------------------------------------------
// The verdict model — computed from the declaration table, never from the schema
// -----------------------------------------------------------------------------

// tsVerdict is the predicted or observed outcome of ONE property write through
// a validator-consulting path.
type tsVerdict uint8

// The write verdicts. They are three distinct refusals plus acceptance, and the
// battery adjudicates WHICH one occurred: a clause that accepted any error
// would pass while the wrong sentinel was raised.
const (
	// tsAccept — the value's kind matches the declared kind, so the write lands.
	tsAccept tsVerdict = iota
	// tsRejectTypeMismatch — [schema.ErrTypeMismatch]: the key is declared and
	// the value's kind disagrees.
	tsRejectTypeMismatch
	// tsRejectUnknownProperty — [schema.ErrUnknownProperty]: the key was never
	// registered.
	tsRejectUnknownProperty
	// tsVerdictCount bounds the enum for the coverage matrix.
	tsVerdictCount
)

// String renders a verdict for reports and for the coverage matrix.
func (v tsVerdict) String() string {
	switch v {
	case tsAccept:
		return "accept"
	case tsRejectTypeMismatch:
		return "reject:type-mismatch"
	case tsRejectUnknownProperty:
		return "reject:unknown-property"
	default:
		return fmt.Sprintf("tsVerdict(%d)", uint8(v))
	}
}

// tsNodeVerdict is the predicted or observed outcome of ONE
// [lpg.Graph.ValidateNode] call. It is a SEPARATE enum from [tsVerdict] because
// the whole-node hook can raise a sentinel the per-value hook structurally
// cannot ([schema.ErrMissingRequired]), and folding them into one enum would
// leave cells in the write-coverage matrix that no write path can ever reach.
type tsNodeVerdict uint8

// The whole-node verdicts.
const (
	tsNodeOK tsNodeVerdict = iota
	tsNodeMissingRequired
	tsNodeTypeMismatch
	tsNodeVerdictCount
)

// String renders a whole-node verdict for reports.
func (v tsNodeVerdict) String() string {
	switch v {
	case tsNodeOK:
		return "ok"
	case tsNodeMissingRequired:
		return "reject:missing-required"
	case tsNodeTypeMismatch:
		return "reject:type-mismatch"
	default:
		return fmt.Sprintf("tsNodeVerdict(%d)", uint8(v))
	}
}

// typedSchemaModel is the independent verdict oracle: the declaration table,
// and nothing else. It answers what the validator MUST decide, computed without
// consulting the validator.
//
// # Concurrency contract
//
// typedSchemaModel is immutable after construction and safe for concurrent
// reads. The scenario drives it from the single simulation goroutine.
type typedSchemaModel struct {
	kinds    map[string]lpg.PropertyKind
	required map[string][]string
}

// newTypedSchemaModel builds the model over a declaration table and a
// label→required-properties map.
func newTypedSchemaModel(decls []tsDecl, required map[string][]string) *typedSchemaModel {
	m := &typedSchemaModel{
		kinds:    make(map[string]lpg.PropertyKind, len(decls)),
		required: make(map[string][]string, len(required)),
	}
	for _, d := range decls {
		m.kinds[d.key] = d.kind
	}
	for label, props := range required {
		cp := make([]string, len(props))
		copy(cp, props)
		sort.Strings(cp)
		m.required[label] = cp
	}
	return m
}

// predictWrite is the model's verdict for writing value under key. It mirrors
// [schema.Schema.Validate]'s CONTRACT, derived from the same declaration table
// the schema was built from, and calls nothing in graph/lpg/schema.
func (m *typedSchemaModel) predictWrite(key string, value lpg.PropertyValue) tsVerdict {
	declared, ok := m.kinds[key]
	if !ok {
		return tsRejectUnknownProperty
	}
	if value.Kind() != declared {
		return tsRejectTypeMismatch
	}
	return tsAccept
}

// predictNode is the model's verdict for a finalised node carrying labels and
// props. It mirrors [schema.Schema.ValidateNode]'s contract: required-property
// existence FIRST (per label, in sorted order so the answer is deterministic),
// then the kind re-check over the properties that ARE present, with an
// unregistered key passing through.
func (m *typedSchemaModel) predictNode(labels []string, props map[string]lpg.PropertyValue) tsNodeVerdict {
	sorted := make([]string, len(labels))
	copy(sorted, labels)
	sort.Strings(sorted)
	for _, label := range sorted {
		for _, want := range m.required[label] {
			if _, present := props[want]; !present {
				return tsNodeMissingRequired
			}
		}
	}
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		declared, ok := m.kinds[k]
		if !ok {
			continue
		}
		if props[k].Kind() != declared {
			return tsNodeTypeMismatch
		}
	}
	return tsNodeOK
}

// tsClassify maps an observed error to a [tsVerdict], reporting false when the
// error matches no declared sentinel — which is itself a finding, never a
// silent pass.
func tsClassify(err error) (tsVerdict, bool) {
	switch {
	case err == nil:
		return tsAccept, true
	case errors.Is(err, schema.ErrTypeMismatch):
		return tsRejectTypeMismatch, true
	case errors.Is(err, schema.ErrUnknownProperty):
		return tsRejectUnknownProperty, true
	default:
		return tsAccept, false
	}
}

// tsClassifyNode maps an observed [lpg.Graph.ValidateNode] error to a
// [tsNodeVerdict], reporting false for an unrecognised error.
func tsClassifyNode(err error) (tsNodeVerdict, bool) {
	switch {
	case err == nil:
		return tsNodeOK, true
	case errors.Is(err, schema.ErrMissingRequired):
		return tsNodeMissingRequired, true
	case errors.Is(err, schema.ErrTypeMismatch):
		return tsNodeTypeMismatch, true
	default:
		return tsNodeOK, false
	}
}

// -----------------------------------------------------------------------------
// The write paths
// -----------------------------------------------------------------------------

// tsPath names one of the five write paths that consult the installed
// [lpg.SchemaValidator]. Every one is exercised, because the four property
// stores behind them are genuinely different stores (see the file header).
type tsPath uint8

// The five hook sites, verified in source.
const (
	// tsPathNodeProp is [lpg.Graph.SetNodeProperty] — graph/lpg/property.go.
	tsPathNodeProp tsPath = iota
	// tsPathEdgePairProp is [lpg.Graph.SetEdgeProperty], the COLUMNAR per-pair
	// write that goes through the adjacency entry's aux column —
	// graph/lpg/edge_property.go.
	tsPathEdgePairProp
	// tsPathEdgeHandleProp is [lpg.Graph.SetEdgePropertyByHandle] —
	// graph/lpg/edge_handle.go.
	tsPathEdgeHandleProp
	// tsPathEdgeInstanceProp is [lpg.Graph.SetEdgePropertyAt] —
	// graph/lpg/edge_instance_props.go.
	tsPathEdgeInstanceProp
	// tsPathFusedAddEdge is [lpg.Graph.AddEdgeLabeledWithProperty], the fused
	// create-and-set the bulk builders use — graph/lpg/lpg.go.
	tsPathFusedAddEdge
	// tsPathCount bounds the enum for the coverage matrix.
	tsPathCount
)

// String renders a path for reports and for the coverage matrix.
func (p tsPath) String() string {
	switch p {
	case tsPathNodeProp:
		return "SetNodeProperty"
	case tsPathEdgePairProp:
		return "SetEdgeProperty"
	case tsPathEdgeHandleProp:
		return "SetEdgePropertyByHandle"
	case tsPathEdgeInstanceProp:
		return "SetEdgePropertyAt"
	case tsPathFusedAddEdge:
		return "AddEdgeLabeledWithProperty"
	default:
		return fmt.Sprintf("tsPath(%d)", uint8(p))
	}
}

// tsPerturb names a TEST-ONLY perturbation of ONE observed side of ONE clause,
// so a test can prove the clause FIRES rather than merely that it is silent.
//
// It is a PARAMETER, threaded from the caller to the observation site, and never
// a package-level variable: that shape is a data race by construction and
// internal/sim/global_state_guard_test.go fails the build if it reappears
// (rmp #2597).
//
// The mutation perturbations do not fake a mismatch. They reproduce the OUTPUT a
// MISSING validator hook would produce, by performing the very same write with
// the validator momentarily uninstalled — so a test using one proves the clause
// catches the real defect shape, not merely that two unequal values compare
// unequal.
type tsPerturb uint8

// The perturbations. tsPerturbNone is what the scenario always passes.
const (
	tsPerturbNone tsPerturb = iota
	// tsPerturbFlipVerdict inverts the OBSERVED verdict of a write, so the
	// accept/reject adjudication clause is proven to fire in both directions
	// (an accepted write reported as a rejection and vice versa).
	tsPerturbFlipVerdict
	// tsPerturbApplyRejected lands a REJECTED value by repeating the write with
	// the validator uninstalled — exactly what the store would hold if the hook
	// were absent from that path. It proves the no-partial-mutation clause fires.
	tsPerturbApplyRejected
	// tsPerturbInternGhostKey interns the unregistered key into the graph's
	// property-key registry after a rejected write, reproducing a hook that ran
	// AFTER the intern rather than before it. It proves the non-interning clause
	// fires.
	tsPerturbInternGhostKey
	// tsPerturbNodePrefill writes the required property BEFORE the mid-build
	// ValidateNode observation, so the "a mid-build node is legitimately
	// rejected" clause is proven to fire.
	tsPerturbNodePrefill
	// tsPerturbNodeSkipRequired omits the required property before the FINALISED
	// ValidateNode observation, so the "a finalised node is accepted" clause is
	// proven to fire.
	tsPerturbNodeSkipRequired
	// tsPerturbRepairPreInstall repairs the pre-installation wrong-kind value, so
	// the clause that ValidateNode's kind re-check catches it is proven to fire.
	tsPerturbRepairPreInstall
	// tsPerturbRecoveryPreinstall installs a validator on the recovered graph
	// BEFORE the post-recovery pin probes it, so the "a reopened graph carries no
	// validator" clause is proven to fire.
	tsPerturbRecoveryPreinstall
	// tsPerturbWitnessPoison lands the REJECTED witness value with the validator
	// uninstalled before the post-recovery read, so the "a rejected value is
	// absent after recovery" clause is proven to fire.
	tsPerturbWitnessPoison
)

// -----------------------------------------------------------------------------
// The side fixture (arms A, B, C)
// -----------------------------------------------------------------------------

// tsPair is one handle-bearing edge of the side fixture.
type tsPair struct {
	src, dst string
	handle   uint64
}

// typedSchemaSide is the graph the direct-API arms drive: a fixture this
// scenario OWNS, with a [schema.Schema] installed as its validator.
//
// It is deliberately NOT the durable store's graph. A direct lpg write does not
// reach the WAL, so writing on the store's graph would create nodes the
// [GraphOracle] does not model — breaking the harness's count parity — and a
// checkpoint would then make them durable. See the file header.
//
// # Concurrency contract
//
// typedSchemaSide is NOT safe for concurrent use; it is driven from the single
// simulation goroutine.
type typedSchemaSide struct {
	g     *lpg.Graph[string, float64]
	sc    *schema.Schema
	model *typedSchemaModel
	nodes []string
	pairs []tsPair
	// shadow is the harness's own record of what each (store, target, key) slot
	// holds, so "a rejected write changed nothing" is checked against a value
	// this file wrote rather than against a value read back from the store it is
	// auditing.
	shadow map[string]lpg.PropertyValue
	// fusedSeq and buildSeq mint fresh, never-reused names for the fused-path
	// endpoints and for the per-battery ValidateNode build sequence.
	fusedSeq int
	buildSeq int
}

// tsShadowKey builds the shadow map's key for one property slot.
func tsShadowKey(store, target, key string) string { return store + "|" + target + "|" + key }

// newTypedSchemaSide builds the side fixture: a chain of handle-bearing edges,
// the three permanent ValidateNode fixture nodes, and then — and only then — the
// installed schema.
//
// The ORDER is load-bearing. [tsPreInstallNode]'s wrong-kind value must be
// written BEFORE [lpg.Graph.SetValidator], because with the validator installed
// no write can produce it; it is the only way, short of the recovery bypass, for
// a graph to legitimately hold a value the schema forbids, and it is what makes
// ValidateNode's kind re-check over PRESENT properties reachable at all.
func newTypedSchemaSide(nodes int) (*typedSchemaSide, error) {
	// A MULTIGRAPH: the per-handle and per-instance stores are addressed by
	// handle and by CREATE ordinal, which a simple graph never mints more than
	// one of, and the fused path adds an edge per call.
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})

	s := &typedSchemaSide{
		g:      g,
		shadow: make(map[string]lpg.PropertyValue),
	}
	s.nodes = make([]string, 0, nodes)
	for i := 0; i < nodes; i++ {
		name := fmt.Sprintf("ts-n%d", i)
		if err := g.AddNode(name); err != nil {
			return nil, fmt.Errorf("sim: typed-schema side AddNode %q: %w", name, err)
		}
		s.nodes = append(s.nodes, name)
	}
	s.pairs = make([]tsPair, 0, len(s.nodes))
	for i := 0; i+1 < len(s.nodes); i++ {
		h, err := g.AddEdgeH(s.nodes[i], s.nodes[i+1], 1)
		if err != nil {
			return nil, fmt.Errorf("sim: typed-schema side AddEdgeH %q->%q: %w",
				s.nodes[i], s.nodes[i+1], err)
		}
		if h == 0 {
			return nil, fmt.Errorf("sim: typed-schema side AddEdgeH %q->%q allocated handle 0",
				s.nodes[i], s.nodes[i+1])
		}
		s.pairs = append(s.pairs, tsPair{src: s.nodes[i], dst: s.nodes[i+1], handle: h})
	}

	// --- the three permanent ValidateNode fixture nodes, built UNVALIDATED. ---
	if err := g.AddNode(tsPreInstallNode); err != nil {
		return nil, fmt.Errorf("sim: typed-schema pre-install node: %w", err)
	}
	if err := g.SetNodeLabel(tsPreInstallNode, tsSideLabel); err != nil {
		return nil, fmt.Errorf("sim: typed-schema pre-install label: %w", err)
	}
	if err := g.SetNodeProperty(tsPreInstallNode, tsKeyStr, lpg.StringValue("pre-install")); err != nil {
		return nil, fmt.Errorf("sim: typed-schema pre-install required prop: %w", err)
	}
	// The forbidden value. tsKeyInt is declared PropInt64 below; a STRING here is
	// only writable because no validator is installed yet.
	if err := g.SetNodeProperty(tsPreInstallNode, tsKeyInt, lpg.StringValue("wrong-kind")); err != nil {
		return nil, fmt.Errorf("sim: typed-schema pre-install forbidden prop: %w", err)
	}
	if err := g.AddNode(tsBareNode); err != nil {
		return nil, fmt.Errorf("sim: typed-schema bare node: %w", err)
	}
	// tsGhostNode is deliberately NOT created.

	// --- the schema, and only now the validator. ---
	sc, model, err := typedSchemaInstall(g, typedSchemaDecls(),
		map[string][]string{tsSideLabel: {tsKeyStr}})
	if err != nil {
		return nil, err
	}
	s.sc, s.model = sc, model
	return s, nil
}

// typedSchemaInstall registers a declaration table on a fresh [schema.Schema]
// bound to g's OWN registries, installs it as g's validator, and returns it
// alongside the model built from the same table.
//
// The registries are g's because [schema.New] documents that requirement: the
// schema mints property-key and label ids through them, so a schema built over
// another graph's registries would hand out ids that do not describe this graph.
// That is why a recovered graph gets a FRESH schema rather than the old one
// re-installed (see [typedSchemaProbes.postRecovery]).
func typedSchemaInstall(
	g *lpg.Graph[string, float64], decls []tsDecl, required map[string][]string,
) (*schema.Schema, *typedSchemaModel, error) {
	sc := schema.New(g.Registry(), g.PropertyKeys())
	for _, d := range decls {
		if _, err := sc.RegisterProperty(d.key, d.kind); err != nil {
			return nil, nil, fmt.Errorf("sim: typed-schema RegisterProperty %q: %w", d.key, err)
		}
	}
	labels := make([]string, 0, len(required))
	for label := range required {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	for _, label := range labels {
		sc.RegisterLabel(label)
		props := make([]string, len(required[label]))
		copy(props, required[label])
		sort.Strings(props)
		for _, p := range props {
			sc.RequireProperty(label, p)
		}
	}
	g.SetValidator(sc)
	return sc, newTypedSchemaModel(decls, required), nil
}

// tsOpSpec is one fully-determined side-battery write: which path, which
// verdict the model must predict, and the (key, value) pair that produces it.
//
// It is a value a caller can construct, which is the seam the falsifiability
// tests drive: the scenario SWEEPS every (path, verdict) cell, and a test can
// pin one cell and one perturbation instead of hoping the draws reach it.
type tsOpSpec struct {
	key   string
	value lpg.PropertyValue
	path  tsPath
	want  tsVerdict
}

// tsValueFor returns a value whose kind MATCHES kind, drawn from seed so the
// stored values differ tick to tick while the verdict stays fixed.
func tsValueFor(kind lpg.PropertyKind, seed *Seed) lpg.PropertyValue {
	switch kind {
	case lpg.PropString:
		return lpg.StringValue(fmt.Sprintf("v%d", seed.IntN(1_000_000)))
	case lpg.PropInt64:
		return lpg.Int64Value(int64(seed.IntN(1_000_000)))
	case lpg.PropFloat64:
		return lpg.Float64Value(float64(seed.IntN(1_000_000)) / 8)
	case lpg.PropBool:
		return lpg.BoolValue(seed.Bool(0.5))
	default:
		// Unreachable for this scenario's declaration table; a value of an
		// undeclared kind would be a harness bug, so make it loud rather than
		// silently plausible.
		return lpg.StringValue("<undeclared-kind>")
	}
}

// tsSpecFor builds the spec for one (path, verdict) cell, drawing the value
// from seed.
//
// The three verdicts are constructed, never hoped for: an ACCEPT uses a declared
// key with a matching value, a TYPE MISMATCH uses a declared key with a value of
// a DIFFERENT declared kind (so the mismatch never depends on an exotic kind
// being available), and an UNKNOWN PROPERTY uses [tsKeyGhost], which no code
// path in this scenario ever registers.
func tsSpecFor(path tsPath, want tsVerdict, seed *Seed) tsOpSpec {
	decls := typedSchemaDecls()
	d := decls[seed.IntN(len(decls))]
	switch want {
	case tsAccept:
		return tsOpSpec{path: path, want: want, key: d.key, value: tsValueFor(d.kind, seed)}
	case tsRejectTypeMismatch:
		other := decls[seed.IntN(len(decls))]
		for other.kind == d.kind {
			other = decls[(seed.IntN(len(decls))+1)%len(decls)]
		}
		return tsOpSpec{path: path, want: want, key: d.key, value: tsValueFor(other.kind, seed)}
	default:
		return tsOpSpec{path: path, want: tsRejectUnknownProperty, key: tsKeyGhost,
			value: tsValueFor(d.kind, seed)}
	}
}

// tsCells returns the fifteen (path, verdict) cells in a seed-shuffled order.
//
// The SWEEP is why the coverage gate is structural: each epoch visits every cell
// exactly once, so "every path was exercised in every verdict" is a property of
// the loop rather than a hope about the draws. The seed decides the ORDER and the
// values; it does not decide the coverage.
func tsCells(seed *Seed) []tsOpSpec {
	type cell struct {
		path tsPath
		want tsVerdict
	}
	cells := make([]cell, 0, int(tsPathCount)*int(tsVerdictCount))
	for p := tsPath(0); p < tsPathCount; p++ {
		for v := tsVerdict(0); v < tsVerdictCount; v++ {
			cells = append(cells, cell{path: p, want: v})
		}
	}
	// Fisher-Yates over the cell list. [Seed.Shuffle] shuffles strings only, and
	// encoding a (path, verdict) pair as a string to reuse it would be worse than
	// four lines of the same algorithm.
	for i := len(cells) - 1; i > 0; i-- {
		j := seed.IntN(i + 1)
		cells[i], cells[j] = cells[j], cells[i]
	}
	out := make([]tsOpSpec, 0, len(cells))
	for _, c := range cells {
		out = append(out, tsSpecFor(c.path, c.want, seed))
	}
	return out
}

// -----------------------------------------------------------------------------
// Targets and readers — one per property store
// -----------------------------------------------------------------------------

// tsTarget is where a spec's write lands: the store it addresses, the entities
// that address it, and the shadow-store name used in reports.
//
// It is passed by POINTER throughout. It is an immutable descriptor that several
// helpers read on every write, and at 96 bytes copying it per call is waste the
// project's efficiency mandate does not accept for a value nothing mutates.
type tsTarget struct {
	store  string
	node   string
	src    string
	dst    string
	rel    string
	handle uint64
	idx    int64
}

// label renders the target for a violation message.
func (t *tsTarget) label() string {
	if t.node != "" {
		return t.store + "[" + t.node + "]"
	}
	return fmt.Sprintf("%s[%s->%s h=%d i=%d]", t.store, t.src, t.dst, t.handle, t.idx)
}

// shadowKey is the target's key in [typedSchemaSide.shadow].
func (t *tsTarget) shadowKey(key string) string {
	if t.node != "" {
		return tsShadowKey(t.store, t.node, key)
	}
	return tsShadowKey(t.store, fmt.Sprintf("%s->%s|%d|%d", t.src, t.dst, t.handle, t.idx), key)
}

// tsRelType is the relationship type the fused path stamps. It is a constant so
// the fused arm's edges are distinguishable from the fixture chain's in a dump.
const tsRelType = "TSFUSED"

// resolveTarget picks the target for spec, drawing the entity from seed. The
// FUSED path always mints a FRESH destination node, because that is the only
// shape in which its all-or-nothing contract is observable: on an accept the
// node and the edge both appear, and on a reject neither may.
func (s *typedSchemaSide) resolveTarget(spec tsOpSpec, seed *Seed) tsTarget {
	switch spec.path {
	case tsPathNodeProp:
		return tsTarget{store: "node-props", node: s.nodes[seed.IntN(len(s.nodes))]}
	case tsPathEdgePairProp:
		p := s.pairs[seed.IntN(len(s.pairs))]
		return tsTarget{store: "edge-pair-props", src: p.src, dst: p.dst}
	case tsPathEdgeHandleProp:
		p := s.pairs[seed.IntN(len(s.pairs))]
		return tsTarget{store: "edge-handle-props", src: p.src, dst: p.dst, handle: p.handle}
	case tsPathEdgeInstanceProp:
		p := s.pairs[seed.IntN(len(s.pairs))]
		// Two ordinals, so the per-instance store is addressed at more than one
		// index over the run rather than always at 1.
		return tsTarget{store: "edge-instance-props", src: p.src, dst: p.dst, idx: int64(1 + seed.IntN(2))}
	default:
		s.fusedSeq++
		return tsTarget{
			store: "edge-pair-props",
			src:   s.nodes[seed.IntN(len(s.nodes))],
			dst:   fmt.Sprintf("ts-f%d", s.fusedSeq),
			rel:   tsRelType,
		}
	}
}

// write performs spec's write through spec.path and returns the path's error
// verbatim, so the caller classifies the SENTINEL rather than a re-wrapped one.
func (s *typedSchemaSide) write(t *tsTarget, spec tsOpSpec) error {
	switch spec.path {
	case tsPathNodeProp:
		return s.g.SetNodeProperty(t.node, spec.key, spec.value)
	case tsPathEdgePairProp:
		return s.g.SetEdgeProperty(t.src, t.dst, spec.key, spec.value)
	case tsPathEdgeHandleProp:
		return s.g.SetEdgePropertyByHandle(t.src, t.dst, t.handle, spec.key, spec.value)
	case tsPathEdgeInstanceProp:
		return s.g.SetEdgePropertyAt(t.src, t.dst, t.idx, spec.key, spec.value)
	default:
		return s.g.AddEdgeLabeledWithProperty(t.src, t.dst, 1, t.rel, spec.key, spec.value)
	}
}

// read returns what the target's CANONICAL accessor reports for key.
func (s *typedSchemaSide) read(t *tsTarget, path tsPath, key string) (lpg.PropertyValue, bool) {
	switch path {
	case tsPathNodeProp:
		return s.g.GetNodeProperty(t.node, key)
	case tsPathEdgeHandleProp:
		v, ok := s.g.EdgePropertiesByHandle(t.src, t.dst, t.handle)[key]
		return v, ok
	case tsPathEdgeInstanceProp:
		v, ok := s.g.EdgePropertiesAt(t.src, t.dst, t.idx)[key]
		return v, ok
	default:
		// Both the columnar per-pair path and the fused path land in the per-pair
		// aux column, so both read through GetEdgeProperty.
		return s.g.GetEdgeProperty(t.src, t.dst, key)
	}
}

// readCross returns what a SECOND accessor reports for key, and whether such an
// accessor exists for this store.
//
// It exists for the columnar per-pair store because that store has two public
// readers — the scalar [lpg.Graph.GetEdgeProperty] and the map-shaped
// [lpg.Graph.EdgeProperties] — and a rejected write that changed one but not the
// other would be a mutation this battery must catch. The per-handle and
// per-instance stores are read map-shaped only, and the node store's map reader
// is [lpg.Graph.NodePropertiesByID], which arm C already drives.
func (s *typedSchemaSide) readCross(t *tsTarget, path tsPath, key string) (lpg.PropertyValue, bool, bool) {
	if path != tsPathEdgePairProp && path != tsPathFusedAddEdge {
		return lpg.PropertyValue{}, false, false
	}
	v, ok := s.g.EdgeProperties(t.src, t.dst)[key]
	return v, ok, true
}

// -----------------------------------------------------------------------------
// Evidence
// -----------------------------------------------------------------------------

// TypedSchemaEvidence is what the run MEASURED, handed back so a test asserts on
// numbers rather than on the mere absence of a violation, and so the report
// prints what actually happened.
//
// Following the shape [FluentQueryEvidence] uses, the checker itself IS the
// record: the non-vacuity gates in [TypedSchemaEvidence.Finish] read these very
// fields, so a test asserting on them cannot drift from what the gates enforce.
type TypedSchemaEvidence struct {
	// Coverage[path][verdict] counts how many side-battery writes landed in each
	// of the fifteen cells. Every cell is visited once per sweep epoch, so a zero
	// means the sweep did not complete even one epoch.
	Coverage [tsPathCount][tsVerdictCount]int
	// NoMutationChecks[path] counts the rejections whose no-mutation battery ran
	// on that path, and AcceptLandedChecks[path] the accepts whose read-back was
	// verified.
	NoMutationChecks   [tsPathCount]int
	AcceptLandedChecks [tsPathCount]int
	// CrossAccessorChecks counts the rejections whose SECOND per-pair accessor was
	// also compared, and KeyInterningChecks the rejections that asserted the
	// unregistered key stayed out of the property-key registry.
	CrossAccessorChecks int
	KeyInterningChecks  int
	// FusedNoEdgeChecks counts the rejected fused writes that asserted no edge and
	// no endpoint node appeared.
	FusedNoEdgeChecks int
	// NodeVerdicts[verdict] counts the whole-node observations by outcome, and
	// the four Validate* counters the individual fixture clauses. They are
	// separate because a single "ValidateNode ran" counter cannot distinguish a
	// run that only ever saw the OK case.
	NodeVerdicts       [tsNodeVerdictCount]int
	ValidateMidBuild   int
	ValidateFinalised  int
	ValidateUnlabelled int
	ValidateGhost      int
	ValidatePreInstall int
	// EngineVerdicts[verdict] counts the durable (Cypher-path) writes by outcome,
	// and EngineRejectedCreateChecks the rejected CREATEs whose no-node clause ran.
	EngineVerdicts             [tsVerdictCount]int
	EngineRejectedCreateChecks int
	// WitnessesArmed is how many (accepted value, then rejected value) witness
	// pairs the run committed, and WitnessReadsAfterRecovery how many witness
	// verifications ran on a graph that came back through real recovery.
	// WitnessCypherReads and WitnessSubstrateReads count the two INDEPENDENT
	// channels each verification used, so a channel that silently stopped
	// answering is visible rather than absorbed.
	WitnessesArmed            int
	WitnessReadsAfterRecovery int
	WitnessCypherReads        int
	WitnessSubstrateReads     int
	// The post-recovery pin's five clauses, counted separately: a single counter
	// could not tell a run that only reached the first from one that reached all.
	PinNoValidatorAccepted  int
	PinNoValidatorNodeClean int
	PinReinstalledRejected  int
	PinValidateNodeDetected int
	PinValidateNodeRepaired int
	// Crashes / Checkpoints / ForcedCrashes are the recovery coverage the pin and
	// witness clauses depend on.
	Crashes       int
	Checkpoints   int
	ForcedCrashes int
	// The pure-store arm's measurements (the finding in the file header):
	// PureStoreArms is how many times it ran, PureStoreCommitNotApplied how often
	// Commit reported [txn.ErrCommittedNotApplied] wrapping the schema sentinel,
	// PureStoreLiveAbsent how often the LIVE graph was left without the rejected
	// value, PureStoreResurrected how often recovery brought it back, and
	// PureStoreAcceptedSurvived how often the ACCEPTED sibling value survived (the
	// non-vacuity half: without it, "absent" could just mean recovery did
	// nothing).
	PureStoreArms             int
	PureStoreCommitNotApplied int
	PureStoreLiveAbsent       int
	PureStoreResurrected      int
	PureStoreAcceptedSurvived int
	// SideBatteries and NodeBatteries are how many times each battery ran.
	SideBatteries int
	NodeBatteries int
	// Digest folds every clause's (tick, clause, verdicts) triple. It is the
	// scenario's reproducibility claim: same seed, same digest. It folds no
	// NodeID and no mapper key, both of which come from a process-global counter
	// and are not a function of the seed.
	Digest uint64
}

// String renders the evidence for a report and for the run's own output.
func (e *TypedSchemaEvidence) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "sideBatteries=%d nodeBatteries=%d crashes=%d (forced=%d) checkpoints=%d",
		e.SideBatteries, e.NodeBatteries, e.Crashes, e.ForcedCrashes, e.Checkpoints)
	fmt.Fprintf(&b, " coverage=%s", e.coverageString())
	fmt.Fprintf(&b, " noMutation=%v acceptLanded=%v cross=%d keyIntern=%d fusedNoEdge=%d",
		e.NoMutationChecks, e.AcceptLandedChecks, e.CrossAccessorChecks,
		e.KeyInterningChecks, e.FusedNoEdgeChecks)
	fmt.Fprintf(&b, " validate(mid=%d final=%d bare=%d ghost=%d preinstall=%d) nodeVerdicts=%v",
		e.ValidateMidBuild, e.ValidateFinalised, e.ValidateUnlabelled, e.ValidateGhost,
		e.ValidatePreInstall, e.NodeVerdicts)
	fmt.Fprintf(&b, " engine=%v rejectedCreate=%d", e.EngineVerdicts, e.EngineRejectedCreateChecks)
	fmt.Fprintf(&b, " witness(armed=%d postRecovery=%d cypher=%d substrate=%d)",
		e.WitnessesArmed, e.WitnessReadsAfterRecovery, e.WitnessCypherReads, e.WitnessSubstrateReads)
	fmt.Fprintf(&b, " pin(noValidator=%d nodeClean=%d rejected=%d detected=%d repaired=%d)",
		e.PinNoValidatorAccepted, e.PinNoValidatorNodeClean, e.PinReinstalledRejected,
		e.PinValidateNodeDetected, e.PinValidateNodeRepaired)
	fmt.Fprintf(&b, " pureStore(arms=%d notApplied=%d liveAbsent=%d RESURRECTED=%d acceptedSurvived=%d)",
		e.PureStoreArms, e.PureStoreCommitNotApplied, e.PureStoreLiveAbsent,
		e.PureStoreResurrected, e.PureStoreAcceptedSurvived)
	fmt.Fprintf(&b, " digest=%#016x", e.Digest)
	return b.String()
}

// ReproducibleSummary renders exactly the fields that are a pure function of the
// seed — which here is every field, because nothing this scenario measures
// depends on a background goroutine. It exists so the determinism test compares
// what the scenario CLAIMS is reproducible, and so a future field that is NOT
// reproducible has an obvious place to be excluded from.
func (e *TypedSchemaEvidence) ReproducibleSummary() string { return e.String() }

// coverageString renders the fifteen-cell matrix compactly and deterministically.
func (e *TypedSchemaEvidence) coverageString() string {
	var b strings.Builder
	b.WriteByte('[')
	for p := tsPath(0); p < tsPathCount; p++ {
		if p > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%d:%v", uint8(p), e.Coverage[p])
	}
	b.WriteByte(']')
	return b.String()
}

// Finish is the terminal assert-something-was-seen gate: it reports a violation
// for every arm, cell and clause the run did NOT reach.
//
// It is deliberately unconditional on the configuration — it fires just as
// loudly when a budget was lowered as when a clause was deleted — so it cannot
// be silenced by the very change it exists to catch.
func (e *TypedSchemaEvidence) Finish(tick int64) []Violation {
	var vs []Violation
	add := func(clause, format string, args ...any) {
		vs = append(vs, tsViolation(ViolationVacuousRun, tick, "vacuity:"+clause, format, args...))
	}
	if e.SideBatteries == 0 {
		add("side-batteries", "the side write battery never ran: no accept/reject verdict in this "+
			"scenario was adjudicated at all")
		return vs
	}
	for p := tsPath(0); p < tsPathCount; p++ {
		for v := tsVerdict(0); v < tsVerdictCount; v++ {
			if e.Coverage[p][v] == 0 {
				add("coverage", "the (%s, %s) cell was never exercised. The sweep visits every one of "+
					"the %d cells once per epoch, so a zero here means the tick budget did not complete "+
					"one epoch — not that the draws were unlucky", p, v, int(tsPathCount)*int(tsVerdictCount))
			}
		}
		if e.NoMutationChecks[p] == 0 {
			add("no-mutation", "no rejected write on %s ever had its no-mutation battery run, so "+
				"\"the write returned an error\" was never separated from \"the write changed nothing\" "+
				"on that path", p)
		}
		if e.AcceptLandedChecks[p] == 0 {
			add("accept-landed", "no accepted write on %s was ever read back, so the path's accept arm "+
				"proved only that no error was returned", p)
		}
	}
	if e.CrossAccessorChecks == 0 {
		add("cross-accessor", "the columnar per-pair store's SECOND accessor (Graph.EdgeProperties) was "+
			"never compared against the scalar one, so a rejection that changed one view and not the "+
			"other would have passed")
	}
	if e.KeyInterningChecks == 0 {
		add("key-interning", "no rejected write ever asserted that the unregistered key %q stayed out "+
			"of the property-key registry, which is the clause that distinguishes a hook running BEFORE "+
			"the intern from one running after it", tsKeyGhost)
	}
	if e.FusedNoEdgeChecks == 0 {
		add("fused-no-edge", "no rejected AddEdgeLabeledWithProperty ever asserted that neither the edge "+
			"nor its fresh endpoint appeared, so the fused path's all-or-nothing contract was not checked")
	}
	if e.NodeBatteries == 0 {
		add("node-batteries", "the ValidateNode boundary battery never ran, so required-property "+
			"enforcement — which Graph.ValidateNode is the ONLY caller of, and which the engine never "+
			"invokes — was not adjudicated at all")
	}
	for v := tsNodeVerdict(0); v < tsNodeVerdictCount; v++ {
		if e.NodeVerdicts[v] == 0 {
			add("node-verdict", "ValidateNode never returned %s, so that arm of the whole-node hook was "+
				"never observed", v)
		}
	}
	// Two independent ifs, not a switch: a gate that reported only the FIRST
	// missing clause would hide the second, and this file's whole point is that a
	// clause nobody evaluated must say so.
	if e.ValidateMidBuild == 0 {
		add("validate-mid-build", "the mid-build clause never ran: nothing asserted that a node carrying "+
			"its label but not yet its required property is LEGITIMATELY rejected")
	}
	if e.ValidateFinalised == 0 {
		add("validate-finalised", "the finalised clause never ran, so the mid-build rejection was never "+
			"paired with the acceptance that makes it meaningful")
	}
	if e.ValidateUnlabelled == 0 {
		add("validate-unlabelled", "the unlabelled control never ran, so \"a requirement applies\" was "+
			"never separated from \"ValidateNode always refuses\"")
	}
	if e.ValidateGhost == 0 {
		add("validate-ghost", "the never-interned-node case never ran, so ValidateNode's documented "+
			"benign nothing-to-check exit was not exercised")
	}
	if e.ValidatePreInstall == 0 {
		add("validate-pre-install", "the pre-installation fixture clause never ran, so ValidateNode's "+
			"kind RE-CHECK over already-present properties — the only branch a per-value hook "+
			"structurally cannot reach — was never exercised")
	}
	for v := tsVerdict(0); v < tsVerdictCount; v++ {
		if e.EngineVerdicts[v] == 0 {
			add("engine-verdict", "the durable Cypher path never produced %s, so the validator was not "+
				"shown to sit on the engine's write path in that direction", v)
		}
	}
	if e.EngineRejectedCreateChecks == 0 {
		add("engine-rejected-create", "no rejected CREATE ever had its no-node clause run, so the "+
			"statement-level rollback that keeps a refused CREATE out of the node count was not checked")
	}
	if e.Crashes == 0 {
		add("crashes", "the run performed NO crash+recovery cycle, so every recovery clause below was "+
			"unreachable. The loop forces one at the end precisely so this cannot depend on the seeded "+
			"schedule")
	}
	if e.Checkpoints == 0 {
		add("checkpoints", "the run published NO checkpoint, so no recovery crossed the SNAPSHOT "+
			"boundary and every post-recovery clause validated a pure WAL replay")
	}
	if e.WitnessesArmed == 0 {
		add("witnesses-armed", "no witness was armed, so no node ever carried an accepted value AND a "+
			"refused one before a crash")
	}
	if e.WitnessReadsAfterRecovery == 0 {
		add("witness-post-recovery", "no witness was ever read on a recovered graph, so \"the rejected "+
			"value never reached the WAL\" was never actually observed after a recovery (armed=%d)",
			e.WitnessesArmed)
	}
	if e.WitnessCypherReads == 0 || e.WitnessSubstrateReads == 0 {
		add("witness-channels", "the witness verification did not use BOTH independent channels "+
			"(cypher=%d substrate=%d): a single channel cannot distinguish a projection defect from a "+
			"stored-value defect", e.WitnessCypherReads, e.WitnessSubstrateReads)
	}
	if e.PinNoValidatorAccepted == 0 {
		add("pin-no-validator", "the recovery-asymmetry pin never ran, so the documented limitation — a "+
			"reopened graph carries NO validator, because the schema is not persisted with snapshots — "+
			"is asserted nowhere")
	}
	if e.PinNoValidatorNodeClean == 0 {
		add("pin-node-clean", "the pin never observed ValidateNode reporting clean on the recovered, "+
			"validator-less graph, so the second half of the asymmetry (the WHOLE-NODE hook is absent "+
			"too) was not pinned")
	}
	if e.PinReinstalledRejected == 0 {
		add("pin-reinstalled", "the pin never re-installed a schema and re-attempted the write, so "+
			"\"the write was accepted\" was never paired with the rejection that proves the write really "+
			"was forbidden")
	}
	if e.PinValidateNodeDetected == 0 || e.PinValidateNodeRepaired == 0 {
		add("pin-validate-node", "the pin did not exercise both directions of ValidateNode over the "+
			"planted value (detected=%d repaired=%d)", e.PinValidateNodeDetected, e.PinValidateNodeRepaired)
	}
	if e.PureStoreArms == 0 {
		add("pure-store", "the pure store/txn arm never ran, so the fsync-BEFORE-validate ordering — the "+
			"one path on which a rejected value becomes durable — was not adjudicated")
	} else {
		if e.PureStoreCommitNotApplied == 0 {
			add("pure-store-not-applied", "the pure store/txn arm never saw txn.ErrCommittedNotApplied, "+
				"so its precondition (a validator rejection during the post-fsync apply) was never "+
				"constructed")
		}
		if e.PureStoreAcceptedSurvived == 0 {
			add("pure-store-accepted", "the pure store/txn arm never confirmed its ACCEPTED sibling value "+
				"survived recovery, so any statement it makes about the rejected value is unattributable: "+
				"an absent value would be indistinguishable from a recovery that replayed nothing")
		}
	}
	return vs
}

// tsOp renders a clause name as a report op label.
func tsOp(clause string) string { return "<typed-schema:" + clause + ">" }

// tsViolation builds a violation for a clause.
func tsViolation(kind ViolationKind, tick int64, clause, format string, args ...any) Violation {
	return Violation{Kind: kind, Tick: tick, Op: tsOp(clause), Message: fmt.Sprintf(format, args...)}
}

// tsDigest folds a clause's identity and two small integers into the running
// digest. It reuses [genSwapMix] — the package's byte-at-a-time FNV-1a fold —
// rather than declaring a second identical mixer.
func tsDigest(h uint64, tick int64, clause string, a, b int) uint64 {
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

// TypedSchemaProbes is the stateful checker: it runs the write battery, the
// ValidateNode boundary battery and the post-recovery pin on demand, and
// accumulates the evidence its terminal non-vacuity gate reads.
//
// # Concurrency contract
//
// TypedSchemaProbes is NOT safe for concurrent use. It draws from a [Seed] and
// issues reads that need a quiescent view of the graph, so it must be driven
// from the single simulation goroutine — the same contract [CheckSearch] and
// [FluentQueryProbes] carry, and for the same reason.
type TypedSchemaProbes struct {
	seed *Seed
	ev   *TypedSchemaEvidence
	// pending is the seed-shuffled remainder of the current side sweep epoch, and
	// pendingEngine the same for the durable arm's three verdicts. When one
	// empties a fresh epoch is drawn, which is what makes every one of the fifteen
	// (path, verdict) cells — and each durable verdict — a CONSTRUCTED visit
	// rather than a lucky draw.
	pending       []tsOpSpec
	pendingEngine []tsVerdict
	// witnesses are the (name, accepted age) pairs armed so far. Every one is
	// re-verified after every recovery and once at the end; the list only grows,
	// because nothing in this scenario deletes a Person.
	witnesses []tsWitness
	// engineSchema is the [schema.Schema] currently installed on the DURABLE
	// graph, and engineModel its verdict oracle. Both are rebuilt on every
	// recovery, from the same declaration table, because a recovered graph has
	// FRESH registries and [schema.New] requires the schema to be bound to the
	// registries of the graph it describes.
	engineSchema *schema.Schema
	engineModel  *typedSchemaModel
}

// tsWitness is one armed durability witness: a Person that received an ACCEPTED
// age and then a REFUSED one, so a post-recovery read separates "the durable
// path worked" from "the refused value stayed out of the log".
type tsWitness struct {
	name        string
	acceptedAge int64
	rejected    string
}

// NewTypedSchemaProbes returns a probe battery drawing its cells and values from
// seed. The seed must be derived from the run seed and must NOT be the durable
// workload's, so the durable op stream stays a pure function of the master seed.
func NewTypedSchemaProbes(seed *Seed) *TypedSchemaProbes {
	return &TypedSchemaProbes{seed: seed, ev: &TypedSchemaEvidence{}}
}

// Evidence returns the accumulating record. The pointer is owned by the probes
// and is live for the whole run.
func (p *TypedSchemaProbes) Evidence() *TypedSchemaEvidence { return p.ev }

// nextSpec returns the next cell of the sweep, drawing a fresh seed-shuffled
// epoch when the current one is exhausted.
func (p *TypedSchemaProbes) nextSpec() tsOpSpec {
	if len(p.pending) == 0 {
		p.pending = tsCells(p.seed)
	}
	spec := p.pending[0]
	p.pending = p.pending[1:]
	return spec
}

// SideWrite drives one side-battery write and adjudicates it: the verdict
// against the model, then either the accept read-back or the full no-mutation
// battery.
//
// perturb is a TEST-ONLY parameter (see [tsPerturb]); the scenario always passes
// [tsPerturbNone].
//
// pre/post snapshot inline; splitting it would hide the ordering the clauses
// depend on.
//
//nolint:gocyclo // one linear pass over five paths and three verdicts, with the
func (p *TypedSchemaProbes) SideWrite(
	tick int64, side *typedSchemaSide, spec tsOpSpec, perturb tsPerturb,
) []Violation {
	p.ev.SideBatteries++
	target := side.resolveTarget(spec, p.seed)
	t := &target

	// Harness self-check FIRST: the cell builder and the model must agree about
	// what this spec means. If they do not, every clause below is adjudicating
	// against the wrong expectation, and saying so is more useful than a
	// downstream mismatch.
	predicted := side.model.predictWrite(spec.key, spec.value)
	if predicted != spec.want {
		return []Violation{tsViolation(ViolationOracleDeviation, tick, "harness:spec-model-disagreement",
			"the cell builder asked for %s on %s but the model predicts %s for key=%q value=%s",
			spec.want, spec.path, predicted, spec.key, tsRender(spec.value, true))}
	}

	preVal, preHad := side.read(t, spec.path, spec.key)
	preCross, preCrossHad, hasCross := side.readCross(t, spec.path, spec.key)
	preNodes := side.g.AdjList().Order()
	preEdges := side.g.AdjList().Size()
	_, preInterned := side.g.PropertyKeys().Lookup(spec.key)

	err := side.write(t, spec)
	observed, classified := tsClassify(err)
	if !classified {
		return []Violation{tsViolation(ViolationOracleDeviation, tick, "unclassified-error",
			"%s on %s returned an error matching NO declared schema sentinel: %v", spec.path, t.label(), err)}
	}
	if perturb == tsPerturbFlipVerdict {
		if observed == tsAccept {
			observed = tsRejectTypeMismatch
		} else {
			observed = tsAccept
		}
	}

	p.ev.Coverage[spec.path][predicted]++
	p.ev.Digest = tsDigest(p.ev.Digest, tick, "side:"+spec.path.String(), int(predicted), int(observed))

	if observed != predicted {
		return []Violation{tsViolation(ViolationACIDConsistency, tick, "verdict",
			"validator enforcement gap on %s at %s: observed %s, model predicts %s "+
				"(key=%q value=%s, err=%v)",
			spec.path, t.label(), observed, predicted, spec.key, tsRender(spec.value, true), err)}
	}

	if predicted == tsAccept {
		return p.checkAcceptLanded(tick, side, t, spec, preNodes, preEdges)
	}
	return p.checkNoMutation(tick, side, t, spec, tsNoMutationPre{
		val: preVal, had: preHad, cross: preCross, crossHad: preCrossHad, hasCross: hasCross,
		nodes: preNodes, edges: preEdges, interned: preInterned,
	}, perturb)
}

// tsNoMutationPre is the pre-write snapshot the no-mutation battery compares
// against. It is captured BEFORE the write, so the comparison is against a state
// this file observed rather than against one re-derived afterwards from the same
// store it is auditing.
type tsNoMutationPre struct {
	val      lpg.PropertyValue
	cross    lpg.PropertyValue
	nodes    uint64
	edges    uint64
	had      bool
	crossHad bool
	hasCross bool
	interned bool
}

// checkAcceptLanded verifies an accepted write is readable through the path's
// own accessors and that the population moved by exactly the amount the path
// implies: the fused path adds one node and one edge, every other path adds
// neither.
func (p *TypedSchemaProbes) checkAcceptLanded(
	tick int64, side *typedSchemaSide, t *tsTarget, spec tsOpSpec, preNodes, preEdges uint64,
) []Violation {
	var vs []Violation
	got, had := side.read(t, spec.path, spec.key)
	if !had || !graphIOPropValueEqual(got, spec.value) {
		vs = append(vs, tsViolation(ViolationACIDConsistency, tick, "accept-landed",
			"%s on %s was ACCEPTED but reads back %s, want %s",
			spec.path, t.label(), tsRender(got, had), tsRender(spec.value, true)))
	}
	if cross, crossHad, ok := side.readCross(t, spec.path, spec.key); ok {
		if !crossHad || !graphIOPropValueEqual(cross, spec.value) {
			vs = append(vs, tsViolation(ViolationACIDConsistency, tick, "accept-landed-cross",
				"%s on %s was ACCEPTED but the second accessor reads %s, want %s",
				spec.path, t.label(), tsRender(cross, crossHad), tsRender(spec.value, true)))
		}
	}
	wantNodes, wantEdges := preNodes, preEdges
	if spec.path == tsPathFusedAddEdge {
		wantNodes, wantEdges = preNodes+1, preEdges+1
		if !side.g.AdjList().HasEdge(t.src, t.dst) {
			vs = append(vs, tsViolation(ViolationACIDConsistency, tick, "accept-landed-fused",
				"the accepted fused write %s->%s inserted NO edge", t.src, t.dst))
		}
	}
	if gotN, gotE := side.g.AdjList().Order(), side.g.AdjList().Size(); gotN != wantNodes || gotE != wantEdges {
		vs = append(vs, tsViolation(ViolationGraphIntegrity, tick, "accept-population",
			"%s on %s moved the population to (nodes=%d edges=%d), want (nodes=%d edges=%d)",
			spec.path, t.label(), gotN, gotE, wantNodes, wantEdges))
	}
	if len(vs) == 0 {
		side.shadow[t.shadowKey(spec.key)] = spec.value
		p.ev.AcceptLandedChecks[spec.path]++
	}
	return vs
}

// checkNoMutation is the battery a REJECTED write must survive: nothing
// observable may have changed.
//
// It checks five things, and each is a separate clause because they fail for
// different reasons: the target's value through the path's own accessor, the
// same value through the store's SECOND accessor where one exists, the node and
// edge population, whether the property key was interned, and — on the fused
// path — that neither the edge nor its fresh endpoint node appeared.
func (p *TypedSchemaProbes) checkNoMutation(
	tick int64, side *typedSchemaSide, t *tsTarget, spec tsOpSpec,
	pre tsNoMutationPre, perturb tsPerturb,
) []Violation {
	// The perturbations reproduce the OUTPUT a MISSING hook would produce, rather
	// than faking a mismatch: repeating the very same write with the validator
	// uninstalled is exactly what the store would hold if the path did not
	// consult it.
	switch perturb {
	case tsPerturbApplyRejected:
		side.g.SetValidator(nil)
		_ = side.write(t, spec) //nolint:errcheck // the perturbation WANTS the write to land
		side.g.SetValidator(side.sc)
	case tsPerturbInternGhostKey:
		// Reproduces a hook that ran AFTER the intern. It permanently taints the
		// fixture's registry, which is why it is only ever passed by a test.
		side.g.PropertyKeys().Intern(spec.key)
	default:
	}

	var vs []Violation
	postVal, postHad := side.read(t, spec.path, spec.key)
	if postHad != pre.had || (postHad && !graphIOPropValueEqual(postVal, pre.val)) {
		vs = append(vs, tsViolation(ViolationACIDAtomicity, tick, "no-mutation:value",
			"%s on %s was REJECTED (%s) yet the stored value moved from %s to %s",
			spec.path, t.label(), spec.want, tsRender(pre.val, pre.had), tsRender(postVal, postHad)))
	}
	if pre.hasCross {
		postCross, postCrossHad, _ := side.readCross(t, spec.path, spec.key)
		if postCrossHad != pre.crossHad || (postCrossHad && !graphIOPropValueEqual(postCross, pre.cross)) {
			vs = append(vs, tsViolation(ViolationACIDAtomicity, tick, "no-mutation:cross-accessor",
				"%s on %s was REJECTED yet the SECOND accessor moved from %s to %s",
				spec.path, t.label(), tsRender(pre.cross, pre.crossHad), tsRender(postCross, postCrossHad)))
		}
		p.ev.CrossAccessorChecks++
	}
	if gotN, gotE := side.g.AdjList().Order(), side.g.AdjList().Size(); gotN != pre.nodes || gotE != pre.edges {
		vs = append(vs, tsViolation(ViolationACIDAtomicity, tick, "no-mutation:population",
			"%s on %s was REJECTED yet the population moved from (nodes=%d edges=%d) to (nodes=%d edges=%d)",
			spec.path, t.label(), pre.nodes, pre.edges, gotN, gotE))
	}
	if spec.want == tsRejectUnknownProperty {
		_, interned := side.g.PropertyKeys().Lookup(spec.key)
		if interned != pre.interned {
			vs = append(vs, tsViolation(ViolationACIDAtomicity, tick, "no-mutation:key-interning",
				"%s on %s was REJECTED with %s yet the key %q was INTERNED into the property-key "+
					"registry (interned before=%t after=%t): the hook ran after the intern, not before it",
				spec.path, t.label(), spec.want, spec.key, pre.interned, interned))
		}
		p.ev.KeyInterningChecks++
	}
	if spec.path == tsPathFusedAddEdge {
		if side.g.AdjList().HasEdge(t.src, t.dst) {
			vs = append(vs, tsViolation(ViolationACIDAtomicity, tick, "no-mutation:fused-edge",
				"the REJECTED fused write inserted the edge %s->%s anyway", t.src, t.dst))
		}
		if _, interned := side.g.AdjList().Mapper().Lookup(t.dst); interned {
			vs = append(vs, tsViolation(ViolationACIDAtomicity, tick, "no-mutation:fused-endpoint",
				"the REJECTED fused write INTERNED its fresh endpoint node %q", t.dst))
		}
		p.ev.FusedNoEdgeChecks++
	}
	if len(vs) == 0 {
		p.ev.NoMutationChecks[spec.path]++
	}
	return vs
}

// -----------------------------------------------------------------------------
// Arm C — the ValidateNode boundary
// -----------------------------------------------------------------------------

// NodeBattery drives the whole-node boundary: the mid-build rejection, the
// finalised acceptance, the unlabelled and never-interned controls, and the
// pre-installation fixture whose stored value the schema forbids.
//
// Every clause carries TWO expectations, and they are separate on purpose. The
// LITERAL expectation is the harness's own precondition — "I set the label and
// did not set the required property, so this must be refused" — and the MODEL
// expectation is [typedSchemaModel.predictNode] over the node's actual labels
// and properties. A perturbation that changes the graph fires the literal clause
// while leaving the model clause silent, which is the correct attribution: the
// fixture stopped being what the harness thought, rather than the hook being
// wrong.
//
// and splitting them would obscure the build sequence they depend on.
//
//nolint:gocyclo // five fixture clauses in a fixed order; each is three lines
func (p *TypedSchemaProbes) NodeBattery(
	tick int64, side *typedSchemaSide, perturb tsPerturb,
) []Violation {
	p.ev.NodeBatteries++
	var vs []Violation

	side.buildSeq++
	name := fmt.Sprintf("ts-b%d", side.buildSeq)
	if err := side.g.AddNode(name); err != nil {
		return []Violation{tsViolation(ViolationOracleDeviation, tick, "harness:node-battery",
			"AddNode %q: %v", name, err)}
	}
	if err := side.g.SetNodeLabel(name, tsSideLabel); err != nil {
		return []Violation{tsViolation(ViolationOracleDeviation, tick, "harness:node-battery",
			"SetNodeLabel %q: %v", name, err)}
	}
	if perturb == tsPerturbNodePrefill {
		// Reproduces a fixture that was already complete: the mid-build clause's
		// precondition is gone, so the clause must fire.
		_ = side.g.SetNodeProperty(name, tsKeyStr, lpg.StringValue("prefilled")) //nolint:errcheck // perturbation
	}
	vs = append(vs, p.validateClause(tick, side, name, "validate:mid-build", tsNodeMissingRequired)...)
	p.ev.ValidateMidBuild++

	if perturb != tsPerturbNodeSkipRequired {
		if err := side.g.SetNodeProperty(name, tsKeyStr, lpg.StringValue("final-"+name)); err != nil {
			return append(vs, tsViolation(ViolationOracleDeviation, tick, "harness:node-battery",
				"SetNodeProperty %q required prop: %v", name, err))
		}
	}
	vs = append(vs, p.validateClause(tick, side, name, "validate:finalised", tsNodeOK)...)
	p.ev.ValidateFinalised++

	vs = append(vs, p.validateClause(tick, side, tsBareNode, "validate:unlabelled", tsNodeOK)...)
	p.ev.ValidateUnlabelled++

	// The never-interned node. ValidateNode documents a nil return for it (there
	// is nothing to validate), and the clause exists so that documented exit is
	// exercised rather than assumed.
	if err := side.g.ValidateNode(tsGhostNode); err != nil {
		vs = append(vs, tsViolation(ViolationACIDConsistency, tick, "validate:ghost",
			"ValidateNode on the never-interned node %q returned %v, want nil (nothing to validate)",
			tsGhostNode, err))
	} else {
		p.ev.NodeVerdicts[tsNodeOK]++
	}
	p.ev.ValidateGhost++

	if perturb == tsPerturbRepairPreInstall {
		// Reproduces a fixture whose forbidden value is gone: the kind-re-check
		// clause's precondition is gone with it, so the clause must fire.
		_ = side.g.SetNodeProperty(tsPreInstallNode, tsKeyInt, lpg.Int64Value(1)) //nolint:errcheck // perturbation
	}
	vs = append(vs, p.validateClause(tick, side, tsPreInstallNode, "validate:pre-install", tsNodeTypeMismatch)...)
	p.ev.ValidatePreInstall++
	return vs
}

// validateClause adjudicates one [lpg.Graph.ValidateNode] observation against
// both the literal precondition and the model.
func (p *TypedSchemaProbes) validateClause(
	tick int64, side *typedSchemaSide, key, clause string, want tsNodeVerdict,
) []Violation {
	err := side.g.ValidateNode(key)
	observed, classified := tsClassifyNode(err)
	if !classified {
		return []Violation{tsViolation(ViolationOracleDeviation, tick, clause,
			"ValidateNode(%q) returned an error matching NO declared schema sentinel: %v", key, err)}
	}
	p.ev.NodeVerdicts[observed]++
	p.ev.Digest = tsDigest(p.ev.Digest, tick, clause, int(want), int(observed))

	var vs []Violation
	if observed != want {
		vs = append(vs, tsViolation(ViolationACIDConsistency, tick, clause,
			"ValidateNode(%q): observed %s, the constructed fixture requires %s (err=%v)",
			key, observed, want, err))
	}
	// The independent channel: the model over the node's ACTUAL labels and
	// properties, read from the graph rather than assumed by the harness.
	if id, ok := side.g.AdjList().Mapper().Lookup(key); ok {
		props := side.g.NodePropertiesByID(id)
		if props == nil {
			props = map[string]lpg.PropertyValue{}
		}
		if modelled := side.model.predictNode(side.g.NodeLabelsByID(id), props); modelled != observed {
			vs = append(vs, tsViolation(ViolationACIDConsistency, tick, clause+":model",
				"ValidateNode(%q): observed %s, the model over the node's actual labels=%v props=%v "+
					"predicts %s", key, observed, side.g.NodeLabelsByID(id), tsRenderProps(props), modelled))
		}
	}
	return vs
}

// tsRenderProps renders a property bag deterministically for a report.
func tsRenderProps(props map[string]lpg.PropertyValue) string {
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s=%s", k, tsRender(props[k], true))
	}
	b.WriteByte('}')
	return b.String()
}

// -----------------------------------------------------------------------------
// The substrate channel — the native store, read without a Cypher projection
// -----------------------------------------------------------------------------

// typedSchemaSubstrate is a view of the durable graph built by walking
// [graph.Mapper.Walk] directly and reading each live survivor's property bag
// through [lpg.Graph.NodePropertiesByID].
//
// It exists because the durable arm's witnesses are addressed by their `name`
// property while their MAPPER KEY is the engine's synthetic `__cx_<hex>`, and
// because a post-recovery value must be read through a channel that is not a
// Cypher projection: a projection defect and a stored-value defect are different
// findings, and one channel cannot separate them.
type typedSchemaSubstrate struct {
	keyOf   map[string]string
	propsOf map[string]map[string]lpg.PropertyValue
}

// newTypedSchemaSubstrate walks g and indexes every live, `name`-bearing node by
// its name.
//
// The Walk callback only appends to local state and never re-enters the Mapper,
// satisfying [graph.Mapper.Walk]'s re-entrancy contract; the property and
// tombstone reads happen after Walk returns.
func newTypedSchemaSubstrate(g *lpg.Graph[string, float64]) *typedSchemaSubstrate {
	type entry struct {
		key string
		id  graph.NodeID
	}
	var entries []entry
	g.AdjList().Mapper().Walk(func(id graph.NodeID, key string) bool {
		entries = append(entries, entry{key: key, id: id})
		return true
	})
	s := &typedSchemaSubstrate{
		keyOf:   make(map[string]string, len(entries)),
		propsOf: make(map[string]map[string]lpg.PropertyValue, len(entries)),
	}
	for _, e := range entries {
		if g.IsTombstoned(e.id) {
			continue
		}
		props := g.NodePropertiesByID(e.id)
		if props == nil {
			continue
		}
		nv, ok := props[tsEngineKeyName]
		if !ok {
			continue
		}
		name, ok := nv.String()
		if !ok {
			continue
		}
		s.keyOf[name] = e.key
		s.propsOf[name] = props
	}
	return s
}

// -----------------------------------------------------------------------------
// Arm D — the durable (Cypher) path
// -----------------------------------------------------------------------------

// tsEngineWrite is one property write a modelled template performs.
type tsEngineWrite struct {
	key   string
	value lpg.PropertyValue
}

// tsParamValue converts a bound Cypher parameter to the [lpg.PropertyValue] the
// engine will hand the validator, reporting false for a kind this scenario never
// binds (which would be a harness bug, not a silent pass).
func tsParamValue(v any) (lpg.PropertyValue, bool) {
	switch x := v.(type) {
	case string:
		return lpg.StringValue(x), true
	case int64:
		return lpg.Int64Value(x), true
	case int:
		return lpg.Int64Value(int64(x)), true
	case float64:
		return lpg.Float64Value(x), true
	case bool:
		return lpg.BoolValue(x), true
	default:
		return lpg.PropertyValue{}, false
	}
}

// tsEngineWrites returns the property writes op's template performs, so the
// model can predict the statement's verdict from the declaration table alone.
//
// It is an explicit table rather than a parse: the templates are shared
// constants (oracle.go), so a template this scenario starts emitting without
// being added here reports false and the run says so instead of silently
// predicting acceptance.
func tsEngineWrites(op Op) ([]tsEngineWrite, bool) {
	get := func(param string) (lpg.PropertyValue, bool) {
		raw, ok := op.Params[param]
		if !ok {
			return lpg.PropertyValue{}, false
		}
		return tsParamValue(raw)
	}
	switch op.Cypher {
	case tmplCreatePerson:
		name, ok1 := get("name")
		age, ok2 := get("age")
		return []tsEngineWrite{{tsEngineKeyName, name}, {tsEngineKeyAge, age}}, ok1 && ok2
	case tmplCreatePersonCity:
		name, ok1 := get("name")
		age, ok2 := get("age")
		city, ok3 := get("city")
		return []tsEngineWrite{
			{tsEngineKeyName, name}, {tsEngineKeyAge, age}, {tsEngineKeyCity, city},
		}, ok1 && ok2 && ok3
	case tmplSetAge:
		age, ok := get("age")
		return []tsEngineWrite{{tsEngineKeyAge, age}}, ok
	case tmplMergePerson:
		name, ok := get("name")
		// ON CREATE SET n.created=true is a LITERAL in the template, not a
		// parameter, so it is spelled out here.
		return []tsEngineWrite{{tsEngineKeyName, name}, {tsEngineKeyCreated, lpg.BoolValue(true)}}, ok
	default:
		return nil, false
	}
}

// tsPredictEngine returns the model's verdict for a whole statement: the first
// non-accepting write decides it.
//
// Each template this scenario emits makes AT MOST ONE rejectable write, so the
// order the engine evaluates its writes in cannot change the answer — which is
// why the arm asserts the verdict CLASS and never which key produced it.
func tsPredictEngine(m *typedSchemaModel, op Op) (tsVerdict, bool) {
	writes, ok := tsEngineWrites(op)
	if !ok {
		return tsAccept, false
	}
	for _, w := range writes {
		if v := m.predictWrite(w.key, w.value); v != tsAccept {
			return v, true
		}
	}
	return tsAccept, true
}

// tsRunWrite runs op through the engine's write path and returns the error the
// statement actually failed with — the run error when the statement was refused
// up front, otherwise the DRAIN error.
//
// The distinction is measured, not assumed: a validator rejection inside a
// Cypher statement surfaces at DRAIN time, not from RunWrite, so a helper that
// only looked at the first return value would report every rejection as an
// acceptance.
func tsRunWrite(ctx context.Context, e *EngineAdapter, op Op) error {
	res, err := e.RunWrite(ctx, op.Cypher, op.Params)
	if err != nil {
		return err
	}
	for res.Next() {
	}
	drain := res.Err()
	_ = res.Close()
	return drain
}

// EngineWrite drives one durable statement and adjudicates its verdict against
// the model, then — for a refused CREATE — asserts the statement left no node
// behind.
//
// It advances the [GraphOracle] only when the statement committed, exactly as
// [Simulator.applyToOracle] requires, so the harness's own count parity and the
// crash-boundary durability check stay meaningful for the whole run.
func (p *TypedSchemaProbes) EngineWrite(
	ctx context.Context, sm *Simulator, tick int64, op Op, want tsVerdict,
) []Violation {
	predicted, known := tsPredictEngine(p.engineModel, op)
	if !known {
		return []Violation{tsViolation(ViolationOracleDeviation, tick, "harness:engine-template",
			"no write table for template %q: the model cannot predict its verdict", op.Cypher)}
	}
	if predicted != want {
		return []Violation{tsViolation(ViolationOracleDeviation, tick, "harness:engine-spec",
			"the durable op builder asked for %s but the model predicts %s for %q params=%v",
			want, predicted, op.Cypher, op.Params)}
	}

	nodesBefore, cntErr := sm.engine.NodeCount()
	if cntErr != nil {
		return []Violation{tsViolation(ViolationOracleDeviation, tick, "harness:engine-count",
			"NodeCount before %q: %v", op.Cypher, cntErr)}
	}

	err := tsRunWrite(ctx, sm.engine, op)
	sm.applyToOracle(op, err == nil)
	observed, classified := tsClassify(err)
	if !classified {
		return []Violation{tsViolation(ViolationOracleDeviation, tick, "engine:unclassified-error",
			"%q returned an error matching NO declared schema sentinel: %v", op.Cypher, err)}
	}
	p.ev.EngineVerdicts[predicted]++
	p.ev.Digest = tsDigest(p.ev.Digest, tick, "engine:"+string(op.Kind), int(predicted), int(observed))

	if observed != predicted {
		return []Violation{tsViolation(ViolationACIDConsistency, tick, "engine:verdict",
			"validator enforcement gap on the durable path: %q observed %s, model predicts %s (err=%v)",
			op.Cypher, observed, predicted, err)}
	}
	if predicted == tsAccept || op.Kind != OpCreate {
		return nil
	}

	// A REFUSED CREATE must leave no node. MEASURED: the engine interns a mapper
	// slot and then TOMBSTONES it (the statement's undo runs), so the count is
	// unchanged and the name is unfindable while the slot leaks — the documented
	// NodeID-stability contract. Both halves of that are asserted; the leaked
	// slot is not, because it is not a defect.
	var vs []Violation
	nodesAfter, err2 := sm.engine.NodeCount()
	switch {
	case err2 != nil:
		vs = append(vs, tsViolation(ViolationOracleDeviation, tick, "harness:engine-count",
			"NodeCount after the refused %q: %v", op.Cypher, err2))
	case nodesAfter != nodesBefore:
		vs = append(vs, tsViolation(ViolationACIDAtomicity, tick, "engine:rejected-create-count",
			"the REFUSED %q moved the live node count from %d to %d", op.Cypher, nodesBefore, nodesAfter))
	}
	if name, ok := op.Params["name"].(string); ok {
		n, qerr := tsCountByName(ctx, sm.engine, name)
		switch {
		case qerr != nil:
			vs = append(vs, tsViolation(ViolationOracleDeviation, tick, "harness:engine-probe",
				"count probe for the refused name %q: %v", name, qerr))
		case n != 0:
			vs = append(vs, tsViolation(ViolationACIDAtomicity, tick, "engine:rejected-create-visible",
				"the REFUSED %q left %d node(s) findable under name %q", op.Cypher, n, name))
		}
	}
	if len(vs) == 0 {
		p.ev.EngineRejectedCreateChecks++
	}
	return vs
}

// tsCountByName counts the Persons carrying name.
func tsCountByName(ctx context.Context, e *EngineAdapter, name string) (int64, error) {
	res, err := e.Run(ctx, "MATCH (n:Person {name:$name}) RETURN count(n)", map[string]any{"name": name})
	if err != nil {
		return 0, err
	}
	var out int64
	for res.Next() {
		if v, ok := res.ScalarInt(); ok {
			out = v
		}
	}
	drain := res.Err()
	_ = res.Close()
	return out, drain
}

// tsAgeByName reads a Person's `age` through a CYPHER PROJECTION, reporting
// whether the projection produced an INTEGER. A stored STRING makes the integer
// read fail, which is exactly the discrimination the witness clause needs.
func tsAgeByName(ctx context.Context, e *EngineAdapter, name string) (int64, bool, error) {
	res, err := e.Run(ctx, "MATCH (n:Person {name:$name}) RETURN n.age", map[string]any{"name": name})
	if err != nil {
		return 0, false, err
	}
	var (
		got   int64
		isInt bool
	)
	for res.Next() {
		got, isInt = res.IntAt(0)
	}
	drain := res.Err()
	_ = res.Close()
	return got, isInt, drain
}

// ArmWitness commits one durability witness: a Person created with an ACCEPTED
// age, immediately followed by a REFUSED age on the same node.
//
// The pair is what makes the post-recovery clause non-vacuous. "The refused
// value is absent" is unfalsifiable on its own — a recovery that replayed
// nothing would satisfy it — so every witness also carries a value that MUST
// come back.
func (p *TypedSchemaProbes) ArmWitness(ctx context.Context, sm *Simulator, tick int64) []Violation {
	seq := len(p.witnesses)
	name := fmt.Sprintf("ts-w%d", seq)
	age := typedSchemaWitnessAgeBase + int64(p.seed.IntN(500))
	create := Op{Kind: OpCreate, Cypher: tmplCreatePerson,
		Params: map[string]any{"name": name, "age": age}}
	if vs := p.EngineWrite(ctx, sm, tick, create, tsAccept); len(vs) > 0 {
		return vs
	}
	poison := fmt.Sprintf("poison-%d", seq)
	refuse := Op{Kind: OpUpdate, Cypher: tmplSetAge,
		Params: map[string]any{"name": name, "age": poison}}
	if vs := p.EngineWrite(ctx, sm, tick, refuse, tsRejectTypeMismatch); len(vs) > 0 {
		return vs
	}
	p.witnesses = append(p.witnesses, tsWitness{name: name, acceptedAge: age, rejected: poison})
	p.ev.WitnessesArmed++
	return nil
}

// VerifyWitnesses reads every armed witness through BOTH channels and requires
// the ACCEPTED value and only the accepted value.
//
// afterRecovery distinguishes the reads that matter for the coverage claim (a
// graph that came back through real recovery) from the terminal read on the live
// graph, which is a cheaper regression check on the same invariant.
func (p *TypedSchemaProbes) VerifyWitnesses(
	ctx context.Context, sm *Simulator, tick int64, phase string, afterRecovery bool, perturb tsPerturb,
) []Violation {
	if len(p.witnesses) == 0 {
		return nil
	}
	g := sm.graph()
	if perturb == tsPerturbWitnessPoison && g != nil {
		// Reproduces the OUTPUT a validator-less WAL replay would produce for the
		// refused value: land it with the validator momentarily uninstalled.
		sub := newTypedSchemaSubstrate(g)
		w := p.witnesses[0]
		if key, ok := sub.keyOf[w.name]; ok {
			g.SetValidator(nil)
			_ = g.SetNodeProperty(key, tsEngineKeyAge, lpg.StringValue(w.rejected)) //nolint:errcheck // perturbation
			g.SetValidator(p.engineSchema)
		}
	}
	sub := newTypedSchemaSubstrate(g)

	var vs []Violation
	for _, w := range p.witnesses {
		got, isInt, err := tsAgeByName(ctx, sm.engine, w.name)
		switch {
		case err != nil:
			vs = append(vs, tsViolation(ViolationOracleDeviation, tick, "witness:cypher-probe",
				"[%s] projecting n.age for witness %q: %v", phase, w.name, err))
		case !isInt || got != w.acceptedAge:
			vs = append(vs, tsViolation(ViolationACIDConsistency, tick, "witness:cypher",
				"[%s] witness %q projects age integer=%t value=%d, want the ACCEPTED %d. A "+
					"non-integer here is the REFUSED value %q having become durable",
				phase, w.name, isInt, got, w.acceptedAge, w.rejected))
		default:
			p.ev.WitnessCypherReads++
		}

		props, ok := sub.propsOf[w.name]
		if !ok {
			vs = append(vs, tsViolation(ViolationACIDDurability, tick, "witness:substrate-missing",
				"[%s] witness %q is absent from the native store (mapper walk found no live "+
					"`name`-bearing node with that name)", phase, w.name))
			continue
		}
		stored, had := props[tsEngineKeyAge]
		switch {
		case !had:
			vs = append(vs, tsViolation(ViolationACIDDurability, tick, "witness:substrate",
				"[%s] witness %q carries NO age in the native store; the ACCEPTED %d was lost",
				phase, w.name, w.acceptedAge))
		case stored.Kind() != lpg.PropInt64:
			vs = append(vs, tsViolation(ViolationACIDConsistency, tick, "witness:substrate",
				"[%s] witness %q carries %s in the native store: the REFUSED value reached the "+
					"durable image", phase, w.name, tsRender(stored, true)))
		default:
			if i, _ := stored.Int64(); i != w.acceptedAge {
				vs = append(vs, tsViolation(ViolationACIDConsistency, tick, "witness:substrate",
					"[%s] witness %q carries INTEGER(%d) in the native store, want %d",
					phase, w.name, i, w.acceptedAge))
			} else {
				p.ev.WitnessSubstrateReads++
			}
		}
	}
	if afterRecovery && len(vs) == 0 {
		p.ev.WitnessReadsAfterRecovery++
	}
	p.ev.Digest = tsDigest(p.ev.Digest, tick, "witness:"+phase, len(p.witnesses), len(vs))
	return vs
}

// InstallEngineSchema binds a FRESH [schema.Schema] to g's own registries,
// installs it as g's validator, and records it (and its model) on the probes.
//
// It is called once before the loop and again after every recovery. The schema
// is rebuilt rather than re-installed because a recovered graph has fresh
// registries: [schema.New] mints property-key and label ids through the
// registries it is handed, so re-installing a schema built over the crashed
// graph's registries would hand out ids that no longer describe this graph.
func (p *TypedSchemaProbes) InstallEngineSchema(g *lpg.Graph[string, float64]) error {
	sc, model, err := typedSchemaInstall(g, typedSchemaEngineDecls(),
		map[string][]string{tsEngineLabel: {tsEngineKeyName}})
	if err != nil {
		return err
	}
	p.engineSchema, p.engineModel = sc, model
	return nil
}

// RecoveryPin is the constructed pin for the recovery asymmetry the acceptance
// criteria require, and it yields FIVE clauses from one probe:
//
//  1. pin:no-validator      — a write the LIVE validator refused is ACCEPTED on
//     the freshly recovered graph. This is the documented
//     limitation, asserted positively rather than only written
//     down: the schema is not among the snapshot's components,
//     so nothing re-installs it.
//  2. pin:node-clean        — [lpg.Graph.ValidateNode] reports clean on the same
//     graph, so the WHOLE-NODE hook is absent too, not merely
//     the per-value one.
//  3. pin:reinstalled       — with a fresh schema installed, the IDENTICAL write
//     is refused. Without this clause, clause 1 could pass
//     because the write was never forbidden at all.
//  4. pin:validate-detected — ValidateNode now reports the planted value as a
//     type mismatch, because the value planted while the
//     graph was unvalidated is still stored. This is the kind
//     re-check branch that only the whole-node hook reaches.
//  5. pin:validate-repaired — once the value is repaired, ValidateNode reports
//     clean, so clause 4 measured the value rather than a
//     permanently-refusing node.
//
// The plant is a DIRECT lpg write, so it never reaches the WAL and cannot
// contaminate the durable image; the repair happens before this function returns,
// so no checkpoint can capture it either.
func (p *TypedSchemaProbes) RecoveryPin(
	sm *Simulator, tick int64, epoch int, perturb tsPerturb,
) []Violation {
	g := sm.graph()
	if g == nil {
		return []Violation{tsViolation(ViolationOracleDeviation, tick, "harness:pin",
			"the simulator exposes no live graph, so the recovery pin cannot run")}
	}
	name := fmt.Sprintf("ts-pin%d", epoch%typedSchemaPins)
	sub := newTypedSchemaSubstrate(g)
	key, ok := sub.keyOf[name]
	if !ok {
		return []Violation{tsViolation(ViolationACIDDurability, tick, "pin:target-missing",
			"the pin target %q did not survive recovery: the native store holds no live "+
				"`name`-bearing node with that name", name)}
	}
	poison := lpg.StringValue(fmt.Sprintf("pin-poison-%d", tick))

	if perturb == tsPerturbRecoveryPreinstall {
		// Reproduces a reopen that DID carry a validator: clause 1 must fire.
		if err := p.InstallEngineSchema(g); err != nil {
			return []Violation{tsViolation(ViolationOracleDeviation, tick, "harness:pin", "%v", err)}
		}
	}

	// Clause 1 — the asymmetry itself.
	if err := g.SetNodeProperty(key, tsEngineKeyAge, poison); err != nil {
		return []Violation{tsViolation(ViolationACIDConsistency, tick, "pin:no-validator",
			"the recovered graph REFUSED %s under %q with %v. That contradicts the documented "+
				"limitation this clause pins: the schema is not persisted with snapshots, so a reopened "+
				"graph is expected to carry no validator. If the schema is now recovered, this pin and "+
				"docs/dst-feature-coverage.md both need updating",
			tsRender(poison, true), tsEngineKeyAge, err)}
	}
	p.ev.PinNoValidatorAccepted++

	// Clause 2 — the whole-node hook is absent too.
	if err := g.ValidateNode(key); err != nil {
		return []Violation{tsViolation(ViolationACIDConsistency, tick, "pin:node-clean",
			"ValidateNode on the freshly recovered graph returned %v; with no validator installed it "+
				"must report clean", err)}
	}
	p.ev.PinNoValidatorNodeClean++

	// Re-install, then clause 3 — the write really was forbidden.
	if err := p.InstallEngineSchema(g); err != nil {
		return []Violation{tsViolation(ViolationOracleDeviation, tick, "harness:pin", "%v", err)}
	}
	err := g.SetNodeProperty(key, tsEngineKeyAge, poison)
	if v, classified := tsClassify(err); !classified || v != tsRejectTypeMismatch {
		return []Violation{tsViolation(ViolationACIDConsistency, tick, "pin:reinstalled",
			"with the schema re-installed, %s under %q was answered with %v (classified=%t as %s); "+
				"want %s. Clause pin:no-validator is meaningless unless this one holds",
			tsRender(poison, true), tsEngineKeyAge, err, classified, v, tsRejectTypeMismatch)}
	}
	p.ev.PinReinstalledRejected++

	// Clause 4 — the planted value is still stored, and the whole-node hook sees it.
	nerr := g.ValidateNode(key)
	if nv, classified := tsClassifyNode(nerr); !classified || nv != tsNodeTypeMismatch {
		return []Violation{tsViolation(ViolationACIDConsistency, tick, "pin:validate-detected",
			"ValidateNode after the plant returned %v (classified=%t as %s); want %s — the value "+
				"planted while the graph was unvalidated is still in the store",
			nerr, classified, nv, tsNodeTypeMismatch)}
	}
	p.ev.PinValidateNodeDetected++

	// Repair, then clause 5.
	if rerr := g.SetNodeProperty(key, tsEngineKeyAge, lpg.Int64Value(typedSchemaPinAge)); rerr != nil {
		return []Violation{tsViolation(ViolationOracleDeviation, tick, "harness:pin",
			"repairing the pin target %q: %v", name, rerr)}
	}
	if rerr := g.ValidateNode(key); rerr != nil {
		return []Violation{tsViolation(ViolationACIDConsistency, tick, "pin:validate-repaired",
			"ValidateNode after the repair returned %v, want clean: clause pin:validate-detected "+
				"measured a permanently-refusing node rather than the planted value", rerr)}
	}
	p.ev.PinValidateNodeRepaired++
	p.ev.Digest = tsDigest(p.ev.Digest, tick, "pin", epoch, 5)
	return nil
}

// -----------------------------------------------------------------------------
// Arm D2 — the pure store/txn path, where the fsync precedes the validation
// -----------------------------------------------------------------------------

// tsPureStoreObservation is what the pure store/txn arm MEASURED. It is a record
// rather than a verdict, so the clauses that read it live in one place
// ([checkTypedSchemaPureStore]) and the measurement can be logged by a test
// whether or not the clauses hold.
type tsPureStoreObservation struct {
	// acceptErr and rejectErr are the two commits' errors, rendered.
	acceptErr string
	rejectErr string
	// stored renders what the RECOVERED graph holds for the refused key.
	stored string
	// notApplied and typeMismatch are the two halves of the refused commit's
	// error classification: [txn.ErrCommittedNotApplied] wrapping
	// [schema.ErrTypeMismatch].
	notApplied   bool
	typeMismatch bool
	// liveAbsent is whether the LIVE graph was left without the refused value.
	liveAbsent bool
	// resurrected is whether the RECOVERED graph carries it, and resurrectedKind
	// the kind it came back as.
	resurrected     bool
	resurrectedKind lpg.PropertyKind
	// acceptedSurvived is whether the sibling ACCEPTED value came back. It is the
	// non-vacuity half: without it, an absent refused value would be
	// indistinguishable from a recovery that replayed nothing.
	acceptedSurvived bool
	// walOps is how many ops the reopen replayed.
	walOps int
}

// String renders the observation for a test's log line.
func (o tsPureStoreObservation) String() string {
	return fmt.Sprintf("acceptErr=%q rejectErr=%q notApplied=%t typeMismatch=%t liveAbsent=%t "+
		"acceptedSurvived=%t resurrected=%t storedAfterRecovery=%s walOps=%d",
		o.acceptErr, o.rejectErr, o.notApplied, o.typeMismatch, o.liveAbsent,
		o.acceptedSurvived, o.resurrected, o.stored, o.walOps)
}

// typedSchemaPureStoreArm drives the store/txn path directly — no Cypher engine
// — with a schema installed on the graph, and MEASURES what a validator
// rejection during the post-fsync apply does to the durable image.
//
// It runs on its OWN [SimDisk] so nothing it does can touch the scenario's
// durable store, and it uses [openSimTypedStore] rather than [OpenSimStore]
// because it needs the [txn.Store] itself: the Cypher engine's adapter validates
// BEFORE buffering the WAL op and therefore cannot reach this ordering at all.
func typedSchemaPureStoreArm(seed *Seed) (tsPureStoreObservation, error) {
	var obs tsPureStoreObservation
	nonce := seed.IntN(1_000_000)
	disk := NewSimDisk(NewSeed(uint64(nonce)^typedSchemaPureSeedMix), 0)
	cfg := simulatorStoreConfig()
	cfg.dir = defaultCheckpointDir

	st, err := openSimTypedStore(disk, cfg, txn.NewStringCodec(), txn.NewFloat64WeightCodec())
	if err != nil {
		return obs, fmt.Errorf("sim: typed-schema pure-store open: %w", err)
	}
	if _, _, err := typedSchemaInstall(st.graph, typedSchemaEngineDecls(),
		map[string][]string{tsEngineLabel: {tsEngineKeyName}}); err != nil {
		st.Crash()
		return obs, err
	}

	goodName := fmt.Sprintf("ps-good-%d", nonce)
	badName := fmt.Sprintf("ps-bad-%d", nonce)
	acceptedAge := int64(100 + nonce%100)
	poison := fmt.Sprintf("ps-poison-%d", nonce)

	acceptTx := st.store.Begin()
	if err := acceptTx.AddNode(goodName); err != nil {
		st.Crash()
		return obs, fmt.Errorf("sim: typed-schema pure-store AddNode: %w", err)
	}
	if err := acceptTx.SetNodeProperty(goodName, tsEngineKeyAge, lpg.Int64Value(acceptedAge)); err != nil {
		st.Crash()
		return obs, fmt.Errorf("sim: typed-schema pure-store buffer accepted: %w", err)
	}
	acceptErr := acceptTx.Commit()
	obs.acceptErr = fmt.Sprint(acceptErr)

	rejectTx := st.store.Begin()
	if err := rejectTx.AddNode(badName); err != nil {
		st.Crash()
		return obs, fmt.Errorf("sim: typed-schema pure-store AddNode: %w", err)
	}
	if err := rejectTx.SetNodeProperty(badName, tsEngineKeyAge, lpg.StringValue(poison)); err != nil {
		st.Crash()
		return obs, fmt.Errorf("sim: typed-schema pure-store buffer refused: %w", err)
	}
	rejectErr := rejectTx.Commit()
	obs.rejectErr = fmt.Sprint(rejectErr)
	obs.notApplied = errors.Is(rejectErr, txn.ErrCommittedNotApplied)
	obs.typeMismatch = errors.Is(rejectErr, schema.ErrTypeMismatch)
	_, liveHad := st.graph.GetNodeProperty(badName, tsEngineKeyAge)
	obs.liveAbsent = !liveHad

	// A HOST crash: no graceful close, so only the fsync'd WAL prefix survives —
	// which is exactly the prefix the refused commit's own fsync created.
	st.Crash()
	st2, err := openSimTypedStore(disk, cfg, txn.NewStringCodec(), txn.NewFloat64WeightCodec())
	if err != nil {
		return obs, fmt.Errorf("sim: typed-schema pure-store reopen: %w", err)
	}
	defer func() { _ = st2.Close() }()
	obs.walOps = st2.schema.walOps
	if v, had := st2.graph.GetNodeProperty(goodName, tsEngineKeyAge); had {
		if i, ok := v.Int64(); ok && i == acceptedAge {
			obs.acceptedSurvived = true
		}
	}
	stored, had := st2.graph.GetNodeProperty(badName, tsEngineKeyAge)
	obs.resurrected = had
	obs.stored = tsRender(stored, had)
	if had {
		obs.resurrectedKind = stored.Kind()
	}
	return obs, nil
}

// checkTypedSchemaPureStore adjudicates the pure-store observation.
//
// It PINS the behaviour measured on 2026-08-24 rather than asserting the
// behaviour one might prefer: `txn.Tx.Commit` fsyncs every buffered op and only
// then applies them, so a validator rejection at apply time leaves the frame
// durable, and the reopen — which installs no validator — materialises the very
// value the live validator refused.
//
// Both directions are clauses. If the ordering is ever changed so a rejected
// value cannot become durable, the resurrection clause fails and its message
// says the pin, this file's header and docs/dst-feature-coverage.md must be
// updated — which is the whole point of pinning a measurement instead of
// documenting it.
func checkTypedSchemaPureStore(tick int64, obs tsPureStoreObservation) []Violation {
	var vs []Violation
	if !obs.notApplied || !obs.typeMismatch {
		vs = append(vs, tsViolation(ViolationVacuousRun, tick, "pure-store:precondition",
			"the refused commit reported %q; want an error satisfying BOTH "+
				"errors.Is(txn.ErrCommittedNotApplied)=%t and errors.Is(schema.ErrTypeMismatch)=%t. "+
				"Without it the arm's precondition — a validator rejection during the POST-FSYNC apply "+
				"— was never constructed",
			obs.rejectErr, obs.notApplied, obs.typeMismatch))
		return vs
	}
	if obs.acceptErr != "<nil>" {
		vs = append(vs, tsViolation(ViolationOracleDeviation, tick, "pure-store:accepted-commit",
			"the ACCEPTED sibling commit failed with %q, so the arm has no control", obs.acceptErr))
	}
	if !obs.liveAbsent {
		vs = append(vs, tsViolation(ViolationACIDConsistency, tick, "pure-store:live",
			"the refused value is present in the LIVE graph. The hook runs before the mutation, so a "+
				"rejection during apply must leave the in-memory graph untouched"))
	}
	if !obs.acceptedSurvived {
		vs = append(vs, tsViolation(ViolationACIDDurability, tick, "pure-store:accepted-survived",
			"the ACCEPTED sibling value did NOT survive recovery (walOps=%d), so nothing this arm says "+
				"about the refused value is attributable: an absent value would be indistinguishable "+
				"from a recovery that replayed nothing", obs.walOps))
		return vs
	}
	if !obs.resurrected {
		vs = append(vs, tsViolation(ViolationOracleDeviation, tick, "pure-store:resurrection-pin",
			"the refused value is ABSENT after recovery. That is BETTER than the behaviour this clause "+
				"pins (MEASURED 2026-08-24: txn.Tx.Commit fsyncs before it applies, so the refused frame "+
				"is durable and the validator-less replay materialises it, stored=%s walOps=%d). If the "+
				"ordering was deliberately fixed, update this pin, the typed_schema.go header and "+
				"docs/dst-feature-coverage.md; do not delete the clause", obs.stored, obs.walOps))
		return vs
	}
	if obs.resurrectedKind != lpg.PropString {
		vs = append(vs, tsViolation(ViolationOracleDeviation, tick, "pure-store:resurrection-kind",
			"the refused value came back as %s, not the STRING that was committed (%s): the pin measures "+
				"a different value than the one it thinks it does",
			tsKindName(obs.resurrectedKind), obs.stored))
	}
	return vs
}

// -----------------------------------------------------------------------------
// The durable op builder
// -----------------------------------------------------------------------------

// nextEngineVerdict returns the next durable verdict of the sweep, drawing a
// fresh seed-shuffled epoch over the three verdicts when the current one is
// exhausted. Like the side sweep, the seed decides the ORDER, not the coverage.
func (p *TypedSchemaProbes) nextEngineVerdict() tsVerdict {
	if len(p.pendingEngine) == 0 {
		vs := make([]tsVerdict, 0, int(tsVerdictCount))
		for v := tsVerdict(0); v < tsVerdictCount; v++ {
			vs = append(vs, v)
		}
		for i := len(vs) - 1; i > 0; i-- {
			j := p.seed.IntN(i + 1)
			vs[i], vs[j] = vs[j], vs[i]
		}
		p.pendingEngine = vs
	}
	v := p.pendingEngine[0]
	p.pendingEngine = p.pendingEngine[1:]
	return v
}

// engineOpFor builds a durable statement whose MODEL verdict is want.
//
// Every shape goes through a modelled template (oracle.go), so the [GraphOracle]
// stays the arbiter for the harness's own count parity and crash-boundary
// durability check. The SET shapes target the dedicated mutable pool rather than
// any modelled name, because a SET on a WITNESS would move the accepted value a
// post-recovery clause is holding the graph to.
func (p *TypedSchemaProbes) engineOpFor(want tsVerdict, seq int) Op {
	mutable := fmt.Sprintf("ts-m%d", p.seed.IntN(typedSchemaPersons))
	switch want {
	case tsAccept:
		switch p.seed.IntN(3) {
		case 0:
			return Op{Kind: OpCreate, Cypher: tmplCreatePerson,
				Params: map[string]any{"name": fmt.Sprintf("ts-e%d", seq), "age": int64(p.seed.IntN(90))}}
		case 1:
			return Op{Kind: OpUpdate, Cypher: tmplSetAge,
				Params: map[string]any{"name": mutable, "age": int64(p.seed.IntN(90))}}
		default:
			// A FRESH name on purpose: ON CREATE SET n.created=true only fires when
			// the MERGE creates, and this arm's whole point is that the boolean
			// write is validated.
			return Op{Kind: OpMerge, Cypher: tmplMergePerson,
				Params: map[string]any{"name": fmt.Sprintf("ts-g%d", seq)}}
		}
	case tsRejectTypeMismatch:
		if p.seed.Bool(0.5) {
			return Op{Kind: OpCreate, Cypher: tmplCreatePerson,
				Params: map[string]any{"name": fmt.Sprintf("ts-x%d", seq), "age": fmt.Sprintf("bad-%d", seq)}}
		}
		return Op{Kind: OpUpdate, Cypher: tmplSetAge,
			Params: map[string]any{"name": mutable, "age": fmt.Sprintf("bad-%d", seq)}}
	default:
		// `city` is bound but never registered, so the statement is refused with
		// [schema.ErrUnknownProperty] on the Cypher path too.
		return Op{Kind: OpCreate, Cypher: tmplCreatePersonCity,
			Params: map[string]any{
				"name": fmt.Sprintf("ts-x%d", seq), "age": int64(p.seed.IntN(90)),
				"city": fmt.Sprintf("city-%d", seq),
			}}
	}
}

// -----------------------------------------------------------------------------
// The run
// -----------------------------------------------------------------------------

// TypedSchemaConfig parameterises a typed-schema run. The zero value is not
// usable; [DefaultTypedSchemaConfig] fills in the short-layer budgets and
// [TypedSchemaConfig.normalise] repairs any field a caller left at zero, so a
// test can override one field without restating the rest.
type TypedSchemaConfig struct {
	// Seed is the master seed. Every sub-stream (the probe sweep, the durable op
	// builder, the crash schedule, the SimDisk) derives from it, so the whole run
	// — including [TypedSchemaEvidence.Digest] — is a pure function of this value.
	Seed uint64
	// MaxTicks bounds the deterministic loop. It must admit at least one full
	// fifteen-cell sweep epoch or the coverage gate fires — which is exactly the
	// seam the gate's own meta-test drives.
	MaxTicks int
	// SideNodes is how many nodes the side fixture chains together.
	SideNodes int
	// NodeEvery and WitnessEvery are the in-loop cadences, in ticks, of the
	// ValidateNode boundary battery and of witness arming.
	NodeEvery    int
	WitnessEvery int
	// Crash is the crash/recovery schedule, and Checkpoint the in-loop
	// checkpoint cadence. Both are fields rather than constants so a test can
	// disable one and watch the corresponding non-vacuity gate fire.
	Crash      CrashConfig
	Checkpoint CheckpointConfig
}

// DefaultTypedSchemaConfig returns the short-layer configuration for seed.
func DefaultTypedSchemaConfig(seed uint64) TypedSchemaConfig {
	return TypedSchemaConfig{
		Seed:         seed,
		MaxTicks:     typedSchemaMaxTicks,
		SideNodes:    typedSchemaSideNodes,
		NodeEvery:    typedSchemaNodeEvery,
		WitnessEvery: typedSchemaWitnessEvery,
		Crash:        CrashConfig{Enabled: true, CrashProb: 1.0 / 70.0, StabilityWindow: 25},
		// Comfortably inside the crash stability window, so most crashes follow at
		// least one snapshot+WAL-truncate and the post-recovery clauses adjudicate a
		// graph that came back through the SNAPSHOT path rather than only the WAL.
		Checkpoint: CheckpointConfig{Enabled: true, Every: 45},
	}
}

// normalise replaces any non-positive budget with its default, so a caller may
// override one field and leave the rest zero.
//
// Crash and Checkpoint are deliberately NOT defaulted: a caller that disabled
// one meant it, and quietly re-enabling it would take away the only way to reach
// the corresponding non-vacuity gate.
func (c *TypedSchemaConfig) normalise() {
	if c.MaxTicks <= 0 {
		c.MaxTicks = typedSchemaMaxTicks
	}
	if c.SideNodes <= 1 {
		c.SideNodes = typedSchemaSideNodes
	}
	if c.NodeEvery <= 0 {
		c.NodeEvery = typedSchemaNodeEvery
	}
	if c.WitnessEvery <= 0 {
		c.WitnessEvery = typedSchemaWitnessEvery
	}
}

// typedSchemaSimConfig builds the simulator [Config] the durable arm drives.
//
// The workload is supplied explicitly even though this scenario never selects an
// actor from it: [New] would otherwise build [DefaultWorkload] from the master
// [Seed] instance, and passing a separate one keeps that instance untouched.
func typedSchemaSimConfig(cfg TypedSchemaConfig) Config {
	return Config{
		Seed:       cfg.Seed,
		MaxTicks:   cfg.MaxTicks,
		Workload:   WriteHeavyWorkload(NewSeed(cfg.Seed)),
		Crash:      cfg.Crash,
		Checkpoint: cfg.Checkpoint,
	}
}

// RunTypedSchema drives the scenario once and returns the evidence it measured
// alongside the report of the first violation (nil when the run is clean).
//
// It owns and closes the simulator, so no durable handle or goroutine leaks past
// the run. The evidence is returned even on a violation, because what the run
// managed to exercise before failing is part of the diagnosis.
func RunTypedSchema(
	ctx context.Context, cfg TypedSchemaConfig,
) (*TypedSchemaEvidence, *SimReport, error) {
	cfg.normalise()
	sm, err := New(typedSchemaSimConfig(cfg))
	if err != nil {
		return nil, nil, fmt.Errorf("sim: typed-schema new: %w", err)
	}
	defer func() { _ = sm.Close() }()

	probes := NewTypedSchemaProbes(NewSeed(cfg.Seed ^ typedSchemaSideSeedMix))
	side, err := newTypedSchemaSide(cfg.SideNodes)
	if err != nil {
		return probes.Evidence(), nil, err
	}
	report, err := typedSchemaLoop(ctx, sm, side, cfg, probes)
	return probes.Evidence(), report, err
}

// typedSchemaPrologue installs the durable schema and creates the fixed node
// pools every arm D clause addresses: the MUTABLE persons the SET shapes target,
// the PIN targets the post-recovery probe plants on, and one armed witness.
//
// The schema goes on FIRST, so even the prologue's own writes are validated and a
// declaration error surfaces before any clause runs.
//
// The three pools are disjoint by construction. A SET on a witness would move the
// accepted value a post-recovery clause holds the graph to, and the pin's plant
// would do the same, so neither pool is ever a SET target — which is why the
// mutable pool exists rather than the arm drawing from the whole modelled name
// set.
//
// It is split out of [typedSchemaLoop] so the falsifiability tests can reach the
// post-recovery clauses over the identical fixture the scenario builds, instead
// of a second, drifting copy of it.
func typedSchemaPrologue(
	ctx context.Context, sm *Simulator, probes *TypedSchemaProbes,
) ([]Violation, error) {
	if err := probes.InstallEngineSchema(sm.graph()); err != nil {
		return nil, err
	}
	for i := 0; i < typedSchemaPersons; i++ {
		op := Op{Kind: OpCreate, Cypher: tmplCreatePerson,
			Params: map[string]any{"name": fmt.Sprintf("ts-m%d", i), "age": int64(20 + i)}}
		if vs := probes.EngineWrite(ctx, sm, 0, op, tsAccept); len(vs) > 0 {
			return vs, nil
		}
	}
	for i := 0; i < typedSchemaPins; i++ {
		op := Op{Kind: OpCreate, Cypher: tmplCreatePerson,
			Params: map[string]any{"name": fmt.Sprintf("ts-pin%d", i), "age": typedSchemaPinAge}}
		if vs := probes.EngineWrite(ctx, sm, 0, op, tsAccept); len(vs) > 0 {
			return vs, nil
		}
	}
	// One CONSTRUCTED witness before the first crash can fire, so the
	// post-recovery clauses are never vacuous merely because the cadence had not
	// come round yet.
	return probes.ArmWitness(ctx, sm, 0), nil
}

// typedSchemaLoop is the scenario body over a simulator the caller owns.
//
// It mirrors [Simulator.Run]'s tick sequence deliberately — checkpoint, then the
// crash decision, then the op, then the periodic parity check — so the run is the
// standard deterministic loop with the four batteries inserted, rather than a
// different harness that happens to look similar.
//
// branch is a distinct, documented phase and inlining them is what makes the
// ordering auditable against Simulator.Run.
//
//nolint:gocyclo // the standard tick loop plus four inserted phases; every
func typedSchemaLoop(
	ctx context.Context, sm *Simulator, side *typedSchemaSide,
	cfg TypedSchemaConfig, probes *TypedSchemaProbes,
) (*SimReport, error) {
	if vs, err := typedSchemaPrologue(ctx, sm, probes); err != nil {
		return nil, err
	} else if len(vs) > 0 {
		return sm.report(0, Op{Kind: OpMatch, Cypher: tsOp("prologue")}, vs), nil
	}

	epoch := 0
	seq := 0
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
			epoch++
			if vs := probes.RecoveryPin(sm, tick, epoch, tsPerturbNone); len(vs) > 0 {
				return sm.report(tick, Op{Kind: OpMatch, Cypher: tsOp("recovery-pin")}, vs), nil
			}
			if vs := probes.VerifyWitnesses(ctx, sm, tick, "post-recovery", true, tsPerturbNone); len(vs) > 0 {
				return sm.report(tick, Op{Kind: OpMatch, Cypher: tsOp("post-recovery-witness")}, vs), nil
			}
		}

		seq++
		op := probes.engineOpFor(probes.nextEngineVerdict(), seq)
		want, _ := tsPredictEngine(probes.engineModel, op)
		if vs := probes.EngineWrite(ctx, sm, tick, op, want); len(vs) > 0 {
			return sm.report(tick, op, vs), nil
		}

		spec := probes.nextSpec()
		if vs := probes.SideWrite(tick, side, spec, tsPerturbNone); len(vs) > 0 {
			return sm.report(tick, Op{Kind: OpMatch, Cypher: tsOp("side-" + spec.path.String())}, vs), nil
		}

		if tick%int64(cfg.NodeEvery) == 0 {
			if vs := probes.NodeBattery(tick, side, tsPerturbNone); len(vs) > 0 {
				return sm.report(tick, Op{Kind: OpMatch, Cypher: tsOp("node-battery")}, vs), nil
			}
		}
		if tick%int64(cfg.WitnessEvery) == 0 {
			if vs := probes.ArmWitness(ctx, sm, tick); len(vs) > 0 {
				return sm.report(tick, Op{Kind: OpMatch, Cypher: tsOp("arm-witness")}, vs), nil
			}
		}
		if tick%int64(sm.cfg.CheckEvery) == 0 {
			if vs := sm.checker.Check(tick, sm.oracle, sm.engine); len(vs) > 0 {
				return sm.report(tick, op, vs), nil
			}
		}
	}

	final := int64(cfg.MaxTicks)
	// --- a CONSTRUCTED crash, if the schedule never fired. ---
	//
	// The post-recovery clauses are a coverage claim, and gating them on a crash
	// the SCHEDULE happened to draw would fail runs whose seed simply did not
	// crash inside the budget. So the precondition is constructed.
	if sm.CrashCount() == 0 {
		if report, err := sm.forceCrash(final, tsOp("forced-crash-recovery")); err != nil {
			return nil, err
		} else if report != nil {
			return report, nil
		}
		probes.ev.ForcedCrashes++
		epoch++
		if vs := probes.RecoveryPin(sm, final, epoch, tsPerturbNone); len(vs) > 0 {
			return sm.report(final, Op{Kind: OpMatch, Cypher: tsOp("forced-recovery-pin")}, vs), nil
		}
		if vs := probes.VerifyWitnesses(ctx, sm, final, "post-forced-recovery", true, tsPerturbNone); len(vs) > 0 {
			return sm.report(final, Op{Kind: OpMatch, Cypher: tsOp("forced-recovery-witness")}, vs), nil
		}
	}

	if vs := probes.VerifyWitnesses(ctx, sm, final, "terminal", false, tsPerturbNone); len(vs) > 0 {
		return sm.report(final, Op{Kind: OpMatch, Cypher: tsOp("terminal-witness")}, vs), nil
	}

	// --- arm D2: the pure store/txn path, on its own disk. ---
	obs, err := typedSchemaPureStoreArm(probes.seed)
	if err != nil {
		return nil, err
	}
	probes.ev.PureStoreArms++
	if obs.notApplied {
		probes.ev.PureStoreCommitNotApplied++
	}
	if obs.liveAbsent {
		probes.ev.PureStoreLiveAbsent++
	}
	if obs.resurrected {
		probes.ev.PureStoreResurrected++
	}
	if obs.acceptedSurvived {
		probes.ev.PureStoreAcceptedSurvived++
	}
	probes.ev.Digest = tsDigest(probes.ev.Digest, final, "pure-store",
		tsBoolDigit(obs.resurrected), tsBoolDigit(obs.acceptedSurvived))
	if vs := checkTypedSchemaPureStore(final, obs); len(vs) > 0 {
		return sm.report(final, Op{Kind: OpMatch, Cypher: tsOp("pure-store")}, vs), nil
	}

	// --- the non-vacuity gates. ---
	probes.ev.Crashes = sm.CrashCount()
	probes.ev.Checkpoints = sm.CheckpointCount()
	if vs := probes.ev.Finish(final); len(vs) > 0 {
		return sm.report(final, Op{Kind: OpMatch, Cypher: tsOp("vacuity")}, vs), nil
	}
	// A CheckpointConfig is INERT unless the loop calls maybeCheckpoint, so this
	// gate is what stops the scenario claiming a snapshot-crossing recovery it
	// never produced (rmp #2457/#2464).
	if vs := sm.checkCheckpointsFired(final); len(vs) > 0 {
		return sm.report(final, Op{Kind: OpMatch, Cypher: tsOp("checkpoint-vacuity")}, vs), nil
	}
	return nil, nil
}

// tsBoolDigit folds a bool into the digest as 0 or 1. It is prefixed like every
// other identifier in this file because the package is large and a bare
// boolToInt at package scope invites a collision.
func tsBoolDigit(b bool) int {
	if b {
		return 1
	}
	return 0
}

// typedSchemaScenario is the catalogue entry (rmp #2493).
//
// It carries a custom run override rather than using the standard deterministic
// dispatch because three of its four arms need the live [lpg.Graph] and a
// [schema.Schema] installed on it — neither of which the standard loop exposes to
// a [CheckSelection] check — and because the post-recovery pin has to run
// immediately after each recovery rather than at the end.
func typedSchemaScenario() Scenario {
	return Scenario{
		Name: ScenarioTypedSchema,
		Description: "the typed-schema validator as a runtime ENFORCEMENT hook: accept/reject " +
			"adjudicated against a declaration-table oracle on all five validator-consulting write " +
			"paths, every rejection proven side-effect-free (value, both accessors, population, and the " +
			"property-key registry), the ValidateNode required-property boundary, and the recovery " +
			"asymmetry pinned — a reopened graph carries no validator, and on the pure store/txn path a " +
			"refused value is durable and comes back",
		Mode:        ModeDeterministic,
		DefaultSeed: typedSchemaDefaultSeed,
		MaxTicks:    typedSchemaMaxTicks,
		Crash:       CrashConfig{Enabled: true, CrashProb: 1.0 / 70.0, StabilityWindow: 25},
		Checkpoint:  CheckpointConfig{Enabled: true, Every: 45},
		run:         runTypedSchemaScenario,
	}
}

// runTypedSchemaScenario is the scenario's run override: drive the loop, then
// attach the measured evidence to whatever report came back so an operator
// reading only the log sees what the run exercised — including the pure-store
// measurement the scenario pins rather than endorses.
func runTypedSchemaScenario(ctx context.Context, seed uint64) (*SimReport, error) {
	ev, report, err := RunTypedSchema(ctx, DefaultTypedSchemaConfig(seed))
	if err != nil {
		return nil, err
	}
	if report == nil {
		return nil, nil
	}
	report.Scenario = ScenarioTypedSchema
	report.Mode = ModeDeterministic
	report.FailedOp.Cypher = report.FailedOp.Cypher + " " + ev.String()
	return report, nil
}
