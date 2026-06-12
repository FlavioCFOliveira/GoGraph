package sema

import (
	"fmt"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg/schema"
)

// SchemaError is reported by [CheckSchema] when a property access is
// statically compared against a literal whose type contradicts the
// schema-declared kind for that property key.
type SchemaError struct {
	// PropertyName is the property key that triggered the mismatch.
	PropertyName string
	// DeclaredKind is the kind registered in the schema.
	DeclaredKind lpg.PropertyKind
	// UsedAs is the CypherType of the literal on the other side of the
	// comparison.
	UsedAs CypherType
	// Pos is the source position of the BinaryOp that holds the mismatch.
	Pos ast.Position
	// Hint is a human-readable suggestion to help the caller fix the query.
	Hint string
}

// Error implements the error interface.
func (e *SchemaError) Error() string {
	return fmt.Sprintf(
		"schema error at %s: property %q declared as %s but compared against %s — %s",
		e.Pos, e.PropertyName, kindName(e.DeclaredKind), e.UsedAs, e.Hint,
	)
}

// kindName maps a PropertyKind to a human-readable type name for diagnostics.
func kindName(k lpg.PropertyKind) string {
	switch k {
	case lpg.PropString:
		return "String"
	case lpg.PropInt64:
		return "Integer"
	case lpg.PropFloat64:
		return "Float"
	case lpg.PropBool:
		return "Boolean"
	case lpg.PropTime:
		return "Time"
	case lpg.PropBytes:
		return "Bytes"
	default:
		return fmt.Sprintf("PropertyKind(%d)", uint8(k))
	}
}

// kindCompatible reports whether the literal CypherType is compatible with
// the declared PropertyKind.  The mapping is:
//
//   - PropString  ↔ TypeString
//   - PropInt64   ↔ TypeInteger
//   - PropFloat64 ↔ TypeFloat or TypeInteger (integer literals widen to float)
//   - PropBool    ↔ TypeBoolean
//
// All other combinations are mismatches.  TypeAny or TypeNull are always
// considered compatible (the value is not statically known).
func kindCompatible(kind lpg.PropertyKind, lit CypherType) bool {
	if lit == TypeAny || lit == TypeNull {
		return true
	}
	switch kind {
	case lpg.PropString:
		return lit == TypeString
	case lpg.PropInt64:
		return lit == TypeInteger
	case lpg.PropFloat64:
		return lit == TypeFloat || lit == TypeInteger
	case lpg.PropBool:
		return lit == TypeBoolean
	default:
		// PropTime, PropBytes: no Cypher literal maps to these; treat as
		// compatible to avoid false positives.
		return true
	}
}

// CheckSchema validates property accesses in q against the declared schema.
// If schema is nil, this is a no-op and an empty slice is returned.
//
// The check is applied to every [ast.BinaryOp] node that has:
//   - one side being a [ast.Property] access, AND
//   - the other side being a literal whose static type can be determined.
//
// When the declared PropertyKind for the property key is incompatible with the
// literal's type, a [SchemaError] is appended to the result.
//
// If a property key is not registered in the schema the access is silently
// skipped (warning-only / partial-schema policy).
//
// CheckSchema is a pure function and safe for concurrent use.
func CheckSchema(q ast.Query, sch *schema.Schema) []SchemaError {
	if sch == nil {
		return nil
	}
	c := &schemaChecker{sch: sch}
	c.query(q)
	return c.errs
}

// ─────────────────────────────────────────────────────────────────────────────
// Internal walker
// ─────────────────────────────────────────────────────────────────────────────

type schemaChecker struct {
	sch  *schema.Schema
	errs []SchemaError
}

func (c *schemaChecker) query(q ast.Query) {
	switch v := q.(type) {
	case *ast.SingleQuery:
		c.singleQuery(v)
	case *ast.MultiQuery:
		for _, part := range v.Parts {
			c.singleQuery(part)
		}
	}
}

func (c *schemaChecker) singleQuery(q *ast.SingleQuery) {
	for _, rc := range q.ReadingClauses {
		c.readingClause(rc)
	}
	for _, w := range q.With {
		c.withClause(w)
	}
	for _, uc := range q.UpdatingClauses {
		c.updatingClause(uc)
	}
	if q.Return != nil {
		c.projection(q.Return.Projection)
	}
}

func (c *schemaChecker) readingClause(rc ast.ReadingClause) {
	switch v := rc.(type) {
	case *ast.Match:
		if v.Where != nil {
			c.expr(v.Where.Predicate)
		}
	case *ast.OptionalMatch:
		if v.Where != nil {
			c.expr(v.Where.Predicate)
		}
	case *ast.Unwind:
		c.expr(v.Expr)
	case *ast.With:
		c.withClause(v)
	case *ast.Call:
		for _, arg := range v.Args {
			c.expr(arg)
		}
		if v.Where != nil {
			c.expr(v.Where.Predicate)
		}
	case *ast.Return:
		c.projection(v.Projection)
	}
}

func (c *schemaChecker) withClause(w *ast.With) {
	for _, item := range w.Projection.Items {
		c.expr(item.Expr)
	}
	if w.Where != nil {
		c.expr(w.Where.Predicate)
	}
}

//nolint:gocyclo // One branch per concrete UpdatingClause type; complexity is structural, not reducible.
func (c *schemaChecker) updatingClause(uc ast.UpdatingClause) {
	switch v := uc.(type) {
	case *ast.Set:
		for _, item := range v.Items {
			c.expr(item.Target)
			if item.Value != nil {
				c.expr(item.Value)
			}
		}
	case *ast.Merge:
		for _, item := range v.OnCreate {
			c.expr(item.Target)
			if item.Value != nil {
				c.expr(item.Value)
			}
		}
		for _, item := range v.OnMatch {
			c.expr(item.Target)
			if item.Value != nil {
				c.expr(item.Value)
			}
		}
	case *ast.Delete:
		for _, e := range v.Expressions {
			c.expr(e)
		}
	case *ast.DetachDelete:
		for _, e := range v.Expressions {
			c.expr(e)
		}
	case *ast.Remove:
		for _, item := range v.Items {
			c.expr(item.Target)
		}
	case *ast.Call:
		for _, arg := range v.Args {
			c.expr(arg)
		}
		if v.Where != nil {
			c.expr(v.Where.Predicate)
		}
	}
}

func (c *schemaChecker) projection(p *ast.Projection) {
	if p == nil {
		return
	}
	for _, item := range p.Items {
		c.expr(item.Expr)
	}
	for _, s := range p.OrderBy {
		c.expr(s.Expr)
	}
	if p.Skip != nil {
		c.expr(p.Skip)
	}
	if p.Limit != nil {
		c.expr(p.Limit)
	}
}

// expr walks the expression tree and checks every BinaryOp that pairs a
// Property access with a literal.
//
//nolint:gocyclo // Structural switch over concrete Expression types; not reducible.
func (c *schemaChecker) expr(e ast.Expression) {
	if e == nil {
		return
	}
	switch v := e.(type) {
	case *ast.BinaryOp:
		c.checkBinaryOp(v)
		// Recurse into both sides for nested operators.
		c.expr(v.Left)
		c.expr(v.Right)

	case *ast.UnaryOp:
		c.expr(v.Operand)

	case *ast.Property:
		// Stand-alone property access: no literal to compare against.
		c.expr(v.Receiver)

	case *ast.FunctionInvocation:
		for _, arg := range v.Args {
			c.expr(arg)
		}

	case *ast.CaseExpression:
		c.expr(v.Subject)
		for _, alt := range v.Alternatives {
			c.expr(alt.Condition)
			c.expr(alt.Consequent)
		}
		c.expr(v.ElseExpr)

	case *ast.ListLiteral:
		for _, elem := range v.Elements {
			c.expr(elem)
		}

	case *ast.MapLiteral:
		for _, val := range v.Values {
			c.expr(val)
		}

	case *ast.SubscriptExpr:
		c.expr(v.Expr)
		c.expr(v.Index)

	case *ast.SliceExpr:
		c.expr(v.Expr)
		c.expr(v.From)
		c.expr(v.To)

	case *ast.ReduceExpr:
		c.expr(v.Init)
		c.expr(v.Source)
		c.expr(v.Projection)

	case *ast.ListComprehension:
		c.expr(v.Source)
		c.expr(v.Predicate)
		c.expr(v.Projection)

	case *ast.PatternComprehension:
		c.expr(v.Predicate)
		c.expr(v.Projection)

	case *ast.MapProjection:
		c.expr(v.Subject)
		for _, item := range v.Items {
			if !item.IsAll && item.Value != nil {
				c.expr(item.Value)
			}
		}

	case *ast.ExistsSubquery:
		if v.Query != nil {
			c.singleQuery(v.Query)
		}

	case *ast.CountSubquery:
		if v.Query != nil {
			c.singleQuery(v.Query)
		}

	// Literals, variables, parameters, and path patterns carry no sub-expressions
	// relevant to schema checking.
	default:
		// no-op
	}
}

// checkBinaryOp inspects a single BinaryOp: if one side is a Property and the
// other is a literal, it validates the literal type against the schema.
func (c *schemaChecker) checkBinaryOp(b *ast.BinaryOp) {
	// Only comparison operators are relevant for schema checking; arithmetic
	// operators do not produce type-mismatch schema errors.
	switch b.Operator {
	case "=", "<>", "<", ">", "<=", ">=":
		// continue
	default:
		return
	}

	prop, lit, ok := extractPropLiteral(b.Left, b.Right)
	if !ok {
		// Try flipped sides.
		prop, lit, ok = extractPropLiteral(b.Right, b.Left)
	}
	if !ok {
		return
	}

	kind, registered := c.sch.PropertyKind(prop.Key)
	if !registered {
		// Partial schema: property not declared → no-op.
		return
	}

	litType, _ := InferType(lit)
	if litType == TypeAny || litType == TypeNull {
		// Cannot statically determine literal type; skip.
		return
	}

	if !kindCompatible(kind, litType) {
		c.errs = append(c.errs, SchemaError{
			PropertyName: prop.Key,
			DeclaredKind: kind,
			UsedAs:       litType,
			Pos:          b.Pos,
			Hint: fmt.Sprintf(
				"property %q is declared as %s; use a %s literal instead of %s",
				prop.Key, kindName(kind), kindName(kind), litType,
			),
		})
	}
}

// extractPropLiteral returns the Property and the literal Expression when
// candidate is a *ast.Property and other is a literal node.  Returns ok=false
// otherwise.
func extractPropLiteral(candidate, other ast.Expression) (*ast.Property, ast.Expression, bool) {
	prop, ok := candidate.(*ast.Property)
	if !ok {
		return nil, nil, false
	}
	if !isLiteral(other) {
		return nil, nil, false
	}
	return prop, other, true
}

// isLiteral reports whether e is a statically-typed literal node.
func isLiteral(e ast.Expression) bool {
	switch e.(type) {
	case *ast.IntLiteral, *ast.FloatLiteral, *ast.StringLiteral,
		*ast.BoolLiteral, *ast.NullLiteral:
		return true
	default:
		return false
	}
}
