package main

// Node labels used by the social-network fixture and by every Cypher
// statement issued by this CLI. Kept as exported package-level constants
// so other files in the package reference one name per concept and a
// future rename surfaces compilation errors in a single sweep.
const (
	labelUser    = "User"
	labelPost    = "Post"
	labelComment = "Comment"
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
