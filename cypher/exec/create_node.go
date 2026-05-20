package exec

// create_node.go — CreateNode write operator (task-269).
//
// CreateNode is a Volcano write operator that, for every row produced by its
// child, creates a new graph node, attaches labels and properties, and binds
// the new node's NodeID to a new column in the row. When no NodeVar is given
// the row passes through without extension.
//
// # Node key generation
//
// lpg.Graph[string, float64] uses arbitrary strings as user-facing node
// identifiers. Cypher CREATE does not require a user-visible stable key;
// the runtime only needs a NodeID to refer to the node downstream. We
// generate a unique string key per created node using a monotonic counter
// embedded in the operator. The generated key has the form
// "__cx_<counter>" and is stored as a hidden internal key; downstream
// operators reference the node by the emitted NodeID (IntegerValue), not
// by the string key.
//
// # Side effects
//
// Each Next call performs one AddNode + N×SetNodeLabel + M×SetNodeProperty.
//
// # Concurrency
//
// CreateNode is NOT safe for concurrent use.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"

	"gograph/cypher/expr"
	"gograph/graph/lpg"
)

// globalNodeCounter provides a process-wide monotonic source for generated
// node keys. Using an atomic counter avoids collisions if multiple Engine
// instances operate on the same graph concurrently (though the single-writer
// contract from CLAUDE.md prevents concurrent writes; the counter is a cheap
// safety net).
//
//nolint:gochecknoglobals // process-wide monotonic counter for unique key generation
var globalNodeCounter atomic.Uint64

// CreateNode creates a new graph node per input row, sets its labels and
// properties, and appends the new NodeID as a new column.
//
// CreateNode is NOT safe for concurrent use.
type CreateNode struct {
	nodeVar string
	labels  []string
	props   []propLiteral // parsed once from the properties string
	child   Operator
	mutator GraphMutator
	ctx     context.Context //nolint:containedctx // stored for per-Next ctx check
}

// propLiteral is a pre-parsed key/value pair from a literal property map
// expression like {name: "Alice", age: 30}.
type propLiteral struct {
	key   string
	value lpg.PropertyValue
}

// NewCreateNode creates a CreateNode operator.
//
// nodeVar is the variable name bound to the new node (may be empty if the node
// is not referenced downstream). labels is the ordered list of labels to
// attach. properties is the opaque literal property-map string (e.g.
// `{name: "Alice"}`) produced by the IR translator; it is parsed once during
// construction. mutator is the graph write surface.
func NewCreateNode(
	nodeVar string,
	labels []string,
	properties string,
	child Operator,
	mutator GraphMutator,
) (*CreateNode, error) {
	lb := make([]string, len(labels))
	copy(lb, labels)
	props, err := parsePropLiteral(properties)
	if err != nil {
		return nil, fmt.Errorf("exec: CreateNode: parse properties %q: %w", properties, err)
	}
	return &CreateNode{
		nodeVar: nodeVar,
		labels:  lb,
		props:   props,
		child:   child,
		mutator: mutator,
	}, nil
}

// Init initialises the operator and its child.
func (op *CreateNode) Init(ctx context.Context) error {
	op.ctx = ctx
	return op.child.Init(ctx)
}

// Next pulls one row from the child, creates a node, and appends the NodeID
// column. Returns (true, nil) when a row was produced, (false, nil) at
// end-of-stream, (false, err) on error.
func (op *CreateNode) Next(out *Row) (bool, error) {
	if err := op.ctx.Err(); err != nil {
		return false, err
	}

	var childRow Row
	ok, err := op.child.Next(&childRow)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}

	nodeKey := op.freshNodeKey()
	nodeID := op.mutator.AddNode(nodeKey)
	for _, lbl := range op.labels {
		op.mutator.SetNodeLabel(nodeKey, lbl)
	}
	for _, p := range op.props {
		op.mutator.SetNodeProperty(nodeKey, p.key, p.value)
	}

	// Build output row: child columns + optional NodeID column.
	if op.nodeVar == "" {
		*out = childRow
		return true, nil
	}

	newRow := make(Row, len(childRow)+1)
	copy(newRow, childRow)
	newRow[len(childRow)] = expr.IntegerValue(int64(nodeID))
	*out = newRow
	return true, nil
}

// freshNodeKey returns a string key that is guaranteed to be unique within the
// current process by drawing from a global monotonic counter. The key is never
// visible to Cypher callers; only the NodeID is emitted into the row.
func (op *CreateNode) freshNodeKey() string {
	n := globalNodeCounter.Add(1)
	return "__cx_" + strconv.FormatUint(n, 16)
}

// Close closes the child operator.
func (op *CreateNode) Close() error {
	return op.child.Close()
}

// ─────────────────────────────────────────────────────────────────────────────
// parsePropLiteral — minimal literal-map parser
// ─────────────────────────────────────────────────────────────────────────────

// parsePropLiteral parses a Cypher literal property map string of the form
// `{key: value, ...}` into a slice of propLiteral. Only the subset of literal
// types produced by the IR translator is supported:
//   - String literals: `"..."` or `'...'`
//   - Integer literals: decimal digits, optionally negated
//   - Float literals: decimal with `.`
//   - Boolean literals: `true` / `false`
//
// Returns nil (no error) for empty or absent property maps.
func parsePropLiteral(s string) ([]propLiteral, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return nil, fmt.Errorf("expected map literal enclosed in {}, got %q", s)
	}
	inner := strings.TrimSpace(s[1 : len(s)-1])
	if inner == "" {
		return nil, nil
	}

	var out []propLiteral
	parts := splitMapItems(inner)
	for _, part := range parts {
		part = strings.TrimSpace(part)
		colonIdx := strings.Index(part, ":")
		if colonIdx < 0 {
			return nil, fmt.Errorf("missing ':' in map item %q", part)
		}
		key := strings.TrimSpace(part[:colonIdx])
		key = strings.Trim(key, "`")
		valStr := strings.TrimSpace(part[colonIdx+1:])

		pv, err := parsePropValue(valStr)
		if err != nil {
			return nil, fmt.Errorf("key %q: %w", key, err)
		}
		out = append(out, propLiteral{key: key, value: pv})
	}
	return out, nil
}

// splitMapItems splits a comma-separated list of map items, respecting
// string literal boundaries (no nesting of sub-maps is needed for the
// current IR literal format).
func splitMapItems(s string) []string {
	var parts []string
	depth := 0
	inStr := false
	strChar := byte(0)
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			if c == strChar && (i == 0 || s[i-1] != '\\') {
				inStr = false
			}
			continue
		}
		switch c {
		case '"', '\'':
			inStr = true
			strChar = c
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	if start < len(s) {
		parts = append(parts, s[start:])
	}
	return parts
}

// parsePropValue parses a single Cypher literal value string into a
// lpg.PropertyValue.
func parsePropValue(s string) (lpg.PropertyValue, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return lpg.PropertyValue{}, fmt.Errorf("empty value")
	}
	// String literal.
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') {
		end := s[0]
		if s[len(s)-1] != end {
			return lpg.PropertyValue{}, fmt.Errorf("unterminated string literal %q", s)
		}
		return lpg.StringValue(unescapeString(s[1 : len(s)-1])), nil
	}
	// Boolean.
	if s == "true" {
		return lpg.BoolValue(true), nil
	}
	if s == "false" {
		return lpg.BoolValue(false), nil
	}
	// Float (contains dot).
	if strings.Contains(s, ".") {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return lpg.PropertyValue{}, fmt.Errorf("invalid float %q: %w", s, err)
		}
		return lpg.Float64Value(f), nil
	}
	// Integer.
	i, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return lpg.PropertyValue{}, fmt.Errorf("invalid literal %q: %w", s, err)
	}
	return lpg.Int64Value(i), nil
}

// unescapeString handles the most common escape sequences in Cypher strings.
func unescapeString(s string) string {
	s = strings.ReplaceAll(s, `\"`, `"`)
	s = strings.ReplaceAll(s, `\'`, `'`)
	s = strings.ReplaceAll(s, `\\`, `\`)
	s = strings.ReplaceAll(s, `\n`, "\n")
	s = strings.ReplaceAll(s, `\t`, "\t")
	return s
}
