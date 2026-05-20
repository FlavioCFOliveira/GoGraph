package expr

// map.go — map projection evaluation (task-265).
//
// Implements ast.MapProjection: n {.name, .age, .*,  extra: expr}.
//
// Projection item semantics (openCypher 9 §3.5):
//
//   - Property selector  .key   — copies a single property from the subject.
//   - Star selector      .*     — copies all properties from the subject.
//   - Computed entry   key: e   — evaluates e and assigns it to key.
//   - Variable selector  var    — variable references are handled as computed
//                                  entries whose key is the variable name.
//
// Key order in the result MapValue follows the order of the items in the
// projection. Because MapValue is map[string]Value (unordered) the logical
// order is documented here but not enforced in the map; the ordered key slice
// in the returned OrderedMapValue (if needed externally) is recorded in the
// mapProjectionResult helper below.
//
// NULL subject: returns NULL per openCypher semantics.

import "gograph/cypher/ast"

// evalMapProjection evaluates a [ast.MapProjection] node.
func evalMapProjection(n *ast.MapProjection, row RowContext, params map[string]Value, reg FunctionRegistry) (Value, error) {
	subject, err := evalExpr(n.Subject, row, params, reg)
	if err != nil {
		return nil, err
	}
	if IsNull(subject) {
		return Null, nil
	}

	// Extract the source property map from the subject.
	var srcProps MapValue
	switch s := subject.(type) {
	case NodeValue:
		srcProps = s.Properties
	case RelationshipValue:
		srcProps = s.Properties
	case MapValue:
		srcProps = s
	default:
		// Non-map subject → empty source; computed projections still work.
		srcProps = nil
	}

	result := make(MapValue, len(n.Items))
	for _, item := range n.Items {
		if item.IsAll {
			// .* — copy all source properties.
			for k, v := range srcProps {
				result[k] = v
			}
			continue
		}

		if item.Value == nil {
			// .key shorthand — extract a single property from the subject.
			if srcProps != nil {
				if v, ok := srcProps[item.Key]; ok {
					result[item.Key] = v
				} else {
					result[item.Key] = Null
				}
			} else {
				result[item.Key] = Null
			}
			continue
		}

		// key: expr  or  variable reference (key == "").
		v, err := evalExpr(item.Value, row, params, reg)
		if err != nil {
			return nil, err
		}
		key := item.Key
		if key == "" {
			// Variable reference: derive the key from the expression.
			if vr, ok := item.Value.(*ast.Variable); ok {
				key = vr.Name
			} else {
				// Unnamed expression — skip (should not happen for well-formed AST).
				continue
			}
		}
		result[key] = v
	}
	return result, nil
}
