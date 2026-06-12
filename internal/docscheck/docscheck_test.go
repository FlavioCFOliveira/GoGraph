// Package docscheck holds documentation-accuracy regression gates for
// the GoGraph repository. The tests assert that the top-level docs
// (README.md, SECURITY.md, CONTRIBUTING.md, docs/benchmarks/) stay
// faithful to the code and to the current release, so a stale or broken
// doc fails `go test ./...` instead of shipping silently.
//
// The package is test-only by design; it has no production surface.
package docscheck

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// repoRoot walks up from the test's working directory until it finds the
// directory containing go.mod, which is the repository root.
func repoRoot(t *testing.T) string {
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

func readDoc(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

var semverFile = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)\.md$`)

// latestReleaseNotesVersion returns the highest-semver vX.Y.Z taken from
// the release-notes/ directory — the authoritative "current release"
// according to the committed long-form notes.
func latestReleaseNotesVersion(t *testing.T, root string) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "release-notes"))
	if err != nil {
		t.Fatalf("read release-notes/: %v", err)
	}
	var best [3]int
	var bestStr string
	for _, e := range entries {
		m := semverFile.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		var v [3]int
		for i := 0; i < 3; i++ {
			v[i], _ = strconv.Atoi(m[i+1])
		}
		if bestStr == "" || v[0] > best[0] ||
			(v[0] == best[0] && v[1] > best[1]) ||
			(v[0] == best[0] && v[1] == best[1] && v[2] > best[2]) {
			best = v
			bestStr = "v" + m[1] + "." + m[2] + "." + m[3]
		}
	}
	if bestStr == "" {
		t.Fatal("no release-notes/vX.Y.Z.md file found")
	}
	return bestStr
}

var readmeCurrentRelease = regexp.MustCompile("Current release: `(v\\d+\\.\\d+\\.\\d+)`")

// TestREADMECurrentReleaseMatchesLatest guards #1397: the README Status
// block must name the latest released version, not a superseded one.
func TestREADMECurrentReleaseMatchesLatest(t *testing.T) {
	root := repoRoot(t)
	readme := readDoc(t, root, "README.md")
	latest := latestReleaseNotesVersion(t, root)

	m := readmeCurrentRelease.FindStringSubmatch(readme)
	if m == nil {
		t.Fatalf("README.md does not contain a \"Current release: `vX.Y.Z`\" marker")
	}
	if m[1] != latest {
		t.Errorf("README Status names %s as the current release, but the latest release-notes file is %s; update the README Status block", m[1], latest)
	}
	// The README should also point users at the matching release notes and
	// the matching go get pin.
	if !strings.Contains(readme, "release-notes/"+latest+".md") {
		t.Errorf("README does not link release-notes/%s.md", latest)
	}
	if !strings.Contains(readme, "@"+latest) {
		t.Errorf("README `go get` line does not pin @%s", latest)
	}
}

// TestPerReleaseBenchmarkReportExists guards #1398: every release must
// ship docs/benchmarks/<version>.md.
func TestPerReleaseBenchmarkReportExists(t *testing.T) {
	root := repoRoot(t)
	latest := latestReleaseNotesVersion(t, root)
	path := filepath.Join(root, "docs", "benchmarks", latest+".md")
	if _, err := os.Stat(path); err != nil {
		t.Errorf("per-release benchmark report docs/benchmarks/%s.md is missing (%v); each release must record its benchmark/load-test numbers", latest, err)
	}
}

// TestValidationPipelineDocsMentionCoverGate guards #1403: README and
// CONTRIBUTING must describe the coverage gate and its thresholds.
func TestValidationPipelineDocsMentionCoverGate(t *testing.T) {
	root := repoRoot(t)
	for _, rel := range []string{"README.md", "CONTRIBUTING.md"} {
		doc := readDoc(t, root, rel)
		lower := strings.ToLower(doc)
		if !strings.Contains(lower, "cover-gate") && !strings.Contains(lower, "coverage gate") {
			t.Errorf("%s does not mention the coverage gate (cover-gate)", rel)
		}
		if !strings.Contains(doc, "85") {
			t.Errorf("%s does not state the aggregate coverage threshold (85)", rel)
		}
		if !strings.Contains(doc, "75") {
			t.Errorf("%s does not state the per-package coverage threshold (75)", rel)
		}
	}
}

// propertyKindTokens maps every exported lpg.PropertyKind to the token
// the README PropertyValue sentence must list for it. Referencing each
// constant by name makes this fail to compile if a kind is renamed.
//
// IMPORTANT: if you add a new PropertyKind, add it here AND to the README
// PropertyValue sentence — the PropList==max guard below is a tripwire,
// not a substitute for updating this map.
var propertyKindTokens = map[lpg.PropertyKind]string{
	lpg.PropString:  "string",
	lpg.PropInt64:   "int64",
	lpg.PropFloat64: "float64",
	lpg.PropBool:    "bool",
	lpg.PropTime:    "time.Time",
	lpg.PropBytes:   "[]byte",
	lpg.PropList:    "list",
}

// TestREADMEListsAllPropertyKinds guards #1404: the README PropertyValue
// sentence must enumerate every implemented property kind, including the
// list kind (PropList) the audit found omitted.
func TestREADMEListsAllPropertyKinds(t *testing.T) {
	root := repoRoot(t)
	readme := readDoc(t, root, "README.md")

	// Tripwire: PropList is the highest-numbered kind (kinds are iota+1,
	// 1..7). If this fails a kind was inserted or renumbered — extend
	// propertyKindTokens and the README sentence, then update this guard.
	if got := int(lpg.PropList); got != len(propertyKindTokens) {
		t.Fatalf("PropList == %d but propertyKindTokens has %d entries; a PropertyKind was added/renumbered — update this test and the README PropertyValue sentence", got, len(propertyKindTokens))
	}

	// Isolate the README sentence describing PropertyValue (it spans two
	// wrapped lines) so a token present elsewhere in the README cannot
	// mask an omission here.
	snippet := propertyValueSentence(t, readme)
	for kind, token := range propertyKindTokens {
		if !strings.Contains(snippet, token) {
			t.Errorf("README PropertyValue sentence omits the %v kind (expected token %q); sentence was:\n%s", kind, token, snippet)
		}
	}
}

// propertyValueSentence returns the README fragment that begins with the
// "`PropertyValue` covers" marker and the following wrapped line.
func propertyValueSentence(t *testing.T, readme string) string {
	t.Helper()
	lines := strings.Split(readme, "\n")
	for i, ln := range lines {
		if strings.Contains(ln, "`PropertyValue` covers") {
			end := i + 1
			if i+1 < len(lines) {
				end = i + 2
			}
			return strings.Join(lines[i:end], " ")
		}
	}
	t.Fatal("README does not contain the \"`PropertyValue` covers\" sentence")
	return ""
}

// --- #1396: link / placeholder checker over SECURITY.md and README.md ---

var (
	mdLink       = regexp.MustCompile(`\[[^\]]*\]\(([^)]+)\)`)
	inlineCode   = regexp.MustCompile("`[^`]*`")
	fencedToggle = "```"
)

// stripCode removes fenced code blocks and inline code spans so Go
// generic-call syntax inside examples (e.g. `Foo[string,int64]("x")`) is
// not mistaken for a markdown link.
func stripCode(doc string) string {
	var b strings.Builder
	inFence := false
	for _, ln := range strings.Split(doc, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), fencedToggle) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		b.WriteString(inlineCode.ReplaceAllString(ln, ""))
		b.WriteByte('\n')
	}
	return b.String()
}

// TestSecurityAndReadmeLinks guards #1396: relative markdown links in the
// two highest-stakes docs must resolve, and neither doc may carry the
// placeholder hosts the audit found (xumiga / RFC 2606 *.example).
func TestSecurityAndReadmeLinks(t *testing.T) {
	root := repoRoot(t)
	placeholders := []string{"xumiga", ".example", "example.com", "example.org", "example.net"}

	for _, rel := range []string{"SECURITY.md", "README.md"} {
		doc := readDoc(t, root, rel)

		for _, ph := range placeholders {
			if strings.Contains(doc, ph) {
				t.Errorf("%s contains placeholder host %q — replace it with a real, functional reference", rel, ph)
			}
		}

		for _, m := range mdLink.FindAllStringSubmatch(stripCode(doc), -1) {
			target := m[1]
			switch {
			case strings.HasPrefix(target, "http://"), strings.HasPrefix(target, "https://"),
				strings.HasPrefix(target, "mailto:"), strings.HasPrefix(target, "#"):
				continue
			}
			path := target
			if i := strings.IndexByte(path, '#'); i >= 0 {
				path = path[:i]
			}
			if path == "" {
				continue
			}
			if _, err := os.Stat(filepath.Join(root, path)); err != nil {
				t.Errorf("%s links to %q which does not resolve to a repository file (%v)", rel, target, err)
			}
		}
	}

	// The corrected GitHub Security Advisories URL must point at the real
	// repository.
	sec := readDoc(t, root, "SECURITY.md")
	const wantAdvisory = "https://github.com/FlavioCFOliveira/GoGraph/security/advisories/new"
	if !strings.Contains(sec, wantAdvisory) {
		t.Errorf("SECURITY.md does not contain the real advisory URL %q", wantAdvisory)
	}
}
