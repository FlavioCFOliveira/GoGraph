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
// "__cx_<hex>" and is stored as a hidden internal key; downstream
// operators reference the node by the emitted NodeID (IntegerValue), not
// by the string key.
//
// The counter is process-local; across process restarts it would reset to
// zero and collide with __cx_<hex> keys interned by an earlier process and
// reloaded via WAL / snapshot recovery. To defend against that, the first
// CreateNode.Init in each process seeds the counter from the maximum
// __cx_<hex> suffix present in the mapper, advancing it via a CAS loop. The
// seed runs once per process (sync.Once) so the O(N) cost is amortised.
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
	"sync"
	"sync/atomic"

	"gograph/cypher/expr"
	"gograph/graph"
	"gograph/graph/index"
	"gograph/graph/lpg"
)

// synthKeyPrefix is the fixed prefix of every synthetic node key produced by
// [CreateNode.freshNodeKey]. Kept as a constant so the counter-seeding scan in
// [seedGlobalNodeCounter] and the formatter in [CreateNode.freshNodeKey] cannot
// drift apart.
const synthKeyPrefix = "__cx_"

// globalNodeCounter provides a process-wide monotonic source for generated
// node keys. Using an atomic counter avoids collisions if multiple Engine
// instances operate on the same graph concurrently (though the single-writer
// contract from CLAUDE.md prevents concurrent writes; the counter is a cheap
// safety net).
//
// The counter is process-local and resets to zero in every new process. Across
// process restarts this would produce keys that collide with previously
// persisted ones from the same graph (Mapper.Intern of an existing key returns
// the existing NodeID, silently overwriting the original node's properties on
// the follow-up SetNodeProperty calls). To defend against that, every
// [CreateNode] operator seeds the counter from the keys already interned in
// its mutator on first [CreateNode.Init], advancing the counter past the
// largest existing __cx_<hex> suffix via a CAS loop. The seed runs once per
// process (gated by [globalNodeCounterSeededOnce]); subsequent CreateNode
// operators observe the [sync.Once] as already-fired and skip the scan.
//
//nolint:gochecknoglobals // process-wide monotonic counter for unique key generation
var globalNodeCounter atomic.Uint64

// globalNodeCounterSeededOnce guards the one-shot seed scan triggered by the
// first [CreateNode.Init] in the process. The seed walks the mutator's
// interned node keys (O(N) over distinct keys) and CASes
// [globalNodeCounter] forward to one past the maximum __cx_<hex> suffix found.
// All later CreateNode.Init calls observe the Once as already-fired and skip
// the scan, so the cost is amortised across the lifetime of the process.
//
//nolint:gochecknoglobals // paired with globalNodeCounter
var globalNodeCounterSeededOnce sync.Once

// CreateNode creates a new graph node per input row, sets its labels and
// properties, and appends the new NodeID as a new column.
//
// CreateNode is NOT safe for concurrent use.
type CreateNode struct {
	nodeVar  string
	labels   []string
	propsRaw string        // original properties string, retained for re-parse with params
	props    []propLiteral // parsed once from the properties string
	child    Operator
	mutator  GraphMutator
	params   map[string]expr.Value // query parameters for $name substitution
	reg      *ConstraintRegistry   // nil means no enforcement
	mgr      *index.Manager        // nil when reg is nil
	ctx      context.Context       //nolint:containedctx // stored for per-Next ctx check
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
		nodeVar:  nodeVar,
		labels:   lb,
		propsRaw: properties,
		props:    props,
		child:    child,
		mutator:  mutator,
	}, nil
}

// WithParams attaches query parameters for $name substitution in property
// expressions. Re-parses the property map with the supplied params.
// Returns op for chaining.
func (op *CreateNode) WithParams(params map[string]expr.Value) (*CreateNode, error) {
	if len(params) == 0 {
		return op, nil
	}
	props, err := parsePropLiteralWithParams(op.propsRaw, params)
	if err != nil {
		return nil, fmt.Errorf("exec: CreateNode: parse properties %q: %w", op.propsRaw, err)
	}
	op.params = params
	op.props = props
	return op, nil
}

// WithConstraints attaches a ConstraintRegistry and index.Manager to the
// operator for pre-write enforcement. Both must be non-nil. Returns op for
// chaining.
func (op *CreateNode) WithConstraints(reg *ConstraintRegistry, mgr *index.Manager) *CreateNode {
	op.reg = reg
	op.mgr = mgr
	return op
}

// Init initialises the operator and its child.
//
// The first CreateNode.Init in the process also seeds [globalNodeCounter]
// past the largest synthetic key currently interned in op.mutator, so that
// node keys generated in this process cannot collide with keys persisted by
// an earlier process and replayed during WAL / snapshot recovery. The seed
// is gated by [globalNodeCounterSeededOnce] so the scan runs at most once
// per process regardless of how many CreateNode operators are created.
func (op *CreateNode) Init(ctx context.Context) error {
	op.ctx = ctx
	globalNodeCounterSeededOnce.Do(func() {
		seedGlobalNodeCounter(op.mutator)
	})
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

	// Constraint enforcement: check before any mutation.
	if op.reg != nil {
		for _, p := range op.props {
			if err := op.reg.CheckSetProperty(op.labels, p.key, p.value, op.mgr); err != nil {
				return false, err
			}
		}
	}

	nodeKey := op.freshNodeKey()
	nodeID, err := op.mutator.AddNode(nodeKey)
	if err != nil {
		return false, err
	}
	for _, lbl := range op.labels {
		if err := op.mutator.SetNodeLabel(nodeKey, lbl); err != nil {
			return false, err
		}
	}
	for _, p := range op.props {
		if err := op.mutator.SetNodeProperty(nodeKey, p.key, p.value); err != nil {
			return false, err
		}
		if op.reg != nil {
			op.reg.RecordPropertySet(op.labels, p.key, p.value)
		}
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
	return synthKeyPrefix + strconv.FormatUint(n, 16)
}

// seedGlobalNodeCounter walks every node key already interned in m and
// advances [globalNodeCounter] past the largest __cx_<hex> suffix found.
// The advance uses a CAS loop so concurrent advances by other goroutines (or
// by [CreateNode.freshNodeKey] in this goroutine) never roll the counter
// backwards.
//
// Cost is O(N) over the number of distinct keys in m at call time. The
// caller guarantees seedGlobalNodeCounter runs at most once per process via
// [globalNodeCounterSeededOnce], so the cost is amortised across the
// lifetime of the engine. A nil mutator is tolerated (no-op) so the
// operator stays usable in unit tests that build a CreateNode without a
// backing mutator.
func seedGlobalNodeCounter(m GraphMutator) {
	if m == nil {
		return
	}
	var maxSeen uint64
	m.WalkNodeIDs(func(id graph.NodeID) bool {
		key, ok := m.ResolveNodeLabel(id)
		if !ok {
			return true
		}
		if v, ok := parseSynthKeySuffix(key); ok && v > maxSeen {
			maxSeen = v
		}
		return true
	})
	for {
		cur := globalNodeCounter.Load()
		if cur >= maxSeen {
			return
		}
		if globalNodeCounter.CompareAndSwap(cur, maxSeen) {
			return
		}
	}
}

// parseSynthKeySuffix returns the numeric hex suffix of a synthetic node key
// produced by [CreateNode.freshNodeKey] (form "__cx_<hex>"). It returns
// (0, false) when key does not match the synthetic-key pattern, when the
// suffix is empty, or when the suffix is not a valid hexadecimal uint64.
//
// The parser deliberately rejects keys that share the __cx_ prefix but carry
// a non-hex middle segment (notably the "__cx_merge_<hex>" keys produced by
// the Merge operator). The seeding logic must not advance globalNodeCounter
// from those keys: the Merge counter is the same global, but its key format
// is intentionally distinct and is out of scope for the cross-process
// counter-reset fix tracked under this change.
func parseSynthKeySuffix(key string) (uint64, bool) {
	if !strings.HasPrefix(key, synthKeyPrefix) {
		return 0, false
	}
	suffix := key[len(synthKeyPrefix):]
	if suffix == "" {
		return 0, false
	}
	v, err := strconv.ParseUint(suffix, 16, 64)
	if err != nil {
		return 0, false
	}
	return v, true
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
	return parsePropLiteralDeferred(s)
}

// parsePropLiteralDeferred is like parsePropLiteralWithParams but returns nil
// (without error) when the value expression is a $param reference and the
// params map does not contain that key. Used during plan construction when
// query parameters are not yet available; callers must invoke WithParams before
// executing the operator.
func parsePropLiteralDeferred(s string) ([]propLiteral, error) {
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

		if strings.HasPrefix(valStr, "$") {
			// $param reference — deferred; skip for now.
			continue
		}
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

// parsePropLiteralWithParams parses a Cypher property-map literal (e.g.
// "{key: $param, key2: 'lit'}") into a slice of propLiterals, substituting
// parameter references of the form "$name" from the supplied params map.
// Unrecognised parameter names yield an error.
//
// When params is nil the function behaves identically to parsePropLiteral.
func parsePropLiteralWithParams(s string, params map[string]expr.Value) ([]propLiteral, error) {
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

		pv, err := parsePropValueWithParams(valStr, params)
		if err != nil {
			return nil, fmt.Errorf("key %q: %w", key, err)
		}
		out = append(out, propLiteral{key: key, value: pv})
	}
	return out, nil
}

// parsePropValueWithParams parses a single Cypher literal value string into a
// lpg.PropertyValue, substituting parameter references of the form "$name"
// from the supplied params map.
//
// When params is nil (or empty) the function behaves identically to
// parsePropValue.
func parsePropValueWithParams(s string, params map[string]expr.Value) (lpg.PropertyValue, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "$") {
		name := strings.TrimPrefix(s, "$")
		v, ok := params[name]
		if !ok {
			return lpg.PropertyValue{}, fmt.Errorf("unbound parameter $%s", name)
		}
		switch val := v.(type) {
		case expr.StringValue:
			return lpg.StringValue(string(val)), nil
		case expr.IntegerValue:
			return lpg.Int64Value(int64(val)), nil
		case expr.FloatValue:
			return lpg.Float64Value(float64(val)), nil
		case expr.BoolValue:
			return lpg.BoolValue(bool(val)), nil
		default:
			return lpg.PropertyValue{}, fmt.Errorf("unsupported param type %T for $%s", v, name)
		}
	}
	return parsePropValue(s)
}

// parsePropValue parses a single Cypher literal value string into a
// lpg.PropertyValue.
//
// In addition to the primitive literals (string, boolean, integer, float) the
// parser recognises temporal function calls expressed as their source-text
// representation:
//
//	date('YYYY-MM-DD')                       → encoded PropString with magic prefix
//	localdatetime('YYYY-MM-DDTHH:MM:SS')     → encoded PropString
//	datetime('YYYY-MM-DDTHH:MM:SS±HH:MM')    → encoded PropString
//	localtime('HH:MM:SS')                    → encoded PropString
//	time('HH:MM:SS±HH:MM')                   → encoded PropString
//	duration('P...')                         → encoded PropString
//
// Temporal values are persisted as [lpg.PropString] with a leading
// SOH-byte tag (0x01..0x06) followed by the canonical openCypher textual
// form. This keeps the WAL backward-compatible (existing PropString payloads
// do not start with a SOH byte) while allowing temporal values to round-trip
// snapshot+WAL replay without introducing a new property kind.
func parsePropValue(s string) (lpg.PropertyValue, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return lpg.PropertyValue{}, fmt.Errorf("empty value")
	}
	// Temporal function calls (string-form constructors).
	if pv, ok, err := parseTemporalLiteral(s); ok {
		return pv, err
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
