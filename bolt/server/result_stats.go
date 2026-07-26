package server

// result_stats.go — Bolt result statistics metadata (rmp #2190).
//
// The server never sent the `stats` field, so after a successful CREATE the official
// driver reported NodesCreated=0 and ContainsUpdates=false, and a MERGE that created was
// indistinguishable from one that matched. That is worse than an error: it is
// plausible-looking WRONG data.
//
// # The wire contract, read from the decoder
//
// The key names and value types are transcribed from the driver that must read them
// (neo4j-go-driver v5.28.4), not recalled:
//
//   - the counter names are the constants in neo4j/db/summary.go: nodes-created,
//     nodes-deleted, relationships-created, relationships-deleted, properties-set,
//     labels-added, labels-removed, indexes-added, indexes-removed, constraints-added,
//     constraints-removed, system-updates;
//   - `contains-updates` must be a BOOLEAN on the wire and every other key an INTEGER.
//     The hydrator's parseStatValue switches on the key: contains-updates and
//     contains-system-updates are read with unp.Bool(), and EVERY OTHER key with
//     unp.Int(). So an unknown key would be misread as an integer — which is why this
//     encoder emits only names from that list.
//
// # openCypher has one counter Bolt does not
//
// openCypher counts a property REMOVAL as its own side effect, `-properties` (see
// [exec.QueryCounters]). Bolt has no properties-removed: it carries only properties-set.
// Both are property writes, so they are summed into properties-set here. This is the one
// lossy step in the mapping, and it lives here — at the protocol boundary — rather than
// in the counters, which stay faithful to the spec.
//
// # Only non-zero counters are sent
//
// Neo4j omits zero counters, and the driver's successStats returns nil for an empty map,
// so a read-only statement's SUCCESS is byte-identical to before this change. That also
// means `contains-updates` is sent only when something actually changed.

import (
	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
)

// Bolt statistics key names, from neo4j/db/summary.go.
const (
	statNodesCreated         = "nodes-created"
	statNodesDeleted         = "nodes-deleted"
	statRelationshipsCreated = "relationships-created"
	statRelationshipsDeleted = "relationships-deleted"
	statPropertiesSet        = "properties-set"
	statLabelsAdded          = "labels-added"
	statLabelsRemoved        = "labels-removed"
	statIndexesAdded         = "indexes-added"
	statIndexesRemoved       = "indexes-removed"
	statConstraintsAdded     = "constraints-added"
	statConstraintsRemoved   = "constraints-removed"
	statContainsUpdates      = "contains-updates"
)

// resultStats renders a statement's write effects as the Bolt `stats` metadata map, or
// nil when the statement changed nothing.
//
// nil in — a read-only statement, which reports no counters at all — yields nil out, as
// does a write that applied nothing: in both cases there is no statistics map on the
// wire, so `ResultSummary.Counters().ContainsUpdates()` is false either way, which is
// correct. The distinction between the two is available to an embedder through
// [cypher.Result.Counters] and is not something Bolt can express.
func resultStats(c *exec.QueryCounters) map[string]packstream.Value {
	if c == nil || !c.ContainsUpdates() {
		return nil
	}
	stats := make(map[string]packstream.Value, 6)
	put := func(key string, n int64) {
		if n != 0 {
			stats[key] = n
		}
	}
	put(statNodesCreated, c.NodesCreated)
	put(statNodesDeleted, c.NodesDeleted)
	put(statRelationshipsCreated, c.RelationshipsCreated)
	put(statRelationshipsDeleted, c.RelationshipsDeleted)
	// openCypher's +properties and -properties both map onto Bolt's single
	// properties-set, which is the only lossy step in this mapping.
	put(statPropertiesSet, c.PropertiesSet+c.PropertiesRemoved)
	put(statLabelsAdded, c.LabelsAdded)
	put(statLabelsRemoved, c.LabelsRemoved)
	put(statIndexesAdded, c.IndexesAdded)
	put(statIndexesRemoved, c.IndexesRemoved)
	put(statConstraintsAdded, c.ConstraintsAdded)
	put(statConstraintsRemoved, c.ConstraintsRemoved)
	// contains-updates is a BOOLEAN on the wire; the driver reads it with unp.Bool().
	// It is only reached when something changed, so it is always true here.
	stats[statContainsUpdates] = true
	return stats
}
