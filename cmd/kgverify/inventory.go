package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// declKind is the symbol tier a declaration belongs to. The values are
// byte-equal to the knowledge graph's node labels, because the whole point of
// this inventory is to be comparable with them.
type declKind string

const (
	kindType      declKind = "Type"
	kindFunction  declKind = "Function"
	kindMethod    declKind = "Method"
	kindTest      declKind = "Test"
	kindBenchmark declKind = "Benchmark"
	kindFuzz      declKind = "FuzzTarget"
	kindExample   declKind = "Example"
)

// symbolLabels is the set of graph labels this tool holds to the tree. A node
// carrying one of these labels claims a Go declaration exists; that claim is
// checkable, which is why these labels and no others are audited for fidelity.
var symbolLabels = map[string]declKind{
	"Type":       kindType,
	"Function":   kindFunction,
	"Method":     kindMethod,
	"Test":       kindTest,
	"Benchmark":  kindBenchmark,
	"FuzzTarget": kindFuzz,
	"Example":    kindExample,
}

// decl is one top-level declaration read out of the tree.
type decl struct {
	Kind       declKind
	Name       string
	Recv       string
	File       string // repo-relative, slash-separated
	Dir        string // repo-relative directory, "." at the root
	PkgName    string // the package clause
	ImportPath string
}

// inventory is the authoritative answer to "what does this tree declare?".
//
// It is built by parsing every .go file with go/parser, never by scanning text.
// That distinction is the entire reason this type exists: a name that appears
// only in a comment, a task description, a report or a Markdown document is
// absent from this inventory, while a text scan would report it present. The
// two fabrications recorded in the regression fixture were created exactly that
// way, and edgeTypeFilterFor — deleted from the code but still named in four Go
// comments — is the worked example that a text scan cannot get right.
type inventory struct {
	decls   []decl
	byName  map[string][]int
	files   map[string]struct{}
	goFiles int
}

// buildInventory parses the whole tree rooted at repo.
//
// A .go file that fails to parse is a hard error, not a skip: an unparsed file
// silently shrinks the inventory, and a shrunken inventory manufactures
// fabrication reports for perfectly real symbols. Failing loudly is the only
// safe direction.
func buildInventory(repo, modulePath string) (*inventory, error) {
	inv := &inventory{
		byName: make(map[string][]int),
		files:  make(map[string]struct{}),
	}
	fset := token.NewFileSet()

	walkErr := filepath.WalkDir(repo, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(repo, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == "." {
				return nil
			}
			if skipDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		inv.files[rel] = struct{}{}
		if !strings.HasSuffix(rel, ".go") {
			return nil
		}
		inv.goFiles++
		return inv.parseGoFile(fset, path, rel, modulePath)
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return inv, nil
}

// skipDir names the directories that hold no module source. .git and .claude
// are not part of the tree under audit; .claude in particular holds transient
// agent worktrees, whole copies of the repo whose declarations would otherwise
// be counted twice.
func skipDir(name string) bool {
	switch name {
	case ".git", ".claude", "node_modules", "vendor":
		return true
	default:
		return false
	}
}

func (inv *inventory) parseGoFile(fset *token.FileSet, path, rel, modulePath string) error {
	src, err := os.ReadFile(path) //nolint:gosec // path comes from WalkDir over the audited repo, not from user input.
	if err != nil {
		return fmt.Errorf("read %s: %w", rel, err)
	}
	f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		return fmt.Errorf("parse %s: %w", rel, err)
	}
	dir := filepath.ToSlash(filepath.Dir(rel))
	importPath := modulePath
	if dir != "." {
		importPath = modulePath + "/" + dir
	}
	base := decl{
		File:       rel,
		Dir:        dir,
		PkgName:    f.Name.Name,
		ImportPath: importPath,
	}
	for _, d := range f.Decls {
		inv.addDecl(&base, d)
	}
	return nil
}

func (inv *inventory) addDecl(base *decl, d ast.Decl) {
	switch n := d.(type) {
	case *ast.FuncDecl:
		if n.Name == nil {
			return
		}
		e := *base
		e.Name = n.Name.Name
		if n.Recv != nil && len(n.Recv.List) > 0 {
			e.Kind = kindMethod
			e.Recv = receiverName(n.Recv.List[0].Type)
		} else {
			e.Kind = funcKind(n.Name.Name)
		}
		inv.add(&e)
	case *ast.GenDecl:
		if n.Tok != token.TYPE {
			return
		}
		for _, spec := range n.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || ts.Name == nil {
				continue
			}
			e := *base
			e.Kind = kindType
			e.Name = ts.Name.Name
			inv.add(&e)
		}
	}
}

func (inv *inventory) add(e *decl) {
	inv.byName[e.Name] = append(inv.byName[e.Name], len(inv.decls))
	inv.decls = append(inv.decls, *e)
}

// funcKind classifies a receiverless func exactly as knowledge-model.md
// describes the labels: by name prefix. This deliberately mirrors the model in
// force rather than Go's stricter go/doc test-name rule, so that a
// classification difference is never mistaken for a fidelity defect.
func funcKind(name string) declKind {
	switch {
	case strings.HasPrefix(name, "Test"):
		return kindTest
	case strings.HasPrefix(name, "Benchmark"):
		return kindBenchmark
	case strings.HasPrefix(name, "Fuzz"):
		return kindFuzz
	case strings.HasPrefix(name, "Example"):
		return kindExample
	default:
		return kindFunction
	}
}

// receiverName reduces a receiver type expression to the bare type name,
// unwrapping the pointer and any type parameters: (r *Foo[T]) yields "Foo".
func receiverName(e ast.Expr) string {
	for {
		switch t := e.(type) {
		case *ast.StarExpr:
			e = t.X
		case *ast.IndexExpr:
			e = t.X
		case *ast.IndexListExpr:
			e = t.X
		case *ast.ParenExpr:
			e = t.X
		case *ast.Ident:
			return t.Name
		case *ast.SelectorExpr:
			if t.Sel != nil {
				return t.Sel.Name
			}
			return ""
		default:
			return ""
		}
	}
}

// declared reports whether the tree declares name at all, under any symbol
// kind, in any package. This is the fabrication oracle, and it is deliberately
// the weakest form of the question: a node surviving it may still sit in the
// wrong package or carry the wrong kind, but a node failing it names something
// that does not exist.
func (inv *inventory) declared(name string) bool {
	_, ok := inv.byName[name]
	return ok
}

func (inv *inventory) declaredAs(name string, kind declKind) bool {
	for _, i := range inv.byName[name] {
		if inv.decls[i].Kind == kind {
			return true
		}
	}
	return false
}

// declaredIn reports whether name is declared with kind in the package the node
// claims. The graph keys packages inconsistently — knowledge-model.md records
// that internal/sim symbols carry `package` holding the repo-relative directory
// where every other package carries `pkg` holding the import path — so all three
// spellings are accepted.
func (inv *inventory) declaredIn(name string, kind declKind, pkg string) bool {
	if pkg == "" {
		return true
	}
	for _, i := range inv.byName[name] {
		d := inv.decls[i]
		if d.Kind != kind {
			continue
		}
		if pkg == d.ImportPath || pkg == d.Dir || pkg == d.PkgName {
			return true
		}
	}
	return false
}

func (inv *inventory) hasFile(path string) bool {
	_, ok := inv.files[path]
	return ok
}

// declsInFiles returns every declaration read out of the named files.
func (inv *inventory) declsInFiles(files map[string]struct{}) []decl {
	var out []decl
	for _, d := range inv.decls {
		if _, ok := files[d.File]; ok {
			out = append(out, d)
		}
	}
	return out
}

// hasDirPrefix reports whether path names a directory in the tree. Component
// nodes locate themselves by either a file or a directory, so both must count
// as present.
func (inv *inventory) hasDirPrefix(path string) bool {
	p := strings.TrimSuffix(path, "/")
	if p == "" || p == "." {
		return true
	}
	prefix := p + "/"
	for f := range inv.files {
		if strings.HasPrefix(f, prefix) {
			return true
		}
	}
	return false
}
