package exec

// merge_outer_target.go — resolving an ON CREATE / ON MATCH SET action whose
// target variable the MERGE clause does not itself bind.
//
// A MERGE branch may name any variable in scope, not only the ones its own
// pattern introduces:
//
//	MATCH (m:M) MERGE (a:P {name:'a'})-[r:T]->(b:P {name:'b'}) ON CREATE SET m.w = 7
//
// Both [MergePattern] and the node-only [Merge] used to resolve an action target
// through a lookup that could only see part of the row: MergePattern searched its
// chain positions and its chain hops, and Merge understood node values only. A
// target outside what each could see was skipped rather than written and rather
// than raising, so the statement reported success, counted no side effect, and
// the property read back null (rmp #2511).
//
// [resolveRowEntity] closes that gap by resolving the target against the FULL row
// scope, using the same evidence the standalone SET operator uses:
// [SetProperty.resolveEntity]'s value-kind dispatch, and [resolveRelBinding] for
// the endpoint/handle triplet an [Expand] leaves in the row for a relationship
// variable. It is consulted only AFTER each operator's own pattern-local lookup
// misses, so every target a MERGE pattern does bind keeps its existing writer and
// its existing behaviour.

import (
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// resolveRowEntity resolves the node or relationship bound to varName from row,
// using schema for the column and relCols for the endpoint/handle triplet of a
// relationship variable bound by a preceding clause.
//
// It reports ok=false — never an error — for every shape a MERGE action must
// tolerate rather than abort on: a variable absent from the row scope, a column
// beyond the row's width (the MERGE fired against the empty driving row), a null
// binding (openCypher 9 §3.5.4: a mutating clause silently ignores a null
// entity), an entity whose identity no longer resolves in the graph, and any
// value kind that is not an entity at all.
//
// The value-kind dispatch mirrors [SetProperty.resolveEntity]: a
// [expr.RelationshipValue] is self-describing (its ID is the instance's stable
// handle since rmp #2317), a [expr.NodeValue] carries its NodeID, and a bare
// [expr.IntegerValue] is ambiguous — it is the edge handle for a variable listed
// in relCols and a NodeID otherwise.
func resolveRowEntity(
	varName string,
	schema map[string]int,
	relCols map[string]RelCols,
	row Row,
	mut GraphMutator,
) (entityBinding, bool) {
	col, ok := schema[varName]
	if !ok || col < 0 || col >= len(row) {
		return entityBinding{}, false
	}
	v := row[col]
	if v == nil || expr.IsNull(v) {
		return entityBinding{}, false
	}
	switch t := v.(type) {
	case expr.RelationshipValue:
		srcKey, srcOK := mut.ResolveNodeLabel(graph.NodeID(t.StartID))
		dstKey, dstOK := mut.ResolveNodeLabel(graph.NodeID(t.EndID))
		if !srcOK || !dstOK {
			return entityBinding{}, false
		}
		return entityBinding{isRel: true, relSrcKey: srcKey, relDstKey: dstKey, relHandle: t.ID}, true
	case expr.NodeValue:
		nodeKey, resolved := mut.ResolveNodeLabel(graph.NodeID(t.ID))
		if !resolved {
			return entityBinding{}, false
		}
		return entityBinding{nodeKey: nodeKey}, true
	case expr.IntegerValue:
		if rc, isRel := relCols[varName]; isRel {
			ent, err := resolveRelBinding(&rc, row, mut)
			if err != nil {
				return entityBinding{}, false
			}
			return ent, true
		}
		nodeKey, resolved := mut.ResolveNodeLabel(graph.NodeID(t))
		if !resolved {
			return entityBinding{}, false
		}
		return entityBinding{nodeKey: nodeKey}, true
	default:
		return entityBinding{}, false
	}
}
