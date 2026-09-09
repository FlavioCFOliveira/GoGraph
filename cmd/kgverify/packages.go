package main

import (
	"fmt"
	"sort"
)

// checkPackageGap is the id of the per-package parity check.
const checkPackageGap = "package-symbol-gap"

// pkgCoverage is one package's declaration/node accounting, and it is the unit
// acceptance criterion 1 of rmp #2719 asks for: "per package, the count of code
// declarations and of graph nodes".
//
// Four numbers rather than two, because the raw counts alone cannot say whether
// a package is covered. A package with 40 declarations and 40 nodes is at
// parity only if the nodes name those declarations; two duplicate nodes plus
// two missing ones balance to the same total while covering 38. Covered and
// Extra separate the two readings, and Gap — the declarations no node names —
// is the number the check gates on.
type pkgCoverage struct {
	ImportPath string `json:"importPath"`
	Decls      int    `json:"decls"`
	Nodes      int    `json:"nodes"`
	Covered    int    `json:"covered"`
	Gap        int    `json:"gap"`
	Extra      int    `json:"extra"`
}

// Modelled reports whether the graph claims to model this package at all.
//
// The distinction is load-bearing and is what keeps this check inside the
// task's scope: rmp #2719 puts "modelling packages the graph does not already
// cover" explicitly out of scope, so a package with no symbol node is reported
// and never gated. A package with even one node has made a claim, and a claim
// is checkable.
func (p pkgCoverage) Modelled() bool { return p.Nodes > 0 }

// packageKeyIndex resolves the package spellings a node may carry to the one
// canonical import path.
//
// knowledge-model.md records that symbol nodes carry `pkg` holding the full
// import path, except in internal/sim where they carry `package` holding the
// repo-relative directory. Both spellings are live in the graph today — 90
// nodes under "internal/sim" and 56 under the bare package name "sim" — so a
// report keyed on the import path alone would score internal/sim at zero nodes
// and demand 4795 duplicates be created for a package that is already partly
// modelled. Resolving the alias is therefore a correctness requirement of the
// count, not a convenience.
//
// A bare package name is accepted only when it is unambiguous across the
// module: "main" names dozens of packages and resolving it would attribute a
// node to an arbitrary one of them.
type packageKeyIndex struct {
	byImportPath map[string]struct{}
	byDir        map[string]string
	byPkgName    map[string]string // only names that are unique module-wide
}

func newPackageKeyIndex(inv *inventory) *packageKeyIndex {
	idx := &packageKeyIndex{
		byImportPath: map[string]struct{}{},
		byDir:        map[string]string{},
		byPkgName:    map[string]string{},
	}
	nameHits := map[string]map[string]struct{}{}
	for i := range inv.decls {
		d := &inv.decls[i]
		idx.byImportPath[d.ImportPath] = struct{}{}
		idx.byDir[d.Dir] = d.ImportPath
		if nameHits[d.PkgName] == nil {
			nameHits[d.PkgName] = map[string]struct{}{}
		}
		nameHits[d.PkgName][d.ImportPath] = struct{}{}
	}
	for name, paths := range nameHits {
		if len(paths) != 1 {
			continue
		}
		for p := range paths {
			idx.byPkgName[name] = p
		}
	}
	return idx
}

// resolve maps whatever a node carries to a canonical import path, reporting
// whether the key could be placed at all. An unresolvable key names a package
// the tree does not have — a deleted package, or a typo — and is counted
// separately rather than silently attributed to something.
func (idx *packageKeyIndex) resolve(key string) (string, bool) {
	if key == "" {
		return "", false
	}
	if _, ok := idx.byImportPath[key]; ok {
		return key, true
	}
	if p, ok := idx.byDir[key]; ok {
		return p, true
	}
	if p, ok := idx.byPkgName[key]; ok {
		return p, true
	}
	return "", false
}

// buildPackageCoverage accounts every package in the tree against the graph.
//
// Unresolvable node keys are returned separately: they are the residue this
// report cannot attribute, and reporting them beside the table is what stops a
// silent drop being read as parity.
func buildPackageCoverage(inv *inventory, nodes []symNode) ([]pkgCoverage, map[string]int) {
	idx := newPackageKeyIndex(inv)

	declsBy := map[string]int{}
	declKeys := map[string]map[string]struct{}{}
	for i := range inv.decls {
		d := &inv.decls[i]
		declsBy[d.ImportPath]++
		if declKeys[d.ImportPath] == nil {
			declKeys[d.ImportPath] = map[string]struct{}{}
		}
		declKeys[d.ImportPath][d.key()] = struct{}{}
	}

	nodesBy := map[string]int{}
	nodeKeys := map[string]map[string]struct{}{}
	unresolved := map[string]int{}
	for i := range nodes {
		n := &nodes[i]
		if _, isSym := symbolLabels[n.Label]; !isSym {
			continue
		}
		path, ok := idx.resolve(n.Pkg)
		if !ok {
			unresolved[keyOrNull(n.Pkg)]++
			continue
		}
		nodesBy[path]++
		if nodeKeys[path] == nil {
			nodeKeys[path] = map[string]struct{}{}
		}
		nodeKeys[path][n.key()] = struct{}{}
	}

	seen := map[string]struct{}{}
	for p := range declsBy {
		seen[p] = struct{}{}
	}
	for p := range nodesBy {
		seen[p] = struct{}{}
	}
	out := make([]pkgCoverage, 0, len(seen))
	for p := range seen {
		c := pkgCoverage{ImportPath: p, Decls: declsBy[p], Nodes: nodesBy[p]}
		for k := range declKeys[p] {
			if _, ok := nodeKeys[p][k]; ok {
				c.Covered++
			}
		}
		c.Gap = len(declKeys[p]) - c.Covered
		for k := range nodeKeys[p] {
			if _, ok := declKeys[p][k]; !ok {
				c.Extra++
			}
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Gap != out[j].Gap {
			return out[i].Gap > out[j].Gap
		}
		return out[i].ImportPath < out[j].ImportPath
	})
	return out, unresolved
}

func keyOrNull(s string) string {
	if s == "" {
		return "<null>"
	}
	return s
}

// checkPackages gates the per-package parity acceptance criterion.
//
// Only modelled packages are gated, and the violation names the two counts, so
// the failure carries its own repair instruction rather than a bare number.
func (a *auditor) checkPackages(cov []pkgCoverage, base *baseline) {
	var behind []string
	for _, c := range cov {
		if !c.Modelled() || c.Gap == 0 {
			continue
		}
		behind = append(behind, fmt.Sprintf("%s: %d declarations, %d nodes, %d declarations with no node",
			c.ImportPath, c.Decls, c.Nodes, c.Gap))
	}
	a.record(checkPackageGap, behind,
		"package the graph models where a declaration has no symbol node", base.of(checkPackageGap))
}

// emitPackages prints the per-package table acceptance criterion 1 asks to be
// committed and re-runnable.
func emitPackages(cov []pkgCoverage, unresolved map[string]int) string {
	b := make([]byte, 0, 96*len(cov)+96)
	b = append(b, "# importPath\tdecls\tnodes\tcovered\tgap\textra\tmodelled\n"...)
	for _, c := range cov {
		b = append(b, fmt.Sprintf("%s\t%d\t%d\t%d\t%d\t%d\t%t\n",
			c.ImportPath, c.Decls, c.Nodes, c.Covered, c.Gap, c.Extra, c.Modelled())...)
	}
	keys := make([]string, 0, len(unresolved))
	for k := range unresolved {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b = append(b, fmt.Sprintf("# UNRESOLVED\t%s\t%d nodes carry a package key the tree does not have\n", k, unresolved[k])...)
	}
	return string(b)
}

// emitAbsent prints every symbol-tier node naming a declaration the tree does
// not have — the worklist for acceptance criterion 2 of rmp #2719, "delete
// every node naming a symbol that no longer exists".
//
// It exists so the deletion is driven by the same go/parser oracle that detects
// the fabrication, rather than by reading names out of the gate's prose report.
// A name retyped from a report is exactly how the two fabrications in the
// regression fixture were created, and a DELETE keyed on a retyped name is
// worse than a CREATE keyed on one: it removes a node that may have been
// correct. The label is printed alongside the name because the deletion must be
// scoped to the symbol tier — a Feature or a Component may legitimately carry
// the same name.
func emitAbsent(inv *inventory, nodes []symNode) (string, int) {
	b := make([]byte, 0, 64*len(nodes)+64)
	b = append(b, "# label\tname\tpkg\tfile\trecv\n"...)
	n := 0
	for i := range nodes {
		s := &nodes[i]
		if _, isSym := symbolLabels[s.Label]; !isSym {
			continue
		}
		if s.Name != "" && inv.declared(s.Name) {
			continue
		}
		n++
		b = append(b, fmt.Sprintf("%s\t%s\t%s\t%s\t%s\n", s.Label, s.Name, s.Pkg, s.File, s.Recv)...)
	}
	return string(b), n
}
