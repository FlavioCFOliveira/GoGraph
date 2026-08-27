package sim

// delete_contract.go — DST adjudication of the NON-DETACH `DELETE n` contract
// (rmp #2462).
//
// Every DST scenario deletes through `DETACH DELETE`, so the plain `DELETE n`
// form — and the openCypher rule that it must REFUSE a node that still has
// relationships — was never exercised. This probe closes that gap the way the
// constraint scenarios adjudicate UNIQUE and NOT NULL: it predicts the outcome
// from the shadow model and holds the engine to it.
//
// The contract, pinned against cypher/delete_rejects_connected_test.go:
//
//   - `DELETE n` on a node of degree 0 COMMITS, reporting exactly one
//     -nodes and nothing else.
//   - `DELETE n` on a node of degree > 0 is REFUSED with
//     [exec.ErrDeleteNodeHasRelationships] ("cannot delete node with existing
//     relationships; use DETACH DELETE"), and applies NOTHING: the node and its
//     edges must still be there afterwards.
//
// Degree is read from the oracle's adjacency ([GraphOracle.incidentEdges]), so
// both arms are oracle-PREDICTED rather than observed: the engine is never
// asked what it thinks the degree is.
//
// The typed error arrives on the DRAIN, not from the write call: the engine
// accepts the statement and fails while producing rows, so
// [EngineAdapter.RunWrite] returns a nil error and `res.Err()` carries
// [exec.ErrDeleteNodeHasRelationships]. The probe therefore inspects BOTH, and
// a regression that moved the refusal to either side alone would still be
// caught.

import (
	"context"
	"errors"
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
)

// tmplDeleteNode is the non-detach node deletion the probe adjudicates. It is
// deliberately the same MATCH shape as [tmplDetachDelete] so the only variable
// between the two contracts is the DETACH keyword.
const tmplDeleteNode = "MATCH (n:Person {name:$name}) DELETE n"

// tmplCountPersonByName counts the Persons carrying a given name, used to prove
// a refused DELETE left its target in place.
const tmplCountPersonByName = "MATCH (n:Person {name:$name}) RETURN count(n)"

// deleteContractStats records which arms of the DELETE contract actually fired
// during a run, so the terminal gate can prove the probe was not vacuous. A
// scenario whose population never held a degree-0 node (or never held a
// connected one) compared nothing and must not pass silently.
//
// # Concurrency contract
//
// deleteContractStats is NOT safe for concurrent use; it is updated from the
// single simulation goroutine.
type deleteContractStats struct {
	accepted int64 // degree-0 `DELETE n` committed, counters adjudicated
	refused  int64 // degree>0 `DELETE n` refused with the typed error
}

// probeDeleteContract runs one accept arm and one reject arm of the non-detach
// DELETE contract against the live engine and adjudicates both against the
// oracle's adjacency. It MUTATES both engine and oracle on the accept arm (the
// deleted node is removed from the model), so it must be called from the
// scenario loop, never from a read-only checker.
//
// Targets are chosen deterministically — the lexicographically first eligible
// name from [GraphOracle.NodeNames] — and the probe draws no randomness, so a
// run that includes it stays bit-reproducible.
//
// Either arm is skipped when the population holds no eligible node; st records
// what actually ran and [deleteContractVacuity] gates on it at the end.
func probeDeleteContract(ctx context.Context, tick int64, oracle *GraphOracle, engine *EngineAdapter, st *deleteContractStats) []Violation {
	var vs []Violation
	connected, isolated := deleteContractTargets(oracle)

	// Reject arm first: a connected node must survive its own DELETE, and running
	// it before the accept arm keeps the two targets independent.
	if connected != "" {
		vs = append(vs, probeDeleteRefused(ctx, tick, connected, oracle, engine, st)...)
	}
	if isolated != "" {
		vs = append(vs, probeDeleteAccepted(ctx, tick, isolated, oracle, engine, st)...)
	}
	return vs
}

// deleteContractTargets picks the probe's two targets from the oracle: the
// lexicographically first Person with at least one incident edge (the reject
// arm) and the first with none (the accept arm). Either is "" when the model
// holds no such node.
func deleteContractTargets(oracle *GraphOracle) (connected, isolated string) {
	for _, name := range oracle.NodeNames() {
		id, ok := oracle.byName[name]
		if !ok {
			continue
		}
		if oracle.incidentEdges(id) > 0 {
			if connected == "" {
				connected = name
			}
		} else if isolated == "" {
			isolated = name
		}
		if connected != "" && isolated != "" {
			return connected, isolated
		}
	}
	return connected, isolated
}

// probeDeleteRefused asserts that `DELETE n` on a node the oracle knows to be
// connected is refused with [exec.ErrDeleteNodeHasRelationships] and applies
// nothing. The oracle is deliberately NOT advanced: a refused write changes no
// modelled state, so the per-tick parity checker keeps guarding the pair.
func probeDeleteRefused(ctx context.Context, tick int64, name string, oracle *GraphOracle, engine *EngineAdapter, st *deleteContractStats) []Violation {
	params := map[string]any{"name": name}
	degree := oracle.incidentEdges(oracle.byName[name])

	_, err := runWriteCollect(ctx, engine, tmplDeleteNode, params)
	if err == nil {
		return []Violation{{
			Kind: ViolationACIDConsistency, Tick: tick, Op: "non-detach DELETE (connected)",
			Message: fmt.Sprintf(
				"DELETE of %q COMMITTED although the oracle models %d incident edge(s); openCypher requires "+
					"a connected node to be refused with %v (use DETACH DELETE)", name, degree, exec.ErrDeleteNodeHasRelationships),
		}}
	}
	if !errors.Is(err, exec.ErrDeleteNodeHasRelationships) {
		return []Violation{{
			Kind: ViolationACIDConsistency, Tick: tick, Op: "non-detach DELETE (connected)",
			Message: fmt.Sprintf(
				"DELETE of %q (%d incident edge(s)) was refused with the WRONG error: got %v, want %v",
				name, degree, err, exec.ErrDeleteNodeHasRelationships),
		}}
	}

	// The refusal must have applied nothing: the target must still be there.
	n, cerr := scalarCountWithParams(ctx, engine, tmplCountPersonByName, params)
	switch {
	case cerr != nil:
		return []Violation{{
			Kind: ViolationGraphIntegrity, Tick: tick, Op: "non-detach DELETE (connected)",
			Message: fmt.Sprintf("post-refusal existence probe for %q failed: %v", name, cerr),
		}}
	case n != 1:
		return []Violation{{
			Kind: ViolationACIDAtomicity, Tick: tick, Op: "non-detach DELETE (connected)",
			Message: fmt.Sprintf(
				"refused DELETE of %q was not atomic: the node count for that name is %d, want 1 — "+
					"a rejected write must leave the graph unchanged", name, n),
		}}
	}
	st.refused++
	return nil
}

// probeDeleteAccepted asserts that `DELETE n` on a node the oracle knows to be
// isolated commits, reports exactly one -nodes through the per-op counters
// oracle, and actually removes the node. The oracle is advanced so the model
// stays in lock-step with the engine's committed set.
func probeDeleteAccepted(ctx context.Context, tick int64, name string, oracle *GraphOracle, engine *EngineAdapter, st *deleteContractStats) []Violation {
	params := map[string]any{"name": name}
	op := Op{Kind: OpDelete, Cypher: tmplDeleteNode, Params: params}

	counters, err := runWriteCollect(ctx, engine, tmplDeleteNode, params)
	if err != nil {
		return []Violation{{
			Kind: ViolationACIDConsistency, Tick: tick, Op: "non-detach DELETE (isolated)",
			Message: fmt.Sprintf(
				"DELETE of %q was REFUSED although the oracle models zero incident edges: %v — "+
					"a node with no relationships must delete without DETACH", name, err),
		}}
	}

	// Counters are adjudicated BEFORE the oracle advances: the expected effect is
	// a function of the pre-op model, exactly as [CheckOpCounters] requires.
	vs := CheckOpCounters(tick, op, true, counters, oracle)
	oracle.ApplyDelete(tmplDeleteNode, params)

	n, cerr := scalarCountWithParams(ctx, engine, tmplCountPersonByName, params)
	switch {
	case cerr != nil:
		vs = append(vs, Violation{
			Kind: ViolationGraphIntegrity, Tick: tick, Op: "non-detach DELETE (isolated)",
			Message: fmt.Sprintf("post-delete existence probe for %q failed: %v", name, cerr),
		})
	case n != 0:
		vs = append(vs, Violation{
			Kind: ViolationOracleDeviation, Tick: tick, Op: "non-detach DELETE (isolated)",
			Message: fmt.Sprintf(
				"committed DELETE of %q left %d node(s) with that name behind, want 0", name, n),
		})
	}
	if len(vs) == 0 {
		st.accepted++
	}
	return vs
}

// deleteContractVacuity is the terminal assert-something-was-seen gate for the
// DELETE contract: both arms must have fired at least once. A run in which the
// population never offered a connected node (or never an isolated one) proved
// nothing about the contract and must not report a silent pass.
func deleteContractVacuity(tick int64, st *deleteContractStats) []Violation {
	var missing []string
	if st.accepted == 0 {
		missing = append(missing, "accepted DELETE of an isolated node")
	}
	if st.refused == 0 {
		missing = append(missing, "refused DELETE of a connected node")
	}
	if len(missing) == 0 {
		return nil
	}
	return []Violation{{
		Kind: ViolationVacuousRun, Tick: tick, Op: "non-detach DELETE non-vacuity",
		Message: fmt.Sprintf(
			"vacuous run: the non-detach DELETE probe never exercised: %v — the contract was never "+
				"adjudicated (accepted=%d refused=%d)", missing, st.accepted, st.refused),
	}}
}

// runWriteCollect executes a mutating statement and returns the engine's
// per-statement counters together with the operative error. The error is taken
// from the write call when it refuses up front, otherwise from the DRAIN —
// which is where a statement that fails mid-execution (such as a DELETE of a
// connected node) reports it. Counters are read from the same drained result,
// never from a second query.
func runWriteCollect(ctx context.Context, engine *EngineAdapter, query string, params map[string]any) (*exec.QueryCounters, error) {
	res, err := engine.RunWrite(ctx, query, params)
	if err != nil {
		return nil, err
	}
	for res.Next() { //nolint:revive // draining is the point
	}
	drainErr := res.Err()
	var counters *exec.QueryCounters
	if cr, ok := res.(counterReporter); ok {
		counters = cr.Counters()
	}
	_ = res.Close()
	return counters, drainErr
}

// scalarCountWithParams runs a single-column count query with parameters and
// returns the first row's integer value. A query that yields no row reports 0.
func scalarCountWithParams(ctx context.Context, engine *EngineAdapter, query string, params map[string]any) (int64, error) {
	res, err := engine.Run(ctx, query, params)
	if err != nil {
		return 0, err
	}
	var got int64
	if res.Next() {
		got, _ = res.IntAt(0)
	}
	for res.Next() { //nolint:revive // draining is the point
	}
	drainErr := res.Err()
	_ = res.Close()
	if drainErr != nil {
		return 0, drainErr
	}
	return got, nil
}
