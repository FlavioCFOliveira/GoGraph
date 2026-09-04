package server

// entity_struct.go — Bolt structure encoding for graph entities (rmp #2189).
//
// Node, Relationship and Path are STRUCTURES in the Bolt protocol, not maps. The
// server used to emit them as PackStream maps, so the official neo4j-go-driver could
// not materialise them as dbtype.Node / dbtype.Relationship / dbtype.Path at all: a
// raw wire capture showed a3/a5/a2 TinyMaps where b4 4e, b8 52 and b3 50 structures
// belong, and that was a large part of the 13 hard failures the round-3 audit measured
// across 37 driver checks.
//
// # The authoritative contract
//
// The layouts below are transcribed from the decoder that has to read them — the
// neo4j-go-driver v5.28.4 hydrator (neo4j/internal/bolt/hydrator.go, and
// neo4j/internal/bolt/path.go for the path index semantics), the same source the
// temporal tags in exprValueToPackstream were taken from. Field counts are asserted by
// the driver (h.assertLength), so an off-by-one is a protocol error, not a silent
// mis-read:
//
//	'N' 0x4E Node                 Bolt <5: [id, labels, properties]
//	                              Bolt 5+: [id, labels, properties, element_id]
//	'R' 0x52 Relationship         Bolt <5: [id, start_id, end_id, type, properties]
//	                              Bolt 5+: + [element_id, start_element_id, end_element_id]
//	'r' 0x72 UnboundRelationship  Bolt <5: [id, type, properties]
//	                              Bolt 5+: [id, type, properties, element_id]
//	'P' 0x50 Path                 [nodes, unbound_relationships, indices]
//
// # element_id
//
// element_id is the entity's durable id rendered in decimal, which is exactly what the
// Cypher elementId() function returns (cypher/funcs/completeness.go). Keeping the two
// identical is the point: a client that reads elementId() in a projection and a client
// that reads Node.ElementId off the wire must see the same string for the same entity.
// It is stable for the lifetime of the entity, and the driver's own pre-5 fallback
// synthesises the same decimal form, so the two protocol versions agree.
//
// # Path indices
//
// Path is the subtle one. It carries a node list, an unbound-relationship list, and an
// `indices` list of (relationship, node) PAIRS — one pair per hop, in traversal order:
//
//   - the relationship index is ONE-based into the relationship list. Positive means
//     the hop traverses the relationship in its natural direction (the previous node is
//     its start); NEGATIVE means it is traversed in reverse, and the driver resolves it
//     as relNodes[(-index)-1].
//   - the node index is ZERO-based into the node list and names the hop's END node.
//
// This encoder emits the path's nodes and relationships in path order rather than
// deduplicating them, so hop i uses node index i+1 and relationship index ±(i+1). That
// is within the format — the driver only indexes into the lists, and it returns the
// node list verbatim as Path.Nodes — and it keeps the encoding O(n) with no identity
// map. Direction is decided per hop by comparing the relationship's StartID against the
// hop's source node, which is what makes a reverse-traversed hop round-trip with its
// endpoints the right way round.

import (
	"strconv"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// Bolt structure tags for graph entities.
const (
	tagNode                = 0x4E // 'N'
	tagRelationship        = 0x52 // 'R'
	tagUnboundRelationship = 0x72 // 'r'
	tagPath                = 0x50 // 'P'
)

// boltElementID renders an entity id as its element_id string: the durable id in
// decimal, identical to what the Cypher elementId() function returns.
func boltElementID(id uint64) string { return strconv.FormatUint(id, 10) }

// propsToPackstream converts an entity property map to PackStream values.
func propsToPackstream(props map[string]expr.Value, boltMajor uint8) map[string]packstream.Value {
	out := make(map[string]packstream.Value, len(props))
	for k, pv := range props {
		out[k] = exprValueToPackstream(pv, boltMajor)
	}
	return out
}

// nodeToStruct encodes a node as the Bolt 'N' structure.
func nodeToStruct(x expr.NodeValue, boltMajor uint8) packstream.Struct {
	labels := make([]packstream.Value, len(x.Labels))
	for i, l := range x.Labels {
		labels[i] = l
	}
	fields := []packstream.Value{
		//nolint:gosec // G115: Bolt types an entity id as a signed Integer. A NodeID is packNodeID(shard,idx) with idx=len(s.reverse) (graph/mapper.go:346); id>=1<<63 needs 2^55 nodes in one of 256 shards
		int64(x.ID),
		labels,
		propsToPackstream(x.Properties, boltMajor),
	}
	if boltMajor >= 5 {
		fields = append(fields, boltElementID(uint64(x.ID)))
	}
	return packstream.Struct{Tag: tagNode, Fields: fields}
}

// relationshipToStruct encodes a relationship as the Bolt 'R' structure, which carries
// its endpoints.
func relationshipToStruct(x expr.RelationshipValue, boltMajor uint8) packstream.Struct {
	fields := []packstream.Value{
		//nolint:gosec // G115: Bolt types an entity id as a signed Integer. Relationship handles are handleSeq.Add(1) (graph/adjlist/adjlist.go:643,218): they start at 1 and are never reused, so 1<<63 needs 2^63 AddEdge calls
		int64(x.ID),
		//nolint:gosec // G115: Bolt types an entity id as a signed Integer. A NodeID is packNodeID(shard,idx) with idx=len(s.reverse) (graph/mapper.go:346); id>=1<<63 needs 2^55 nodes in one of 256 shards
		int64(x.StartID),
		//nolint:gosec // G115: Bolt types an entity id as a signed Integer. A NodeID is packNodeID(shard,idx) with idx=len(s.reverse) (graph/mapper.go:346); id>=1<<63 needs 2^55 nodes in one of 256 shards
		int64(x.EndID),
		x.Type,
		propsToPackstream(x.Properties, boltMajor),
	}
	if boltMajor >= 5 {
		fields = append(fields,
			boltElementID(uint64(x.ID)),
			boltElementID(uint64(x.StartID)),
			boltElementID(uint64(x.EndID)),
		)
	}
	return packstream.Struct{Tag: tagRelationship, Fields: fields}
}

// unboundRelationshipToStruct encodes a relationship as the Bolt 'r' structure — the
// form a Path carries, which omits the endpoints because the path's indices supply them.
func unboundRelationshipToStruct(x expr.RelationshipValue, boltMajor uint8) packstream.Struct {
	fields := []packstream.Value{
		//nolint:gosec // G115: Bolt types an entity id as a signed Integer. Relationship handles are handleSeq.Add(1) (graph/adjlist/adjlist.go:643,218): they start at 1 and are never reused, so 1<<63 needs 2^63 AddEdge calls
		int64(x.ID),
		x.Type,
		propsToPackstream(x.Properties, boltMajor),
	}
	if boltMajor >= 5 {
		fields = append(fields, boltElementID(uint64(x.ID)))
	}
	return packstream.Struct{Tag: tagUnboundRelationship, Fields: fields}
}

// pathToStruct encodes a path as the Bolt 'P' structure: the node list, the
// unbound-relationship list, and the (relationship, node) index pairs described in the
// file comment.
//
// A path with no relationships — a single disconnected node, which openCypher produces
// for a zero-length path — emits its node list with an EMPTY index list, which is what
// the driver's buildPath expects for that case (it returns the nodes with no
// relationships rather than treating it as malformed).
func pathToStruct(x expr.PathValue, boltMajor uint8) packstream.Struct {
	nodes := make([]packstream.Value, len(x.Nodes))
	for i, n := range x.Nodes {
		nodes[i] = nodeToStruct(n, boltMajor)
	}
	rels := make([]packstream.Value, len(x.Relationships))
	for i, r := range x.Relationships {
		rels[i] = unboundRelationshipToStruct(r, boltMajor)
	}

	// One (relationship, node) pair per hop. Hop i joins Nodes[i] to Nodes[i+1] via
	// Relationships[i]; a hop whose relationship does not START at Nodes[i] is
	// traversed in reverse and takes a negative relationship index.
	indices := make([]packstream.Value, 0, 2*len(x.Relationships))
	for i, r := range x.Relationships {
		if i+1 >= len(x.Nodes) {
			// Malformed path (more relationships than hops). Emit only the pairs the
			// node list can support rather than indexing out of range; the driver
			// requires an even index count, which this preserves.
			break
		}
		relIdx := int64(i + 1)
		if r.StartID != x.Nodes[i].ID {
			relIdx = -relIdx
		}
		indices = append(indices, relIdx, int64(i+1))
	}

	return packstream.Struct{Tag: tagPath, Fields: []packstream.Value{nodes, rels, indices}}
}
