package sema

import (
	"fmt"
	"strings"

	"gograph/cypher/ast"
)

// CypherType enumerates the static types that can be inferred for a Cypher
// expression. The zero value is TypeAny, which is used when no more specific
// type can be determined (e.g. for variables and parameters in the absence of
// scope context).
type CypherType uint8

const (
	// TypeAny is the top type: any value is assignable to it. Used for
	// variables, parameters, and expressions whose type cannot be statically
	// narrowed.
	TypeAny CypherType = iota

	// TypeNull is the type of the literal null value.
	TypeNull

	// TypeBoolean is the type of boolean expressions.
	TypeBoolean

	// TypeInteger is the type of integer literals and integer-valued functions.
	TypeInteger

	// TypeFloat is the type of floating-point literals and float-valued
	// functions, as well as the result of mixed Int+Float arithmetic.
	TypeFloat

	// TypeString is the type of string literals and string-valued functions.
	TypeString

	// TypeNode is the type of graph node entities.
	TypeNode

	// TypeRelationship is the type of graph relationship entities.
	TypeRelationship

	// TypePath is the type of graph path values.
	TypePath

	// TypeList is the type of list values. The element type is not tracked at
	// this level of inference; use TypeList for all list expressions.
	TypeList

	// TypeMap is the type of map literals and map-valued expressions.
	TypeMap
)

// String returns the Cypher type name as it would appear in an error message.
func (t CypherType) String() string {
	switch t {
	case TypeAny:
		return "Any"
	case TypeNull:
		return "Null"
	case TypeBoolean:
		return "Boolean"
	case TypeInteger:
		return "Integer"
	case TypeFloat:
		return "Float"
	case TypeString:
		return "String"
	case TypeNode:
		return "Node"
	case TypeRelationship:
		return "Relationship"
	case TypePath:
		return "Path"
	case TypeList:
		return "List"
	case TypeMap:
		return "Map"
	default:
		return fmt.Sprintf("CypherType(%d)", uint8(t))
	}
}

// isNumeric reports whether t is a numeric type (Integer or Float).
func isNumeric(t CypherType) bool {
	return t == TypeInteger || t == TypeFloat
}

// TypeError is returned when a binary or unary operator is applied to
// operand type(s) that are incompatible with the operator under Cypher 9
// type rules.
type TypeError struct {
	// Op is the operator string (e.g. "+", "AND", "NOT").
	Op string
	// Left is the inferred type of the left operand. For unary operators it
	// holds the operand type and Right is TypeAny.
	Left CypherType
	// Right is the inferred type of the right operand. Zero (TypeAny) for
	// unary operators.
	Right CypherType
	// Pos is the source position of the operator or expression.
	Pos ast.Position
}

// Error implements the error interface.
func (e *TypeError) Error() string {
	if e.Right == TypeAny {
		return fmt.Sprintf(
			"type error at %s: operator %q is not defined for type %s",
			e.Pos, e.Op, e.Left,
		)
	}
	return fmt.Sprintf(
		"type error at %s: operator %q is not defined for types %s and %s",
		e.Pos, e.Op, e.Left, e.Right,
	)
}

// ─────────────────────────────────────────────────────────────────────────────
// Function registry
// ─────────────────────────────────────────────────────────────────────────────

// funcRegistry maps lower-cased function names to their return CypherType.
// Namespace-qualified names use the dotted form (e.g. "apoc.algo.dijkstra").
// When a function is absent from the registry, InferType returns TypeAny.
var funcRegistry = map[string]CypherType{
	// Aggregation (all keys lowercase — lookupFunc normalises the input)
	"count":          TypeInteger,
	"sum":            TypeFloat, // sum can return Integer or Float; Float is the safe upper bound
	"avg":            TypeFloat,
	"min":            TypeAny, // min/max depend on input type
	"max":            TypeAny,
	"collect":        TypeList,
	"stdev":          TypeFloat,
	"stdevp":         TypeFloat,
	"percentilecont": TypeFloat,
	"percentiledisc": TypeAny,

	// Scalar
	"id":            TypeInteger,
	"size":          TypeInteger,
	"length":        TypeInteger,
	"type":          TypeString,
	"startnode":     TypeNode,
	"endnode":       TypeNode,
	"head":          TypeAny,
	"last":          TypeAny,
	"tail":          TypeList,
	"range":         TypeList,
	"labels":        TypeList,
	"keys":          TypeList,
	"nodes":         TypeList,
	"relationships": TypeList,
	"coalesce":      TypeAny,
	"nullif":        TypeAny,

	// Math
	"abs":      TypeAny, // preserves input type Integer→Integer, Float→Float
	"ceil":     TypeFloat,
	"floor":    TypeFloat,
	"round":    TypeFloat,
	"sign":     TypeInteger,
	"sqrt":     TypeFloat,
	"log":      TypeFloat,
	"log10":    TypeFloat,
	"exp":      TypeFloat,
	"e":        TypeFloat,
	"pi":       TypeFloat,
	"rand":     TypeFloat,
	"sin":      TypeFloat,
	"cos":      TypeFloat,
	"tan":      TypeFloat,
	"asin":     TypeFloat,
	"acos":     TypeFloat,
	"atan":     TypeFloat,
	"atan2":    TypeFloat,
	"haversin": TypeFloat,
	"degrees":  TypeFloat,
	"radians":  TypeFloat,

	// Type-conversion (keys must be lowercase — lookupFunc lowercases the input)
	"tostring":  TypeString,
	"tointeger": TypeInteger,
	"tofloat":   TypeFloat,
	"toboolean": TypeBoolean,

	// String functions
	"tolower":   TypeString,
	"toupper":   TypeString,
	"trim":      TypeString,
	"ltrim":     TypeString,
	"rtrim":     TypeString,
	"left":      TypeString,
	"right":     TypeString,
	"substring": TypeString,
	"replace":   TypeString,
	"reverse":   TypeAny, // works on strings and lists
	"split":     TypeList,
	"str":       TypeString,

	// List functions
	"reduce":  TypeAny,
	"extract": TypeList,
	"filter":  TypeList,
	"any":     TypeBoolean,
	"all":     TypeBoolean,
	"none":    TypeBoolean,
	"single":  TypeBoolean,
	"isempty": TypeBoolean,

	// Predicate / existence
	"exists": TypeBoolean,
}

// lookupFunc returns the return type for a (possibly namespace-qualified)
// function name. The lookup is case-insensitive on the final name component.
func lookupFunc(ns []string, name string) CypherType {
	var key string
	if len(ns) > 0 {
		key = strings.ToLower(strings.Join(ns, ".") + "." + name)
	} else {
		key = strings.ToLower(name)
	}
	if t, ok := funcRegistry[key]; ok {
		return t
	}
	// Try bare name (no namespace) as fallback.
	if len(ns) > 0 {
		if t, ok := funcRegistry[strings.ToLower(name)]; ok {
			return t
		}
	}
	return TypeAny
}

// ─────────────────────────────────────────────────────────────────────────────
// InferType
// ─────────────────────────────────────────────────────────────────────────────

// InferType infers the static CypherType of expr according to openCypher 9
// type rules. It returns a non-nil error only when a type violation is
// detected (e.g. unsupported operand combination for an operator).
//
// For expressions whose type cannot be statically determined (Variable,
// Parameter, Property, CaseExpression, etc.) the function returns TypeAny and
// a nil error.
//
// InferType is a pure function; it does not mutate the AST.
//
// Concurrency: safe for concurrent use — no shared mutable state.
//
//nolint:gocyclo // One case per concrete AST type; complexity is structural, not reducible.
func InferType(expr ast.Expression) (CypherType, error) {
	if expr == nil {
		return TypeNull, nil
	}
	switch v := expr.(type) {
	// ── Literals ──────────────────────────────────────────────────────────────
	case *ast.NullLiteral:
		return TypeNull, nil
	case *ast.BoolLiteral:
		return TypeBoolean, nil
	case *ast.IntLiteral:
		return TypeInteger, nil
	case *ast.FloatLiteral:
		return TypeFloat, nil
	case *ast.StringLiteral:
		return TypeString, nil
	case *ast.ListLiteral:
		return TypeList, nil
	case *ast.MapLiteral:
		return TypeMap, nil

	// ── Variables and parameters ──────────────────────────────────────────────
	case *ast.Variable:
		// Variable types depend on scope context (which bindings carry a
		// graph type). Without full scope propagation the safe approximation
		// is TypeAny.
		return TypeAny, nil
	case *ast.Parameter:
		// Parameters are bound at runtime; their type is unknown statically.
		return TypeAny, nil

	// ── Property access ───────────────────────────────────────────────────────
	case *ast.Property:
		// Property values are typed at runtime by the graph model.
		return TypeAny, nil

	// ── Function invocation ───────────────────────────────────────────────────
	case *ast.FunctionInvocation:
		return lookupFunc(v.Namespace, v.Name), nil

	// ── Operators ─────────────────────────────────────────────────────────────
	case *ast.BinaryOp:
		return inferBinaryOp(v)
	case *ast.UnaryOp:
		return inferUnaryOp(v)

	// ── Subscript / slice ─────────────────────────────────────────────────────
	case *ast.SubscriptExpr:
		// xs[i] — element type is unknown without element-level inference.
		return TypeAny, nil
	case *ast.SliceExpr:
		// xs[a..b] — always a list (subset of the source list).
		return TypeList, nil

	// ── Comprehensions ────────────────────────────────────────────────────────
	case *ast.ListComprehension:
		return TypeList, nil
	case *ast.PatternComprehension:
		return TypeList, nil

	// ── Map projection ────────────────────────────────────────────────────────
	case *ast.MapProjection:
		return TypeMap, nil

	// ── Case expression ───────────────────────────────────────────────────────
	case *ast.CaseExpression:
		// The result type of a CASE is the unified type of all THEN (and ELSE)
		// branches. Without branch-level unification (which would require
		// recursive type unification) TypeAny is the conservative choice.
		return TypeAny, nil

	// ── Subqueries ────────────────────────────────────────────────────────────
	case *ast.ExistsSubquery:
		return TypeBoolean, nil
	case *ast.CountSubquery:
		return TypeInteger, nil

	// ── Path pattern in expression context ───────────────────────────────────
	case *ast.PathPattern:
		return TypePath, nil

	default:
		// Unknown expression node — conservative fallback.
		return TypeAny, nil
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// inferBinaryOp
// ─────────────────────────────────────────────────────────────────────────────

// inferBinaryOp implements the binary-operator type rules from openCypher 9
// §6. The function first infers the operand types (propagating any errors) and
// then applies the operator-specific rules.
//
// Short-circuit: if either operand is TypeAny the operator cannot be
// statically rejected; return TypeAny (the result type becomes unknown).
// This preserves the invariant that a TypeError is only returned when the
// types are concrete and the combination is provably illegal.
func inferBinaryOp(b *ast.BinaryOp) (CypherType, error) {
	lt, err := InferType(b.Left)
	if err != nil {
		return TypeAny, err
	}
	rt, err := InferType(b.Right)
	if err != nil {
		return TypeAny, err
	}
	return inferBinaryOpTypes(b.Operator, lt, rt, b.Pos)
}

// inferBinaryOpTypes contains the pure type-rule logic so it can be called
// from both inferBinaryOp and directly from tests.
//
//nolint:gocyclo // One case per oC9 operator group; complexity is structural, not reducible.
func inferBinaryOpTypes(op string, lt, rt CypherType, pos ast.Position) (CypherType, error) {
	// If either side is Any or Null, we cannot statically reject the
	// combination.  Return the widest safe result type.
	if lt == TypeAny || rt == TypeAny {
		return anyResult(op), nil
	}
	if lt == TypeNull || rt == TypeNull {
		// Null propagates: any operation with null yields null (or Boolean for
		// comparisons, per oC9 three-valued logic — but null is acceptable).
		return TypeNull, nil
	}

	switch op {
	// ── Arithmetic ───────────────────────────────────────────────────────────
	case "+":
		return inferPlus(lt, rt, pos)

	case "-", "*", "/", "%":
		// Number op Number → Number (Int op Int → Int; otherwise Float)
		if isNumeric(lt) && isNumeric(rt) {
			return numericResult(lt, rt), nil
		}
		return TypeAny, &TypeError{Op: op, Left: lt, Right: rt, Pos: pos}

	case "^":
		// Power: Number ^ Number → Float (Cypher 9 spec)
		if isNumeric(lt) && isNumeric(rt) {
			return TypeFloat, nil
		}
		return TypeAny, &TypeError{Op: op, Left: lt, Right: rt, Pos: pos}

	// ── Comparisons ──────────────────────────────────────────────────────────
	case "=", "<>", "<", ">", "<=", ">=":
		// Any comparable type pair → Boolean.
		// oC9 allows comparison between any same-type values (and also between
		// integers and floats); type errors at this level are deferred to
		// runtime.
		return TypeBoolean, nil

	// ── Logical ──────────────────────────────────────────────────────────────
	case "AND", "OR", "XOR":
		if lt == TypeBoolean && rt == TypeBoolean {
			return TypeBoolean, nil
		}
		return TypeAny, &TypeError{Op: op, Left: lt, Right: rt, Pos: pos}

	// ── Membership ───────────────────────────────────────────────────────────
	case "IN":
		// Any IN List → Boolean
		if rt == TypeList {
			return TypeBoolean, nil
		}
		return TypeAny, &TypeError{Op: op, Left: lt, Right: rt, Pos: pos}

	// ── String predicates ────────────────────────────────────────────────────
	case "STARTS WITH", "ENDS WITH", "CONTAINS":
		if lt == TypeString && rt == TypeString {
			return TypeBoolean, nil
		}
		return TypeAny, &TypeError{Op: op, Left: lt, Right: rt, Pos: pos}

	// ── Regex ─────────────────────────────────────────────────────────────────
	case "=~":
		if lt == TypeString && rt == TypeString {
			return TypeBoolean, nil
		}
		return TypeAny, &TypeError{Op: op, Left: lt, Right: rt, Pos: pos}

	default:
		// Unknown operator — cannot statically type-check.
		return TypeAny, nil
	}
}

// inferPlus implements the + operator rules from oC9 §6:
//
//	Integer + Integer → Integer
//	Float   + Float   → Float
//	Integer + Float   → Float  (coercion)
//	Float   + Integer → Float  (coercion)
//	String  + String  → String
//	List    + List    → List
//	String  + Number  → TypeError  (NOT allowed in oC9)
//	Number  + String  → TypeError
//	any other combination → TypeError
func inferPlus(lt, rt CypherType, pos ast.Position) (CypherType, error) {
	if isNumeric(lt) && isNumeric(rt) {
		return numericResult(lt, rt), nil
	}
	if lt == TypeString && rt == TypeString {
		return TypeString, nil
	}
	if lt == TypeList && rt == TypeList {
		return TypeList, nil
	}
	return TypeAny, &TypeError{Op: "+", Left: lt, Right: rt, Pos: pos}
}

// numericResult returns the result type of a numeric operation: if both
// operands are Integer the result is Integer; if either is Float the result
// is Float.
func numericResult(lt, rt CypherType) CypherType {
	if lt == TypeFloat || rt == TypeFloat {
		return TypeFloat
	}
	return TypeInteger
}

// anyResult returns the most specific result type that can be stated when one
// or both operands is TypeAny (i.e. type not statically known).
func anyResult(op string) CypherType {
	switch op {
	case "=", "<>", "<", ">", "<=", ">=",
		"AND", "OR", "XOR", "IN",
		"STARTS WITH", "ENDS WITH", "CONTAINS", "=~":
		return TypeBoolean
	case "^":
		return TypeFloat
	default:
		return TypeAny
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// inferUnaryOp
// ─────────────────────────────────────────────────────────────────────────────

// inferUnaryOp implements the unary-operator type rules from openCypher 9 §6:
//
//   - NOT Boolean → Boolean
//   - - (negation) Number → Number (Integer→Integer, Float→Float)
//   - IS NULL / IS NOT NULL → Boolean (any operand)
func inferUnaryOp(u *ast.UnaryOp) (CypherType, error) {
	ot, err := InferType(u.Operand)
	if err != nil {
		return TypeAny, err
	}

	switch u.Operator {
	case "NOT":
		if ot == TypeBoolean || ot == TypeAny {
			return TypeBoolean, nil
		}
		return TypeAny, &TypeError{Op: "NOT", Left: ot, Pos: u.Pos}

	case "-":
		if isNumeric(ot) || ot == TypeAny {
			if ot == TypeAny {
				return TypeAny, nil
			}
			return ot, nil // Integer→Integer, Float→Float
		}
		return TypeAny, &TypeError{Op: "-", Left: ot, Pos: u.Pos}

	case "IS NULL", "IS NOT NULL":
		// Defined for any type.
		return TypeBoolean, nil

	default:
		return TypeAny, nil
	}
}
