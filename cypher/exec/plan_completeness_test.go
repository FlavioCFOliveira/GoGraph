package exec

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// TestPlanChildren_EveryOperatorWithInputsImplementsIt is the drift gate on the
// physical-plan surface.
//
// An operator's inputs live in UNEXPORTED fields, which reflection cannot read,
// so the plan tree is recovered through the explicit [PlanChildren] method. That
// makes forgetting the method a silent defect of exactly the kind this surface
// exists to remove: the rendered plan would simply STOP at the offending node,
// and every operator beneath it — the access path, the join, the tier actually
// engaged — would vanish from the diagnostic a user reaches for when GoGraph is
// slow (rmp #2222).
//
// The test therefore derives the obligation from the source itself rather than
// from a hand-maintained list, so a newly added operator is covered the moment it
// is written:
//
//  1. every struct in this package with a Next(out *Row) method is an operator;
//  2. a field can carry an input if its type is Operator, any interface in this
//     package that embeds Operator (ChunkProducer, NodeIDColumnProducer, …), or
//     any operator struct — through any number of pointers or slices;
//  3. an operator with such a field MUST implement PlanChildren.
func TestPlanChildren_EveryOperatorWithInputsImplementsIt(t *testing.T) {
	t.Parallel()

	// Parse the package's non-test files directly rather than through
	// parser.ParseDir, which is deprecated because it ignores build tags.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, name, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		t.Fatal("no non-test source files found in the exec package")
	}

	structs := map[string]*ast.StructType{}
	ifaces := map[string]*ast.InterfaceType{}
	methods := map[string]map[string]bool{} // receiver type -> method names

	for _, f := range files {
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					ts, isType := spec.(*ast.TypeSpec)
					if !isType {
						continue
					}
					switch t := ts.Type.(type) {
					case *ast.StructType:
						structs[ts.Name.Name] = t
					case *ast.InterfaceType:
						ifaces[ts.Name.Name] = t
					}
				}
			case *ast.FuncDecl:
				if d.Recv == nil || len(d.Recv.List) == 0 {
					continue
				}
				recv := baseTypeName(d.Recv.List[0].Type)
				if recv == "" {
					continue
				}
				if methods[recv] == nil {
					methods[recv] = map[string]bool{}
				}
				methods[recv][d.Name.Name] = true
			}
		}
	}

	// Resolve method sets through struct embedding, transitively. The columnar
	// operators depend on this: ColumnarFilter embeds Filter and gets both Next
	// and PlanChildren from it — correctly, because its scanChild is the SAME
	// object as the embedded Filter.child. Without promotion the gate would both
	// fail to recognise it as an operator and misreport the ones that are already
	// covered.
	effective := resolveEmbeddedMethods(structs, methods)

	// (1) Operators: structs with a Next method, own or promoted.
	operators := map[string]bool{}
	for name := range structs {
		if effective[name]["Next"] {
			operators[name] = true
		}
	}
	if len(operators) < 40 {
		t.Fatalf("found only %d operator structs; the detection heuristic has broken", len(operators))
	}

	// (2) Interfaces in this package that embed Operator, transitively.
	operatorIfaces := map[string]bool{"Operator": true}
	for changed := true; changed; {
		changed = false
		for name, it := range ifaces {
			if operatorIfaces[name] || it.Methods == nil {
				continue
			}
			for _, m := range it.Methods.List {
				if len(m.Names) != 0 {
					continue // a method, not an embedded interface
				}
				if operatorIfaces[baseTypeName(m.Type)] {
					operatorIfaces[name] = true
					changed = true
					break
				}
			}
		}
	}

	carriesInput := func(typeName string) bool {
		return operatorIfaces[typeName] || operators[typeName]
	}

	// (3) The obligation.
	var missing []string
	for name := range operators {
		if effective[name]["PlanChildren"] {
			continue
		}
		for _, field := range structs[name].Fields.List {
			base := baseTypeName(field.Type)
			if !carriesInput(base) {
				continue
			}
			for _, fn := range fieldNames(field) {
				missing = append(missing, name+"."+fn+" ("+base+")")
			}
			break
		}
	}
	sort.Strings(missing)

	if len(missing) > 0 {
		t.Fatalf("%d operator(s) hold an input but do not implement PlanChildren, so the "+
			"rendered physical plan would truncate at them:\n  %s\n\nAdd a PlanChildren method "+
			"returning the inputs in execution order (see cypher/exec/plan_children.go).",
			len(missing), strings.Join(missing, "\n  "))
	}
}

// resolveEmbeddedMethods returns each struct's method set including methods
// promoted from embedded structs, resolved transitively.
func resolveEmbeddedMethods(structs map[string]*ast.StructType, own map[string]map[string]bool) map[string]map[string]bool {
	embeds := map[string][]string{}
	for name, st := range structs {
		for _, f := range st.Fields.List {
			if len(f.Names) != 0 {
				continue // named field: no promotion
			}
			if base := baseTypeName(f.Type); base != "" && structs[base] != nil {
				embeds[name] = append(embeds[name], base)
			}
		}
	}

	out := map[string]map[string]bool{}
	for name := range structs {
		out[name] = map[string]bool{}
		for m := range own[name] {
			out[name][m] = true
		}
	}
	// Iterate to a fixed point so a chain of embeddings resolves fully.
	for changed := true; changed; {
		changed = false
		for name, parents := range embeds {
			for _, p := range parents {
				for m := range out[p] {
					if !out[name][m] {
						out[name][m] = true
						changed = true
					}
				}
			}
		}
	}
	return out
}

// baseTypeName strips pointers, slices, arrays and maps-of to the underlying
// named type, and returns "" for a type with no name (a func type, an inline
// struct, a qualified type from another package).
func baseTypeName(e ast.Expr) string {
	for {
		switch t := e.(type) {
		case *ast.StarExpr:
			e = t.X
		case *ast.ArrayType:
			e = t.Elt
		case *ast.Ident:
			return t.Name
		default:
			return ""
		}
	}
}

// fieldNames returns a field's declared names, or the embedded type's name when
// the field is embedded.
func fieldNames(f *ast.Field) []string {
	if len(f.Names) == 0 {
		return []string{baseTypeName(f.Type)}
	}
	out := make([]string, 0, len(f.Names))
	for _, n := range f.Names {
		out = append(out, n.Name)
	}
	return out
}
