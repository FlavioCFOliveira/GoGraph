// Package synthkey defines the synthetic node keys the Cypher engine mints for
// the nodes its CREATE and MERGE operators make, for the two packages that must
// agree on them: github.com/FlavioCFOliveira/GoGraph/cypher/exec, which mints
// them, and github.com/FlavioCFOliveira/GoGraph/graph/lpg, which gives back the
// ones an aborted transaction minted (rmp #3119).
//
// # Why this package exists
//
// The rule is shared across two packages that cannot import each other in that
// direction, and exporting it from either would enlarge the module's PUBLIC API
// for a convention that is internal to the engine. Go's internal-package rule
// makes everything below unimportable outside this module — the same reasoning
// as github.com/FlavioCFOliveira/GoGraph/internal/planseam.
//
// # The contract a synthetic key carries
//
// A synthetic key is Prefix followed by an operator infix (empty for CREATE,
// "merge_" for MERGE) and a hexadecimal value drawn from the target graph's
// lpg.KeySequence. The engine never shows it to a Cypher caller, and the minting
// probe guarantees no other statement is handed the same key, so the only writer
// that knows it is the transaction that minted it. A caller of the lpg API that
// writes keys with this prefix itself steps into the engine's namespace and is
// outside that guarantee.
//
// Everything here is immutable and safe for concurrent use.
package synthkey

import "strings"

// Prefix is the fixed prefix of every synthetic node key.
const Prefix = "__cx_"

// Is reports whether k is a synthetic node key.
func Is(k string) bool { return strings.HasPrefix(k, Prefix) }
