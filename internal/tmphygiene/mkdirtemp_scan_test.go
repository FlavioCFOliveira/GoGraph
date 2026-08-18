package tmphygiene

// mkdirtemp_scan_test.go — the source scan that keeps ownedTempPrefixes honest
// (rmp #2527), plus its sensitivity proof.
//
// The scan is AST-based rather than textual. A regex over source would match the
// string literals in this very package's fixtures and miss a call split across
// lines; the parser sees calls as calls and literals as literals.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// mkdirTempSite is one os.MkdirTemp call whose base directory is the empty
// string, i.e. one that writes straight into the system temp area.
type mkdirTempSite struct {
	// where is "relative/path.go:line".
	where string
	// pattern is the literal second argument, or "" when it is not a literal
	// (which the guard cannot match and therefore rejects).
	pattern string
}

// mkdirTempSitesInSource returns the temp-rooted os.MkdirTemp calls in one Go
// source file. where is used only to label the results.
//
// Calls whose FIRST argument is not the empty string literal are skipped: a
// non-empty base means the directory is created inside somewhere the caller
// already owns (t.TempDir(), a working directory, an explicit path), which is
// the outcome this gate is trying to encourage. A non-literal base is likewise
// skipped, because it is overwhelmingly a caller-supplied path; the pattern
// argument is where the guard needs certainty, and a non-literal pattern IS
// reported.
func mkdirTempSitesInSource(where, src string) ([]mkdirTempSite, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, where, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", where, err)
	}

	var sites []mkdirTempSite
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 2 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "MkdirTemp" {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "os" {
			return true
		}
		base, isLiteral := stringLiteral(call.Args[0])
		if !isLiteral || base != "" {
			return true
		}
		pattern, _ := stringLiteral(call.Args[1]) // "" when not a literal
		sites = append(sites, mkdirTempSite{
			where:   fmt.Sprintf("%s:%d", where, fset.Position(call.Pos()).Line),
			pattern: pattern,
		})
		return true
	})
	return sites, nil
}

// stringLiteral unquotes an untyped string literal expression.
func stringLiteral(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return s, true
}

// tempRootedMkdirTempSites scans every Go file in the module for temp-rooted
// os.MkdirTemp calls. Build tags are deliberately ignored: a site that only
// compiles under -tags=soak strands directories just as effectively.
func tempRootedMkdirTempSites(root string) ([]mkdirTempSite, error) {
	var sites []mkdirTempSite
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch base := d.Name(); {
			case p == root:
				return nil
			case strings.HasPrefix(base, "."), base == "dist", base == "soak-artefacts", base == "testdata":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		b, readErr := os.ReadFile(p) //nolint:gosec // G304: p comes from walking this module's own tree
		if readErr != nil {
			return readErr
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			rel = p
		}
		found, parseErr := mkdirTempSitesInSource(filepath.ToSlash(rel), string(b))
		if parseErr != nil {
			return parseErr
		}
		sites = append(sites, found...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(sites, func(a, b mkdirTempSite) int { return strings.Compare(a.where, b.where) })
	return sites, nil
}

// moduleRoot walks up from the test's working directory to the directory holding
// go.mod, matching the helper used by internal/docscheck and internal/scriptgate.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate go.mod above %s", dir)
		}
		dir = parent
	}
}

// TestMkdirTempSitesInSource is the sensitivity proof for the scanner that
// TestOwnedTempPrefixes_CoverEveryTempRootedCallSite depends on. Each case is a
// way the scanner could go quietly blind and let an unlisted prefix through.
func TestMkdirTempSitesInSource(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		src  string
		want []mkdirTempSite
	}{
		{
			name: "temp-rooted literal pattern is FOUND",
			src:  "package p\nimport \"os\"\nfunc f() { _, _ = os.MkdirTemp(\"\", \"leaky-prefix-\") }\n",
			want: []mkdirTempSite{{where: "x.go:3", pattern: "leaky-prefix-"}},
		},
		{
			name: "a call split across lines is FOUND",
			src:  "package p\nimport \"os\"\nfunc f() {\n\t_, _ = os.MkdirTemp(\n\t\t\"\",\n\t\t\"split-prefix-\",\n\t)\n}\n",
			want: []mkdirTempSite{{where: "x.go:4", pattern: "split-prefix-"}},
		},
		{
			name: "a caller-owned base is NOT a site",
			src:  "package p\nimport \"os\"\nfunc f(d string) { _, _ = os.MkdirTemp(\".\", \"cwd-\"); _, _ = os.MkdirTemp(d, \"given-\") }\n",
			want: nil,
		},
		{
			name: "a NON-LITERAL pattern is found with an empty pattern, so the gate can reject it",
			src:  "package p\nimport \"os\"\nfunc f(p string) { _, _ = os.MkdirTemp(\"\", p) }\n",
			want: []mkdirTempSite{{where: "x.go:3", pattern: ""}},
		},
		{
			name: "a string LITERAL that merely spells the call is not a call",
			src:  "package p\nvar doc = `os.MkdirTemp(\"\", \"not-a-call-\")`\n",
			want: nil,
		},
		{
			name: "MkdirTemp on another package is not os.MkdirTemp",
			src:  "package p\nimport \"other\"\nfunc f() { _, _ = other.MkdirTemp(\"\", \"other-\") }\n",
			want: nil,
		},
		{
			name: "os.CreateTemp is a different call and out of scope here",
			src:  "package p\nimport \"os\"\nfunc f() { _, _ = os.CreateTemp(\"\", \"file-\") }\n",
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := mkdirTempSitesInSource("x.go", tc.src)
			if err != nil {
				t.Fatalf("mkdirTempSitesInSource: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("sites = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestCoverageGate_FiresOnAnUnlistedPrefix is the end-to-end sensitivity proof
// for TestOwnedTempPrefixes_CoverEveryTempRootedCallSite. The two tests around it
// certify the scanner and the matcher in isolation; this one composes them
// exactly as the gate does, and asserts the composition reaches a FAILING verdict
// for a prefix nobody listed. Without it the gate could be green because the
// halves never meet.
func TestCoverageGate_FiresOnAnUnlistedPrefix(t *testing.T) {
	t.Parallel()

	const src = "package p\nimport \"os\"\nfunc f() { _, _ = os.MkdirTemp(\"\", \"brand-new-unlisted-\") }\n"

	sites, err := mkdirTempSitesInSource("new_site.go", src)
	if err != nil {
		t.Fatalf("mkdirTempSitesInSource: %v", err)
	}
	if len(sites) != 1 {
		t.Fatalf("sites = %+v, want exactly one", sites)
	}
	if _, listed := matchOwnedPrefix(strings.TrimSuffix(sites[0].pattern, "*")); listed {
		t.Fatalf("prefix %q is matched by ownedTempPrefixes; the fixture no longer represents an "+
			"unlisted site, so this proof no longer shows the gate can fail", sites[0].pattern)
	}

	// The same composition applied to a prefix that IS listed must NOT fire, or
	// the gate would be failing unconditionally rather than discriminating.
	listedSites, err := mkdirTempSitesInSource("known_site.go",
		"package p\nimport \"os\"\nfunc f() { _, _ = os.MkdirTemp(\"\", \"gograph-crashinject-*\") }\n")
	if err != nil {
		t.Fatalf("mkdirTempSitesInSource (listed): %v", err)
	}
	if len(listedSites) != 1 {
		t.Fatalf("listedSites = %+v, want exactly one", listedSites)
	}
	if _, listed := matchOwnedPrefix(strings.TrimSuffix(listedSites[0].pattern, "*")); !listed {
		t.Errorf("prefix %q is NOT matched by ownedTempPrefixes; the gate would fail on the "+
			"module's own known site", listedSites[0].pattern)
	}
}

// TestMatchOwnedPrefix pins the matcher the coverage gate and the accumulation
// gate share, including the longest-match rule that keeps nested prefixes from
// being miscredited.
func TestMatchOwnedPrefix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		dir        string
		wantPrefix string
		wantOK     bool
	}{
		{"the #2527 leak", "gograph-crashinject-123456", "gograph-crashinject-", true},
		{"nested prefixes take the LONGEST match", "gograph-xrelease-image-99", "gograph-xrelease-image-", true},
		{"the shorter nested prefix still matches its own dirs", "gograph-xrelease-99", "gograph-xrelease-", true},
		{"a retired prefix is still owned", "sec-store-oom-7", "sec-store-oom-", true},
		{"a pattern without a trailing dash", "wal-example4242", "wal-example", true},
		{"go build work dirs are not ours", "go-build3141592", "", false},
		{"a t.TempDir leftover is not ours", "TestFoo_Bar123", "", false},
		{"an unrelated entry", "com.apple.something", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			prefix, ok := matchOwnedPrefix(tc.dir)
			if ok != tc.wantOK || prefix != tc.wantPrefix {
				t.Errorf("matchOwnedPrefix(%q) = (%q, %v), want (%q, %v)",
					tc.dir, prefix, ok, tc.wantPrefix, tc.wantOK)
			}
		})
	}
}
