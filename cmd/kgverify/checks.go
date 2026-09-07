package main

import (
	"fmt"
	"sort"
	"strings"
)

// Check ids. They are stable strings because the baseline file keys on them: a
// renamed check silently loses its baseline, which would either hide a
// regression or raise a false one.
const (
	checkFixtureFabrication = "fixture-fabrication-present"
	checkSymbolAbsent       = "symbol-absent"
	checkSymbolFileAbsent   = "symbol-file-absent"
	checkSymbolKind         = "symbol-kind-mismatch"
	checkSymbolPkg          = "symbol-pkg-mismatch"
	checkTaskIDNotInt       = "task-id-not-int"
	checkTaskLegacyIdentity = "task-legacy-identity"
	checkTaskDuplicateID    = "task-id-duplicated"
	checkTaskStub           = "task-stub"
	checkTaskStatusInvalid  = "task-status-invalid"
	checkTaskStatusStale    = "task-status-disagrees-with-rmp"
	checkTaskAbsentInRmp    = "task-absent-in-rmp"
	checkProvenanceNoNode   = "provenance-no-node"
	checkProvenanceNoCommit = "provenance-no-gitcommit"
	checkEdgeOffModel       = "edge-off-model"
	checkEdgeTypeUndoc      = "edge-type-undocumented"
	checkLabelUndoc         = "label-undocumented"
	checkComponentNoPath    = "component-path-missing"
	checkComponentPathGone  = "component-path-absent"
)

// rmpStatuses are the only admissible Task.status values. knowledge-model.md
// states plainly that rmp is the authority and these five are the whole set; a
// graph-only value duplicates something the rmp title already says.
var rmpStatuses = map[string]struct{}{
	"BACKLOG": {}, "SPRINT": {}, "DOING": {}, "TESTING": {}, "COMPLETED": {},
}

// result is one check's outcome.
type result struct {
	ID         string   `json:"id"`
	Actual     int      `json:"actual"`
	Baseline   int      `json:"baseline"`
	Violations []string `json:"violations,omitempty"`
	Note       string   `json:"note,omitempty"`
}

// Exceeded reports whether the check must fail the run.
func (r result) Exceeded() bool { return r.Actual > r.Baseline }

type auditor struct {
	inv     *inventory
	mdl     *model
	results []result
	order   []string
}

func (a *auditor) record(id string, violations []string, note string, baseline int) {
	sort.Strings(violations)
	a.results = append(a.results, result{
		ID:         id,
		Actual:     len(violations),
		Baseline:   baseline,
		Violations: violations,
		Note:       note,
	})
	a.order = append(a.order, id)
}

// checkFixture holds the graph to the specific fabrications that produced this
// gate, so that exact defect cannot recur silently.
//
// It is also the gate's own liveness assertion. Each fixture name must be
// absent from the tree's declarations; if one turns out to be declared, the
// fixture is stale and the run aborts rather than reporting a pass, because a
// fixture that names a real symbol proves nothing about fabrication. This is
// how the checker asserts that its oracle was actually consulted.
func (a *auditor) checkFixture(names []string, byName map[string][]symNode, allNames map[string][]string, baseline int) error {
	var stale []string
	var present []string
	for _, n := range names {
		if a.inv.declared(n) {
			stale = append(stale, n)
			continue
		}
		if labels, ok := allNames[n]; ok {
			present = append(present, fmt.Sprintf("%s is back in the graph as %s (fabricated node, sprint-352 finding)", n, strings.Join(labels, "|")))
			continue
		}
		if _, ok := byName[n]; ok {
			present = append(present, n+" is back in the graph as a symbol node (fabricated node, sprint-352 finding)")
		}
	}
	if len(stale) > 0 {
		return fmt.Errorf("regression fixture is stale: %s now declared in the tree; a fixture entry must name a non-declaration, so remove it from the baseline rather than leaving it to pass vacuously", strings.Join(stale, ", "))
	}
	a.record(checkFixtureFabrication, present,
		fmt.Sprintf("%d fixture names verified absent from both the tree and the graph", len(names)), baseline)
	return nil
}

// symNode is a symbol-tier node as the graph holds it.
type symNode struct {
	Label     string
	Name      string
	Pkg       string
	File      string
	Recv      string
	GitCommit string
}

func (s *symNode) where() string {
	loc := s.Pkg
	if loc == "" {
		loc = "?"
	}
	if s.File != "" {
		loc += " (" + s.File + ")"
	}
	return loc
}

// checkSymbols holds every symbol-tier node to the tree.
//
// The fabrication check is the strict one and its baseline is zero: a node
// whose name is declared nowhere, under any kind, in any package, names
// something that does not exist. The kind and package checks are reported
// separately and carry their own baselines, because knowledge-model.md records
// the internal/sim package-keying divergence as the model in force — failing on
// a documented, accepted state would leave the gate permanently red, which is
// just a different way of being useless.
func (a *auditor) checkSymbols(nodes []symNode, base *baseline) {
	var absent, fileGone, kindBad, pkgBad []string
	for _, n := range nodes {
		kind, ok := symbolLabels[n.Label]
		if !ok {
			continue
		}
		if n.Name == "" {
			absent = append(absent, fmt.Sprintf("%s node with no name at %s", n.Label, n.where()))
			continue
		}
		switch {
		case !a.inv.declared(n.Name):
			absent = append(absent, fmt.Sprintf("%s %s at %s is declared nowhere in the tree", n.Label, n.Name, n.where()))
		case !a.inv.declaredAs(n.Name, kind):
			kindBad = append(kindBad, fmt.Sprintf("%s %s at %s is declared, but never as a %s", n.Label, n.Name, n.where(), kind))
		case !a.inv.declaredIn(n.Name, kind, n.Pkg):
			pkgBad = append(pkgBad, fmt.Sprintf("%s %s is declared, but not in %s", n.Label, n.Name, n.Pkg))
		}
		if n.File != "" && !a.inv.hasFile(n.File) {
			fileGone = append(fileGone, fmt.Sprintf("%s %s points at %s, which does not exist", n.Label, n.Name, n.File))
		}
	}
	a.record(checkSymbolAbsent, absent, "symbol-tier node naming a declaration absent from the tree", base.of(checkSymbolAbsent))
	a.record(checkSymbolFileAbsent, fileGone, "symbol-tier node whose file property names a missing path", base.of(checkSymbolFileAbsent))
	a.record(checkSymbolKind, kindBad, "name exists in the tree but never under the node's label", base.of(checkSymbolKind))
	a.record(checkSymbolPkg, pkgBad, "name exists in the tree but not in the node's claimed package", base.of(checkSymbolPkg))
}

// taskNode is a Task node with its identity kept untyped, so the id's JSON type
// survives to be checked.
type taskNode struct {
	RawID    any
	Status   any
	Title    any
	Name     any
	LegacyID any
	LegacyNo any
}

// checkTasks pins the canonical Task identity and holds every status to rmp.
//
// Task.id must be an INTEGER. rmp #2612 migrated the label off name-as-string
// and the retired form came back, invisible to every identity lookup — a node
// that reads as MISSING rather than as WRONG. These checks make it read as
// wrong: a string id, a legacy task_id/number key, or a name holding a bare
// ticket number each fail with a baseline of zero.
func (a *auditor) checkTasks(nodes []taskNode, live map[int]rmpTask, missingInRmp []int, base *baseline) []int {
	var notInt, legacy, stub, badStatus, stale []string
	seen := map[int]int{}
	var ids []int

	for _, t := range nodes {
		id, isInt := jsonInt(t.RawID)
		switch {
		case t.RawID == nil:
			notInt = append(notInt, fmt.Sprintf("Task node with no id at all (title=%s)", quoteAny(t.Title)))
		case !isInt:
			notInt = append(notInt, fmt.Sprintf("Task id %s is %s, not an integer: MERGE {id: %s} binds a DIFFERENT node from MERGE {id: %s} (rmp #2612)",
				quoteAny(t.RawID), jsonTypeName(t.RawID), quoteAny(t.RawID), strings.Trim(quoteAny(t.RawID), `"`)))
		default:
			seen[id]++
			ids = append(ids, id)
		}
		if t.LegacyID != nil {
			legacy = append(legacy, fmt.Sprintf("Task %s carries the retired identity key task_id=%s", quoteAny(t.RawID), quoteAny(t.LegacyID)))
		}
		if t.LegacyNo != nil {
			legacy = append(legacy, fmt.Sprintf("Task %s carries the retired identity key number=%s", quoteAny(t.RawID), quoteAny(t.LegacyNo)))
		}
		if s, ok := t.Name.(string); ok && isBareTicket(s) {
			legacy = append(legacy, fmt.Sprintf("Task carries the retired name-as-id form name=%q; keyed on name, it is invisible to every id lookup and reads as MISSING rather than wrong (rmp #2612)", s))
		}
		if t.Title == nil || t.Status == nil {
			stub = append(stub, fmt.Sprintf("Task %s is a stub: title=%s status=%s", quoteAny(t.RawID), quoteAny(t.Title), quoteAny(t.Status)))
		}
		if s, ok := t.Status.(string); ok {
			if _, admissible := rmpStatuses[s]; !admissible {
				badStatus = append(badStatus, fmt.Sprintf("Task %s has status %q, which rmp does not define", quoteAny(t.RawID), s))
			} else if isInt {
				if rt, ok := live[id]; ok && rt.Status != s {
					stale = append(stale, fmt.Sprintf("Task %d: graph says %s, rmp says %s", id, s, rt.Status))
				}
			}
		}
	}

	var dups []string
	for id, n := range seen {
		if n > 1 {
			dups = append(dups, fmt.Sprintf("Task id %d maps to %d nodes; a full-property MERGE against a node with different properties creates a duplicate instead of matching it", id, n))
		}
	}
	absent := make([]string, 0, len(missingInRmp))
	for _, id := range missingInRmp {
		absent = append(absent, fmt.Sprintf("Task %d exists in the graph but rmp has no such task", id))
	}

	a.record(checkTaskIDNotInt, notInt, "Task.id must be an integer (rmp #2612)", base.of(checkTaskIDNotInt))
	a.record(checkTaskLegacyIdentity, legacy, "retired Task identity forms: task_id, number, name-as-id", base.of(checkTaskLegacyIdentity))
	a.record(checkTaskDuplicateID, dups, "one Task id bound to more than one node", base.of(checkTaskDuplicateID))
	a.record(checkTaskStub, stub, "Task node with no title or no status", base.of(checkTaskStub))
	a.record(checkTaskStatusInvalid, badStatus, "Task.status outside rmp's five values", base.of(checkTaskStatusInvalid))
	a.record(checkTaskStatusStale, stale, "graph Task.status disagrees with rmp, the authority", base.of(checkTaskStatusStale))
	a.record(checkTaskAbsentInRmp, absent, "graph names a task rmp has never heard of", base.of(checkTaskAbsentInRmp))
	return ids
}

// checkProvenance holds the sprint's own footprint to the per-commit sync
// directive: every declaration in a file the range touched should have a node,
// and that node should carry a gitCommit.
func (a *auditor) checkProvenance(touched map[string]struct{}, nodes []symNode, base *baseline) {
	byKey := make(map[string]symNode, len(nodes))
	for _, n := range nodes {
		if n.File != "" && n.Name != "" {
			byKey[n.File+"\x00"+n.Name] = n
		}
	}
	var noNode, noCommit []string
	touchedDecls := a.inv.declsInFiles(touched)
	for _, d := range touchedDecls {
		n, ok := byKey[d.File+"\x00"+d.Name]
		if !ok {
			noNode = append(noNode, fmt.Sprintf("%s %s (%s) was touched in the audited range and has no graph node", d.Kind, d.Name, d.File))
			continue
		}
		if n.GitCommit == "" {
			noCommit = append(noCommit, fmt.Sprintf("%s %s (%s) has a node with no gitCommit", d.Kind, d.Name, d.File))
		}
	}
	// The denominator travels with the count deliberately. "1000 declarations
	// have no node" invites the reading that the sprint's own sync was skipped;
	// "1000 of 1837, against 43.5% coverage tree-wide" says the true thing,
	// which is that this is a global bootstrap gap and not a sprint-local one.
	denom := fmt.Sprintf("of %d declarations in %d touched .go files", len(touchedDecls), len(touched))
	a.record(checkProvenanceNoNode, noNode, "declaration in a touched file with no node at all, "+denom, base.of(checkProvenanceNoNode))
	a.record(checkProvenanceNoCommit, noCommit, "node for a touched declaration carrying no gitCommit, "+denom, base.of(checkProvenanceNoCommit))
}

// edgeShape is one live (srcLabel, type, dstLabel) triple and how many edges
// carry it.
type edgeShape struct {
	Src, Type, Dst string
	Count          int
}

// checkEdges holds every live edge form to the documented model. An edge type
// the model documents has a closed set of endpoint shapes, and a shape outside
// that set is off-model. An edge type the model does not document at all is a
// separate, counted class: knowledge-model.md admits that backlog itself, and
// conflating the two would bury the off-model IMPLEMENTED_IN edges under it.
func (a *auditor) checkEdges(shapes []edgeShape, base *baseline) {
	var offModel, undocumented []string
	for _, s := range shapes {
		if !a.mdl.hasEdgeType(s.Type) {
			undocumented = append(undocumented, fmt.Sprintf("(%s)-[:%s]->(%s) x%d: edge type absent from knowledge-model.md", s.Src, s.Type, s.Dst, s.Count))
			continue
		}
		if !a.mdl.hasEdgeForm(edgeForm{Src: s.Src, Type: s.Type, Dst: s.Dst}) {
			offModel = append(offModel, fmt.Sprintf("(%s)-[:%s]->(%s) x%d is not a documented form of %s", s.Src, s.Type, s.Dst, s.Count, s.Type))
		}
	}
	a.record(checkEdgeOffModel, offModel, "documented edge type used with an undocumented endpoint shape", base.of(checkEdgeOffModel))
	a.record(checkEdgeTypeUndoc, undocumented, "edge type present in the graph and absent from the model's table", base.of(checkEdgeTypeUndoc))
}

// checkLabels holds every live node label to the model's label table.
func (a *auditor) checkLabels(counts map[string]int, base *baseline) {
	var undoc []string
	for l, c := range counts {
		if !a.mdl.hasLabel(l) {
			undoc = append(undoc, fmt.Sprintf("%s x%d is present in the graph and absent from the label table in knowledge-model.md", l, c))
		}
	}
	a.record(checkLabelUndoc, undoc, "node label absent from knowledge-model.md's label table", base.of(checkLabelUndoc))
}

// componentNode is a Component node's location properties.
type componentNode struct {
	Name string
	Path string
	File string
}

// checkComponents holds Component nodes to the property the model names.
// knowledge-model.md is explicit that this label uses path, not file, so a node
// carrying only file is invisible to the documented path query.
func (a *auditor) checkComponents(nodes []componentNode, base *baseline) {
	var noPath, pathGone []string
	for _, c := range nodes {
		switch {
		case c.Path == "" && c.File != "":
			noPath = append(noPath, fmt.Sprintf("Component %s carries file=%q with a NULL path; the documented query reads path, so this node is invisible to it", c.Name, c.File))
		case c.Path == "":
			noPath = append(noPath, fmt.Sprintf("Component %s carries neither path nor file", c.Name))
		case !a.inv.hasFile(c.Path) && !a.inv.hasDirPrefix(c.Path):
			pathGone = append(pathGone, fmt.Sprintf("Component %s has path=%q, which is neither a file nor a directory in the tree", c.Name, c.Path))
		}
	}
	a.record(checkComponentNoPath, noPath, "Component node with no usable path property", base.of(checkComponentNoPath))
	a.record(checkComponentPathGone, pathGone, "Component.path naming something absent from the tree", base.of(checkComponentPathGone))
}

func jsonInt(v any) (int, bool) {
	f, ok := v.(float64)
	if !ok {
		return 0, false
	}
	if f != float64(int(f)) {
		return 0, false
	}
	return int(f), true
}

func jsonTypeName(v any) string {
	switch v.(type) {
	case string:
		return "a string"
	case float64:
		return "a non-integral number"
	case bool:
		return "a boolean"
	case nil:
		return "null"
	default:
		return fmt.Sprintf("%T", v)
	}
}

func quoteAny(v any) string {
	if v == nil {
		return "NULL"
	}
	if s, ok := v.(string); ok {
		return fmt.Sprintf("%q", s)
	}
	if f, ok := v.(float64); ok && f == float64(int(f)) {
		return fmt.Sprint(int(f))
	}
	return fmt.Sprint(v)
}

// isBareTicket reports whether s is nothing but digits — the retired
// name-as-id form, where a Task's identity was written into name as a string.
func isBareTicket(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
