package sim

// global_state_guard_test.go — a structural tripwire against the one defect
// shape that reddened `make ci` under rmp #2597: a package-level `var` declared
// in a NON-TEST file of this package, which a `_test.go` file writes to.
//
// That shape is a data race by construction, and it is invisible to every
// targeted test run. The package's tests call t.Parallel(), and the whole-tree
// gate (`go test -race -count=1 -timeout=30m ./...`) runs the package under the
// load of every other package, so the window in which one test's write overlaps
// another test's read through the same variable is wide there and narrow in
// isolation. The original offender — ctxCancelPrecedenceOverride in
// search_ctx_cancel.go, saved/overwritten/restored by the precedence
// falsifiability helper while TestSearchCtxCancel_RunsCleanAndIsDeterministic
// read it — passed its own task's validation twice and then failed sixteen
// tests at once on the gate, because Go's testing package marks every parallel
// test in flight when the detector fires.
//
// CLAUDE.md forbids the shape outright ("No hidden global state. Package-level
// mutable variables are forbidden outside of carefully reviewed registries"), so
// the guard asserts the rule rather than the single instance: any reintroduction
// anywhere in this package fails here, at the moment it is written, instead of
// probabilistically on someone else's gate run.
//
// The fix for such a finding is never a mutex, a sync.Once, or dropping
// t.Parallel() — each of those keeps the shared variable and leaves the trap for
// the next reader. Pass the value in as a parameter, as
// [ctxCancelPrecedenceViolations] now does.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// simGlobalMutation is one write, from a test file, to a package-level variable
// declared in a non-test file.
type simGlobalMutation struct {
	Name    string // the variable's name
	Kind    string // how it is written: assign, elem/field-assign, incdec, addr-of, delete, clear
	At      string // file:line:col of the write
	DeclAt  string // file:line:col of the declaration
	Snippet string // the offending line, trimmed
}

func (m simGlobalMutation) String() string {
	return fmt.Sprintf("%s: %s of %s (declared at %s)\n\t%s", m.At, m.Kind, m.Name, m.DeclAt, m.Snippet)
}

// simScanTestMutatedGlobals reports every write, from a `_test.go` file in dir,
// to a package-level variable declared in a non-test file of dir.
//
// Build constraints are deliberately NOT applied: every `.go` file in the
// directory is parsed, so a `//go:build soak`- or `//go:build nightly`-gated test
// file is covered by the default short run. That is the conservative direction —
// a racy write behind a tag is still a racy write, and the layer that would have
// caught it is the layer that runs least often. It follows the same convention
// as ctxParseFamily in search_ctx_cancel_test.go, and for the same reason:
// parser.ParseDir is deprecated in favour of golang.org/x/tools/go/packages,
// which this module does not depend on and which WOULD apply build constraints.
//
// The analysis is syntactic, and its one deliberate conservatism is documented
// here rather than left to be discovered. Type information is not available
// without a package loader, so an identifier is taken to be the package-level
// variable unless the enclosing function binds that same name somewhere (a
// parameter, a `:=`, a `var`, a range variable, a func-literal parameter). A
// function that both shadows name X locally and writes package-level X elsewhere
// in its own body would therefore be missed. No such function exists, and the
// shape this guards against does not look like that: the deleted offender read
// `saved := ctxCancelPrecedenceOverride` and then assigned to it, which binds
// `saved`, not the global, and is caught.
//
// Reads are not reported. `&v[i]` inside a `for i := range v` loop — the
// standard way to iterate a table of structs without copying each element — is a
// read, and this package uses it in three places. A bare `&v` on the whole
// variable IS reported: it hands a caller the address of the global, which is a
// write channel however it is spelled at the call site.
func simScanTestMutatedGlobals(dir string) ([]simGlobalMutation, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var prod, tests []string
	for _, ent := range ents {
		name := ent.Name()
		if ent.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		if strings.HasSuffix(name, "_test.go") {
			tests = append(tests, name)
		} else {
			prod = append(prod, name)
		}
	}
	sort.Strings(prod)
	sort.Strings(tests)

	fset := token.NewFileSet()
	parse := func(name string) (*ast.File, []string, error) {
		path := filepath.Join(dir, name)
		src, err := os.ReadFile(path) //nolint:gosec // a fixed path inside the package directory under test
		if err != nil {
			return nil, nil, fmt.Errorf("read %s: %w", path, err)
		}
		f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
		if err != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", path, err)
		}
		return f, strings.Split(string(src), "\n"), nil
	}

	// Pass one: every package-level var name declared in a non-test file.
	globals := map[string]string{}
	for _, name := range prod {
		f, _, err := parse(name)
		if err != nil {
			return nil, err
		}
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, id := range vs.Names {
					if id.Name != "_" {
						globals[id.Name] = fset.Position(id.Pos()).String()
					}
				}
			}
		}
	}

	// Pass two: every write to one of them from a test file.
	var out []simGlobalMutation
	for _, name := range tests {
		f, lines, err := parse(name)
		if err != nil {
			return nil, err
		}
		record := func(kind string, id *ast.Ident, locals map[string]bool) {
			if id == nil || locals[id.Name] {
				return
			}
			decl, ok := globals[id.Name]
			if !ok {
				return
			}
			pos := fset.Position(id.Pos())
			snippet := ""
			if pos.Line-1 >= 0 && pos.Line-1 < len(lines) {
				snippet = strings.TrimSpace(lines[pos.Line-1])
			}
			out = append(out, simGlobalMutation{
				Name: id.Name, Kind: kind, At: pos.String(), DeclAt: decl, Snippet: snippet,
			})
		}
		for _, d := range f.Decls {
			locals := map[string]bool{}
			if fd, ok := d.(*ast.FuncDecl); ok {
				locals = simBoundNames(fd)
			}
			simFindWrites(d, locals, record)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At < out[j].At })
	return out, nil
}

// simRootIdent returns the identifier an assignable expression is rooted at:
// `v`, `v[i]`, `v.f`, `*v` and `v[a:b]` all root at `v`. It returns nil for
// anything else, which is the safe direction — an expression whose root is not a
// plain identifier cannot be naming a package-level variable directly.
func simRootIdent(e ast.Expr) *ast.Ident {
	for {
		switch x := e.(type) {
		case *ast.Ident:
			return x
		case *ast.IndexExpr:
			e = x.X
		case *ast.SelectorExpr:
			e = x.X
		case *ast.StarExpr:
			e = x.X
		case *ast.SliceExpr:
			e = x.X
		case *ast.ParenExpr:
			e = x.X
		default:
			return nil
		}
	}
}

// simBoundNames returns every name the function binds anywhere in its own body,
// including inside nested function literals: receiver, parameters, results,
// `:=` definitions, `var`/`const` declarations, and range variables. An
// identifier written inside the function whose name is in this set is assumed to
// be that local, not a package-level variable.
func simBoundNames(fd *ast.FuncDecl) map[string]bool {
	bound := map[string]bool{}
	addFields := func(fl *ast.FieldList) {
		if fl == nil {
			return
		}
		for _, fld := range fl.List {
			for _, id := range fld.Names {
				bound[id.Name] = true
			}
		}
	}
	addFields(fd.Recv)
	if fd.Type != nil {
		addFields(fd.Type.Params)
		addFields(fd.Type.Results)
	}
	ast.Inspect(fd, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.FuncLit:
			if s.Type != nil {
				addFields(s.Type.Params)
				addFields(s.Type.Results)
			}
		case *ast.AssignStmt:
			if s.Tok != token.DEFINE {
				return true
			}
			for _, lhs := range s.Lhs {
				if id, ok := lhs.(*ast.Ident); ok {
					bound[id.Name] = true
				}
			}
		case *ast.RangeStmt:
			if s.Tok != token.DEFINE {
				return true
			}
			for _, e := range []ast.Expr{s.Key, s.Value} {
				if id, ok := e.(*ast.Ident); ok {
					bound[id.Name] = true
				}
			}
		case *ast.GenDecl:
			if s.Tok != token.VAR && s.Tok != token.CONST {
				return true
			}
			for _, spec := range s.Specs {
				if vs, ok := spec.(*ast.ValueSpec); ok {
					for _, id := range vs.Names {
						bound[id.Name] = true
					}
				}
			}
		}
		return true
	})
	return bound
}

// simFindWrites walks node and calls record for every construct that writes
// through an identifier: assignment, element or field assignment, ++/--,
// address-of the whole variable, and the delete and clear builtins.
func simFindWrites(node ast.Node, locals map[string]bool, record func(kind string, id *ast.Ident, locals map[string]bool)) {
	ast.Inspect(node, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			if s.Tok == token.DEFINE {
				return true
			}
			for _, lhs := range s.Lhs {
				kind := "elem/field-assign"
				if _, plain := lhs.(*ast.Ident); plain {
					kind = "assign"
				}
				record(kind, simRootIdent(lhs), locals)
			}
		case *ast.IncDecStmt:
			record("incdec", simRootIdent(s.X), locals)
		case *ast.UnaryExpr:
			// Only a bare &v: &v[i] and &v.f are how this package iterates its
			// immutable tables without copying, and are reads.
			if s.Op != token.AND {
				return true
			}
			if id, ok := s.X.(*ast.Ident); ok {
				record("addr-of", id, locals)
			}
		case *ast.CallExpr:
			id, ok := s.Fun.(*ast.Ident)
			if !ok || len(s.Args) == 0 || (id.Name != "delete" && id.Name != "clear") {
				return true
			}
			record(id.Name, simRootIdent(s.Args[0]), locals)
		}
		return true
	})
}

// TestSim_NoPackageLevelVarMutatedByTests is the regression guard for rmp #2597.
//
// It fails if any `_test.go` file in this package writes to a package-level
// variable declared in a non-test file. On the pre-fix tree it reports the two
// writes to ctxCancelPrecedenceOverride in search_ctx_cancel_test.go and fails;
// on the fixed tree the package has no such variable and it passes.
//
// [TestSim_GlobalMutationScannerCanFire] proves the scanner can still fire now
// that the real offender is gone, so a pass here means "no offender", never
// "the scanner stopped looking".
func TestSim_NoPackageLevelVarMutatedByTests(t *testing.T) {
	t.Parallel()
	// The test binary's working directory is the package's source directory.
	hits, err := simScanTestMutatedGlobals(".")
	if err != nil {
		t.Fatalf("scan the package for test-mutated globals: %v", err)
	}
	if len(hits) == 0 {
		return
	}
	var b strings.Builder
	for _, h := range hits {
		fmt.Fprintf(&b, "  %s\n", h)
	}
	t.Fatalf("%d package-level variable(s) declared in a non-test file are written by a test file.\n"+
		"Each one is a data race against any parallel test that reads it through the same code, and is forbidden by\n"+
		"CLAUDE.md's no-hidden-global-state rule. Pass the value in as a parameter — do NOT add a mutex, a sync.Once,\n"+
		"or drop t.Parallel(), and do NOT move the variable into a _test.go file: all four keep the shared state.\n%s",
		len(hits), b.String())
}

// TestSim_GlobalMutationScannerCanFire is the non-vacuity gate for
// [TestSim_NoPackageLevelVarMutatedByTests].
//
// The scanner's subject was deleted by the fix it guards, so from now on it runs
// against a clean tree for ever and would report zero whether it worked or not.
// These cases reconstruct the deleted shape in a temporary directory and require
// it to be caught, and reconstruct the near-misses that must NOT be caught: a
// test that only reads the global, and the `&table[i]` range idiom this package
// uses in three real places.
func TestSim_GlobalMutationScannerCanFire(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		prod     string
		test     string
		wantKind string // "" means: expect no finding
	}{
		{
			// The deleted offender, reduced: declared in a non-test file, and
			// saved/overwritten/restored by a test helper.
			name: "override var saved and restored by a test",
			prod: "package p\n\ntype row struct{ n int }\n\nvar tableOverride []row\n",
			test: "package p\n\nfunc render(r row) int {\n\tsaved := tableOverride\n\ttableOverride = []row{r}\n\tdefer func() { tableOverride = saved }()\n\treturn len(tableOverride)\n}\n",
			// Three writes; the first is what the assertion below pins.
			wantKind: "assign",
		},
		{
			name:     "element write into a package-level table",
			prod:     "package p\n\nvar table = []int{1, 2, 3}\n",
			test:     "package p\n\nfunc bump() { table[0] = 9 }\n",
			wantKind: "elem/field-assign",
		},
		{
			name:     "map write into a package-level map",
			prod:     "package p\n\nvar counts = map[string]int{}\n",
			test:     "package p\n\nfunc bump() { counts[\"a\"]++ }\n",
			wantKind: "incdec",
		},
		{
			name:     "delete from a package-level map",
			prod:     "package p\n\nvar counts = map[string]int{}\n",
			test:     "package p\n\nfunc drop() { delete(counts, \"a\") }\n",
			wantKind: "delete",
		},
		{
			name:     "address of the whole package-level var",
			prod:     "package p\n\nvar table = []int{1, 2, 3}\n",
			test:     "package p\n\nfunc leak() *[]int { return &table }\n",
			wantKind: "addr-of",
		},
		{
			name: "reading the global is not a write",
			prod: "package p\n\nvar table = []int{1, 2, 3}\n",
			test: "package p\n\nfunc read() int {\n\tn := 0\n\tfor _, v := range table {\n\t\tn += v\n\t}\n\treturn n\n}\n",
		},
		{
			// The real idiom in bolt_version_matrix_test.go and
			// bolt_begin_extras_test.go: &table[i] to avoid copying each element.
			name: "&table[i] in a range loop is not a write",
			prod: "package p\n\ntype row struct{ n int }\n\nvar table = []row{{1}, {2}}\n",
			test: "package p\n\nfunc read() int {\n\tn := 0\n\tfor i := range table {\n\t\tr := &table[i]\n\t\tn += r.n\n\t}\n\treturn n\n}\n",
		},
		{
			// A local that shadows the global's name must not be reported.
			name: "a local of the same name is not the global",
			prod: "package p\n\nvar table = []int{1, 2, 3}\n",
			test: "package p\n\nfunc shadow() int {\n\ttable := []int{0}\n\ttable[0] = 7\n\treturn table[0]\n}\n",
		},
		{
			// Writing a var that a _test.go file itself declares is not this
			// defect: it is not reachable from the shipped code path at all.
			name: "a package-level var declared in the test file is out of scope",
			prod: "package p\n\nfunc unrelated() {}\n",
			test: "package p\n\nvar testOnly []int\n\nfunc set() { testOnly = []int{1} }\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "p.go"), []byte(tc.prod), 0o600); err != nil {
				t.Fatalf("write p.go: %v", err)
			}
			if err := os.WriteFile(filepath.Join(dir, "p_test.go"), []byte(tc.test), 0o600); err != nil {
				t.Fatalf("write p_test.go: %v", err)
			}
			hits, err := simScanTestMutatedGlobals(dir)
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			if tc.wantKind == "" {
				if len(hits) != 0 {
					t.Fatalf("the scanner reported %d write(s) on a case that writes nothing: %v", len(hits), hits)
				}
				return
			}
			if len(hits) == 0 {
				t.Fatal("the scanner reported nothing on a case that writes a package-level global; it cannot fire, " +
					"so TestSim_NoPackageLevelVarMutatedByTests passing proves nothing")
			}
			if hits[0].Kind != tc.wantKind {
				t.Errorf("first write reported as kind %q, want %q (all: %v)", hits[0].Kind, tc.wantKind, hits)
			}
		})
	}
}
