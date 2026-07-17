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
	"regexp"
	"strings"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/parser"
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
// known DDL keyword that the lightweight DDL parser handles. This includes the
// SHOW CONSTRAINTS / SHOW INDEXES read-only schema-introspection statements
// ([IsShow]) so they route through [ParseDDL] and bypass the ANTLR grammar
// exactly as the CREATE/DROP DDL statements do (#1922). SHOW is DDL for
// dispatch purposes but is a pure read, not a schema write; callers that must
// distinguish the two (for example, to permit SHOW on a read-only transaction)
// use [IsShow].
func IsDDL(query string) bool {
	upper := strings.ToUpper(strings.TrimSpace(query))
	return strings.HasPrefix(upper, "CREATE INDEX") ||
		strings.HasPrefix(upper, "DROP INDEX") ||
		strings.HasPrefix(upper, "CREATE CONSTRAINT") ||
		strings.HasPrefix(upper, "DROP CONSTRAINT") ||
		isShowUpper(upper)
}

// IsShow returns true when query (trimmed, case-insensitive) is a SHOW
// CONSTRAINTS / SHOW INDEXES schema-introspection statement (or its singular
// SHOW CONSTRAINT / SHOW INDEX form). SHOW is classified as DDL by [IsDDL] for
// dispatch, but unlike CREATE/DROP it is a pure read that emits a result set and
// mutates nothing, so it is permitted on a read-only transaction where the
// schema-writing DDL statements are rejected (#1922).
func IsShow(query string) bool {
	return isShowUpper(strings.ToUpper(strings.TrimSpace(query)))
}

// isShowUpper reports whether upper (an already trimmed, upper-cased query)
// begins with a supported SHOW schema-introspection keyword. The prefix
// "SHOW CONSTRAINT" matches both CONSTRAINT and CONSTRAINTS, and "SHOW INDEX"
// matches both INDEX and INDEXES; the exact singular/plural token is validated
// by [parseShow]. Sharing this helper keeps [IsDDL] and [IsShow] in lockstep.
func isShowUpper(upper string) bool {
	return strings.HasPrefix(upper, "SHOW CONSTRAINT") ||
		strings.HasPrefix(upper, "SHOW INDEX")
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
	case isShowUpper(upper):
		return parseShow(strings.TrimSpace(query))
	}
	return nil, fmt.Errorf("ir: unrecognised DDL statement: %q", query)
}

// ─────────────────────────────────────────────────────────────────────────────
// SHOW CONSTRAINTS / SHOW INDEXES parser (#1922)
// ─────────────────────────────────────────────────────────────────────────────

// showPrefixRe matches the SHOW CONSTRAINT(S) / SHOW INDEX(ES) command prefix
// and captures (1) the target keyword and (2) the remaining tail. INDEXES is
// listed before INDEX so the longer plural wins; the trailing \b rejects a
// near-miss such as "CONSTRAINTX". (?is): case-insensitive and "." spans
// newlines so a multi-line YIELD/WHERE/RETURN tail is captured whole.
var showPrefixRe = regexp.MustCompile(`(?is)^\s*SHOW\s+(CONSTRAINTS?|INDEXES|INDEX)\b(.*)$`)

// parseShow parses the read-only schema-introspection statements:
//
//	SHOW CONSTRAINTS   (and the singular SHOW CONSTRAINT)
//	SHOW INDEXES       (and the singular SHOW INDEX)
//
// optionally followed by a YIELD / WHERE / RETURN projection (#2044):
//
//	SHOW CONSTRAINTS YIELD name, type AS t [WHERE <pred>] [RETURN <items>]
//	SHOW INDEXES     YIELD *
//	SHOW CONSTRAINTS WHERE <pred>            (no YIELD: scope is every column)
//
// A single optional trailing ";" is tolerated. The legacy Neo4j BRIEF / VERBOSE
// suffixes — and any other unrecognised trailing clause — are rejected with a
// specific error rather than silently ignored. The parser is case-insensitive on
// keywords, matching the rest of the DDL parser.
func parseShow(query string) (LogicalPlan, error) {
	m := showPrefixRe.FindStringSubmatch(query)
	if m == nil {
		// isShowUpper already gated the "SHOW CONSTRAINT" / "SHOW INDEX" prefix,
		// so reaching here means a near-miss target (e.g. "SHOW CONSTRAINTX").
		got := ""
		if toks := tokenise(query); len(toks) >= 2 {
			got = toks[1]
		}
		return nil, fmt.Errorf("ir: SHOW: expected CONSTRAINT(S) or INDEX(ES), got %q", got)
	}

	isConstraints := strings.HasPrefix(strings.ToUpper(m[1]), "CONSTRAINT")
	columns := ShowIndexColumns
	if isConstraints {
		columns = ShowConstraintColumns
	}

	tail := strings.TrimSpace(m[2])
	// Tolerate a single trailing statement terminator.
	if strings.HasSuffix(tail, ";") {
		tail = strings.TrimSpace(tail[:len(tail)-1])
	}

	var proj *ShowProjection
	if tail != "" {
		var err error
		if proj, err = parseShowProjection(tail, columns); err != nil {
			return nil, err
		}
	}

	if isConstraints {
		return &ShowConstraints{Projection: proj}, nil
	}
	return &ShowIndexes{Projection: proj}, nil
}

// parseShowProjection parses the YIELD / WHERE / RETURN tail of a SHOW command
// (#2044) into a [ShowProjection], validating it against columns (the SHOW
// command's default output columns).
//
// The tail is re-parsed with the existing ANTLR expression grammar WITHOUT
// modifying that grammar: it is spliced onto a synthetic "CALL __g.s() …" clause
// — whose "CALL proc() YIELD items [WHERE pred] [RETURN …]" shape the grammar
// already accepts — and parsed with [parser.Parse]. The unknown procedure name
// never fails at parse time (procedure resolution is a later engine step this
// path does not reach), so the parse yields exactly the YIELD items, the WHERE
// predicate, and the RETURN projection. The WHERE-without-YIELD form is
// normalised by injecting an explicit YIELD of every default column, so its
// scope is the full column set and the shared extraction path handles it too.
func parseShowProjection(tail string, columns []string) (*ShowProjection, error) {
	upper := strings.ToUpper(tail)
	var synthetic string
	explicitYield := false
	switch {
	case hasKeywordPrefix(upper, "YIELD"):
		synthetic = "CALL __g.s() " + tail
		explicitYield = true
	case hasKeywordPrefix(upper, "WHERE"):
		// WHERE with no YIELD: scope is every default column. Inject an explicit
		// YIELD of all columns so the shared extraction path below handles it.
		synthetic = "CALL __g.s() YIELD " + strings.Join(columns, ", ") + " " + tail
	default:
		return nil, fmt.Errorf("ir: SHOW: unsupported clause %q; supported forms are the "+
			"plain SHOW …, SHOW … YIELD …, and SHOW … WHERE … "+
			"(BRIEF and VERBOSE are not supported)", firstToken(tail))
	}

	q, perr := parser.Parse(synthetic)
	if perr != nil {
		return nil, fmt.Errorf("ir: SHOW: unsupported YIELD form: %w", perr)
	}
	return buildShowProjection(q, columns, explicitYield)
}

// buildShowProjection extracts the YIELD projection, WHERE predicate, and RETURN
// clause from the re-parsed synthetic CALL query and validates the SHOW-command
// constraints (known columns, the YIELD scope barrier, RETURN⇒YIELD, and the
// rejected RETURN sub-clauses).
func buildShowProjection(q ast.Query, columns []string, explicitYield bool) (*ShowProjection, error) {
	sq, ok := q.(*ast.SingleQuery)
	if !ok {
		return nil, fmt.Errorf("ir: SHOW: unsupported projection")
	}
	// The synthetic query is exactly one CALL reading clause plus an optional
	// RETURN. Anything else (an injected MATCH/WITH/UNWIND/CREATE) is out of
	// scope for SHOW and rejected.
	if len(sq.ReadingClauses) != 1 || len(sq.UpdatingClauses) != 0 || len(sq.With) != 0 {
		return nil, fmt.Errorf("ir: SHOW: only YIELD, WHERE and RETURN are supported after SHOW")
	}
	call, ok := sq.ReadingClauses[0].(*ast.Call)
	if !ok {
		return nil, fmt.Errorf("ir: SHOW: unsupported projection")
	}

	if sq.Return != nil && !explicitYield {
		return nil, fmt.Errorf("ir: SHOW: RETURN requires an explicit YIELD clause")
	}

	project, err := resolveShowYield(call.Yield, columns, sq.Return != nil)
	if err != nil {
		return nil, err
	}

	inScope := make(map[string]bool, len(project))
	for _, p := range project {
		inScope[p.Output] = true
	}

	proj := &ShowProjection{Project: project}
	if call.Where != nil {
		if err := checkShowScope(call.Where.Predicate, inScope); err != nil {
			return nil, err
		}
		proj.Where = call.Where.Predicate
	}
	if sq.Return != nil {
		if err := checkShowReturn(sq.Return.Projection, inScope); err != nil {
			return nil, err
		}
		proj.Return = sq.Return.Projection
		if !sq.Return.Projection.All {
			cols := make([]string, len(sq.Return.Projection.Items))
			for i, item := range sq.Return.Projection.Items {
				cols[i] = projectionColumnName(item)
			}
			proj.ReturnColumns = cols
		}
	}
	return proj, nil
}

// resolveShowYield turns the parsed YIELD items into the ordered [ShowYield]
// projection, validating each named column against the SHOW command's columns.
// A nil yield is unreachable (the caller only re-parses a YIELD/WHERE tail); an
// empty yield is YIELD *, which expands to every column and is forbidden when a
// RETURN follows (Neo4j: YIELD * cannot be combined with RETURN).
func resolveShowYield(yield []*ast.YieldItem, columns []string, hasReturn bool) ([]ShowYield, error) {
	if len(yield) == 0 {
		if hasReturn {
			return nil, fmt.Errorf("ir: SHOW: YIELD * cannot be combined with RETURN; " +
				"list the yielded columns explicitly")
		}
		project := make([]ShowYield, len(columns))
		for i, c := range columns {
			project[i] = ShowYield{Source: c, Output: c}
		}
		return project, nil
	}
	project := make([]ShowYield, 0, len(yield))
	for _, yi := range yield {
		if !containsString(columns, yi.Name) {
			return nil, fmt.Errorf("ir: SHOW: YIELD refers to unknown column %q (available: %s)",
				yi.Name, strings.Join(columns, ", "))
		}
		out := yi.Name
		if yi.Alias != nil {
			out = *yi.Alias
		}
		project = append(project, ShowYield{Source: yi.Name, Output: out})
	}
	return project, nil
}

// checkShowReturn validates a RETURN projection on a SHOW command: it rejects the
// pipeline sub-clauses the executor does not implement (DISTINCT, ORDER BY, SKIP,
// LIMIT, aggregation) and enforces the YIELD scope barrier on every returned
// expression. RETURN * (Projection.All) returns the yielded columns and needs no
// per-item checking.
func checkShowReturn(p *ast.Projection, inScope map[string]bool) error {
	if p.Distinct {
		return fmt.Errorf("ir: SHOW: RETURN DISTINCT is not supported")
	}
	if len(p.OrderBy) != 0 || p.Skip != nil || p.Limit != nil {
		return fmt.Errorf("ir: SHOW: ORDER BY / SKIP / LIMIT are not supported")
	}
	if p.All {
		return nil
	}
	for _, item := range p.Items {
		if containsAggregate(item.Expr) {
			return fmt.Errorf("ir: SHOW: aggregation in RETURN is not supported")
		}
		if err := checkShowScope(item.Expr, inScope); err != nil {
			return err
		}
	}
	return nil
}

// checkShowScope walks e and returns an error if it references a variable that is
// not in scope (the YIELD scope barrier) or uses an expression construct that is
// not meaningful over SHOW rows (a subquery or graph pattern). List
// comprehensions and reduce introduce their own bound variable, which is added to
// the scope for the descent into their sub-expressions.
func checkShowScope(e ast.Expression, inScope map[string]bool) error { //nolint:gocyclo // per-AST-node dispatch; each branch is a simple recursion
	switch n := e.(type) {
	case nil:
		return nil
	case *ast.Variable:
		if !inScope[n.Name] {
			return fmt.Errorf("ir: SHOW: variable %q is not defined; only yielded columns are in scope", n.Name)
		}
	case *ast.Parameter, *ast.NullLiteral, *ast.BoolLiteral, *ast.IntLiteral,
		*ast.FloatLiteral, *ast.StringLiteral, *ast.StarLiteral:
		// Leaves that reference no variable.
	case *ast.Property:
		return checkShowScope(n.Receiver, inScope)
	case *ast.BinaryOp:
		if err := checkShowScope(n.Left, inScope); err != nil {
			return err
		}
		return checkShowScope(n.Right, inScope)
	case *ast.UnaryOp:
		return checkShowScope(n.Operand, inScope)
	case *ast.SubscriptExpr:
		if err := checkShowScope(n.Expr, inScope); err != nil {
			return err
		}
		return checkShowScope(n.Index, inScope)
	case *ast.SliceExpr:
		return checkShowScopeAll(inScope, n.Expr, n.From, n.To)
	case *ast.ListLiteral:
		return checkShowScopeAll(inScope, n.Elements...)
	case *ast.MapLiteral:
		return checkShowScopeAll(inScope, n.Values...)
	case *ast.FunctionInvocation:
		return checkShowScopeAll(inScope, n.Args...)
	case *ast.CaseExpression:
		if err := checkShowScopeAll(inScope, n.Subject, n.ElseExpr); err != nil {
			return err
		}
		for _, alt := range n.Alternatives {
			if err := checkShowScopeAll(inScope, alt.Condition, alt.Consequent); err != nil {
				return err
			}
		}
	case *ast.ListComprehension:
		return checkShowComprehension(n.Variable, inScope, n.Source, n.Predicate, n.Projection)
	case *ast.ReduceExpr:
		if err := checkShowScope(n.Init, inScope); err != nil {
			return err
		}
		// Both the accumulator and the per-element variable are in scope inside
		// the reduce projection.
		return checkShowComprehension(n.ElemVar, addScope(inScope, n.AccVar), n.Source, n.Projection)
	default:
		return fmt.Errorf("ir: SHOW: unsupported expression %T in WHERE/RETURN", e)
	}
	return nil
}

// checkShowScopeAll runs [checkShowScope] over every non-nil expression in exprs.
func checkShowScopeAll(inScope map[string]bool, exprs ...ast.Expression) error {
	for _, e := range exprs {
		if e == nil {
			continue
		}
		if err := checkShowScope(e, inScope); err != nil {
			return err
		}
	}
	return nil
}

// checkShowComprehension checks the sub-expressions of a comprehension/reduce
// with bound (the comprehension's element variable) added to the in-scope set
// for the descent. The Source expression sits in the outer scope; the
// predicate/projection run with the bound variable visible.
func checkShowComprehension(bound string, inScope map[string]bool, source ast.Expression, inner ...ast.Expression) error {
	if err := checkShowScope(source, inScope); err != nil {
		return err
	}
	return checkShowScopeAll(addScope(inScope, bound), inner...)
}

// addScope returns a copy of inScope with name added, leaving the original
// untouched so a comprehension's bound variable does not leak into a sibling
// sub-expression.
func addScope(inScope map[string]bool, name string) map[string]bool {
	out := make(map[string]bool, len(inScope)+1)
	for k := range inScope {
		out[k] = true
	}
	out[name] = true
	return out
}

// hasKeywordPrefix reports whether upper (an already upper-cased string) begins
// with keyword kw followed by a word boundary (whitespace or end of string), so
// "YIELDED" does not match the "YIELD" keyword.
func hasKeywordPrefix(upper, kw string) bool {
	if !strings.HasPrefix(upper, kw) {
		return false
	}
	rest := upper[len(kw):]
	return rest == "" || rest[0] == ' ' || rest[0] == '\t' || rest[0] == '\n' || rest[0] == '\r'
}

// firstToken returns the first whitespace-delimited token of s, for an error
// message that names the unsupported clause.
func firstToken(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return f[0]
	}
	return s
}

// containsString reports whether s is an element of list.
func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
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
