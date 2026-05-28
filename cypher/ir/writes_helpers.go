package ir

// writes_helpers.go — predicates for detecting write IR nodes.

// ContainsWrite walks p and reports whether any node in the tree is a write
// operator (CreateNode, CreateRelationship, SetProperty, SetAllProperties,
// SetLabels, RemoveProperty, RemoveLabels, DeleteNode, DeleteRelationship,
// DetachDelete, Merge, MergeRelationship). The walk is depth-first; nil
// plans return false.
func ContainsWrite(p LogicalPlan) bool {
	if p == nil {
		return false
	}
	switch p.(type) {
	case *CreateNode,
		*CreateRelationship,
		*SetProperty,
		*SetAllProperties,
		*SetLabels,
		*RemoveProperty,
		*RemoveLabels,
		*DeleteNode,
		*DeleteRelationship,
		*DetachDelete,
		*Merge,
		*MergeRelationship:
		return true
	}
	for _, ch := range p.Children() {
		if ContainsWrite(ch) {
			return true
		}
	}
	return false
}

// ContainsDelete reports whether p's subtree contains any DELETE-class
// operator. Used to decide whether a subsequent MERGE needs an Eager
// pipeline-breaker so the deletion's writes are visible to the
// MERGE-search (Merge1 [14]).
func ContainsDelete(p LogicalPlan) bool {
	if p == nil {
		return false
	}
	switch p.(type) {
	case *DeleteNode, *DeleteRelationship, *DetachDelete:
		return true
	}
	for _, ch := range p.Children() {
		if ContainsDelete(ch) {
			return true
		}
	}
	return false
}
