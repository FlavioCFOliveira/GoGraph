// Package concurrencydoc holds the CI doc-scan gate that enforces the
// CLAUDE.md mandate: "Every exported type carries a godoc clause stating
// whether it is safe for concurrent use; ambiguity is a defect."
//
// The package is test-only support: it exposes a single scanner that
// enumerates exported types across the public source trees and classifies
// each as documented-for-concurrency or not. The gate test in this package
// (concurrencydoc_test.go) asserts that the count of undocumented exported
// types never rises above a baseline that ratchets down as types are
// documented over time.
//
// # Concurrency
//
// The scanner is stateless apart from the *ScanResult it returns; a
// ScanResult is constructed once by Scan and then read-only, so it is safe
// for concurrent reads once Scan has returned. Scan itself is meant to be
// driven from a single test goroutine.
package concurrencydoc

import (
	"fmt"
	"go/ast"
	"go/doc"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// excludedTopLevel lists repository top-level directories whose packages are
// NOT part of the public library API, so they are excluded from the
// concurrency-doc scan. Everything else under the repository root is treated
// as public and IS scanned — deriving the universe this way (rather than the
// old hardcoded six-tree list) means a NEW top-level public package, e.g. the
// `metrics` package the old list missed, is covered automatically. The
// invariant "scanned packages == public packages from `go list`" is verified
// by TestScanUniverseMatchesGoList.
var excludedTopLevel = map[string]struct{}{
	"internal": {}, // test/CI support, not public API
	"examples": {}, // runnable examples, not library API
	"cmd":      {}, // command binaries, not library API
	"bench":    {}, // benchmark harnesses, not library API
	"dist":     {}, // build artefacts
	"bin":      {}, // build artefacts
}

// skipDirComponents lists path components that mark a directory whose
// packages are excluded from the scan: generated parser/lexer sources
// (cypher/parser/gen) carry no hand-written concurrency contracts, and
// testdata holds fixtures rather than library code.
var skipDirComponents = map[string]struct{}{
	"gen":      {},
	"testdata": {},
}

// concurrencyMarkers is the set of case-insensitive substrings that, when
// found in a type's own doc comment OR in its package-level doc comment,
// count the type as having a concurrency contract. The set is intentionally
// broad: it matches both the positive forms ("safe for concurrent use",
// "goroutine-safe") and the negative forms ("not safe for concurrent",
// "single-writer", "owned by one goroutine"), because a documented type is
// one whose contract is STATED, whether that contract is "safe" or "unsafe".
//
// "concurren" is the broadest marker: it matches "concurrent", "concurrency",
// and "Concurrent" alike, so any sentence discussing concurrency qualifies.
var concurrencyMarkers = []string{
	"concurren",
	"goroutine-safe",
	"thread-safe",
	"not safe for concurrent",
	"safe for concurrent",
	"single-writer",
	"immutable, so",
	"immutable after construction",
	"must not be shared",
	"owned by one goroutine",
}

// TypeInfo records one exported type discovered by the scan.
type TypeInfo struct {
	// Pkg is the import-path-style package identifier relative to the
	// repository root (e.g. "cypher/expr").
	Pkg string
	// Name is the exported type name (e.g. "NodeValue").
	Name string
	// Documented reports whether the type's own doc comment or its
	// package doc comment carries a concurrency marker.
	Documented bool
}

// Qualified returns the "pkg.Name" identifier used in the allowlist and in
// failure messages (e.g. "cypher/expr.NodeValue").
func (t TypeInfo) Qualified() string {
	return t.Pkg + "." + t.Name
}

// ScanResult is the immutable outcome of a Scan over the public trees.
//
// # Concurrency
//
// A ScanResult is populated once by Scan and then treated as read-only, so
// it is safe for concurrent reads without external locking.
type ScanResult struct {
	// Types holds every exported type discovered, sorted by Qualified().
	Types []TypeInfo
	// Packages holds the repository-relative import path (slash-separated,
	// e.g. "graph/lpg", "metrics", or "." for the module root) of every
	// scanned package — that is, every non-excluded directory that contains
	// at least one non-test .go file. It records the scan's UNIVERSE
	// independently of whether a package declared exported types, so the gate
	// can prove its universe matches the public-package set. Sorted.
	Packages []string
	// SkippedDirs records directories that failed to parse and were
	// skipped (with the reason), so the scan never panics on an
	// unparseable tree and the test can surface the skip.
	SkippedDirs []string
}

// Documented returns the subset of Types whose concurrency contract is
// stated.
func (r *ScanResult) Documented() []TypeInfo {
	out := make([]TypeInfo, 0, len(r.Types))
	for _, t := range r.Types {
		if t.Documented {
			out = append(out, t)
		}
	}
	return out
}

// Undocumented returns the subset of Types whose concurrency contract is NOT
// stated, in sorted order.
func (r *ScanResult) Undocumented() []TypeInfo {
	out := make([]TypeInfo, 0, len(r.Types))
	for _, t := range r.Types {
		if !t.Documented {
			out = append(out, t)
		}
	}
	return out
}

// Lookup returns the TypeInfo for the given qualified name ("pkg.Name") and
// whether it was found.
func (r *ScanResult) Lookup(qualified string) (TypeInfo, bool) {
	for _, t := range r.Types {
		if t.Qualified() == qualified {
			return t, true
		}
	}
	return TypeInfo{}, false
}

// RepoRoot walks up from start until it finds a directory containing a
// go.mod file, and returns that directory. Under `go test` the working
// directory is the test's own package directory, so this lets the scan
// locate the repository root without hard-coded paths.
func RepoRoot(start string) (string, error) {
	dir := start
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("concurrencydoc: no go.mod found walking up from %q", start)
		}
		dir = parent
	}
}

// Scan enumerates every exported type in every non-generated, non-test
// public package rooted at repoRoot, classifying each as
// documented-for-concurrency or not. The universe is the whole repository
// minus the excludedTopLevel directories (internal/examples/cmd/bench/…) and
// the skipDirComponents (gen/testdata), so any public package — present or
// future — is covered, not just the historical six trees.
//
// The scan never panics: a directory whose Go files fail to parse is skipped
// and recorded in ScanResult.SkippedDirs rather than aborting the whole scan.
func Scan(repoRoot string) (*ScanResult, error) {
	res := &ScanResult{}
	seen := make(map[string]struct{}) // qualified name -> dedup guard

	walkErr := filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// Record and continue; do not abort the whole walk.
			res.SkippedDirs = append(res.SkippedDirs, fmt.Sprintf("%s: walk error: %v", path, err))
			return nil //nolint:nilerr // a per-entry walk error must not abort the scan
		}
		if !d.IsDir() {
			return nil
		}
		base := d.Name()
		// At the repository root, prune the non-public top-level directories.
		if filepath.Dir(path) == repoRoot {
			if _, excluded := excludedTopLevel[base]; excluded {
				return filepath.SkipDir
			}
		}
		if _, skip := skipDirComponents[base]; skip {
			return filepath.SkipDir
		}
		if strings.HasPrefix(base, ".") && path != repoRoot {
			return filepath.SkipDir
		}
		scanPackageDir(repoRoot, path, seen, res)
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("concurrencydoc: walking %q: %w", repoRoot, walkErr)
	}

	sort.Slice(res.Types, func(i, j int) bool {
		return res.Types[i].Qualified() < res.Types[j].Qualified()
	})
	sort.Strings(res.Packages)
	sort.Strings(res.SkippedDirs)
	return res, nil
}

// scanPackageDir parses the single directory dir (non-recursively) and
// appends its exported types to res. Parse failures are recorded as skips.
func scanPackageDir(repoRoot, dir string, seen map[string]struct{}, res *ScanResult) {
	rel := filepath.ToSlash(relOrDir(repoRoot, dir))

	entries, err := os.ReadDir(dir)
	if err != nil {
		res.SkippedDirs = append(res.SkippedDirs, fmt.Sprintf("%s: read dir error: %v", rel, err))
		return
	}

	// Parse only non-test .go files. Generated parser/lexer dirs are
	// excluded at the walk level (skipDirComponents), so anything reaching
	// here is hand-written. A malformed file is recorded as a skip and
	// does not abort the scan of the rest of the directory.
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ParseComments)
		if perr != nil {
			res.SkippedDirs = append(res.SkippedDirs, fmt.Sprintf("%s/%s: parse error: %v", rel, name, perr))
			continue
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		return
	}

	// All non-test .go files in a directory belong to the same package, so
	// doc.NewFromFiles builds one *doc.Package. It is the non-deprecated
	// replacement for parser.ParseDir + doc.New (which relied on the
	// deprecated *ast.Package).
	docPkg, derr := doc.NewFromFiles(fset, files, rel, doc.AllDecls)
	if derr != nil {
		res.SkippedDirs = append(res.SkippedDirs, fmt.Sprintf("%s: doc build error: %v", rel, derr))
		return
	}
	if strings.HasSuffix(docPkg.Name, "_test") {
		return
	}

	// Record the package in the scanned universe (independently of whether it
	// declares exported types) so the gate can prove its universe matches the
	// public-package set from `go list`.
	res.Packages = append(res.Packages, rel)

	pkgDocHasMarker := hasConcurrencyMarker(docPkg.Doc)
	for _, t := range docPkg.Types {
		if !ast.IsExported(t.Name) {
			continue
		}
		qualified := rel + "." + t.Name
		if _, dup := seen[qualified]; dup {
			continue
		}
		seen[qualified] = struct{}{}
		documented := pkgDocHasMarker || hasConcurrencyMarker(t.Doc)
		res.Types = append(res.Types, TypeInfo{
			Pkg:        rel,
			Name:       t.Name,
			Documented: documented,
		})
	}
}

// relOrDir returns dir relative to repoRoot, falling back to dir itself
// when the relative path cannot be computed.
func relOrDir(repoRoot, dir string) string {
	rel, err := filepath.Rel(repoRoot, dir)
	if err != nil {
		return dir
	}
	return rel
}

// hasConcurrencyMarker reports whether s contains any concurrency marker,
// matched case-insensitively.
func hasConcurrencyMarker(s string) bool {
	if s == "" {
		return false
	}
	low := strings.ToLower(s)
	for _, m := range concurrencyMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}
