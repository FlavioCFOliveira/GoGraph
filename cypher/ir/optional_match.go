package ir

// optional_match.go — OPTIONAL MATCH translation helpers.
//
// translateOptionalMatch is intentionally thin: it delegates to translateMatch
// with optional=true, which causes every relationship hop to emit
// OptionalExpand instead of Expand. The entry-point and dispatch live in
// translator.go; the node-scan / expand helpers live in match.go.
//
// See match.go for the shared matchPattern / matchPathPattern / matchNodeScan /
// matchExpandStepBoundWithFrom implementation that both MATCH and OPTIONAL
// MATCH share.
