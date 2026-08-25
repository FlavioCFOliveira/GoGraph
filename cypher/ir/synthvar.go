package ir

import "strings"

// IsSyntheticVar reports whether name was minted by GoGraph rather than written
// by the query author.
//
// The test is exact, not heuristic. Every synthetic name GoGraph mints carries
// [anonVarPrefix], and that prefix is not a legal openCypher identifier start, so
// no user variable can ever collide with one. This covers all three families:
// the translation walk's own `__anon_N` ([translator.freshAnonVar]) and its
// suffixed forms (`__anon_N_to_x`, `__anon_N_rel_x`, `__anon_N_vle_elem`), the
// subquery pre-naming pass's `__anon_sq_N` ([anonSubqueryVarPrefix]), and the
// physical builder's schema keys (`__anon_rel_3`, `__anon_to_4`).
func IsSyntheticVar(name string) bool {
	return strings.HasPrefix(name, anonVarPrefix)
}

// UserNamed reports whether v is a name the QUERY AUTHOR wrote, as opposed to
// absent or synthetic.
//
// It exists because a plain `v != nil` test conflates two different questions.
// "Does this entity have a name?" is a syntactic question; "did the user bind
// this entity, so that something else can observe it?" is a semantic one, and it
// is the semantic one that fast-path recognisers actually need. The two were the
// same thing only for as long as nothing named an entity on the user's behalf.
// [NameSubqueryAnonymousEntities] does exactly that, and it broke every guard
// spelled the syntactic way (rmp #2508): the degree-rewrite and labelled-hop
// recognisers refuse a named relationship, so they silently stopped firing and
// fell back to driving an inner plan per outer row.
func UserNamed(v *string) bool {
	return v != nil && !IsSyntheticVar(*v)
}
