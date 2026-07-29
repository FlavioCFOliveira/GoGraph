package main

import "github.com/FlavioCFOliveira/GoGraph/graph/adjlist"

// lpgConfig returns the adjacency-list configuration shared by every
// subcommand. The social-network model is directional throughout —
// FOLLOWS, AUTHORED, ON, REPLY_OF and LIKED all have a well-defined
// direction — so the backend is constructed with Directed: true.
// Multigraph: true is required because this CLI issues openCypher writes,
// whose data model is a multigraph: a CREATE always adds a relationship,
// including a second relationship between an existing node pair. The helper
// centralises this choice so a future change is single-edit.
func lpgConfig() adjlist.Config {
	return adjlist.Config{Directed: true, Multigraph: true}
}

// Node labels used by the social-network fixture and by every Cypher
// statement issued by this CLI. Kept as exported package-level constants
// so other files in the package reference one name per concept and a
// future rename surfaces compilation errors in a single sweep.
const (
	labelUser    = "User"
	labelPost    = "Post"
	labelComment = "Comment"
	// labelVerified and labelFirehose exist only for the `plandiff` subcommand's
	// symmetric-anchor-swap scenario (#2154). A single :Firehose account with a very
	// large FOLLOWS out-adjacency, against a small :Verified population of which it
	// follows exactly one, is the skew that makes the reverse anchor genuinely cheaper
	// — and it is a shape a real social product has, not a contrivance.
	labelVerified = "Verified"
	labelFirehose = "Firehose"
	// labelCore marks the mutually-following community the `plandiff` triangle
	// scenario anchors on. A triangle materialises Theta(n*d^2) intermediate rows, so
	// anchoring on the whole user population costs seconds per run at a realistic
	// out-degree; :Core keeps the one-shot command fast and asks the more meaningful
	// question of triangles WITHIN a community.
	labelCore = "Core"
	// labelSeed is the small slice of :Core that anchors the triangle scenario. A
	// triangle's cost is cubic in the out-degree, so its anchor must stay small for the
	// subcommand to remain a one-shot command.
	labelSeed = "Seed"
)

// Relationship types. The polymorphic edges (AUTHORED, LIKED) are
// distinguished by the label of their target node, not by their type
// name, which matches the convention used throughout GoGraph's Cypher
// examples.
const (
	relFollows  = "FOLLOWS"  // (:User)-[:FOLLOWS]->(:User)
	relAuthored = "AUTHORED" // (:User)-[:AUTHORED]->(:Post|:Comment)
	relOn       = "ON"       // (:Comment)-[:ON]->(:Post)
	relReplyOf  = "REPLY_OF" // (:Comment)-[:REPLY_OF]->(:Comment)
	relLiked    = "LIKED"    // (:User)-[:LIKED]->(:Post|:Comment)
)
