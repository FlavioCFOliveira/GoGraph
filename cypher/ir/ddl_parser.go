package ir

// ddl_parser.go — lightweight string parser for Cypher DDL statements.
//
// The ANTLR grammar covers DML (MATCH/CREATE/MERGE/…) but not DDL
// (CREATE INDEX / DROP INDEX / CREATE CONSTRAINT / …). This module
// provides a hand-written parser for the DDL subset so that the Engine
// can handle DDL queries without going through the ANTLR pipeline.
//
// Supported syntax:
//
//   CREATE INDEX [name] FOR (n:Label) ON (n.prop) [OPTIONS {indexType: 'hash'|'btree'}]
//   CREATE INDEX IF NOT EXISTS [name] FOR (n:Label) ON (n.prop) [OPTIONS {…}]
//   DROP INDEX name [IF EXISTS]
//
// IsDDL reports whether a query string is a DDL statement so that the caller
// can bypass the ANTLR parser. The check is a fast keyword prefix scan.

import (
	"fmt"
	"strings"
)

// reservedNumericCompanionSuffix is the lowercase name suffix reserved for the
// internal unified-numeric btree companion (see cypher.numericBTreeName /
// procs.numericCompanionSuffix — kept in sync across the three packages; ir must
// not import cypher). A user-supplied CREATE INDEX name may not end with it.
const reservedNumericCompanionSuffix = "_btree_num"

// reservedUniqueBackingPrefix is the name prefix reserved for the internal hash
// index that backs a UNIQUE constraint (see cypher/exec.uniqueIndexName, which
// builds "__uniq__<label>.<prop>" — kept in sync here; ir must not import
// cypher/exec). A user CREATE INDEX / DROP INDEX name may not use it: dropping
// such an index directly would desynchronise the index catalogue from the
// constraint set, and creating one would squat the slot a real constraint needs
// (#1912). Matched case-insensitively so a near-miss cannot squat it either.
const reservedUniqueBackingPrefix = "__uniq__"

// checkReservedIndexName rejects a user index name that claims a reserved
// internal namespace (the numeric-companion suffix or the UNIQUE-backing
// prefix). kind names the statement for an actionable error.
func checkReservedIndexName(kind, name string) error {
	lower := strings.ToLower(name)
	if strings.HasSuffix(lower, reservedNumericCompanionSuffix) {
		return fmt.Errorf("ir: %s: index name %q uses the reserved %q suffix", kind, name, reservedNumericCompanionSuffix)
	}
	if strings.HasPrefix(lower, reservedUniqueBackingPrefix) {
		return fmt.Errorf("ir: %s: index name %q uses the reserved %q prefix (constraint-backing index)", kind, name, reservedUniqueBackingPrefix)
	}
	return nil
}

// maxSchemaIdentifierLen bounds the byte length of a CREATE/DROP INDEX or
// CREATE/DROP CONSTRAINT identifier (name, label, or property). It is far above
// any legitimate schema identifier yet well below both the uint16 length prefix
// the WAL op-body encoders use (store/txn) and the 64 KiB per-field cap the
// snapshot reader enforces (store/snapshot), so an accepted identifier can
// never truncate in the WAL nor be written wider than the snapshot reader will
// accept (#1903). The DDL parser is a public boundary for untrusted query text,
// so an over-long identifier is rejected here — a fail-stop typed error —
// rather than silently mangled or lost at the durable layer (an ACID
// Consistency/Durability breach) or turned into a snapshot poison-pill.
const maxSchemaIdentifierLen = 4096

// checkSchemaIdentifier returns a typed error when id exceeds
// [maxSchemaIdentifierLen]. kind names the statement (e.g. "CREATE CONSTRAINT")
// and what names the field (e.g. "name") for an actionable message.
func checkSchemaIdentifier(kind, what, id string) error {
	if len(id) > maxSchemaIdentifierLen {
		return fmt.Errorf("ir: %s: %s is too long (%d bytes; maximum %d)",
			kind, what, len(id), maxSchemaIdentifierLen)
	}
	return nil
}

// IsDDL returns true when query (trimmed, case-insensitive) begins with a
// known DDL keyword that the lightweight DDL parser handles.
func IsDDL(query string) bool {
	upper := strings.ToUpper(strings.TrimSpace(query))
	return strings.HasPrefix(upper, "CREATE INDEX") ||
		strings.HasPrefix(upper, "DROP INDEX") ||
		strings.HasPrefix(upper, "CREATE CONSTRAINT") ||
		strings.HasPrefix(upper, "DROP CONSTRAINT")
}

// ParseDDL parses a DDL query string and returns a LogicalPlan (one of
// *CreateIndex, *DropIndex, *CreateConstraint, *DropConstraint). Returns an
// error for unrecognised DDL.
func ParseDDL(query string) (LogicalPlan, error) {
	upper := strings.ToUpper(strings.TrimSpace(query))
	switch {
	case strings.HasPrefix(upper, "CREATE INDEX"):
		return parseCreateIndex(strings.TrimSpace(query))
	case strings.HasPrefix(upper, "DROP INDEX"):
		return parseDropIndex(strings.TrimSpace(query))
	case strings.HasPrefix(upper, "CREATE CONSTRAINT"):
		return parseCreateConstraint(strings.TrimSpace(query))
	case strings.HasPrefix(upper, "DROP CONSTRAINT"):
		return parseDropConstraint(strings.TrimSpace(query))
	}
	return nil, fmt.Errorf("ir: unrecognised DDL statement: %q", query)
}

// ─────────────────────────────────────────────────────────────────────────────
// CREATE INDEX parser
// ─────────────────────────────────────────────────────────────────────────────

// tokenReader is a simple cursor over a token slice used by the DDL parsers.
type tokenReader struct {
	tokens []string
	pos    int
}

func newTokenReader(query string) *tokenReader { return &tokenReader{tokens: tokenise(query)} }

func (r *tokenReader) peek() string {
	if r.pos >= len(r.tokens) {
		return ""
	}
	return r.tokens[r.pos]
}

func (r *tokenReader) peekUpper() string { return strings.ToUpper(r.peek()) }

func (r *tokenReader) consume() string {
	if r.pos >= len(r.tokens) {
		return ""
	}
	t := r.tokens[r.pos]
	r.pos++
	return t
}

func (r *tokenReader) expectU(want string) error {
	tok := strings.ToUpper(r.consume())
	if tok != want {
		return fmt.Errorf("ir: DDL parser: expected %q, got %q", want, tok)
	}
	return nil
}

// parseCreateIndex parses:
//
//	CREATE INDEX [IF NOT EXISTS] [name] FOR (n:Label) ON (n.prop) [OPTIONS {indexType:'hash'|'btree'}]
func parseCreateIndex(query string) (*CreateIndex, error) {
	r := newTokenReader(query)

	if err := r.expectU("CREATE"); err != nil {
		return nil, err
	}
	if err := r.expectU("INDEX"); err != nil {
		return nil, err
	}

	// Accept BOTH clause orders around the optional name:
	//   CREATE INDEX [name] [IF NOT EXISTS] FOR …   (Neo4j / openCypher order)
	//   CREATE INDEX [IF NOT EXISTS] [name] FOR …   (legacy GoGraph order)
	// Neo4j places the name before IF NOT EXISTS; the parser historically only
	// accepted the reverse, rejecting the common migration idiom
	// `CREATE INDEX myidx IF NOT EXISTS FOR …` (audit 2026-07-13 #1982). Parse
	// IF NOT EXISTS on either side of the name.
	ifNotExists, err := parseIfNotExists(r)
	if err != nil {
		return nil, err
	}

	name := ""
	if kw := r.peekUpper(); kw != "FOR" && kw != "IF" {
		name = r.consume()
		// A user index name may not carry a reserved internal namespace: the
		// numeric-companion suffix (#F-CY5) — a btree CREATE INDEX registers an
		// internal "<label>_<prop>_btree_num" companion that db.indexes() hides
		// by that suffix — nor the "__uniq__" UNIQUE-constraint-backing prefix
		// (#1912). Auto-generated names end in "_hash"/"_btree" and never trip
		// these.
		if err := checkReservedIndexName("CREATE INDEX", name); err != nil {
			return nil, err
		}
	}

	// Neo4j order: IF NOT EXISTS may follow the name. Only look again when it was
	// not already consumed before the name.
	if !ifNotExists {
		ifNotExists, err = parseIfNotExists(r)
		if err != nil {
			return nil, err
		}
	}

	if err := r.expectU("FOR"); err != nil {
		return nil, err
	}

	label, err := parseNodePattern(r.tokens, &r.pos)
	if err != nil {
		return nil, fmt.Errorf("ir: CREATE INDEX %q: %w", name, err)
	}

	if err := r.expectU("ON"); err != nil {
		return nil, err
	}

	propKey, err := parsePropAccess(r.tokens, &r.pos)
	if err != nil {
		return nil, fmt.Errorf("ir: CREATE INDEX %q: %w", name, err)
	}

	idxType, err := parseOptionalIndexOptions(r, name)
	if err != nil {
		return nil, err
	}

	if name == "" {
		suffix := "hash"
		if idxType == IndexTypeBTree {
			suffix = "btree"
		}
		name = strings.ToLower(label) + "_" + strings.ToLower(propKey) + "_" + suffix
	}

	for _, f := range [...]struct{ what, val string }{
		{"name", name}, {"label", label}, {"property", propKey},
	} {
		if err := checkSchemaIdentifier("CREATE INDEX", f.what, f.val); err != nil {
			return nil, err
		}
	}

	return NewCreateIndex(name, label, propKey, idxType, ifNotExists), nil
}

// parseIfNotExists consumes "IF NOT EXISTS" when present and returns true.
func parseIfNotExists(r *tokenReader) (bool, error) {
	if r.peekUpper() != "IF" {
		return false, nil
	}
	r.consume() // IF
	if err := r.expectU("NOT"); err != nil {
		return false, err
	}
	if err := r.expectU("EXISTS"); err != nil {
		return false, err
	}
	return true, nil
}

// parseOptionalIndexOptions consumes the OPTIONS clause when present.
func parseOptionalIndexOptions(r *tokenReader, name string) (IndexType, error) {
	if r.peekUpper() != "OPTIONS" {
		return IndexTypeHash, nil
	}
	r.consume() // OPTIONS
	t, err := parseIndexOptions(r.tokens, &r.pos)
	if err != nil {
		return IndexTypeHash, fmt.Errorf("ir: CREATE INDEX %q options: %w", name, err)
	}
	return t, nil
}

// tokAt returns tokens[pos] and true, or ("", false) when pos is out of range.
// Every DDL sub-parser reads tokens through tokAt (never a bare tokens[pos]
// index) so truncated input returns a typed error instead of an
// index-out-of-range panic (#F-PARSER1): the DDL parser is a public boundary
// for untrusted query text and must fail with a SyntaxError, not crash.
func tokAt(tokens []string, pos int) (string, bool) {
	if pos < 0 || pos >= len(tokens) {
		return "", false
	}
	return tokens[pos], true
}

// tokDisplay renders tokens[pos] for an error message, or "end of input" when
// pos is past the end.
func tokDisplay(tokens []string, pos int) string {
	if t, ok := tokAt(tokens, pos); ok {
		return t
	}
	return "end of input"
}

// parseNodePattern parses "(n:Label)" at tokens[*pos] and advances *pos past
// the closing paren. Returns the Label string. Truncated input yields an error.
func parseNodePattern(tokens []string, pos *int) (string, error) {
	open, ok := tokAt(tokens, *pos)
	if !ok {
		return "", fmt.Errorf("expected node pattern (n:Label), got end of input")
	}
	if !strings.EqualFold(open, "(") {
		// Tokens may have included the parenthesis as part of a single token
		// if the query had no spaces. Try a fallback approach.
		return parseNodePatternCompact(tokens, pos)
	}
	(*pos)++ // (
	// Skip variable name (before ':').
	if _, ok := tokAt(tokens, *pos); !ok {
		return "", fmt.Errorf("unterminated node pattern: expected variable after '('")
	}
	(*pos)++
	// ':'
	if colon, ok := tokAt(tokens, *pos); !ok || colon != ":" {
		return "", fmt.Errorf("expected ':' in node pattern, got %q", tokDisplay(tokens, *pos))
	}
	(*pos)++
	// Label
	label, ok := tokAt(tokens, *pos)
	if !ok {
		return "", fmt.Errorf("expected label in node pattern, got end of input")
	}
	(*pos)++
	// )
	if closeTok, ok := tokAt(tokens, *pos); !ok || closeTok != ")" {
		return "", fmt.Errorf("expected ')' in node pattern, got %q", tokDisplay(tokens, *pos))
	}
	(*pos)++
	return label, nil
}

// parseNodePatternCompact handles the case where the pattern is a single token
// like "(n:Label)".
func parseNodePatternCompact(tokens []string, pos *int) (string, error) {
	tok, ok := tokAt(tokens, *pos)
	if !ok {
		return "", fmt.Errorf("expected node pattern (n:Label), got end of input")
	}
	// Strip optional parens.
	trimmed := strings.TrimSuffix(strings.TrimPrefix(tok, "("), ")")
	// Expect "var:Label".
	colonIdx := strings.Index(trimmed, ":")
	if colonIdx < 0 {
		return "", fmt.Errorf("expected node pattern (n:Label), got %q", tok)
	}
	(*pos)++
	return trimmed[colonIdx+1:], nil
}

// parsePropAccess parses "(n.prop)" and returns the property key. Truncated
// input yields an error.
func parsePropAccess(tokens []string, pos *int) (string, error) {
	open, ok := tokAt(tokens, *pos)
	if !ok {
		return "", fmt.Errorf("expected property access (n.prop), got end of input")
	}
	if !strings.EqualFold(open, "(") {
		return parsePropAccessCompact(tokens, pos)
	}
	(*pos)++ // (
	// "n.prop"
	access, ok := tokAt(tokens, *pos)
	if !ok {
		return "", fmt.Errorf("unterminated property access: expected n.prop after '('")
	}
	(*pos)++
	// A comma here means a multi-property list — a composite index. Give a
	// specific, actionable error rather than a bare "expected ')'" (#F-CY4):
	// composite indexes are out of scope (single node property only).
	if next, ok := tokAt(tokens, *pos); ok && next == "," {
		return "", fmt.Errorf("composite indexes (multiple properties) are not supported; index a single property")
	}
	if closeTok, ok := tokAt(tokens, *pos); !ok || closeTok != ")" {
		return "", fmt.Errorf("expected ')' in property access, got %q", tokDisplay(tokens, *pos))
	}
	(*pos)++
	// Extract property key from "n.prop".
	dotIdx := strings.LastIndex(access, ".")
	if dotIdx < 0 {
		return "", fmt.Errorf("expected n.prop form, got %q", access)
	}
	return access[dotIdx+1:], nil
}

func parsePropAccessCompact(tokens []string, pos *int) (string, error) {
	tok, ok := tokAt(tokens, *pos)
	if !ok {
		return "", fmt.Errorf("expected n.prop form, got end of input")
	}
	trimmed := strings.TrimSuffix(strings.TrimPrefix(tok, "("), ")")
	dotIdx := strings.LastIndex(trimmed, ".")
	if dotIdx < 0 {
		return "", fmt.Errorf("expected n.prop form, got %q", tok)
	}
	(*pos)++
	return trimmed[dotIdx+1:], nil
}

// parseIndexOptions parses "{indexType: 'hash'|'btree'}" and returns the
// chosen IndexType.
func parseIndexOptions(tokens []string, pos *int) (IndexType, error) {
	// Consume tokens until we find indexType value.
	// Accept any ordering; ignore unknown keys.
	// Reconstruct the options map from the token stream.
	if *pos >= len(tokens) || tokens[*pos] != "{" {
		// The brace may be part of the preceding token — try the full string approach.
		return IndexTypeHash, nil
	}
	(*pos)++ // {
	idxType := IndexTypeHash
	for *pos < len(tokens) && tokens[*pos] != "}" {
		key := strings.ToLower(tokens[*pos])
		(*pos)++
		if *pos < len(tokens) && tokens[*pos] == ":" {
			(*pos)++ // :
		}
		if *pos >= len(tokens) {
			break
		}
		val := strings.ToLower(strings.Trim(tokens[*pos], `"'`))
		(*pos)++
		if key == "indextype" {
			switch val {
			case "hash":
				idxType = IndexTypeHash
			case "btree":
				idxType = IndexTypeBTree
			default:
				return 0, fmt.Errorf("unknown indexType %q (want 'hash' or 'btree')", val)
			}
		}
		// Skip trailing commas.
		if *pos < len(tokens) && tokens[*pos] == "," {
			(*pos)++
		}
	}
	if *pos < len(tokens) && tokens[*pos] == "}" {
		(*pos)++
	}
	return idxType, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// DROP INDEX parser
// ─────────────────────────────────────────────────────────────────────────────

// parseDropIndex parses: DROP INDEX name [IF EXISTS]
func parseDropIndex(query string) (*DropIndex, error) {
	tokens := tokenise(query)
	pos := 0
	consume := func() string {
		if pos >= len(tokens) {
			return ""
		}
		t := tokens[pos]
		pos++
		return t
	}
	expectU := func(want string) error {
		tok := strings.ToUpper(consume())
		if tok != want {
			return fmt.Errorf("ir: DROP INDEX: expected %q, got %q", want, tok)
		}
		return nil
	}

	if err := expectU("DROP"); err != nil {
		return nil, err
	}
	if err := expectU("INDEX"); err != nil {
		return nil, err
	}
	name := consume()
	if name == "" {
		return nil, fmt.Errorf("ir: DROP INDEX: missing index name")
	}
	if err := checkSchemaIdentifier("DROP INDEX", "name", name); err != nil {
		return nil, err
	}
	// A UNIQUE constraint's backing index may only be dropped via DROP
	// CONSTRAINT, never by name here — otherwise the index catalogue and the
	// constraint set desynchronise (#1912).
	if err := checkReservedIndexName("DROP INDEX", name); err != nil {
		return nil, err
	}

	ifExists := false
	if strings.ToUpper(consume()) == "IF" {
		if strings.ToUpper(consume()) != "EXISTS" {
			return nil, fmt.Errorf("ir: DROP INDEX: expected EXISTS after IF")
		}
		ifExists = true
	}
	return NewDropIndex(name, ifExists), nil
}

// ─────────────────────────────────────────────────────────────────────────────
// CREATE CONSTRAINT parser
// ─────────────────────────────────────────────────────────────────────────────

// parseCreateConstraint parses both the modern and the legacy node-property
// constraint grammars, which map to the same IR:
//
//	CREATE CONSTRAINT [name] [IF NOT EXISTS] FOR (n:Label) REQUIRE n.prop IS UNIQUE
//	CREATE CONSTRAINT [name] [IF NOT EXISTS] FOR (n:Label) REQUIRE n.prop IS NOT NULL
//	CREATE CONSTRAINT [name] ON (n:Label) ASSERT n.prop IS UNIQUE [IF NOT EXISTS]
//	CREATE CONSTRAINT [name] ON (n:Label) ASSERT n.prop IS NOT NULL [IF NOT EXISTS]
//
// FOR … REQUIRE is the current Neo4j form (the ON … ASSERT form was removed in
// Neo4j 5 and is kept here as a legacy alias). Out-of-scope forms — relationship
// constraints, composite (multi-property) constraints, NODE KEY / relationship
// key, and property type constraints (IS :: <TYPE>) — are rejected with a
// specific error.
//
//nolint:gocyclo // parser function: complexity reflects DDL grammar, not hidden branching
func parseCreateConstraint(query string) (*CreateConstraint, error) {
	tokens := tokenise(query)
	pos := 0
	consume := func() string {
		if pos >= len(tokens) {
			return ""
		}
		t := tokens[pos]
		pos++
		return t
	}
	peek := func() string {
		if pos >= len(tokens) {
			return ""
		}
		return tokens[pos]
	}
	peekUpper := func() string { return strings.ToUpper(peek()) }
	expectU := func(want string) error {
		tok := strings.ToUpper(consume())
		if tok != want {
			return fmt.Errorf("ir: CREATE CONSTRAINT: expected %q, got %q", want, tok)
		}
		return nil
	}

	if err := expectU("CREATE"); err != nil {
		return nil, err
	}
	if err := expectU("CONSTRAINT"); err != nil {
		return nil, err
	}

	// Optional name (present unless the next token starts the constraint body:
	// FOR/ON, or IF for IF NOT EXISTS). Excluding FOR is essential — otherwise
	// the modern FOR … REQUIRE form mis-parses the keyword FOR as the name
	// (#1906).
	name := ""
	if u := peekUpper(); u != "ON" && u != "FOR" && u != "IF" {
		name = consume()
	}

	// Optional IF NOT EXISTS (before ON in some dialects, but we accept it here
	// for symmetry; the more common position is after the assertion — handled below)
	ifNotExists := false
	if peekUpper() == "IF" {
		consume() // IF
		if err := expectU("NOT"); err != nil {
			return nil, err
		}
		if err := expectU("EXISTS"); err != nil {
			return nil, err
		}
		ifNotExists = true
	}

	// Connective: modern FOR … REQUIRE (Neo4j 4.x+/current) or legacy
	// ON … ASSERT (Neo4j 3.x, removed in Neo4j 5). Both map to the same IR.
	var assertKW string
	switch peekUpper() {
	case "FOR":
		assertKW = "REQUIRE"
	case "ON":
		assertKW = "ASSERT"
	default:
		return nil, fmt.Errorf("ir: CREATE CONSTRAINT %q: expected FOR or ON, got %q", name, peek())
	}
	consume() // FOR | ON

	// Reject relationship constraints (FOR ()-[r:T]-() REQUIRE …): only node
	// property UNIQUE / NOT NULL is supported. Scan the pattern span up to the
	// assert keyword for a relationship bracket so the error is specific rather
	// than a misleading node-pattern parse failure.
	for i := pos; i < len(tokens) && !strings.EqualFold(tokens[i], assertKW); i++ {
		if strings.ContainsAny(tokens[i], "[]") {
			return nil, fmt.Errorf("ir: CREATE CONSTRAINT %q: relationship constraints are not supported; only node property UNIQUE and NOT NULL constraints are supported", name)
		}
	}

	// (n:Label)
	label, err := parseNodePattern(tokens, &pos)
	if err != nil {
		return nil, fmt.Errorf("ir: CREATE CONSTRAINT %q: %w", name, err)
	}

	if err := expectU(assertKW); err != nil {
		return nil, err
	}

	// n.prop — a single node property. A parenthesised composite (n.a, n.b) is
	// recognised and rejected (out of scope).
	propKey, err := parseConstraintProp(tokens, &pos)
	if err != nil {
		return nil, fmt.Errorf("ir: CREATE CONSTRAINT %q: %w", name, err)
	}

	if err := expectU("IS"); err != nil {
		return nil, err
	}

	// UNIQUE and NOT NULL are supported. NODE KEY, relationship-key, and
	// property type constraints (IS :: <TYPE> / IS TYPED <TYPE>) are recognised
	// and rejected with a specific error rather than a misleading parse failure.
	var kind ConstraintKind
	switch nextKw := strings.ToUpper(consume()); nextKw {
	case "UNIQUE":
		kind = ConstraintUnique
	case "NOT":
		if err := expectU("NULL"); err != nil {
			return nil, err
		}
		kind = ConstraintNotNull
	case "NODE":
		return nil, fmt.Errorf("ir: CREATE CONSTRAINT %q: NODE KEY constraints are not supported; declare IS UNIQUE and IS NOT NULL separately", name)
	case "RELATIONSHIP", "REL", "KEY":
		return nil, fmt.Errorf("ir: CREATE CONSTRAINT %q: key constraints are not supported; only node property UNIQUE and NOT NULL are supported", name)
	case ":", "TYPED":
		return nil, fmt.Errorf("ir: CREATE CONSTRAINT %q: property type constraints (IS :: <TYPE>) are not supported", name)
	default:
		return nil, fmt.Errorf("ir: CREATE CONSTRAINT %q: expected UNIQUE or NOT NULL after IS, got %q", name, nextKw)
	}

	// Optional IF NOT EXISTS (after assertion)
	if !ifNotExists && peekUpper() == "IF" {
		consume() // IF
		if err := expectU("NOT"); err != nil {
			return nil, err
		}
		if err := expectU("EXISTS"); err != nil {
			return nil, err
		}
		ifNotExists = true
	}

	// Auto-name when not provided.
	if name == "" {
		suffix := "unique"
		if kind == ConstraintNotNull {
			suffix = "not_null"
		}
		name = strings.ToLower(label) + "_" + strings.ToLower(propKey) + "_" + suffix
	}

	for _, f := range [...]struct{ what, val string }{
		{"name", name}, {"label", label}, {"property", propKey},
	} {
		if err := checkSchemaIdentifier("CREATE CONSTRAINT", f.what, f.val); err != nil {
			return nil, err
		}
	}

	return NewCreateConstraint(name, label, propKey, kind, ifNotExists), nil
}

// parseConstraintProp parses the property target of a REQUIRE/ASSERT clause and
// returns the single property key. The tokeniser keeps "n.prop" as one token
// (it does not split on "."), so the bare form is a single token. A
// parenthesised form is also accepted: a single "(n.prop)" is unwrapped, while a
// composite "(n.a, n.b)" is recognised and rejected — composite constraints are
// out of scope.
func parseConstraintProp(tokens []string, pos *int) (string, error) {
	tok, ok := tokAt(tokens, *pos)
	if !ok {
		return "", fmt.Errorf("expected n.prop, got end of input")
	}
	if tok == "(" {
		(*pos)++ // (
		first, ok := tokAt(tokens, *pos)
		if !ok {
			return "", fmt.Errorf("unterminated property list after '('")
		}
		(*pos)++
		if next, ok := tokAt(tokens, *pos); ok && next == "," {
			return "", fmt.Errorf("composite constraints (multiple properties) are not supported; constrain a single property")
		}
		if closeTok, ok := tokAt(tokens, *pos); !ok || closeTok != ")" {
			return "", fmt.Errorf("expected ')' after property, got %q", tokDisplay(tokens, *pos))
		}
		(*pos)++
		return propKeyFromAccess(first)
	}
	(*pos)++
	return propKeyFromAccess(tok)
}

// propKeyFromAccess extracts the property key from an "n.prop" access token.
func propKeyFromAccess(tok string) (string, error) {
	dotIdx := strings.LastIndex(tok, ".")
	if dotIdx < 0 {
		return "", fmt.Errorf("expected n.prop form, got %q", tok)
	}
	return tok[dotIdx+1:], nil
}

// ─────────────────────────────────────────────────────────────────────────────
// DROP CONSTRAINT parser
// ─────────────────────────────────────────────────────────────────────────────

// parseDropConstraint parses: DROP CONSTRAINT name [IF EXISTS]
func parseDropConstraint(query string) (*DropConstraint, error) {
	tokens := tokenise(query)
	pos := 0
	consume := func() string {
		if pos >= len(tokens) {
			return ""
		}
		t := tokens[pos]
		pos++
		return t
	}
	expectU := func(want string) error {
		tok := strings.ToUpper(consume())
		if tok != want {
			return fmt.Errorf("ir: DROP CONSTRAINT: expected %q, got %q", want, tok)
		}
		return nil
	}

	if err := expectU("DROP"); err != nil {
		return nil, err
	}
	if err := expectU("CONSTRAINT"); err != nil {
		return nil, err
	}
	name := consume()
	if name == "" {
		return nil, fmt.Errorf("ir: DROP CONSTRAINT: missing constraint name")
	}
	if err := checkSchemaIdentifier("DROP CONSTRAINT", "name", name); err != nil {
		return nil, err
	}

	ifExists := false
	if strings.ToUpper(consume()) == "IF" {
		if strings.ToUpper(consume()) != "EXISTS" {
			return nil, fmt.Errorf("ir: DROP CONSTRAINT: expected EXISTS after IF")
		}
		ifExists = true
	}
	// Kind is unknown when dropping by name only; default to ConstraintUnique
	// (the executor uses the registry to resolve the actual kind on drop).
	return NewDropConstraint(name, "", "", ConstraintUnique, ifExists), nil
}

// ─────────────────────────────────────────────────────────────────────────────
// tokenise — split a DDL string into tokens
// ─────────────────────────────────────────────────────────────────────────────

// tokenise splits a Cypher DDL string into tokens, treating whitespace as a
// separator and punctuation characters as individual tokens.
func tokenise(s string) []string {
	var tokens []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			tokens = append(tokens, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r':
			flush()
		case '(', ')', '{', '}', ':', ',', ';':
			flush()
			tokens = append(tokens, string(r))
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return tokens
}
