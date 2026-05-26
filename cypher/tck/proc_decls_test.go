package tck_test

// proc_decls_test.go — parser + impl builder for the TCK Gherkin step
// `there exists a procedure <signature>` followed by an optional table
// of rows. The parsed procedure is registered on the world's engine via
// procs.Registry.Register so that subsequent CALL <ns>.<name>(...) in
// the scenario's `When executing query:` step resolves correctly.

import (
	"context"
	"fmt"
	"strings"

	"github.com/cucumber/godog"

	"gograph/cypher/expr"
	"gograph/cypher/procs"
)

// parsedProc binds a [procs.Signature] to the column names of the
// declared input and output positions (preserved for filtering rows
// from the result table at CALL time).
type parsedProc struct {
	procs.Signature
	InputNames  []string
	OutputNames []string
}

// fqn returns the fully-qualified procedure name for diagnostics.
func (p *parsedProc) fqn() string {
	if len(p.Namespace) == 0 {
		return p.Name
	}
	return strings.Join(p.Namespace, ".") + "." + p.Name
}

// parseProcedureSignature parses a TCK procedure signature of the form
//
//	ns1.ns2.name(arg1 :: TYPE?, …) :: (out1 :: TYPE?, …)
//
// or its empty-arg / empty-result variants. The trailing `?` on each
// type is treated as a Cypher-NULL annotation and stripped.
func parseProcedureSignature(sig string) (parsedProc, error) {
	sig = strings.TrimSpace(sig)
	if sig == "" {
		return parsedProc{}, fmt.Errorf("empty signature")
	}
	// godog sometimes hands the step the line with a trailing colon.
	sig = strings.TrimSuffix(sig, ":")
	sig = strings.TrimSpace(sig)

	// The return-type separator `::` is at parenthesis depth 0; the
	// parameter type annotations inside the input list (e.g.
	// `name :: STRING?`) live at depth ≥ 1 and must be skipped.
	resultIdx := findTopLevelSeparator(sig)
	if resultIdx < 0 {
		return parsedProc{}, fmt.Errorf("missing top-level `::` between inputs and outputs in %q", sig)
	}
	head := strings.TrimSpace(sig[:resultIdx])
	tail := strings.TrimSpace(sig[resultIdx+2:])

	name, inputs, err := parseProcNameAndInputs(head)
	if err != nil {
		return parsedProc{}, err
	}
	outputs, err := parseProcOutputs(tail)
	if err != nil {
		return parsedProc{}, err
	}

	inputKinds := make([]expr.Kind, len(inputs))
	inputNames := make([]string, len(inputs))
	for i, p := range inputs {
		inputKinds[i] = mapTCKTypeToKind(p.Type)
		inputNames[i] = p.Name
	}
	outputCols := make([]procs.NamedType, len(outputs))
	outputNames := make([]string, len(outputs))
	for i, p := range outputs {
		outputCols[i] = procs.NamedType{Name: p.Name, Kind: mapTCKTypeToKind(p.Type)}
		outputNames[i] = p.Name
	}

	ns, leaf := splitNamespace(name)
	return parsedProc{
		Signature: procs.Signature{
			Namespace: ns,
			Name:      leaf,
			Inputs:    inputKinds,
			Outputs:   outputCols,
		},
		InputNames:  inputNames,
		OutputNames: outputNames,
	}, nil
}

// findTopLevelSeparator returns the index of the first `::` token in s
// that sits at parenthesis depth zero. Returns −1 when no such token
// exists. This avoids confusing a parameter type annotation like
// `name :: STRING?` (depth 1) with the return-type separator that
// follows the closing input paren (depth 0).
func findTopLevelSeparator(s string) int {
	depth := 0
	for i := 0; i+1 < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ':':
			if depth == 0 && s[i+1] == ':' {
				return i
			}
		}
	}
	return -1
}

// procParam is a single declared parameter or output.
type procParam struct {
	Name string
	Type string
}

// parseProcNameAndInputs splits `name(arg :: TYPE, …)` into the name
// and the input parameter list.
func parseProcNameAndInputs(s string) (name string, inputs []procParam, err error) {
	open := strings.IndexByte(s, '(')
	if open < 0 || !strings.HasSuffix(s, ")") {
		return "", nil, fmt.Errorf("invalid procedure head %q (expected name(args))", s)
	}
	name = strings.TrimSpace(s[:open])
	if name == "" {
		return "", nil, fmt.Errorf("empty procedure name in %q", s)
	}
	body := strings.TrimSpace(s[open+1 : len(s)-1])
	inputs, err = parseProcParamList(body)
	if err != nil {
		return "", nil, fmt.Errorf("inputs: %w", err)
	}
	return name, inputs, nil
}

// parseProcOutputs splits `(out :: TYPE, …)` into the output parameter
// list. Empty parentheses denote a void return.
func parseProcOutputs(s string) ([]procParam, error) {
	if s == "()" || s == "" {
		return nil, nil
	}
	if !strings.HasPrefix(s, "(") || !strings.HasSuffix(s, ")") {
		return nil, fmt.Errorf("invalid output spec %q (expected `()` or `(out :: TYPE, …)`)", s)
	}
	body := strings.TrimSpace(s[1 : len(s)-1])
	out, err := parseProcParamList(body)
	if err != nil {
		return nil, fmt.Errorf("outputs: %w", err)
	}
	return out, nil
}

// parseProcParamList parses `name1 :: TYPE1?, name2 :: TYPE2?, …`.
// An empty body yields a nil slice.
func parseProcParamList(body string) ([]procParam, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, nil
	}
	parts := strings.Split(body, ",")
	out := make([]procParam, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		typeIdx := strings.Index(p, "::")
		if typeIdx < 0 {
			return nil, fmt.Errorf("missing `::` in parameter %q", p)
		}
		pName := strings.TrimSpace(p[:typeIdx])
		pType := strings.TrimSpace(p[typeIdx+2:])
		pType = strings.TrimSuffix(pType, "?")
		pType = strings.TrimSpace(pType)
		if pName == "" || pType == "" {
			return nil, fmt.Errorf("empty parameter name or type in %q", p)
		}
		out = append(out, procParam{Name: pName, Type: pType})
	}
	return out, nil
}

// splitNamespace splits "a.b.c.name" into namespace ["a", "b", "c"] and
// leaf name "name". A name without dots yields an empty namespace.
func splitNamespace(qname string) (ns []string, name string) {
	parts := strings.Split(qname, ".")
	if len(parts) == 1 {
		return nil, parts[0]
	}
	return parts[:len(parts)-1], parts[len(parts)-1]
}

// mapTCKTypeToKind maps a TCK declared type name (uppercase) to an
// [expr.Kind]. Unknown types fall back to KindNull as a permissive
// placeholder.
func mapTCKTypeToKind(t string) expr.Kind {
	switch strings.ToUpper(t) {
	case "STRING":
		return expr.KindString
	case "INTEGER":
		return expr.KindInteger
	case "FLOAT":
		return expr.KindFloat
	case "BOOLEAN":
		return expr.KindBool
	case "NODE":
		return expr.KindNode
	case "RELATIONSHIP":
		return expr.KindRelationship
	case "LIST":
		return expr.KindList
	case "MAP":
		return expr.KindMap
	case "PATH":
		return expr.KindPath
	default:
		return expr.KindNull
	}
}

// buildProcImplFromTable returns a [procs.ProcImpl] backed by the rows
// declared in table. Rows are matched against the call args column-by-
// column on the InputNames prefix; matched rows yield the OutputNames
// suffix.
func buildProcImplFromTable(p *parsedProc, table *godog.Table) (procs.ProcImpl, error) {
	// Void procedures (no inputs, no outputs) — return empty result
	// unconditionally. The declaration's table may carry an empty
	// header row (`|`); we deliberately do not validate it.
	if len(p.InputNames) == 0 && len(p.OutputNames) == 0 {
		return func(_ context.Context, _ []expr.Value) ([][]expr.Value, error) {
			return nil, nil
		}, nil
	}

	rows, err := parseProcTable(p, table)
	if err != nil {
		return nil, err
	}

	inputCount := len(p.InputNames)
	outputCount := len(p.OutputNames)
	return func(_ context.Context, args []expr.Value) ([][]expr.Value, error) {
		var matches [][]expr.Value
		for _, row := range rows {
			if !rowMatchesArgs(row[:inputCount], args) {
				continue
			}
			out := make([]expr.Value, outputCount)
			copy(out, row[inputCount:inputCount+outputCount])
			matches = append(matches, out)
		}
		return matches, nil
	}, nil
}

// parseProcTable validates the table header against the declared input
// and output column names, then converts each data row's cells into
// [expr.Value] tokens using the column's expected kind.
func parseProcTable(p *parsedProc, table *godog.Table) ([][]expr.Value, error) {
	if table == nil || len(table.Rows) == 0 {
		return nil, nil
	}
	header := table.Rows[0]
	wantCols := append(append([]string{}, p.InputNames...), p.OutputNames...)
	if len(header.Cells) != len(wantCols) {
		return nil, fmt.Errorf("table has %d columns, signature declares %d", len(header.Cells), len(wantCols))
	}
	for i, cell := range header.Cells {
		got := strings.TrimSpace(cell.Value)
		if got != wantCols[i] {
			return nil, fmt.Errorf("table column %d = %q, signature expects %q", i, got, wantCols[i])
		}
	}

	kinds := make([]expr.Kind, len(wantCols))
	for i := range p.InputNames {
		kinds[i] = p.Inputs[i]
	}
	for i, c := range p.Outputs {
		kinds[len(p.InputNames)+i] = c.Kind
	}

	var rows [][]expr.Value
	for _, r := range table.Rows[1:] {
		if len(r.Cells) != len(wantCols) {
			return nil, fmt.Errorf("table row has %d cells, expected %d", len(r.Cells), len(wantCols))
		}
		row := make([]expr.Value, len(wantCols))
		for i, cell := range r.Cells {
			v, err := parseProcCell(strings.TrimSpace(cell.Value), kinds[i])
			if err != nil {
				return nil, fmt.Errorf("row %d col %d: %w", len(rows)+1, i, err)
			}
			row[i] = v
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// parseProcCell converts a single textual TCK cell into an [expr.Value]
// using the column's declared kind.
func parseProcCell(s string, kind expr.Kind) (expr.Value, error) {
	if s == "null" || s == "" {
		return expr.Null, nil
	}
	switch kind {
	case expr.KindString:
		if len(s) >= 2 && (s[0] == '\'' || s[0] == '"') && s[len(s)-1] == s[0] {
			return expr.StringValue(s[1 : len(s)-1]), nil
		}
		return expr.StringValue(s), nil
	case expr.KindInteger:
		var n int64
		if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
			return nil, fmt.Errorf("not an integer: %q", s)
		}
		return expr.IntegerValue(n), nil
	case expr.KindFloat:
		var f float64
		if _, err := fmt.Sscanf(s, "%f", &f); err != nil {
			return nil, fmt.Errorf("not a float: %q", s)
		}
		return expr.FloatValue(f), nil
	case expr.KindBool:
		switch strings.ToLower(s) {
		case "true":
			return expr.BoolValue(true), nil
		case "false":
			return expr.BoolValue(false), nil
		default:
			return nil, fmt.Errorf("not a boolean: %q", s)
		}
	default:
		return expr.StringValue(s), nil
	}
}

// rowMatchesArgs reports whether every key value in row equals the
// corresponding arg in args. Lengths are checked by the caller.
func rowMatchesArgs(row, args []expr.Value) bool {
	if len(row) != len(args) {
		return false
	}
	for i := range row {
		if !exprValuesEqual(row[i], args[i]) {
			return false
		}
	}
	return true
}

// exprValuesEqual compares two [expr.Value] for equality using the
// textual form when neither is NULL.
func exprValuesEqual(a, b expr.Value) bool {
	if a == nil || a == expr.Null {
		return b == nil || b == expr.Null
	}
	if b == nil || b == expr.Null {
		return false
	}
	return a.String() == b.String()
}
