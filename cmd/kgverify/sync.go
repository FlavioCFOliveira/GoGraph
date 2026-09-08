package main

import (
	"fmt"
	"sort"
	"strings"
)

// syncBatch is how many declarations one emitted statement carries.
//
// Every statement must fit two server limits at once: the 1048576-byte
// statement cap and the 5-second execution budget. 60 keeps the widest observed
// statement near 13 KB, which clears both with three orders of magnitude to
// spare, while keeping the number of round trips to the low hundreds.
const syncBatch = 60

// cypherString renders s as a Cypher single-quoted literal.
//
// Only the backslash and the quote need escaping, and the backslash must go
// first or the escape introduced for the quote would itself be escaped. The
// round trip is verified against the live engine rather than assumed: a title
// holding both characters reads back byte-identical.
func cypherString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `'`, `\'`)
	return "'" + r.Replace(s) + "'"
}

// effectivePackage is the package a node really belongs to.
//
// A node whose declaration identity matches the tree is placed by the TREE, not
// by the pkg property it happens to carry: that is the whole point of the
// repair pass, since the property may be null or name a package that was
// deleted. Only when no declaration matches does the node's own key decide,
// and an unplaceable node is reported rather than guessed at.
func effectivePackage(n *symNode, declByKey map[string]*decl, idx *packageKeyIndex) (string, bool) {
	if d, ok := declByKey[n.key()]; ok {
		return d.ImportPath, true
	}
	return idx.resolve(n.Pkg)
}

// buildSync produces the two statement families that bring the graph to
// per-package parity, and it is the answer to rmp #2719's "drive it from a
// parser over top-level declarations, not by hand".
//
// Every name, package, file and receiver in the output came out of go/parser.
// Nothing here can introduce a fabrication, because a name that is not a
// declaration is never reachable: the create pass iterates the inventory, and
// the repair pass only ever writes a package path taken from the declaration
// the node already matches.
//
// The two passes must run in this order. Repair first, because a node whose pkg
// is null or names a deleted package is invisible to its real package; MERGE-ing
// the same declaration before that node is repaired would create a SECOND node
// for it rather than matching it, turning a mis-keyed node into a duplicate.
func buildSync(inv *inventory, nodes []symNode, commit, gdate string) (repair, create []string) {
	idx := newPackageKeyIndex(inv)

	declByKey := make(map[string]*decl, len(inv.decls))
	for i := range inv.decls {
		d := &inv.decls[i]
		if _, dup := declByKey[d.key()]; !dup {
			declByKey[d.key()] = d
		}
	}

	haveKey := make(map[string]struct{}, len(nodes))
	modelled := map[string]struct{}{}
	type fix struct {
		node *symNode
		want string
	}
	var fixes []fix
	for i := range nodes {
		n := &nodes[i]
		if _, isSym := symbolLabels[n.Label]; !isSym {
			continue
		}
		haveKey[n.key()] = struct{}{}
		eff, placed := effectivePackage(n, declByKey, idx)
		if !placed {
			continue
		}
		modelled[eff] = struct{}{}
		if cur, ok := idx.resolve(n.Pkg); !ok || cur != eff {
			fixes = append(fixes, fix{node: n, want: eff})
		}
	}

	sort.Slice(fixes, func(i, j int) bool { return fixes[i].node.key() < fixes[j].node.key() })
	for i := 0; i < len(fixes); i += syncBatch {
		end := min(i+syncBatch, len(fixes))
		items := make([]string, 0, end-i)
		for _, f := range fixes[i:end] {
			items = append(items, fmt.Sprintf("{n:%s,f:%s,r:%s,p:%s}",
				cypherString(f.node.Name), cypherString(f.node.File),
				cypherString(f.node.Recv), cypherString(f.want)))
		}
		repair = append(repair, fmt.Sprintf(
			"UNWIND [%s] AS d MATCH (x) WHERE %s AND x.name = d.n AND coalesce(x.file,'') = d.f "+
				"AND coalesce(x.recv,'') = d.r SET x.pkg = d.p, x.gitCommit = %s, x.gitDate = %s",
			strings.Join(items, ","), symbolLabelPredicate("x"),
			cypherString(commit), cypherString(gdate)))
	}

	// The create pass is grouped by LABEL because a Cypher MERGE cannot take a
	// label from a parameter: one statement family per symbol tier is the only
	// shape the engine admits.
	byLabel := map[declKind][]*decl{}
	emitted := map[string]struct{}{}
	for i := range inv.decls {
		d := &inv.decls[i]
		if _, ok := modelled[d.ImportPath]; !ok {
			continue // out of scope: rmp #2719 excludes packages the graph does not model
		}
		if _, ok := haveKey[d.key()]; ok {
			continue
		}
		if _, dup := emitted[d.key()]; dup {
			continue // one node per declaration IDENTITY; see declKey on the init collision
		}
		emitted[d.key()] = struct{}{}
		byLabel[d.Kind] = append(byLabel[d.Kind], d)
	}
	kinds := make([]string, 0, len(byLabel))
	for k := range byLabel {
		kinds = append(kinds, string(k))
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		group := byLabel[declKind(k)]
		sort.Slice(group, func(i, j int) bool { return group[i].key() < group[j].key() })
		// A method's identity includes its receiver: two methods of the same
		// name on different receivers share a file legitimately, and 531 pairs
		// in this tree do. Merging them on (name, pkg, file) alone would bind
		// the second to the first and silently lose one.
		mergeKey := "{name:d.n, pkg:d.p, file:d.f}"
		if declKind(k) == kindMethod {
			mergeKey = "{name:d.n, pkg:d.p, file:d.f, recv:d.r}"
		}
		for i := 0; i < len(group); i += syncBatch {
			end := min(i+syncBatch, len(group))
			items := make([]string, 0, end-i)
			for _, d := range group[i:end] {
				items = append(items, fmt.Sprintf("{n:%s,p:%s,f:%s,r:%s,e:%t}",
					cypherString(d.Name), cypherString(d.ImportPath),
					cypherString(d.File), cypherString(d.Recv), exportedName(d.Name)))
			}
			create = append(create, fmt.Sprintf(
				"UNWIND [%s] AS d MERGE (x:%s %s) ON CREATE SET x.recv = d.r, x.exported = d.e, "+
					"x.gitCommit = %s, x.gitDate = %s ON MATCH SET x.gitCommit = %s, x.gitDate = %s",
				strings.Join(items, ","), k, mergeKey,
				cypherString(commit), cypherString(gdate),
				cypherString(commit), cypherString(gdate)))
		}
	}
	return repair, create
}

// exportedName reports whether a Go identifier is exported. knowledge-model.md
// lists `exported` on Type, Function and Method, so the sync sets it rather
// than leaving the new nodes thinner than the ones already in the graph.
func exportedName(s string) bool {
	if s == "" {
		return false
	}
	c := s[0]
	return c >= 'A' && c <= 'Z'
}
