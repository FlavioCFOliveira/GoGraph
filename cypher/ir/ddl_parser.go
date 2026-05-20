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

// IsDDL returns true when query (trimmed, case-insensitive) begins with a
// known DDL keyword that the lightweight DDL parser handles.
func IsDDL(query string) bool {
	upper := strings.ToUpper(strings.TrimSpace(query))
	return strings.HasPrefix(upper, "CREATE INDEX") ||
		strings.HasPrefix(upper, "DROP INDEX")
}

// ParseDDL parses a DDL query string and returns a LogicalPlan (either
// *CreateIndex or *DropIndex). Returns an error for unrecognised DDL.
func ParseDDL(query string) (LogicalPlan, error) {
	upper := strings.ToUpper(strings.TrimSpace(query))
	switch {
	case strings.HasPrefix(upper, "CREATE INDEX"):
		return parseCreateIndex(strings.TrimSpace(query))
	case strings.HasPrefix(upper, "DROP INDEX"):
		return parseDropIndex(strings.TrimSpace(query))
	}
	return nil, fmt.Errorf("ir: unrecognised DDL statement: %q", query)
}

// ─────────────────────────────────────────────────────────────────────────────
// CREATE INDEX parser
// ─────────────────────────────────────────────────────────────────────────────

// parseCreateIndex parses:
//
//	CREATE INDEX [IF NOT EXISTS] [name] FOR (n:Label) ON (n.prop) [OPTIONS {indexType:'hash'|'btree'}]
func parseCreateIndex(query string) (*CreateIndex, error) {
	// Tokenise preserving original case except for keyword detection.
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
			return fmt.Errorf("ir: CREATE INDEX: expected %q, got %q", want, tok)
		}
		return nil
	}

	// "CREATE"
	if err := expectU("CREATE"); err != nil {
		return nil, err
	}
	// "INDEX"
	if err := expectU("INDEX"); err != nil {
		return nil, err
	}

	// Optional IF NOT EXISTS
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

	// Optional name (present unless next token is "FOR")
	name := ""
	if peekUpper() != "FOR" {
		name = consume()
	}

	// "FOR"
	if err := expectU("FOR"); err != nil {
		return nil, err
	}

	// (n:Label)
	label, err := parseNodePattern(tokens, &pos)
	if err != nil {
		return nil, fmt.Errorf("ir: CREATE INDEX %q: %w", name, err)
	}

	// "ON"
	if err := expectU("ON"); err != nil {
		return nil, err
	}

	// (n.prop)
	propKey, err := parsePropAccess(tokens, &pos)
	if err != nil {
		return nil, fmt.Errorf("ir: CREATE INDEX %q: %w", name, err)
	}

	// Optional OPTIONS {indexType: 'hash'|'btree'}
	idxType := IndexTypeHash
	if peekUpper() == "OPTIONS" {
		consume() // OPTIONS
		t, err2 := parseIndexOptions(tokens, &pos)
		if err2 != nil {
			return nil, fmt.Errorf("ir: CREATE INDEX %q options: %w", name, err2)
		}
		idxType = t
	}

	// Auto-name: label_prop_type
	if name == "" {
		suffix := "hash"
		if idxType == IndexTypeBTree {
			suffix = "btree"
		}
		name = strings.ToLower(label) + "_" + strings.ToLower(propKey) + "_" + suffix
	}

	return NewCreateIndex(name, label, propKey, idxType, ifNotExists), nil
}

// parseNodePattern parses "(n:Label)" at tokens[*pos] and advances *pos past
// the closing paren. Returns the Label string.
func parseNodePattern(tokens []string, pos *int) (string, error) {
	if strings.ToUpper(tokens[*pos]) != "(" {
		// Tokens may have included the parenthesis as part of a single token
		// if the query had no spaces. Try a fallback approach.
		return parseNodePatternCompact(tokens, pos)
	}
	(*pos)++ // (
	// Skip variable name (before ':')
	varName := tokens[*pos]
	_ = varName
	(*pos)++
	// ':'
	if tokens[*pos] != ":" {
		return "", fmt.Errorf("expected ':' in node pattern, got %q", tokens[*pos])
	}
	(*pos)++
	// Label
	label := tokens[*pos]
	(*pos)++
	// )
	if tokens[*pos] != ")" {
		return "", fmt.Errorf("expected ')' in node pattern, got %q", tokens[*pos])
	}
	(*pos)++
	return label, nil
}

// parseNodePatternCompact handles the case where the pattern is a single token
// like "(n:Label)".
func parseNodePatternCompact(tokens []string, pos *int) (string, error) {
	tok := tokens[*pos]
	// Strip optional parens.
	tok = strings.TrimPrefix(tok, "(")
	tok = strings.TrimSuffix(tok, ")")
	// Expect "var:Label".
	colonIdx := strings.Index(tok, ":")
	if colonIdx < 0 {
		return "", fmt.Errorf("expected node pattern (n:Label), got %q", tokens[*pos])
	}
	(*pos)++
	return tok[colonIdx+1:], nil
}

// parsePropAccess parses "(n.prop)" and returns the property key.
func parsePropAccess(tokens []string, pos *int) (string, error) {
	if strings.ToUpper(tokens[*pos]) != "(" {
		return parsePropAccessCompact(tokens, pos)
	}
	(*pos)++ // (
	// "n.prop"
	access := tokens[*pos]
	(*pos)++
	if tokens[*pos] != ")" {
		return "", fmt.Errorf("expected ')' in property access, got %q", tokens[*pos])
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
	tok := tokens[*pos]
	tok = strings.TrimPrefix(tok, "(")
	tok = strings.TrimSuffix(tok, ")")
	dotIdx := strings.LastIndex(tok, ".")
	if dotIdx < 0 {
		return "", fmt.Errorf("expected n.prop form, got %q", tokens[*pos])
	}
	(*pos)++
	return tok[dotIdx+1:], nil
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
		switch {
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			flush()
		case r == '(' || r == ')' || r == '{' || r == '}' || r == ':' || r == ',' || r == ';':
			flush()
			tokens = append(tokens, string(r))
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return tokens
}
